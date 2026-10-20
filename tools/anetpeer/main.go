// anetpeer is a peer process for the p2p module.
//
// The module is a client: it speaks a small newline-JSON protocol over a
// Unix socket and presents whatever answers as an ordinary transport. That
// design is deliberate — a libp2p stack pulls a thousand-module dependency
// tree, and a build tag removes code from a binary, never a dependency
// from go.mod — but it left the wire with no implementation, so the one
// module that changes how delegations travel was the one module no joint
// run could exercise.
//
// This is that implementation, at the smallest size that is honest. Each
// peer process listens for its daemon on one socket and for other peers on
// another (a Unix socket, or a TCP address for peers on other machines). It
// dials one address only: the one in the daemon's send frame (addr), which
// the recipient gave this node in the contact-class delivery credential it
// issued (sealed-sender SSR-P3). There is no directory and no lookup — a hub
// is never asked where anyone is — and an address is dialled only at public
// IPs: a name is resolved once, every address it resolves to is checked, and
// only those are dialled (SSR-P4, internal/p2paddr; --lan lets private ones
// through for a lab). What it is not is a substitute for a real peer stack:
// there is no NAT traversal and no relay.
//
//	anetpeer --socket /run/anet/peer.sock --peer :39100
//
// For tests and joint runs on one machine, --rendezvous DIR replaces the
// address with a shared directory: each peer process writes where it
// listens under its daemon's AID, and a send dials the recipient's entry.
// Nothing is checked there, so it is for tests only.
//
// Only the recipient says a message arrived. This process passes on what
// the receiving daemon answered — its ack, with the delivery confirmation
// (dack) exactly as that daemon wrote it, or its refusal — and says nothing
// of its own about delivery: it makes no confirmation, reads none, checks
// none and keeps none (frame v3). A transport that vouched for delivery on
// its own would turn a dropped delegation into a silent one, and a
// confirmation it could make up would be worth nothing to the sender.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/internal/p2paddr"
	"github.com/ANetResearch/ANet/internal/peercred"
)

// frame is the module's wire, both sides of it (module/p2p, frame v3;
// module/p2p/testdata/frame-v3.json). It carries the recipient AID, the
// sealed envelope and an ID, and nothing about the sender or the content:
// those are inside the envelope, which this process cannot open (A2A-DESIGN
// §3.10). Nor does any field say whether a message was processed or
// refused: the only word on delivery is the recipient's dack, which is
// sealed to its sender.
type frame struct {
	Op string `json:"op"`
	// V is the frame version, on every frame. A frame of another version
	// is refused: a hello with an error frame and a closed connection, a
	// delivery with an error frame, and the daemon never sees it.
	V int `json:"v"`
	// ID correlates a request with its reply and must be echoed unchanged.
	// A peer that answers concurrently — which this one does, because
	// answering in its read loop deadlocks — would otherwise have its
	// replies matched to the wrong requests. For a delivery handed to the
	// daemon (op recv) this process mints a fresh ID and matches the
	// daemon's ack on it.
	ID       string `json:"id,omitempty"`
	To       string `json:"to,omitempty"`
	Envelope string `json:"envelope,omitempty"`
	// Addr, on a send from the daemon, is where the recipient can be
	// dialled. It goes no further than this process.
	Addr string `json:"addr,omitempty"`
	// DAck is the receiving daemon's delivery confirmation, carried from
	// its ack to the answer of the send that brought the envelope, as it
	// came: never decoded, measured, kept or changed here — "dack":"" is
	// passed on as "dack":"", not as no dack.
	DAck dackText `json:"dack,omitzero"`
	Self string   `json:"self,omitempty"`
	// Error is a sentence for the log. It decides nothing.
	Error string `json:"error,omitempty"`
}

// dackText is a dack member as it came: whether there was one, and its
// text. An empty one is not the same as none (core CORE-K13a), and it is the
// sending daemon that tells them apart, not this process.
type dackText struct {
	s   string
	set bool
}

// dack is a dack member of text s; "" is none.
func dack(s string) dackText { return dackText{s: s, set: s != ""} }

func (d dackText) IsZero() bool                 { return !d.set }
func (d dackText) String() string               { return d.s }
func (d dackText) MarshalJSON() ([]byte, error) { return json.Marshal(d.s) }

func (d *dackText) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '"' {
		return errors.New("dack is not a string")
	}
	if err := json.Unmarshal(b, &d.s); err != nil {
		return err
	}
	d.set = true
	return nil
}

// wireVersion is the frame version this process speaks.
const wireVersion = 3

// versionRefused is the answer to a frame of another version. It names the
// version the other side needs, so its operator can act on it.
func versionRefused(v int) string {
	return fmt.Sprintf("frame version %d refused: this peer speaks frame version %d (anet 0.3.0)", v, wireVersion)
}

// frameRow is what one op's frame may carry on one leg of the wire (core
// §2.10): the members every such frame has, and those it may have.
type frameRow struct{ must, may []string }

// The legs this process reads, each with the frames that travel on it.
var (
	// fromDaemon: the daemon to this process.
	fromDaemon = map[string]frameRow{
		"hello": {must: []string{"v", "self"}},
		"send":  {must: []string{"v", "id", "to", "envelope", "addr"}},
		"ack":   {must: []string{"v", "id"}, may: []string{"dack"}},
		"nack":  {must: []string{"v", "id", "error"}},
		"error": {must: []string{"v"}, may: []string{"id", "error"}},
	}
	// fromPeer: another peer process handing this one a delivery.
	fromPeer = map[string]frameRow{
		"send": {must: []string{"v", "id", "to", "envelope"}},
	}
	// answerFromPeer: the receiving peer process's answer to a delivery.
	answerFromPeer = map[string]frameRow{
		"send":  {must: []string{"v", "id"}, may: []string{"dack", "error"}},
		"error": {must: []string{"v"}, may: []string{"id", "error"}},
	}
)

// readFrame decodes one line read on a leg strictly (core CORE-G4,
// CORE-K13a): a frame of another version, of an op the leg does not carry,
// with a member its row does not have or without one it must have, or with
// a member of the wrong type is a bad frame, and bad says why. f is the
// frame as far as it could be read — its op, v and id, to answer it. err is a
// line that is not a JSON object.
//
// The dack is not looked at beyond being a string: this process passes it
// on as it came (DLV-W-9), and the sending daemon judges it.
func readFrame(line []byte, leg map[string]frameRow) (f frame, bad string, err error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(line, &members); err != nil {
		return frame{}, "", err
	}
	if members == nil {
		return frame{}, "", errors.New("null")
	}
	typeErr := json.Unmarshal(line, &f)
	for m, raw := range members {
		want := byte('"')
		if m == "v" {
			if len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9') {
				continue
			}
			typeErr = fmt.Errorf("member v is not a number")
			break
		}
		if len(raw) == 0 || raw[0] != want {
			typeErr = fmt.Errorf("member %q is not a string", m)
			break
		}
	}
	if f.V != wireVersion {
		return f, versionRefused(f.V), nil
	}
	row, ok := leg[f.Op]
	if !ok {
		return f, fmt.Sprintf("a frame with op %q, which does not travel this way", f.Op), nil
	}
	for m := range members {
		if m != "op" && !slices.Contains(row.must, m) && !slices.Contains(row.may, m) {
			return f, fmt.Sprintf("a %s frame with the member %q, which frame version %d does not have", f.Op, m, wireVersion), nil
		}
	}
	for _, m := range row.must {
		if _, ok := members[m]; !ok {
			return f, fmt.Sprintf("a %s frame without its member %q", f.Op, m), nil
		}
	}
	if typeErr != nil {
		return f, "a frame with a member of the wrong type: " + typeErr.Error(), nil
	}
	return f, "", nil
}

// decodeB64 decodes base64url without padding and refuses every other
// spelling of the same bytes: "=", "+", "/", white space, and trailing bits
// that are not zero. It is the daemon's rule for every binary value of the
// wire (internal/hubapi DecodeB64, core CORE-E5), written out here so that
// this process does not carry the daemon's hub client.
func decodeB64(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return nil, fmt.Errorf("not base64url without padding (byte %d)", i)
		}
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

// checkEnvelope is why an envelope member cannot be carried, "" when it
// can: an envelope is base64url without padding of at least one byte, and
// no larger than a hub would carry.
func checkEnvelope(s string) string {
	if s == "" {
		return "an empty envelope"
	}
	if len(s) > base64.RawURLEncoding.EncodedLen(maxEnvelopeBytes) {
		return "an envelope over 96 MiB"
	}
	if _, err := decodeB64(s); err != nil {
		return "the envelope is " + err.Error()
	}
	return ""
}

// The sizes of what crosses this process. An envelope is at most what a hub
// carries (96 MiB); a frame is that envelope in base64url with room for the
// rest of the frame; an answer has no envelope. A longer line is not a
// frame this wire has: the connection it came on is closed.
const (
	maxEnvelopeBytes = 96 << 20
	maxFrameLen      = (maxEnvelopeBytes+2)/3*4 + 64<<10
	maxAnswerLen     = 64 << 10
)

// readLine reads one line, newline excluded, of at most max bytes.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max+1 {
			return nil, fmt.Errorf("a frame over %d bytes", max)
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// encodeLine is a frame as one line of the wire.
func encodeLine(f frame) ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// handOffAnswer is what the receiving daemon answered to one delivery: its
// dack (possibly none), or why it did not take the delivery.
type handOffAnswer struct {
	dack dackText
	err  error
}

func main() {
	refuseUnsupportedPlatform()
	sock := flag.String("socket", "", "socket the daemon's p2p module connects to (modules.p2p.socket)")
	peerSock := flag.String("peer", "", "where other peer processes connect: a TCP address (:39100) or a socket path")
	lan := flag.Bool("lan", false,
		"also dial private addresses and 100.64.0.0/10 (a lab on one network; set modules.p2p.lan to match)")
	rv := flag.String("rendezvous", "",
		"testing only: a directory shared with the other peer processes, dialled instead of the address in each send")
	advertise := flag.String("advertise", "", "testing only, with --rendezvous: what to write there (default: --peer)")
	tee := flag.String("tee-dir", "",
		"testing only: write a copy of every envelope this peer delivered for its daemon into this directory")
	flag.Parse()
	if *sock == "" || *peerSock == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: anetpeer --socket S --peer ADDR [--lan]")
		os.Exit(2)
	}
	p, err := startOpts(options{socket: *sock, peer: *peerSock, lan: *lan, rendezvous: *rv, advertise: *advertise, teeDir: *tee})
	if err != nil {
		log.Fatalf("anetpeer: %v", err)
	}
	mode := "the address in each send, public IPs only"
	switch {
	case *rv != "":
		mode = "the rendezvous directory " + *rv + " (testing only)"
	case *lan:
		mode = "the address in each send, public and private IPs (LAN mode)"
	}
	log.Printf("anetpeer: daemon on %s (modules.p2p.socket), peers on %s; dialling %s", p.daemonLn.Addr(), p.peerLn.Addr(), mode)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	p.stop()
}

// options are what a peer process is started with.
type options struct {
	socket, peer string
	lan          bool
	// rendezvous, advertise and teeDir are for tests (see main).
	rendezvous, advertise, teeDir string
}

// start brings both listeners up with a rendezvous directory and serves
// them; startWith also keeps a tee directory.
//
// Separated from main so a test can run two of these in one process and
// drive a real module/p2p transport against them. The client half and this
// half are the two sides of one contract, and both of them had the same
// deadlock — handling a request inline on the loop that has to deliver its
// answer. Testing them apart is what let that ship twice.
func start(sock, peerSock, rendezvous, advertise string) (*peer, error) {
	return startWith(sock, peerSock, rendezvous, advertise, "")
}

// startWith is start with a tee directory (see tee); "" keeps none.
func startWith(sock, peerSock, rendezvous, advertise, teeDir string) (*peer, error) {
	return startOpts(options{socket: sock, peer: peerSock, rendezvous: rendezvous, advertise: advertise, teeDir: teeDir})
}

func startOpts(o options) (*peer, error) {
	if o.teeDir != "" {
		if err := os.MkdirAll(o.teeDir, 0o700); err != nil {
			return nil, fmt.Errorf("tee dir: %w", err)
		}
	}
	if o.rendezvous != "" {
		if err := os.MkdirAll(o.rendezvous, 0o755); err != nil {
			return nil, err
		}
	}
	p := &peer{peerSocket: o.peer, rendezvous: o.rendezvous, advertise: o.advertise, teeDir: o.teeDir, lan: o.lan,
		acks: map[string]chan handOffAnswer{}, limits: newSourceLimits(), reading: make(chan struct{}, maxInboundReads)}

	// The peer-facing listener takes either a socket path or a TCP
	// address, because peers on two machines cannot share a Unix socket.
	pnet, paddr := addrKind(o.peer)
	if pnet == "unix" {
		if err := clearStaleSocket(paddr); err != nil {
			return nil, fmt.Errorf("peer wire: %w", err)
		}
	}
	pl, err := net.Listen(pnet, paddr)
	if err != nil {
		return nil, fmt.Errorf("peer wire: %w", err)
	}
	dl, err := listenDaemon(o.socket)
	if err != nil {
		pl.Close()
		return nil, fmt.Errorf("daemon wire: %w", err)
	}
	p.peerLn, p.daemonLn = pl, dl
	p.inbound.Add(1)
	go func() {
		defer p.inbound.Done()
		p.servePeers(pl)
	}()
	go func() {
		for {
			c, err := dl.Accept()
			if err != nil {
				return
			}
			// Who connected is checked before this connection may take the
			// daemon's place, and before a byte of it is read: whatever
			// connects here is handed every envelope delivered for the
			// daemon's AID and may answer for it (LEC PC-4).
			if err := p.admitDaemon(c); err != nil {
				_ = c.Close()
				continue
			}
			go p.serveDaemon(c)
		}
	}()
	return p, nil
}

// listenDaemon listens for the daemon on sock (LEC PC-4). A missing
// directory is made 0700. An existing file at sock is removed only when it
// is a socket nobody listens on (a peer process that died): a live one, or
// anything else, is left and refused. The path, once listened on, must be
// one the daemon will connect through — its directories changeable only by
// this user or root (backendconn's path checks, which the daemon's p2p
// module applies before every connection): a path the daemon would refuse
// is refused here, at start, with the reason, rather than by every connection
// later. And where the kernel cannot say who connects, nothing is served.
//
// A relative sock is taken from the working directory, as a shell would:
// the daemon dials what modules.p2p.socket names, which must be absolute, so
// it is the absolute path that is listened on, checked and logged, for the
// daemon's configuration to name.
func listenDaemon(sock string) (net.Listener, error) {
	abs, err := filepath.Abs(sock)
	if err != nil {
		return nil, err
	}
	sock = abs
	if !peercredSupported() {
		return nil, fmt.Errorf("this platform cannot tell who connects to %s, so anyone on this machine could take the daemon's place; "+
			"anetpeer runs on Linux, macOS and FreeBSD", sock)
	}
	dir := filepath.Dir(sock)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		// MkdirAll's mode passes through the umask; say exactly what is meant.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := clearStaleSocket(sock); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	rules, err := backendconn.Policy{ExpectedUser: strconv.Itoa(os.Getuid())}.Resolve()
	if err == nil {
		_, _, err = rules.CheckPath(sock)
	}
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("the daemon would not connect to %s: %w; choose a socket in a directory only you can write "+
			"(one that does not exist yet is made 0700)", sock, err)
	}
	return ln, nil
}

// clearStaleSocket makes path free for a new socket. Nothing there is fine;
// a socket nobody listens on is removed. A socket somebody listens on — the
// daemon's session, or another peer process's — and anything that is not a
// socket are left in place and refused: removing them would take the path
// from whoever holds it (cmd/anet-official's removeStaleSocket, the same
// rule).
func clearStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; move it away or choose another path", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("%s is in use: another process listens on it; stop it or choose another path", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%s: %v; not removing it", path, err)
	}
	return os.Remove(path)
}

// peercredSupported is whether the daemon socket's peer can be told; a test
// stands in for a platform that cannot tell.
var peercredSupported = peercred.Supported

// refusedLogEvery is how often one account's refused connections are
// logged: once, then not again for this long, so that a process knocking in
// a loop cannot fill the log.
const refusedLogEvery = time.Minute

// admitDaemon lets a connection to the daemon socket through only when the
// process on the other end runs as this process's user, the daemon's: root
// is not let through for being root. A refusal is logged, at most once a
// minute per account.
func (p *peer) admitDaemon(c net.Conn) error {
	p.admitMu.Lock()
	credOf := p.credOf
	p.admitMu.Unlock()
	if credOf == nil {
		credOf = peercred.Of
	}
	cred, err := credOf(c)
	switch {
	case err != nil:
		err = fmt.Errorf("cannot tell who connected to the daemon socket (%v); refused", err)
	case cred.UID != os.Getuid():
		err = fmt.Errorf("uid %d (pid %d) connected to the daemon socket, which only this user, uid %d, may use; refused", cred.UID, cred.PID, os.Getuid())
	default:
		return nil
	}
	p.admitMu.Lock()
	if p.refusedLog == nil {
		p.refusedLog = map[int]time.Time{}
	}
	now := timeNow()
	last, seen := p.refusedLog[cred.UID]
	quiet := seen && now.Sub(last) < refusedLogEvery
	if !quiet {
		p.refusedLog[cred.UID] = now
	}
	p.admitMu.Unlock()
	if !quiet {
		log.Printf("anetpeer: %v", err)
	}
	return err
}

// stop closes both listeners and withdraws this AID from the rendezvous.
func (p *peer) stop() {
	p.forget()
	if p.daemonLn != nil {
		p.daemonLn.Close()
	}
	if p.peerLn != nil {
		p.peerLn.Close()
	}
}

type peer struct {
	peerSocket string
	// rendezvous, when set, is the test directory dialled instead of the
	// address in a send; advertise is what this process writes there.
	rendezvous string
	advertise  string
	// lan lets a dial go to private addresses (--lan, internal/p2paddr).
	lan      bool
	peerLn   net.Listener
	daemonLn net.Listener

	mu   sync.Mutex
	self string
	// conn is the daemon's connection: the one whose first frame came
	// last. Frames to the daemon are written through it.
	conn *daemonConn

	// teeDir, when set, receives a copy of every envelope delivered for
	// the daemon (see tee); teeN numbers them.
	teeDir string
	teeN   uint64

	// acks correlates a delivery with the receiving daemon's answer, keyed
	// by the ID this process minted for the delivery: the dack of an ack,
	// the daemon's reason for a nack.
	acks   map[string]chan handOffAnswer
	nextID uint64
	// limits is the per-source half of the inbound rate limit (A2A-DESIGN
	// §3.6 step 0); the daemon applies the daemon-wide half.
	limits *sourceLimits
	// reading holds a token for each frame from another peer being read
	// (maxInboundReads of them).
	reading chan struct{}
	// down holds the peers whose address could not be dialled just now
	// (see markDown), keyed by AID.
	down map[string]downEntry
	// refusedAddrs counts the sends whose address was refused
	// (p2p-addr-refused); badFrames the frames refused as bad on any leg
	// (core CORE-K13a). Both are said in the log with each refusal.
	refusedAddrs atomic.Uint64
	badFrames    atomic.Uint64
	// inbound counts the peer listener's accept loop and the connections
	// it is serving; stop does not wait for them (each ends within
	// handOffTimeout + replyMargin), tests that change those do.
	inbound sync.WaitGroup
	// admitMu guards refusedLog and credOf. It is not mu: admitDaemon runs
	// on the accept loop, which must not wait while a hand-off holds mu.
	admitMu sync.Mutex
	// refusedLog is when each uid's refused connection to the daemon
	// socket was last logged (admitDaemon).
	refusedLog map[int]time.Time
	// credOf reads who connected to the daemon socket; nil is peercred.Of.
	// A test stands in for another account.
	credOf func(net.Conn) (peercred.Cred, error)
	// resolve and dialIP are how a send's address is looked up and dialled
	// (net's, when nil); a test puts the addresses it needs behind them.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dialIP  func(ctx context.Context, ap netip.AddrPort) (net.Conn, error)
}

// daemonConn is a connection from the daemon, and the lock that keeps one
// frame written to it at a time.
type daemonConn struct {
	c   net.Conn
	wmu sync.Mutex
}

// daemonWriteTimeout bounds one frame's write to the daemon. A daemon reads
// as it goes, and the largest frame is written in milliseconds; one that has
// stopped reading would otherwise hold the write, and every hand-off and
// answer behind it, for as long as it chose. A variable so a test can
// shorten it.
var daemonWriteTimeout = 5 * time.Second

// write writes one frame to the daemon. The frame is encoded before the
// lock is taken, and no lock of the peer is held: a hand-off of a large
// envelope does not stop the rest of this process while it is encoded or
// written. A write that fails, the deadline's included, closes the
// connection: a frame cut short leaves a stream the daemon can no longer
// read, and its read loop ends with it.
func (d *daemonConn) write(f frame) error {
	b, err := encodeLine(f)
	if err != nil {
		return err
	}
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_ = d.c.SetWriteDeadline(time.Now().Add(daemonWriteTimeout))
	if _, err := d.c.Write(b); err != nil {
		_ = d.c.Close()
		return fmt.Errorf("writing to the daemon: %w", err)
	}
	_ = d.c.SetWriteDeadline(time.Time{})
	return nil
}

// downEntry is an address a dial to has failed, and until when it is
// reported unreachable.
type downEntry struct {
	addr    string
	until   time.Time
	backoff time.Duration
}

// How long an address that could not be dialled is not dialled again:
// dialDownMin after the first failure, doubling on every failed retry up to
// dialDownMax, forgotten on the first delivery that gets through. A send to
// it in the meantime fails at once.
//
// Without it an address that cannot be dialled — a peer behind a firewall
// that drops inbound connections, which is the ordinary case for a node that
// can only dial out — costs every single message to that peer the dial
// timeout before the hub is tried. On the test network (an internal note)
// that was three seconds on every message from one node to the other, and
// since the answer to a delegation is sent before the delegation is
// acknowledged, it also held the ack past the sender's own timeout, so the
// delegation went a second time through the hub. A peer that comes back is
// found again at the next retry.
var (
	dialDownMin = 30 * time.Second
	dialDownMax = 10 * time.Minute
	// timeNow is the clock of the down marks; tests move it.
	timeNow = time.Now
)

// markDown records that addr, the address of aid, could not be dialled. It
// returns the new window, or 0 when a window for that address is already
// open (a concurrent send failing changes nothing).
func (p *peer) markDown(aid, addr string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := timeNow()
	e, ok := p.down[aid]
	switch {
	case !ok || e.addr != addr:
		e = downEntry{addr: addr, backoff: dialDownMin}
	case now.Before(e.until):
		return 0
	default:
		e.backoff *= 2
		if e.backoff > dialDownMax {
			e.backoff = dialDownMax
		}
	}
	e.until = now.Add(e.backoff)
	if p.down == nil {
		p.down = map[string]downEntry{}
	}
	p.down[aid] = e
	return e.backoff
}

// isDown reports whether addr, aid's address, is inside a window opened by
// markDown. A different address — the peer moved — is not.
func (p *peer) isDown(aid, addr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.down[aid]
	return ok && e.addr == addr && timeNow().Before(e.until)
}

// clearDown forgets aid's mark after a delivery got through.
func (p *peer) clearDown(aid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.down, aid)
}

// serveDaemon handles the module's connection: hello, send, and the acks
// that come back for what we delivered.
//
// The connection takes the daemon's place with its first frame (the
// daemon's hello comes first), not when it is accepted: a connection that
// says nothing and closes — another peer process started on the same path,
// asking whether anyone still listens (clearStaleSocket) — must not leave
// the daemon that is there without its deliveries.
func (p *peer) serveDaemon(c net.Conn) {
	defer c.Close()
	dc := &daemonConn{c: c}
	defer func() {
		p.mu.Lock()
		if p.conn == dc {
			p.conn = nil
		}
		p.mu.Unlock()
	}()
	br := bufio.NewReaderSize(c, 64<<10)
	for first := true; ; first = false {
		line, err := readLine(br, maxFrameLen)
		if err != nil {
			return
		}
		f, bad, err := readFrame(line, fromDaemon)
		if err != nil {
			log.Printf("anetpeer: the daemon wrote a line that is not a frame (%v); closing its connection", err)
			return
		}
		if first {
			p.mu.Lock()
			p.conn = dc
			p.mu.Unlock()
		}
		if bad == "" && f.Op == "send" {
			bad = checkEnvelope(f.Envelope)
		}
		if bad != "" {
			// A bad frame. A hello is answered and the connection closed:
			// a daemon that speaks another version cannot be carried for.
			// A send is answered and not delivered. Anything else is
			// dropped: an ack this process cannot read releases nothing.
			log.Printf("anetpeer: bad-frame (count %d): refusing a %q frame from the daemon: %s", p.badFrames.Add(1), f.Op, bad)
			switch f.Op {
			case "hello":
				p.reply(dc, frame{Op: "error", V: wireVersion, Error: bad})
				return
			case "send":
				p.reply(dc, frame{Op: "error", V: wireVersion, ID: f.ID, Error: bad})
			}
			continue
		}
		switch f.Op {
		case "hello":
			p.announce(f.Self)
			log.Printf("anetpeer: carrying %s", f.Self)
		case "send":
			// Off the read loop, and it has to be.
			//
			// Delivering means dialling the receiving peer, which waits for
			// its daemon to accept — and this daemon's ack for an inbound
			// message arrives on this same loop. Handled inline, the loop
			// blocked on a delivery while the ack it needed sat unread
			// behind it, and both sides unwound on timeouts ten seconds
			// apart. The daemon's transport had the identical bug for the
			// identical reason; a request/reply loop that does work inline
			// is the shape to distrust.
			go func(f frame) {
				// The answer carries the receiving daemon's dack as it
				// came, with an error or without.
				d, err := p.deliver(f)
				if err != nil {
					log.Printf("anetpeer: send to %s: %v", f.To, err)
					p.reply(dc, frame{Op: "send", V: wireVersion, ID: f.ID, DAck: d, Error: err.Error()})
					return
				}
				log.Printf("anetpeer: delivered %d envelope bytes → %s", base64.RawURLEncoding.DecodedLen(len(f.Envelope)), f.To)
				p.tee(f)
				p.reply(dc, frame{Op: "send", V: wireVersion, ID: f.ID, DAck: d})
			}(f)
		case "ack", "nack", "error":
			// Our daemon accepted something we handed it (with its dack),
			// or did not take it: refused it for now, or could not read the
			// frame. Tell the peer that is waiting. The ID is the one this
			// process minted for that delivery, so an answer can only
			// release the delivery it answers. A refusal is passed on at
			// once rather than left to run out handOffTimeout: the sender
			// falls back to the hub now.
			var answer handOffAnswer
			if f.Op == "ack" {
				answer.dack = f.DAck
			} else {
				answer.err = fmt.Errorf("receiving daemon did not take the delivery: %s", f.Error)
			}
			p.mu.Lock()
			if ch, ok := p.acks[f.ID]; ok {
				ch <- answer
				delete(p.acks, f.ID)
			}
			p.mu.Unlock()
		}
	}
}

// tee keeps a copy of an envelope this process delivered for its daemon,
// as <n>-<to>.env holding the standard base64 of the bytes (the frame
// carries base64url; the copy is in the form anetfixture --envelope reads),
// written only once the receiving side accepted it.
//
// For tests only. The joint run replays these exact bytes through the hub
// and over the peer wire again (A2A-DESIGN SI-10): a replay test that
// re-sealed the message would test a new message, not a replay. The copy
// is of what this process already carries — sealed to the recipient and
// unreadable here — so it reveals nothing this process did not hold.
func (p *peer) tee(f frame) {
	if p.teeDir == "" {
		return
	}
	p.mu.Lock()
	p.teeN++
	n := p.teeN
	p.mu.Unlock()
	env, err := decodeB64(f.Envelope)
	if err != nil {
		log.Printf("anetpeer: tee: envelope %d is not base64url: %v", n, err)
		return
	}
	to := strings.NewReplacer("/", "_", string(filepath.Separator), "_").Replace(f.To)
	name := filepath.Join(p.teeDir, fmt.Sprintf("%06d-%s.env", n, to))
	if err := os.WriteFile(name, []byte(base64.StdEncoding.EncodeToString(env)+"\n"), 0o600); err != nil {
		log.Printf("anetpeer: tee: %v", err)
	}
}

// maxInboundReads bounds the frames from other peer processes read at
// once: each may hold up to maxFrameLen while it is read. A connection past
// it waits for its turn as long as its sender waits for the whole delivery,
// then is answered busy, and its sender goes to the hub.
const maxInboundReads = 16

// servePeers handles other anetpeer processes handing us traffic.
func (p *peer) servePeers(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		p.inbound.Add(1)
		go func() {
			defer p.inbound.Done()
			p.servePeer(c)
		}()
	}
}

// servePeer handles one connection from another peer process: one frame,
// one answer. The frame must arrive within the time its sender waits for
// the whole delivery (handOffTimeout + replyMargin), and be no longer than
// maxFrameLen; the answer is written within replyMargin. A connection that
// sends nothing, sends without end or does not read its answer holds
// nothing here for longer than that.
func (p *peer) servePeer(c net.Conn) {
	defer c.Close()
	answer := func(f frame) {
		b, err := encodeLine(f)
		if err != nil {
			return
		}
		_ = c.SetWriteDeadline(time.Now().Add(replyMargin))
		_, _ = c.Write(b)
	}
	deadline := time.Now().Add(handOffTimeout + replyMargin)
	if p.reading != nil {
		turn := time.NewTimer(time.Until(deadline))
		select {
		case p.reading <- struct{}{}:
			turn.Stop()
		case <-turn.C:
			answer(frame{Op: "error", V: wireVersion, Error: "busy; deliver through the hub"})
			return
		}
	}
	_ = c.SetReadDeadline(deadline)
	line, err := readLine(bufio.NewReaderSize(c, 64<<10), maxFrameLen)
	if p.reading != nil {
		<-p.reading
	}
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	f, bad, err := readFrame(line, fromPeer)
	if err != nil {
		return
	}
	if bad == "" {
		bad = checkEnvelope(f.Envelope)
	}
	if bad != "" {
		log.Printf("anetpeer: bad-frame (count %d): refusing a %q frame from %s: %s", p.badFrames.Add(1), f.Op, sourceOf(c), bad)
		answer(frame{Op: "error", V: wireVersion, ID: f.ID, Error: bad})
		return
	}
	answer(p.receive(sourceOf(c), f))
}

// receive decides what to do with one delivery from another peer and
// returns the reply for it.
func (p *peer) receive(source string, f frame) frame {
	if f.V != wireVersion {
		// A sender of another frame version: one from before sealed
		// envelopes sends plaintext the daemon would refuse anyway, and one
		// from v2 expects answers this process no longer gives. Answering
		// with an error rather than handing it over sends it to its hub.
		return frame{Op: "error", V: wireVersion, ID: f.ID, Error: versionRefused(f.V)}
	}
	if !p.limits.allow(source, time.Now()) {
		return frame{Op: "error", V: wireVersion, ID: f.ID, Error: "rate limited; deliver through the hub"}
	}
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()
	if f.To == "" || f.To != self {
		// Not the AID this process carries: the sender dialled an address
		// that no longer (or never) belonged to its recipient — a stale
		// credential, a reused port. Handing it over would get it
		// acknowledged by the wrong daemon; refusing it sends the sender to
		// the hub, where the recipient's mailbox is ([redteam:F22]).
		return frame{Op: "error", V: wireVersion, ID: f.ID, Error: "this peer does not carry " + f.To + "; deliver through the hub"}
	}
	d, err := p.handOff(f)
	out := frame{Op: "send", V: wireVersion, ID: f.ID, DAck: d}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

// handOff pushes an inbound envelope to our daemon and waits for it to say
// it accepted it, and returns the dack that came with the ack. Every
// delivery gets its own ID, and the daemon's ack is matched on that ID
// alone.
func (p *peer) handOff(f frame) (dackText, error) {
	p.mu.Lock()
	dc := p.conn
	if dc == nil {
		p.mu.Unlock()
		return dackText{}, fmt.Errorf("no daemon attached")
	}
	p.nextID++
	id := fmt.Sprintf("d%d", p.nextID)
	ch := make(chan handOffAnswer, 1)
	p.acks[id] = ch
	p.mu.Unlock()
	forget := func() {
		p.mu.Lock()
		delete(p.acks, id)
		p.mu.Unlock()
	}
	if err := dc.write(frame{Op: "recv", V: wireVersion, ID: id, Envelope: f.Envelope}); err != nil {
		forget()
		return dackText{}, err
	}
	select {
	case a := <-ch:
		return a.dack, a.err
	case <-time.After(handOffTimeout):
		forget()
		// No answer, and saying so is the point: the sender falls through
		// to the hub rather than losing the message.
		return dackText{}, fmt.Errorf("receiving daemon did not accept delivery %s", id)
	}
}

// The bounds of one delivery, and how they fit together (an internal note item,
// an internal note N3). The sending daemon waits for this process's answer
// to a send (module/p2p, 20 s by default) and has to hear it: a daemon that
// gives up while this process is still dialling or waiting counts the
// envelope undelivered and sends it through the hub, and if this process
// then gets it through, the recipient has it twice. So every wait here
// ends well inside that: dialTimeout to resolve and connect, handOffTimeout
// for the receiving daemon's ack or nack (it acks as soon as its step 10
// has committed), replyMargin for the reply to travel back — 18 s in all.
//
// The dial is short on purpose: an address that cannot be dialled is then
// reported unreachable for a while (markDown), and the next messages go
// straight to the hub.
var (
	dialTimeout    = 3 * time.Second
	handOffTimeout = 10 * time.Second
	replyMargin    = 5 * time.Second
)

// sourceOf names where a peer connection came from, for the per-source
// rate limit: the remote host for TCP; for a Unix socket every peer shares
// one source, because a local socket carries no address.
func sourceOf(c net.Conn) string {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP.String()
	}
	return "unix"
}

// sourceLimits is a token bucket per source address, bounded in number.
type sourceLimits struct {
	mu      sync.Mutex
	buckets map[string]*sourceBucket
	order   []string
}

type sourceBucket struct {
	tokens float64
	last   time.Time
}

// Per-source inbound limit. Lower than the receiving daemon's own limit so
// one source cannot use all of it.
const (
	sourceRatePerSecond = 10
	sourceBurst         = 50
	sourceLimitEntries  = 4096
)

func newSourceLimits() *sourceLimits { return &sourceLimits{buckets: map[string]*sourceBucket{}} }

func (l *sourceLimits) allow(src string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[src]
	if b == nil {
		b = &sourceBucket{tokens: sourceBurst, last: now}
		l.buckets[src] = b
		l.order = append(l.order, src)
		if len(l.order) > sourceLimitEntries {
			delete(l.buckets, l.order[0])
			l.order = l.order[1:]
		}
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * sourceRatePerSecond
		if b.tokens > sourceBurst {
			b.tokens = sourceBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// deliver carries one envelope to the peer at the send's address (or, in
// a test, at the recipient's rendezvous entry), and returns the receiving
// daemon's dack as its peer passed it on.
func (p *peer) deliver(f frame) (dackText, error) {
	target, err := p.target(f)
	if err != nil {
		return dackText{}, err
	}
	if p.isDown(f.To, target) {
		return dackText{}, fmt.Errorf("%s at %s could not be dialled a moment ago; not dialling it again yet", f.To, target)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	c, err := p.dial(ctx, target)
	cancel()
	if errors.Is(err, p2paddr.ErrRefused) {
		// Not a failure of the network: the address is one no delivery is
		// dialled at, now or later (SSR-P4). Counted, and the daemon goes to
		// the hub.
		n := p.refusedAddrs.Add(1)
		log.Printf("anetpeer: p2p-addr-refused (count %d): %v", n, err)
		return dackText{}, err
	}
	if err != nil {
		// Only a failed dial marks the address: an error after the
		// connection is up (a refusal, a slow ack) is the peer answering.
		if w := p.markDown(f.To, target); w > 0 {
			log.Printf("anetpeer: %s at %s cannot be dialled (%v); not dialling it for %s, the hub carries its traffic",
				f.To, target, err, w)
		}
		return dackText{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(handOffTimeout + replyMargin))
	// The address stays here: the receiving peer has no use for it.
	b, err := encodeLine(frame{Op: "send", V: wireVersion, ID: f.ID, To: f.To, Envelope: f.Envelope})
	if err != nil {
		return dackText{}, err
	}
	if _, err := c.Write(b); err != nil {
		return dackText{}, err
	}
	line, err := readLine(bufio.NewReader(c), maxAnswerLen)
	if err != nil {
		return dackText{}, err
	}
	reply, bad, err := readFrame(line, answerFromPeer)
	if err == nil && bad == "" && reply.ID != f.ID && (reply.Op == "send" || reply.ID != "") {
		// An error frame may come without an id (a peer that is busy says
		// so before it reads); anything else answers this delivery's id.
		bad = fmt.Sprintf("an answer for %q to the delivery %q", reply.ID, f.ID)
	}
	switch {
	case err != nil:
		return dackText{}, fmt.Errorf("the receiving peer answered with a line that is not a frame: %v", err)
	case bad != "":
		// Nothing in a bad answer is passed on, a dack included.
		p.badFrames.Add(1)
		return dackText{}, fmt.Errorf("the receiving peer answered with a bad frame: %s", bad)
	case reply.Op != "send" || reply.Error != "":
		msg := reply.Error
		if msg == "" {
			msg = "the receiving peer answered with op " + reply.Op
		}
		return reply.DAck, errors.New(msg)
	}
	p.clearDown(f.To)
	return reply.DAck, nil
}

// target is where a send is dialled: its address, or in a test the
// recipient's rendezvous entry.
func (p *peer) target(f frame) (string, error) {
	if p.rendezvous == "" {
		if f.Addr == "" {
			return "", errors.New("the send names no address")
		}
		return f.Addr, nil
	}
	sock, ok := p.lookup(f.To)
	if !ok {
		return "", fmt.Errorf("no peer for %s", f.To)
	}
	return sock, nil
}

// dial connects to a send's target. An address from a send is dialled only
// at IPs it was checked for (dialChecked); a rendezvous entry, in a test, as
// it is written.
func (p *peer) dial(ctx context.Context, target string) (net.Conn, error) {
	if p.rendezvous != "" {
		dnet, daddr := addrKind(target)
		var d net.Dialer
		return d.DialContext(ctx, dnet, daddr)
	}
	return p.dialChecked(ctx, target)
}

// dialChecked dials addr (host:port) the way SSR-P4 says: the host is
// resolved once, every address it resolves to is checked, and only the
// checked ones that pass are dialled, in order, by IP — never by name again,
// so a second answer from the name's DNS (rebinding) has nothing to change.
// When none passes, the error is a *p2paddr.RefusedError.
func (p *peer) dialChecked(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := p2paddr.Split(addr)
	if err != nil {
		return nil, &p2paddr.RefusedError{Addr: addr, Why: err.Error()}
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		resolve := p.resolve
		if resolve == nil {
			resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		if ips, err = resolve(ctx, host); err != nil {
			return nil, err
		}
	}
	var allowed []netip.Addr
	why := "it resolves to no address"
	for _, ip := range ips {
		ip = ip.Unmap()
		if w := p2paddr.Refused(ip, p.lan); w != "" {
			why = ip.String() + " is " + w
			continue
		}
		allowed = append(allowed, ip)
	}
	if len(allowed) == 0 {
		return nil, &p2paddr.RefusedError{Addr: addr, Why: why}
	}
	dialIP := p.dialIP
	if dialIP == nil {
		dialIP = func(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ap.String())
		}
	}
	var last error
	for _, ip := range allowed {
		c, err := dialIP(ctx, netip.AddrPortFrom(ip, port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// reply writes a frame to the daemon connection dc, if it is still the
// daemon's.
func (p *peer) reply(dc *daemonConn, f frame) {
	p.mu.Lock()
	cur := p.conn
	p.mu.Unlock()
	if cur != dc {
		return
	}
	if err := dc.write(f); err != nil {
		log.Printf("anetpeer: %v", err)
	}
}

// addrKind splits a rendezvous entry into a network and an address.
//
//	tcp://host:port   a peer on another machine
//	host:port         likewise, when it looks like one
//	/path/to.sock     a peer on this machine
//
// Two forms rather than a flag, because the entry is what one peer reads to
// reach another and it has to say which kind it is. A flag would describe
// the local peer and say nothing about the remote one.
func addrKind(a string) (network, addr string) {
	switch {
	case strings.HasPrefix(a, "tcp://"):
		return "tcp", strings.TrimPrefix(a, "tcp://")
	case strings.HasPrefix(a, "unix://"):
		return "unix", strings.TrimPrefix(a, "unix://")
	case strings.Contains(a, "/"):
		return "unix", a
	case strings.Contains(a, ":"):
		return "tcp", a
	default:
		return "unix", a
	}
}

// announce records the AID this process carries and, in a test, writes
// "this AID is reachable at this address" to the rendezvous directory.
// Nothing is published anywhere else: a contact learns this node's address
// from the delivery credential the daemon gives it.
func (p *peer) announce(aid string) {
	if aid == "" {
		return
	}
	p.mu.Lock()
	p.self = aid
	p.mu.Unlock()
	if p.rendezvous == "" {
		return
	}
	// What is written is what a peer should dial, which is not always
	// what this process listens on: --advertise says the second when they
	// differ.
	reach := p.advertise
	if reach == "" {
		reach = p.peerSocket
	}
	if err := writeEntry(p.rendezvous, aid, reach); err != nil {
		log.Printf("anetpeer: cannot write %s to the rendezvous: %v", aid, err)
	}
}

// writeEntry replaces the rendezvous entry for aid with reach in one step:
// written to a temporary file in the same directory and renamed over it.
// Peers read the entry while this process writes it; written in place, a
// peer could read it created or truncated and not yet written — an empty
// address, which it dialled ("dial unix: missing address") and marked down.
func writeEntry(dir, aid, reach string) error {
	tmp, err := os.CreateTemp(dir, ".entry-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // after the rename there is nothing by that name
	if _, err := tmp.WriteString(reach); err != nil {
		tmp.Close()
		return err
	}
	// Peers of other accounts on this host read the directory too.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, aid))
}

func (p *peer) forget() {
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()
	if self == "" || p.rendezvous == "" {
		return
	}
	_ = os.Remove(filepath.Join(p.rendezvous, self))
}

// lookup reads aid's rendezvous entry.
func (p *peer) lookup(aid string) (string, bool) {
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()
	if aid == "" || aid == self || p.rendezvous == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(p.rendezvous, aid))
	if err != nil {
		return "", false
	}
	// An empty entry names nowhere to dial: not listed, not an address
	// ("" would be dialled as a Unix socket and marked down).
	addr := string(bytes.TrimSpace(b))
	return addr, addr != ""
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/p2paddr"
)

// fakeNet stands in for DNS and the network: names resolve as listed, and a
// dial to any IP reaches one local peer wire that accepts every delivery.
// It records every IP dialled.
type fakeNet struct {
	t       *testing.T
	names   map[string][]netip.Addr
	answer  string // the local listener every dial reaches
	mu      sync.Mutex
	dialled []netip.AddrPort
	looked  []string
}

func newFakeNet(t *testing.T, names map[string][]string) *fakeNet {
	t.Helper()
	n := &fakeNet{t: t, names: map[string][]netip.Addr{}, answer: answeringPeer(t)}
	for name, ips := range names {
		n.names[name] = []netip.Addr{}
		for _, ip := range ips {
			n.names[name] = append(n.names[name], netip.MustParseAddr(ip))
		}
	}
	return n
}

func (n *fakeNet) install(p *peer) {
	p.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		n.mu.Lock()
		n.looked = append(n.looked, host)
		n.mu.Unlock()
		ips, ok := n.names[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return ips, nil
	}
	p.dialIP = func(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
		n.mu.Lock()
		n.dialled = append(n.dialled, ap)
		n.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, "tcp", n.answer)
	}
}

func (n *fakeNet) took() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, ap := range n.dialled {
		out = append(out, ap.String())
	}
	n.dialled = nil
	return out
}

// TestDialFilter (SSR-P4, SI-18, M15): a send's address is dialled only at
// IPs that pass the filter — loopback, unspecified, multicast, link-local,
// private, 100.64.0.0/10 and 0.0.0.0/8 refused, the private ones and
// 100.64.0.0/10 let through in LAN mode — whether it is written as an IP
// (IPv4, IPv6, IPv4-mapped) or as a name. A name is resolved once and every
// address it resolves to is checked: only the ones that pass are dialled,
// by IP, so a name that also resolves to a private address (rebinding) gets
// nothing dialled there. A refused address is answered with an error,
// counted p2p-addr-refused, and does not mark the peer down.
func TestDialFilter(t *testing.T) {
	names := map[string][]string{
		"public.example":    {"203.0.113.9"},
		"public6.example":   {"2001:db8::9"},
		"loop.example":      {"127.0.0.1"},
		"mapped.example":    {"::ffff:127.0.0.1"},
		"private.example":   {"10.0.0.7"},
		"cgnat.example":     {"100.64.3.4"},
		"rebind.example":    {"192.168.1.20", "203.0.113.10", "127.0.0.1", "169.254.169.254", "203.0.113.11"},
		"linklocal.example": {"fe80::1", "169.254.0.1"},
		"empty.example":     {},
	}
	cases := []struct {
		addr     string
		off, lan []string // the IPs dialled with LAN mode off, on; nil: refused
	}{
		{"203.0.113.9:4001", []string{"203.0.113.9:4001"}, []string{"203.0.113.9:4001"}},
		{"[2001:db8::9]:4001", []string{"[2001:db8::9]:4001"}, []string{"[2001:db8::9]:4001"}},
		{"public.example:4001", []string{"203.0.113.9:4001"}, []string{"203.0.113.9:4001"}},
		{"public6.example:4001", []string{"[2001:db8::9]:4001"}, []string{"[2001:db8::9]:4001"}},
		{"127.0.0.1:4001", nil, nil},
		{"[::1]:4001", nil, nil},
		{"[::ffff:127.0.0.1]:4001", nil, nil},
		{"[::ffff:10.0.0.7]:4001", nil, []string{"10.0.0.7:4001"}},
		{"0.0.0.0:4001", nil, nil},
		{"0.9.9.9:4001", nil, nil},
		{"[::]:4001", nil, nil},
		{"224.0.0.1:4001", nil, nil},
		{"[ff02::1]:4001", nil, nil},
		{"169.254.169.254:80", nil, nil},
		{"[fe80::1]:4001", nil, nil},
		{"10.0.0.7:4001", nil, []string{"10.0.0.7:4001"}},
		{"172.20.0.1:4001", nil, []string{"172.20.0.1:4001"}},
		{"192.168.0.1:4001", nil, []string{"192.168.0.1:4001"}},
		{"[fd00::7]:4001", nil, []string{"[fd00::7]:4001"}},
		{"100.64.0.1:4001", nil, []string{"100.64.0.1:4001"}},
		{"loop.example:4001", nil, nil},
		{"mapped.example:4001", nil, nil},
		{"private.example:4001", nil, []string{"10.0.0.7:4001"}},
		{"cgnat.example:4001", nil, []string{"100.64.3.4:4001"}},
		{"linklocal.example:4001", nil, nil},
		{"empty.example:4001", nil, nil},
		// Multiple A records with a rebinding mix: only the checked public
		// ones are dialled (the first that answers), never a name again.
		{"rebind.example:4001", []string{"203.0.113.10:4001"}, []string{"192.168.1.20:4001"}},
		{"not-host-port", nil, nil},
		{"public.example:0", nil, nil},
	}
	for _, lan := range []bool{false, true} {
		dir := t.TempDir()
		p, err := startOpts(options{socket: filepath.Join(dir, "a.sock"), peer: filepath.Join(dir, "a.wire"), lan: lan})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.stop)
		n := newFakeNet(t, names)
		n.install(p)
		for _, c := range cases {
			want := c.off
			if lan {
				want = c.lan
			}
			before := p.refusedAddrs.Load()
			_, err := p.deliver(frame{Op: "send", V: wireVersion, ID: "1", To: "aid-far", Envelope: "ZW52", Addr: c.addr})
			got := n.took()
			switch {
			case want == nil && !errors.Is(err, p2paddr.ErrRefused):
				t.Errorf("lan=%v %s: %v, dialled %v; want refused", lan, c.addr, err, got)
			case want == nil && p.refusedAddrs.Load() != before+1:
				t.Errorf("lan=%v %s: refused without being counted", lan, c.addr)
			case want == nil && p.isDown("aid-far", c.addr):
				t.Errorf("lan=%v %s: a refused address marked the peer down", lan, c.addr)
			case want != nil && (err != nil || !slices.Equal(got, want)):
				t.Errorf("lan=%v %s: dialled %v (%v), want %v", lan, c.addr, got, err, want)
			case want == nil && len(got) != 0:
				t.Errorf("lan=%v %s: refused, yet dialled %v", lan, c.addr, got)
			}
		}
		// The name was looked up once per send, and the dial went to the IP.
		n.mu.Lock()
		looked := strings.Count(strings.Join(n.looked, " "), "rebind.example")
		n.mu.Unlock()
		if looked != 1 {
			t.Errorf("lan=%v: rebind.example looked up %d times for one send", lan, looked)
		}
	}
}

// A dial that fails at the first public address goes on to the next one the
// name resolved to — the checked list, in order, and nothing else.
func TestADialGoesThroughTheCheckedAddressesInOrder(t *testing.T) {
	dir := t.TempDir()
	p, err := startOpts(options{socket: filepath.Join(dir, "a.sock"), peer: filepath.Join(dir, "a.wire")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	n := newFakeNet(t, map[string][]string{"multi.example": {"203.0.113.1", "10.0.0.1", "203.0.113.2"}})
	n.install(p)
	inner := p.dialIP
	p.dialIP = func(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
		if ap.Addr() == netip.MustParseAddr("203.0.113.1") {
			n.mu.Lock()
			n.dialled = append(n.dialled, ap)
			n.mu.Unlock()
			return nil, errors.New("connection refused")
		}
		return inner(ctx, ap)
	}
	if _, err := p.deliver(frame{Op: "send", V: wireVersion, ID: "1", To: "aid-far", Envelope: "ZW52", Addr: "multi.example:7"}); err != nil {
		t.Fatal(err)
	}
	if got := n.took(); !slices.Equal(got, []string{"203.0.113.1:7", "203.0.113.2:7"}) {
		t.Fatalf("dialled %v", got)
	}
}

// End to end, from the daemon's socket: a send frame's address is the
// only thing dialled, its answer reaches the daemon, and a refused address
// is answered with an error at once — the daemon goes to the hub.
func TestASendDialsItsAddressOnly(t *testing.T) {
	dir := t.TempDir()
	p, err := startOpts(options{socket: filepath.Join(dir, "a.sock"), peer: filepath.Join(dir, "a.wire")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	n := newFakeNet(t, nil)
	n.install(p)
	d := dialRawDaemon(t, filepath.Join(dir, "a.sock"))
	d.send(frame{Op: "hello", V: wireVersion, Self: "aid-alice"})
	d.send(frame{Op: "send", V: wireVersion, ID: "s1", To: "aid-far", Envelope: "ZW52", Addr: "203.0.113.5:39100"})
	reply, _, err := d.read()
	if err != nil || reply.Op != "send" || reply.ID != "s1" || reply.Error != "" {
		t.Fatalf("answer %+v, %v", reply, err)
	}
	if got := n.took(); !slices.Equal(got, []string{"203.0.113.5:39100"}) {
		t.Fatalf("dialled %v", got)
	}
	d.send(frame{Op: "send", V: wireVersion, ID: "s2", To: "aid-far", Envelope: "ZW52", Addr: "192.168.7.7:39100"})
	reply, _, err = d.read()
	if err != nil || reply.Op != "send" || reply.ID != "s2" || !strings.Contains(reply.Error, "not dialled") {
		t.Fatalf("answer %+v, %v; want the send refused for its address", reply, err)
	}
	if got := n.took(); len(got) != 0 {
		t.Fatalf("a refused address was dialled: %v", got)
	}
}

// SSR-P3: this process asks no hub where anyone is. Its code has no HTTP
// client and no hub route; the rendezvous is a directory and only a
// directory, used only for tests.
func TestAnetpeerHasNoHubLookup(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		if strings.HasPrefix(path, "net/http") || path == "net/url" {
			t.Errorf("main.go imports %s", path)
		}
	}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			continue
		}
		inspectNoHub(t, fset, d)
	}
	// A rendezvous given as a URL is a directory of that name, not a hub.
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	p := &peer{rendezvous: "https://hub.example.org", self: "aid-local"}
	if addr, ok := p.lookup("aid-remote"); ok {
		t.Fatalf("a URL rendezvous answered %q", addr)
	}
}

// inspectNoHub reports a hub route, URL or lookup function in d.
func inspectNoHub(t *testing.T, fset *token.FileSet, d ast.Decl) {
	t.Helper()
	ast.Inspect(d, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING && (strings.Contains(x.Value, "/p2p") || strings.Contains(x.Value, "/agents/") ||
				strings.Contains(x.Value, "http://") || strings.Contains(x.Value, "https://")) {
				t.Errorf("%s: %s", fset.Position(x.Pos()), x.Value)
			}
		case *ast.FuncDecl:
			switch x.Name.Name {
			case "lookupHub", "askHub", "isURL":
				t.Errorf("main.go still has %s", x.Name.Name)
			}
		case *ast.Field:
			for _, nm := range x.Names {
				if nm.Name == "hubCache" {
					t.Error("main.go still has hubCache")
				}
			}
		}
		return true
	})
}

// A peer whose daemon stopped reading does not hold this process: a write
// to it gives up after daemonWriteTimeout and closes the connection, and
// meanwhile the peer's lock is free — another delivery, the accept loop and
// the answers to other sends go on.
func TestAWriteToTheDaemonHasADeadlineAndHoldsNoLock(t *testing.T) {
	old := daemonWriteTimeout
	daemonWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { daemonWriteTimeout = old })
	ours, theirs := net.Pipe() // a pipe has no buffer: nothing is written until the far end reads
	defer theirs.Close()
	p := &peer{acks: map[string]chan handOffAnswer{}, limits: newSourceLimits()}
	go p.serveDaemon(ours)
	enc := json.NewEncoder(theirs)
	go func() { _ = enc.Encode(frame{Op: "hello", V: wireVersion, Self: "aid-self"}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		attached := p.conn != nil
		p.mu.Unlock()
		if attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never attached")
		}
		time.Sleep(time.Millisecond)
	}
	// The daemon reads nothing now. A hand-off writes a recv frame to it.
	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := p.handOff(frame{Envelope: strings.Repeat("A", 1<<20)}); done <- err }()
	// While that write waits, the peer's lock is free.
	locked := make(chan struct{})
	go func() { p.mu.Lock(); p.mu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("a write to the daemon holds the peer's lock")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hand-off to a daemon that does not read succeeded")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("the write gave up after %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write to a daemon that does not read has no deadline")
	}
	// The connection was closed: the daemon's place is free.
	deadline = time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		gone := p.conn == nil
		p.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection a write gave up on still holds the daemon's place")
		}
		time.Sleep(time.Millisecond)
	}
}

// A connection from another peer that sends nothing, or a line without
// end, is closed after the time a sender waits for the whole delivery, or
// at maxFrameLen; neither reaches the daemon.
func TestAPeerConnectionIsBoundedInTimeAndSize(t *testing.T) {
	oldH, oldR := handOffTimeout, replyMargin
	handOffTimeout, replyMargin = 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { handOffTimeout, replyMargin = oldH, oldR })
	dir := t.TempDir()
	p, err := start(filepath.Join(dir, "a.sock"), filepath.Join(dir, "a.wire"), filepath.Join(dir, "rv"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stopAndWait)

	silent, err := net.Dial("unix", filepath.Join(dir, "a.wire"))
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	_ = silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("a silent connection was answered")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a silent connection was held %s", d)
	}

}

// stopAndWait stops p and waits for the peer connections it was serving,
// so that a test which changed handOffTimeout or replyMargin puts them back
// only after the last read of them.
func (p *peer) stopAndWait() {
	p.stop()
	p.inbound.Wait()
}

// A line is read up to its bound and no further: one without end fails once
// it is past the bound, having taken at most the bound and one buffer.
func TestReadLineIsBounded(t *testing.T) {
	src := &countingReader{}
	_, err := readLine(bufio.NewReaderSize(src, 16), 100)
	if err == nil {
		t.Fatal("an endless line was read")
	}
	if src.n > 100+16+16 {
		t.Fatalf("%d bytes read for a bound of 100", src.n)
	}
	line, err := readLine(bufio.NewReader(strings.NewReader("abc\nrest")), 3)
	if err != nil || string(line) != "abc" {
		t.Fatalf("%q %v", line, err)
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("abcd\n")), 3); err == nil {
		t.Fatal("a line one byte over the bound was read")
	}
}

// countingReader is a line of 'A' with no end in sight: 64 KiB, far over
// any bound the test sets, and then EOF. It ends so that a readLine that
// lost its bound reads it all and the test fails on the count, rather than
// growing one line until the test is out of memory.
type countingReader struct{ n int }

const countingReaderLen = 64 << 10

func (r *countingReader) Read(b []byte) (int, error) {
	if r.n >= countingReaderLen {
		return 0, io.EOF
	}
	b = b[:min(len(b), countingReaderLen-r.n)]
	for i := range b {
		b[i] = 'A'
	}
	r.n += len(b)
	return len(b), nil
}

// Frames that are not frame v3 as core §2.10 has it are bad frames
// (core CORE-K13a), on every leg: a member the op's row does not have, one
// missing that it must have, a member of the wrong type, or another version.
func TestStrictFramesOnEveryLeg(t *testing.T) {
	env := "ZW52"
	for _, c := range []struct {
		leg  map[string]frameRow
		line string
		bad  bool
	}{
		{fromDaemon, `{"op":"hello","v":3,"self":"a"}`, false},
		{fromDaemon, `{"op":"hello","v":3,"self":"a","lan":true}`, true},
		{fromDaemon, `{"op":"hello","v":3}`, true},
		{fromDaemon, `{"op":"send","v":3,"id":"1","to":"b","envelope":"` + env + `","addr":"h:1"}`, false},
		{fromDaemon, `{"op":"send","v":3,"id":"1","to":"b","envelope":"` + env + `"}`, true},
		{fromDaemon, `{"op":"send","v":3,"id":"1","to":"b","envelope":"` + env + `","addr":"h:1","reach":true}`, true},
		{fromDaemon, `{"op":"send","v":3,"id":1,"to":"b","envelope":"` + env + `","addr":"h:1"}`, true},
		{fromDaemon, `{"op":"ack","v":3,"id":"d1","dack":""}`, false},
		{fromDaemon, `{"op":"ack","v":3,"id":"d1","dack":null}`, true},
		{fromDaemon, `{"op":"ack","v":3,"id":"d1","outcome":1}`, true},
		{fromDaemon, `{"op":"nack","v":3,"id":"d1"}`, true},
		{fromDaemon, `{"op":"recv","v":3,"id":"d1","envelope":"` + env + `"}`, true},
		{fromDaemon, `{"op":"send","v":"3","id":"1","to":"b","envelope":"` + env + `","addr":"h:1"}`, true},
		{fromPeer, `{"op":"send","v":3,"id":"1","to":"b","envelope":"` + env + `"}`, false},
		{fromPeer, `{"op":"send","v":3,"id":"1","to":"b","envelope":"` + env + `","addr":"h:1"}`, true},
		{fromPeer, `{"op":"send","v":3,"id":"1","to":"b","envelope":"` + env + `","not_delivered":true}`, true},
		{fromPeer, `{"op":"hello","v":3,"self":"a"}`, true},
		{answerFromPeer, `{"op":"send","v":3,"id":"1","dack":"x","error":"e"}`, false},
		{answerFromPeer, `{"op":"send","v":3,"id":"1","reachable":false}`, true},
		{answerFromPeer, `{"op":"error","v":3,"error":"busy"}`, false},
		{answerFromPeer, `{"op":"send","v":2,"id":"1"}`, true},
	} {
		_, bad, err := readFrame([]byte(c.line), c.leg)
		if err != nil || (bad != "") != c.bad {
			t.Errorf("%s: bad %q err %v, want bad=%v", c.line, bad, err, c.bad)
		}
	}
	if _, _, err := readFrame([]byte(`[1]`), fromDaemon); err == nil {
		t.Error("a line that is not an object read as a frame")
	}
}

// A receiving peer's answer with a member frame v3 does not have is no
// answer: the send fails and nothing in it, a dack included, is passed on.
// An answer that is not even JSON is the same.
func TestABadAnswerFromTheReceivingPeerCarriesNothing(t *testing.T) {
	for _, line := range []string{
		`{"op":"send","v":3,"id":"1","dack":"AAAA","outcome":"processed"}`,
		`{"op":"send","v":3,"id":"2","dack":"AAAA"}`, // the id of another delivery
		`not json`,
	} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = bufio.NewReader(c).ReadBytes('\n')
			_, _ = c.Write([]byte(line + "\n"))
		}()
		rv := t.TempDir()
		if err := os.WriteFile(filepath.Join(rv, "aid-far"), []byte(l.Addr().String()), 0o644); err != nil {
			t.Fatal(err)
		}
		p := &peer{rendezvous: rv, self: "aid-local"}
		d, err := p.deliver(frame{Op: "send", V: wireVersion, ID: "1", To: "aid-far", Envelope: "ZW52"})
		if err == nil || d.set {
			t.Errorf("%s: dack %+v err %v; want a failure carrying nothing", line, d, err)
		}
		if line != "not json" && p.badFrames.Load() != 1 {
			t.Errorf("%s: counted %d bad frames", line, p.badFrames.Load())
		}
		l.Close()
	}
}

// The envelope rule is the daemon's (internal/hubapi DecodeB64): checked
// against the same inputs, the two take and refuse the same strings.
func TestTheEnvelopeRuleIsTheDaemons(t *testing.T) {
	for _, s := range []string{"ZW52", "ZW52ZWxvcGU", "ZW52ZWxvcGU=", "+/+/", "ZW52\nZW52", " ZW52", "ZW52ZWxvcGV", "", "A",
		"ZW52é", "-_-_"} {
		_, mine := decodeB64(s)
		_, theirs := hubDecodeB64(s)
		if (mine == nil) != (theirs == nil) {
			t.Errorf("%q: anetpeer %v, daemon %v", s, mine, theirs)
		}
	}
	if checkEnvelope("") == "" || checkEnvelope("ZW52 ") == "" || checkEnvelope("ZW52") != "" {
		t.Error("checkEnvelope")
	}
}

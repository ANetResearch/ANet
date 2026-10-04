//go:build !no_p2p

package p2p

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/module"
)

// The peer wire.
//
// Newline-delimited JSON rather than the CoreDet-CBOR that ADAP uses, and
// the difference is deliberate. ADAP carries EffectRecords — objects that
// are already CoreDet-CBOR elsewhere in the family, where a re-encode would
// be both waste and a chance to lose fidelity. This carries an opaque
// sealed envelope that only the recipient daemon can open; the frame around
// it is addressing, nothing more. A peer process in another language should
// be able to speak it with its standard library.
//
// A frame carries the recipient AID, the envelope bytes and an ID (A2A-DESIGN
// §3.10). It carries no sender, no message kind and no interaction id: those
// are inside the envelope, encrypted to the recipient, and a peer process
// has no use for them.
//
// Frame v3 (testdata/frame-v3.json is the golden of every op). What a frame
// can say about delivery is the recipient's delivery confirmation (dack),
// carried back opaque, and nothing else: no field says whether a message was
// processed or refused, and nothing a peer process answers on its own counts
// as "delivered". The v2 frame let a peer process declare a failed send as
// having reached nobody (not_delivered) and answer whether an AID was
// reachable (the reach frame); both are gone. Binary values are base64url
// without padding.
type frame struct {
	Op string `json:"op"`
	// V is the frame version, on every frame. An endpoint refuses a frame
	// of any other version: a hello with an error frame and a closed
	// connection, a delivery with an error frame and without handing it to
	// the daemon.
	V int `json:"v"`
	// ID correlates a request with its reply, and a peer process must echo
	// it back unchanged.
	//
	// For a delivery the peer process hands to the daemon (op recv), the
	// peer process mints a fresh ID per delivery and the daemon acks with
	// that ID. It is the only thing the ack can be matched on: two
	// deliveries in flight can have the same recipient and even the same
	// envelope bytes (a retry that overlaps the first attempt).
	ID string `json:"id,omitempty"`
	// To is the recipient AID. An AID, never a peer id: what the peer stack
	// calls its nodes is its own business.
	To string `json:"to,omitempty"`
	// Envelope is the sealed envelope. Opaque to the peer process, which
	// cannot read or forge it.
	Envelope string `json:"envelope,omitempty"`
	// Addr, on a send from the daemon to its peer process, is where the
	// recipient said it can be dialled: the p2p address of the contact
	// grant it gave this node.
	Addr string `json:"addr,omitempty"`
	// DAck is the recipient daemon's delivery confirmation: on its ack, and
	// on the answer to the send that carried the envelope. A peer process
	// passes it on as it came and makes nothing of it.
	DAck string `json:"dack,omitempty"`
	Self string `json:"self,omitempty"`
	// Error is a sentence for the log of whoever reads it. It decides
	// nothing.
	Error string `json:"error,omitempty"`
}

// WireVersion is the frame version this module speaks (A2A-DESIGN §3.10).
const WireVersion = 3

const (
	opHello = "hello"
	opSend  = "send"
	opRecv  = "recv"
	opAck   = "ack"
	opNack  = "nack"
	opError = "error"
)

// encodeB64 is how a frame carries bytes: base64url without padding.
func encodeB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// decodeB64 decodes base64url without padding and refuses every other
// spelling of the same bytes: "=", "+", "/", white space, and trailing bits
// that are not zero. One value, one spelling.
func decodeB64(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return nil, fmt.Errorf("not base64url without padding (byte %d)", i)
		}
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

// versionRefused is the error sentence for a frame of another version.
func versionRefused(v int) string {
	return fmt.Sprintf("frame version %d refused: this node speaks frame version %d (anet 0.3.0)", v, WireVersion)
}

// Transport delivers over a peer process. It satisfies module.Transport.
type Transport struct {
	socket      string
	selfAID     string
	sendTimeout time.Duration
	inbound     module.Inbound
	// badDAck counts an answer whose dack is malformed (the daemon's
	// dack-bad-frame); nil counts nothing.
	badDAck func()

	// listenerUID is the account the peer process must run as; nil is this
	// daemon's own (LEC §4.4). Tests set another to stand for a peer process
	// of another account.
	listenerUID *int

	mu     sync.Mutex
	conn   net.Conn
	closed bool
	// wmu serialises the writes to the session connection (write). It is
	// not mu: a write can wait on a peer process that has stopped reading,
	// for up to sessionWriteTimeout, and the read loop, Reachable and the
	// other senders must not wait with it.
	wmu sync.Mutex

	// pending maps a request id to the caller waiting for its reply.
	pending map[string]chan frame
	nextID  uint64
}

func (t *Transport) Name() string { return name }

// run keeps a connection to the peer process, reconnecting with backoff.
//
// Reconnecting rather than failing is what makes this an optimisation
// rather than a dependency: the peer stack can be restarted, upgraded or
// crash, and delegations keep flowing through the hub while it is away.
func (t *Transport) run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		if err := t.session(ctx); err != nil && ctx.Err() == nil {
			log.Printf("anet: p2p peer process: %v (retrying in %s; hub is carrying delivery)",
				err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// dial connects to the peer process through backendconn (LEC PC-3): the
// socket's path must be one only this daemon's user or root can change, and
// the process listening on it must run as this daemon's user. Anything else
// listening there — an account that took the path while the peer process was
// down — is sent nothing, not even the hello naming this node's AID.
func (t *Transport) dial(ctx context.Context) (net.Conn, error) {
	uid := os.Getuid()
	if t.listenerUID != nil {
		uid = *t.listenerUID
	}
	rules, err := backendconn.Policy{ExpectedUser: strconv.Itoa(uid)}.Resolve()
	if err != nil {
		return nil, err
	}
	return rules.DialUnix(ctx, nil, t.socket)
}

func (t *Transport) session(ctx context.Context) error {
	c, err := t.dial(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		c.Close()
		return errors.New("closed")
	}
	t.conn, t.pending = c, map[string]chan frame{}
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		if t.conn == c {
			t.conn = nil
			// Everyone still waiting on this session is waiting forever.
			// Closing their channels turns that into a prompt error and a
			// fall-through to the hub, rather than each caller sitting out
			// its own timeout.
			for id, ch := range t.pending {
				close(ch)
				delete(t.pending, id)
			}
			t.pending = nil
		}
		t.mu.Unlock()
		c.Close()
	}()

	// Tell the peer process which AID it is carrying for, so it can
	// announce us to the network under the identity the daemon owns.
	if err := t.write(c, frame{Op: opHello, V: WireVersion, Self: t.selfAID}); err != nil {
		return err
	}

	dec := json.NewDecoder(bufio.NewReader(c))
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			return err
		}
		switch f.Op {
		case opHello:
			// A peer process that greets back says which version it
			// speaks. Another one cannot carry this node's traffic: say
			// so once and hang up, rather than trade frames neither side
			// reads the same way.
			if f.V != WireVersion {
				_ = t.write(c, frame{Op: opError, V: WireVersion, Error: versionRefused(f.V)})
				return fmt.Errorf("the peer process speaks frame version %d and this node %d: run a peer process of anet 0.3.0", f.V, WireVersion)
			}
		case opRecv:
			// Off the loop, and this is the whole of the bug it fixes.
			//
			// Handling an inbound delegation means answering it, answering
			// means Send, and Send waits for a reply that only this loop can
			// deliver. Run inline, the loop waited on the daemon while the
			// daemon waited on the loop, and it unwound on Send's timeout —
			// so the transport reported failure and every capability call
			// fell through to the hub. A transport that cannot carry a
			// reply is not carrying anything.
			go t.deliverInbound(ctx, c, f)
		default:
			t.mu.Lock()
			ch, waiting := t.pending[f.ID]
			if waiting {
				delete(t.pending, f.ID)
			}
			t.mu.Unlock()
			if !waiting {
				// A reply nobody is waiting for: the request timed out and
				// moved on, or the peer process answered something it was
				// never asked. Dropping it is right — the delegation
				// already went to the hub, and acting on it now would be a
				// duplicate.
				continue
			}
			ch <- f
		}
	}
}

// deliverInbound hands a received envelope to the daemon and acks it with
// the delivery's own ID once the daemon has decided it should be acked:
// accepted (the daemon answers as soon as its step 10 has committed, not
// after the work that follows), or refused for a reason that will not
// change on retry. The ack carries the daemon's delivery confirmation as the
// daemon made it. A temporary refusal is answered with a nack carrying the
// reason, so the peer process reports the delivery as failed at once and
// the sender falls back to the hub instead of waiting out the peer
// process's hand-off timeout (A2A-DESIGN §3.6, §3.10; an internal note item).
func (t *Transport) deliverInbound(ctx context.Context, c net.Conn, f frame) {
	env, refusal := checkRecv(f)
	if env == nil {
		log.Printf("anet: p2p: refusing a delivery frame from the peer process: %s", refusal)
		if f.ID != "" || f.V != WireVersion {
			_ = t.write(c, frame{Op: opError, V: WireVersion, ID: f.ID, Error: refusal})
		}
		return
	}
	// The envelope is end-to-end sealed and signed, so the daemon checks it
	// the same way it checks anything from the hub mailbox. A peer process
	// that lies produces an envelope that fails to open or verify, not one
	// that gets trusted because it arrived over a direct connection.
	dack, err := t.inbound.Receive(ctx, env)
	if err != nil {
		log.Printf("anet: p2p: inbound delivery %s not acknowledged: %v", f.ID, err)
		_ = t.write(c, frame{Op: opNack, V: WireVersion, ID: f.ID, Error: err.Error()})
		return
	}
	_ = t.write(c, frame{Op: opAck, V: WireVersion, ID: f.ID, DAck: dackText(dack)})
}

// checkRecv decides whether a recv frame goes to the daemon: its envelope,
// or why not. A frame of another version is refused before anything else
// in it is read; one without an id cannot be acked; an envelope in any
// spelling but base64url without padding, or an empty one, is not an
// envelope.
func checkRecv(f frame) (env []byte, refusal string) {
	if f.V != WireVersion {
		return nil, versionRefused(f.V)
	}
	if f.ID == "" {
		return nil, "a delivery frame without an id"
	}
	env, err := decodeB64(f.Envelope)
	if err == nil && len(env) == 0 {
		err = errors.New("empty")
	}
	if err != nil {
		return nil, "the envelope is not base64url without padding: " + err.Error()
	}
	return env, ""
}

// dackText is a confirmation as a frame carries it, "" for none.
func dackText(dack []byte) string {
	if len(dack) == 0 {
		return ""
	}
	return encodeB64(dack)
}

// write serialises one frame onto the session connection.
//
// One writer, because there are now several: request sends, inbound acks,
// and the opening hello. Two writers on one connection interleave their
// bytes, and an interleaved frame is a connection the peer process can no
// longer parse — it loses not one message but every message after it.
//
// The connection is passed in rather than read from the struct so a
// handler that outlives its session writes to the connection it was
// serving, never the next one.
func (t *Transport) write(c net.Conn, f frame) error {
	_, err := t.writeN(c, f)
	return err
}

// sessionWriteTimeout bounds one frame's write to the peer process. A peer
// process on this machine reads as it goes, and the largest frame it carries
// is written in milliseconds; one that has stopped reading would otherwise
// hold the write — and every send queued behind it, and the acks of what it
// delivered — for as long as it chose. A variable so a test can shorten it.
var sessionWriteTimeout = 5 * time.Second

// writeN is write that also says how many bytes reached the connection,
// which for a send is whether the envelope may have left this process.
//
// A write that fails, the deadline's included, ends the session: a frame
// cut short leaves a stream the peer process can no longer read, and one
// that does not read is carrying nothing. Closing the connection ends the
// read loop, which tells everyone waiting on the session at once; their
// sends go through the hub, and run dials the peer process again.
func (t *Transport) writeN(c net.Conn, f frame) (int, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return 0, err
	}
	b = append(b, '\n')
	t.mu.Lock()
	live := t.conn == c
	t.mu.Unlock()
	if !live {
		return 0, errors.New("p2p: session closed")
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(sessionWriteTimeout))
	n, err := c.Write(b)
	if err != nil {
		_ = c.Close()
		return n, fmt.Errorf("p2p: writing to the peer process: %w", err)
	}
	_ = c.SetWriteDeadline(time.Time{})
	return n, nil
}

// Reachable reports whether the peer process is there to hand an envelope
// to. It asks the peer process nothing: whether the recipient can be
// reached is not something a peer process gets to answer, and a send to an
// AID it cannot reach comes back as a failure the caller falls through on.
func (t *Transport) Reachable(ctx context.Context, toAID string) bool {
	// v030-todo(SS-11): true only when this node holds a contact grant from
	// toAID that carries a p2p address (engine ENG-I7), which Send puts in
	// the frame's addr.
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn != nil
}

// Send hands one envelope to the peer process and waits for its answer.
//
// handed is false only when the frame never reached the peer process's
// socket: no session, a cancellation before the write, a write that failed
// before its first byte. From the first byte written it is true, whatever
// the answer: a peer process that has the envelope may deliver it even
// while it answers an error, or after the answer timed out here.
//
// dack is the confirmation in the answer, decoded; nil when the answer has
// none, or one that is not base64url of DAckLen bytes, which is counted
// (dack-bad-frame) and otherwise treated as none. Whether it is the
// recipient's is for the caller to check: this transport has no key to
// check it with, and does not try.
func (t *Transport) Send(ctx context.Context, toAID string, envelope []byte) ([]byte, bool, error) {
	sctx, cancel := context.WithTimeout(ctx, t.sendTimeout)
	defer cancel()
	reply, handed, err := t.roundTrip(sctx, frame{
		Op: opSend, V: WireVersion, To: toAID,
		Envelope: encodeB64(envelope),
		// v030-todo(SS-11): Addr, from the recipient's contact grant.
	})
	if err != nil {
		return nil, handed, err
	}
	dack, bad, err := readAnswer(reply)
	if bad && t.badDAck != nil {
		t.badDAck()
	}
	return dack, true, err
}

// readAnswer reads the peer process's answer to a send: the confirmation in
// it, whether a confirmation was there but malformed (bad), and the failure
// it reports. Only a send answer of this version carries a confirmation;
// one that is not base64url without padding, or not DAckLen bytes, is none.
func readAnswer(reply frame) (dack []byte, bad bool, err error) {
	if reply.V != WireVersion {
		return nil, false, fmt.Errorf("p2p: the peer process answered in frame version %d, not %d", reply.V, WireVersion)
	}
	if reply.Op == opSend && reply.DAck != "" {
		b, derr := decodeB64(reply.DAck)
		if derr == nil && len(b) == module.DAckLen {
			dack = b
		} else {
			bad = true
		}
	}
	switch {
	case reply.Op != opSend && reply.Error == "":
		return dack, bad, fmt.Errorf("p2p: the peer process answered a send with op %q", reply.Op)
	case reply.Error != "":
		return dack, bad, fmt.Errorf("p2p: %s", reply.Error)
	}
	return dack, bad, nil
}

// roundTrip sends one frame and waits for the reply carrying its id. handed
// reports whether any byte of the frame reached the connection.
func (t *Transport) roundTrip(ctx context.Context, f frame) (frame, bool, error) {
	ch := make(chan frame, 1)
	t.mu.Lock()
	conn := t.conn
	if conn == nil || t.pending == nil {
		t.mu.Unlock()
		return frame{}, false, errors.New("p2p: peer process not connected")
	}
	t.nextID++
	f.ID = strconv.FormatUint(t.nextID, 10)
	t.pending[f.ID] = ch
	t.mu.Unlock()

	forget := func() {
		t.mu.Lock()
		if t.pending != nil {
			delete(t.pending, f.ID)
		}
		t.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		forget()
		return frame{}, false, err
	}
	if n, err := t.writeN(conn, f); err != nil {
		forget()
		return frame{}, n > 0, err
	}
	select {
	case <-ctx.Done():
		forget()
		return frame{}, true, ctx.Err()
	case r, ok := <-ch:
		if !ok {
			return frame{}, true, errors.New("p2p: peer process went away")
		}
		return r, true, nil
	}
}

func (t *Transport) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.conn != nil {
		return t.conn.Close()
	}
	return nil
}

var _ module.Transport = (*Transport)(nil)

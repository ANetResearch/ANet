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
	"strconv"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// The peer wire.
//
// Newline-delimited JSON rather than the CoreDet-CBOR that ADAP uses, and
// the difference is deliberate. ADAP carries EffectRecords — objects that
// are already CoreDet-CBOR elsewhere in the family, where a re-encode would
// be both waste and a chance to lose fidelity. This carries an opaque
// payload that the daemon signed and will verify itself; the frame around
// it is addressing, nothing more. A peer process in another language should
// be able to speak it with its standard library.
type frame struct {
	Op string `json:"op"`
	// ID correlates a request with its reply, and a peer process must echo
	// it back unchanged.
	//
	// This wire had no ids: replies were matched first-in-first-out on the
	// grounds that a peer answers in the order it was asked. That is a
	// constraint the daemon cannot check and a peer author will not think
	// about — the first peer process written against this wire broke it
	// within the hour, by handling a delivery off its read loop so it would
	// not deadlock. Under load the failure is a delegation reported
	// delivered because another one succeeded, which is the worst kind: it
	// looks like success.
	ID string `json:"id,omitempty"`
	// Addressing. AIDs, never peer ids: what the peer stack calls its nodes
	// is its own business.
	To   string `json:"to,omitempty"`
	From string `json:"from,omitempty"`
	Kind string `json:"kind,omitempty"`
	IX   string `json:"ix,omitempty"`
	// Payload is opaque and end-to-end verifiable — the peer process cannot
	// forge it and does not need to understand it.
	Payload string `json:"payload,omitempty"`
	// Reachable answers an op:"reach" query.
	Reachable bool   `json:"reachable,omitempty"`
	Error     string `json:"error,omitempty"`
	Self      string `json:"self,omitempty"`
}

const (
	opHello = "hello"
	opReach = "reach"
	opSend  = "send"
	opRecv  = "recv"
	opAck   = "ack"
)

// Transport delivers over a peer process. It satisfies module.Transport.
type Transport struct {
	socket       string
	selfAID      string
	dialTimeout  time.Duration
	reachTimeout time.Duration
	inbound      module.Inbound

	mu     sync.Mutex
	conn   net.Conn
	enc    *json.Encoder
	closed bool

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

func (t *Transport) session(ctx context.Context) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", t.socket)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		c.Close()
		return errors.New("closed")
	}
	t.conn, t.enc, t.pending = c, json.NewEncoder(c), map[string]chan frame{}
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		if t.conn == c {
			t.conn, t.enc = nil, nil
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
	if err := t.write(c, frame{Op: opHello, Self: t.selfAID}); err != nil {
		return err
	}

	dec := json.NewDecoder(bufio.NewReader(c))
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			return err
		}
		switch f.Op {
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

// deliverInbound hands a received message to the daemon.
func (t *Transport) deliverInbound(ctx context.Context, c net.Conn, f frame) {
	payload, err := base64.StdEncoding.DecodeString(f.Payload)
	if err != nil {
		log.Printf("anet: p2p: malformed inbound payload: %v", err)
		return
	}
	// The payload is end-to-end verifiable, so the daemon checks it the
	// same way it checks anything from the hub mailbox. A peer process that
	// lies produces a message that fails verification, not one that gets
	// trusted because it arrived over a direct connection.
	if err := t.inbound.Receive(ctx, f.From, f.Kind, f.IX, payload); err != nil {
		log.Printf("anet: p2p: inbound %s from %s: %v", f.Kind, f.From, err)
		return
	}
	_ = t.write(c, frame{Op: opAck, IX: f.IX})
}

// write serialises one frame onto the session connection.
//
// One writer, because there are now several: request sends, inbound acks,
// and the opening hello. Two json.Encoders on one connection interleave
// their bytes, and an interleaved frame is a connection the peer process
// can no longer parse — it loses not one message but every message after
// it.
//
// The connection is passed in rather than read from the struct so a
// handler that outlives its session writes to the connection it was
// serving, never the next one.
func (t *Transport) write(c net.Conn, f frame) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != c || t.enc == nil {
		return errors.New("p2p: session closed")
	}
	return t.enc.Encode(f)
}

// Reachable asks the peer process whether it can reach an AID right now.
func (t *Transport) Reachable(ctx context.Context, toAID string) bool {
	rctx, cancel := context.WithTimeout(ctx, t.reachTimeout)
	defer cancel()
	reply, err := t.roundTrip(rctx, frame{Op: opReach, To: toAID})
	return err == nil && reply.Reachable
}

// Send delivers one payload to a peer.
func (t *Transport) Send(ctx context.Context, toAID, kind, ix string, payload []byte) error {
	sctx, cancel := context.WithTimeout(ctx, t.dialTimeout)
	defer cancel()
	reply, err := t.roundTrip(sctx, frame{
		Op: opSend, To: toAID, From: t.selfAID, Kind: kind, IX: ix,
		Payload: base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("p2p: %s", reply.Error)
	}
	return nil
}

// roundTrip sends one frame and waits for the reply carrying its id.
func (t *Transport) roundTrip(ctx context.Context, f frame) (frame, error) {
	ch := make(chan frame, 1)
	t.mu.Lock()
	conn := t.conn
	if conn == nil || t.pending == nil {
		t.mu.Unlock()
		return frame{}, errors.New("p2p: peer process not connected")
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
	if err := t.write(conn, f); err != nil {
		forget()
		return frame{}, err
	}
	select {
	case <-ctx.Done():
		forget()
		return frame{}, ctx.Err()
	case r, ok := <-ch:
		if !ok {
			return frame{}, errors.New("p2p: peer process went away")
		}
		return r, nil
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

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
	br     *bufio.Reader
	closed bool

	// pending correlates a request with its reply. The peer process answers
	// in order on one connection, so a queue is enough and there is no need
	// for request ids on the wire.
	replies chan frame
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
	replies := make(chan frame, 8)

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		c.Close()
		return errors.New("closed")
	}
	t.conn, t.enc, t.br, t.replies = c, json.NewEncoder(c), bufio.NewReader(c), replies
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		if t.conn == c {
			t.conn, t.enc, t.br, t.replies = nil, nil, nil, nil
		}
		t.mu.Unlock()
		c.Close()
		close(replies)
	}()

	// Tell the peer process which AID it is carrying for, so it can
	// announce us to the network under the identity the daemon owns.
	if err := json.NewEncoder(c).Encode(frame{Op: opHello, Self: t.selfAID}); err != nil {
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
			t.deliverInbound(ctx, c, f)
		default:
			select {
			case replies <- f:
			default:
				// A reply nobody is waiting for: the request timed out and
				// moved on. Dropping it is right — the delegation already
				// went to the hub, and delivering it now would be a
				// duplicate.
			}
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
	_ = json.NewEncoder(c).Encode(frame{Op: opAck, IX: f.IX})
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

// roundTrip sends one frame and waits for the next reply.
func (t *Transport) roundTrip(ctx context.Context, f frame) (frame, error) {
	t.mu.Lock()
	enc, replies := t.enc, t.replies
	t.mu.Unlock()
	if enc == nil {
		return frame{}, errors.New("p2p: peer process not connected")
	}
	if err := enc.Encode(f); err != nil {
		return frame{}, err
	}
	select {
	case <-ctx.Done():
		return frame{}, ctx.Err()
	case r, ok := <-replies:
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

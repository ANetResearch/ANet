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
// This is that implementation, at the smallest size that is honest. It
// carries real traffic between real daemons on one machine: each peer
// listens for its daemon on one socket and for other peers on another, and
// finds them through a shared rendezvous directory instead of a DHT. What
// it is not is a substitute for the libp2p peer — there is no NAT
// traversal, no relay, no discovery beyond the directory. What it is, is a
// complete statement of the contract a real peer has to satisfy, in about
// two hundred lines you can read.
//
//	anetpeer --socket run/a/peer.sock --peer run/a/wire.sock --rendezvous run/rv
//
// Delivery is acknowledged end to end: this process does not tell the
// sender "delivered" until the receiving daemon has accepted the payload.
// A transport that acks on its own receipt turns a dropped delegation into
// a silent one, and the whole point of the transport list is that a failed
// Send falls through to the hub.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// frame is the module's wire, both sides of it.
type frame struct {
	Op string `json:"op"`
	// ID correlates a request with its reply and must be echoed unchanged.
	// A peer that answers concurrently — which this one does, because
	// answering in its read loop deadlocks — would otherwise have its
	// replies matched to the wrong requests.
	ID        string `json:"id,omitempty"`
	To        string `json:"to,omitempty"`
	From      string `json:"from,omitempty"`
	Kind      string `json:"kind,omitempty"`
	IX        string `json:"ix,omitempty"`
	Payload   string `json:"payload,omitempty"`
	Reachable bool   `json:"reachable,omitempty"`
	Error     string `json:"error,omitempty"`
	Self      string `json:"self,omitempty"`
}

func main() {
	sock := flag.String("socket", "", "socket the daemon's p2p module connects to")
	peerSock := flag.String("peer", "", "socket other anetpeer processes connect to")
	rv := flag.String("rendezvous", "", "directory mapping AID → peer socket")
	flag.Parse()
	if *sock == "" || *peerSock == "" || *rv == "" {
		fmt.Fprintln(os.Stderr, "usage: anetpeer --socket S --peer P --rendezvous DIR")
		os.Exit(2)
	}
	if err := os.MkdirAll(*rv, 0o755); err != nil {
		log.Fatal(err)
	}
	p := &peer{peerSocket: *peerSock, rendezvous: *rv, acks: map[string]chan struct{}{}}

	_ = os.Remove(*peerSock)
	pl, err := net.Listen("unix", *peerSock)
	if err != nil {
		log.Fatalf("anetpeer: peer wire: %v", err)
	}
	go p.servePeers(pl)

	_ = os.Remove(*sock)
	dl, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatalf("anetpeer: daemon wire: %v", err)
	}
	log.Printf("anetpeer: daemon on %s, peers on %s, rendezvous %s", *sock, *peerSock, *rv)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		p.forget()
		dl.Close()
		pl.Close()
		os.Exit(0)
	}()

	for {
		c, err := dl.Accept()
		if err != nil {
			return
		}
		go p.serveDaemon(c)
	}
}

type peer struct {
	peerSocket string
	rendezvous string

	mu   sync.Mutex
	self string
	conn net.Conn
	enc  *json.Encoder

	// acks correlates a delivery with the receiving daemon's acceptance.
	acks map[string]chan struct{}
}

// serveDaemon handles the module's connection: hello, reach, send, and the
// acks that come back for what we delivered.
func (p *peer) serveDaemon(c net.Conn) {
	defer c.Close()
	p.mu.Lock()
	p.conn, p.enc = c, json.NewEncoder(c)
	p.mu.Unlock()

	dec := json.NewDecoder(bufio.NewReader(c))
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			p.mu.Lock()
			if p.conn == c {
				p.conn, p.enc = nil, nil
			}
			p.mu.Unlock()
			return
		}
		switch f.Op {
		case "hello":
			p.announce(f.Self)
			log.Printf("anetpeer: carrying %s", f.Self)
		case "reach":
			_, ok := p.lookup(f.To)
			p.reply(c, frame{Op: "reach", ID: f.ID, To: f.To, Reachable: ok})
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
				if err := p.deliver(f); err != nil {
					log.Printf("anetpeer: send to %s: %v", f.To, err)
					p.reply(c, frame{Op: "send", ID: f.ID, IX: f.IX, Error: err.Error()})
					return
				}
				log.Printf("anetpeer: delivered %s %s → %s", f.Kind, f.IX, f.To)
				p.reply(c, frame{Op: "send", ID: f.ID, IX: f.IX})
			}(f)
		case "ack":
			// Our daemon accepted something we handed it; release the peer
			// that is waiting to hear so.
			p.mu.Lock()
			if ch, ok := p.acks[f.IX]; ok {
				close(ch)
				delete(p.acks, f.IX)
			}
			p.mu.Unlock()
		}
	}
}

// servePeers handles other anetpeer processes handing us traffic.
func (p *peer) servePeers(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var f frame
			if err := json.NewDecoder(bufio.NewReader(c)).Decode(&f); err != nil {
				return
			}
			err := p.handOff(f)
			out := frame{Op: "send", IX: f.IX}
			if err != nil {
				out.Error = err.Error()
			}
			_ = json.NewEncoder(c).Encode(out)
		}()
	}
}

// handOff pushes an inbound payload to our daemon and waits for it to say
// it accepted it.
func (p *peer) handOff(f frame) error {
	p.mu.Lock()
	enc := p.enc
	if enc == nil {
		p.mu.Unlock()
		return fmt.Errorf("no daemon attached")
	}
	ch := make(chan struct{}, 1)
	p.acks[f.IX] = ch
	err := enc.Encode(frame{Op: "recv", From: f.From, Kind: f.Kind, IX: f.IX, Payload: f.Payload})
	p.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ch:
		return nil
	case <-time.After(10 * time.Second):
		p.mu.Lock()
		delete(p.acks, f.IX)
		p.mu.Unlock()
		// Not delivered, and saying so is the point: the daemon falls
		// through to the hub rather than losing the delegation.
		return fmt.Errorf("receiving daemon did not accept %s", f.IX)
	}
}

// deliver carries one payload to the peer holding the target AID.
func (p *peer) deliver(f frame) error {
	sock, ok := p.lookup(f.To)
	if !ok {
		return fmt.Errorf("no peer for %s", f.To)
	}
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewEncoder(c).Encode(f); err != nil {
		return err
	}
	var reply frame
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("%s", reply.Error)
	}
	return nil
}

func (p *peer) reply(c net.Conn, f frame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == c && p.enc != nil {
		_ = p.enc.Encode(f)
	}
}

// announce publishes "this AID is reachable at this socket".
//
// A directory rather than a DHT. It is the smallest thing that is still a
// real rendezvous: two peers that have never met find each other through
// it, and neither daemon learns what it is.
func (p *peer) announce(aid string) {
	if aid == "" {
		return
	}
	p.mu.Lock()
	p.self = aid
	p.mu.Unlock()
	_ = os.WriteFile(filepath.Join(p.rendezvous, aid), []byte(p.peerSocket), 0o644)
}

func (p *peer) forget() {
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()
	if self != "" {
		_ = os.Remove(filepath.Join(p.rendezvous, self))
	}
}

func (p *peer) lookup(aid string) (string, bool) {
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()
	if aid == "" || aid == self {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(p.rendezvous, aid))
	if err != nil {
		return "", false
	}
	return string(b), true
}

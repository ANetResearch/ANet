//go:build !no_a2a

// Package a2a is this node's local A2A interface (A2A-DESIGN §11): an A2A
// server on 127.0.0.1 through which any A2A client — an agent framework, a
// coding tool, a script using an A2A SDK — reaches the agents of the
// network as if each of them were an ordinary A2A server.
//
// It is how anet contributes to A2A rather than competing with it. An agent
// that cannot run an HTTPS server of its own still gets an A2A endpoint
// here, and the client talking to it needs nothing from anet: it reads a
// card, sends JSON-RPC or HTTP+JSON, and receives Tasks. What lies behind
// the card — end-to-end encryption through a hub that sees no content,
// identities with key histories, receipts, payments — is the daemon's
// business and reaches the client only as A2A objects and anet.* metadata.
//
// The shape, and why:
//
//   - One path per remote agent: /a2a/v1/agents/{aid}/… . A client holding
//     one agent's URL and the local token reaches that agent's tasks and no
//     others (A2A-DESIGN X5 [C17]); every task operation goes to the
//     kernel's TaskSeam with the AID from the path, and the kernel answers
//     TaskNotFound for anything outside it.
//   - The wire is a2a-go's (a2asrv's JSON-RPC and HTTP+JSON handlers); the
//     semantics are ours, in a RequestHandler that maps each operation to
//     the TaskSeam. This package is the only place that imports an A2A SDK,
//     so a build with -tags no_a2a carries none of it (SI-8).
//   - Loopback only, with a token of its own (a2a_token.txt), separate from
//     the control token: what it authorizes is narrower (requester-side
//     tasks, agent-tier payments), so it must not be the same secret.
//   - Enabled without a configuration block: this is the default product
//     surface, and `anet init` does not write one (a config naming "a2a"
//     would not load in a no_a2a build) [C43].
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/module"
)

const name = anethome.A2AModule

func init() {
	module.Register(name, New)
}

// New builds the module from its configuration block. Unlike the other
// modules, an absent block does not mean "not configured": the local A2A
// interface is on by default (A2A-DESIGN §11.1), and the block only adds
// provider-side backends (§11.6).
func New(raw []byte) (module.Module, error) {
	var cfg Config
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		// A misspelt key in a block about who may reach a local agent is
		// an error to report, not a default to fall back on.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("a2a: %w", err)
		}
	}
	for i := range cfg.Backends {
		// Held as matched: " * " is "*", not a skill nobody has.
		cfg.Backends[i].Match = strings.TrimSpace(cfg.Backends[i].Match)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Module{cfg: cfg}, nil
}

// Config is the modules.a2a block. Everything in it is optional.
type Config struct {
	// Backends forward accepted text tasks to local A2A servers (§11.6).
	Backends []Backend `json:"backends,omitempty"`
	// Retry is how a forward to a backend that failed for a reason a later
	// attempt may fix is tried again (backend.go).
	Retry BackendRetry `json:"retry,omitempty"`
}

// Module is the local A2A interface.
type Module struct {
	cfg Config

	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	done chan struct{}
}

// Name is the module's stable name; -tags no_a2a removes it.
func (m *Module) Name() string { return name }

// Start binds the interface and serves it until ctx ends.
//
// ctx is the daemon's own lifetime, and it is what ends the server: the
// daemon cancels it on the way down, and every request — a blocking
// SendMessage, an open stream — derives from it (BaseContext), so none of
// them holds the shutdown open.
func (m *Module) Start(ctx context.Context, h module.Host) error {
	// Declared first, before anything can be forwarded, so the kernel's
	// configuration check sees it (A2A-DESIGN §5.1).
	if m.cfg.declaresUntrusted() {
		h.DeclareUntrustedBackend()
	}
	if err := m.startBackends(ctx, h); err != nil {
		return err
	}
	seam, ok := h.TaskSeam()
	if !ok || seam == nil {
		log.Printf("anet: a2a: this daemon offers no task seam; the local A2A interface is not started")
		return nil
	}
	dir := h.StateDir(name)
	if dir == "" {
		return errors.New("a2a: no private state directory for the local A2A interface (see the log line above)")
	}
	_, statErr := os.Lstat(filepath.Join(dir, TokenFile))
	hadToken := statErr == nil
	token, err := loadOrCreateToken(dir)
	if err != nil {
		return fmt.Errorf("a2a: %w", err)
	}
	ln, fresh, err := listen(dir)
	var taken *portTakenError
	if errors.As(err, &taken) {
		portTaken(dir, taken)
		return nil // the rest of the node runs; see portTaken
	}
	if err != nil {
		return fmt.Errorf("a2a: %w", err)
	}
	if fresh && hadToken {
		// A new port while clients hold the token for another one: they go
		// on sending it to the old port, where anybody may be listening by
		// now. Replace it, so what they send is worth nothing [redteam:F18].
		if token, err = rotateToken(dir); err != nil {
			_ = ln.Close()
			return fmt.Errorf("a2a: replace the token for the new address: %w", err)
		}
		log.Printf("anet: a2a: %s was missing, so the local A2A interface chose its address afresh (%s) "+
			"and has a new token; update its clients: anet agents wire --refresh", AddrFile, ln.Addr())
	}
	_ = os.Remove(filepath.Join(dir, ConflictFile))
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	signer, _ := h.(module.ProxyCardSigner)
	s := newServer(seam, serverConfig{token: token, host: host, port: port, self: h.AID(), signer: signer})
	srv := &http.Server{
		Handler: s,
		// No WriteTimeout: a blocking SendMessage waits for the remote
		// agent, and a stream stays open until the task ends. Both end
		// with the request context, which ends with the daemon.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(log.Writer(), "anet: a2a: ", log.Flags()),
	}
	done := make(chan struct{})
	m.mu.Lock()
	m.srv, m.ln, m.done = srv, ln, done
	m.mu.Unlock()
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("anet: a2a: local interface stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		m.shutdown()
	}()
	log.Printf("anet: a2a: local A2A interface on http://%s/a2a/v1/agents", ln.Addr())
	return nil
}

// portTaken handles a recorded port that another process holds: the
// interface does not start, loudly, and the token is replaced when that
// process may belong to another local user (A2A-DESIGN §11.1 [redteam:F18]).
//
// Moving to another port, which this used to do, left every configured
// client — Hermes' a2a_agents entries among them — sending the token to the
// old port, to whoever held it, while the token went on working on the new
// one. Staying down is the choice that leaks nothing more, and it costs the
// node only this interface: everything else keeps running, which is also
// why this is not an error returned to the daemon (a port another user can
// take must not be a way to stop the node). The token is replaced because
// the clients may already have sent it to the holder; it takes effect
// whenever the interface next starts. A holder that is provably this same
// user is not a boundary (A2A-DESIGN §21 item 13) and keeps the token.
//
// What happened is written to a2a_port_conflict.txt, which `anet up` and
// `anet doctor` report, and logged.
func portTaken(dir string, te *portTakenError) {
	who, otherUser := portHolder(te.addr)
	rotated := ""
	if otherUser {
		if _, err := rotateToken(dir); err != nil {
			rotated = " The local A2A token could NOT be replaced (" + err.Error() + "); delete " +
				filepath.Join(dir, TokenFile) + " before the interface starts again."
		} else {
			rotated = " The local A2A token was replaced, because its clients may have sent the old one to that process."
		}
	}
	msg := fmt.Sprintf("%s %s is held by %s, so the local A2A interface did not start "+
		"(it does not move while its clients point at this port).%s "+
		"Free the port (stop the process holding it), restart the node (anet stop && anet up), "+
		"then run: anet agents wire --refresh",
		time.Now().UTC().Format(time.RFC3339), te.addr, who, rotated)
	if err := writePrivateFile(filepath.Join(dir, ConflictFile), []byte(msg+"\n")); err != nil {
		log.Printf("anet: a2a: record the port conflict: %v", err)
	}
	log.Printf("anet: a2a: ERROR: %s", msg)
}

// Stop shuts the server down. The daemon's own shutdown goes through the
// Start context; Stop is for a daemon that stops its modules because it
// could not finish starting.
func (m *Module) Stop(context.Context) error {
	m.shutdown()
	return nil
}

func (m *Module) shutdown() {
	m.mu.Lock()
	srv, done := m.srv, m.done
	m.srv = nil
	m.mu.Unlock()
	if srv == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	<-done
}

// Addr is the address the interface listens on, or "" before Start.
func (m *Module) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

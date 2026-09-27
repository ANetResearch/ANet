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
	"sync"
	"time"

	"github.com/ANetResearch/ANet/module"
)

const name = "a2a"

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
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Module{cfg: cfg}, nil
}

// Config is the modules.a2a block. Everything in it is optional.
type Config struct {
	// Backends forward accepted text tasks to local A2A servers (§11.6).
	Backends []Backend `json:"backends,omitempty"`
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
	m.cfg.logBackends()
	seam, ok := h.TaskSeam()
	if !ok || seam == nil {
		log.Printf("anet: a2a: this daemon offers no task seam; the local A2A interface is not started")
		return nil
	}
	dir := h.StateDir(name)
	if dir == "" {
		return errors.New("a2a: no private state directory for the local A2A interface (see the log line above)")
	}
	token, err := loadOrCreateToken(dir)
	if err != nil {
		return fmt.Errorf("a2a: %w", err)
	}
	ln, err := listen(dir)
	if err != nil {
		return fmt.Errorf("a2a: %w", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	signer, _ := h.(module.ProxyCardSigner)
	s := newServer(seam, serverConfig{token: token, port: port, self: h.AID(), signer: signer})
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

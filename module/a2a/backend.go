//go:build !no_a2a

package a2a

// backend.go is the provider side (A2A-DESIGN §11.6): text tasks delegated
// to this node, handed to a local A2A server — an agent that already speaks
// A2A answering work that arrives over the network.
//
//	"modules": {"a2a": {"backends": [{
//	    "match": "*", "url": "unix:///run/my-agent/a2a.sock",
//	    "token_file": "/path/to/token", "accept_untrusted": false, "toolless": false}]}}
//
// The rules, all of them about who may reach a local agent:
//
//   - The backend is checked before a task's text or the token is written
//     to it (docs/notes/0030 N1, internal/backendconn). A unix:// URL — the
//     recommended form — is a socket whose path only trusted accounts can
//     change and, on Linux, whose listener is the socket's owner or the
//     account expected_uid/expected_user names. An http(s) URL needs
//     allow_tcp: true, for a backend that can only listen on TCP (Hermes'
//     default 127.0.0.1:9900), and a listener on loopback must then run as
//     this daemon's user: one another local user took while the backend
//     was down gets nothing.
//   - Only text tasks the kernel has accepted, and only from peers on the
//     trust list, are forwarded; everything else stays in the inbox for
//     the operator (MCP reply_task, the CLI).
//   - accept_untrusted: true forwards work from peers that are not trusted
//     as well, and is accepted only together with toolless: true — the
//     operator's statement that the backend cannot act on the machine (no
//     tools, no shell, no files). The module declares such a backend to
//     the kernel (Host.DeclareUntrustedBackend), whose configuration check
//     refuses it together with inbound.policy=open (§5.1).
//   - What is forwarded carries anet.peer_aid, anet.trusted and the
//     a2a.serviceParameters the requester sent, restored as headers. All
//     requests use one token, so a backend cannot tell peers apart by its
//     credential and must read anet.peer_aid.
//   - The agent behind a backend should be a service agent of its own (for
//     Hermes, a separate profile), not the session the operator works in:
//     what reaches it is work from other nodes, and it would land in the
//     operator's own conversation (0017 Q23).
//   - Each forwarded task is recorded: anet.backend.forwarded{backend,
//     interaction_id, peer_aid, trusted}, once, when the backend took the
//     message; a forward that ends without an answer is recorded once as
//     anet.backend.failed{…, attempts, error}.
//   - A forward that failed before the backend could have taken the
//     message — the backend not reached (not listening yet, restarting, a
//     connect timeout), or its card answered 5xx, so the request carrying
//     the message was never written whole — or that the backend answered
//     503 (it did not take it) is tried again with exponential backoff from
//     5 s, up to retry.max_interval (2m) between attempts, until
//     retry.give_up_after (10m) has passed since the first; the task stays
//     as it is meanwhile, and gives up its place among the forwards running
//     at once while it waits. Before each attempt the kernel decides again
//     whether the task may still go to a backend (InboundTask): a peer taken
//     off the trust list, a task answered in the meantime or a newer message
//     from the requester ends the retries. Every attempt carries the same
//     message id, so a backend that saw an earlier one can tell. Once the
//     message was written to the backend, nothing but a 503 is retried: a
//     connection lost or timed out while it worked, or a 500 after it
//     started, may follow work it did, and sending the message again would
//     run the requester's task twice. A refusal by the path and listener
//     checks, an error the backend answered with (4xx, an A2A error), or an
//     answer without content is not retried either: the task stays in the
//     inbox for the operator (docs/notes/0035 §5.1: a backend that was down
//     once kept its tasks unanswered until the daemon restarted).
//   - A first turn the backend has had for announceAfter (a minute) without
//     answering is announced to the requester as working (ReplyTask with
//     state working, no message): until then the requester has heard
//     nothing, and it fails a task it hears nothing about for its
//     no_response_after (15m by default) as no_response (A2A-DESIGN §4.2) —
//     an agent turn longer than that is work, not silence. A backend that
//     answers within the minute costs no extra message.
//
// The tasks come from the kernel through module.InboundTaskHost, which a
// daemon offers or does not; without it a configured backend is validated,
// declared, and logged as not forwarding.
//
// A task maps to one task on the backend: the first forward starts it (in
// the context the kernel gives the task, which it derives from the peer and
// the network task's context, so the backend keeps one conversation per
// peer and context and two peers never share one) and each later message
// from the requester continues it. The
// backend's answer goes back as this node's reply: a question
// (input-required) as a question, a final answer as completion. The
// mapping lives in memory: after a restart a follow-up starts a new task on
// the backend, in the same context.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/module"
)

// Backend is one provider-side A2A backend.
type Backend struct {
	// Match is "*" (every accepted text task) or a skill id: a task whose
	// metadata names that skill (anet.skill) goes to this backend before a
	// "*" one. Text tasks from the network name none today, so "*" is what
	// applies to them.
	Match string `json:"match"`
	// URL is where the backend's agent card is: a URL with no path (the
	// card is then read from /.well-known/agent-card.json) or the card's own
	// URL. unix:///path/to/socket[:/card/path] (recommended; every call goes
	// to that socket, whatever host the card's interfaces name), or, with
	// allow_tcp, http on loopback or https anywhere. The interfaces the card
	// names are held to the same rule.
	URL string `json:"url"`
	// TokenFile holds the bearer token the daemon presents to the backend,
	// on the card request and on every call.
	TokenFile string `json:"token_file,omitempty"`
	// AcceptUntrusted forwards tasks from peers not on the trust list; it
	// needs Toolless.
	AcceptUntrusted bool `json:"accept_untrusted,omitempty"`
	// Toolless is the operator's statement that the backend has no tools:
	// it cannot run commands, read files or reach the network on a task's
	// behalf.
	Toolless bool `json:"toolless,omitempty"`
	// Policy: allow_tcp, expected_uid/expected_user, socket_group — who
	// may be on the far side of URL (internal/backendconn).
	backendconn.Policy
}

// BackendRetry is how a forward the backend did not take is tried again:
// the backend not reached (not listening, being restarted, a connect
// timeout) before the message was written to it, or answering 503
// (retryable). The wait between attempts starts at retryBase and doubles.
type BackendRetry struct {
	// MaxInterval caps the wait between two attempts: a Go duration,
	// default 2m.
	MaxInterval string `json:"max_interval,omitempty"`
	// GiveUpAfter is how long after the first attempt a task is still
	// tried: a Go duration, default 10m; "0" tries once. Keep it under the
	// requesters' no_response_after (15m unless they changed it): a task
	// they have heard nothing about by then they fail as no_response.
	GiveUpAfter string `json:"give_up_after,omitempty"`
}

// Retry defaults.
const (
	defaultRetryMax     = 2 * time.Minute
	defaultRetryGiveUp  = 10 * time.Minute
	retryConfigExamples = "a Go duration such as \"2m\""
)

// retryBase is the first wait between two attempts. A variable so tests can
// shorten it.
var retryBase = 5 * time.Second

// retryPolicy is a BackendRetry read.
type retryPolicy struct {
	base, max, giveUp time.Duration
}

func (r BackendRetry) resolve() (retryPolicy, error) {
	p := retryPolicy{base: retryBase, max: defaultRetryMax, giveUp: defaultRetryGiveUp}
	read := func(key, v string, into *time.Duration) error {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("a2a: retry.%s %q is not %s of zero or more", key, v, retryConfigExamples)
		}
		*into = d
		return nil
	}
	if err := read("max_interval", r.MaxInterval, &p.max); err != nil {
		return p, err
	}
	if err := read("give_up_after", r.GiveUpAfter, &p.giveUp); err != nil {
		return p, err
	}
	if p.max < p.base {
		p.max = p.base
	}
	return p, nil
}

func (c Config) validate() error {
	if _, err := c.Retry.resolve(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, b := range c.Backends {
		where := fmt.Sprintf("a2a: backends[%d]", i)
		m := strings.TrimSpace(b.Match)
		if m == "" {
			return errors.New(where + ": match is required (\"*\" or a skill id)")
		}
		if seen[m] {
			return fmt.Errorf("%s: a second backend for match %q", where, m)
		}
		seen[m] = true
		if _, _, err := b.target(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if b.AcceptUntrusted && !b.Toolless {
			return errors.New(where + ": accept_untrusted needs toolless: true — a backend that serves peers " +
				"nobody vouched for must not be able to act on this machine")
		}
	}
	return nil
}

// declaresUntrusted reports a backend that takes work from untrusted peers.
func (c Config) declaresUntrusted() bool {
	for _, b := range c.Backends {
		if b.AcceptUntrusted {
			return true
		}
	}
	return false
}

// forwardsText reports a backend for every text task (match "*").
func (c Config) forwardsText() bool {
	for _, b := range c.Backends {
		if b.Match == "*" {
			return true
		}
	}
	return false
}

// target parses the backend's URL and policy: a socket, or with allow_tcp
// an http(s) URL that passes checkBackendURL.
func (b Backend) target() (backendconn.Target, backendconn.Rules, error) {
	rules, err := b.Policy.Resolve()
	if err != nil {
		return backendconn.Target{}, rules, err
	}
	t, err := backendconn.Parse(b.URL)
	if err != nil {
		return t, rules, err
	}
	if err := rules.Admit(t); err != nil {
		return t, rules, err
	}
	if !t.Unix() {
		if err := checkBackendURL(b.URL); err != nil {
			return t, rules, err
		}
	}
	return t, rules, nil
}

// checkBackendURL accepts http on a loopback host, or https: a task's text
// goes there, and in the clear only on this machine.
func checkBackendURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("url %q is not an http(s) URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); (ip != nil && ip.IsLoopback()) || strings.EqualFold(h, "localhost") {
			return nil
		}
		return fmt.Errorf("url %q: plain http only to a loopback host; use https", raw)
	}
	return fmt.Errorf("url %q is not an http(s) URL", raw)
}

// startBackends begins forwarding, if there is anything to forward and a
// kernel to forward from. A token file that cannot be read is an error:
// the backend was asked for and cannot be reached as configured.
func (m *Module) startBackends(ctx context.Context, h module.Host) error {
	if len(m.cfg.Backends) == 0 {
		return nil
	}
	if !m.cfg.forwardsText() {
		// A task the kernel delivers is a text task, and names no skill (a
		// call naming one is a capability call, never delivered). Without
		// a "*" backend none would be forwarded, and subscribing would only
		// keep those tasks from the auto-reply agent.
		log.Printf("anet: a2a: no backend has match \"*\": text tasks from the network name no skill, so none " +
			"is forwarded; they are answered as without backends (inbox, auto-reply)")
		return nil
	}
	in, ok := h.(module.InboundTaskHost)
	if !ok {
		for _, b := range m.cfg.Backends {
			log.Printf("anet: a2a: backend %s is configured, but this daemon hands no inbound tasks to modules; "+
				"accepted tasks stay in the inbox", b.URL)
		}
		return nil
	}
	retry, err := m.cfg.Retry.resolve()
	if err != nil { // checked by New; kept for a Module made otherwise
		return err
	}
	f := &forwarder{host: h, in: in, remote: map[string]string{}, busy: map[string]bool{}, pending: map[string]module.Task{},
		timeout: backendTimeout, sem: make(chan struct{}, maxForwards), retry: retry}
	for _, b := range m.cfg.Backends {
		t, rules, err := b.target()
		if err != nil { // checked by New; kept for a Module made otherwise
			return fmt.Errorf("a2a: backend %s: %w", b.URL, err)
		}
		hc := rules.Client(t, 0)
		hc.Transport = noting{next: hc.Transport}
		bc := &backendClient{cfg: b, target: t, http: hc}
		if !t.Unix() {
			log.Printf("anet: a2a: backend %s is reached over TCP (allow_tcp); a Unix socket (unix:///path) is the recommended form", b.URL)
		}
		if b.TokenFile != "" {
			tok, err := os.ReadFile(b.TokenFile)
			if err != nil {
				return fmt.Errorf("a2a: backend %s: token_file: %w", b.URL, err)
			}
			if bc.token = strings.TrimSpace(string(tok)); bc.token == "" {
				return fmt.Errorf("a2a: backend %s: token_file %s is empty", b.URL, b.TokenFile)
			}
		}
		f.backends = append(f.backends, bc)
	}
	tasks, err := in.InboundTasks(ctx)
	if err != nil {
		return fmt.Errorf("a2a: backends: %w", err)
	}
	for _, b := range m.cfg.Backends {
		who := "trusted peers"
		if b.AcceptUntrusted {
			who = "any admitted peer (toolless)"
		}
		log.Printf("anet: a2a: forwarding accepted text tasks from %s to the A2A backend %s (match %s)", who, b.URL, b.Match)
	}
	go f.run(ctx, tasks)
	return nil
}

// How long one forward may take (the backend's whole answer), and how many
// run at once.
const (
	backendTimeout = 30 * time.Minute
	maxForwards    = 8
)

// forwarder hands inbound tasks to the backends.
type forwarder struct {
	host     module.Host
	in       module.InboundTaskHost
	backends []*backendClient
	timeout  time.Duration
	sem      chan struct{}
	retry    retryPolicy

	mu sync.Mutex
	// remote maps a network task to its task on the backend.
	remote map[string]string
	// busy marks a task being forwarded, and pending holds its newest
	// delivery that arrived meanwhile: one task is forwarded one message
	// at a time, in order, and a message is never dropped for arriving
	// while the last one was still with the backend.
	busy    map[string]bool
	pending map[string]module.Task
}

func (f *forwarder) run(ctx context.Context, tasks <-chan module.Task) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-tasks:
			if !ok {
				return
			}
			f.mu.Lock()
			if f.busy[t.ID] {
				f.pending[t.ID] = t
				f.mu.Unlock()
				continue
			}
			f.busy[t.ID] = true
			f.mu.Unlock()
			select {
			case f.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			wg.Add(1)
			s := &slot{sem: f.sem, held: true}
			go func() {
				defer wg.Done()
				defer s.release()
				for {
					f.deliver(ctx, t, s)
					f.mu.Lock()
					next, more := f.pending[t.ID]
					delete(f.pending, t.ID)
					if !more || ctx.Err() != nil {
						delete(f.busy, t.ID)
						f.mu.Unlock()
						return
					}
					f.mu.Unlock()
					t = next
				}
			}()
		}
	}
}

// slot is one of the maxForwards places a forward runs in (forwarder.sem).
// A task waiting to be tried again gives its place up for the wait: with
// one backend down, maxForwards of its tasks would otherwise hold every
// place for retry.give_up_after, and a task for a backend that is up would
// wait behind them — long enough for its requester to fail it as
// no_response. Used by one goroutine.
type slot struct {
	sem  chan struct{}
	held bool
}

// release gives the place up, if held.
func (s *slot) release() {
	if s.held {
		<-s.sem
		s.held = false
	}
}

// take waits for a place; false when ctx ended first.
func (s *slot) take(ctx context.Context) bool {
	if s.held {
		return true
	}
	select {
	case s.sem <- struct{}{}:
		s.held = true
		return true
	case <-ctx.Done():
		return false
	}
}

// pick is the backend for a task: an exact skill match first, then "*",
// and only one that serves the task's peer.
func (f *forwarder) pick(t module.Task, trusted bool) *backendClient {
	skill, _ := t.Metadata[a2ashape.KeySkill].(string)
	var star *backendClient
	for _, b := range f.backends {
		if !trusted && !b.cfg.AcceptUntrusted {
			continue
		}
		switch b.cfg.Match {
		case skill:
			if skill != "" {
				return b
			}
		case "*":
			if star == nil {
				star = b
			}
		}
	}
	return star
}

// deliver forwards t, and forwards it again after a failure a later attempt
// may fix (retryable), waiting retry.base, then twice that each time up to
// retry.max, until retry.giveUp has passed since the first attempt. Before
// each retry the kernel is asked for the task as it is now (InboundTask):
// the retries end when it may no longer go to a backend, when the
// requester's latest message is no longer the one being forwarded, and when
// a newer delivery of the task is waiting (the run loop forwards that one).
// A forward that ends without an answer is recorded once
// (anet.backend.failed). s is the task's place among the forwards running at
// once: given up while it waits, taken again before the next attempt.
func (f *forwarder) deliver(ctx context.Context, t module.Task, s *slot) {
	first := time.Now()
	wait := f.retry.base
	for attempt := 1; ; attempt++ {
		b, retry, err := f.forward(ctx, t)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			// Stopping: the task stays as it is, and the next process
			// delivers it again.
			return
		}
		if !retry {
			f.failed(b, t, attempt, err, false)
			return
		}
		if time.Since(first)+wait > f.retry.giveUp {
			log.Printf("anet: a2a: backend %s: task %s: %v; gave up after %d attempt(s) in %s (left in the inbox)",
				b.cfg.URL, t.ID, err, attempt, time.Since(first).Round(time.Second))
			f.failed(b, t, attempt, err, true)
			return
		}
		if attempt == 1 {
			log.Printf("anet: a2a: backend %s: task %s: %v; trying again in %s, for up to %s",
				b.cfg.URL, t.ID, err, wait, f.retry.giveUp)
		}
		s.release()
		if !f.pause(ctx, wait) || !s.take(ctx) {
			return
		}
		wait = min(wait*2, f.retry.max)
		f.mu.Lock()
		_, newer := f.pending[t.ID]
		f.mu.Unlock()
		if newer {
			return
		}
		now, ok, err := f.in.InboundTask(ctx, t.ID)
		if err != nil || !ok || !sameLatest(now, t) {
			if err != nil {
				log.Printf("anet: a2a: task %s: not tried again: %v", t.ID, err)
			}
			return
		}
		t = now
	}
}

// pause waits d, or until ctx ends (false).
func (f *forwarder) pause(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// sameLatest reports whether a and b end with the same requester message.
func sameLatest(a, b module.Task) bool {
	la, lb := lastRequesterMessage(a), lastRequesterMessage(b)
	return la != nil && lb != nil && la.ID == lb.ID && len(a.History) == len(b.History)
}

// failed records a forward that ended without an answer: once, with the
// attempts made and the last error. retried says it was given up after
// retries rather than failing for good.
func (f *forwarder) failed(b *backendClient, t module.Task, attempts int, err error, retried bool) {
	if b == nil {
		return
	}
	if !retried {
		log.Printf("anet: a2a: backend %s: task %s: %v (left in the inbox)", b.cfg.URL, t.ID, err)
	}
	trusted, _ := t.Metadata[a2ashape.KeyTrusted].(bool)
	peer, _ := t.Metadata[a2ashape.KeyPeerAID].(string)
	msg := err.Error()
	if len(msg) > 512 {
		msg = msg[:512]
	}
	if rerr := f.host.RecordEvidence("anet.backend.failed", map[string]any{
		"backend": b.cfg.URL, "interaction_id": t.ID, "peer_aid": peer, "trusted": trusted,
		"attempts": attempts, "error": msg, "gave_up": retried,
	}); rerr != nil {
		log.Printf("anet: a2a: evidence for task %s: %v", t.ID, rerr)
	}
}

// errNoContent is a backend answer with nothing to send back.
var errNoContent = errors.New("the answer has no content")

// forward sends one task's latest message to its backend and answers the
// task with what comes back. It returns the backend, and on failure the
// error and whether a later attempt may get through (retryable); the task
// is left as it is. A task no backend takes, or with no message of the
// requester's, is done.
func (f *forwarder) forward(ctx context.Context, t module.Task) (*backendClient, bool, error) {
	trusted, _ := t.Metadata[a2ashape.KeyTrusted].(bool)
	peer, _ := t.Metadata[a2ashape.KeyPeerAID].(string)
	b := f.pick(t, trusted)
	if b == nil {
		return nil, false, nil
	}
	last := lastRequesterMessage(t)
	if last == nil {
		return b, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	note := newHTTPNote()
	ctx = context.WithValue(ctx, noteKey{}, note)
	// Announced (working) when the backend has had a first turn for
	// announceAfter; ended, and any announcement waited for, before the
	// answer is sent, so that it never follows the answer.
	announced := f.announceWhenSlow(ctx, t, note)
	defer announced()

	msg, params := backendMessage(t, *last, peer, trusted)
	f.mu.Lock()
	msg.TaskID = a2a.TaskID(f.remote[t.ID])
	f.mu.Unlock()
	res, err := b.send(ctx, msg, params)
	if err != nil && msg.TaskID != "" && errors.Is(err, a2a.ErrTaskNotFound) {
		// The backend forgot the task (it restarted): start again in the
		// same context.
		msg.TaskID = ""
		res, err = b.send(ctx, msg, params)
	}
	if err != nil {
		return b, retryable(err, note), err
	}
	if err := f.host.RecordEvidence("anet.backend.forwarded", map[string]any{
		"backend": b.cfg.URL, "interaction_id": t.ID, "peer_aid": peer, "trusted": trusted,
	}); err != nil {
		log.Printf("anet: a2a: evidence for task %s: %v", t.ID, err)
	}
	// From here on the backend has the message: what fails is not sent
	// again (a second message would be a second turn of its conversation).
	if bt, ok := res.(*a2a.Task); ok {
		bt, err = b.settle(ctx, bt)
		if err != nil {
			return b, false, err
		}
		f.mu.Lock()
		if bt.Status.State.Terminal() {
			delete(f.remote, t.ID)
		} else {
			f.remote[t.ID] = string(bt.ID)
		}
		f.mu.Unlock()
		res = bt
	}
	reply, state, ok := replyFrom(res)
	if !ok {
		return b, false, errNoContent
	}
	reply.TaskID, reply.ContextID = t.ID, t.ContextID
	announced()
	if _, err := f.in.ReplyTask(ctx, t.ID, reply, state); err != nil {
		log.Printf("anet: a2a: task %s: reply from backend %s: %v", t.ID, b.cfg.URL, err)
	}
	return b, false, nil
}

// retryable reports a failed forward a later attempt may get through
// without the backend running the requester's message twice (note: what
// this attempt's HTTP exchanges came to).
//
// Before the request carrying the message was written whole, the backend
// cannot have taken it: an attempt that reached no backend (no socket yet,
// connection refused, a connect timeout, a connection lost while writing)
// or whose card answered 5xx is tried again. After it was written, the
// backend may be doing the work, and only an answer that says it was not
// taken — 503 — is: a connection lost or timed out while the backend
// worked, or a 500 after it started, may follow work it did (a backend need
// not recognise the repeated message id). Never: a refusal by the path and
// listener checks (nothing was sent, and waiting changes nothing), and any
// other answer — a 4xx, an A2A error in a 200, a card this node cannot use.
func retryable(err error, note *httpNote) bool {
	if err == nil || errors.Is(err, backendconn.ErrRefused) {
		return false
	}
	code := note.code.Load()
	if note.sent.Load() {
		return code == http.StatusServiceUnavailable
	}
	if code != 0 {
		return code >= 500
	}
	return note.failed.Load()
}

// announceAfter is how long a backend may have a task's first turn before
// the requester is told it is being worked on (announceWhenSlow). A
// variable so tests can shorten it.
var announceAfter = time.Minute

// announceWhenSlow tells t's requester, through the kernel, that the task
// is being worked on (working) once the backend has had its first turn —
// the request carrying the message written — for announceAfter without
// answering. The requester has heard nothing until then, and a requester
// that hears nothing for its no_response_after (15m by default) fails the
// task as no_response (A2A-DESIGN §4.2): a long agent turn is work, not
// silence. Only a submitted task, one nobody has answered yet, is
// announced; a later turn is already working on both sides. The returned
// function ends the wait and returns once an announcement under way is
// done, so that it cannot follow the answer; it may be called more than
// once.
func (f *forwarder) announceWhenSlow(ctx context.Context, t module.Task, note *httpNote) func() {
	if t.Status.State != a2ashape.TaskStateSubmitted || announceAfter <= 0 {
		return func() {}
	}
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-note.sentCh:
		case <-stop:
			return
		}
		tm := time.NewTimer(announceAfter)
		defer tm.Stop()
		select {
		case <-tm.C:
		case <-stop:
			return
		}
		if _, err := f.in.ReplyTask(ctx, t.ID, a2ashape.Message{}, a2ashape.TaskStateWorking); err != nil && ctx.Err() == nil {
			log.Printf("anet: a2a: task %s: tell the requester the backend is working on it: %v", t.ID, err)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-finished
	}
}

// noteKey carries an attempt's httpNote in its context.
type noteKey struct{}

// httpNote is what the HTTP exchanges of a forward attempt came to: the
// status of the last response, or that none came; and whether a request
// carrying the message (any but a GET, which reads the card or a task) was
// written whole, sentCh closing when it first was.
type httpNote struct {
	code     atomic.Int32
	failed   atomic.Bool
	sent     atomic.Bool
	sentCh   chan struct{}
	sentOnce sync.Once
}

func newHTTPNote() *httpNote { return &httpNote{sentCh: make(chan struct{})} }

func (n *httpNote) markSent() {
	n.sent.Store(true)
	n.sentOnce.Do(func() { close(n.sentCh) })
}

// noting records each exchange in the httpNote of the request's context.
type noting struct{ next http.RoundTripper }

func (n noting) RoundTrip(r *http.Request) (*http.Response, error) {
	note, ok := r.Context().Value(noteKey{}).(*httpNote)
	if !ok {
		return n.next.RoundTrip(r)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// WroteRequest comes once the request is flushed to the
		// connection; with an error, it was not written whole, and the
		// backend cannot have read it as a request.
		trace := &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				note.markSent()
			}
		}}
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	}
	resp, err := n.next.RoundTrip(r)
	if err != nil {
		note.code.Store(0)
		note.failed.Store(true)
	} else {
		note.code.Store(int32(resp.StatusCode))
	}
	return resp, err
}

// lastRequesterMessage is the requester's latest message, which the kernel
// guarantees is the last in the history of a task it delivers.
func lastRequesterMessage(t module.Task) *a2ashape.Message {
	for i := len(t.History) - 1; i >= 0; i-- {
		if t.History[i].Role == a2ashape.RoleUser {
			return &t.History[i]
		}
	}
	return nil
}

// backendMessage is what the backend receives: the requester's message,
// its metadata minus the reserved relay key, and who sent it. The service
// parameters the requester sent across the relay become headers again.
func backendMessage(t module.Task, m a2ashape.Message, peer string, trusted bool) (*a2a.Message, a2aclient.ServiceParams) {
	meta := map[string]any{}
	for k, v := range m.Metadata {
		if k != a2ashape.KeyServiceParameters {
			meta[k] = v
		}
	}
	meta[a2ashape.KeyPeerAID] = peer
	meta[a2ashape.KeyTrusted] = trusted
	var parts []a2ashape.Part
	for _, p := range m.Parts {
		// A reference to this node's attachment store means nothing to
		// the backend; the kernel delivers files inline.
		if p.Kind != a2ashape.PartURL {
			parts = append(parts, p)
		}
	}
	shaped := a2ashape.Message{ID: forwardMessageID(t.ID, m.ID), ContextID: t.ContextID, Role: a2ashape.RoleUser,
		Parts: parts, Metadata: meta, Extensions: m.Extensions}
	out, err := convert[a2a.Message](shaped)
	if err != nil {
		out = *a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(textOfParts(parts)))
		out.ContextID, out.Metadata = t.ContextID, meta
	}
	params := a2aclient.ServiceParams{}
	if sp, ok := m.Metadata[a2ashape.KeyServiceParameters].(map[string]any); ok {
		for k, v := range sp {
			if !strings.EqualFold(k, a2a.SvcParamExtensions) {
				continue // the client sets its own A2A-Version
			}
			for _, u := range stringList(v) {
				if u = strings.TrimSpace(u); u != "" {
					params.Append(a2a.SvcParamExtensions, u)
				}
			}
		}
	}
	return &out, params
}

// forwardMessageID is the id of the message a backend is sent for the
// requester's message msgID on task taskID: the same for every attempt at
// it, so a backend that saw an earlier attempt can tell a retry; different
// for each message and task; and not the requester's own id.
func forwardMessageID(taskID, msgID string) string {
	h := sha256.Sum256([]byte("anet/backend-message/1\x00" + taskID + "\x00" + msgID))
	return "anet-msg-" + hex.EncodeToString(h[:16])
}

func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		return strings.Split(x, ",")
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	}
	return nil
}

func textOfParts(parts []a2ashape.Part) string {
	var b []string
	for _, p := range parts {
		if p.Kind == a2ashape.PartText {
			b = append(b, p.Text)
		}
	}
	return strings.Join(b, "\n")
}

// replyFrom reads the backend's answer: a message is a final answer; a task
// answers with its artifacts when it completed and with its status message
// otherwise, and its state says what the answer leaves the network task in.
func replyFrom(res a2a.SendMessageResult) (a2ashape.Message, a2ashape.TaskState, bool) {
	var parts []*a2a.Part
	state := a2ashape.TaskStateCompleted
	switch r := res.(type) {
	case *a2a.Message:
		parts = r.Parts
	case *a2a.Task:
		switch r.Status.State {
		case a2a.TaskStateCompleted:
			for _, a := range r.Artifacts {
				parts = append(parts, a.Parts...)
			}
		case a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired:
			state = a2ashape.TaskStateInputRequired
		case a2a.TaskStateRejected:
			state = a2ashape.TaskStateRejected
		default: // failed, canceled, or still running when time ran out
			state = a2ashape.TaskStateFailed
		}
		if len(parts) == 0 && r.Status.Message != nil {
			parts = r.Status.Message.Parts
		}
		if len(parts) == 0 && state == a2ashape.TaskStateFailed {
			parts = []*a2a.Part{a2a.NewTextPart("the agent could not complete this task")}
		}
	}
	if len(parts) == 0 {
		return a2ashape.Message{}, "", false
	}
	out, err := convert[[]a2ashape.Part](parts)
	if err != nil || len(out) == 0 {
		return a2ashape.Message{}, "", false
	}
	return a2ashape.Message{ID: a2a.NewMessageID(), Role: a2ashape.RoleAgent, Parts: out}, state, true
}

// backendClient is one backend and its A2A client, made from the
// backend's card on first use and again after a failure.
type backendClient struct {
	cfg    Backend
	target backendconn.Target
	// http checks the far side of every connection before it is used
	// (backendconn.Rules.Client). It has no overall timeout: a forward's
	// context bounds each call. It follows no redirect: every request
	// carries the backend's token and a task's text, and a redirect would
	// take both somewhere the url rule (checkBackendURL) never looked at —
	// https to plain http on the same host keeps the Authorization header.
	http  *http.Client
	token string

	mu     sync.Mutex
	client *a2aclient.Client
}

func (b *backendClient) get(ctx context.Context) (*a2aclient.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	var opts []agentcard.ResolveOption
	if b.token != "" {
		opts = append(opts, agentcard.WithRequestHeader("Authorization", "Bearer "+b.token))
	}
	// For a socket the card is read at http://localhost/<path>; the client
	// dials the socket whatever the host.
	card, err := (&agentcard.Resolver{Client: b.http}).Resolve(ctx, b.target.URL.String(), opts...)
	if err != nil {
		return nil, fmt.Errorf("card: %w", err)
	}
	if err := restrictInterfaces(card, b.target); err != nil {
		return nil, err
	}
	cl, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(b.http), a2aclient.WithRESTTransport(b.http),
		a2aclient.WithCallInterceptors(bearer{token: b.token}))
	if err != nil {
		return nil, err
	}
	b.client = cl
	return cl, nil
}

// restrictInterfaces keeps the interfaces of a backend's card that the url
// rule allows: http on a loopback host, or https. The configured URL passed
// that rule, but the calls go to the URLs the card names, and the card is
// the backend's own word — an interface in the clear off this machine would
// receive the task's text and the token, so it is dropped, not used.
//
// For a socket backend every connection goes to the configured socket, so
// an http interface is kept whatever host it names (that host is only a
// label), a unix:// interface on the same socket is rewritten to the path
// it names, and everything else is dropped.
func restrictInterfaces(card *a2a.AgentCard, t backendconn.Target) error {
	var keep []*a2a.AgentInterface
	for _, i := range card.SupportedInterfaces {
		if i == nil {
			continue
		}
		if !t.Unix() {
			if !strings.HasPrefix(i.URL, backendconn.SchemeUnix+":") && checkBackendURL(i.URL) == nil {
				keep = append(keep, i)
			}
			continue
		}
		if it, err := backendconn.Parse(i.URL); err == nil && it.Unix() {
			if it.Socket == t.Socket {
				c := *i
				c.URL = it.URL.String()
				keep = append(keep, &c)
			}
			continue
		}
		if u, err := url.Parse(i.URL); err == nil && u.Scheme == "http" && u.Host != "" && u.User == nil {
			keep = append(keep, i)
		}
	}
	if len(keep) == 0 {
		if t.Unix() {
			return errors.New("card: no interface this node sends tasks to (a socket backend's card names http:// or its own unix:// socket)")
		}
		return errors.New("card: no interface this node sends tasks to (plain http only on a loopback host; https)")
	}
	card.SupportedInterfaces = keep
	return nil
}

func (b *backendClient) drop() {
	b.mu.Lock()
	b.client = nil
	b.mu.Unlock()
}

// send is one blocking SendMessage.
func (b *backendClient) send(ctx context.Context, msg *a2a.Message, params a2aclient.ServiceParams) (a2a.SendMessageResult, error) {
	cl, err := b.get(ctx)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		ctx = a2aclient.AttachServiceParams(ctx, params)
	}
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil && !isA2AError(err) {
		b.drop() // the transport failed: read the card again next time
	}
	return res, err
}

// settle waits for a task the backend returned before it ended or asked
// back — a backend that does not block — by reading it until it does.
func (b *backendClient) settle(ctx context.Context, t *a2a.Task) (*a2a.Task, error) {
	for !t.Status.State.Terminal() && t.Status.State != a2a.TaskStateInputRequired && t.Status.State != a2a.TaskStateAuthRequired {
		select {
		case <-ctx.Done():
			return t, nil // answered as failed: still running when time ran out
		case <-time.After(time.Second):
		}
		cl, err := b.get(ctx)
		if err != nil {
			return nil, err
		}
		next, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: t.ID})
		if err != nil {
			if ctx.Err() != nil {
				return t, nil
			}
			return nil, err
		}
		t = next
	}
	return t, nil
}

// isA2AError reports an error the backend answered with, as opposed to one
// of reaching it.
func isA2AError(err error) bool {
	for _, e := range []error{a2a.ErrTaskNotFound, a2a.ErrInvalidParams, a2a.ErrUnsupportedOperation,
		a2a.ErrInternalError, a2a.ErrTaskNotCancelable, a2a.ErrInvalidRequest, a2a.ErrUnauthenticated} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// bearer presents the backend's token on every call, whatever the
// backend's card says about security: the operator configured it.
type bearer struct {
	a2aclient.PassthroughInterceptor
	token string
}

func (i bearer) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if i.token != "" {
		req.ServiceParams["Authorization"] = []string{"Bearer " + i.token}
	}
	return ctx, nil, nil
}

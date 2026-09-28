package main

// provider.go is the provider's side of the joint run, which a test has to
// play itself: responder is the provider's agent, answering text tasks
// over the provider's control plane (/tasks/list, /tasks/reply — what MCP
// reply_task does), and backend is the service behind the provider's
// capabilities (module/service's plain JSON contract).

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// ---------------------------------------------------------------------------
// responder

func cmdResponder(args []string) int {
	fs := flag.NewFlagSet("responder", flag.ExitOnError)
	ctl := fs.String("ctl", "", "the provider's control address, host:port")
	tokenFile := fs.String("token-file", "", "the provider's control_token.txt")
	interval := fs.Duration("interval", 300*time.Millisecond, "how often the provider's inbound tasks are read")
	hold := fs.String("hold", holdMark, "a task whose text holds this is left unanswered")
	_ = fs.Parse(args)
	if *ctl == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "a2aprobe responder: --ctl and --token-file are required")
		return 2
	}
	if err := loopbackAddr(*ctl); err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe responder: %v\n", err)
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe responder: %v\n", err)
		return 2
	}
	// The control token goes only to a listener verified to be this user's daemon, as it does from the
	// CLI (internal/localpeer, A2A-DESIGN §7 item 10 [redteam:F18]).
	r := &responder{ctl: *ctl, token: tok, hold: *hold, answered: map[string]string{},
		hc: localpeer.Client(tok, 60*time.Second)}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("responder: answering text tasks at %s", *ctl)
	var lastErr string
	for {
		if err := r.once(context.Background()); err != nil && err.Error() != lastErr {
			lastErr = err.Error()
			log.Printf("responder: %v", err)
		} else if err == nil {
			lastErr = ""
		}
		time.Sleep(*interval)
	}
}

// responder answers each text task delegated to the provider once per
// requester message: "echo: " and the message's text, completing the task.
type responder struct {
	ctl, token, hold string
	hc               *http.Client
	// answered is, per task, the id of the requester message last dealt
	// with (answered, or held).
	answered map[string]string
}

// ctlTask is the part of the control plane's A2A projection the responder
// reads.
type ctlTask struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
	History []struct {
		MessageID string `json:"messageId"`
		Role      string `json:"role"`
		Parts     []struct {
			Text *string `json:"text"`
		} `json:"parts"`
	} `json:"history"`
	Metadata map[string]any `json:"metadata"`
}

// answer decides what to do with an inbound task: the reply text and the
// message it answers, ok false when there is nothing to do. hold is true
// for a message that asks not to be answered.
func (r *responder) answer(t ctlTask) (reply, msgID string, hold, ok bool) {
	switch t.Status.State {
	case "TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED":
		return "", "", false, false
	}
	if _, isCap := t.Metadata[keySkill]; isCap {
		return "", "", false, false // a capability call answers itself
	}
	if len(t.History) == 0 {
		return "", "", false, false
	}
	last := t.History[len(t.History)-1]
	if last.Role != "ROLE_USER" || r.answered[t.ID] == last.MessageID {
		return "", "", false, false
	}
	var parts []string
	for _, p := range last.Parts {
		if p.Text != nil {
			parts = append(parts, *p.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if r.hold != "" && strings.Contains(text, r.hold) {
		return "", last.MessageID, true, true
	}
	return echoPrefix + text, last.MessageID, false, true
}

// once reads the inbound tasks and answers what waits.
func (r *responder) once(ctx context.Context) error {
	var page struct {
		Tasks []ctlTask `json:"tasks"`
	}
	if _, err := r.call(ctx, "/tasks/list", map[string]any{"role": "inbound", "page_size": 100, "history_length": 20}, &page); err != nil {
		return err
	}
	for _, t := range page.Tasks {
		reply, msgID, hold, ok := r.answer(t)
		if !ok {
			continue
		}
		if hold {
			log.Printf("responder: %s: holding (not answered)", t.ID)
			r.answered[t.ID] = msgID
			continue
		}
		code, err := r.call(ctx, "/tasks/reply", map[string]any{"task_id": t.ID, "text": reply, "state": "completed"}, nil)
		switch {
		case err == nil:
			log.Printf("responder: %s: answered", t.ID)
			r.answered[t.ID] = msgID
		case code >= 400 && code < 500:
			// Not answerable (ended meanwhile, or not a text task): once.
			log.Printf("responder: %s: %v", t.ID, err)
			r.answered[t.ID] = msgID
		default:
			log.Printf("responder: %s: %v (will retry)", t.ID, err)
		}
	}
	return nil
}

// call posts body to a control-plane route with the control token.
func (r *responder) call(ctx context.Context, path string, body, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+r.ctl+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s: %v", path, err)
		}
	}
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// backend

func cmdBackend(args []string) int {
	fs := flag.NewFlagSet("backend", flag.ExitOnError)
	addr := fs.String("addr", "", "where to listen, a loopback host:port")
	tokenFile := fs.String("token-file", "", "the bearer the daemon must present (the service module's token_file); empty: none")
	_ = fs.Parse(args)
	if err := loopbackAddr(*addr); err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe backend: %v\n", err)
		return 2
	}
	b := &backend{calls: map[string]int{}}
	if *tokenFile != "" {
		tok, err := readToken(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "a2aprobe backend: %v\n", err)
			return 2
		}
		b.token = tok
	}
	srv := &http.Server{Addr: *addr, Handler: b, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("backend: listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe backend: %v\n", err)
		return 2
	}
	return 0
}

// backend is a capability service: POST {text} answers {digest, length}
// with digest the hex SHA-256 of text; GET /calls says how often each
// capability (X-ANet-Capability) was called — the check that nothing
// unpaid ran. With a token, a POST without it is refused (401) and not
// counted: the daemon proves itself to its service (A2A-DESIGN §15).
type backend struct {
	token string
	mu    sync.Mutex
	calls map[string]int
}

func (b *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost && b.token != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"the service token is required"}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Path != "/calls" {
			http.NotFound(w, r)
			return
		}
		b.mu.Lock()
		out, _ := json.Marshal(b.calls)
		b.mu.Unlock()
		w.Write(out)
	case http.MethodPost:
		var args map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		text, _ := args["text"].(string)
		capID := r.Header.Get("X-ANet-Capability")
		b.mu.Lock()
		b.calls[capID]++
		b.mu.Unlock()
		log.Printf("backend: %s called by %s (call %s)", capID, r.Header.Get("X-ANet-Caller"), r.Header.Get("X-ANet-Call"))
		out, _ := json.Marshal(map[string]any{"digest": digestHex(text), "length": len(text)})
		w.Write(out)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// loopbackAddr accepts host:port on a loopback address only: the
// responder carries the provider's control token, and the backend is a
// capability anyone reaching it could call.
func loopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q is not a loopback address", addr)
	}
	return nil
}

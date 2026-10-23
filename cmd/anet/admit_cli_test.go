//go:build !no_autoreply

package main

// `anet inbound admit set|off` and `anet autoreply set --admitted` (design
// v0.3.9 §A): the requests they make, and the terminal confirmation that
// turning on direct runs for admitted peers needs.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

type sentReq struct {
	path string
	body map[string]any
}

// bodyDaemon is a control plane that records each request with its body and
// answers /status with autoReply as its auto_reply member.
func bodyDaemon(t *testing.T, autoReply string) (*client, func() []sentReq) {
	t.Helper()
	var mu sync.Mutex
	var sent []sentReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		sent = append(sent, sentReq{r.URL.Path, body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(`{"auto_reply":` + autoReply + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"policy":"closed"}`))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, func() []sentReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]sentReq(nil), sent...)
	}
}

func TestInboundAdmitCommands(t *testing.T) {
	withTTY(t, "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"admit", "set", "org.admit"}, `{"admit":{"capability":"org.admit"}}`},
		{[]string{"admit", "set", "org.admit", "--cache-seconds", "60"}, `{"admit":{"cache_seconds":60,"capability":"org.admit"}}`},
		{[]string{"admit", "off"}, `{"admit":null}`},
	} {
		cl, sent := bodyDaemon(t, "null")
		if err := runInbound(cl, c.args); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		got := sent()
		if len(got) != 1 || got[0].path != "/inbound/policy" {
			t.Fatalf("%v: sent %+v", c.args, got)
		}
		b, _ := json.Marshal(got[0].body)
		if string(b) != c.want {
			t.Errorf("%v: body %s, want %s", c.args, b, c.want)
		}
	}
	for _, args := range [][]string{
		{"admit"}, {"admit", "set"}, {"admit", "set", "a", "b"}, {"admit", "off", "x"},
		{"admit", "set", "org.admit", "--cache-seconds", "0"}, {"admit", "set", "org.admit", "--cache-seconds", "301"},
		{"admit", "set", "org.admit", "--cache-seconds", "x"}, {"admit", "off", "--cache-seconds", "5"}, {"admit", "on"},
	} {
		cl, sent := bodyDaemon(t, "null")
		var ue usageError
		if err := runInbound(cl, args); !errors.As(err, &ue) {
			t.Errorf("%v: %v, want a usage error", args, err)
		}
		if got := sent(); len(got) != 0 {
			t.Errorf("%v reached the daemon: %+v", args, got)
		}
	}
	if err := checkFlags("inbound", []string{"admit", "set", "x", "--cache-seconds", "5"}); err != nil {
		t.Errorf("--cache-seconds is not a known flag of inbound: %v", err)
	}
}

// --admitted direct is confirmed on the terminal unless admitted peers
// already run direct; off and sandbox are not.
func TestAutoReplyAdmittedDirectIsConfirmed(t *testing.T) {
	posted := func(sent []sentReq) map[string]any {
		for _, s := range sent {
			if s.path == "/autoreply" {
				return s.body
			}
		}
		return nil
	}
	// No terminal: refused, nothing written.
	cl, sent := bodyDaemon(t, `{"backend":"exec","admitted":"off"}`)
	withTTY(t, "")
	if err := runAutoReply(cl, []string{"set", "--admitted", "direct"}); err == nil {
		t.Fatal("--admitted direct without a terminal was accepted")
	}
	if posted(sent()) != nil {
		t.Fatal("--admitted direct without a terminal reached POST /autoreply")
	}
	// Declined.
	cl, sent = bodyDaemon(t, `{"backend":"exec","admitted":"sandbox"}`)
	withTTY(t, "no")
	if err := runAutoReply(cl, []string{"set", "--admitted", "direct"}); err == nil || posted(sent()) != nil {
		t.Fatalf("declined: %v, posted %v", err, posted(sent()))
	}
	// Confirmed: the prompt says what it grants, and the setting is sent.
	cl, sent = bodyDaemon(t, `{"backend":"exec","admitted":"off"}`)
	tty := withTTY(t, "yes")
	if err := runAutoReply(cl, []string{"set", "--admitted", "direct"}); err != nil {
		t.Fatal(err)
	}
	if b := posted(sent()); b == nil || b["admitted"] != "direct" {
		t.Fatalf("confirmed: posted %v", b)
	}
	if want := a2ashape.Sentence("autoreply.confirm_admitted_direct", a2ashape.Lang(), nil); !strings.Contains(tty.prompt.String(), want) {
		t.Errorf("the prompt %q does not say %q", tty.prompt.String(), want)
	}
	// Already direct, and the modes that keep the sandbox: no question.
	for _, c := range []struct{ cur, mode string }{{`{"backend":"exec","admitted":"direct"}`, "direct"},
		{`{"backend":"exec","admitted":"direct"}`, "sandbox"}, {"null", "off"}, {"null", "sandbox"}} {
		cl, sent = bodyDaemon(t, c.cur)
		withTTY(t, "")
		if err := runAutoReply(cl, []string{"set", "--admitted", c.mode}); err != nil {
			t.Errorf("%s from %s: %v", c.mode, c.cur, err)
		}
		if b := posted(sent()); b == nil || b["admitted"] != c.mode {
			t.Errorf("%s from %s: posted %v", c.mode, c.cur, b)
		}
	}
	if err := checkFlags("autoreply", []string{"set", "--admitted", "direct"}); err != nil {
		t.Errorf("--admitted is not a known flag of autoreply: %v", err)
	}
}

// `anet autoreply show` says how admitted peers run when it is not off.
func TestAutoReplyShowSaysAdmitted(t *testing.T) {
	lang := a2ashape.Lang()
	var b strings.Builder
	printAutoReplyShow(&b, map[string]any{"backend": "exec", "agent": "claude", "trusted": "direct", "admitted": "direct",
		"sandbox": map[string]any{"state": "off", "message": "x"}}, lang)
	if !strings.Contains(b.String(), a2ashape.Sentence("sandbox.admitted_direct", lang, nil)) {
		t.Errorf("show with admitted=direct:\n%s", b.String())
	}
	b.Reset()
	printAutoReplyShow(&b, map[string]any{"backend": "exec", "agent": "claude", "trusted": "direct", "admitted": "off",
		"sandbox": map[string]any{"state": "off", "message": "x"}}, lang)
	if strings.Contains(b.String(), a2ashape.Label("autoreply.admitted", lang)) {
		t.Errorf("show with admitted=off has an admitted row:\n%s", b.String())
	}
}

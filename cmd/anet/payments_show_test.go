package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// `anet payments` prints the answer on stdout and, on stderr, the limits at
// a glance: the daemon's own sentence, the one MCP's get_balance carries.
func TestPaymentsShowPrintsTheLimitsSummary(t *testing.T) {
	const sentence = "Automatic and agent payments are off (limit 0); manual payments (anet pay): ≤10 each, 50 left today."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"explicit_max":10,"daily_max":50,"left_today":50,"summary":"` + sentence + `"}`))
	}))
	t.Cleanup(srv.Close)
	var so, se bytes.Buffer
	cliStdout, cliStderr = &so, &se
	t.Cleanup(func() { cliStdout, cliStderr = nil, nil })
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	if err := runPayments(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(so.String(), `"left_today": 50`) {
		t.Errorf("stdout %q", so.String())
	}
	if strings.TrimSpace(se.String()) != sentence {
		t.Errorf("stderr %q, want the summary", se.String())
	}
}

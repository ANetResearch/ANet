package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// MOD-F3: `anet balance` without the payment module says module_absent and
// exits 2 like the other payment commands, also when the daemon answers it
// with an error because it has neither the account state nor a balance.
func TestBalanceWithoutX402ExitsTwo(t *testing.T) {
	layout := daemonLayout(t)
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{"without":["x402"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if requireModule(layout, "x402") == nil {
		t.Skip("this binary cannot turn x402 off")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"payments are not available here"}`))
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second, dataDir: layout.Root}
	err := runBalance(c)
	if err == nil {
		t.Fatal("no error")
	}
	var sb strings.Builder
	if code := exitStatus(err, &sb); code != 2 {
		t.Errorf("exit status %d, want 2 (%q)", code, sb.String())
	}
}

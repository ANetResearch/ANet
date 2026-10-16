package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// TestPayAsksNothingWithoutPayer: the commands of the payment module end with
// module_absent and exit status 2 before the terminal is opened and before
// the daemon is contacted, when config.json turns the module off (MOD-2,
// MOD-F3). A -tags no_x402 build is the same path: the module is not
// available either way.
func TestPayAsksNothingWithoutPayer(t *testing.T) {
	layout := daemonLayout(t)
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{"without":["x402"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := openTTY
	t.Cleanup(func() { openTTY = prev })
	openTTY = func() (io.ReadWriteCloser, error) {
		t.Error("the terminal was opened")
		return nil, errors.New("asked")
	}
	for _, c := range []struct {
		cmd  string
		args []string
	}{{"pay", []string{"task-1"}}, {"redeem", []string{"7"}}, {"reconcile", nil}, {"audit-hub", nil}} {
		err := runClient(layout, c.cmd, c.args, false)
		var sb strings.Builder
		if code := exitStatus(err, &sb); err == nil || code != 2 {
			t.Fatalf("%s: err %v, exit status %d, want 2", c.cmd, err, code)
		}
		want := a2ashape.Sentence("module_absent", a2ashape.Lang(), map[string]any{"m": "x402", "reason": absentReason()})
		if strings.TrimSpace(sb.String()) != want {
			t.Errorf("%s: said %q, want %q", c.cmd, sb.String(), want)
		}
		if strings.Contains(sb.String(), "error") {
			t.Errorf("%s: %q", c.cmd, sb.String())
		}
	}
}

// absentReason is how x402 is absent here: not built in under -tags no_x402,
// otherwise turned off by the test's config.json.
func absentReason() string {
	if r := (module.Availability{}).Absence("x402"); r != "" {
		return r
	}
	return module.AbsentWithout
}

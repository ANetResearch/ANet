//go:build !no_autoreply

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// MOD-3, MOD-F3: `anet autoreply` is refused before the daemon is contacted when config.json turns the
// module off, with the one sentence and exit status 2; the API key on the command line is refused ahead of
// it (api_key_on_argv, AV-8).
func TestAutoReplyIsRefusedWhenTurnedOff(t *testing.T) {
	layout := daemonLayout(t)
	if err := requireModule(layout, "autoreply"); err != nil {
		t.Fatalf("nothing turned off: %v", err)
	}
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{"without":["autoreply"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := requireModule(layout, "autoreply")
	if err == nil {
		t.Fatal("the module is turned off and the command is not refused")
	}
	var sb strings.Builder
	if code := exitStatus(err, &sb); code != 2 {
		t.Errorf("exit status %d, want 2", code)
	}
	for _, want := range []string{"auto-reply", "anet init --with autoreply"} {
		if lang := a2ashape.Lang(); lang == a2ashape.English && !strings.Contains(sb.String(), want) {
			t.Errorf("sentence %q lacks %q", sb.String(), want)
		}
	}
	if s, ok := refuseAPIKeyOnArgv("autoreply", []string{"set", "--api-key", "sk-x"}, a2ashape.Lang()); !ok || s == "" {
		t.Error("the key on the command line is not refused first")
	}
}

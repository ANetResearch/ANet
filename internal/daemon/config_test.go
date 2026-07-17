package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// SaveConfig is verbatim: the daemon's in-memory config is authoritative. Setting auto_reply then
// clearing it (nil) must truly remove the block — otherwise `anet autoreply off` could never turn it off.
func TestSaveConfigWritesVerbatimIncludingClear(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	auto := &AutoReplyConfig{Backend: "exec", Agent: "cursor"}
	if err := SaveConfig(l, Config{ControlAddr: "127.0.0.1:39999", AutoReply: auto}); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfig(l); err != nil || got.AutoReply == nil || got.AutoReply.Agent != "cursor" {
		t.Fatalf("auto_reply not persisted: %+v (err %v)", got.AutoReply, err)
	}
	// Clear it: nil block must not be resurrected from disk.
	if err := SaveConfig(l, Config{ControlAddr: "127.0.0.1:39999"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoReply != nil {
		t.Fatalf("auto_reply should be cleared, got %+v", got.AutoReply)
	}
}

func TestSaveConfigCreatesFileWhenMissing(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	cfg := DefaultConfig()
	if err := SaveConfig(l, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); err != nil {
		t.Fatal(err)
	}
}

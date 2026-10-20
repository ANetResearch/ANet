//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/fsperm"
)

// On Windows the controller grant writes "shell":"powershell" into modules.shell, puts the AID on
// peers.allow, and leaves config.json and peers.allow private to this user.
func TestWindowsInitShellControllerWritesPowerShell(t *testing.T) {
	l := daemon.NewLayout(filepath.Join(t.TempDir(), "data"))
	opened := withShellCtl(t, true, "yes")
	if err := initShellControllerTo(io.Discard, l, false, shellCtlAID); err != nil {
		t.Fatalf("error %v (exit %d)", err, exitStatus(err, io.Discard))
	}
	if *opened != 1 {
		t.Fatalf("terminal opened %d times, want 1", *opened)
	}
	block, allow := shellCtlOnDisk(t, l)
	want := map[string]any{"allow": []any{shellCtlAID}, "allow_arbitrary": true, "shell": "powershell"}
	if !reflect.DeepEqual(block, want) {
		t.Fatalf("modules.shell = %v", block)
	}
	if !strings.Contains(allow, shellCtlAID+"\n") {
		t.Fatalf("peers.allow = %q", allow)
	}
	for _, p := range []string{l.Root, l.ConfigPath(), filepath.Join(l.Root, "peers.allow")} {
		if err := fsperm.CheckPrivate(p); err != nil {
			t.Errorf("%v", err)
		}
	}
	// A second run changes nothing and asks nothing.
	if err := initShellControllerTo(io.Discard, l, false, shellCtlAID); err != nil {
		t.Fatal(err)
	}
	if *opened != 1 {
		t.Fatalf("an unchanged grant opened the terminal (%d)", *opened)
	}
}

// A declined confirmation on Windows writes nothing and is exit status 1.
func TestWindowsInitShellControllerDeclined(t *testing.T) {
	l := daemon.NewLayout(filepath.Join(t.TempDir(), "data"))
	withShellCtl(t, true, "no")
	err := initShellControllerTo(io.Discard, l, false, shellCtlAID)
	if err == nil || exitStatus(err, io.Discard) != 1 {
		t.Fatalf("error %v", err)
	}
	if _, err := os.Stat(l.Root); !os.IsNotExist(err) {
		t.Fatal("the data directory was created")
	}
}

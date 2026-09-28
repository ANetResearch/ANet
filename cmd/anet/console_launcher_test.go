package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// F19 (red team, si7): `anet console` ran xdg-open with the ticket URL, so the ticket sat in the argv of
// the opener and the browser, where every local user reads it (/proc/<pid>/cmdline is 0444 without
// hidepid) and redeems it before the browser does. The skeptic's PoC scanned /proc/*/cmdline while a
// stand-in xdg-open ran and won every time. Now the opener is given the path of a launcher page in the
// private runtime directory; the ticket is in the file, and the file is gone once redeemed
// (A2A-DESIGN §7.2 [redteam:F19]).
func TestConsoleCommandKeepsTheTicketOffEveryCommandLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stands in for xdg-open and reads /proc")
	}
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)
	layout, addr := servedDaemon(t)

	// The stand-in opener records its arguments and stays alive a moment, as a real one does while the
	// browser starts.
	bin := t.TempDir()
	rec := filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + rec + ".tmp' && mv '" + rec + ".tmp' '" + rec + "'\nsleep 1\n"
	if err := os.WriteFile(filepath.Join(bin, "xdg-open"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out := captureStdout(t, func() {
		if err := runClient(layout, "console", nil, true); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(out, "#t=") {
		t.Fatalf("the ticket URL was printed: %q", out)
	}
	var argv []byte
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(rec); err == nil {
			argv = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the opener was not run")
		}
	}
	args := strings.Split(strings.TrimRight(string(argv), "\n"), "\n")
	if len(args) != 1 || !filepath.IsAbs(args[0]) || strings.Contains(args[0], "#") || filepath.Dir(args[0]) != daemon.RuntimeDir() {
		t.Fatalf("the opener was given %q; want one launcher path in %s", args, daemon.RuntimeDir())
	}
	page, err := os.ReadFile(args[0])
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`http://` + regexp.QuoteMeta(addr) + `/console#t=([A-Za-z0-9_-]+)"`).FindSubmatch(page)
	if m == nil {
		t.Fatalf("launcher page does not redirect to this daemon's console:\n%s", page)
	}
	ticket := m[1]

	// While the opener runs, no process on this machine shows the ticket in its arguments.
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, ticket) {
			t.Fatalf("%s carries the ticket: %q", p, b)
		}
	}

	// The page's ticket opens the console once, and the page is gone.
	body, _ := json.Marshal(map[string]string{"ticket": string(ticket)})
	req, _ := http.NewRequest("POST", "http://"+addr+"/console/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+addr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the launcher's ticket does not open a session: %d", resp.StatusCode)
	}
	if _, err := os.Stat(args[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the launcher page outlived its ticket: %v", err)
	}
}

package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var launcherTicket = regexp.MustCompile(`/console#t=([A-Za-z0-9_-]+)"`)

// launcherFor asks p for a ticket with a launcher page and returns the page's path and the ticket in it.
func launcherFor(t *testing.T, p *plane) (string, string) {
	t.Helper()
	resp, b := p.req(t, "POST", "/console/ticket", `{"launcher":true}`, p.bearer)
	if resp.StatusCode != 200 {
		t.Fatalf("ticket = %d %s", resp.StatusCode, b)
	}
	var out struct {
		Launcher string `json:"launcher"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Launcher == "" {
		t.Fatalf("no launcher in %s", b)
	}
	page, err := os.ReadFile(out.Launcher)
	if err != nil {
		t.Fatal(err)
	}
	m := launcherTicket.FindSubmatch(page)
	if m == nil {
		t.Fatalf("launcher page has no ticket URL:\n%s", page)
	}
	return out.Launcher, string(m[1])
}

// F19 (red team, si7): the launcher page `anet console` opens instead of a ticket URL is private to this
// uid and lives no longer than its ticket: it is removed when the ticket is redeemed, and when the
// ticket expires unredeemed, without any further request to the daemon (A2A-DESIGN §7.2 [redteam:F19]).
func TestConsoleLauncherIsPrivateAndLivesNoLongerThanItsTicket(t *testing.T) {
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)
	p := newPlane(t)

	path, ticket := launcherFor(t, p)
	if filepath.Dir(path) != RuntimeDir() {
		t.Fatalf("launcher %s is not in the private runtime dir %s", path, RuntimeDir())
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("launcher mode %v %v", fi.Mode(), err)
	}
	if strings.Contains(path, ticket) {
		t.Fatal("the launcher's path carries the ticket")
	}
	// Redeemed: gone.
	if r, b := p.startSession(t, ticket); r.StatusCode != 200 {
		t.Fatalf("the launcher's ticket does not open a session: %d %s", r.StatusCode, b)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launcher still there after the ticket was redeemed: %v", err)
	}

	// Expired unredeemed: gone on its own.
	p.cp.sessions.mu.Lock()
	p.cp.sessions.ttl = 150 * time.Millisecond
	p.cp.sessions.mu.Unlock()
	path, ticket = launcherFor(t, p)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher outlived its ticket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r, _ := p.startSession(t, ticket); r.StatusCode == 200 {
		t.Fatal("an expired ticket opened a session")
	}
}

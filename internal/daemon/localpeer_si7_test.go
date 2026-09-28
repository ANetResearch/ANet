package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// F18 (red team, si7): /console/switch took an unauthenticated /ping 200 as proof that a registry entry
// is a live daemon of this user and then sent the target identity's control token there. After the
// target daemon crashes its entry stays behind, and another local user can bind the recorded port. The
// token must not go to that listener (A2A-DESIGN §7.4 [redteam:F18]).
func TestConsoleSwitchDoesNotSendTheTargetTokenToAPortSquatter(t *testing.T) {
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)

	a := newPlane(t)
	// B crashed: its data dir, identity and token are there, its registry entry names its old port, and
	// another local user now listens on that port, answering /ping like a daemon.
	bd := newBareDaemon(t)
	if _, err := loadOrGenControlToken(bd.layout); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	squat := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			mu.Lock()
			sent = append(sent, h)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/ping" {
			_, _ = w.Write([]byte(`{"anet":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"url":"http://` + r.Host + `/console#t=x"}`))
	})}
	go func() { _ = squat.Serve(ln) }()
	t.Cleanup(func() { _ = squat.Close() })
	defer localpeer.TreatAsForeignForTest(ln.Addr().String())()
	entry, _ := json.Marshal(IdentityEntry{AID: bd.AID(), Name: "b", ControlAddr: ln.Addr().String(), DataDir: bd.layout.Root})
	if err := ensurePrivateDir(DaemonsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(DaemonsDir(), "b.json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}

	s := a.session(t)
	resp, body := a.req(t, "POST", "/console/switch", `{"aid":"`+bd.AID()+`"}`, s.as(a))
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 0 {
		t.Fatalf("the switcher sent %q to another user's listener", sent)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("switch to a squatted port succeeded: %s", body)
	}
	if !strings.Contains(string(body), "not sent") {
		t.Fatalf("switch error does not say the token stayed here: %s", body)
	}
}

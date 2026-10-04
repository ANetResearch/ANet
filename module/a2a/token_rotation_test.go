//go:build !no_a2a

package a2a

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/anethome"
)

// LEC AV-10 (EXE-5): the interface accepts the token its file holds now. The daemon's rotateLocalTokens
// replaces a2a_token.txt by renaming a new file over it; from the next request on the old token is
// refused (401) and the new one works, on a running node, without a restart. A file edited in place is
// followed the same way, and a missing or malformed file accepts nothing.
func TestTheInterfaceFollowsItsTokenFile(t *testing.T) {
	n := newSI7Node(t)
	// GetTask of an unknown task: past the token check the answer is 200 with a TaskNotFound error, and
	// 401 before it.
	rpc := n.a2aURL + agentsPath + "/bafyremoteagent/jsonrpc"
	status := func(tok string) int {
		resp, _ := si7Do(t, "POST", rpc, rpcGet, bearerOf(tok))
		return resp.StatusCode
	}
	if got := status(n.a2aToken); got != http.StatusOK {
		t.Fatalf("the token at start: %d", got)
	}
	path := filepath.Join(anethome.A2ADir(n.root), anethome.A2ATokenFile)
	replace := func(tok string) {
		t.Helper()
		f, err := os.CreateTemp(filepath.Dir(path), ".a2a_token.txt.*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(tok + "\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := os.Rename(f.Name(), path); err != nil {
			t.Fatal(err)
		}
	}
	rotated := strings.Repeat("5e", 32)
	replace(rotated)
	if got := status(n.a2aToken); got != http.StatusUnauthorized {
		t.Fatalf("the old token after the file was replaced: %d, want 401", got)
	}
	if got := status(rotated); got != http.StatusOK {
		t.Fatalf("the new token: %d", got)
	}
	// Edited in place (same inode, new size).
	edited := strings.Repeat("7c", 40)
	if err := os.WriteFile(path, []byte(edited+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status(rotated) != http.StatusUnauthorized || status(edited) != http.StatusOK {
		t.Fatal("an in-place edit of the token file was not followed")
	}
	// Not a token, then gone: nothing is accepted.
	replace("short")
	if status(edited) != http.StatusUnauthorized || status("short") != http.StatusUnauthorized {
		t.Fatal("a malformed token file let a request in")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if status(edited) != http.StatusUnauthorized {
		t.Fatal("a missing token file let a request in")
	}
}

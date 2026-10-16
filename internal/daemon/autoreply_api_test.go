//go:build mod4_pending

package daemon

// LEC §2.3.3, §2.3.4, AV-8 and AV-10 (EXE-5): POST /autoreply merges what it names into the current
// configuration and never shows the key; /status says only whether one is set; rotateLocalTokens
// replaces both local tokens and the old ones stop working at once.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/localpeer"
)

// postAutoReply sends one /autoreply body to d's handler.
func postAutoReply(t *testing.T, d *Daemon, body string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// statusOf is d's /status answer.
func statusOf(t *testing.T, d *Daemon) (map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out, rec.Body.String()
}

func sameAutoReply(a, b AutoReplyConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func readTokenFile(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", path, fi, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

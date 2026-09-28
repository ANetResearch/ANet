package scripts

// scripts/a2a-tck-responder.py answers a2a-tck's scenarios by the client's messageId prefix. Since
// redteam F33 the provider's view of a message the requester wrote carries the envelope's id as
// messageId, and the client's id only as metadata a2a.messageId; the responder that read messageId
// matched no scenario and answered every task "echo: …", so every a2a-tck case that needs a scenario
// answer failed or was skipped (docs/notes/0029 §4).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTheTCKResponderReadsTheClientsMessageIDFromTheMetadata(t *testing.T) {
	needPython(t)
	var mu sync.Mutex
	var replies []map[string]any
	got := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/list":
			// What the provider's control plane lists after F33: the envelope id as messageId.
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": []any{map[string]any{
				"id":     "ix_1",
				"status": map[string]any{"state": "TASK_STATE_SUBMITTED"},
				"history": []any{map[string]any{
					"role": "ROLE_USER", "messageId": "104a95c4ba587d700c3bd07cb5e761c6",
					"metadata": map[string]any{"a2a.messageId": "tck-artifact-text-24df3b49"},
					"parts":    []any{map[string]any{"text": "TCK artifact test"}},
				}},
			}}})
		case "/tasks/reply":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			replies = append(replies, body)
			mu.Unlock()
			select {
			case got <- struct{}{}:
			default:
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "control_token.txt")
	if err := os.WriteFile(tok, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "a2a-tck-responder.py", srv.Listener.Addr().String(), tok)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("the responder answered nothing")
	}
	mu.Lock()
	defer mu.Unlock()
	msg, _ := replies[0]["message"].(map[string]any)
	parts, _ := msg["parts"].([]any)
	var text string
	if len(parts) > 0 {
		text, _ = parts[0].(map[string]any)["text"].(string)
	}
	if replies[0]["state"] != "completed" || text != "Generated text content" {
		t.Fatalf("answered %v: want the tck-artifact-text scenario's reply (completed, %q)", replies[0], "Generated text content")
	}
}

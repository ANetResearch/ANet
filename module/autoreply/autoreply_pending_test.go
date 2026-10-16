package autoreply

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests that drive the auto-reply module through its unexported API or through the daemon fixtures of the
// old package. They are not compiled until they are ported to module/autoreply (see the MOD-4 notes).

func (r denyDuring) Reply(ctx context.Context, rc replyContext, turns []chatTurn) (string, error) {
	r.deny()
	return r.inner.Reply(ctx, rc, turns)
}

// A server that ignores stream and answers JSON is read as JSON.
func TestOpenAIReplierStreamAnsweredWithJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" whole "}}]}`))
	}))
	defer srv.Close()
	r := &openAIReplier{cfg: AutoReplyConfig{APIBase: srv.URL, Model: "m"}}
	got, err := r.Reply(context.Background(), replyContext{Preview: func(string, string) {}}, []chatTurn{{Role: "user", Text: "hi"}})
	if err != nil || got != "whole" {
		t.Fatalf("%q %v", got, err)
	}
}

// A stream that breaks, ends early or is empty is an error: never half an
// answer sent as a whole one.
func TestStreamErrors(t *testing.T) {
	for name, s := range map[string]string{
		"no end":     "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n",
		"no content": "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"empty":      "",
		"too long":   "data: " + strings.Repeat("x", maxSSELine+10) + "\n",
	} {
		if got, err := readChatStream(strings.NewReader(s), func(string, string) {}); err == nil {
			t.Errorf("%s: %q accepted", name, got)
		}
	}
}

// from wire_edges_test.go
// denyDuring is a replier through which the peer is denied while the
// backend works.
type denyDuring struct {
	inner autoReplier
	deny  func()
}

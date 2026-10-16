package autoreply

// autoreply_control.go — the control-plane face of the auto-reply module (K-3, K-4): POST /autoreply
// (LEC §2.3.4) and POST /autoreply-test (§2.3.5, CL-3) as a module.ControlHandler, and the auto_reply
// member of /status and the SI-5 value as a module.StatusContributor. The kernel owns the route table,
// the bearer authentication, the body limit and the error body; this file only answers for its two
// routes while the module is available.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// HandleControl implements module.ControlHandler.
func (m *autoReplyModule) HandleControl(ctx context.Context, route string, body []byte) (int, any, error) {
	switch route {
	case "POST /autoreply":
		return m.handleSet(ctx, body)
	case "POST /autoreply-test":
		return m.handleTest(ctx, body)
	}
	return http.StatusNotFound, nil, errors.New("not found")
}

// handleSet changes the auto-reply configuration live, behind `anet autoreply set|off` (LEC §2.3.4).
// `{"off":true}` removes the whole block. Any other body is merged into the current configuration: the
// members it names change, the others stay, so settings made one at a time do not undo each other; a
// member set to null goes back to its default. api_key is write-only: absent keeps it, a value replaces
// it, "" is refused (api_key_empty). The answer never holds the key, only api_key_set. "off" is a
// boolean: any other value would leave auto-reply running while the caller believes it stopped, so it
// is refused.
func (m *autoReplyModule) handleSet(_ context.Context, body []byte) (int, any, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil || members == nil {
		return http.StatusBadRequest, nil, errors.New("bad request")
	}
	off := false
	if raw, ok := members["off"]; ok && string(raw) != "null" && json.Unmarshal(raw, &off) != nil {
		return http.StatusBadRequest, nil, arConfigInvalid("off", arRule("只能是 true 或 false", "it must be true or false"))
	}
	// The configuration read here is the one the merge result replaces: a second request waits (apply),
	// so two settings sent at once do not undo each other.
	cfg, err := m.apply(func(cur *AutoReplyConfig) (*AutoReplyConfig, error) {
		if off {
			return nil, nil
		}
		next, ref := mergeAutoReply(cur, members)
		if ref != nil {
			return nil, ref
		}
		return next, nil
	})
	if err != nil {
		var ref *a2ashape.Refusal
		switch {
		case errors.As(err, &ref):
			return http.StatusBadRequest, nil, err
		case errors.Is(err, module.ErrConfigConflict):
			return http.StatusConflict, nil, err
		}
		return http.StatusBadRequest, nil, err
	}
	var view any
	resp := map[string]any{"status": "updated"}
	if cfg != nil {
		v := m.autoReplyView(cfg)
		view = v
		resp["sandbox"] = v["sandbox"]
		if sb, ok := v["sandbox"].(map[string]any); ok {
			resp["message"] = sb["message"]
		}
	}
	resp["auto_reply"] = view
	return http.StatusOK, resp, nil
}

// handleTest runs the enabled paths once on a synthetic prompt (no Hub, no identity) so an operator or
// agent can verify auto-reply works without registering a throwaway node. Behind `anet autoreply test`
// (LEC §2.3.5, CL-3). A run here is not counted in the budget (BU-7).
func (m *autoReplyModule) handleTest(ctx context.Context, body []byte) (int, any, error) {
	var req struct {
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(body, &req)
	results, err := m.TestAutoReplyPaths(ctx, req.Prompt)
	if err != nil {
		return http.StatusBadRequest, nil, err
	}
	return http.StatusOK, map[string]any{"results": results}, nil
}

// StatusMember implements module.StatusContributor: the auto_reply member of /status, which is absent
// (nil) while auto-reply is not configured.
func (m *autoReplyModule) StatusMember() (string, any) {
	cfg := m.current()
	if cfg == nil {
		return "auto_reply", nil
	}
	return "auto_reply", m.autoReplyView(cfg)
}

// SI5 implements module.StatusContributor: the SI-5 values the module holds.
func (m *autoReplyModule) SI5() map[string]any {
	var raw json.RawMessage
	if cfg := m.current(); cfg != nil {
		raw = marshalAutoReply(cfg)
	}
	return si5OfBlock(raw)
}

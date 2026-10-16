package daemon

// preview_source.go is where the two sources of a provider's preview meet
// the kernel (CUX-P5, UX-9): the openai auto-reply, which streams its
// model's answer, and the control-plane route POST /tasks/preview, which
// takes the text an operator's own agent or backend has written so far.
//
// Both reach the coalescer only through previewWanted and previewOffer.
// They are the one place that knows when a round may be previewed (CUX-P4)
// and what this node's stream_preview allows, so that a source asks and
// offers and never reads the configuration or the store itself.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// maxPreviewPostBody bounds a POST /tasks/preview body: text of at most
// 1 MiB (CUX §4.8), reasoning of the same size, and the JSON around them.
const maxPreviewPostBody = 4<<20 + 4096

// maxPreviewPostText bounds the text, and the reasoning, of one POST.
const maxPreviewPostText = 1 << 20

// previewWanted reports whether the current round of task ix is to be
// previewed (CUX-P4): this node's stream_preview is not off, the task is an
// inbound text task that has not ended and has no result, and the
// requester's message that opened the round asked for it and has not been
// answered. mode is what the round asked for.
func (d *Daemon) previewWanted(ix string) (mode string, ok bool) {
	if d.config().streamPreview() == streamPreviewOff {
		return "", false
	}
	if _, open := d.openPreviewTask(ix); !open {
		return "", false
	}
	return d.previews().Wanted(ix)
}

// previewOffer gives the coalescer what the provider has written so far in
// the round of task ix — the whole text and reasoning, not a delta. It
// reports whether the round took it: false when previews are off on this
// node or the round is not one that asked for them (CUX-P4). The coalescer
// decides when a frame goes.
func (d *Daemon) previewOffer(ix, text, reasoning string) bool {
	node := d.config().streamPreview()
	if node == streamPreviewOff {
		return false
	}
	return d.previews().Offer(ix, text, reasoning, node)
}

// openPreviewTask is the task ix as a preview may be made for it: an
// inbound text task, not a capability call, not ended and without a result.
func (d *Daemon) openPreviewTask(ix string) (*interactions.Interaction, bool) {
	row, err := d.ix.Get(ix)
	if err != nil || row.Role != interactions.RoleInbound || row.IsCapability ||
		row.Trust == interactions.TrustPublicCap || row.IsTerminal() || len(row.Result) > 0 {
		return nil, false
	}
	return row, true
}

// hTasksPreview is POST /tasks/preview (CUX §4.8): the provider's own agent
// or backend pushes what it has written so far for the round it is
// answering. Bearer only. {"task_id", "text", "reasoning"?}:
//
//	409 task_not_open      not an open inbound text task of this node
//	409 preview_off        this node's stream_preview is off
//	200 sent: false        the requester did not ask for a preview this turn
//	200 sent: true         handed to the coalescer (not: delivered)
func (d *Daemon) hTasksPreview(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPreviewPostBody))
	var req struct {
		TaskID    string `json:"task_id"`
		Text      string `json:"text"`
		Reasoning string `json:"reasoning"`
	}
	if err != nil || json.Unmarshal(b, &req) != nil || req.TaskID == "" ||
		len(req.Text) > maxPreviewPostText || len(req.Reasoning) > maxPreviewPostText {
		badTaskRequest(w, "body: a preview is {\"task_id\", \"text\" of at most 1 MiB, \"reasoning\"?}")
		return
	}
	if _, open := d.openPreviewTask(req.TaskID); !open {
		writeRefusal(w, http.StatusConflict, a2ashape.Refuse("task_not_open", nil))
		return
	}
	if d.config().streamPreview() == streamPreviewOff {
		writeRefusal(w, http.StatusConflict, a2ashape.Refuse("preview_off", nil))
		return
	}
	if !d.previewOffer(req.TaskID, req.Text, req.Reasoning) {
		writeJSON(w, http.StatusOK, map[string]any{"sent": false, "reason": "not_requested",
			"message": a2ashape.Sentence("not_requested", a2ashape.Lang(), nil)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}

// hPreview is POST /preview, behind `anet preview` (CUX §4.5): {} says what
// stream_preview is; {"mode": "off"|"on"|"with_reasoning"} sets it, saves
// config.json first and then puts it in force, and records the change. The
// CLI asks for the terminal confirmation that with_reasoning needs; the
// route takes the control token alone, so it is on no surface an agent or a
// browser session reaches (never in sessionRoutes or MCP's allowed paths).
func (d *Daemon) hPreview(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	var req struct {
		Mode *string `json:"mode"`
	}
	if err != nil || json.Unmarshal(b, &req) != nil {
		badTaskRequest(w, "body: {\"mode\": \"off\"|\"on\"|\"with_reasoning\"}")
		return
	}
	if req.Mode == nil {
		writeJSON(w, http.StatusOK, map[string]any{"mode": d.config().streamPreview()})
		return
	}
	if !validStreamPreview(*req.Mode) {
		writeRefusal(w, http.StatusConflict, a2ashape.Refuse("config_invalid",
			a2ashape.Args{"key": "stream_preview", "rule": "off, on or with_reasoning"}))
		return
	}
	from, err := d.SetStreamPreview(*req.Mode)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": *req.Mode, "from": from})
}

// SetStreamPreview puts stream_preview in force: config.json is saved
// first, then the setting is live (redteam F9), the change is recorded
// (anet.policy.changed), and a change between off and on or with_reasoning
// makes the node republish its card, which declares previews when they are
// on (CUX-P30). A change between on and with_reasoning does not: the card
// says the same. It returns the setting it replaced.
func (d *Daemon) SetStreamPreview(mode string) (from string, err error) {
	if !validStreamPreview(mode) {
		return "", interactions.ErrBadInput
	}
	if _, err = d.updateConfig(func(c *Config) error {
		from = c.streamPreview()
		c.StreamPreview = mode
		return nil
	}, func(c *Config) { c.StreamPreview = mode }); err != nil {
		return "", err
	}
	if from == mode {
		return from, nil
	}
	d.recordPolicyChange("stream_preview", from, mode, nil)
	if (from == streamPreviewOff) != (mode == streamPreviewOff) {
		d.cardInputsChanged()
	}
	return from, nil
}

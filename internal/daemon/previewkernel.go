package daemon

// previewkernel.go is what the kernel keeps of stream_preview besides the
// seam (previewseam.go): the vocabulary of the protocol (the modes, the
// metadata key, the event), the Previewer of a node without previews, and
// the reading of a round's mode. The coalescer and the cache are the
// preview module's (module/preview); the kernel reaches them only through
// module.Previewer (d.previews()) and previewArrived.

import (
	"encoding/json"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// The preview modes (core §2.13, CUX-P0): what a requester's message asks
// for in anet.preview_mode, and what this node's stream_preview allows.
const (
	previewModeOn            = "on"
	previewModeWithReasoning = "with_reasoning"
	// metaPreviewMode is the end-to-end metadata key of a round's message.
	metaPreviewMode = "anet.preview_mode"
)

// previewCacheTasks bounds the table of requested rounds, as it bounds the
// preview module's own rounds and cache (CUX-P15).
const previewCacheTasks = 1024

// previewSendTimeout bounds one frame's hand-over; the module holds the
// same bound (module/preview).
const previewSendTimeout = 30 * time.Second

// cntPreviewDropped counts a frame this node did not send: over the node's
// frames per second, or one that would not pass ValidatePreview (CUX-P6,
// CUX-P2).
const cntPreviewDropped = "preview-dropped"

// EventPreview says a preview frame was taken for a task this node
// delegated (CUX-P15): the event carries no text, and Current has the frame
// until the task moves on. It changes nothing stored.
const EventPreview = "preview"

// The preview types are the seam's (module/preview.go): the kernel and the
// implementation share them.
type (
	previewContent = module.PreviewContent
	previewFrame   = module.PreviewSnapshot
	previewIn      = module.PreviewIn
)

// previewsUnavailable is the Previewer while previews are not available on
// this node — the absence behavior of MOD-P5, and not a stub: no round is
// recorded, nothing is offered or sent, and every frame that arrives is
// dropped (previewArrived counts it preview-unsupported before it gets
// here). It is what d.previews() answers when no Previewer is set.
type previewsUnavailable struct{}

func (previewsUnavailable) Requested(string, string, string) {}

func (previewsUnavailable) Offer(string, string, string) module.PreviewOffer {
	return module.PreviewOffer{}
}

func (previewsUnavailable) CancelRound(string) {}

func (previewsUnavailable) Epoch() uint64 { return 0 }

func (previewsUnavailable) Arrived(previewIn) bool { return false }

func (previewsUnavailable) Clear(string) {}

func (previewsUnavailable) Current(string) (previewFrame, bool) { return previewFrame{}, false }

var _ module.Previewer = previewsUnavailable{}

// previewModeOf is CUX-P0: the anet.preview_mode a requester's message
// metadata carries, read before anet.* is stripped for storage. A value
// other than "on" or "with_reasoning" is no request — not an error, and not
// counted as malformed — so that a later mode does not break this one.
func previewModeOf(meta []byte) string {
	if len(meta) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return ""
	}
	var v string
	if json.Unmarshal(m[metaPreviewMode], &v) != nil {
		return ""
	}
	return normalPreviewMode(v)
}

// normalPreviewMode is v when it is a mode that sends previews, else "".
func normalPreviewMode(v string) string {
	switch v {
	case previewModeOn, previewModeWithReasoning:
		return v
	}
	return ""
}

// previewOut is a frame as the kernel judges it before it is sent: the
// round it belongs to, the mode the round asked for, this node's setting
// and what the frame says (previewChatMsg).
type previewOut struct {
	IX       string
	Answers  string
	Mode     string // the round's anet.preview_mode
	NodeMode string // this node's stream_preview
	Seq      uint64
	previewContent
}

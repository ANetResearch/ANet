package daemon

// taskview.go reads an interaction into its A2A projection. The mapping
// itself is a2ashape.Project (A2A-DESIGN §11.5, SI-6); this file supplies
// what only the daemon has: attachment bytes, safe file names and the key
// history a receipt was checked against, and the payment flow's reading of
// the task's payment columns (PaymentStatusMeta). The projection carries the state
// sequence number (a2ashape.KeyStateSeq), which a caller passes back to
// /tasks/wait as after_seq to wait for a state newer than the one it has
// seen (C35).

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// viewOpts selects what a task view carries.
type viewOpts struct {
	// historyLen bounds the history to the newest n messages; nil is all
	// of it and 0 is none.
	historyLen *int
	// artifacts includes the artifacts (ListTasks leaves them out unless
	// asked, A2A-DESIGN §11.5).
	artifacts bool
	// inline carries attachment bytes in raw parts, in the artifacts and
	// status.message, up to a2ashape.MaxInlineBytes for the task; the
	// history and everything past the limit are placeholders
	// (anet.attachment_cid, 0017 Q12). The A2A interface's single-task
	// reads do: its client has no other way to fetch the bytes. The
	// control plane, lists and streams give the placeholder, which GET
	// /attachment and `anet pull` resolve, and which keeps a task small
	// enough to hand to a model.
	inline bool
}

// taskView renders ix as an A2A task.
func (d *Daemon) taskView(ix *interactions.Interaction, o viewOpts) (a2ashape.Task, error) {
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	atts, err := d.ix.Attachments(ix.ID)
	if err != nil {
		return a2ashape.Task{}, err
	}
	opt := a2ashape.Options{HistoryLength: o.historyLen, Artifacts: o.artifacts, InlineFiles: o.inline, SafeName: safeName}
	if o.inline {
		opt.LoadFile = func(cid string) ([]byte, error) {
			a, err := d.ix.AttachmentData(ix.ID, cid)
			if err != nil {
				return nil, err
			}
			return a.Data, nil
		}
	}
	if o.artifacts && len(ix.Receipt) > 0 && ix.Role == interactions.RoleOutbound {
		// The key the receipt was checked against travels with it, so a
		// holder can check it again (as /results has always done).
		opt.ProviderKEL = d.encodedPeerKEL(ix.PeerAID)
	}
	// The x402 part of the status as the payment flow derives it from its
	// own columns (A2A-DESIGN §8.2, §11.5); the projection places it.
	t := a2ashape.Project(a2ashape.Source{Interaction: ix, Messages: msgs, Attachments: atts,
		Payment: d.PaymentStatusMeta(ix)}, opt)
	if t.Metadata == nil {
		// Project always sets it; callers add keys (anet.wait) to it.
		t.Metadata = map[string]any{}
	}
	return t, nil
}

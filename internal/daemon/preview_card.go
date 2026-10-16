package daemon

// preview_card.go is the kernel's own contribution to the cards it signs:
// the declaration of the preview extension (correctness-ux CUX-P30). It is
// given here, in cardContributions, and not by a module: the extension is
// a statement about this node's stream_preview setting.

import (
	"github.com/ANetResearch/ANet/module"
)

// previewExtensionDescription is the declaration's text.
const previewExtensionDescription = "This agent can send a preview of its reply while it is being written; " +
	"a client that activates the extension receives it as updates of the anet.reply artifact."

// nodeStreamPreview is this node's stream_preview setting: "off" (the
// default), "on" or "with_reasoning".
func (d *Daemon) nodeStreamPreview() string {
	if d.streamPreviewFn != nil {
		if m := normalPreviewMode(d.streamPreviewFn()); m != "" {
			return m
		}
	}
	return "off"
}

// kernelCardExtensions are the extensions the kernel declares on every
// card it signs. The preview extension is declared when stream_preview is
// not off, the same for "on" and "with_reasoning" (reasoning does not go
// over A2A, CUX-P17); it is not required and has no params.
func (d *Daemon) kernelCardExtensions() []map[string]any {
	if d.nodeStreamPreview() == "off" {
		return nil
	}
	return []map[string]any{{"uri": module.ExtPreviewURI, "description": previewExtensionDescription}}
}

// streamPreviewChanged is called when stream_preview changes from old to
// new. The card says whether previews are sent, so a change between off and
// not off signs and publishes the card again; a change between "on" and
// "with_reasoning" does not alter the card.
func (d *Daemon) streamPreviewChanged(old, cur string) {
	if (normalPreviewMode(old) == "") != (normalPreviewMode(cur) == "") {
		d.cardInputsChanged()
	}
}

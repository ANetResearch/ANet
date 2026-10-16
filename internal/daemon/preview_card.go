package daemon

// preview_card.go is the preview module's contribution to the cards this
// node signs: the declaration of the preview extension (correctness-ux
// CUX-P30, K-15). It is given in cardContributions, for the public card and
// the extended card alike, and only while previews are available and
// stream_preview is not off: a node without the module never declares it.

import (
	"github.com/ANetResearch/ANet/module"
)

// previewExtensionDescription is the declaration's text.
const previewExtensionDescription = "This agent can send a preview of its reply while it is being written; " +
	"a client that activates the extension receives it as updates of the anet.reply artifact."

// nodeStreamPreview is this node's stream_preview setting: "off" (the
// default), "on" or "with_reasoning".
func (d *Daemon) nodeStreamPreview() string {
	if !d.previewAvailable() {
		return "off"
	}
	if d.streamPreviewFn != nil {
		if m := normalPreviewMode(d.streamPreviewFn()); m != "" {
			return m
		}
		return "off"
	}
	if m := normalPreviewMode(d.config().streamPreview()); m != "" {
		return m
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

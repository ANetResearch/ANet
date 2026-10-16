package daemon

// preview_card.go is the kernel's side of the preview declaration on the
// cards this node signs (correctness-ux CUX-P30, K-15): the setting the
// module reads through the seam, and the republication when it changes. The
// declaration itself is the preview module's, given through
// module.CardContributor for the public card and the extended card alike,
// and only while previews are available and stream_preview is not off: a
// node without the module never declares it.

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

// streamPreviewChanged is called when stream_preview changes from old to
// new. The card says whether previews are sent, so a change between off and
// not off signs and publishes the card again; a change between "on" and
// "with_reasoning" does not alter the card.
func (d *Daemon) streamPreviewChanged(old, cur string) {
	if (normalPreviewMode(old) == "") != (normalPreviewMode(cur) == "") {
		d.cardInputsChanged()
	}
}

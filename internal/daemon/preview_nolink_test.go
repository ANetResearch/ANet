//go:build no_preview

package daemon

import "testing"

// needPreviewModule skips a test that needs previews to be there: this
// binary was built with -tags no_preview.
func needPreviewModule(t *testing.T) {
	t.Helper()
	t.Skip("the preview module is not in this build (-tags no_preview)")
}

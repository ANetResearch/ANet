//go:build !no_preview

package daemon

// The daemon's tests run with the preview module in the binary, as the
// default build has it. Under -tags no_preview this file is not compiled
// and the module is absent (preview_nolink_test.go), which is the build the
// "absent" tests judge.
import (
	"testing"

	_ "github.com/ANetResearch/ANet/module/preview"
)

// needPreviewModule marks a test that needs previews to be there.
func needPreviewModule(*testing.T) {}

//go:build !no_p2p && windows

package p2p

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// On Windows a configured p2p module is refused with a sentence that names Windows, and a node without
// modules.p2p builds no p2p module and starts normally.
func TestWindowsP2PRefused(t *testing.T) {
	_, err := module.BuildOne(name, []byte(`{"socket":"C:\\anet\\peer.sock"}`))
	if err == nil || !strings.Contains(err.Error(), "not available on Windows") {
		t.Fatalf("BuildOne: %v", err)
	}
	m, err := module.BuildOne(name, nil)
	if err != nil || m != nil {
		t.Fatalf("an unconfigured p2p module: %v, %v", m, err)
	}
}

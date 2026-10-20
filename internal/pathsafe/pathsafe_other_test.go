//go:build !windows

package pathsafe

import "testing"

// Outside Windows a pulled file keeps the name SafeName gives it.
func TestExplorerInertNameIsUnchangedOutsideWindows(t *testing.T) {
	for _, n := range []string{"invoice.lnk", "desktop.ini", "x.url", "report.md"} {
		if got := ExplorerInertName(n); got != n {
			t.Errorf("ExplorerInertName(%q) = %q", n, got)
		}
	}
}

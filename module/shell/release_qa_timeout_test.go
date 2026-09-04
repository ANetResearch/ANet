//go:build shell

package shell

import (
	"testing"
	"time"

	"github.com/ANetResearch/ANet/provider"
)

// The bound the operator configures has to be the bound that applies.
// The daemon's own constant used to win silently: the module validated
// timeout_s up to its ceiling, an operator set twenty minutes, and the
// command was killed at sixty seconds.
func TestTheModuleTellsTheDaemonHowLongACommandMayTake(t *testing.T) {
	m := &Module{cfg: Config{
		Commands: map[string]Command{
			"quick": {Run: "true"},
			"build": {Run: "make", TimeoutS: 3600},
		},
		TimeoutS: 45,
	}}
	p := &shellProvider{m: m}

	var _ provider.LongRunning = p // the daemon looks for exactly this

	for _, tc := range []struct {
		cap      string
		want     time.Duration
		declared bool
	}{
		// Per-command wins.
		{runPrefix + "build", time.Hour, true},
		// Falls back to the module-wide setting.
		{runPrefix + "quick", 45 * time.Second, true},
		// A table lookup takes the daemon's default; it does not run
		// anything of the operator's.
		{CapList, 0, false},
		// An unknown command declares nothing — it will be refused
		// before it ever runs.
		{runPrefix + "nosuch", 0, false},
	} {
		got, declared := p.InvokeTimeout(tc.cap)
		if declared != tc.declared {
			t.Errorf("%s: declared = %v, want %v", tc.cap, declared, tc.declared)
		}
		if declared && got != tc.want {
			t.Errorf("%s: timeout = %s, want %s", tc.cap, got, tc.want)
		}
	}
}

// An hour has to be a legal setting. The ceiling was thirty minutes,
// which is under the work people actually put behind this — a build, an
// image flash, a model conversion on a board — so configuring a real
// timeout meant discovering the configuration was rejected.
func TestAnHourIsAConfigurableTimeout(t *testing.T) {
	for _, tc := range []struct {
		secs int
		ok   bool
	}{
		{3600, true},     // the hour that started this
		{4 * 3600, true}, // and well past it
		{86400, true},    // the ceiling itself
		{86401, false},   // past the ceiling
		{-1, false},      // negative is not "no limit"
	} {
		err := Config{
			Commands: map[string]Command{"c": {Run: "true", TimeoutS: tc.secs}},
			TimeoutS: tc.secs,
		}.validate()
		if tc.ok && err != nil {
			t.Errorf("timeout_s=%d was refused: %v", tc.secs, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("timeout_s=%d was accepted", tc.secs)
		}
	}
}

package daemon

// datadir.go: the data directory the daemon refuses to start in (core
// CORE-V4, FS-S1). v0.3.0 migrates no data: a directory an earlier anet
// wrote is refused, with one sentence that says so and what to do — move it
// away and run anet up — and nothing in it is changed.

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// CheckDataDirForStart is the check the daemon makes of its data directory
// before it starts, and before it writes anything there: an interactions
// store an earlier anet wrote (user_version not 3) is refused with
// identity_v02. It reads without writing (interactions.CheckStore). The
// error is a *ConfigRefusal, or the error reading the store.
func CheckDataDirForStart(l Layout) error {
	switch err := interactions.CheckStore(l.InteractionsDir()); {
	case errors.Is(err, interactions.ErrOldStore):
		return oldDataDirRefusal()
	case err != nil:
		return fmt.Errorf("anet: %w", err)
	}
	return nil
}

// oldDataDirRefusal is the refusal of a data directory an earlier anet
// wrote (FS-S1; ux §1.4 E2).
func oldDataDirRefusal() *ConfigRefusal {
	return &ConfigRefusal{Refusals: []*a2ashape.Refusal{a2ashape.Refuse("identity_v02", nil)}}
}

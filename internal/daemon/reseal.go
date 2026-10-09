package daemon

// reseal.go is the reseal engine (engine §3): when an attempt seals a new
// version of a queued message before it transmits, and the sealing itself.

// resealNeeded is engine ENG-R2 with the suppressions of ENG-R3: whether
// the attempt at row seals a new version before it transmits. cur is this
// node's key state now (keyCursor).
//
// v030-todo(ENG-1a): T0 and T2.
func resealNeeded(row RowView, vers []VerView, now uint64, cur KeyCursor) bool {
	return row.CurVer == 0
}

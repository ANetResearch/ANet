package interactions

// SetPruneBatch sets the batch size of PruneTerminal for a test and returns
// the function that restores it.
func SetPruneBatch(n int) (restore func()) {
	old := pruneBatch
	pruneBatch = n
	return func() { pruneBatch = old }
}

//go:build !linux

package localpeer

// readTables: no socket table outside Linux; Verify uses the challenge instead.
func readTables(func(sockEntry) bool) ([]sockEntry, error) { return nil, ErrNoSocketTable }

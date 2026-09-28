//go:build linux

package localpeer

import (
	"errors"
	"io/fs"
	"os"
)

// socketTables are the kernel's TCP tables for this network namespace. A connection to 127.0.0.1 may
// be served by an IPv6 socket (a dual-stack listener), so both are read.
var socketTables = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// readTables returns the rows of both tables that match keep, or ErrNoSocketTable when neither can be
// opened (a /proc without the files, as some sandboxes have).
func readTables(keep func(sockEntry) bool) ([]sockEntry, error) {
	var out []sockEntry
	opened := 0
	for _, p := range socketTables {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, err
		}
		opened++
		ents, err := scanTable(f, keep)
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, ents...)
	}
	if opened == 0 {
		return nil, ErrNoSocketTable
	}
	return out, nil
}

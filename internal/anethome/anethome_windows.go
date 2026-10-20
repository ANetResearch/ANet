//go:build windows

package anethome

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// osValidName refuses the names Windows cannot use for a directory: device names, with or without
// an extension, and names ending in a dot or a space, which Windows drops.
func osValidName(name string) bool {
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.TrimRight(stem, " ")
	switch strings.ToLower(stem) {
	case "con", "prn", "aux", "nul",
		"com0", "com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt0", "lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9",
		"com¹", "com²", "com³", "lpt¹", "lpt²", "lpt³":
		return false
	}
	return true
}

// nameTaken: an entry whose name equals name without regard to case.
func nameTaken(idsDir, name string) bool {
	ents, err := os.ReadDir(idsDir)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	for _, e := range ents {
		if strings.EqualFold(e.Name(), name) {
			return true
		}
	}
	return false
}

// listedAlready reports a listed identity whose name differs from name at most in case.
func listedAlready(ids []Identity, name string) bool {
	for _, id := range ids {
		if strings.EqualFold(id.Name, name) {
			return true
		}
	}
	return false
}

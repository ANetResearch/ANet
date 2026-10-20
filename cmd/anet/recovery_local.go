package main

// recovery_local.go is the recovery file that has no passphrase yet (KEL C-36): the recovery
// seed of an identity that joined a hub without a terminal, sealed under a random key kept beside
// it in the data directory. Only this CLI writes, reads and deletes the two files; the daemon never
// opens them (KEL D-2, SI-38).
//
//	<data dir>/recovery_local.key        0600, 32 random bytes
//	<data dir>/recovery_local.anetrec    0600, the recovery file, kdf "local"
//
// The files are written key first, then the recovery file, each by temporary file, fsync and rename,
// and the directory is synced last. They are deleted recovery file first, then key.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

const (
	localKeyName = "recovery_local.key"
	localRecName = "recovery_local.anetrec"
)

func localKeyPath(dataDir string) string { return filepath.Join(dataDir, localKeyName) }
func localRecPath(dataDir string) string { return filepath.Join(dataDir, localRecName) }

// writeFileDurable writes data to path through a temporary file in the same directory: fsync, then
// rename. The file is mode perm; a directory that is missing is made 0700. A temporary file for a perm
// that grants nothing to group or other is private from the moment it exists (fsperm.CreateTempPrivate;
// on unix the same as os.CreateTemp).
func writeFileDurable(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := mkdirDurable(dir); err != nil {
		return err
	}
	createTemp := os.CreateTemp
	if perm&0o077 == 0 {
		createTemp = fsperm.CreateTempPrivate
	}
	f, err := createTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = protectDurableTemp(tmp, perm)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = renameDurable(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// syncDir makes the entries of dir durable; a platform that cannot sync a directory is not an error.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// writeLocalRecovery seals rs under a new local key and writes both files (C-36). The recovery file
// carries the unprotected note in the language lines the caller gives.
func writeLocalRecovery(dataDir string, rs [32]byte, aid, hub string, commits identity.Commit, note ...string) error {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	defer clear(key[:])
	file, err := identity.SealRecoveryFileLocal(rs, aid, hub, commits, key)
	if err != nil {
		return err
	}
	if len(note) > 0 {
		if file, err = identity.WithRecoveryNote(file, note...); err != nil {
			return err
		}
	}
	if err := writeFileDurable(localKeyPath(dataDir), key[:], 0o600); err != nil {
		return err
	}
	if err := writeFileDurable(localRecPath(dataDir), file, 0o600); err != nil {
		return err
	}
	syncDir(dataDir)
	return nil
}

// errLocalRecoveryMissing is a data directory without a readable pair of local recovery files, or
// one whose key does not open its file.
var errLocalRecoveryMissing = errors.New("anet: the local recovery file is missing or does not open")

// readLocalRecovery opens recovery_local.anetrec with recovery_local.key.
func readLocalRecovery(dataDir string) (rs [32]byte, aid, hub string, commits identity.Commit, err error) {
	kb, kerr := os.ReadFile(localKeyPath(dataDir))
	fb, ferr := os.ReadFile(localRecPath(dataDir))
	if kerr != nil || ferr != nil || len(kb) != 32 {
		return rs, "", "", commits, errLocalRecoveryMissing
	}
	var key [32]byte
	copy(key[:], kb)
	defer clear(key[:])
	clear(kb)
	rs, aid, hub, commits, err = identity.OpenRecoveryFileLocal(fb, key)
	if err != nil {
		return [32]byte{}, "", "", commits, fmt.Errorf("%w: %v", errLocalRecoveryMissing, err)
	}
	return rs, aid, hub, commits, nil
}

// localRecoveryPresent reports whether either of the two files is there.
func localRecoveryPresent(dataDir string) bool {
	for _, p := range []string{localRecPath(dataDir), localKeyPath(dataDir)} {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

// removeLocalRecovery deletes the pair, recovery file first (C-36). A file that is not there is fine.
func removeLocalRecovery(dataDir string) error {
	for _, p := range []string{localRecPath(dataDir), localKeyPath(dataDir)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	syncDir(dataDir)
	return nil
}

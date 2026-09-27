package release

// installed.go is the record an installation leaves beside the binary, and
// the check `anet doctor` makes of it (A2A-DESIGN §13.1 "版本与签名").
//
// install.sh and `anet update` both verify release.json before they
// install anything, and then keep a copy of it and of its signature next
// to the binary they installed:
//
//	<binary>.release.json        the manifest, byte for byte
//	<binary>.release.json.sig    its SSHSIG signature
//
// Next to the binary, not in a data directory: one binary serves every
// identity on the machine, and a data directory says nothing about which
// file was installed. Named after the binary rather than a bare
// release.json because the install directory is a bin directory
// (~/.local/bin, /usr/local/bin) shared with other programs.
//
// The copy proves nothing by being there. CheckInstalled verifies it again
// with the key compiled into the running binary, and then asks whether the
// running binary is one the manifest names, by its sha256. A binary built
// from source, or replaced since, has no record or one that does not name
// it, and is reported as such rather than as verified.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Installed-record file suffixes, appended to the binary's path.
const (
	InstalledManifestSuffix = ".release.json"
	InstalledSigSuffix      = ".release.json.sig"
)

// Statuses of CheckInstalled.
const (
	// InstalledVerified: the record's signature verifies with the trusted
	// key, and the running binary's sha256 is one the manifest names.
	InstalledVerified = "verified"
	// InstalledUnverified: there is a record and it does not vouch for
	// this binary: a bad or missing signature, a key that is not trusted,
	// or a binary the manifest does not name.
	InstalledUnverified = "unverified"
	// InstalledUnknown: there is no record to check (a source build, or an
	// installer that kept none), or the binary could not be read.
	InstalledUnknown = "unknown"
)

// InstalledCheck is what CheckInstalled found.
type InstalledCheck struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Record is the manifest copy that was read, when there was one.
	Record string `json:"record,omitempty"`
	// Version, Asset and Signer are set when the record verified.
	Version string `json:"version,omitempty"`
	Asset   string `json:"asset,omitempty"`
	Signer  string `json:"signer,omitempty"`
	// Expired is set when the manifest has since expired. The binary was
	// checked when it was installed; an expired manifest only means a
	// newer release should have replaced it (anet update --check).
	Expired bool `json:"expired,omitempty"`
}

// InstalledPaths are the record's two files for the binary at exe.
func InstalledPaths(exe string) (manifest, sig string) {
	return exe + InstalledManifestSuffix, exe + InstalledSigSuffix
}

// SaveInstalled writes the record of an installation of exe: the manifest
// and signature it was verified against. Each file is written beside exe
// and renamed into place, so a reader sees an old record or a new one,
// never half of one.
func SaveInstalled(exe string, manifest, sig []byte) error {
	if len(manifest) == 0 || len(sig) == 0 {
		return errors.New("release: no manifest or signature to record")
	}
	mp, sp := InstalledPaths(exe)
	// The signature first: a crash between the two leaves the old manifest
	// with a new signature, which fails verification — reported, not
	// believed — rather than a new manifest with an old signature.
	for _, f := range []struct {
		path string
		body []byte
	}{{sp, sig}, {mp, manifest}} {
		if err := writeReplace(f.path, f.body); err != nil {
			return err
		}
	}
	return nil
}

func writeReplace(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".anet-release-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // after a successful rename there is nothing to remove
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// CheckInstalled checks the record beside exe against trust and against
// exe itself (see the file comment). It reads; it writes nothing.
func CheckInstalled(exe string, trust Trust, now time.Time) InstalledCheck {
	mp, sp := InstalledPaths(exe)
	raw, err := readLimited(mp, MaxManifestBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return InstalledCheck{Status: InstalledUnknown,
			Detail: "no " + mp + ": this binary was not installed by install.sh or `anet update` " +
				"(a source build, or an older installer); `anet update` installs a checked one and records it"}
	}
	if err != nil {
		return InstalledCheck{Status: InstalledUnknown, Detail: err.Error()}
	}
	out := InstalledCheck{Status: InstalledUnverified, Record: mp}
	sig, err := readLimited(sp, MaxSigBytes)
	if err != nil {
		out.Detail = "the release record has no readable signature (" + sp + "): " + err.Error()
		return out
	}
	m, s, err := VerifyManifest(raw, sig, trust)
	if err != nil {
		out.Detail = mp + ": " + err.Error()
		return out
	}
	sum, err := fileSHA256(exe)
	if err != nil {
		return InstalledCheck{Status: InstalledUnknown, Record: mp,
			Detail: "cannot read this binary to compare it with the release record: " + err.Error()}
	}
	name, ok := assetBySHA256(m, sum)
	if !ok {
		out.Detail = fmt.Sprintf("this binary (sha256 %s) is not one the signed manifest of anet %s names: "+
			"it was replaced or rebuilt after that release was installed", sum[:16], m.Version)
		return out
	}
	out.Status, out.Version, out.Asset, out.Signer = InstalledVerified, m.Version, name, Fingerprint(s.PublicKey)
	out.Detail = fmt.Sprintf("anet %s, asset %s: sha256 matches the manifest signed by the release key %s",
		m.Version, name, out.Signer)
	if m.CheckFresh(now) != nil {
		out.Expired = true
		out.Detail += fmt.Sprintf(" (the manifest expired at %s; `anet update --check` looks for a newer release)",
			m.ExpiresAt)
	}
	return out
}

// assetBySHA256 is the asset whose binary has sha256 sum.
func assetBySHA256(m *Manifest, sum string) (string, bool) {
	names := make([]string, 0, len(m.Assets))
	for n := range m.Assets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if m.Assets[n].SHA256 == sum {
			return n, true
		}
	}
	return "", false
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, limit)
	}
	return b, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

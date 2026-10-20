package daemon

// datadir.go: the data directory the daemon refuses to start in (core
// CORE-V4, FS-S1). v0.3.0 migrates no data: a directory an earlier anet
// wrote is refused, with one sentence that says so and what to do — move it
// away and run anet up — and nothing in it is changed. A directory a newer
// anet wrote is refused too, with another sentence: upgrade, and do not
// delete it, since what is in it is still good.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANetCore/coredet"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// dataAge is what a part of the data directory says about the anet that
// wrote it.
type dataAge int

const (
	dataThisVersion dataAge = iota // this version's, absent, or damaged (the start says what is wrong)
	dataOlder                      // an earlier anet's: identity_v02
	dataNewer                      // a newer anet's: datadir_too_new
	dataV1                         // AgentNetwork 1.x's: identity_v02 as well
)

// CheckDataDirForStart is the check the daemon makes of its data directory
// before it starts, and before it writes anything there: the identity file
// (its type tag), the interactions store (user_version 3) and the key ring
// (enc_keys.cbor v 2). A part an earlier anet wrote is refused with
// identity_v02; a part a newer anet wrote with datadir_too_new, which wins
// when the parts disagree, since moving away a directory that newer data is
// in would lose it. It reads without writing (interactions.CheckStore). The
// error is a *ConfigRefusal, or the error reading a part.
func CheckDataDirForStart(l Layout) error {
	var ages []dataAge
	switch err := checkIdentityFileForStart(l); {
	case err == nil:
	case isRefusal(err, "identity_v02"):
		ages = append(ages, dataOlder)
	case isRefusal(err, "datadir_too_new"):
		ages = append(ages, dataNewer)
	default:
		return err
	}
	switch err := interactions.CheckStore(l.InteractionsDir()); {
	case errors.Is(err, interactions.ErrOldStore):
		ages = append(ages, dataOlder)
	case errors.Is(err, interactions.ErrNewerStore):
		ages = append(ages, dataNewer)
	case err != nil:
		return fmt.Errorf("anet: %w", err)
	}
	age, err := keyRingFileAge(l)
	if err != nil {
		return err
	}
	ages = append(ages, age)
	if ClassifyDataDir(l.Root) == DataDirV1 {
		ages = append(ages, dataV1)
	}
	return dataDirRefusal(ages...)
}

// dataDirRefusal is the refusal for the ages of the parts of a data
// directory: datadir_too_new when one is newer, identity_v02 when one is
// older or the directory is AgentNetwork 1.x's, nil when all are this
// version's. A 1.x directory has no code of its own: the code would have to
// be registered in the wire registry of ANetCore, and its next step, to
// move the directory away and run anet up, is identity_v02's.
func dataDirRefusal(ages ...dataAge) error {
	older := false
	for _, a := range ages {
		switch a {
		case dataNewer:
			return newerDataDirRefusal()
		case dataOlder, dataV1:
			older = true
		}
	}
	if older {
		return oldDataDirRefusal()
	}
	return nil
}

// isRefusal reports whether err is a *ConfigRefusal of exactly code.
func isRefusal(err error, code string) bool {
	var cr *ConfigRefusal
	return errors.As(err, &cr) && len(cr.Refusals) == 1 && cr.Refusals[0].Code() == code
}

// keyRingFileAge is the key ring's part of CheckDataDirForStart: its file
// version, key 1 of enc_keys.cbor (FS-S1). Below encKeyFileVersion (0.2
// wrote 1, and a file without the key is older still) it is an earlier
// anet's; above it, a newer one's. A missing ring is this version's (the
// start makes one), and so is a file that is not a CBOR map with an
// unsigned key 1: damaged, which the start reports as such.
func keyRingFileAge(l Layout) (dataAge, error) {
	b, err := os.ReadFile(l.EncKeysPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return dataThisVersion, nil
	case err != nil:
		return 0, fmt.Errorf("anet: read the encryption key ring: %w", err)
	}
	v, ok := keyRingFileVersion(b)
	switch {
	case !ok:
		return dataThisVersion, nil
	case v < encKeyFileVersion:
		return dataOlder, nil
	case v > encKeyFileVersion:
		return dataNewer, nil
	}
	return dataThisVersion, nil
}

// keyRingFileVersion reads v, key 1, of an enc_keys.cbor: 0 when the map
// has no key 1. ok is false when b is not a CBOR map with integer keys, or
// its key 1 is not an unsigned integer.
func keyRingFileVersion(b []byte) (v uint64, ok bool) {
	var m map[uint64]any
	if err := coredet.Unmarshal(b, &m); err != nil {
		return 0, false
	}
	x, present := m[1]
	if !present {
		return 0, true
	}
	v, ok = x.(uint64)
	return v, ok
}

// oldDataDirRefusal is the refusal of a data directory an earlier anet
// wrote, or AgentNetwork 1.x (FS-S1; ux §1.4 E2).
func oldDataDirRefusal() *ConfigRefusal {
	return &ConfigRefusal{Refusals: []*a2ashape.Refusal{a2ashape.Refuse("identity_v02", nil)}}
}

// newerDataDirRefusal is the refusal of a data directory a newer anet wrote
// (core CORE-V4; ux §1.4 E2): upgrade, and keep the directory.
func newerDataDirRefusal() *ConfigRefusal {
	return &ConfigRefusal{Refusals: []*a2ashape.Refusal{a2ashape.Refuse("datadir_too_new", nil)}}
}

// invalidIdentityFileRefusal is the refusal of an identity file that is neither 0.2's nor a newer
// anet's but cannot be read (ux §1.4 E2, identity_file_invalid): the start and doctor say one
// sentence, with the next step, instead of the Go error text.
func invalidIdentityFileRefusal(path string) *ConfigRefusal {
	return &ConfigRefusal{Refusals: []*a2ashape.Refusal{a2ashape.Refuse("identity_file_invalid", a2ashape.Args{"path": path})}}
}

// DataDirKind is what a data directory holds, by the rule that the daemon
// and the installer share. deploy/release/install.sh implements the same
// rule in its function datadir_kind, and TestInstallShDataDirKindAgrees
// holds the two equal on the same directories. The installer moves a
// directory of kind older or v1 aside whole, moves only the AgentNetwork
// 1.x entries out of one of kind mixed, and leaves every other kind alone.
type DataDirKind string

const (
	// DataDirNone: the directory does not exist or is empty.
	DataDirNone DataDirKind = "none"
	// DataDirCurrent: an identity file of this version, or nothing that
	// another release wrote.
	DataDirCurrent DataDirKind = "current"
	// DataDirOlder: an identity file of anet 0.1 or 0.2 (identity_v02).
	DataDirOlder DataDirKind = "older"
	// DataDirNewer: an identity file a newer anet wrote (datadir_too_new).
	DataDirNewer DataDirKind = "newer"
	// DataDirInvalid: an identity file that is neither of the above and
	// whose type tag does not read.
	DataDirInvalid DataDirKind = "invalid"
	// DataDirV1: the files of AgentNetwork 1.x, the earlier project that
	// published the npm package @agentnetwork/anet 1.x, and no identity of
	// this version, neither at the top nor under ids/. Files that anet
	// init of 0.3 wrote beside them (config.json, the peer lists) do not
	// change the kind: they hold no identity. The daemon refuses to start
	// on it with identity_v02.
	DataDirV1 DataDirKind = "v1"
	// DataDirMixed: the files of AgentNetwork 1.x beside an identity of
	// this version, at the top or under ids/. The daemon starts and
	// ignores the 1.x files.
	DataDirMixed DataDirKind = "mixed"
)

// V1Markers are entries, relative to the data directory, that AgentNetwork
// 1.x writes when its daemon first starts and that no release of anet
// writes. The installer tests the same list (datadir_kind).
var V1Markers = []string{
	"api_token",
	"agent_token",
	"scoped_tokens.json",
	"peer-journal.json",
	filepath.Join("anet", "identity.json"),
	filepath.Join("anet", "api_token"),
}

// ClassifyDataDir is the kind of the data directory root. It only reads.
// The identity file decides first, by its type tag alone (as
// identityFileAge); without one, the 1.x markers do, and an identity file
// of this version under ids/ makes the directory mixed rather than 1.x's.
func ClassifyDataDir(root string) DataDirKind {
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		return DataDirNone
	}
	v1 := false
	for _, m := range V1Markers {
		if _, err := os.Lstat(filepath.Join(root, m)); err == nil {
			v1 = true
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "identity.kel")); err == nil {
		switch age := identityFileAge(b); {
		case age != DataDirCurrent:
			return age
		case v1:
			return DataDirMixed
		}
		return DataDirCurrent
	}
	if !v1 {
		return DataDirCurrent
	}
	named, _ := filepath.Glob(filepath.Join(root, "ids", "*", "identity.kel"))
	for _, p := range named {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return DataDirMixed
		}
	}
	return DataDirV1
}

// identityFileAge is the kind an identity file gives its directory, by its
// form alone: older for anet 0.2's untagged map and for a tag below this
// version's, newer for a tag above it, current for this version's tag, and
// invalid for anything else. Whether the rest of the file reads is the
// start's question (identity_file_invalid), not this one's.
func identityFileAge(b []byte) DataDirKind {
	switch {
	case identityFileIsV02(b), identityFileTagIsOlder(b):
		return DataDirOlder
	case identityFileIsNewer(b):
		return DataDirNewer
	}
	if n, ok := identityFileTagVersion(b); ok && n == identityFileVersion {
		return DataDirCurrent
	}
	return DataDirInvalid
}

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
	return dataDirRefusal(ages...)
}

// dataDirRefusal is the refusal for the ages of the parts of a data
// directory: datadir_too_new when one is newer, identity_v02 when one is
// older, nil when all are this version's.
func dataDirRefusal(ages ...dataAge) error {
	older := false
	for _, a := range ages {
		switch a {
		case dataNewer:
			return newerDataDirRefusal()
		case dataOlder:
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
// wrote (FS-S1; ux §1.4 E2).
func oldDataDirRefusal() *ConfigRefusal {
	return &ConfigRefusal{Refusals: []*a2ashape.Refusal{a2ashape.Refuse("identity_v02", nil)}}
}

// newerDataDirRefusal is the refusal of a data directory a newer anet wrote
// (core CORE-V4; ux §1.4 E2): upgrade, and keep the directory.
func newerDataDirRefusal() *ConfigRefusal {
	return &ConfigRefusal{Refusals: []*a2ashape.Refusal{a2ashape.Refuse("datadir_too_new", nil)}}
}

package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
)

// LoadOrGenerateIdentity loads the daemon's persisted identity (identity.kel), or, on first run,
// incepts a fresh agent identity (toad 1, no witness until it joins a hub) and persists it (0600 —
// it holds private keys). The returned Controller is the node's single AID/KEL for its lifetime
// (one runtime = one agent = one AID).
//
// An identity file an earlier anet wrote is refused with identity_v02 (KEL S-2): 0.3.0 neither
// reads nor converts it, and it does not incept a new identity over it.
func LoadOrGenerateIdentity(l Layout) (*identity.Controller, error) {
	b, err := os.ReadFile(l.IdentityPath())
	if err == nil {
		if identityFileIsV02(b) {
			return nil, oldDataDirRefusal()
		}
		if identityFileTagIsOlder(b) {
			return nil, oldDataDirRefusal()
		}
		if identityFileIsNewer(b) {
			return nil, newerDataDirRefusal()
		}
		c, rerr := identity.Restore(b)
		if rerr != nil {
			return nil, fmt.Errorf("anet: restore identity %s: %w", l.IdentityPath(), rerr)
		}
		return c, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("anet: read identity: %w", err)
	}
	c, err := identity.InceptAgent(uint64(time.Now().UnixMilli()))
	if err != nil {
		return nil, err
	}
	if err := saveIdentity(l, c); err != nil {
		return nil, err
	}
	return c, nil
}

// identityFileIsV02 reports whether b is an identity file of anet 0.2 or earlier (KEL S-2): a CBOR
// map without key 0, the type tag every identity file v2 starts with. Those wrote {1: the current
// key's seed, 2: the next key's seed, 3: the KEL}. A file that is not a CBOR map at all is damaged,
// not old, and is not called 0.2's: moving it away is not the advice for it.
func identityFileIsV02(b []byte) bool {
	var m map[uint64]any
	if err := coredet.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return false
	}
	_, tagged := m[0]
	return !tagged
}

// identityFileIsNewer reports whether b is an identity file a newer anet wrote: its type tag (key 0)
// is "anet.identity/<n>" with n above this version's (core CORE-V4). Such a file is not 0.2's: the
// advice for it is to upgrade, and moving the data directory away would lose an identity that is
// still good. An older tag than this version's is 0.2's (identityFileTagIsOlder).
func identityFileIsNewer(b []byte) bool {
	n, ok := identityFileTagVersion(b)
	return ok && n > identityFileVersion
}

// identityFileTagIsOlder reports whether b is a tagged identity file of a version below this one's.
// No anet released one; it is refused like 0.2's untagged file, which it would be older than.
func identityFileTagIsOlder(b []byte) bool {
	n, ok := identityFileTagVersion(b)
	return ok && n < identityFileVersion
}

// identityFileVersion is n of this version's type tag, identity.IdentityFileType.
const identityFileVersion = 2

// identityFileTagVersion reads n from the type tag "anet.identity/<n>" (key 0) of b. ok is false
// when b is not a CBOR map, or its key 0 is not a tag of that form: a damaged file, which the start
// reports as damaged.
func identityFileTagVersion(b []byte) (n uint64, ok bool) {
	var m map[uint64]any
	if err := coredet.Unmarshal(b, &m); err != nil {
		return 0, false
	}
	tag, isStr := m[0].(string)
	digits, found := strings.CutPrefix(tag, identityFileTagPrefix)
	if !isStr || !found || digits == "" || len(digits) > 9 || strings.TrimLeft(digits, "0123456789") != "" ||
		(len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 32)
	return n, err == nil
}

// identityFileTagPrefix is identity.IdentityFileType without its version ("anet.identity/").
var identityFileTagPrefix = strings.TrimRight(identity.IdentityFileType, "0123456789")

// checkIdentityFileForStart is the identity file's part of CheckDataDirForStart: an identity file
// of anet 0.2 or earlier is refused with identity_v02, one a newer anet wrote with
// datadir_too_new, before anything is written. It only reads. A missing file passes (the start
// incepts one); a damaged one is left to the start, which says what is wrong with it.
func checkIdentityFileForStart(l Layout) error {
	b, err := os.ReadFile(l.IdentityPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("anet: read identity: %w", err)
	case identityFileIsV02(b), identityFileTagIsOlder(b):
		return oldDataDirRefusal()
	case identityFileIsNewer(b):
		return newerDataDirRefusal()
	}
	return nil
}

// identityWriteMu serializes the writes of identity.kel. Each write exports the controller and
// renames the result into place while holding it, so when two writes overlap the file ends with the
// later state: a write that exported first can no longer be renamed last and take back what the
// other just made durable, such as a pending key event (KEL D-13 step 2, S-6).
var identityWriteMu sync.Mutex

// saveIdentity persists a Controller (private-key seeds + KEL) to identity.kel, 0600.
func saveIdentity(l Layout, c *identity.Controller) error {
	identityWriteMu.Lock()
	defer identityWriteMu.Unlock()
	if err := l.EnsureRoot(); err != nil {
		return err
	}
	blob, err := c.Export()
	if err != nil {
		return err
	}
	return writeFileAtomic(l.IdentityPath(), blob, 0o600)
}

// saveLocalCommit records local_commit, identity file key 7 (KEL §2.9, D-11): the next-key digest
// the recovery seed kept without a passphrase in the data directory commits to. It is public: the
// daemon learns it from /hub-register, never from the recovery file, which it does not open (KEL
// D-2). Whether the identity is still unprotected is read from it and the KEL (recoveryStateOf);
// it is never removed.
func (d *Daemon) saveLocalCommit(digest []byte) error {
	if err := d.self.SetLocalCommit(digest); err != nil {
		return err
	}
	return saveIdentity(d.layout, d.self)
}

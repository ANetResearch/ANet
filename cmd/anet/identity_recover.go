package main

// identity_recover.go is `anet identity recover <file>` (KEL L-5, D-45): the identity comes back on
// this machine from its recovery file. The work is done offline, in a new data directory, and the
// daemon that starts there publishes the first rotation.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/hubapi"
)

// hubGetTimeout bounds one read of the hub during a recovery.
const hubGetTimeout = 30 * time.Second

// maxRecoverBody bounds what a hub's key history may be when read (a KEL is a few kilobytes).
const maxRecoverBody = 4 << 20

// idHTTP is the client `anet identity recover` reads the hub with; tests replace it.
var idHTTP = &http.Client{Timeout: hubGetTimeout}

func hubGet(url string, out any) (int, error) {
	resp, err := idHTTP.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRecoverBody+1))
	if err != nil || len(b) > maxRecoverBody {
		return resp.StatusCode, errors.New("the answer is unreadable or too large")
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(b, out)
}

// recover is `anet identity recover <file> [--hub URL] [--name N]` (KEL L-5).
func (x *identityCLI) recover() error {
	const use = "anet identity recover <recovery-file> [--hub URL] [--name N]"
	if len(x.pos) != 1 {
		return usageError{use}
	}
	file := x.pos[0]
	b, err := os.ReadFile(file)
	if err != nil {
		return x.refuse("recovery_file_missing", a2ashape.Args{"path": file})
	}
	info, err := identity.ParseRecoveryFile(b)
	switch {
	case err != nil:
		return x.refuse("recovery_wrong_identity", a2ashape.Args{"aid": "unknown", "file_aid": "unknown"})
	case info.Unprotected:
		return x.refuse("recovery_file_unprotected", nil)
	}
	hub := strings.TrimRight(strings.TrimSpace(x.flags["hub"]), "/")
	if hub == "" || hub == "true" {
		hub = strings.TrimRight(info.Hub, "/")
	}
	if hub == "" {
		return usageError{use + "  (the file has no hub hint: add --hub URL)"}
	}
	name := x.flags["name"]
	if name == "" || name == "true" {
		name = "recovered-" + a2ashape.Short8(info.AID)[len("…"):]
	}
	if !daemon.ValidIdentityName(name) || name == "default" {
		return usageError{fmt.Sprintf("%s  (%q is not a name: use letters, digits and ._-)", use, printable(name, 64))}
	}
	layout := daemon.IdentityLayout(name)
	if layout.Initialized() {
		return x.refuse("recover_target_exists", a2ashape.Args{"name": name})
	}
	t, err := x.openTerminal("recovery_passphrase")
	if err != nil {
		return err
	}
	defer t.Close()
	fmt.Fprintln(x.out, x.sentence("identity.recover.start", a2ashape.Args{"aid": info.AID, "hub": hub}))
	pass, err := readPassphrase(t, x.sentence("identity.prompt.passphrase", nil))
	if err != nil {
		return x.refuse("tty_required", a2ashape.Args{"action": "recovery_passphrase", "command": typedCommand(commandLine())})
	}
	defer clear(pass)
	if err := x.passphraseIsUTF8(pass); err != nil {
		return err
	}
	rs, aid, _, commits, err := identity.OpenRecoveryFile(b, pass)
	defer clear(rs[:])
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrRecoveryPassphrase):
		return x.refuse("recovery_passphrase_wrong", nil)
	case errors.Is(err, identity.ErrPassphraseNotUTF8):
		return x.refuse("passphrase_not_utf8", nil)
	case errors.Is(err, identity.ErrRecoveryUnprotected):
		return x.refuse("recovery_file_unprotected", nil)
	default:
		return x.refuse("recovery_wrong_identity", a2ashape.Args{"aid": info.AID, "file_aid": info.AID})
	}
	// What the hub holds of this identity (KEL D-45 step 2).
	var hid hubapi.HubIdentity
	if code, err := hubGet(hub+"/hub/identity", &hid); err != nil || code != http.StatusOK || hid.AID == "" {
		return x.refuse("hub_unreachable", a2ashape.Args{"hub": hub})
	}
	var kv struct {
		KEL string `json:"kel"`
	}
	if code, err := hubGet(hub+"/agents/"+aid+"/kel", &kv); err != nil || code != http.StatusOK {
		return x.refuse("hub_unreachable", a2ashape.Args{"hub": hub})
	}
	raw, err := hubapi.DecodeB64(kv.KEL, -1)
	if err != nil {
		return x.refuse("recover_not_witness", a2ashape.Args{"hub": hub})
	}
	kel, err := identity.UnmarshalKEL(raw)
	if err != nil {
		return x.refuse("recover_not_witness", a2ashape.Args{"hub": hub})
	}
	states, err := identity.Replay(kel)
	if err != nil || len(states) == 0 {
		return x.refuse("recover_not_witness", a2ashape.Args{"hub": hub})
	}
	tip := states[len(states)-1]
	if tip.AID != aid || tip.Status != "active" || tip.Witness != hid.AID {
		return x.refuse("recover_not_witness", a2ashape.Args{"hub": hub})
	}
	j := uint64(len(kel))
	// The file says how long the history was when it was written (its commitment is for key
	// commits.Seq), and every rotation writes it again. A hub that serves fewer events than that
	// is behind the file; one that serves more is ahead of it, or the file is an older copy. In
	// both cases the key this seed derives for position j may be one the real history has used
	// already, and a rotation signed with it would fork the identity for good (T-1 RT-F8).
	switch {
	case j < commits.Seq:
		return x.refuse("recover_not_witness", a2ashape.Args{"hub": hub})
	case j > commits.Seq:
		return x.refuse("recovery_mismatch", nil)
	}
	commit := lastCommitment(kel)
	seed, err := seedAt(rs, aid, j, commit)
	if errors.Is(err, errMismatch) {
		return x.refuse("recovery_mismatch", nil)
	}
	if err != nil {
		return err
	}
	defer clear(seed)
	// The new recovery seed (KEL D-45 step 4): a new passphrase, and a file for it written in two
	// phases, so that a failure leaves the old file.
	fmt.Fprintln(x.out, x.sentence("identity.recover.new_passphrase", nil))
	newPass, err := x.askNewPassphrase(t)
	if err != nil {
		return err
	}
	defer clear(newPass)
	rsNew, err := newSeed()
	if err != nil {
		return err
	}
	defer clear(rsNew[:])
	next, err := identity.NextDigest(rsNew, aid, j+1)
	if err != nil {
		return err
	}
	newFile, err := sealRecovery(rsNew, aid, hub, identity.Commit{Seq: j + 1, Digest: next}, newPass)
	if err != nil {
		return err
	}
	path, err := x.recoveryPath(aid)
	if err != nil {
		return err
	}
	// A new data directory with the identity file (KEL D-45 step 5), then the new recovery file.
	if err := layout.EnsureRoot(); err != nil {
		return err
	}
	idf := identity.IdentityFile{T: identity.IdentityFileType, KEL: raw, State: identity.StateOK,
		Pending: &identity.Pending{Kind: identity.PendingRot, Revealed: seed, Next: next[:], HubURL: hub, Emergency: true,
			Since: uint64(time.Now().UnixMilli())}}
	blob, err := idf.Marshal()
	if err != nil {
		return err
	}
	if err := writeFileDurable(layout.IdentityPath(), blob, 0o600); err != nil {
		_ = os.RemoveAll(layout.Root)
		return err
	}
	cfg, _, err := daemon.EnsureLayoutInit(layout)
	if err == nil {
		cfg.HubURL = hub
		err = daemon.SaveConfig(layout, cfg)
	}
	if err != nil {
		_ = os.RemoveAll(layout.Root)
		return err
	}
	if err := writeFileDurable(path+".new", newFile, 0o600); err != nil {
		_ = os.RemoveAll(layout.Root)
		return err
	}
	if err := os.Rename(path+".new", path); err != nil {
		_ = os.RemoveAll(layout.Root)
		return err
	}
	syncDir(parentDir(path))
	// Start the daemon; it publishes the pending rotation and the state goes from publishing to ok.
	if err := idStartDaemon(layout); err != nil {
		return err
	}
	if !x.waitRecovered(layout) {
		x.tell("rotation_pending", nil)
		return nil
	}
	x.tell("identity.recover.done", a2ashape.Args{"name": name, "path": layout.Root, "file": path})
	x.tell("identity.recover.old_stopped", nil)
	return nil
}

// passphraseIsUTF8 refuses a passphrase that is not UTF-8 text (KEL C-34).
func (x *identityCLI) passphraseIsUTF8(p []byte) error {
	if errors.Is(identity.CheckPassphrase(append([]byte(nil), p...)), identity.ErrPassphraseNotUTF8) {
		return x.refuse("passphrase_not_utf8", nil)
	}
	return nil
}

// lastCommitment is key 6 of the last establishment event: the digest the next key must have.
func lastCommitment(kel []identity.SignedEvent) []byte {
	for i := len(kel) - 1; i >= 0; i-- {
		if t := kel[i].Event.Type; t == identity.Inception || t == identity.Rotation {
			return kel[i].Event.NextDigest
		}
	}
	return nil
}

// waitRecovered waits for the daemon of the new data directory to be ok again (KEL L-5).
func (x *identityCLI) waitRecovered(layout daemon.Layout) bool {
	deadline := time.Now().Add(idSettle)
	for {
		if base, token, err := daemon.ResolveControlStrict(layout); err == nil {
			c := &client{base: base, token: token, timeout: 10 * time.Second, dataDir: layout.Root}
			if b, code, err := c.fetch("/identity/status", map[string]any{}); err == nil && code == 200 {
				var st daemon.IdentityStatus
				if json.Unmarshal(b, &st) == nil && st.Pending == nil && (st.State == "ok" || st.State == "notice_stale") {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(idPoll)
	}
}

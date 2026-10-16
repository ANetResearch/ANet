package main

// identity.go is `anet identity` (KEL §4.5, L-1–L-10): status, protect, rotate, recover,
// deactivate, and the `rm` of `anet id`. It holds the part of the work that has the user's
// secrets in it: the recovery passphrase is asked for on the terminal and used here, the recovery
// seed is opened here, and the daemon is sent only the key the seed derives (KEL P-1, P-2, D-2).
// The daemon decides and words every refusal; this file words what only the terminal knows.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// Timing of the waits; tests shorten them.
var (
	// idPoll is how often the CLI asks /identity/status while it waits.
	idPoll = time.Second
	// idSettle is how long the CLI waits for the witness to take a key event (KEL L-3, L-4, L-5).
	idSettle = 60 * time.Second
	// idOldHub is how long a hub switch waits for the old witness (KEL L-4).
	idOldHub = 10 * time.Second
)

// Where `anet identity` writes and what it asks of its environment; tests replace them.
var (
	idStdout io.Writer = os.Stdout
	idStderr io.Writer = os.Stderr
	idHome             = os.UserHomeDir
	// idStartDaemon starts the daemon of a data directory detached (anet identity recover).
	idStartDaemon = runDaemonDetached
)

const identityUsage = "anet identity status|protect|rotate|recover|deactivate|rm|ls|new|use"

// identityCLI is one run of an `anet identity` command.
type identityCLI struct {
	layout   daemon.Layout
	explicit bool
	pos      []string
	flags    map[string]string
	lang     a2ashape.Language
	out, err io.Writer

	// c is the control client of a daemon that answered; nil while none did.
	c *client
}

func newIdentityCLI(layout daemon.Layout, explicit bool, rest []string) *identityCLI {
	pos, flags := splitFlags(rest)
	return &identityCLI{layout: layout, explicit: explicit, pos: pos, flags: flags, lang: a2ashape.Lang(),
		out: idStdout, err: idStderr}
}

// runIdentity is `anet identity <sub>` for the subcommands that act on one identity's keys; the rest
// of the family (ls, new, use, rm) is runID's.
func runIdentity(layout daemon.Layout, explicit bool, sub string, rest []string) error {
	x := newIdentityCLI(layout, explicit, rest)
	switch sub {
	case "status":
		return x.statusCommand()
	case "protect":
		return x.protect()
	case "rotate":
		return x.rotate()
	case "recover":
		return x.recover()
	case "deactivate":
		return x.deactivate()
	}
	return usageError{identityUsage}
}

// identitySubcommands are the subcommands runIdentity takes.
var identitySubcommands = map[string]bool{"status": true, "protect": true, "rotate": true, "recover": true, "deactivate": true}

// --- words ---

func (x *identityCLI) sentence(key string, args a2ashape.Args) string {
	return a2ashape.Sentence(key, x.lang, args)
}

// tell prints a sentence on stdout, and its command on the next line when it has one.
func (x *identityCLI) tell(key string, args a2ashape.Args) {
	say(x.out, x.sentence(key, args), a2ashape.Next(key, x.lang, args))
}

// refuse prints a refusal on stderr and ends the command with exit status 1.
func (x *identityCLI) refuse(code string, args a2ashape.Args) error {
	say(x.err, x.sentence(code, args), a2ashape.Next(code, x.lang, args))
	return errExit1
}

// idArgs are the placeholders every sentence about an identity takes ({aid8}, {name}), with extra.
func idArgs(st daemon.IdentityStatus, extra a2ashape.Args) a2ashape.Args {
	out := a2ashape.Args{"aid": st.AID, "name": st.Name}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func hubOf(st daemon.IdentityStatus) string {
	if st.Witness != nil {
		return st.Witness.URL
	}
	return ""
}

// --- the daemon, or the data directory ---

// control resolves the control client of this identity's daemon; nil when there is none to reach.
func (x *identityCLI) control() *client {
	if x.c != nil {
		return x.c
	}
	resolve := daemon.ResolveControl
	if x.explicit {
		resolve = daemon.ResolveControlStrict
	}
	base, token, err := resolve(x.layout)
	if err != nil {
		return nil
	}
	return &client{base: base, token: token, timeout: 3 * time.Minute, dataDir: x.layout.Root}
}

// statusNow asks /identity/status of the daemon, and reads the data directory when none answers. Offline,
// a lock that a daemon holds means it is there and busy: the commands that change files then say so
// (daemon_not_answering) instead of going on; busyOK lets the read-only ones go on.
func (x *identityCLI) statusNow(busyOK ...bool) (st daemon.IdentityStatus, err error) {
	if c := x.control(); c != nil {
		b, code, ferr := c.fetch("/identity/status", map[string]any{})
		if ferr == nil {
			if code != 200 {
				return st, defaultHubCLI().refusedBy(code, b)
			}
			if err := json.Unmarshal(b, &st); err != nil {
				return st, fmt.Errorf("identity: the daemon's answer does not read: %v", err)
			}
			x.c = c
			return st, nil
		}
	}
	if daemon.DataLockHeld(x.layout) && len(busyOK) == 0 {
		return st, x.refuse("daemon_not_answering", a2ashape.Args{"path": x.layout.Root, "dur": 3 * time.Second})
	}
	st, err = daemon.ReadIdentityStatus(x.layout, x.lang)
	var cr *daemon.ConfigRefusal
	switch {
	case errors.As(err, &cr):
		for _, s := range cr.Sentences(x.lang) {
			fmt.Fprintln(x.err, s)
		}
		return st, errExit1
	case errors.Is(err, daemon.ErrNoIdentity):
		return st, diagnoseNoDaemon("http://"+daemon.LocalControlAddr(x.layout), x.layout.Root, err)
	}
	return st, err
}

// needDaemon is the error of a command that changes the identity while no daemon answers.
func (x *identityCLI) needDaemon() error {
	return diagnoseNoDaemon("http://"+daemon.LocalControlAddr(x.layout), x.layout.Root, errors.New("no daemon"))
}

// post calls a control route and decodes a 200 answer into out; a refusal is said as the daemon
// words it.
func (x *identityCLI) post(path string, body, out any) error {
	b, code, err := x.c.fetch(path, body)
	if err != nil {
		return diagnoseNoDaemon(x.c.base, x.layout.Root, err)
	}
	if code != 200 {
		return defaultHubCLI().refusedBy(code, b)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("identity: the daemon's answer does not read: %v", err)
	}
	return nil
}

// postRaw is post without the printing, for a call whose refusal the caller words.
func (x *identityCLI) postRaw(path string, body any) ([]byte, int, error) {
	b, code, err := x.c.fetch(path, body)
	if err != nil {
		return nil, 0, diagnoseNoDaemon(x.c.base, x.layout.Root, err)
	}
	return b, code, nil
}

// --- the recovery files ---

// recoveryPath is where the recovery file of aid is: --recovery-file, else ~/.anet-recovery/<tail>.anetrec.
func (x *identityCLI) recoveryPath(aid string) (string, error) {
	if p := x.flags["recovery-file"]; p != "" && p != "true" {
		return p, nil
	}
	home, err := idHome()
	if err != nil || home == "" {
		return "", errors.New("the home directory is unknown, so there is no default place for the recovery file; add --recovery-file <path>")
	}
	return defaultRecoveryFile(home, aid), nil
}

// recoveryNote is the note lines a recovery file carries: the sentence of key in Chinese and English.
func recoveryNote(key string) []string {
	return []string{a2ashape.Sentence(key, a2ashape.Chinese, nil), a2ashape.Sentence(key, a2ashape.English, nil)}
}

// sealRecovery is the text of a passphrase-protected recovery file for rs with its note.
func sealRecovery(rs [32]byte, aid, hub string, commits identity.Commit, pass []byte) ([]byte, error) {
	f, err := identity.SealRecoveryFile(rs, aid, hub, commits, pass)
	if err != nil {
		return nil, err
	}
	return identity.WithRecoveryNote(f, recoveryNote("identity.recovery_file_note")...)
}

// settleLeftovers finishes what an interrupted command left (KEL L-3, L-10 step 2): a `.new` file
// whose commitment is the daemon's current one replaces the recovery file, any other is deleted; and
// the unprotected recovery files of a data directory that is protected now are deleted. Neither
// needs a passphrase.
func (x *identityCLI) settleLeftovers(st daemon.IdentityStatus) {
	var paths []string
	if p := x.flags["recovery-file"]; p != "" && p != "true" {
		paths = append(paths, p)
	}
	if home, err := idHome(); err == nil && home != "" {
		paths = append(paths, defaultRecoveryFile(home, st.AID))
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		np := p + ".new"
		b, err := os.ReadFile(np)
		if err != nil {
			continue
		}
		info, perr := identity.ParseRecoveryFile(b)
		if perr == nil && info.AID == st.AID && !info.Unprotected && st.Commitment != nil &&
			string(info.Commits.Digest[:]) == string(st.Commitment) {
			if os.Rename(np, p) == nil {
				syncDir(filepath.Dir(p))
				continue
			}
		}
		_ = os.Remove(np)
	}
	if st.Recovery.State == "protected" && localRecoveryPresent(x.layout.Root) {
		_ = removeLocalRecovery(x.layout.Root)
	}
}

// --- unlocking ---

// unlocked is the recovery seed of an identity opened for one command, with the passphrase of the
// file it came from (nil for the local key).
type unlocked struct {
	rs   [32]byte
	pass []byte
}

func (u *unlocked) clear() {
	clear(u.rs[:])
	clear(u.pass)
}

// checkPassphrase words ErrPassphraseNotUTF8 and ErrPassphraseTooShort.
func (x *identityCLI) checkPassphrase(p []byte) error {
	switch err := identity.CheckPassphrase(p); {
	case errors.Is(err, identity.ErrPassphraseNotUTF8):
		return x.refuse("passphrase_not_utf8", nil)
	case errors.Is(err, identity.ErrPassphraseTooShort):
		return x.refuse("passphrase_too_short", nil)
	}
	return nil
}

// unlock opens the recovery seed of the identity st describes (KEL L-3, D-16 step 1): with the local
// key when the identity has no passphrase yet, else with the passphrase asked on t. path is the
// recovery file to open when it is not the default.
func (x *identityCLI) unlock(t *ttySession, st daemon.IdentityStatus, path string) (*unlocked, error) {
	if st.Recovery.State == "unprotected" && path == "" {
		rs, aid, _, _, err := readLocalRecovery(x.layout.Root)
		if err != nil {
			return nil, x.refuse("recovery_file_missing", idArgs(st, a2ashape.Args{"path": localRecPath(x.layout.Root)}))
		}
		if aid != st.AID {
			clear(rs[:])
			return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": aid}))
		}
		return &unlocked{rs: rs}, nil
	}
	if path == "" {
		p, err := x.recoveryPath(st.AID)
		if err != nil {
			return nil, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, x.refuse("recovery_file_missing", idArgs(st, a2ashape.Args{"path": path}))
	}
	info, err := identity.ParseRecoveryFile(b)
	switch {
	case err != nil:
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": "unknown"}))
	case info.Unprotected:
		return nil, x.refuse("recovery_file_unprotected", nil)
	case info.AID != st.AID:
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": info.AID}))
	}
	pass, err := readPassphrase(t, x.sentence("identity.prompt.passphrase", nil))
	if err != nil {
		return nil, x.refuse("tty_required", a2ashape.Args{"action": "recovery_passphrase", "command": typedCommand(commandLine())})
	}
	// A passphrase that is not UTF-8 is said so before the file is tried (KEL C-34); a short one
	// goes to the file, which a passphrase set under an older rule may still open.
	if !utf8.Valid(pass) {
		clear(pass)
		return nil, x.refuse("passphrase_not_utf8", nil)
	}
	rs, aid, _, _, err := identity.OpenRecoveryFile(b, pass)
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrRecoveryPassphrase):
		clear(pass)
		return nil, x.refuse("recovery_passphrase_wrong", nil)
	case errors.Is(err, identity.ErrPassphraseNotUTF8):
		clear(pass)
		return nil, x.refuse("passphrase_not_utf8", nil)
	case errors.Is(err, identity.ErrRecoveryUnprotected):
		clear(pass)
		return nil, x.refuse("recovery_file_unprotected", nil)
	default:
		clear(pass)
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": info.AID}))
	}
	if aid != st.AID {
		clear(pass)
		clear(rs[:])
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": aid}))
	}
	return &unlocked{rs: rs, pass: pass}, nil
}

// askNewPassphrase asks for a new recovery passphrase twice (KEL C-34, L-1): too short or not UTF-8 is
// refused at once; two entries that differ ask again, three times.
func (x *identityCLI) askNewPassphrase(t *ttySession) ([]byte, error) {
	for try := 0; try < 3; try++ {
		p, err := readPassphrase(t, x.sentence("identity.prompt.new_passphrase", nil))
		if err != nil {
			return nil, x.refuse("tty_required", a2ashape.Args{"action": "set_recovery_passphrase", "command": typedCommand(commandLine())})
		}
		if err := x.checkPassphrase(p); err != nil {
			clear(p)
			return nil, err
		}
		again, err := readPassphrase(t, x.sentence("identity.prompt.again", nil))
		if err != nil {
			clear(p)
			return nil, x.refuse("tty_required", a2ashape.Args{"action": "set_recovery_passphrase", "command": typedCommand(commandLine())})
		}
		same := string(p) == string(again)
		clear(again)
		if same {
			return p, nil
		}
		clear(p)
		t.say(x.sentence("identity.prompt.mismatch", nil))
	}
	return nil, x.refuse("identity.prompt.mismatch", nil)
}

// openTerminal opens the terminal for a command that needs the passphrase; without one it says
// tty_required for action (KEL L-1) and nothing is sent to the daemon.
func (x *identityCLI) openTerminal(action string) (*ttySession, error) {
	t, err := openTTYSession()
	if err != nil {
		return nil, x.refuse("tty_required", a2ashape.Args{"action": action, "command": typedCommand(commandLine())})
	}
	return t, nil
}

// --- one key event ---

// stepSpec is one rotation the CLI asks the daemon for: the seed that reveals the next key, the
// seed that commits the one after, and the recovery file that goes with it.
type stepSpec struct {
	st     daemon.IdentityStatus
	rsOld  [32]byte
	rsNew  [32]byte
	mode   string
	hubURL string
	// newPath is the recovery file replaced once the daemon has the event; "" keeps the file as it is.
	newPath string
	newPass []byte
	// fileHub is the hub hint of the new file.
	fileHub string
}

// seedAt derives the key the event at seq reveals from rs, and checks it against the commitment the
// daemon states (KEL L-3): a recovery file of another generation is recovery_mismatch, and the
// daemon is not asked.
func seedAt(rs [32]byte, aid string, seq uint64, commitment []byte) ([]byte, error) {
	k, err := identity.RecoverySeed(rs, aid, seq)
	if err != nil {
		return nil, err
	}
	defer clear(k)
	sum := sha256.Sum256(k.Public().(ed25519.PublicKey))
	if string(sum[:]) != string(commitment) {
		return nil, errMismatch
	}
	return append([]byte(nil), k.Seed()...), nil
}

var errMismatch = errors.New("recovery_mismatch")

// step does one rotation (KEL L-3): derive and check the key, write the new recovery file as `.new`
// first, call the daemon, and put the new file in place once the daemon has the event on disk.
func (x *identityCLI) step(sp stepSpec) error {
	st := sp.st
	j := st.KeySeq + 1
	seed, err := seedAt(sp.rsOld, st.AID, j, st.Commitment)
	if errors.Is(err, errMismatch) {
		return x.refuse("recovery_mismatch", nil)
	}
	if err != nil {
		return err
	}
	defer clear(seed)
	next, err := identity.NextDigest(sp.rsNew, st.AID, j+1)
	if err != nil {
		return err
	}
	var np string
	if sp.newPath != "" {
		file, err := sealRecovery(sp.rsNew, st.AID, sp.fileHub, identity.Commit{Seq: j + 1, Digest: next}, sp.newPass)
		if err != nil {
			return err
		}
		np = sp.newPath + ".new"
		if err := writeFileDurable(np, file, 0o600); err != nil {
			return err
		}
	}
	body := map[string]any{
		"mode":          sp.mode,
		"revealed_seed": base64.StdEncoding.EncodeToString(seed),
		"next_digest":   base64.StdEncoding.EncodeToString(next[:]),
	}
	if sp.hubURL != "" {
		body["hub_url"] = sp.hubURL
	}
	b, code, err := x.postRaw("/identity/rotate", body)
	if err == nil && code != 200 {
		err = defaultHubCLI().refusedBy(code, b)
	}
	if err != nil {
		if np != "" {
			_ = os.Remove(np)
		}
		return err
	}
	if np != "" {
		if err := os.Rename(np, sp.newPath); err != nil {
			return err
		}
		syncDir(filepath.Dir(sp.newPath))
	}
	return nil
}

// settle waits for the witness to take the key events up to seq (KEL L-3): the identity is ok again
// and nothing is pending. done is false when idSettle ran out; the event is on disk and the daemon
// finishes it on its own.
func (x *identityCLI) settle(seq uint64) (st daemon.IdentityStatus, done bool, err error) {
	deadline := time.Now().Add(idSettle)
	for {
		b, code, ferr := x.postRaw("/identity/status", map[string]any{})
		if ferr != nil {
			return st, false, ferr
		}
		if code != 200 {
			return st, false, defaultHubCLI().refusedBy(code, b)
		}
		st = daemon.IdentityStatus{}
		if err := json.Unmarshal(b, &st); err != nil {
			return st, false, err
		}
		if st.Pending == nil && st.KeySeq >= seq && (st.State == "ok" || st.State == "notice_stale") {
			return st, true, nil
		}
		if time.Now().After(deadline) {
			return st, false, nil
		}
		time.Sleep(idPoll)
	}
}

// stillPublishing says rotation_pending as information and ends the command with status 0: the event
// is on disk and the daemon finishes it (KEL L-3, L-10).
func (x *identityCLI) stillPublishing() error {
	x.tell("rotation_pending", nil)
	return nil
}

// newSeed makes a recovery seed.
func newSeed() ([32]byte, error) {
	var rs [32]byte
	_, err := rand.Read(rs[:])
	return rs, err
}

func rfc3339Time(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// --- anet identity status (L-7) ---

// statusLabels are the left column of `anet identity status` (UX-G6).
var statusLabels = map[a2ashape.Language][5]string{
	a2ashape.Chinese: {"身份", "密钥", "见证 hub", "恢复文件", "状态"},
	a2ashape.English: {"Identity", "Key", "Witness hub", "Recovery file", "State"},
}

// recoveryFileInfo is what `anet identity status --json` adds (KEL L-7): the recovery file as a stat
// sees it, never opened.
type recoveryFileInfo struct {
	Path        string `json:"path"`
	Present     bool   `json:"present"`
	ModeOK      bool   `json:"mode_ok"`
	Unprotected bool   `json:"unprotected"`
}

func (x *identityCLI) recoveryFileInfo(st daemon.IdentityStatus) recoveryFileInfo {
	if st.Recovery.State == "unprotected" {
		p := localRecPath(x.layout.Root)
		fi, err := os.Stat(p)
		return recoveryFileInfo{Path: p, Present: err == nil, ModeOK: err == nil && fi.Mode().Perm()&0o077 == 0, Unprotected: true}
	}
	p, err := x.recoveryPath(st.AID)
	if err != nil {
		return recoveryFileInfo{}
	}
	fi, serr := os.Stat(p)
	return recoveryFileInfo{Path: p, Present: serr == nil, ModeOK: serr == nil && fi.Mode().Perm()&0o077 == 0}
}

func b64enc(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func parentDir(p string) string { return filepath.Dir(p) }

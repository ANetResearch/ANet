package daemon

// identities.go adds a friendly "named identities" layer over the existing one-data-dir-per-identity
// model, so a power user can run several personas on one laptop (e.g. a "coder" that accepts tasks and a
// "delegator" that sends them out) without hand-juggling data dirs and control ports.
//
// The on-disk reality is unchanged: each identity is still a full data dir (identity.kel + config.json +
// interactions). What's new is a container ("anet home", default ~/.anet) under which named identities
// live at <home>/ids/<name>, plus a <home>/current selector. The reserved name "default" is the flat
// <home> root itself — so a pre-existing single-identity install keeps working untouched (it IS "default").
//
// Selection precedence (strongest first): --id flag  >  ANET_ID  >  ANET_DATA_DIR (a raw dir)  >  the
// `current` selection  >  the default identity. ANET_DATA_DIR remains the low-level escape hatch that
// points at an arbitrary dir (used by tests/scripts) and bypasses the naming machinery.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

// controlPortBase is where auto-allocation starts scanning (matches the historical single-daemon default,
// so the FIRST identity created still lands on 39811 and legacy installs are unaffected).
const controlPortBase = 39811

// identityNameRe constrains identity names to a filesystem- and URL-safe token.
var identityNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidIdentityName reports whether name is a legal identity name. "default" is allowed (it maps to the
// flat home root); the literal "ids" is reserved (it is the container subdir).
func ValidIdentityName(name string) bool {
	if name == "" || name == "ids" || len(name) > 64 {
		return false
	}
	return identityNameRe.MatchString(name)
}

// AnetHome is the container for named identities: env ANET_HOME, else ~/.anet, else ./.anet. It is
// deliberately independent of ANET_DATA_DIR (which is a raw single-dir override handled in ResolveLayout).
func AnetHome() string {
	if d := os.Getenv("ANET_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".anet"
	}
	return filepath.Join(home, ".anet")
}

// IdentityLayout returns the data-dir layout for a named identity. "" or "default" → the flat home root
// (backward compatible); any other name → <home>/ids/<name>.
func IdentityLayout(name string) Layout {
	if name == "" || name == "default" {
		return Layout{Root: AnetHome()}
	}
	return Layout{Root: filepath.Join(AnetHome(), "ids", name)}
}

// IdentityNameForDir is the reverse map used for diagnostics: given a data dir, return the identity name
// ("default" for the home root, the subdir name for <home>/ids/<name>, or "" for an unrelated raw dir).
func IdentityNameForDir(dir string) string {
	clean := filepath.Clean(dir)
	if clean == filepath.Clean(AnetHome()) {
		return "default"
	}
	idsDir := filepath.Clean(filepath.Join(AnetHome(), "ids"))
	if parent := filepath.Dir(clean); parent == idsDir {
		return filepath.Base(clean)
	}
	return ""
}

// currentPath holds the name selected by `anet id use`.
func currentPath() string { return filepath.Join(AnetHome(), "current") }

// CurrentIdentity returns the operator's selected default identity name ("default" when unset).
func CurrentIdentity() string {
	b, err := os.ReadFile(currentPath())
	if err != nil {
		return "default"
	}
	if name := strings.TrimSpace(string(b)); ValidIdentityName(name) {
		return name
	}
	return "default"
}

// SetCurrentIdentity persists the selected default identity name.
func SetCurrentIdentity(name string) error {
	if !ValidIdentityName(name) {
		return fmt.Errorf("invalid identity name %q", name)
	}
	if err := os.MkdirAll(AnetHome(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(currentPath(), []byte(name+"\n"), 0o600)
}

// ClearCurrentIfName resets the selection to default if it currently points at name (used when removing an
// identity that happens to be the selected one).
func ClearCurrentIfName(name string) {
	if CurrentIdentity() == name {
		_ = os.Remove(currentPath())
	}
}

// ResolveLayout picks the identity data dir from the strongest available selector. idFlag is the parsed
// --id value ("" if absent). See the precedence note at the top of this file.
func ResolveLayout(idFlag string) Layout {
	if idFlag != "" {
		return IdentityLayout(idFlag)
	}
	if v := os.Getenv("ANET_ID"); v != "" {
		return IdentityLayout(v)
	}
	if v := os.Getenv("ANET_DATA_DIR"); v != "" {
		return Layout{Root: v}
	}
	if cur := CurrentIdentity(); cur != "default" {
		return IdentityLayout(cur)
	}
	return IdentityLayout("default")
}

// SelectionIsExplicit reports whether the operator pinned a specific identity (via --id/ANET_ID/
// ANET_DATA_DIR/a non-default `current`). When true, the CLI resolves the control plane STRICTLY — it
// talks only to that identity's own daemon and never falls back to the uid pointer (which, with several
// daemons running, could otherwise silently target the wrong one).
func SelectionIsExplicit(idFlag string) bool {
	return idFlag != "" || os.Getenv("ANET_ID") != "" || os.Getenv("ANET_DATA_DIR") != "" || CurrentIdentity() != "default"
}

// loadConfigNoCreate reads config.json WITHOUT the side effect of writing a default file when absent (used
// by read-only enumeration/port scanning). ok=false means no readable config yet.
func loadConfigNoCreate(l Layout) (cfg Config, ok bool) {
	b, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		return DefaultConfig(), false
	}
	if json.Unmarshal(b, &cfg) != nil {
		return DefaultConfig(), false
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = DefaultConfig().ControlAddr
	}
	return cfg, true
}

// ReadIdentityAID returns the AID persisted in a layout WITHOUT generating one (unlike
// LoadOrGenerateIdentity). "" when the identity has not been incepted yet.
func ReadIdentityAID(l Layout) string {
	b, err := os.ReadFile(l.IdentityPath())
	if err != nil {
		return ""
	}
	c, err := identity.Restore(b)
	if err != nil {
		return ""
	}
	return c.AID()
}

// IdentityInfo is one known local identity, for `anet id ls`.
type IdentityInfo struct {
	Name        string
	DataDir     string
	AID         string // "" if not yet incepted
	ControlAddr string
	HubURL      string
	Running     bool
	Current     bool
}

// Initialized reports whether a data dir already holds an identity or config (so enumeration skips empty
// candidate dirs, and `id new`/`id use` can tell "exists" from "not yet").
func (l Layout) Initialized() bool {
	if _, err := os.Stat(l.IdentityPath()); err == nil {
		return true
	}
	if _, err := os.Stat(l.ConfigPath()); err == nil {
		return true
	}
	return false
}

// ListIdentities enumerates every known identity: the default (flat home root, if initialized) plus each
// <home>/ids/<name>. It reads config/identity read-only and marks which are currently running.
func ListIdentities() ([]IdentityInfo, error) {
	cur := CurrentIdentity()
	running := map[string]bool{}
	for _, e := range RunningDaemons() {
		running[e.ControlAddr] = true
	}
	var out []IdentityInfo
	add := func(name, dir string) {
		l := Layout{Root: dir}
		if !l.Initialized() {
			return
		}
		cfg, _ := loadConfigNoCreate(l)
		out = append(out, IdentityInfo{
			Name: name, DataDir: dir, AID: ReadIdentityAID(l),
			ControlAddr: cfg.ControlAddr, HubURL: cfg.HubURL,
			Running: running[cfg.ControlAddr], Current: name == cur,
		})
	}
	add("default", AnetHome())
	ents, err := os.ReadDir(filepath.Join(AnetHome(), "ids"))
	if err == nil {
		for _, e := range ents {
			if e.IsDir() && ValidIdentityName(e.Name()) {
				add(e.Name(), filepath.Join(AnetHome(), "ids", e.Name()))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// portFree reports whether a loopback TCP port can be bound right now.
func portFree(p int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// AllocControlPort returns a loopback control port not already claimed by any known identity's config and
// currently bindable, scanning up from controlPortBase. This is what lets a second identity start without
// the operator hand-editing config.json to avoid the default-port collision.
func AllocControlPort() (int, error) {
	used := map[int]bool{}
	ids, _ := ListIdentities()
	for _, in := range ids {
		if _, ps, err := net.SplitHostPort(in.ControlAddr); err == nil {
			if p, e := strconv.Atoi(ps); e == nil {
				used[p] = true
			}
		}
	}
	for p := controlPortBase; p < controlPortBase+2000; p++ {
		if used[p] || !portFree(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free control port found in %d-%d", controlPortBase, controlPortBase+2000)
}

// EnsureLayoutInit makes a data dir ready to run a daemon: if it has no config yet it writes one with an
// auto-allocated free control port (so multiple identities never collide on the default port) and incepts
// the identity key. Idempotent — an already-initialized dir is loaded as-is. Returns the effective config
// and whether it was freshly created.
func EnsureLayoutInit(l Layout) (Config, bool, error) {
	if _, err := os.Stat(l.ConfigPath()); err == nil {
		cfg, lerr := LoadConfig(l)
		if lerr != nil {
			return Config{}, false, lerr
		}
		if _, ierr := LoadOrGenerateIdentity(l); ierr != nil {
			return cfg, false, ierr
		}
		return cfg, false, nil
	}
	port, err := AllocControlPort()
	if err != nil {
		return Config{}, false, err
	}
	on := true
	cfg := Config{ControlAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), AcceptDelegations: &on}
	if err := SaveConfig(l, cfg); err != nil {
		return Config{}, false, err
	}
	if _, ierr := LoadOrGenerateIdentity(l); ierr != nil {
		return cfg, true, ierr
	}
	return cfg, true, nil
}

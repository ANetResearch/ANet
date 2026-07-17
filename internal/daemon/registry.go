package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IdentityEntry is one locally-running daemon — a "logged-in identity" the web console can switch
// between. anet is one-identity-per-daemon (each has its own data dir, key, and control port), so an
// operator running several personas runs several daemons; this registry lets the console list them and
// hop between their consoles like switching accounts.
type IdentityEntry struct {
	AID         string `json:"aid"`
	Name        string `json:"name"`
	ControlAddr string `json:"control_addr"`
	// DataDir is the daemon's data dir (its identity/config root). Recorded so a CLI that connected to
	// the wrong (default) dir can tell the operator exactly what ANET_DATA_DIR to set. Older entries that
	// predate this field simply carry "".
	DataDir string `json:"data_dir,omitempty"`
}

// registryEntryFile is keyed by control address (unique per running daemon), so restarting a daemon on
// the same port overwrites its own entry rather than accumulating duplicates.
func registryEntryFile(controlAddr string) string {
	safe := strings.NewReplacer(":", "_", "/", "_").Replace(controlAddr)
	return filepath.Join(DaemonsDir(), safe+".json")
}

// writeRegistry records (or refreshes) this daemon's identity so the console can offer it in the
// identity switcher. Best-effort — a missing registry just means the switcher shows only the current
// identity.
func (d *Daemon) writeRegistry() {
	if err := os.MkdirAll(DaemonsDir(), 0o700); err != nil {
		return
	}
	cfg := d.config()
	b, err := json.Marshal(IdentityEntry{AID: d.AID(), Name: cfg.Name, ControlAddr: cfg.ControlAddr, DataDir: d.layout.Root})
	if err != nil {
		return
	}
	_ = writeFileAtomic(registryEntryFile(cfg.ControlAddr), b, 0o600)
}

// removeRegistryEntry drops this daemon's identity from the registry on shutdown.
func removeRegistryEntry(controlAddr string) { _ = os.Remove(registryEntryFile(controlAddr)) }

// listRegistry returns all recorded local identities. It may include stale entries for daemons that
// crashed without cleaning up; the console confirms liveness before offering them (a dead one simply
// fails to load when navigated to).
func listRegistry() ([]IdentityEntry, error) {
	ents, err := os.ReadDir(DaemonsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]IdentityEntry, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(DaemonsDir(), e.Name()))
		if rerr != nil {
			continue
		}
		var ie IdentityEntry
		if json.Unmarshal(b, &ie) == nil && ie.AID != "" && ie.ControlAddr != "" {
			out = append(out, ie)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ControlAddr < out[j].ControlAddr })
	return out, nil
}

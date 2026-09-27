package daemon

// setup.go backs `anet init` and the policy half of `anet doctor`
// (A2A-DESIGN §13.1, SI-5).
//
// InitLayout writes the safe defaults explicitly: a config that states
// inbound.policy=closed and auto_max=0 says so to anyone who opens it,
// where a config that leaves them out relies on this build's defaults
// staying what they are. On an existing config it only adds keys that are
// missing and reports every value that differs from the fresh-install
// default without changing it: init is not a reset.
//
// It does not write a modules.a2a block (§11.1: the no_a2a variant would
// then refuse to load the config), and it does not create an auto_reply
// block: in this daemon the presence of that block is what turns the
// auto-reply loop on, and an absent block already means untrusted=off.
//
// ReadPolicy reads the same configuration without creating or changing
// anything, for doctor.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// InitChange is one thing InitLayout did or found.
type InitChange struct {
	// Key is a config key path ("inbound.policy") or a file name.
	Key string `json:"key"`
	// Action is "added" (a missing key was written), "created" (an empty
	// list file), "removed" (the wire-1 accept_delegations key), "kept" (an
	// existing value or file left as it is) or "mode" (a file's permissions
	// were narrowed to 0600).
	Action string `json:"action"`
	Value  any    `json:"value,omitempty"`
	Note   string `json:"note,omitempty"`
}

// InitReport is the outcome of InitLayout.
type InitReport struct {
	DataDir    string       `json:"data_dir"`
	ConfigPath string       `json:"config_path"`
	Created    bool         `json:"created"` // config.json did not exist
	Wrote      bool         `json:"wrote"`   // config.json was written
	Changes    []InitChange `json:"changes"`
	// Kept lists the SI-5 keys whose value differs from the fresh-install
	// default. InitLayout leaves them as they are.
	Kept []InitChange `json:"kept"`
	// Problems are findings that stop the daemon from starting or make a
	// list unreadable; InitLayout does not fix them.
	Problems []string `json:"problems"`
}

// InitLayout makes l's data directory state the safe defaults (see the file
// comment). It never overwrites an existing value and refuses to touch a
// config.json it cannot parse.
func InitLayout(l Layout) (InitReport, error) {
	rep := InitReport{DataDir: l.Root, ConfigPath: l.ConfigPath(), Changes: []InitChange{}, Kept: []InitChange{},
		Problems: []string{}}
	if err := l.EnsureRoot(); err != nil {
		return rep, err
	}
	raw, err := os.ReadFile(l.ConfigPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg, ferr := freshConfig()
		if ferr != nil {
			cfg = DefaultConfig()
		}
		if err := SaveConfig(l, cfg); err != nil {
			return rep, err
		}
		rep.Created, rep.Wrote = true, true
		rep.Changes = append(rep.Changes, InitChange{Key: "config.json", Action: "created",
			Note: "control_addr " + cfg.ControlAddr})
	case err != nil:
		return rep, fmt.Errorf("anet: read config: %w", err)
	default:
		changes, out, err := fillConfig(raw)
		if err != nil {
			return rep, fmt.Errorf("anet: %s: %w (nothing was changed)", l.ConfigPath(), err)
		}
		if len(changes) > 0 {
			if err := writeFileAtomic(l.ConfigPath(), out, 0o600); err != nil {
				return rep, err
			}
			rep.Wrote = true
			rep.Changes = append(rep.Changes, changes...)
		} else if fi, err := os.Stat(l.ConfigPath()); err == nil && fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(l.ConfigPath(), 0o600); err == nil {
				rep.Changes = append(rep.Changes, InitChange{Key: "config.json", Action: "mode",
					Note: fmt.Sprintf("%04o → 0600", fi.Mode().Perm())})
			}
		}
	}

	st, err := ReadPolicy(l)
	if err != nil {
		return rep, err
	}
	if st.ConfigError != "" {
		rep.Problems = append(rep.Problems, st.ConfigError)
	}
	for _, f := range st.listFiles() {
		if f.path == "" {
			continue
		}
		ch, perr := ensureListFile(f.path)
		if perr != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", f.path, perr))
			continue
		}
		ch.Key = f.key
		rep.Changes = append(rep.Changes, ch)
	}
	// Re-read: the list files may have just been created.
	if st, err = ReadPolicy(l); err != nil {
		return rep, err
	}
	for _, e := range st.ListErrors {
		rep.Problems = append(rep.Problems, e)
	}
	values, changed := st.SI5()
	for _, k := range changed {
		rep.Kept = append(rep.Kept, InitChange{Key: k, Action: "kept", Value: values[k],
			Note: "fresh-install default: " + si5DefaultText[k]})
	}
	return rep, nil
}

// fillConfig adds the missing keys of the safe defaults to a config.json and
// returns what it added and the new file. It works on the JSON object rather
// than on Config, so keys this build does not know are kept.
func fillConfig(raw []byte) ([]InitChange, []byte, error) {
	cur, err := decodeObject(raw)
	if err != nil {
		return nil, nil, err
	}
	b, err := json.Marshal(DefaultConfig())
	if err != nil {
		return nil, nil, err
	}
	def, err := decodeObject(b)
	if err != nil {
		return nil, nil, err
	}
	var changes []InitChange
	if v, ok := cur["control_addr"].(string); !ok || v == "" {
		addr := DefaultConfig().ControlAddr
		if port, perr := AllocControlPort(); perr == nil {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		}
		cur["control_addr"] = addr
		changes = append(changes, InitChange{Key: "control_addr", Action: "added", Value: addr})
	}
	if v, ok := cur["accept_delegations"]; ok {
		delete(cur, "accept_delegations")
		changes = append(changes, InitChange{Key: "accept_delegations", Action: "removed", Value: v,
			Note: "replaced by inbound.policy (A2A-DESIGN §5.1)"})
	}
	for _, block := range []string{"inbound", "payments"} {
		changes = append(changes, fillMissing(cur, def, block, block)...)
	}
	if ar, ok := cur["auto_reply"].(map[string]any); ok {
		if _, has := ar["untrusted"]; !has {
			ar["untrusted"] = UntrustedOff
			changes = append(changes, InitChange{Key: "auto_reply.untrusted", Action: "added", Value: UntrustedOff})
		}
	}
	if len(changes) == 0 {
		return nil, nil, nil
	}
	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	// The result must still be a config this daemon loads.
	var check Config
	if err := json.Unmarshal(out, &check); err != nil {
		return nil, nil, fmt.Errorf("the completed config would not load: %w", err)
	}
	return changes, out, nil
}

// fillMissing copies def[key] into cur[key] when it is missing, and
// recurses into objects present on both sides. path names key in reports.
func fillMissing(cur, def map[string]any, key, path string) []InitChange {
	dv, ok := def[key]
	if !ok {
		return nil
	}
	cv, present := cur[key]
	if !present || cv == nil {
		cur[key] = dv
		if m, ok := dv.(map[string]any); ok {
			var out []InitChange
			for _, k := range sortedMapKeys(m) {
				out = append(out, leafChanges(m[k], path+"."+k)...)
			}
			return out
		}
		return []InitChange{{Key: path, Action: "added", Value: dv}}
	}
	cm, cok := cv.(map[string]any)
	dm, dok := dv.(map[string]any)
	if !cok || !dok {
		return nil
	}
	var out []InitChange
	for _, k := range sortedMapKeys(dm) {
		out = append(out, fillMissing(cm, dm, k, path+"."+k)...)
	}
	return out
}

// leafChanges reports an added value, one entry per leaf of an object.
func leafChanges(v any, path string) []InitChange {
	m, ok := v.(map[string]any)
	if !ok {
		return []InitChange{{Key: path, Action: "added", Value: v}}
	}
	var out []InitChange
	for _, k := range sortedMapKeys(m) {
		out = append(out, leafChanges(m[k], path+"."+k)...)
	}
	return out
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decodeObject decodes a JSON object keeping numbers exact.
func decodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("config is not a JSON object")
	}
	return m, nil
}

// ensureListFile creates an empty list file (0600) when none exists.
func ensureListFile(path string) (InitChange, error) {
	fi, err := os.Stat(path)
	if err == nil {
		if !fi.Mode().IsRegular() {
			return InitChange{}, errors.New("exists and is not a regular file")
		}
		n := 0
		if m, rerr := readPeerFile(path); rerr == nil {
			n = len(m)
		}
		return InitChange{Action: "kept", Value: n, Note: path}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return InitChange{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return InitChange{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return InitChange{}, err
	}
	if err := f.Close(); err != nil {
		return InitChange{}, err
	}
	return InitChange{Action: "created", Note: path}, nil
}

// PolicyState is the inbound and payments configuration of a data
// directory as it is on disk, with the defaults the daemon would apply.
type PolicyState struct {
	ConfigPath    string `json:"config_path"`
	ConfigPresent bool   `json:"config_present"`
	// ConfigError is set when config.json cannot be read or parsed, or
	// fails the daemon's start-up check; the daemon would not start.
	ConfigError string         `json:"config_error,omitempty"`
	Config      Config         `json:"-"`
	Inbound     InboundConfig  `json:"inbound"`
	Payments    PaymentsConfig `json:"payments"`
	// The lists, resolved against the data directory and read now.
	AllowPath  string   `json:"allow_path"`
	DenyPath   string   `json:"deny_path"`
	TrustPath  string   `json:"trust_path"`
	PayeesPath string   `json:"payees_path,omitempty"`
	Allow      []string `json:"allow"`
	Deny       []string `json:"deny"`
	Trust      []string `json:"trust"`
	Payees     []string `json:"payees"`
	// ListErrors names a list that exists but cannot be read.
	ListErrors []string `json:"list_errors,omitempty"`
}

// ReadPolicy reads l's configuration and lists without creating or changing
// anything. A missing config.json yields the defaults with ConfigPresent
// false.
func ReadPolicy(l Layout) (PolicyState, error) {
	st := PolicyState{ConfigPath: l.ConfigPath()}
	cfg := DefaultConfig()
	raw, err := os.ReadFile(l.ConfigPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		st.ConfigPresent = true
		st.ConfigError = fmt.Sprintf("cannot read %s: %v", l.ConfigPath(), err)
	default:
		st.ConfigPresent = true
		var c Config
		if err := json.Unmarshal(raw, &c); err != nil {
			st.ConfigError = fmt.Sprintf("cannot parse %s: %v", l.ConfigPath(), err)
		} else {
			if c.Inbound == nil {
				in := defaultInbound()
				c.Inbound = &in
			}
			if c.Payments == nil {
				p := defaultPayments()
				c.Payments = &p
			}
			cfg = c
			// Module declarations are not known outside the daemon; the
			// untrusted-backend half of the check is left to start-up.
			if err := validatePolicy(cfg, false); err != nil {
				st.ConfigError = err.Error()
			}
		}
	}
	st.Config = cfg
	st.Inbound = cfg.inbound()
	st.Payments = cfg.payments()
	resolve := func(name string) string {
		if name == "" || filepath.IsAbs(name) {
			return name
		}
		return filepath.Join(l.Root, name)
	}
	st.AllowPath, st.DenyPath, st.TrustPath = resolve(st.Inbound.AllowFile), resolve(st.Inbound.DenyFile), resolve(st.Inbound.TrustFile)
	st.PayeesPath = resolve(st.Payments.PayeesFile)
	read := func(path string) []string {
		m, err := readPeerFile(path)
		if err != nil {
			st.ListErrors = append(st.ListErrors, fmt.Sprintf("cannot read %s: %v", path, err))
			return []string{}
		}
		return sortedKeys(m)
	}
	st.Allow, st.Deny, st.Trust, st.Payees = read(st.AllowPath), read(st.DenyPath), read(st.TrustPath), read(st.PayeesPath)
	return st, nil
}

type listFile struct{ key, path string }

// listFiles names the four list files InitLayout creates.
func (st PolicyState) listFiles() []listFile {
	return []listFile{
		{"peers.allow", st.AllowPath}, {"peers.deny", st.DenyPath}, {"peers.trust", st.TrustPath},
		{"payees.allow", st.PayeesPath},
	}
}

// SI5Keys are the keys of SI-5 (A2A-DESIGN §1) in the order the invariant
// lists them. doctor --json reports each under exactly this name.
var SI5Keys = []string{
	"inbound.policy",
	"peers.allow",
	"peers.trust",
	"inbound.public_capabilities",
	"auto_reply.untrusted",
	"payments.auto_max",
	"payments.agent_max",
	"payments.agent_daily_max",
	"payments.payees_file",
	"payments.payees",
}

// si5DefaultText states each key's fresh-install value for reports.
var si5DefaultText = map[string]string{
	"inbound.policy":              PolicyClosed,
	"peers.allow":                 "empty",
	"peers.trust":                 "empty",
	"inbound.public_capabilities": "empty",
	"auto_reply.untrusted":        UntrustedOff,
	"payments.auto_max":           "0",
	"payments.agent_max":          "0",
	"payments.agent_daily_max":    "0",
	"payments.payees_file":        "set (payee list on)",
	"payments.payees":             "empty",
}

// SI5 returns the value of every SI-5 key and the keys whose value differs
// from the fresh-install default, in SI5Keys order.
func (st PolicyState) SI5() (values map[string]any, changed []string) {
	untrusted := UntrustedOff
	if ar := st.Config.AutoReply; ar != nil {
		untrusted = ar.UntrustedMode()
		if ar.Untrusted != "" && ar.Untrusted != UntrustedOff && ar.Untrusted != UntrustedSandbox {
			untrusted = ar.Untrusted // invalid; shown as written
		}
	}
	caps := make([]string, 0, len(st.Inbound.PublicCapabilities))
	for _, c := range st.Inbound.PublicCapabilities {
		caps = append(caps, c.ID)
	}
	values = map[string]any{
		"inbound.policy":              st.Inbound.Policy,
		"peers.allow":                 nonNil(st.Allow),
		"peers.trust":                 nonNil(st.Trust),
		"inbound.public_capabilities": caps,
		"auto_reply.untrusted":        untrusted,
		"payments.auto_max":           st.Payments.AutoMax,
		"payments.agent_max":          st.Payments.AgentMax,
		"payments.agent_daily_max":    st.Payments.AgentDailyMax,
		"payments.payees_file":        st.Payments.PayeesFile,
		"payments.payees":             nonNil(st.Payees),
	}
	safe := map[string]bool{
		"inbound.policy":              st.Inbound.Policy == PolicyClosed,
		"peers.allow":                 len(st.Allow) == 0,
		"peers.trust":                 len(st.Trust) == 0,
		"inbound.public_capabilities": len(caps) == 0,
		"auto_reply.untrusted":        untrusted == UntrustedOff,
		"payments.auto_max":           st.Payments.AutoMax == 0,
		"payments.agent_max":          st.Payments.AgentMax == 0,
		"payments.agent_daily_max":    st.Payments.AgentDailyMax == 0,
		"payments.payees_file":        st.Payments.PayeesFile != "",
		"payments.payees":             len(st.Payees) == 0,
	}
	for _, k := range SI5Keys {
		if !safe[k] {
			changed = append(changed, k)
		}
	}
	return values, changed
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

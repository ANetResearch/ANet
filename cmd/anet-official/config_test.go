package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The capability table is what the routes, the generated configuration and
// the card are built from; it must be fit for all three.
func TestCapabilityTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range allCapabilities() {
		switch {
		case c.ID == "" || c.Group == "" || c.Handle == nil:
			t.Errorf("%+v: id, group and handler are required", c)
		case seen[c.ID]:
			t.Errorf("%s declared twice", c.ID)
		case c.MaxArgsBytes <= 0 || c.Timeout <= 0:
			t.Errorf("%s: limits must be set", c.ID)
		case c.Timeout.Seconds() > 60:
			t.Errorf("%s: a timeout over the daemon's 60 s default would make it a long-running call", c.ID)
		}
		seen[c.ID] = true
		// The card limits of ANetCore a2acard, and what A2A requires of a
		// skill.
		if c.Name == "" || len(c.Name) > 128 || c.Description == "" || len(c.Description) > 4096 {
			t.Errorf("%s: name/description missing or over the card limits", c.ID)
		}
		if len(c.Tags) == 0 || len(c.Tags) > 16 {
			t.Errorf("%s: %d tags", c.ID, len(c.Tags))
		}
		for _, ex := range c.Examples {
			if strings.HasPrefix(ex, "{") && !strings.Contains(ex, "...") && !json.Valid([]byte(ex)) {
				t.Errorf("%s: example %s is not JSON", c.ID, ex)
			}
		}
	}
	for _, want := range []string{"net.echo", "text.stats", "text.digest", "text.diff", "json.validate",
		"a2a.card.validate", "a2a.x402.check", "docs.search", "docs.get", "demo.digest.paid"} {
		if !seen[want] {
			t.Errorf("%s (A2A-DESIGN §15 first batch) is missing", want)
		}
	}
}

// Every example in the table runs and succeeds: they are what an agent
// reading the card tries first.
func TestExamplesRun(t *testing.T) {
	s, _ := testServer(t, "echo,tools,docs,paid")
	for _, c := range allCapabilities() {
		for _, ex := range c.Examples {
			if strings.Contains(ex, "...") {
				continue // an elided illustration, not a runnable call
			}
			if w := do(t, s, routeOf(c), ex); w.Code != 200 {
				t.Errorf("%s example %s: HTTP %d %s", c.ID, ex, w.Code, w.Body.String())
			}
		}
	}
}

// readEnv reads KEY=VALUE lines.
func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: bad line %q", path, line)
		}
		out[k] = v
	}
	return out
}

// The deploy samples are the generated configuration: a limit or a
// description changed in the table and not regenerated into
// deploy/official fails here.
func TestDeploySamplesMatchTheTable(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "official")
	identities := map[string]string{ // identity -> group
		"anet-echo-e": "echo", "anet-echo-f": "echo", "anet-tools": "tools", "anet-docs": "docs", "anet-paid-demo": "paid",
	}
	sockets := map[string]string{}
	for id, group := range identities {
		env := readEnv(t, filepath.Join(root, id, "backend.env"))
		if env["CAP_GROUPS"] != group {
			t.Errorf("%s: CAP_GROUPS=%s, want %s", id, env["CAP_GROUPS"], group)
		}
		// The backend's Unix socket, in the unit's RuntimeDirectory
		// (anet-official/%i): a loopback port could be taken by another
		// local user while the backend is down (docs/notes/0030 N1).
		if want := "unix:/run/anet-official/" + id + "/backend.sock"; env["LISTEN"] != want {
			t.Errorf("%s: LISTEN=%s, want %s", id, env["LISTEN"], want)
		}
		base, err := daemonURL(env["LISTEN"])
		if err != nil {
			t.Errorf("%s: %v", id, err)
		}
		if other, dup := sockets[env["LISTEN"]]; dup {
			t.Errorf("%s and %s share %s", id, other, env["LISTEN"])
		}
		sockets[env["LISTEN"]] = id

		raw, err := os.ReadFile(filepath.Join(root, id, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Name    string `json:"name"`
			Inbound struct {
				Policy             string            `json:"policy"`
				PublicCapabilities []publicCapConfig `json:"public_capabilities"`
			} `json:"inbound"`
			Modules map[string]json.RawMessage `json:"modules"`
			// Keys a sample must not carry.
			AutoReply         any `json:"auto_reply"`
			AcceptDelegations any `json:"accept_delegations"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if cfg.Name != id || cfg.Inbound.Policy != "closed" || cfg.AutoReply != nil || cfg.AcceptDelegations != nil {
			t.Errorf("%s: name %q policy %q auto_reply %v accept_delegations %v", id, cfg.Name, cfg.Inbound.Policy, cfg.AutoReply, cfg.AcceptDelegations)
		}
		caps, _ := selectGroups(group)
		want := buildServiceConfig(caps, base, "${CREDENTIALS_DIRECTORY}/token", 0)
		if !reflect.DeepEqual(cfg.Inbound.PublicCapabilities, want.Inbound.PublicCapabilities) {
			t.Errorf("%s: public_capabilities differ from `anet-official service-config -groups %s`", id, group)
		}
		// The published retention policy (README §5): the chain of an
		// official agent keeps the result CID and the metrics, not answers.
		for _, pc := range cfg.Inbound.PublicCapabilities {
			if pc.Evidence != "cid" {
				t.Errorf("%s: %s has evidence %q, want cid", id, pc.ID, pc.Evidence)
			}
		}
		var svc svcModuleConfig
		if err := json.Unmarshal(cfg.Modules["service"], &svc); err != nil {
			t.Fatalf("%s: modules.service: %v", id, err)
		}
		if !reflect.DeepEqual(svc, want.Modules.Service) {
			t.Errorf("%s: modules.service differs from `anet-official service-config -groups %s -url %s`", id, group, base)
		}
		_, hasX402 := cfg.Modules["x402"]
		if hasX402 != (group == "paid") {
			t.Errorf("%s: x402 module configured = %v", id, hasX402)
		}
	}
	for _, unit := range []string{"anet-official-backend@.service", "anet-official-daemon@.service"} {
		b, err := os.ReadFile(filepath.Join(root, unit))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "User=root") || !strings.Contains(s, "NoNewPrivileges=yes") || !strings.Contains(s, "LoadCredential=token:") {
			t.Errorf("%s: must run unprivileged and take the token as a credential", unit)
		}
		if unit == "anet-official-daemon@.service" && !strings.Contains(s, "User=anet-official") {
			t.Errorf("%s: must run as the dedicated account", unit)
		}
		if unit == "anet-official-backend@.service" && !strings.Contains(s, "IPAddressDeny=any") {
			t.Errorf("%s: the backend must reach nothing but loopback", unit)
		}
		// The socket's directory is the backend's own under root's
		// /run/anet-official, and both units share the socket's group.
		if unit == "anet-official-backend@.service" {
			for _, want := range []string{"RuntimeDirectory=anet-official/%i\n", "SupplementaryGroups=anet-official-ipc\n",
				"-socket-group anet-official-ipc", "RestrictAddressFamilies=AF_UNIX\n", "PrivateNetwork=yes\n"} {
				if !strings.Contains(s, want) {
					t.Errorf("%s: missing %q", unit, strings.TrimSpace(want))
				}
			}
			if strings.Contains(s, "\nPrivateUsers=yes") || strings.Contains(s, "AF_INET") {
				t.Errorf("%s: PrivateUsers= maps the socket's group to nobody; the backend needs no AF_INET", unit)
			}
			// -socket-group is a chown(2), and systemd's @privileged set
			// includes @chown: a unit that denies @privileged and does not
			// allow @chown back starts a backend that exits at once with
			// "operation not permitted". The SystemCallFilter= lines are
			// merged in order, a later allow-list line adding back.
			if !chownAllowed(s) {
				t.Errorf("%s: its SystemCallFilter= denies chown(2), which -socket-group needs; add SystemCallFilter=@chown after ~@privileged", unit)
			}
		}
		if unit == "anet-official-daemon@.service" && !strings.Contains(s, "SupplementaryGroups=anet-official-ipc\n") {
			t.Errorf("%s: the daemon must be in the socket's group", unit)
		}
	}
}

// chownAllowed replays a unit's SystemCallFilter= lines in order, as
// systemd merges them, and reports whether chown(2) ends up allowed. The
// first line decides the kind of list; an allow-list line naming @chown,
// @privileged or @system-service (both include @chown) allows it, a
// "~" line naming @chown or @privileged denies it; an empty assignment
// resets.
func chownAllowed(unit string) bool {
	allowed, seen := true, false
	for _, line := range strings.Split(unit, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "SystemCallFilter=")
		if !ok {
			continue
		}
		if v == "" {
			allowed, seen = true, false
			continue
		}
		deny := strings.HasPrefix(v, "~")
		names := strings.Fields(strings.TrimPrefix(v, "~"))
		has := func(set ...string) bool {
			for _, n := range names {
				for _, s := range set {
					if n == s {
						return true
					}
				}
			}
			return false
		}
		switch {
		case deny && has("@chown", "@privileged"):
			allowed = false
		case !deny && has("@chown", "@privileged", "@system-service"):
			allowed = true
		case !deny && !seen:
			allowed = false // an allow list that does not name it
		}
		seen = true
	}
	return allowed
}

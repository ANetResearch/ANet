package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// controllerAID is a whole AID (identity.ValidAID).
const controllerAID = "bafyreie6k7xot2qpitfcoseuevnuq7zcztdpdgx4coe3p2jlqjjl65hqu4"

// dirSnapshot is every regular file under root with its content, the lock file left out (taking the lock
// creates it).
func dirSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && p == root {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == "daemon.lock" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// shellBlockOnDisk is modules.shell of l's config.json, nil when absent.
func shellBlockOnDisk(t *testing.T, l Layout) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Modules map[string]map[string]any `json:"modules"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Modules["shell"]
}

// countingConfirm answers with err and counts its calls and the plans it was shown.
type countingConfirm struct {
	err   error
	plans []ShellControllerPlan
}

func (c *countingConfirm) confirm(p ShellControllerPlan) error {
	c.plans = append(c.plans, p)
	return c.err
}

// D9: a fresh data directory gets the block and the allow entry under one confirmation; a second run
// changes nothing and asks nothing.
func TestInitShellControllerWritesBothOnceThenNothing(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "fresh"))
	c := &countingConfirm{}
	plan, err := InitShellController(l, controllerAID, "", c.confirm)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.plans) != 1 {
		t.Fatalf("confirm called %d times, want 1", len(c.plans))
	}
	want := ShellControllerPlan{AID: controllerAID, ModuleBlock: json.RawMessage(`{"allow":["` + controllerAID + `"],"allow_arbitrary":true}`),
		AddAllow: true, Changed: true}
	if !reflect.DeepEqual(plan, want) || !reflect.DeepEqual(c.plans[0], want) {
		t.Fatalf("plan %+v (confirmed %+v), want %+v", plan, c.plans[0], want)
	}
	if got := shellBlockOnDisk(t, l); !reflect.DeepEqual(got, map[string]any{"allow": []any{controllerAID}, "allow_arbitrary": true}) {
		t.Fatalf("modules.shell = %v", got)
	}
	allow, err := readPeerFile(filepath.Join(l.Root, peersAllowFile))
	if err != nil || len(allow) != 1 || !allow[controllerAID] {
		t.Fatalf("peers.allow = %v (%v)", allow, err)
	}
	// The configuration still loads, and the rest of the fresh layout is there (InitLayout ran).
	if _, err := LoadConfig(l); err != nil {
		t.Fatalf("config does not load: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.Root, peersDenyFile)); err != nil {
		t.Fatalf("InitLayout did not run: %v", err)
	}

	before := dirSnapshot(t, l.Root)
	c2 := &countingConfirm{}
	plan, err = InitShellController(l, controllerAID, "", c2.confirm)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.plans) != 0 || plan.Changed || plan.AddAllow || plan.ModuleBlock != nil {
		t.Fatalf("second run: plan %+v, confirm called %d times", plan, len(c2.plans))
	}
	if after := dirSnapshot(t, l.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("second run changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// The interpreter goes into the block as "shell", and a block without it is a different block.
func TestInitShellControllerInterpreter(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "d"))
	if _, err := InitShellController(l, controllerAID, "powershell", func(ShellControllerPlan) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"allow": []any{controllerAID}, "allow_arbitrary": true, "shell": "powershell"}
	if got := shellBlockOnDisk(t, l); !reflect.DeepEqual(got, want) {
		t.Fatalf("modules.shell = %v", got)
	}
	before := dirSnapshot(t, l.Root)
	_, err := InitShellController(l, controllerAID, "", func(ShellControllerPlan) error { t.Fatal("confirm asked"); return nil })
	if k := errRefusalKey(t, err); k != "config_invalid" {
		t.Fatalf("refusal %q", k)
	}
	if after := dirSnapshot(t, l.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("a refused grant changed files")
	}
}

// Every refusal comes before anything is written, and before the confirmation.
func TestInitShellControllerRefusals(t *testing.T) {
	cases := []struct {
		name   string
		config string
		deny   string
	}{
		{"different block", `{"modules":{"shell":{"allow":["` + controllerAID + `"]}}}`, ""},
		{"block with another key", `{"modules":{"shell":{"allow":["` + controllerAID + `"],"allow_arbitrary":true,"allow_local":true}}}`, ""},
		{"block for another peer", `{"modules":{"shell":{"allow":["bafyother"],"allow_arbitrary":true}}}`, ""},
		{"shell turned off", `{"without":["shell"]}`, ""},
		{"public shell capability", `{"inbound":{"policy":"closed","public_capabilities":[{"id":"shell.exec"}]}}`, ""},
		{"denied peer", `{}`, "# blocked\n" + controllerAID + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLayout(filepath.Join(t.TempDir(), "d"))
			writeConfigJSON(t, l, tc.config)
			if tc.deny != "" {
				if err := os.WriteFile(filepath.Join(l.Root, peersDenyFile), []byte(tc.deny), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := dirSnapshot(t, l.Root)
			c := &countingConfirm{}
			_, err := InitShellController(l, controllerAID, "", c.confirm)
			if k := errRefusalKey(t, err); k != "config_invalid" {
				t.Fatalf("refusal %q (%v)", k, err)
			}
			if len(c.plans) != 0 {
				t.Fatal("confirm was asked before a refusal")
			}
			if after := dirSnapshot(t, l.Root); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused grant changed files:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
	t.Run("invalid aid", func(t *testing.T) {
		l := NewLayout(filepath.Join(t.TempDir(), "d"))
		_, err := InitShellController(l, "bafynot-an-aid", "", func(ShellControllerPlan) error { t.Fatal("confirm asked"); return nil })
		if k := errRefusalKey(t, err); k != "config_invalid" {
			t.Fatalf("refusal %q", k)
		}
		if _, err := os.Stat(l.Root); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a refused grant created the data directory")
		}
	})
}

// A declined confirmation writes nothing: neither file, and in a fresh directory not the directory.
func TestInitShellControllerDeclinedWritesNothing(t *testing.T) {
	declined := errors.New("declined")

	fresh := NewLayout(filepath.Join(t.TempDir(), "fresh"))
	if _, err := InitShellController(fresh, controllerAID, "", func(ShellControllerPlan) error { return declined }); err != declined {
		t.Fatalf("error %v, want the confirmation's own", err)
	}
	if _, err := os.Stat(fresh.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a declined grant created the data directory")
	}

	l := NewLayout(filepath.Join(t.TempDir(), "d"))
	writeConfigJSON(t, l, `{"control_addr":"127.0.0.1:39999"}`)
	if err := os.WriteFile(filepath.Join(l.Root, peersAllowFile), []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirSnapshot(t, l.Root)
	if _, err := InitShellController(l, controllerAID, "", func(ShellControllerPlan) error { return declined }); err != declined {
		t.Fatalf("error %v, want the confirmation's own", err)
	}
	if after := dirSnapshot(t, l.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("a declined grant changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// An existing peers.allow keeps its comments and lines; a peer already on peers.trust is not added to
// peers.allow, and only the block is written.
func TestInitShellControllerKeepsListsAndConfig(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "d"))
	writeConfigJSON(t, l, `{"control_addr":"127.0.0.1:39998","inbound":{"policy":"closed","public_capabilities":[{"id":"files.get"}]}}`)
	allowPath := filepath.Join(l.Root, peersAllowFile)
	if err := os.WriteFile(allowPath, []byte("# friends\nbafyfriend # a note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitShellController(l, controllerAID, "", func(ShellControllerPlan) error { return nil }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(allowPath)
	if want := "# friends\nbafyfriend # a note\n" + controllerAID + "\n"; string(b) != want {
		t.Fatalf("peers.allow = %q, want %q", b, want)
	}
	raw, _ := os.ReadFile(l.ConfigPath())
	if !strings.Contains(string(raw), `"control_addr": "127.0.0.1:39998"`) || !strings.Contains(string(raw), `"files.get"`) {
		t.Fatalf("other keys lost:\n%s", raw)
	}

	l2 := NewLayout(filepath.Join(t.TempDir(), "t"))
	writeConfigJSON(t, l2, `{}`)
	if err := os.WriteFile(filepath.Join(l2.Root, peersTrustFile), []byte(controllerAID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := InitShellController(l2, controllerAID, "", func(ShellControllerPlan) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if plan.AddAllow || plan.ModuleBlock == nil || !plan.Changed {
		t.Fatalf("plan %+v", plan)
	}
	if allow, _ := readPeerFile(filepath.Join(l2.Root, peersAllowFile)); allow[controllerAID] {
		t.Fatal("a trusted peer was added to peers.allow")
	}
}

// A daemon that holds the data directory is daemon_running, before anything is asked or written.
func TestInitShellControllerRefusesWhileADaemonRuns(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "d"))
	writeConfigJSON(t, l, `{}`)
	held, err := AcquireDataLock(l)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	before := dirSnapshot(t, l.Root)
	_, err = InitShellController(l, controllerAID, "", func(ShellControllerPlan) error { t.Fatal("confirm asked"); return nil })
	if k := errRefusalKey(t, err); k != "daemon_running" {
		t.Fatalf("refusal %q", k)
	}
	if after := dirSnapshot(t, l.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("a refused grant changed files")
	}
}

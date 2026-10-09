//go:build !no_anetlink

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
)

// serveLinkd serves a C1 anetlinkd on a socket in a private directory: health answers with status
// health, the catalogue lists a remote and a local capability.
func serveLinkd(t *testing.T, health int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "c1.sock")
	cbor := func(w http.ResponseWriter, status int, v any) {
		b, err := coredet.Marshal(v)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		if health == http.StatusUpgradeRequired {
			cbor(w, health, map[uint64]any{1: "c1_version"})
			return
		}
		w.WriteHeader(health)
	})
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		cbor(w, http.StatusOK, map[uint64]any{1: []any{
			map[uint64]any{1: "light.onoff@sim/lamp-1", 2: uint64(1)},
			map[uint64]any{1: "sensor.temperature@modbus/plc-1", 2: uint64(0)},
		}})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// anetlinkDoctor runs doctor with modules.anetlink set to block and the public capabilities given,
// and returns its anetlink.* and backends.peer_check findings.
func anetlinkDoctor(t *testing.T, block map[string]any, public ...string) map[string][]map[string]any {
	t.Helper()
	t.Setenv("LANG", "C")
	layout := freshInit(t)
	kv := map[string]any{}
	if block != nil {
		kv["modules"] = map[string]any{"anetlink": block}
	}
	var pcs []any
	for _, id := range public {
		pcs = append(pcs, map[string]any{"id": id})
	}
	if pcs != nil {
		b, err := os.ReadFile(layout.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		in, _ := m["inbound"].(map[string]any)
		if in == nil {
			in = map[string]any{}
		}
		in["public_capabilities"] = pcs
		kv["inbound"] = in
	}
	setConfigKeys(t, layout, kv)
	out, _ := doctorJSON(t, layout, testDoctorEnv(t))
	got := map[string][]map[string]any{}
	for _, raw := range out["checks"].([]any) {
		ch := raw.(map[string]any)
		if id, _ := ch["id"].(string); strings.HasPrefix(id, "anetlink.") || id == "backends.peer_check" {
			got[id] = append(got[id], ch)
		}
	}
	return got
}

// anetlink.socket and anetlink.publish (ux §5.2): ok for a C1 v1 anetlinkd the daemon may use, and a
// warning, anetlink.not_remote, for each public capability ANetLink refuses to remote callers (the
// daemon leaves it unpublished, LEC CC-5).
func TestDoctorANetLinkSocketAndPublish(t *testing.T) {
	sock := serveLinkd(t, http.StatusNoContent)
	got := anetlinkDoctor(t, map[string]any{"socket": sock}, "light.onoff@sim/lamp-1", "sensor.temperature@modbus/plc-1", "text.echo")
	if s := got["anetlink.socket"]; len(s) != 1 || s[0]["status"] != stOK || !strings.Contains(s[0]["detail"].(string), "C1 v1") {
		t.Fatalf("anetlink.socket %v", s)
	}
	p := got["anetlink.publish"]
	if len(p) != 1 || p[0]["status"] != stWarn || p[0]["reason"] != "anetlink.not_remote" ||
		!strings.Contains(p[0]["detail"].(string), "sensor.temperature@modbus/plc-1") {
		t.Fatalf("anetlink.publish %v, want one anetlink.not_remote for the sensor", p)
	}
	if pc := got["backends.peer_check"]; len(pc) != 1 || pc[0]["status"] != stOK {
		t.Fatalf("backends.peer_check %v", pc)
	}
	// Without the module, none of it.
	if got := anetlinkDoctor(t, nil); len(got) != 0 {
		t.Fatalf("no modules.anetlink: %v", got)
	}
}

// anetlink.socket fails with the reason table's codes: c1_version for an anetlinkd of another C1
// version (its own 426, or one that answers no C1 v1 at all), peer_uid_mismatch for a listener that
// is not the expected account, and a plain finding for a socket nobody listens on. peer_check
// path-only is backends.peer_check's warning, as for every other socket backend.
func TestDoctorANetLinkSocketFailures(t *testing.T) {
	for _, c := range []struct {
		name   string
		block  map[string]any
		reason string
		says   string
	}{
		{"426 c1_version", map[string]any{"socket": serveLinkd(t, http.StatusUpgradeRequired)}, "c1_version", "same release"},
		{"C1 v0", map[string]any{"socket": serveLinkd(t, http.StatusNotFound)}, "c1_version", "same release"},
		{"impostor", map[string]any{"socket": serveLinkd(t, http.StatusNoContent), "expected_user": strconv.Itoa(os.Getuid() + 1)},
			"peer_uid_mismatch", "expected_user"},
		{"nobody", map[string]any{"socket": filepath.Join(t.TempDir(), "c1.sock")}, "", "start anetlinkd"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := anetlinkDoctor(t, c.block, "light.onoff@sim/lamp-1")["anetlink.socket"]
			if len(s) != 1 || s[0]["status"] != stFail || (c.reason != "" && s[0]["reason"] != c.reason) {
				t.Fatalf("anetlink.socket %v, want a fail %s", s, c.reason)
			}
			if text := s[0]["detail"].(string) + " " + s[0]["hint"].(string); !strings.Contains(text, c.says) {
				t.Fatalf("%q does not say %q", text, c.says)
			}
		})
	}
	sock := serveLinkd(t, http.StatusNoContent)
	pc := anetlinkDoctor(t, map[string]any{"socket": sock, "peer_check": "path-only"})["backends.peer_check"]
	if len(pc) != 1 || pc[0]["status"] != stWarn || pc[0]["reason"] != "backends.path_only" ||
		!strings.Contains(pc[0]["detail"].(string), "unix:///") {
		t.Fatalf("path-only anetlink: %v", pc)
	}
}

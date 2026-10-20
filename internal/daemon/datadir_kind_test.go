package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
)

// installShFuncs returns the named functions of deploy/release/install.sh,
// as the script defines them.
func installShFuncs(t *testing.T, names ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, name := range names {
		re := regexp.MustCompile(`(?ms)^` + name + `\(\)\s*\{(?:[^\n]*\}\n|[^\n]*\n.*?^\}\n)`)
		f := re.FindString(string(b))
		if f == "" {
			t.Fatalf("install.sh has no function %s", name)
		}
		out = append(out, f)
	}
	return strings.Join(out, "\n")
}

// shellDataDirKind is what install.sh's datadir_kind says of root.
func shellDataDirKind(t *testing.T, root string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := "set -eu\n" + installShFuncs(t, "legacy_defaults", "identity_file_age", "datadir_kind") +
		"\nlegacy_defaults\ndatadir_kind \"$1\"\n"
	out, err := exec.Command(sh, "-c", script, "sh", root).CombinedOutput()
	if err != nil {
		t.Fatalf("datadir_kind %s: %v\n%s", root, err, out)
	}
	return strings.TrimSpace(string(out))
}

// v1Layout writes the files AgentNetwork 1.1.15 leaves in its data
// directory after its daemon has run once (observed on the linux-x64 build
// of the npm package @agentnetwork/anet-linux-x64@1.1.15, in a temporary
// HOME).
func v1Layout(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"api_token":          "0123456789abcdef\n",
		"agent_token":        "fedcba9876543210\n",
		"scoped_tokens.json": "{}",
		"peer-journal.json":  "{}",
		"daemon.log":         "[daemon] AgentNetwork 1.1.15\n",
		"anet/identity.json": `{"did":"did:key:z6Mk"}`,
		"cas/pebble/LOCK":    "",
	}
	files["anet"+".db"] = "SQLite format 3\x00"
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// incept writes a 0.3 identity file into root.
func incept(t *testing.T, root string) {
	t.Helper()
	self, err := identity.InceptAgent(uint64(time.Now().Add(-time.Hour).UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveIdentity(NewLayout(root), self); err != nil {
		t.Fatal(err)
	}
}

// writeV02Identity writes an identity file as anet 0.2 wrote it: {1: the
// current key's seed, 2: the next key's seed, 3: the KEL}, without a type tag.
func writeV02Identity(t *testing.T, root string) {
	t.Helper()
	b, err := coredet.Marshal(map[uint64]any{1: make([]byte, 32), 2: make([]byte, 32), 3: []byte{0x80}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "identity.kel"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestInstallShDataDirKindAgrees: the installer and the daemon call the same
// directories legacy (daemon.ClassifyDataDir and install.sh datadir_kind),
// and the daemon refuses to start on exactly the kinds the installer moves
// aside whole (identity_v02 for older and v1). A mixed directory starts: its
// 0.3 identity is used and the 1.x files are ignored, and the installer
// moves only those out.
func TestInstallShDataDirKindAgrees(t *testing.T) {
	cases := []struct {
		name   string
		make   func(t *testing.T, root string)
		want   DataDirKind
		refuse string // CheckDataDirForStart: "" or the one code
	}{
		{"missing", func(t *testing.T, root string) { _ = os.Remove(root) }, DataDirNone, ""},
		{"empty", func(t *testing.T, root string) {}, DataDirNone, ""},
		{"0.3 identity", func(t *testing.T, root string) {
			writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
			incept(t, root)
		}, DataDirCurrent, ""},
		{"0.3 init only", func(t *testing.T, root string) {
			writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
		}, DataDirCurrent, ""},
		{"0.2 identity", func(t *testing.T, root string) { writeV02Identity(t, root) }, DataDirOlder, "identity_v02"},
		{"identity tag 1", func(t *testing.T, root string) { writeTaggedIdentity(t, NewLayout(root), 1) }, DataDirOlder, "identity_v02"},
		{"identity tag 3", func(t *testing.T, root string) { writeTaggedIdentity(t, NewLayout(root), 3) }, DataDirNewer, "datadir_too_new"},
		{"identity tag 03", func(t *testing.T, root string) {
			writeIdentityTag(t, NewLayout(root), identityFileTagPrefix+"03")
		}, DataDirInvalid, "identity_file_invalid"},
		{"identity not cbor", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "identity.kel"), []byte("not an identity"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, DataDirInvalid, "identity_file_invalid"},
		{"1.x", v1Layout, DataDirV1, "identity_v02"},
		{"1.x and anet init of 0.3.4", func(t *testing.T, root string) {
			v1Layout(t, root)
			writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
			for _, f := range []string{"peers.allow", "peers.deny", "peers.trust", "payees.allow"} {
				if err := os.WriteFile(filepath.Join(root, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}, DataDirV1, "identity_v02"},
		{"1.x token only", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "anet"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "anet", "api_token"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, DataDirV1, "identity_v02"},
		{"1.x and a 0.3 identity", func(t *testing.T, root string) {
			v1Layout(t, root)
			writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
			incept(t, root)
		}, DataDirMixed, ""},
		{"1.x and a named 0.3 identity", func(t *testing.T, root string) {
			v1Layout(t, root)
			incept(t, filepath.Join(root, "ids", "work"))
		}, DataDirMixed, ""},
		{"1.x and a 0.2 identity", func(t *testing.T, root string) {
			v1Layout(t, root)
			writeV02Identity(t, root)
		}, DataDirOlder, "identity_v02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), ".anet")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			c.make(t, root)
			if got := ClassifyDataDir(root); got != c.want {
				t.Errorf("ClassifyDataDir = %s, want %s", got, c.want)
			}
			if got := shellDataDirKind(t, root); got != string(c.want) {
				t.Errorf("install.sh datadir_kind = %s, want %s", got, c.want)
			}
			if c.want == DataDirNone {
				return
			}
			err := CheckDataDirForStart(NewLayout(root))
			switch {
			case c.refuse == "" && err != nil:
				t.Errorf("CheckDataDirForStart: %v, want a start", err)
			case c.refuse != "" && !isRefusal(err, c.refuse):
				t.Errorf("CheckDataDirForStart: %v, want %s", err, c.refuse)
			}
		})
	}
}

// TestRefuseV1DataDir: a data directory of AgentNetwork 1.x is refused at
// start with identity_v02, by the check before the start and by New, and is
// left exactly as it was: the start incepts no identity among the 1.x files.
func TestRefuseV1DataDir(t *testing.T) {
	root := t.TempDir()
	v1Layout(t, root)
	refuseDataDir(t, NewLayout(root), "identity_v02")
	if _, err := os.Stat(filepath.Join(root, "identity.kel")); !os.IsNotExist(err) {
		t.Fatalf("an identity was written into the 1.x directory: %v", err)
	}
}

// TestV1MarkersMatchInstallSh: the installer tests the same 1.x markers as
// the daemon.
func TestV1MarkersMatchInstallSh(t *testing.T) {
	m := regexp.MustCompile(`(?m)^\s*V1_MARKERS='([^']*)'`).FindStringSubmatch(installShFuncs(t, "legacy_defaults"))
	if m == nil {
		t.Fatal("install.sh legacy_defaults sets no V1_MARKERS")
	}
	var want []string
	for _, v := range V1Markers {
		want = append(want, filepath.ToSlash(v))
	}
	if got := strings.Fields(m[1]); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("install.sh V1_MARKERS = %v, daemon.V1Markers = %v", got, want)
	}
}

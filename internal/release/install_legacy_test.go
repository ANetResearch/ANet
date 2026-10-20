package release

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The takeover of earlier installations by install.sh ("EARLIER
// INSTALLATIONS"): AgentNetwork 1.x from npm and as a plain binary, anet
// 0.1/0.2, the data directory, PATH, and sudo only as sudo -n. Everything
// runs in temporary HOMEs, against stand-ins for the programs involved.

var legacyFuncs = []string{"say", "warn", "die", "vercmp", "note", "run_timeout", "can_sudo", "clean_text",
	"resolve_link", "json_str", "semver_ok", "probe_version", "npm_plat", "classify_anet", "survey_anets",
	"survey_add", "legacy_label", "stop_legacy", "identity_file_age", "legacy_defaults", "datadir_kind", "answers",
	"data_busy", "describe_legacy", "move_legacy_data", "remove_npm_legacy", "replace_legacy_bins", "note_others",
	"path_expr", "path_block", "write_block", "profile_files", "exec_detached", "fresh_anet", "is_dest",
	"setup_path", "upgrade_report"}

// legacyEnv is one temporary machine: a HOME, a directory for stand-ins,
// and the environment the script runs with.
type legacyEnv struct {
	t     *testing.T
	home  string
	root  string // everything else lives below it
	bin   string // stand-ins on PATH (npm, sudo)
	log   string // every stand-in appends its argv here
	path  []string
	env   map[string]string
	shell string
}

func newLegacyEnv(t *testing.T) *legacyEnv {
	t.Helper()
	root := t.TempDir()
	e := &legacyEnv{t: t, root: root, home: filepath.Join(root, "home"), bin: filepath.Join(root, "fakebin"),
		log: filepath.Join(root, "calls.log"), env: map[string]string{}, shell: "/bin/sh"}
	for _, d := range []string{e.home, e.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.path = []string{e.bin, "/usr/bin", "/bin"}
	return e
}

// write writes an executable file.
func (e *legacyEnv) write(path, body string, mode os.FileMode) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// fakeAnet is a stand-in binary: it carries marker (the module path the
// installer looks for), prints versionLine for `version`, logs every call,
// and for any verb that would start a daemon leaves a file named STARTED
// beside itself.
func (e *legacyEnv) fakeAnet(path, marker, versionLine string) string {
	return e.write(path, "#!/bin/sh\n# "+marker+"\n"+
		"echo \"$0 $*\" >> '"+e.log+"'\n"+
		"case \"$1\" in\n"+
		"  version|--version|-v) echo '"+versionLine+"' ;;\n"+
		"  stop) echo '[stop] daemon stopped.' ;;\n"+
		"  *) touch \"$(dirname \"$0\")/STARTED\" ;;\n"+
		"esac\n", 0o755)
}

const (
	markerV1   = "github.com/ChatChatTech/AgentNetwork/anet-cli"
	markerANet = "github.com/ANetResearch/ANet/cmd/anet"
)

// npmTree is a global npm prefix holding @agentnetwork/anet at version,
// with its command link in prefix/bin; for a 1.x tree, the shim and the
// platform package beside it, as npm installs 1.1.15.
func (e *legacyEnv) npmTree(prefix, version, repo string) (link, pkg, platBin string) {
	e.t.Helper()
	pkg = filepath.Join(prefix, "lib", "node_modules", "@agentnetwork", "anet")
	e.write(filepath.Join(pkg, "package.json"), fmt.Sprintf(`{
  "name": "@agentnetwork/anet",
  "version": %q,
  "repository": {
    "type": "git",
    "url": %q
  },
  "bin": {
    "anet": "bin/anet"
  }
}`, version, repo), 0o644)
	e.write(filepath.Join(pkg, "bin", "anet"), "#!/usr/bin/env node\n// launcher\n", 0o755)
	if strings.Contains(repo, "ChatChatTech") {
		plat := filepath.Join(pkg, "node_modules", "@agentnetwork", "anet-linux-x64")
		e.write(filepath.Join(plat, "package.json"), fmt.Sprintf(`{"name": "@agentnetwork/anet-linux-x64", "version": %q}`, version), 0o644)
		platBin = e.fakeAnet(filepath.Join(plat, "bin", "anet"), markerV1, version)
	}
	link = filepath.Join(prefix, "bin", "anet")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Symlink("../lib/node_modules/@agentnetwork/anet/bin/anet", link); err != nil {
		e.t.Fatal(err)
	}
	return link, pkg, platBin
}

// fakeNpm is npm: `prefix -g` prints prefix, and `uninstall -g … --prefix P
// NAME` removes the package and its link, unless fail.
func (e *legacyEnv) fakeNpm(prefix string, fail bool) {
	body := "#!/bin/sh\necho \"npm $*\" >> '" + e.log + "'\n" +
		"case \"$1\" in\n  prefix) echo '" + prefix + "' ;;\n  uninstall)\n"
	if fail {
		body += "    echo 'npm ERR! code EACCES' >&2; exit 243 ;;\n"
	} else {
		body += "    rm -rf '" + prefix + "/lib/node_modules/@agentnetwork/anet' '" + prefix + "/bin/anet' ;;\n"
	}
	body += "esac\n"
	e.write(filepath.Join(e.bin, "npm"), body, 0o755)
}

// fakeSudo is sudo that needs a password: every call is logged and fails,
// as `sudo -n` fails without a cached credential.
func (e *legacyEnv) fakeSudo() {
	e.write(filepath.Join(e.bin, "sudo"), "#!/bin/sh\necho \"sudo $*\" >> '"+e.log+"'\necho 'sudo: a password is required' >&2\nexit 1\n", 0o755)
}

func (e *legacyEnv) calls() string {
	b, _ := os.ReadFile(e.log)
	return string(b)
}

// run runs body after install.sh's functions of this part, in this
// machine's environment, with DEST = ~/.local/bin/anet.
func (e *legacyEnv) run(body string) (string, int) {
	e.t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		e.t.Skip("no sh")
	}
	script := "set -eu\n" + installFuncs(e.t, legacyFuncs...) + `
TMP=$(mktemp -d); OS_TAG=linux; ARCH_TAG=amd64
legacy_defaults; : > "$TMP/notes"; : > "$TMP/anets"
V1_API=${TEST_V1_API:-127.0.0.1:1}
PREFIX="$HOME/.local/bin"; DEST="$PREFIX/anet"; SUDO=""; MODIFY_PATH=${TEST_MODIFY_PATH:-1}; WITHOUT=""
` + body + "\n"
	cmd := exec.Command(sh, "-c", script)
	env := []string{"HOME=" + e.home, "PATH=" + strings.Join(e.path, ":"), "SHELL=" + e.shell,
		"TMPDIR=" + e.t.TempDir(), "USER=tester", "LOGNAME=tester"}
	for k, v := range e.env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func mustContain(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, s)
		}
	}
}

func mustNotContain(t *testing.T, what, s string, bad ...string) {
	t.Helper()
	for _, w := range bad {
		if strings.Contains(s, w) {
			t.Errorf("%s has %q:\n%s", what, w, s)
		}
	}
}

// An npm installation of AgentNetwork 1.1.15 — the shim of @agentnetwork/anet
// and the binary of @agentnetwork/anet-linux-x64 — is found through npm's
// global prefix and PATH, its daemon is stopped with that binary's `stop`,
// and the package is removed with npm under the prefix it is installed in.
func TestInstallShLegacyNpm(t *testing.T) {
	e := newLegacyEnv(t)
	prefix := filepath.Join(e.root, "npm-global")
	link, pkg, platBin := e.npmTree(prefix, "1.1.15", "https://github.com/ChatChatTech/AgentNetwork")
	e.fakeNpm(prefix, false)
	e.path = append([]string{filepath.Join(prefix, "bin")}, e.path...)

	out, code := e.run(`survey_anets; cat "$TMP/anets"; stop_legacy; remove_npm_legacy; setup_path; upgrade_report`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, "survey", out, "npm-legacy|v1|1.1.15|"+link+"|", "|@agentnetwork/anet|"+prefix+"|"+platBin+"|")
	calls := e.calls()
	mustContain(t, "calls", calls, platBin+" stop\n",
		"npm uninstall -g --no-fund --no-audit --loglevel=error --prefix "+prefix+" @agentnetwork/anet\n")
	mustNotContain(t, "calls", calls, "--version", "/bin/anet version") // the shim's version is read, not run
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the npm command link is still there: %v", err)
	}
	if _, err := os.Stat(pkg); !os.IsNotExist(err) {
		t.Errorf("the npm package is still there: %v", err)
	}
	mustContain(t, "report", out, "Upgraded from anet 1.1.15 (npm)", "Stopped:", "the daemon of AgentNetwork 1.1.15",
		"Removed:", "npm package @agentnetwork/anet 1.1.15 ("+link+")")
	if _, err := os.Stat(filepath.Join(filepath.Dir(platBin), "STARTED")); err == nil {
		t.Error("a verb that starts a daemon was run")
	}
}

// When npm cannot remove the package, the command link into it is removed
// and the npm command is printed.
func TestInstallShLegacyNpmFails(t *testing.T) {
	e := newLegacyEnv(t)
	prefix := filepath.Join(e.root, "npm-global")
	link, pkg, _ := e.npmTree(prefix, "1.1.15", "https://github.com/ChatChatTech/AgentNetwork")
	e.fakeNpm(prefix, true)
	e.path = append([]string{filepath.Join(prefix, "bin")}, e.path...)
	out, code := e.run(`survey_anets; remove_npm_legacy; upgrade_report`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the command link was left: %v", err)
	}
	if _, err := os.Stat(pkg); err != nil {
		t.Errorf("the package files were removed without npm: %v", err)
	}
	mustContain(t, "report", out, "Needs a command from you:",
		"remove them with: npm uninstall -g --prefix "+prefix+" @agentnetwork/anet")
}

// Plain binaries: AgentNetwork 1.x and anet 0.2 are stopped with their own
// verbs and replaced by a link to the new binary; anet 0.3, the 0.3 npm
// launcher and a file that is not anet are left alone, and the last is
// never run.
func TestInstallShLegacyBinaries(t *testing.T) {
	e := newLegacyEnv(t)
	v1 := e.fakeAnet(filepath.Join(e.root, "opt1", "anet"), markerV1, "1.1.15")
	v0 := e.fakeAnet(filepath.Join(e.root, "opt0", "anet"), markerANet, "anet 0.2.1 (commit 0123456, built 2026-08-01T00:00:00Z)")
	cur := e.fakeAnet(filepath.Join(e.root, "opt3", "anet"), markerANet, "anet 0.3.3 (commit 0123456, built 2026-10-17T00:00:00Z)")
	other := e.write(filepath.Join(e.root, "other", "anet"), "#!/bin/sh\ntouch '"+e.root+"/other-ran'\n", 0o755)
	prefix := filepath.Join(e.root, "npm-global")
	launcher, _, _ := e.npmTree(prefix, "0.3.4", "git+https://github.com/ANetResearch/ANet.git")
	e.fakeNpm(prefix, false)
	e.path = append([]string{filepath.Dir(other), filepath.Dir(v1), filepath.Dir(v0), filepath.Dir(cur), filepath.Join(prefix, "bin")}, e.path...)

	out, code := e.run(`survey_anets; cat "$TMP/anets"; stop_legacy; remove_npm_legacy
mkdir -p "$PREFIX"; printf '#!/bin/sh\necho new\n' > "$DEST"; chmod 755 "$DEST"
replace_legacy_bins; note_others; upgrade_report`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, "survey", out, "bin-legacy|v1|1.1.15|"+v1+"|", "bin-legacy|v0|0.2.1|"+v0+"|",
		"bin-current||0.3.3|"+cur+"|", "other|||"+other+"|", "npm-current||0.3.4|"+launcher+"|")
	calls := e.calls()
	mustContain(t, "calls", calls, v1+" stop\n", v0+" stop --all\n")
	mustNotContain(t, "calls", calls, cur+" stop", "npm uninstall")
	if _, err := os.Stat(filepath.Join(e.root, "other-ran")); err == nil {
		t.Error("a file named anet that is not anet was run")
	}
	for _, p := range []string{v1, v0} {
		if got, err := os.Readlink(p); err != nil || got != filepath.Join(e.home, ".local", "bin", "anet") {
			t.Errorf("%s: link %q, %v; want a link to the new binary", p, got, err)
		}
	}
	for _, p := range []string{cur, other} {
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s was changed", p)
		}
	}
	if _, err := os.Lstat(launcher); err != nil {
		t.Errorf("the 0.3 npm launcher was removed: %v", err)
	}
	mustContain(t, "report", out, "Upgraded from anet 1.1.15, 0.2.1", "Replaced:", v1+" (AgentNetwork 1.1.15) → a link to",
		v0+" (anet 0.2.1) → a link to", "Left in place:", cur+": anet 0.3.3.",
		launcher+": the npm launcher @agentnetwork/anet 0.3.4", other+": not an AgentNetwork binary")
}

// The npm launcher of 0.3 alone is not an earlier release: nothing is
// stopped or removed, and it is named in the report.
func TestInstallShLeavesNpmLauncher(t *testing.T) {
	e := newLegacyEnv(t)
	prefix := filepath.Join(e.root, "npm-global")
	link, pkg, _ := e.npmTree(prefix, "0.3.4", "git+https://github.com/ANetResearch/ANet.git")
	e.fakeNpm(prefix, false)
	e.path = append([]string{filepath.Join(prefix, "bin")}, e.path...)
	out, code := e.run(`survey_anets; stop_legacy; move_legacy_data; remove_npm_legacy; replace_legacy_bins; note_others; cat "$TMP/notes"`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if calls := e.calls(); calls != "npm prefix -g\n" {
		t.Errorf("calls: %q, want npm prefix -g alone", calls)
	}
	for _, p := range []string{link, pkg} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	mustContain(t, "notes", out, "kept|"+link+": the npm launcher @agentnetwork/anet 0.3.4, which runs its own release.")
}

// sudo is only ever sudo -n: a binary in a directory only root can write,
// and an npm prefix that is not the user's, are left with the command that
// removes them when sudo needs a password, and nothing waits for one.
func TestInstallShSudoNonInteractive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory")
	}
	e := newLegacyEnv(t)
	e.fakeSudo()
	v1 := e.fakeAnet(filepath.Join(e.root, "sys", "anet"), markerV1, "1.1.15")
	prefix := filepath.Join(e.root, "npm-root")
	link, _, _ := e.npmTree(prefix, "1.1.15", "https://github.com/ChatChatTech/AgentNetwork")
	e.fakeNpm(prefix, false)
	ro := []string{filepath.Dir(v1), filepath.Join(prefix, "bin"), filepath.Join(prefix, "lib", "node_modules", "@agentnetwork")}
	for _, d := range ro {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range ro {
			_ = os.Chmod(d, 0o755)
		}
	})
	e.path = append([]string{filepath.Dir(v1), filepath.Join(prefix, "bin")}, e.path...)
	start := time.Now()
	out, code := e.run(`survey_anets; remove_npm_legacy
mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"
replace_legacy_bins; echo "LEGACY_LEFT=$LEGACY_LEFT"; upgrade_report`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("took %v", d)
	}
	for _, l := range strings.Split(strings.TrimSpace(e.calls()), "\n") {
		if strings.HasPrefix(l, "sudo ") && !strings.HasPrefix(l, "sudo -n ") {
			t.Errorf("sudo without -n: %q", l)
		}
		if strings.HasPrefix(l, "npm uninstall") {
			t.Errorf("npm ran without the rights to remove the package: %q", l)
		}
	}
	mustContain(t, "calls", e.calls(), "sudo -n true\n")
	for _, p := range []string{v1, link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	mustContain(t, "report", out, "LEGACY_LEFT=1", "Needs a command from you:",
		v1+" is AgentNetwork 1.1.15 and needs root to remove; run: sudo rm '"+v1+"'",
		"remove them with: sudo npm uninstall -g --prefix "+prefix+" @agentnetwork/anet")
}

// Every sudo in install.sh is sudo -n (owner rule: no prompt, ever).
func TestInstallShSudoIsNonInteractive(t *testing.T) {
	script := repoFile(t, "deploy/release/install.sh")
	re := regexp.MustCompile(`(^|[^-\w])sudo( +[^ ]+)?`)
	for i, l := range strings.Split(script, "\n") {
		code := l
		if j := strings.Index(code, "#"); j >= 0 && !strings.Contains(code[:j], "'") && !strings.Contains(code[:j], `"`) {
			code = code[:j]
		}
		for _, m := range re.FindAllStringSubmatch(code, -1) {
			arg := strings.TrimSpace(m[2])
			switch {
			case strings.HasPrefix(arg, "-n"), arg == ">/dev/null":
			case regexp.MustCompile(`apt-get install|dnf install|apk add`).MatchString(code):
				// the message that names the package with ssh-keygen
			case strings.Contains(code, "command -v sudo"):
			case strings.Contains(code, "say ") || strings.Contains(code, "note ") || strings.Contains(code, "die ") ||
				strings.Contains(code, "_show=") || strings.Contains(l, "\\`sudo -v\\`") || strings.HasPrefix(strings.TrimSpace(code), "Run "):
				// a message that names a command for the user
			default:
				t.Errorf("install.sh:%d runs sudo without -n: %s", i+1, strings.TrimSpace(l))
			}
		}
	}
}

// The data directory of an earlier release is renamed aside, never deleted;
// a mixed one keeps its 0.3 identity and loses only the 1.x entries; a 0.3
// one and a newer one are not touched; ANET_DATA_DIR names the directory.
func TestInstallShMovesLegacyData(t *testing.T) {
	tagged := append([]byte{0xa1, 0x00, 0x6f}, "anet.identity/2"...)
	v02 := []byte{0xa3, 0x01, 0x40, 0x02, 0x40, 0x03, 0x40}
	newer := append([]byte{0xa1, 0x00, 0x6f}, "anet.identity/3"...)
	v1Files := map[string]string{"api_token": "t", "agent_token": "t", "anet/identity.json": "{}", "anet/api_token": "t",
		"orgs/root.key": "k", "state/last.json": "{}", "daemon.log": "1.x log\n"}
	v1Files["anet"+".db"] = "db"
	cases := []struct {
		name    string
		files   map[string]string
		ident   []byte
		dataDir string // ANET_DATA_DIR, relative to HOME
		want    string // whole, part, none
	}{
		{"1.x", v1Files, nil, "", "whole"},
		{"1.x with anet init of 0.3.4", merge(v1Files, map[string]string{"config.json": "{}", "peers.allow": ""}), nil, "", "whole"},
		{"1.x with a 0.3 identity", merge(v1Files, map[string]string{"config.json": "{}"}), tagged, "", "part"},
		{"0.2", map[string]string{"config.json": "{}"}, v02, "", "whole"},
		{"0.3", map[string]string{"config.json": "{}"}, tagged, "", "none"},
		{"newer", map[string]string{"config.json": "{}"}, newer, "", "none"},
		{"1.x in ANET_DATA_DIR", v1Files, nil, "data/node", "whole"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLegacyEnv(t)
			dir := filepath.Join(e.home, ".anet")
			if c.dataDir != "" {
				dir = filepath.Join(e.home, c.dataDir)
				e.env["ANET_DATA_DIR"] = dir
				// ~/.anet is not the directory 0.3 uses then: left alone.
				e.write(filepath.Join(e.home, ".anet", "api_token"), "t", 0o600)
			}
			for name, body := range c.files {
				e.write(filepath.Join(dir, filepath.FromSlash(name)), body, 0o600)
			}
			if c.ident != nil {
				e.write(filepath.Join(dir, "identity.kel"), string(c.ident), 0o600)
			}
			e.write(filepath.Join(e.root, "npm-global", "x"), "", 0o644)
			out, code := e.run(`move_legacy_data; echo "KIND=$DATA_KIND HELD=$DATA_HELD"; upgrade_report`)
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			moved, _ := filepath.Glob(dir + "-legacy-*")
			switch c.want {
			case "none":
				if len(moved) != 0 {
					t.Fatalf("moved %v:\n%s", moved, out)
				}
				if _, err := os.Stat(filepath.Join(dir, "identity.kel")); err != nil {
					t.Fatal(err)
				}
				return
			case "whole":
				if len(moved) != 1 {
					t.Fatalf("moved %v:\n%s", moved, out)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("%s is still there: %v", dir, err)
				}
				for name := range c.files {
					if _, err := os.Stat(filepath.Join(moved[0], filepath.FromSlash(name))); err != nil {
						t.Errorf("not moved along: %s", name)
					}
				}
				mustContain(t, "report", out, "Moved aside:", dir+" → "+moved[0], "Nothing was deleted")
			case "part":
				if len(moved) != 1 {
					t.Fatalf("moved %v:\n%s", moved, out)
				}
				for _, keep := range []string{"identity.kel", "config.json", "daemon.log"} {
					if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
						t.Errorf("%s left the 0.3 directory: %v", keep, err)
					}
				}
				for _, gone := range []string{"api_token", "agent_token", "anet", "anet" + ".db", "orgs", "state"} {
					if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
						t.Errorf("%s stayed in the 0.3 directory", gone)
					}
					if _, err := os.Stat(filepath.Join(moved[0], gone)); err != nil {
						t.Errorf("%s did not arrive: %v", gone, err)
					}
				}
				mustContain(t, "report", out, "the anet 0.3 identity in "+dir+" stays where it is")
			}
			if !regexp.MustCompile(`-legacy-(1\.x|0\.2)-\d{8}T\d{6}Z$`).MatchString(moved[0]) {
				t.Errorf("name %s", moved[0])
			}
			mustContain(t, "report", out, "identity: ")
			if c.dataDir != "" {
				if _, err := os.Stat(filepath.Join(e.home, ".anet", "api_token")); err != nil {
					t.Errorf("~/.anet was touched although ANET_DATA_DIR names another directory: %v", err)
				}
			}
		})
	}
}

func merge(a, b map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}

// A 1.x directory a running daemon may still hold is not moved: its
// daemon.pid names a live process called anet, or 1.x's address answers.
// The report gives the command.
func TestInstallShKeepsBusyLegacyData(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	t.Run("daemon.pid", func(t *testing.T) {
		e := newLegacyEnv(t)
		dir := filepath.Join(e.home, ".anet")
		e.write(filepath.Join(dir, "api_token"), "t", 0o600)
		// A process named anet that this test starts and ends by its PID.
		b, err := os.ReadFile(sleep)
		if err != nil {
			t.Skip(err)
		}
		fake := e.write(filepath.Join(e.root, "proc", "anet"), string(b), 0o755)
		p := exec.Command(fake, "60")
		if err := p.Start(); err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { _ = p.Process.Kill(); _ = p.Wait() })
		e.write(filepath.Join(dir, "daemon.pid"), fmt.Sprint(p.Process.Pid), 0o600)
		out, code := e.run(`move_legacy_data; echo "HELD=$DATA_HELD"; upgrade_report`)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if moved, _ := filepath.Glob(dir + "-legacy-*"); len(moved) != 0 {
			t.Fatalf("moved while held: %v", moved)
		}
		mustContain(t, "report", out, "HELD=1", "that a running daemon may still use, so it was not moved",
			"then: mv '"+dir+"' '"+dir+"-legacy-1.x-")
	})
	t.Run("1.x address answers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		e := newLegacyEnv(t)
		dir := filepath.Join(e.home, ".anet")
		e.write(filepath.Join(dir, "anet", "identity.json"), "{}", 0o600)
		e.env["TEST_V1_API"] = strings.TrimPrefix(srv.URL, "http://")
		out, _ := e.run(`move_legacy_data; echo "HELD=$DATA_HELD"`)
		mustContain(t, "output", out, "HELD=1")
		if _, err := os.Stat(dir); err != nil {
			t.Fatal(err)
		}
	})
}

// The PATH block: once per file, last in it, the rest of the file kept, in
// the files of the login shell (zsh with ZDOTDIR, bash, fish, sh), and a
// fresh login shell then runs the new anet.
func TestInstallShPathBlock(t *testing.T) {
	block := func(expr string) string {
		return "# >>> anet installer: PATH >>>\n" +
			"# Puts the directory of anet first on PATH. Written by the anet installer;\n" +
			"# to manage PATH yourself, delete this block and install with --no-modify-path.\n" +
			`case "$PATH" in "` + expr + `"|"` + expr + `":*) ;; *) export PATH="` + expr + `:$PATH" ;; esac` + "\n" +
			"# <<< anet installer: PATH <<<\n"
	}
	cases := []struct {
		name    string
		shell   string
		pre     map[string]string // files before, relative to HOME
		zdotdir string
		files   []string // files that get the block, relative to HOME
		absent  []string
	}{
		{"zsh", "/bin/zsh", nil, "", []string{".zprofile", ".zshrc"}, []string{".bashrc", ".profile"}},
		{"zsh with ZDOTDIR", "/bin/zsh", nil, "zdot", []string{"zdot/.zprofile", "zdot/.zshrc"}, []string{".zshrc", ".zprofile"}},
		{"bash", "/bin/bash", nil, "", []string{".profile", ".bashrc"}, []string{".bash_profile", ".zshrc"}},
		{"bash with .bash_profile", "/bin/bash", map[string]string{".bash_profile": "export FOO=1\n"}, "",
			[]string{".bash_profile", ".bashrc"}, []string{".profile"}},
		{"fish", "/usr/bin/fish", nil, "", []string{".config/fish/conf.d/anet.fish"}, []string{".profile"}},
		{"sh", "/bin/sh", nil, "", []string{".profile"}, []string{".bashrc"}},
		{"zsh, and the bashrc there is", "/bin/zsh", map[string]string{".bashrc": "alias ll='ls -l'\n"}, "",
			[]string{".zprofile", ".zshrc", ".bashrc"}, []string{".profile"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLegacyEnv(t)
			e.shell = c.shell
			for f, body := range c.pre {
				e.write(filepath.Join(e.home, f), body, 0o644)
			}
			if c.zdotdir != "" {
				e.env["ZDOTDIR"] = filepath.Join(e.home, c.zdotdir)
			}
			for i := 0; i < 2; i++ { // the second run changes nothing
				out, code := e.run(`mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"; setup_path; echo "CHECK=$PATH_CHECK"; cat "$TMP/notes"`)
				if code != 0 {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				if i == 1 && strings.Contains(out, "added the anet PATH block") {
					t.Errorf("the second run wrote again:\n%s", out)
				}
			}
			for _, f := range c.files {
				b, err := os.ReadFile(filepath.Join(e.home, f))
				if err != nil {
					t.Fatal(err)
				}
				s := string(b)
				if n := strings.Count(s, "# >>> anet installer: PATH >>>"); n != 1 {
					t.Errorf("%s has %d blocks:\n%s", f, n, s)
				}
				if strings.HasSuffix(f, ".fish") {
					mustContain(t, f, s, `if test "$PATH[1]" != "$HOME/.local/bin"`, `set -gx PATH "$HOME/.local/bin" $PATH`)
				} else if !strings.HasSuffix(s, block("$HOME/.local/bin")) {
					t.Errorf("%s does not end with the block:\n%s", f, s)
				}
				if pre, ok := c.pre[f]; ok && !strings.HasPrefix(s, pre) {
					t.Errorf("%s lost its content:\n%s", f, s)
				}
			}
			for _, f := range c.absent {
				if _, err := os.Stat(filepath.Join(e.home, f)); err == nil {
					t.Errorf("%s was written", f)
				}
			}
		})
	}
}

// A fresh login shell resolves `anet` to the new binary once the block is
// written, even when an earlier anet comes first on the PATH it starts
// from; this is what the installer reports in one line.
func TestInstallShPathVerified(t *testing.T) {
	for _, sh := range []string{"/bin/bash", "/bin/zsh", "/bin/dash"} {
		t.Run(filepath.Base(sh), func(t *testing.T) {
			if _, err := os.Stat(sh); err != nil {
				t.Skip(err)
			}
			e := newLegacyEnv(t)
			e.shell = sh
			// An earlier anet in a profile-added directory ahead of PATH.
			old := e.fakeAnet(filepath.Join(e.home, "old", "anet"), "no marker", "x")
			e.write(filepath.Join(e.home, ".profile"), "PATH=\"$HOME/old:$PATH\"\n", 0o644)
			e.write(filepath.Join(e.home, ".zprofile"), "PATH=\"$HOME/old:$PATH\"\n", 0o644)
			out, code := e.run(`mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"
before=$(fresh_anet); echo "BEFORE=$before"; setup_path; echo "CHECK=$PATH_CHECK"; echo "NOW=$PATH_NOW"`)
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			dest := filepath.Join(e.home, ".local", "bin", "anet")
			if sh != "/bin/dash" {
				mustContain(t, "output", out, "BEFORE="+old+"\n")
			}
			mustContain(t, "output", out, "CHECK=New terminals: `anet` is "+dest+" ("+filepath.Base(sh)+" -lic 'command -v anet').",
				`NOW=export PATH="`+filepath.Join(e.home, ".local", "bin")+`:$PATH"`)
		})
	}
}

// write_block moves an earlier block to the end, keeps everything else, and
// leaves a file with an unterminated block alone.
func TestInstallShWriteBlock(t *testing.T) {
	e := newLegacyEnv(t)
	rc := filepath.Join(e.home, ".bashrc")
	e.write(rc, "a=1\n\n# >>> anet installer: PATH >>>\nold\n# <<< anet installer: PATH <<<\nexport PATH=/x:$PATH\n", 0o644)
	out, _ := e.run(`write_block "$HOME/.bashrc" sh; write_block "$HOME/.bashrc" sh`)
	if out != "written\nunchanged\n" {
		t.Errorf("write_block: %q", out)
	}
	b, _ := os.ReadFile(rc)
	s := string(b)
	if !strings.HasPrefix(s, "a=1\n\nexport PATH=/x:$PATH\n\n# >>> anet installer: PATH >>>\n") || strings.Contains(s, "old\n") ||
		!strings.HasSuffix(s, "# <<< anet installer: PATH <<<\n") {
		t.Errorf("rewritten as:\n%s", s)
	}
	broken := "x=1\n# >>> anet installer: PATH >>>\nno end\n"
	e.write(rc, broken, 0o644)
	out, _ = e.run(`write_block "$HOME/.bashrc" sh`)
	mustContain(t, "write_block", out, "unterminated")
	if b, _ := os.ReadFile(rc); string(b) != broken {
		t.Errorf("a file with an unterminated block was changed:\n%s", b)
	}
	// A profile that is a link (a dotfiles repository) stays a link.
	target := e.write(filepath.Join(e.root, "dotfiles", "zshrc"), "z=1\n", 0o644)
	zrc := filepath.Join(e.home, ".zshrc")
	if err := os.Symlink(target, zrc); err != nil {
		t.Fatal(err)
	}
	e.run(`write_block "$HOME/.zshrc" sh`)
	if fi, err := os.Lstat(zrc); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("~/.zshrc is no longer a link")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "anet installer") {
		t.Errorf("the block did not reach the linked file")
	}
}

// --no-modify-path: no profile is written, and the command is printed.
func TestInstallShNoModifyPath(t *testing.T) {
	out, _, code := runInstallSnippet(t, []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "check_without"},
		"parse_args --no-modify-path; echo \"M=$MODIFY_PATH\"; parse_args; echo \"M=$MODIFY_PATH\"; usage")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, "parse_args", out, "M=0\nM=1\n", "--no-modify-path")

	e := newLegacyEnv(t)
	e.shell = "/bin/bash"
	e.env["TEST_MODIFY_PATH"] = "0"
	out, code = e.run(`mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"; setup_path; upgrade_report`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, f := range []string{".profile", ".bashrc", ".bash_profile", ".zshrc"} {
		if _, err := os.Stat(filepath.Join(e.home, f)); err == nil {
			t.Errorf("%s was written with --no-modify-path", f)
		}
	}
	mustContain(t, "report", out, "PATH was not changed (--no-modify-path)", `export PATH="`+filepath.Join(e.home, ".local", "bin")+`:$PATH"`)
}

// run_timeout ends what it started after the limit, by its PID, with 124,
// and passes the status through otherwise.
func TestInstallShRunTimeout(t *testing.T) {
	e := newLegacyEnv(t)
	start := time.Now()
	out, _ := e.run(`rc=0; run_timeout 1 sleep 30 || rc=$?; echo "slow=$rc"
rc=0; run_timeout 5 sh -c 'exit 3' || rc=$?; echo "fail=$rc"
rc=0; run_timeout 5 true || rc=$?; echo "ok=$rc"`)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	if out != "slow=124\nfail=3\nok=0\n" {
		t.Errorf("run_timeout: %q", out)
	}
}

// An earlier release at the destination is replaced although its number is
// higher than the release on offer (AgentNetwork counts from 1).
func TestInstallShReplacesLegacyDest(t *testing.T) {
	e := newLegacyEnv(t)
	dest := e.fakeAnet(filepath.Join(e.home, ".local", "bin", "anet"), markerV1, "anet 1.2.0 (commit x, built y)")
	out, code := e.run(`VERSION=0.3.5; BASE=https://example.invalid; check_downgrade`)
	_ = dest
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, "output", out, "is an earlier release (1.2.0); it will be replaced")
}

func init() {
	legacyFuncs = append(legacyFuncs, "check_downgrade")
}

//go:build !windows

package main

// The tests of anet uninstall run the command's core (uninstaller) on a
// temporary home with a fake installation: a stand-in binary, its release
// record, links, shell profiles and the fish file as install.sh writes
// them. Every directory the command looks at is given to it; the real
// home, profiles, PATH and daemons are never reached. runUninstall itself,
// which would act on this test binary, is called only where it refuses
// before it looks at anything.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// The blocks install.sh writes (path_block), for $HOME/.local/bin.
const (
	shBlock = pathBlockBegin + "\n" +
		"# Puts the directory of anet first on PATH. Written by the anet installer;\n" +
		"# to manage PATH yourself, delete this block and install with --no-modify-path.\n" +
		`case "$PATH" in "$HOME/.local/bin"|"$HOME/.local/bin":*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac` + "\n" +
		pathBlockEnd + "\n"
	fishBlock = pathBlockBegin + "\n" +
		"# Puts the directory of anet first on PATH. Written by the anet installer;\n" +
		"# to manage PATH yourself, delete this block and install with --no-modify-path.\n" +
		`if test "$PATH[1]" != "$HOME/.local/bin"` + "\n" + `    set -gx PATH "$HOME/.local/bin" $PATH` + "\nend\n" +
		pathBlockEnd + "\n"
)

// uninstallFixture is a temporary machine with anet installed by install.sh.
type uninstallFixture struct {
	t                      *testing.T
	root, home, exe        string
	usrLocal, legacy, path string
	other                  string // an anet of someone else, which a link in ~/bin leads to
	env                    map[string]string
	stopped                int
	runningLeft            []string
	confirmed              []string // the actions asked on the terminal
	confirmErr             error
	tty                    bool
}

func newUninstallFixture(t *testing.T) *uninstallFixture {
	t.Helper()
	root := t.TempDir()
	f := &uninstallFixture{t: t, root: root, home: filepath.Join(root, "home"), tty: true}
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("ANET_HOME", filepath.Join(f.home, ".anet"))
	guardInProcess(t)
	bin := filepath.Join(f.home, ".local", "bin")
	f.exe = filepath.Join(bin, "anet")
	f.usrLocal = filepath.Join(root, "usr-local-bin")
	f.legacy = filepath.Join(root, "legacy-bin")
	f.other = filepath.Join(root, "other", "anet")
	f.write(f.exe, "#!/bin/sh\necho anet\n")
	f.write(f.exe+".release.json", "{}\n")
	f.write(f.exe+".release.json.sig", "-----BEGIN SSH SIGNATURE-----\n")
	f.write(f.other, "#!/bin/sh\necho other\n")
	f.write(filepath.Join(f.home, ".anet", "identity.kel"), "key\n")
	for _, d := range []string{f.usrLocal, f.legacy, filepath.Join(f.home, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.link(f.exe, filepath.Join(f.usrLocal, "anet"))      // the command link
	f.link(f.exe, filepath.Join(f.legacy, "anet"))        // an earlier release, replaced by a link
	f.link(f.other, filepath.Join(f.home, "bin", "anet")) // somebody else's: kept
	f.path = strings.Join([]string{bin, f.legacy, "/usr/bin", "relative/bin"}, string(os.PathListSeparator))
	f.env = map[string]string{"PATH": f.path}
	return f
}

func (f *uninstallFixture) write(path, text string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *uninstallFixture) link(target, link string) {
	f.t.Helper()
	if err := os.Symlink(target, link); err != nil {
		f.t.Fatal(err)
	}
}

func (f *uninstallFixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// uninstaller is the command's core on the fixture: identities "default"
// and "coder", a node running until stopAll, Claude Code wired.
func (f *uninstallFixture) uninstaller(out *bytes.Buffer) *uninstaller {
	running := []string{"default"}
	return &uninstaller{
		exe: f.exe, variant: "default", home: f.home, goos: "linux",
		getenv:   func(k string) string { return f.env[k] },
		linkDirs: []string{f.usrLocal, "/nonexistent/opt-homebrew-bin", filepath.Join(f.home, "bin")},
		anetHome: filepath.Join(f.home, ".anet"),
		identities: func() []daemon.IdentityInfo {
			return []daemon.IdentityInfo{
				{Name: "coder", AID: "bafyreicoderaid", DataDir: filepath.Join(f.home, ".anet", "ids", "coder")},
				{Name: "default", AID: "bafyreidefaultaid", DataDir: filepath.Join(f.home, ".anet")},
			}
		},
		stopAll: func() error { f.stopped++; running = f.runningLeft; return nil },
		running: func() []string { return running },
		wired: func() []wiredAgent {
			return []wiredAgent{{Tool: "claude", Config: filepath.Join(f.home, ".claude.json")}}
		},
		haveTTY: func() bool { return f.tty },
		confirm: func(action, explanation string) error {
			f.confirmed = append(f.confirmed, action+": "+explanation)
			return f.confirmErr
		},
		removeExe: removeInstalledExe,
		platform:  platformUninstall,
		out:       out,
		wait:      time.Second,
	}
}

// A run removes the binary, its record, the links that lead to it, the
// PATH blocks for its directory (the rest of each profile as it was) and
// the fish file; it keeps the data directory, a link to another anet, a
// block for another directory, a profile whose block is not terminated and
// the coding agent's configuration, and says so.
func TestUninstallRemovesWhatTheInstallerMade(t *testing.T) {
	f := newUninstallFixture(t)
	f.write(filepath.Join(f.home, ".bashrc"), "export A=1\n\n"+shBlock+"alias ll='ls -l'\n")
	f.write(filepath.Join(f.home, ".profile"), "umask 022\n\n"+shBlock)
	otherBlock := strings.ReplaceAll(shBlock, "$HOME/.local/bin", "/opt/anet/bin")
	f.write(filepath.Join(f.home, ".zshrc"), "setopt x\n\n"+otherBlock)
	unterminated := "PS1=x\n" + pathBlockBegin + "\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"
	f.write(filepath.Join(f.home, ".zprofile"), unterminated)
	fish := filepath.Join(f.home, ".config", "fish", "conf.d", "anet.fish")
	f.write(fish, fishBlock)
	// A profile kept in a dotfiles repository is a link to it, and stays one.
	f.write(filepath.Join(f.root, "dotfiles", "bash_profile"), "source ~/.bashrc\n\n"+shBlock)
	f.link(filepath.Join(f.root, "dotfiles", "bash_profile"), filepath.Join(f.home, ".bash_profile"))

	var out bytes.Buffer
	u := f.uninstaller(&out)
	u.yes = true
	var err error
	_, stderr := captureCLI(t, func() { err = u.run() })
	if err != nil {
		t.Fatalf("run: %v\n%s\n%s", err, out.String(), stderr)
	}
	for _, p := range []string{f.exe, f.exe + ".release.json", f.exe + ".release.json.sig",
		filepath.Join(f.usrLocal, "anet"), filepath.Join(f.legacy, "anet"), fish} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if got := f.read(filepath.Join(f.home, ".bashrc")); got != "export A=1\nalias ll='ls -l'\n" {
		t.Errorf(".bashrc: %q", got)
	}
	if got := f.read(filepath.Join(f.home, ".profile")); got != "umask 022\n" {
		t.Errorf(".profile: %q", got)
	}
	if got := f.read(filepath.Join(f.home, ".zshrc")); got != "setopt x\n\n"+otherBlock {
		t.Errorf("a block for another directory was changed: %q", got)
	}
	if got := f.read(filepath.Join(f.home, ".zprofile")); got != unterminated {
		t.Errorf("an unterminated block was changed: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(f.home, ".bash_profile")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf(".bash_profile is no longer a link: %v", err)
	}
	if got := f.read(filepath.Join(f.root, "dotfiles", "bash_profile")); got != "source ~/.bashrc\n" {
		t.Errorf("the dotfiles profile: %q", got)
	}
	if target, err := os.Readlink(filepath.Join(f.home, "bin", "anet")); err != nil || target != f.other {
		t.Errorf("the link to another anet was changed: %q %v", target, err)
	}
	if !exists(f.other) || !exists(filepath.Join(f.home, ".anet", "identity.kel")) {
		t.Error("another anet, or the data directory, was removed")
	}
	if f.stopped != 1 {
		t.Errorf("the daemons were stopped %d times", f.stopped)
	}
	if len(f.confirmed) != 0 {
		t.Errorf("--yes asked: %q", f.confirmed)
	}
	text := out.String()
	mustHave(t, text,
		"anet uninstall removes:\n",
		"Removed:\n  "+filepath.Join(f.usrLocal, "anet")+" (a link to "+f.exe+")\n",
		"  "+filepath.Join(f.legacy, "anet")+" (a link to "+f.exe+")\n",
		"  the anet PATH block in "+filepath.Join(f.home, ".bashrc")+"\n",
		"  "+fish+"\n",
		"  "+f.exe+".release.json\n",
		"  "+f.exe+"\n",
		"Kept:\n  "+filepath.Join(f.home, ".anet")+", the data directory, which holds the identity keys (anet uninstall --delete-identity deletes it)\n",
		"  "+filepath.Join(f.home, "bin", "anet")+", a link to "+f.other+", not to this anet\n",
		"  "+filepath.Join(f.home, ".zshrc")+": an anet PATH block for /opt/anet/bin, not the directory of this anet\n",
		"  "+filepath.Join(f.home, ".zprofile")+": its anet PATH block is not terminated; edit it by hand\n",
		"  the MCP entry of claude in "+filepath.Join(f.home, ".claude.json")+", which runs this binary; remove it first with: anet agents unwire claude\n",
		"  the MCP entry of claude in "+filepath.Join(f.home, ".claude.json")+", which runs the removed binary; delete the entry anet from that file, or install anet again and run: anet agents unwire claude\n")
	if strings.Contains(text, "Not removed:") || strings.Contains(text, "!") {
		t.Errorf("output:\n%s", text)
	}
}

// Without --yes and without a terminal, nothing is stopped or changed; on
// a terminal the plan is confirmed there, and a refusal changes nothing.
func TestUninstallChangesNothingWithoutConfirmation(t *testing.T) {
	f := newUninstallFixture(t)
	f.tty = false
	var out bytes.Buffer
	u := f.uninstaller(&out)
	var err error
	_, stderr := captureCLI(t, func() { err = u.run() })
	if !errors.Is(err, errExit1) || !strings.Contains(stderr, "Nothing was changed: anet uninstall is confirmed in a terminal, or with --yes") {
		t.Fatalf("err %v, stderr %q", err, stderr)
	}
	if !exists(f.exe) || !exists(filepath.Join(f.usrLocal, "anet")) || f.stopped != 0 {
		t.Fatalf("changed without a confirmation (stopped %d)", f.stopped)
	}

	f.tty = true
	f.confirmErr = errDeclined
	out.Reset()
	if err := f.uninstaller(&out).run(); !errors.Is(err, errDeclined) {
		t.Fatalf("declined: %v", err)
	}
	if !exists(f.exe) || f.stopped != 0 || len(f.confirmed) != 1 || !strings.HasPrefix(f.confirmed[0], "uninstall: ") {
		t.Fatalf("declined: changed, or not asked: %q", f.confirmed)
	}
}

// --delete-identity names every identity and its AID and is confirmed on
// the terminal, also with --yes; without one nothing is changed. Confirmed,
// the data directory goes with the rest.
func TestUninstallDeleteIdentityIsConfirmedOnTheTerminal(t *testing.T) {
	f := newUninstallFixture(t)
	f.confirmErr = errNoTTY
	var out bytes.Buffer
	u := f.uninstaller(&out)
	u.deleteIdentity, u.yes = true, true
	if err := u.run(); !errors.Is(err, errNoTTY) {
		t.Fatalf("no terminal: %v", err)
	}
	if !exists(filepath.Join(f.home, ".anet", "identity.kel")) || !exists(f.exe) || f.stopped != 0 {
		t.Fatal("changed without the terminal's confirmation")
	}
	if len(f.confirmed) != 1 || !strings.HasPrefix(f.confirmed[0], "delete_identity: ") ||
		!strings.Contains(f.confirmed[0], "coder  bafyreicoderaid") || !strings.Contains(f.confirmed[0], "default  bafyreidefaultaid") {
		t.Fatalf("asked %q", f.confirmed)
	}
	mustHave(t, out.String(), "  "+filepath.Join(f.home, ".anet")+", the data directory, with every identity in it:\n    coder  bafyreicoderaid  ")

	f.confirmErr = nil
	out.Reset()
	u = f.uninstaller(&out)
	u.deleteIdentity = true
	if err := u.run(); err != nil {
		t.Fatalf("confirmed: %v\n%s", err, out.String())
	}
	if exists(filepath.Join(f.home, ".anet")) || exists(f.exe) {
		t.Fatal("the data directory or the binary is still there")
	}
	if !exists(f.home) {
		t.Fatal("the home directory went with it")
	}
	mustHave(t, out.String(), "Removed:\n", "  "+filepath.Join(f.home, ".anet")+", the data directory, and the identities in it\n")
}

// A node that does not stop keeps the data directory, which it holds.
func TestUninstallKeepsTheDataOfARunningNode(t *testing.T) {
	f := newUninstallFixture(t)
	f.runningLeft = []string{"default"}
	var out bytes.Buffer
	u := f.uninstaller(&out)
	u.deleteIdentity = true
	if err := u.run(); !errors.Is(err, errExit1) {
		t.Fatalf("err %v", err)
	}
	if !exists(filepath.Join(f.home, ".anet", "identity.kel")) {
		t.Fatal("the data directory of a running node was deleted")
	}
	mustHave(t, out.String(), "Not removed:\n  "+filepath.Join(f.home, ".anet")+" was not deleted: the node of default is still running")
}

// An installation from npm (the launcher's cache, with its record) is
// npm's: the command says how to remove it and changes nothing.
func TestUninstallLeavesAnNPMInstallationToNPM(t *testing.T) {
	f := newUninstallFixture(t)
	cache := filepath.Join(f.home, ".cache", "anet", "npm", "0.3.9")
	f.exe = filepath.Join(cache, "anet")
	f.write(f.exe, "#!/bin/sh\n")
	f.write(filepath.Join(cache, "anet.npm-check"), "1 2\n")
	var out bytes.Buffer
	u := f.uninstaller(&out)
	u.yes = true
	if err := u.run(); err != nil {
		t.Fatal(err)
	}
	mustHave(t, out.String(), "installed with npm", "  anet stop --all\n  anet agents unwire claude\n  npm uninstall -g @agentnetwork/anet\n")
	if !exists(f.exe) || f.stopped != 0 || !exists(filepath.Join(f.usrLocal, "anet")) {
		t.Fatal("an npm installation was changed")
	}
	if !npmInstalled("/usr/lib/node_modules/@agentnetwork/anet-linux-x64/bin/anet") || npmInstalled("/usr/local/bin/anet") {
		t.Error("npmInstalled by path")
	}
}

// A build from source that no installer recorded is not removed, and
// neither are the links to it.
func TestUninstallKeepsABuildFromSource(t *testing.T) {
	f := newUninstallFixture(t)
	os.Remove(f.exe + ".release.json")
	os.Remove(f.exe + ".release.json.sig")
	var out bytes.Buffer
	u := f.uninstaller(&out)
	u.yes, u.variant = true, ""
	if err := u.run(); err != nil {
		t.Fatal(err)
	}
	if !exists(f.exe) || !exists(filepath.Join(f.usrLocal, "anet")) {
		t.Fatal("a build from source, or a link to it, was removed")
	}
	mustHave(t, out.String(), "  nothing: no file of an installer was found\n",
		f.exe+", a build from source that no installer put there, with the links and PATH entries that lead to it; remove them the way they were installed")
}

// A file that only root can change is left, with the command for it.
func TestUninstallNamesTheCommandForRootsFiles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newUninstallFixture(t)
	if err := os.Chmod(f.usrLocal, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.usrLocal, 0o755) })
	var out bytes.Buffer
	u := f.uninstaller(&out)
	u.yes = true
	if err := u.run(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	link := filepath.Join(f.usrLocal, "anet")
	if !exists(link) {
		t.Fatal("removed from a directory this user cannot write")
	}
	mustHave(t, out.String(), "Needs a command from you:\n  "+link+" (a link to "+f.exe+") needs administrator rights; run: sudo rm "+link+"\n")
}

// The command takes no arguments and its flags no values; it refuses them
// before it looks at the binary.
func TestUninstallUsage(t *testing.T) {
	for _, args := range [][]string{{"now"}, {"--yes=no"}, {"--delete-identity=x"}} {
		if err := runUninstall(args); !isUsage(err) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := checkFlags("uninstall", []string{"--purge"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

// The data directory is deleted only when it is a directory below the home
// directory, never the home or the root.
func TestUninstallRemoveAnetHomeRefuses(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{home, filepath.Dir(home), "/", "relative/.anet", ""} {
		if err := removeAnetHome(d, home); err == nil {
			t.Errorf("%q: deleted", d)
		}
	}
	if !exists(home) {
		t.Fatal("the home directory is gone")
	}
	if err := removeAnetHome(filepath.Join(home, ".anet"), home); err != nil {
		t.Errorf("a missing data directory: %v", err)
	}
}

// Windows: the Run value names its program in quotes; the install
// directory is taken out of the user's PATH however it is written, the
// other entries kept as written; only the installation's own files are
// leftovers.
func TestUninstallWindowsHelpers(t *testing.T) {
	if got := runValueExe(`"C:\Users\a\AppData\Local\Programs\anet\anet.exe" up`); got != `C:\Users\a\AppData\Local\Programs\anet\anet.exe` {
		t.Errorf("runValueExe: %q", got)
	}
	if runValueExe(`C:\x\anet.exe up`) != "" {
		t.Error("an unquoted Run value")
	}
	expand := func(s string) string { return strings.ReplaceAll(s, "%LOCALAPPDATA%", `C:\Users\a\AppData\Local`) }
	dir := `C:\Users\a\AppData\Local\Programs\anet`
	rest, found := withoutPathEntry(`%LOCALAPPDATA%\Programs\anet\;C:\Tools; ;%USERPROFILE%\bin;c:\users\a\appdata\local\programs\ANET`, dir, expand)
	if !found || rest != `C:\Tools;%USERPROFILE%\bin` {
		t.Errorf("withoutPathEntry: %q %v", rest, found)
	}
	if _, found := withoutPathEntry(`C:\Tools`, dir, expand); found {
		t.Error("found in a PATH without it")
	}
	d := t.TempDir()
	for _, n := range []string{"anet.exe.old-1729", "anet.new.exe", ".anet-update-AB12.exe", "anet.exe", "anet.exe.release.json", "notes.txt", "anet.exe.old-x"} {
		os.WriteFile(filepath.Join(d, n), nil, 0o644)
	}
	var names []string
	for _, p := range installLeftovers(d) {
		names = append(names, filepath.Base(p))
	}
	if strings.Join(names, ",") != ".anet-update-AB12.exe,anet.exe.old-1729,anet.new.exe" {
		t.Errorf("leftovers %v", names)
	}
}

func mustHave(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q in:\n%s", w, text)
		}
	}
}

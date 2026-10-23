package main

// uninstall.go is `anet uninstall [--delete-identity] [--yes]`: remove what
// the installers made, and nothing else.
//
// What install.sh makes (deploy/release/install.sh): the binary, by default
// ~/.local/bin/anet, with the release record beside it (anet.release.json and
// anet.release.json.sig); a command link to it in /usr/local/bin,
// /opt/homebrew/bin or ~/bin, and a link in place of an earlier release in
// any directory of PATH (replace_legacy_bins); and the marked PATH block
//
//	# >>> anet installer: PATH >>>
//	…
//	# <<< anet installer: PATH <<<
//
// in the shell profiles it writes (.zprofile and .zshrc, under ZDOTDIR when
// it is set; .bash_profile, .bash_login, .bashrc, .profile), and the file
// conf.d/anet.fish for fish. What install.ps1 makes on Windows: anet.exe and
// its release record in the install directory (and the binaries an update
// moved aside), the value AgentNetwork under
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run, and the install
// directory in the user's PATH (uninstall_windows.go).
//
// The command works out first what is there, shows it, and changes nothing
// until it is confirmed: on the terminal, or with --yes. A link is removed
// only when it leads to this binary, and a PATH block only when it names
// this binary's directory; anything else is reported and left as it is.
// The data directory (~/.anet), which holds the only copy of the identity
// keys, is kept unless --delete-identity is given; deleting it names every
// identity and its AID and is confirmed on the terminal in every case, also
// with --yes, so that an agent driving the command cannot delete an
// identity on its own. The coding agents wired with `anet agents wire` keep
// their configuration: the command names `anet agents unwire <name>` for
// each, which must run before the binary is gone. An installation from npm
// is the npm package's to remove: the command names `npm uninstall -g
// @agentnetwork/anet` and changes nothing.
//
// The daemons are stopped first, with `anet stop --all`.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/release"
	"github.com/ANetResearch/ANet/internal/version"
)

// The markers of the PATH block install.sh writes (PATH_BEGIN, PATH_END).
const (
	pathBlockBegin = "# >>> anet installer: PATH >>>"
	pathBlockEnd   = "# <<< anet installer: PATH <<<"
)

// npmUninstall is how an installation from npm is removed.
const npmUninstall = "npm uninstall -g @agentnetwork/anet"

// The local file names the installers give, after the program's name (local
// files, not wire strings): install.sh's fish configuration in
// fish/conf.d, and the record the npm launcher keeps beside the binary it
// checked (deploy/release/npm/anet.js).
const (
	fishConfName  = "anet" + ".fish"
	npmRecordName = "anet" + ".npm-check"
)

// wiredAgent is a coding agent whose configuration runs anet's MCP server.
type wiredAgent struct {
	Tool   string // the name `anet agents unwire` takes
	Config string // the file that holds the entry
}

// uninstaller is one run of anet uninstall. Every effect on the machine
// goes through its fields, so that a test runs it on a temporary home with
// a fake binary.
type uninstaller struct {
	exe     string // the binary, links resolved
	variant string // version.Variant: "" for a build from source
	home    string // the user's home directory
	getenv  func(string) string
	goos    string

	deleteIdentity, yes bool

	// linkDirs are the directories install.sh makes the command link in;
	// the directories of PATH are looked at as well.
	linkDirs []string
	// anetHome is the identity container (~/.anet); identities lists the
	// identities in it.
	anetHome   string
	identities func() []daemon.IdentityInfo
	// stopAll stops every daemon of anetHome, and running names the
	// identities whose daemon still runs.
	stopAll func() error
	running func() []string
	wired   func() []wiredAgent
	// haveTTY reports whether a terminal can be opened; confirm asks on
	// it (confirmOnTTY), with the tty_required action.
	haveTTY func() bool
	confirm func(action, explanation string) error
	// removeExe removes the binary. It returns what to say when the
	// removal finishes only after this process has ended (Windows).
	removeExe func(exe string) (string, error)
	// platform adds the steps of this system (uninstall_unix.go,
	// uninstall_windows.go).
	platform func(u *uninstaller, p *uninstallPlan)

	out  io.Writer
	wait time.Duration // how long the daemons have to stop
}

// uninstallStep is one thing the command removes or changes.
type uninstallStep struct {
	what string       // the line of the plan and of the report
	do   func() error // the change
	// root is the command that does it with administrator rights, said
	// when the change is refused for lack of them.
	root string
}

// uninstallPlan is what a run finds: the steps and what it keeps (and
// why); notes are what the steps have to add to the report.
type uninstallPlan struct {
	steps []uninstallStep
	keep  []string
	notes []string
	// wired are the coding agents whose MCP entry runs this binary, and
	// exeRemoved whether the binary was removed.
	wired      []wiredAgent
	exeRemoved bool
}

func (p *uninstallPlan) add(what string, do func() error, root string) {
	p.steps = append(p.steps, uninstallStep{what: what, do: do, root: root})
}

func (p *uninstallPlan) kept(format string, a ...any) {
	p.keep = append(p.keep, fmt.Sprintf(format, a...))
}

// runUninstall is `anet uninstall [--delete-identity] [--yes]`.
func runUninstall(rest []string) error {
	const usage = "anet uninstall [--delete-identity] [--yes]"
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
		return usageError{usage + "  (it takes no arguments)"}
	}
	for k, v := range flags {
		if v != "true" {
			return usageError{fmt.Sprintf("%s  (--%s takes no value)", usage, k)}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find this binary: %w", err)
	}
	if exe, err = resolveExe(exe); err != nil {
		return fmt.Errorf("cannot resolve this binary: %w", err)
	}
	// A test binary, or a `go run` build, is never the installed anet.
	if strings.Contains(exe, string(os.PathSeparator)+"go-build") {
		return fmt.Errorf("this anet is a temporary go build (%s), not an installed one; nothing was changed", exe)
	}
	home, _ := os.UserHomeDir()
	u := &uninstaller{
		exe: exe, variant: version.Variant, home: home, getenv: os.Getenv, goos: runtime.GOOS,
		deleteIdentity: flags["delete-identity"] == "true", yes: flags["yes"] == "true",
		anetHome:   daemon.AnetHome(),
		identities: func() []daemon.IdentityInfo { ids, _ := daemon.ListIdentities(); return ids },
		stopAll:    func() error { return runStop("", []string{"--all"}) },
		running: func() []string {
			ids, _ := daemon.ListIdentities()
			var out []string
			for _, in := range ids {
				if in.Running {
					out = append(out, in.Name)
				}
			}
			return out
		},
		wired: func() []wiredAgent { return wiredAgents(daemon.ResolveLayout("")) },
		haveTTY: func() bool {
			t, err := openTTY()
			if err != nil {
				return false
			}
			t.Close()
			return true
		},
		confirm:   confirmOnTTY,
		removeExe: removeInstalledExe,
		platform:  platformUninstall,
		out:       outW(),
		wait:      20 * time.Second,
	}
	if runtime.GOOS != "windows" {
		u.linkDirs = []string{"/usr/local/bin", "/opt/homebrew/bin", filepath.Join(home, "bin")}
	}
	return u.run()
}

// run finds what there is, shows it, asks, and removes it.
func (u *uninstaller) run() error {
	w := u.out
	if npmInstalled(u.exe) {
		fmt.Fprintf(w, "This anet was installed with npm (%s); npm removes it. Nothing was changed.\n", u.exe)
		fmt.Fprintln(w, "To remove it, run in this order:")
		fmt.Fprintln(w, "  anet stop --all")
		for _, a := range u.wired() {
			fmt.Fprintf(w, "  anet agents unwire %s\n", a.Tool)
		}
		fmt.Fprintf(w, "  %s\n", npmUninstall)
		fmt.Fprintf(w, "The checked binary stays in %s until that directory is deleted; the data directory %s is kept.\n",
			filepath.Dir(u.exe), u.anetHome)
		return nil
	}

	p := u.plan()
	ids := u.identities()
	fmt.Fprintln(w, "anet uninstall removes:")
	for _, s := range p.steps {
		fmt.Fprintf(w, "  %s\n", s.what)
	}
	if len(p.steps) == 0 {
		fmt.Fprintln(w, "  nothing: no file of an installer was found")
	}
	if u.deleteIdentity {
		fmt.Fprintf(w, "  %s, the data directory, with every identity in it:\n", u.anetHome)
		for _, line := range identityLines(ids) {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	fmt.Fprintln(w, "and keeps:")
	keep := slices.Clone(p.keep)
	if !u.deleteIdentity {
		keep = append([]string{u.anetHome + ", the data directory, which holds the identity keys (anet uninstall --delete-identity deletes it)"}, keep...)
	}
	for _, k := range keep {
		fmt.Fprintf(w, "  %s\n", k)
	}
	for _, a := range p.wired {
		fmt.Fprintf(w, "  the MCP entry of %s in %s, which runs this binary; remove it first with: anet agents unwire %s\n", a.Tool, a.Config, a.Tool)
	}

	// Nothing has been changed so far, and nothing is without a
	// confirmation: deleting identities is confirmed on the terminal
	// always; the rest there, or by --yes.
	switch {
	case u.deleteIdentity:
		explanation := "This deletes " + u.anetHome + " and with it, for good, the keys of these identities:\n  " +
			strings.Join(identityLines(ids), "\n  ") +
			"\nAn identity can be restored afterwards only from its recovery file and passphrase."
		if err := u.confirm("delete_identity", explanation); err != nil {
			return err
		}
	case !u.yes:
		if !u.haveTTY() {
			fmt.Fprintln(errW(), "Nothing was changed: anet uninstall is confirmed in a terminal, or with --yes (anet uninstall --yes).")
			return errExit1
		}
		if err := u.confirm("uninstall", "This removes anet as listed above."); err != nil {
			return err
		}
	}

	// The daemons first: a running node holds the data directory, and on
	// Windows the binary.
	var still []string
	if len(u.running()) > 0 {
		if err := u.stopAll(); err != nil {
			fmt.Fprintf(errW(), "anet stop --all: %v\n", err)
		}
		deadline := time.Now().Add(u.wait)
		for still = u.running(); len(still) > 0 && time.Now().Before(deadline); still = u.running() {
			time.Sleep(200 * time.Millisecond)
		}
	}

	var removed, failed, manual []string
	for _, s := range p.steps {
		err := s.do()
		switch {
		case err == nil:
			removed = append(removed, s.what)
		case errors.Is(err, fs.ErrPermission) && s.root != "":
			manual = append(manual, fmt.Sprintf("%s needs administrator rights; run: %s", s.what, s.root))
		default:
			failed = append(failed, fmt.Sprintf("%s: %v", s.what, err))
		}
	}
	if u.deleteIdentity {
		switch {
		case len(still) > 0:
			failed = append(failed, fmt.Sprintf("%s was not deleted: the node of %s is still running; stop it with anet stop --all, then delete the directory",
				u.anetHome, strings.Join(still, ", ")))
		default:
			if err := removeAnetHome(u.anetHome, u.home); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", u.anetHome, err))
			} else {
				removed = append(removed, u.anetHome+", the data directory, and the identities in it")
			}
		}
	}

	fmt.Fprintln(w)
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintln(w, title)
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	for _, a := range p.wired {
		if p.exeRemoved {
			keep = append(keep, fmt.Sprintf("the MCP entry of %s in %s, which runs the removed binary; delete the entry anet from that file, or install anet again and run: anet agents unwire %s",
				a.Tool, a.Config, a.Tool))
		} else {
			keep = append(keep, fmt.Sprintf("the MCP entry of %s in %s; remove it with: anet agents unwire %s", a.Tool, a.Config, a.Tool))
		}
	}
	section("Removed:", removed)
	section("Kept:", keep)
	section("Not removed:", failed)
	section("Needs a command from you:", manual)
	for _, n := range p.notes {
		fmt.Fprintln(w, n)
	}
	if len(still) > 0 && !u.deleteIdentity {
		fmt.Fprintf(w, "The node of %s is still running; stop it with: anet stop --all\n", strings.Join(still, ", "))
	}
	if len(failed) > 0 {
		return errExit1
	}
	return nil
}

// plan is what this run would remove and keep. It reads, and changes
// nothing.
func (u *uninstaller) plan() *uninstallPlan {
	p := &uninstallPlan{wired: u.wired()}
	_, err := os.Stat(u.exe + release.InstalledManifestSuffix)
	if u.variant == "" && err != nil {
		// Neither stamped by a release build nor recorded by an
		// installer: a build from source, installed by whoever built it,
		// and so are the ways it is reached.
		p.kept("%s, a build from source that no installer put there, with the links and PATH entries that lead to it; remove them the way they were installed", u.exe)
		return p
	}
	if u.platform != nil {
		u.platform(u, p)
	}
	// The binary and its record last: a step that fails before leaves the
	// command in place to try again.
	for _, f := range []string{u.exe + release.InstalledManifestSuffix, u.exe + release.InstalledSigSuffix} {
		if fi, err := os.Lstat(f); err == nil && fi.Mode().IsRegular() {
			p.add(f, func() error { return os.Remove(f) }, sudoRemove(u.goos, f))
		}
	}
	p.add(u.exe, func() error {
		note, err := u.removeExe(u.exe)
		if err == nil {
			p.exeRemoved = true
			if note != "" {
				p.notes = append(p.notes, note)
			}
		}
		return err
	}, sudoRemove(u.goos, u.exe))
	return p
}

// sudoRemove is the command that removes path as root, on a system that
// has sudo.
func sudoRemove(goos, path string) string {
	if goos == "windows" {
		return ""
	}
	return "sudo rm " + shellQuote(path)
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// identityLines names each identity: its name, AID and data directory.
func identityLines(ids []daemon.IdentityInfo) []string {
	if len(ids) == 0 {
		return []string{"(no identity has been created)"}
	}
	var out []string
	for _, in := range ids {
		aid := in.AID
		if aid == "" {
			aid = "(no key yet)"
		}
		out = append(out, fmt.Sprintf("%s  %s  %s", in.Name, aid, in.DataDir))
	}
	return out
}

// removeAnetHome deletes the identity container dir, which must be an
// absolute path that is neither the root, the home directory nor above it.
func removeAnetHome(dir, home string) error {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || clean == filepath.Dir(clean) {
		return fmt.Errorf("refusing to delete %q: not a data directory", dir)
	}
	if home != "" {
		if rel, err := filepath.Rel(clean, filepath.Clean(home)); err == nil && !strings.HasPrefix(rel, "..") {
			return fmt.Errorf("refusing to delete %q: it is or contains the home directory", dir)
		}
	}
	if _, err := os.Lstat(clean); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return os.RemoveAll(clean)
}

// npmInstalled reports whether exe is the binary the npm launcher of
// @agentnetwork/anet checked and runs: it lies in the launcher's cache
// directory, beside the launcher's record anet.npm-check
// (deploy/release/npm/anet.js), or in a package under
// node_modules/@agentnetwork.
func npmInstalled(exe string) bool {
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), npmRecordName)); err == nil {
		return true
	}
	sep := string(filepath.Separator)
	return strings.Contains(exe, sep+"node_modules"+sep+"@agentnetwork"+sep)
}

// pathBlock is one marked block of a shell profile: its lines [start, end]
// and the directory it puts on PATH.
type pathBlock struct {
	start, end int
	dir        string
}

// The line of a block that names the directory: the case pattern of the sh
// block, the test of the fish block (install.sh path_block).
var (
	shBlockDir   = regexp.MustCompile(`^case "\$PATH" in "([^"]+)"\|`)
	fishBlockDir = regexp.MustCompile(`^if test "\$PATH\[1\]" != "([^"]+)"$`)
)

// findPathBlocks reads the marked blocks of a profile. ok is false when the
// markers do not pair up (a block without its end, or one inside another):
// such a file is left as it is.
func findPathBlocks(lines []string, home string) (blocks []pathBlock, ok bool) {
	start := -1
	for i, l := range lines {
		switch l {
		case pathBlockBegin:
			if start >= 0 {
				return nil, false
			}
			start = i
		case pathBlockEnd:
			if start < 0 {
				return nil, false
			}
			b := pathBlock{start: start, end: i}
			for _, m := range lines[start+1 : i] {
				if s := shBlockDir.FindStringSubmatch(m); s != nil {
					b.dir = s[1]
				} else if s := fishBlockDir.FindStringSubmatch(m); s != nil {
					b.dir = s[1]
				}
			}
			if rest, found := strings.CutPrefix(b.dir, "$HOME/"); found && home != "" {
				b.dir = filepath.Join(home, rest)
			}
			blocks = append(blocks, b)
			start = -1
		}
	}
	return blocks, start < 0
}

// withoutBlocks is lines without the blocks, and without the empty line the
// installer put before each one.
func withoutBlocks(lines []string, blocks []pathBlock) []string {
	drop := map[int]bool{}
	for _, b := range blocks {
		for i := b.start; i <= b.end; i++ {
			drop[i] = true
		}
		if b.start > 0 && strings.TrimSpace(lines[b.start-1]) == "" {
			drop[b.start-1] = true
		}
	}
	var out []string
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// sameDir reports whether a and b are the same directory, their links
// followed where they exist.
func sameDir(a, b string) bool {
	norm := func(p string) string {
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return p
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(strings.TrimRight(norm(a), `\`), strings.TrimRight(norm(b), `\`))
	}
	return norm(a) == norm(b)
}

// readLines reads a text file as lines, and whether it ended with a line
// end.
func readLines(path string) ([]string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, len(b) > 0 && b[len(b)-1] == '\n', sc.Err()
}

// writeInPlace writes lines into path through any link (a profile kept in
// a dotfiles repository stays a link to it), keeping its mode.
func writeInPlace(path string, lines []string, finalNewline bool) error {
	text := strings.Join(lines, "\n")
	if finalNewline && len(lines) > 0 {
		text += "\n"
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// profileBlockSteps adds, for the profile at path, the removal of the
// blocks that name dir, the directory of this binary. A file that holds
// nothing else and is the installer's own (removeEmpty, conf.d/anet.fish)
// is removed whole. Blocks that name another directory, and a file whose
// markers do not pair up, are kept.
func profileBlockSteps(p *uninstallPlan, path, dir, home string, removeEmpty bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	lines, nl, err := readLines(path)
	if err != nil {
		return
	}
	blocks, ok := findPathBlocks(lines, home)
	if !ok {
		p.kept("%s: its anet PATH block is not terminated; edit it by hand", path)
		return
	}
	var ours []pathBlock
	for _, b := range blocks {
		if b.dir != "" && sameDir(b.dir, dir) {
			ours = append(ours, b)
		} else {
			p.kept("%s: an anet PATH block for %s, not the directory of this anet", path, b.dir)
		}
	}
	if len(ours) == 0 {
		return
	}
	rest := withoutBlocks(lines, ours)
	empty := strings.TrimSpace(strings.Join(rest, "")) == ""
	if removeEmpty && empty {
		if lfi, err := os.Lstat(path); err == nil && lfi.Mode().IsRegular() {
			p.add(path, func() error { return os.Remove(path) }, "")
			return
		}
	}
	p.add("the anet PATH block in "+path, func() error { return writeInPlace(path, rest, nl) }, "")
}

// profileFiles are the shell profiles install.sh writes the block into
// (profile_files), each once, and the fish file.
func profileFiles(home string, getenv func(string) string) (sh []string, fish string) {
	zd := getenv("ZDOTDIR")
	if zd == "" {
		zd = home
	}
	cfg := getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	seen := map[string]bool{}
	for _, f := range []string{filepath.Join(zd, ".zprofile"), filepath.Join(zd, ".zshrc"),
		filepath.Join(home, ".bash_profile"), filepath.Join(home, ".bash_login"),
		filepath.Join(home, ".bashrc"), filepath.Join(home, ".profile")} {
		if !seen[f] {
			seen[f] = true
			sh = append(sh, f)
		}
	}
	return sh, filepath.Join(cfg, "fish", "conf.d", fishConfName)
}

// linkSteps adds the removal of each link named anet, in dirs and in the
// directories of PATH, that leads to exe; another anet there is kept and
// said.
func linkSteps(p *uninstallPlan, exe string, dirs []string, pathEnv, goos string) {
	seen := map[string]bool{}
	all := slices.Clone(dirs)
	for _, d := range filepath.SplitList(pathEnv) {
		if filepath.IsAbs(d) {
			all = append(all, d)
		}
	}
	for _, d := range all {
		link := filepath.Join(filepath.Clean(d), "anet")
		if seen[link] || link == exe {
			continue
		}
		seen[link] = true
		fi, err := os.Lstat(link)
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			if same, err := os.Stat(exe); err == nil && os.SameFile(fi, same) {
				continue // the binary itself, by another name of its directory
			}
			p.kept("%s, another anet, which no installer of this one put there", link)
			continue
		}
		target, err := filepath.EvalSymlinks(link)
		if err != nil || target != exe {
			to, _ := os.Readlink(link)
			p.kept("%s, a link to %s, not to this anet", link, to)
			continue
		}
		p.add(link+" (a link to "+exe+")", func() error { return os.Remove(link) }, sudoRemove(goos, link))
	}
}

// runValueExe is the program a Run value starts: the quoted path that
// begins it, as install.ps1 writes it ("<exe>" up), or "".
func runValueExe(v string) string {
	if m := regexp.MustCompile(`^"([^"]+)"`).FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return ""
}

// withoutPathEntry is a Windows PATH value (entries separated by ";")
// without the entries that are dir once expanded (expand), compared as
// Windows compares paths; found reports whether there was one. The other
// entries are kept as they are written, %NAME% unexpanded.
func withoutPathEntry(raw, dir string, expand func(string) string) (rest string, found bool) {
	norm := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), `\`) }
	var keep []string
	for _, e := range strings.Split(raw, ";") {
		if strings.TrimSpace(e) == "" {
			continue
		}
		if strings.EqualFold(norm(expand(strings.Trim(strings.TrimSpace(e), `"`))), norm(dir)) {
			found = true
			continue
		}
		keep = append(keep, strings.TrimSpace(e))
	}
	return strings.Join(keep, ";"), found
}

// leftoverRe are the other files of an installation in its directory on
// Windows: binaries an update moved aside, the staged binary of an
// installation that stopped, and the temporary file of an interrupted
// anet update (install.ps1 Test-AnetDedicatedDir).
var leftoverRe = regexp.MustCompile(`(?i)^(anet\.exe\.old-[0-9]+|anet\.new\.exe|\.anet-update-[0-9A-Za-z]+\.exe)$`)

// installLeftovers lists those files in dir, regular files only.
func installLeftovers(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && leftoverRe.MatchString(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

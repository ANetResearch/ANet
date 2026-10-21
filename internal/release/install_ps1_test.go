package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// install.ps1 is the template of the Windows installer: build-release.sh
// writes the release pins into it (ps1-pins.sh) and signs the result. These
// tests read the template as committed.

const (
	ps1BeginMark = "# BEGIN RELEASE PINS (written by deploy/release/build-release.sh; do not edit)"
	ps1EndMark   = "# END RELEASE PINS"
	ps1NullLine  = "$AnetRelease = $null"
	ps1LastLine  = "if ($env:ANET_INSTALL_PS1_TEST -ne '1') { Install-Anet @PSBoundParameters; if (Test-AnetOneShotHost) { exit $global:LASTEXITCODE } }"
)

func installPS1(t *testing.T) string {
	t.Helper()
	return repoFile(t, "deploy/release/install.ps1")
}

// codeLines are the lines of a PowerShell script that are not comments.
func codeLines(script string) []string {
	var out []string
	for _, l := range strings.Split(script, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, l)
		}
	}
	return out
}

// The signed bytes are what is published: ASCII, so that no code page or
// byte order mark changes them, and without the constructs the design
// rules out.
func TestInstallPS1FileRules(t *testing.T) {
	script := installPS1(t)
	for i, b := range []byte(script) {
		if b > 0x7e || (b < 0x20 && b != '\n' && b != '\r' && b != '\t') {
			t.Fatalf("install.ps1 is not ASCII: byte %#x at offset %d", b, i)
		}
	}
	code := strings.Join(codeLines(script), "\n")
	if regexp.MustCompile(`(?i)\b(invoke-expression|iex)\b`).MatchString(script) {
		t.Error("install.ps1 uses Invoke-Expression")
	}
	// Add-Type only loads the HTTP client assembly of .NET; it never
	// compiles code.
	for _, m := range regexp.MustCompile(`(?i)add-type[^\n]*`).FindAllString(code, -1) {
		if m != "Add-Type -AssemblyName System.Net.Http }" {
			t.Errorf("install.ps1 uses Add-Type other than to load System.Net.Http: %q", m)
		}
	}
	// ssh-keygen appears in the comments that describe the verify-first
	// form, never in code: Windows 10 1809 ships an ssh-keygen without -Y.
	if strings.Contains(strings.ToLower(code), "ssh-keygen") {
		t.Error("install.ps1 runs ssh-keygen")
	}
	// The script ends the PowerShell process it runs in only on its last line, and only when that
	// process was started for this one command (Test-AnetOneShotHost): an open window stays open.
	if n := len(regexp.MustCompile(`(?im)(^|[;{}])\s*exit(\s|$)`).FindAllString(code, -1)); n != 1 {
		t.Errorf("install.ps1 calls exit %d times, want once, on its last line", n)
	}
	// The pins block, exactly once, in the form ps1-pins.sh fills.
	block := ps1BeginMark + "\n" + ps1NullLine + "\n" + ps1EndMark + "\n"
	for _, s := range []string{ps1BeginMark, ps1EndMark, ps1NullLine, block} {
		if n := strings.Count(script, s); n != 1 {
			t.Errorf("install.ps1 holds %q %d times, want once", s, n)
		}
	}
	// Parameters, then the pins and functions, and the call last: a script
	// cut short runs nothing.
	if !strings.HasSuffix(script, "\n"+ps1LastLine+"\n") {
		t.Error("the last line of install.ps1 is not the call of Install-Anet")
	}
	if strings.Count(code, "Install-Anet @PSBoundParameters") != 1 {
		t.Error("install.ps1 calls Install-Anet more than once")
	}
	if !strings.Contains(script, "\nparam(\n  [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,\n"+
		"  [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated,\n  [switch]$NoJoin, [switch]$NoStart, [switch]$Agents, [string[]]$AgentList\n)\n") {
		t.Error("install.ps1 does not declare the parameters of the specification")
	}
	// Every program is started by the one helper, with an argument array.
	if calls := regexp.MustCompile(`&\s*\$`).FindAllString(code, -1); len(calls) != 2 ||
		strings.Count(code, "& $FilePath @ArgumentList") != 2 {
		t.Errorf("install.ps1 runs programs outside Invoke-AnetNative: %q", calls)
	}
	if regexp.MustCompile(`(?i)start-process|\[(system\.)?diagnostics\.process\]|cmd(\.exe)?\s+/c|2>&1|2>\$null|invoke-item|read-host|\[console\]::(in|read)`).MatchString(code) {
		t.Error("install.ps1 starts a program another way, redirects stderr, or reads the console")
	}
	// Programs that are not the anet it installs (an earlier anet, npm, a fresh powershell.exe) run
	// through Invoke-AnetTimed alone, with standard input closed and a time limit; the only COM object
	// is the Task Scheduler's.
	timed := code[strings.Index(code, "function Invoke-AnetTimed {"):]
	timed = timed[:strings.Index(timed, "\n}\n")]
	if strings.Count(code, "System.Diagnostics.Process") != 2 || strings.Count(timed, "System.Diagnostics.Process") != 2 ||
		!strings.Contains(timed, "$psi.RedirectStandardInput = $true") || !strings.Contains(timed, "$p.StandardInput.Close()") {
		t.Error("install.ps1 creates a process outside Invoke-AnetTimed, or with the console as its input")
	}
	if m := regexp.MustCompile(`(?i)-ComObject\s+(\S+)`).FindAllStringSubmatch(code, -1); len(m) != 1 || m[0][1] != "Schedule.Service" {
		t.Errorf("install.ps1 uses COM objects other than the Task Scheduler: %q", m)
	}
	// The commands of the CLI surface the installer depends on.
	for _, s := range []string{"'--verify-dir'", "'--installed'", "'--shell-controller'", "'--yes'", "'--passphrase-later'", "'--without'",
		"'hub-register'", "'init'", "'doctor'", "'up'", "'status'", "'stop'"} {
		if !strings.Contains(code, s) {
			t.Errorf("install.ps1 does not pass %s", s)
		}
	}
	// No secret on a command line, and no invitation (as for install.sh).
	for _, s := range []string{"--token", "ANET_INVITE", "--api-key"} {
		if strings.Contains(script, s) {
			t.Errorf("install.ps1 mentions %s", s)
		}
	}
	// An elevated window is not refused (v0.3.5): -Elevated is accepted and changes nothing.
	if strings.Contains(code, "not elevated") || strings.Contains(code, "-not $Elevated") {
		t.Error("install.ps1 still refuses an elevated window")
	}
	// The hub registration never asks for a passphrase (v0.3.6): every call of hub-register passes
	// --passphrase-later.
	if calls := regexp.MustCompile(`'hub-register'[^\n]*`).FindAllString(code, -1); len(calls) != 1 || !strings.Contains(calls[0], "'--passphrase-later'") {
		t.Errorf("install.ps1 runs hub-register without --passphrase-later: %q", calls)
	}
	// A process is ended only by its ID, after its program was checked to be AgentNetwork 1.x
	// (Stop-AnetLegacyDaemon), or as the tree of a program this script started (Stop-AnetProcessTree);
	// never by name.
	// (A command printed for the user, in quotes, is not a statement.)
	if m := regexp.MustCompile(`(?i)(^|[;{]\s*)stop-process[^\n]*`).FindAllString(strings.ReplaceAll(code, "\n", "\n;"), -1); len(m) != 1 ||
		!strings.HasSuffix(m[0], "Stop-Process -Id $d.Pid -Force -ErrorAction Stop") {
		t.Errorf("install.ps1 ends processes other than by a checked ID: %q", m)
	}
	if regexp.MustCompile(`(?i)(get|stop)-process\s+-name|taskkill[^\n]*/im|\bkillall\b|\bpkill\b`).MatchString(code) {
		t.Error("install.ps1 chooses a process by its name")
	}
	stop := code[strings.Index(code, "function Stop-AnetLegacyDaemon {"):]
	stop = stop[:strings.Index(stop, "\n}\n")]
	if i, j := strings.Index(stop, "(Get-AnetBinaryFamily -Exe $exe) -ne 'v1'"), strings.Index(stop, "Stop-Process -Id"); i < 0 || j < 0 || i > j {
		t.Error("Stop-AnetLegacyDaemon does not check the program of a process before it ends it")
	}
	// The controller grant asks nothing: -Controller is the consent, passed on as --yes.
	if !strings.Contains(code, "@('init', '--shell-controller', $o.Controller, '--yes')") {
		t.Error("install.ps1 does not pass --yes with --shell-controller")
	}
	// The one line and the verify-first form are in the header. The verify-first form writes
	// allowed_signers without a byte order mark, checks in cmd.exe and stops on a failed check before
	// the run, on the same line; the check alone is one line, the same in cmd.exe and a POSIX shell.
	verify := "ssh-keygen -Y verify -f allowed_signers -I " + Identity + " -n " + Namespace + " -s install.ps1.sig < install.ps1"
	for _, s := range []string{
		"#   " + WindowsInstallCommand,
		"#   " + WindowsInstallControllerCommand,
		"#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1 -OutFile install.ps1",
		"#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1.sig -OutFile install.ps1.sig",
		"#   [IO.File]::WriteAllText((Join-Path $PWD.ProviderPath 'allowed_signers'), '" + strings.TrimSpace(allowedSigners) + "')",
		"#   cmd /c \"" + verify + "\"; if ($LASTEXITCODE -ne 0) { throw 'install.ps1 did not verify' }; " +
			"& ([scriptblock]::Create((Get-Content -Raw .\\install.ps1))) -Controller CONTROLLER_AID",
		"#   " + verify,
	} {
		if !strings.Contains(script, s+"\n") {
			t.Errorf("install.ps1's header lacks %q", s)
		}
	}
}

// WindowsInstallCommand is the published Windows installation command (v0.3.5): one line whose bytes
// run unchanged in cmd.exe, Windows PowerShell 5.1, PowerShell 7, bash and zsh in WSL and Git Bash. It
// starts a new powershell.exe, which fetches install.ps1 and runs it as a script block.
// WindowsInstallControllerCommand is the same with -Controller and the placeholder CONTROLLER_AID,
// which install.ps1 refuses by name. cmd/anet's apexdocs test holds the published documents to them.
const (
	WindowsInstallCommand = `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command ` +
		`"[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"`
	WindowsInstallControllerCommand = `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command ` +
		`"[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1))) -Controller CONTROLLER_AID"`
)

// The published commands are one line, everything after -Command is one double-quoted argument, and
// no character in them means anything to cmd.exe, PowerShell, a POSIX shell or Git Bash's path
// conversion: no $, %, !, ^, backtick, single quote, < or >, and no inner double quote.
func TestWindowsInstallCommandIsShellNeutral(t *testing.T) {
	for _, c := range []string{WindowsInstallCommand, WindowsInstallControllerCommand} {
		if strings.ContainsAny(c, "$%!^`'<>\r\n\t") {
			t.Errorf("the command holds a character a shell interprets: %q", c)
		}
		for _, b := range []byte(c) {
			if b < 0x20 || b > 0x7e {
				t.Errorf("the command is not printable ASCII: %q", c)
			}
		}
		head, arg, ok := strings.Cut(c, " -Command ")
		if !ok || head != "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass" {
			t.Errorf("the command does not start powershell.exe with -Command: %q", c)
		}
		if len(arg) < 2 || arg[0] != '"' || arg[len(arg)-1] != '"' || strings.Count(arg, `"`) != 2 {
			t.Errorf("everything after -Command is not one double-quoted argument: %q", arg)
		}
		// Git Bash converts an argument that starts with / (or holds =/) into a Windows path.
		if strings.HasPrefix(arg[1:], "/") || strings.Contains(c, "=/") {
			t.Errorf("Git Bash would rewrite part of the command: %q", c)
		}
	}
	if WindowsInstallControllerCommand != strings.TrimSuffix(WindowsInstallCommand, `"`)+` -Controller CONTROLLER_AID"` {
		t.Error("the controller form is not the plain form with -Controller CONTROLLER_AID")
	}
	if !strings.Contains(installPS1(t), "$AnetControllerPlaceholder = 'CONTROLLER_AID'") {
		t.Error("install.ps1 does not refuse the placeholder by name")
	}
}

// The fingerprint the script compares with what the binary reports is that
// of the key in allowed_signers, and the header shows that key's line.
func TestInstallPS1ReleaseKey(t *testing.T) {
	script := installPS1(t)
	m := regexp.MustCompile(`(?m)^\$AnetReleaseKeyFingerprint = '([^']*)'$`).FindAllStringSubmatch(script, -1)
	if len(m) != 1 {
		t.Fatalf("install.ps1 sets $AnetReleaseKeyFingerprint %d times", len(m))
	}
	if want := Fingerprint(DefaultTrust().Keys[0]); m[0][1] != want {
		t.Errorf("install.ps1 expects the release key %s, allowed_signers holds %s", m[0][1], want)
	}
	if !strings.Contains(script, "#   "+strings.TrimSpace(AllowedSigners())+"\n") {
		t.Error("install.ps1's header does not show the allowed_signers line")
	}
}

// install.ps1 fetches exactly the release it pins, from the sources of the download policy (v0.3.6,
// the same as install.sh and `anet update`): npmmirror and GitHub, probed in parallel and tried in the
// order they answered; then registry.npmjs.org, jsDelivr and the project's two hosts, in that order.
// -Base, else ANET_INSTALL_BASE, replaces them. test-install.ps1 runs the order with fixed probe results.
func TestInstallPS1SourcesFollowThePolicy(t *testing.T) {
	code := strings.Join(codeLines(installPS1(t)), "\n")
	for _, want := range []string{
		"MetaUrl = ('https://registry.npmmirror.com/' + $pkg + '/' + $Version); Url = ('https://registry.npmmirror.com/' + $pkg + $tgz)",
		"$AnetGitHubReleases = 'https://github.com/ANetResearch/ANet/releases/download/v'",
		"Prefix = ($AnetGitHubReleases + $Version + '/'); Redirect = 'github'",
		"MetaUrl = ('https://registry.npmjs.org/' + $pkg + '/' + $Version); Url = ('https://registry.npmjs.org/' + $pkg + $tgz)",
		"Prefix = ('https://cdn.jsdelivr.net/npm/' + $pkg + '@' + $Version + '/dl/')",
		"Prefix = 'https://agentnetwork.org.cn/dl/'",
		"Prefix = 'https://hub.agentnetwork.org.cn/dl/'",
		"$tgz = '/-/' + $leaf + '-' + $Version + '.tgz'",
		"foreach ($n in @('npmjs', 'jsdelivr', 'host1', 'host2')) { $order.Add($List[$n]) }",
		"if (($n -eq 'npmmirror' -or $n -eq 'github')",
		"Invoke-AnetProbe -Sources @($list['npmmirror'], $list['github']) -Version $Version",
		"[int]$TimeoutMs = 3000, [int]$TotalMs = 4000",
		"if ($Options.Base -ne '') { return @(New-AnetBaseSource -Base $Options.Base) }",
		"return @(New-AnetBaseSource -Base (Get-AnetBaseValue -Text $env:ANET_INSTALL_BASE -What 'ANET_INSTALL_BASE'))",
		"$sources = @(Get-AnetSources -Options $o -Arch $arch -Version $version)",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("install.ps1 does not contain %q", want)
		}
	}
	// The version asked of every source is the one the script pins.
	if !strings.Contains(code, "$version = [string]$AnetRelease['Version']") {
		t.Error("install.ps1 does not take the version of its sources from its pins")
	}
	if !strings.Contains(code, "return '@agentnetwork/' + $AssetPrefix + '-win32-' + $cpu") ||
		NPMPackage("default", "windows", "amd64") != "@agentnetwork/anet-win32-x64" {
		t.Error("install.ps1 and NPMPackage name the npm packages differently")
	}
}

// fixtureAssets is a dist/.assets as build-release.sh writes it, for the
// four Windows builds and one other.
func fixtureAssets(t *testing.T, names ...string) (string, map[string][4]string) {
	t.Helper()
	pins := map[string][4]string{}
	var b strings.Builder
	for i, name := range names {
		variant := "default"
		if strings.HasPrefix(name, "anet-shell-") {
			variant = "shell"
		}
		parts := strings.Split(name, "-")
		gz, raw := fmt.Sprintf("%064x", i+1), fmt.Sprintf("%064x", i+101)
		gzSize, size := fmt.Sprint(1000+i), fmt.Sprint(9000000+i)
		pins[name] = [4]string{gz, gzSize, raw, size}
		fmt.Fprintf(&b, "    %q: {\"variant\": %q, \"os\": %q, \"arch\": %q, \"gz_sha256\": %q, \"gz_size\": %s, \"sha256\": %q, \"size\": %s}\n",
			name, variant, parts[len(parts)-2], parts[len(parts)-1], gz, gzSize, raw, size)
	}
	p := filepath.Join(t.TempDir(), ".assets")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, pins
}

var windowsAssets = []string{"anet-windows-amd64", "anet-windows-arm64", "anet-shell-windows-amd64", "anet-shell-windows-arm64"}

func runPins(t *testing.T, assets, version, template string) (string, string, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "deploy", "release", "ps1-pins.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, script, assets, version)
	cmd.Stdin = strings.NewReader(template)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// ps1-pins.sh writes the pins of the four Windows builds into the one pins
// block, and changes nothing else.
func TestPS1PinsWritesThePins(t *testing.T) {
	template := installPS1(t)
	assets, pins := fixtureAssets(t, append([]string{"anet-linux-amd64"}, windowsAssets...)...)
	out, stderr, code := runPins(t, assets, "0.4.0-rc.1", template)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := "$AnetRelease = @{\n  Version = '0.4.0-rc.1'\n  Assets = @{\n"
	for _, n := range windowsAssets {
		p := pins[n]
		want += fmt.Sprintf("    '%s' = @{ GzSha256 = '%s'; GzSize = %s; Sha256 = '%s'; Size = %s }\n", n, p[0], p[1], p[2], p[3])
	}
	want += "  }\n}"
	if out != strings.Replace(template, ps1NullLine, want, 1) {
		t.Fatalf("ps1-pins.sh output differs from the template with the pins written in:\n%s", out)
	}
	for _, s := range []string{ps1BeginMark, ps1EndMark} {
		if n := strings.Count(out, s); n != 1 {
			t.Errorf("the output holds %q %d times", s, n)
		}
	}
	if strings.Contains(out, ps1NullLine) {
		t.Error("the output still holds the template's empty pins")
	}
}

// Anything but the four Windows builds, each once, and a template with one
// pins block, is refused: status 1, nothing on stdout.
func TestPS1PinsRefuses(t *testing.T) {
	template := installPS1(t)
	good, _ := fixtureAssets(t, windowsAssets...)
	missing, _ := fixtureAssets(t, windowsAssets[:3]...)
	twice, _ := fixtureAssets(t, append(windowsAssets, "anet-windows-amd64")...)
	extra, _ := fixtureAssets(t, append(windowsAssets, "anet-other-windows-amd64")...)
	badHex := filepath.Join(t.TempDir(), ".assets")
	b, _ := os.ReadFile(good)
	os.WriteFile(badHex, []byte(strings.Replace(string(b), `"gz_sha256": "0`, `"gz_sha256": "G`, 1)), 0o644)
	for _, c := range []struct {
		name, assets, version, template, want string
	}{
		{"a build missing", missing, "1.0.0", template, "the four Windows builds are required"},
		{"a build twice", twice, "1.0.0", template, "Windows builds"},
		{"another Windows build", extra, "1.0.0", template, "Windows builds"},
		{"a bad hash", badHex, "1.0.0", template, "gz_sha256"},
		{"a bad version", good, "1.0", template, "is not x.y.z"},
		{"no assets file", filepath.Join(t.TempDir(), "none"), "1.0.0", template, "does not exist"},
		{"no pins block", good, "1.0.0", strings.Replace(template, ps1NullLine, "", 1), "pins block"},
		{"two pins blocks", good, "1.0.0", template + ps1BeginMark + "\n" + ps1NullLine + "\n" + ps1EndMark + "\n", "pins block"},
		{"the pins line outside the block", good, "1.0.0",
			strings.Replace(strings.Replace(template, ps1NullLine+"\n", "", 1), ps1EndMark+"\n", ps1EndMark+"\n"+ps1NullLine+"\n", 1), "pins block"},
		{"CRLF line ends", good, "1.0.0", strings.ReplaceAll(template, "\n", "\r\n"), "pins block"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, stderr, code := runPins(t, c.assets, c.version, c.template)
			if code != 1 || out != "" || !strings.Contains(stderr, c.want) {
				t.Fatalf("exit %d, stdout %d bytes, stderr %q; want 1, nothing, %q", code, len(out), stderr, c.want)
			}
		})
	}
}

// build-release.sh writes install.ps1 from the template only when it built
// all four Windows assets, signs it as it signs install.sh, and leaves it
// alone when it re-signs the manifest.
func TestBuildReleaseWritesInstallPS1(t *testing.T) {
	build := repoFile(t, "deploy/release/build-release.sh")
	for _, want := range []string{
		`ALL="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64 windows-arm64"`,
		`bash "$REL_DIR/ps1-pins.sh" "$DIST/.assets" "$VERSION" < "$REL_DIR/install.ps1" > "$DIST/install.ps1"`,
		`then sign "$DIST/install.ps1"; fi`,
		"install.ps1 not written: the Windows platforms were not built",
	} {
		if !strings.Contains(build, want) {
			t.Errorf("build-release.sh does not contain %q", want)
		}
	}
	// The pins are read from .assets before it is removed.
	if i, j := strings.Index(build, `ps1-pins.sh" "$DIST/.assets"`), strings.Index(build, `rm -f "$DIST/.assets"`); i < 0 || j < 0 || i > j {
		t.Error("build-release.sh removes dist/.assets before ps1-pins.sh reads it")
	}
	resign := build[strings.Index(build, `if [ "$MODE" = resign ]; then`):]
	resign = resign[:strings.Index(resign, "\nfi\n")]
	if strings.Contains(resign, "install.ps1") {
		t.Error("build-release.sh --resign touches install.ps1")
	}
}

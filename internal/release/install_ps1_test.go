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
	ps1LastLine  = "if ($env:ANET_INSTALL_PS1_TEST -ne '1') { Install-Anet @PSBoundParameters }"
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
	// The script never ends the PowerShell session it runs in.
	if regexp.MustCompile(`(?im)(^|[;{}])\s*exit(\s|$)`).MatchString(code) {
		t.Error("install.ps1 calls exit")
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
		"  [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated\n)\n") {
		t.Error("install.ps1 does not declare the parameters of the specification")
	}
	// Every program is started by the one helper, with an argument array.
	if calls := regexp.MustCompile(`&\s*\$`).FindAllString(code, -1); len(calls) != 2 ||
		strings.Count(code, "& $FilePath @ArgumentList") != 2 {
		t.Errorf("install.ps1 runs programs outside Invoke-AnetNative: %q", calls)
	}
	if regexp.MustCompile(`(?i)start-process|\[diagnostics\.process\]|cmd(\.exe)?\s+/c|2>&1|2>\$null`).MatchString(code) {
		t.Error("install.ps1 starts a program another way, or redirects stderr")
	}
	// The commands of the CLI surface the installer depends on.
	for _, s := range []string{"'--verify-dir'", "'--installed'", "'--shell-controller'", "'--passphrase-later'", "'--without'",
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
	// The refusal of an elevated window, word for word.
	if !strings.Contains(script, "'Run this in a PowerShell window that is not elevated; the node and the commands it runs for the controller run as the user that installs it. To install for an administrator account anyway, add -Elevated.'") {
		t.Error("install.ps1 lacks the refusal of an elevated window")
	}
	// The one line and the verify-first form are in the header.
	for _, s := range []string{
		"#   " + installPS1OneLine("-Controller '<controller AID>'"),
		"#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1 -OutFile install.ps1",
		"#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1.sig -OutFile install.ps1.sig",
		"#   #   ssh-keygen -Y verify -f allowed_signers -I " + Identity + " -n " + Namespace + " -s install.ps1.sig < install.ps1",
		"#   & ([scriptblock]::Create((Get-Content -Raw .\\install.ps1))) -Controller '<AID>'",
	} {
		if !strings.Contains(script, s+"\n") {
			t.Errorf("install.ps1's header lacks %q", s)
		}
	}
}

// installPS1OneLine is the one-line installation command of the
// specification, with args after it.
func installPS1OneLine(args string) string {
	return "[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; " +
		"& ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing -Uri 'https://agentnetwork.org.cn/install.ps1'))) " + args
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

// install.ps1 tries the same hosts in the same order as `anet update` on
// Windows: the project's hosts, then the npm package of the build on the
// npm mirrors.
func TestInstallPS1BasesMatchUpdate(t *testing.T) {
	code := strings.Join(codeLines(installPS1(t)), "\n")
	var want []string
	for _, b := range DefaultBases {
		want = append(want, "$bases.Add('"+b+"')")
	}
	for _, m := range npmMirrors {
		pre, post, _ := strings.Cut(m("PKG"), "PKG")
		want = append(want, "$bases.Add('"+pre+"' + $pkg + '"+post+"')")
	}
	at := strings.Index(code, "$bases.Add((Get-AnetBaseValue -Text $env:ANET_INSTALL_BASE")
	if at < 0 {
		t.Fatal("install.ps1 does not put ANET_INSTALL_BASE first")
	}
	for _, w := range want {
		i := strings.Index(code[at:], w)
		if i < 0 {
			t.Fatalf("install.ps1 does not add the base %s after the previous one", w)
		}
		at += i
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

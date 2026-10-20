package release

import (
	"reflect"
	"strings"
	"testing"
)

func TestNPMPackage(t *testing.T) {
	for _, c := range []struct{ variant, goos, goarch, want string }{
		{"default", "linux", "amd64", "@agentnetwork/anet-linux-x64"},
		{"default", "darwin", "arm64", "@agentnetwork/anet-darwin-arm64"},
		{"shell", "linux", "arm64", "@agentnetwork/anet-shell-linux-arm64"},
		{"default", "windows", "amd64", "@agentnetwork/anet-win32-x64"},
		{"default", "windows", "arm64", "@agentnetwork/anet-win32-arm64"},
		{"shell", "windows", "amd64", "@agentnetwork/anet-shell-win32-x64"},
		{"shell", "windows", "arm64", "@agentnetwork/anet-shell-win32-arm64"},
		{"default", "win32", "amd64", ""},
		{"default", "freebsd", "amd64", ""},
		{"default", "linux", "386", ""},
		{"", "linux", "amd64", ""},
	} {
		if got := NPMPackage(c.variant, c.goos, c.goarch); got != c.want {
			t.Errorf("NPMPackage(%q, %q, %q) = %q, want %q", c.variant, c.goos, c.goarch, got, c.want)
		}
	}
	if got := MirrorBases("default", "freebsd", "amd64"); got != nil {
		t.Errorf("MirrorBases for freebsd = %v, want none", got)
	}
	for _, c := range []struct{ variant, goos, goarch, pkg string }{
		{"shell", "darwin", "amd64", "@agentnetwork/anet-shell-darwin-x64"},
		{"default", "windows", "amd64", "@agentnetwork/anet-win32-x64"},
		{"shell", "windows", "arm64", "@agentnetwork/anet-shell-win32-arm64"},
	} {
		bases := MirrorBases(c.variant, c.goos, c.goarch)
		if len(bases) != 3 {
			t.Errorf("MirrorBases(%q, %q, %q) = %v, want three bases", c.variant, c.goos, c.goarch, bases)
		}
		for _, b := range bases {
			if _, err := normalizeBase(b); err != nil || !strings.Contains(b, c.pkg+"@latest") && !strings.Contains(b, c.pkg+"/latest/files") {
				t.Errorf("mirror base %q: %v", b, err)
			}
		}
	}
}

// install.sh tries the same hosts in the same order as `anet update`: the
// project's hosts, then the npm mirrors of the build; --base alone replaces
// them.
func TestInstallShBasesMatchUpdate(t *testing.T) {
	funcs := []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "mirror_bases"}
	for _, c := range []struct{ flags, variant, goos, goarch string }{
		{"", "default", "linux", "amd64"},
		{"--shell", "shell", "darwin", "arm64"},
	} {
		stdout, stderr, code := runInstallSnippet(t, funcs,
			"parse_args --prefix /x "+c.flags+"; OS_TAG="+c.goos+"; ARCH_TAG="+c.goarch+"; mirror_bases; echo $BASES")
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", c.variant, code, stderr)
		}
		want := append(append([]string{}, DefaultBases...), MirrorBases(c.variant, c.goos, c.goarch)...)
		if got := strings.Fields(stdout); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: install.sh bases\n %v\nanet update bases\n %v", c.variant, got, want)
		}
	}
	stdout, _, code := runInstallSnippet(t, funcs,
		"parse_args --prefix /x --base https://example.org; OS_TAG=linux; ARCH_TAG=amd64; mirror_bases; echo $BASES")
	if code != 0 || strings.TrimSpace(stdout) != "https://example.org" {
		t.Errorf("--base: exit %d, bases %q", code, stdout)
	}
}

// install.sh run from Git Bash, MSYS2 or Cygwin on Windows refuses and
// points at the PowerShell installer, before anything is downloaded.
func TestInstallShRefersMinGWToPowerShell(t *testing.T) {
	funcs := []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "mirror_bases", "detect_platform"}
	for _, uname := range []string{"MINGW64_NT-10.0-19045", "MSYS_NT-10.0-22631", "CYGWIN_NT-10.0"} {
		stdout, stderr, code := runInstallSnippet(t, funcs,
			"uname() { case \"$1\" in -s) echo '"+uname+"' ;; *) echo x86_64 ;; esac; }; parse_args --prefix /x; detect_platform; echo reached")
		want := "Error: this is Windows; install with PowerShell instead: see " +
			"https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html#installing-on-windows\nNothing was installed or changed.\n"
		if code != 1 || stderr != want || strings.Contains(stdout, "reached") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", uname, code, stdout, stderr)
		}
	}
}

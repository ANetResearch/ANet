package release

import (
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
}

// install.sh run from Git Bash, MSYS2 or Cygwin on Windows refuses and
// points at the PowerShell installer, before anything is downloaded.
func TestInstallShRefersMinGWToPowerShell(t *testing.T) {
	funcs := []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "detect_platform"}
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

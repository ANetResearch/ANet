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
		{"default", "windows", "amd64", ""},
		{"default", "linux", "386", ""},
		{"", "linux", "amd64", ""},
	} {
		if got := NPMPackage(c.variant, c.goos, c.goarch); got != c.want {
			t.Errorf("NPMPackage(%q, %q, %q) = %q, want %q", c.variant, c.goos, c.goarch, got, c.want)
		}
	}
	if got := MirrorBases("default", "windows", "amd64"); got != nil {
		t.Errorf("MirrorBases for windows = %v, want none", got)
	}
	for _, b := range MirrorBases("shell", "darwin", "amd64") {
		if _, err := normalizeBase(b); err != nil || !strings.Contains(b, "@agentnetwork/anet-shell-darwin-x64") {
			t.Errorf("mirror base %q: %v", b, err)
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

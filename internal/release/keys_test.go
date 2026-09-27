package release

import (
	"regexp"
	"strings"
	"testing"
)

// The release key as this commit ships it. DEV KEY — 正式发布前由产品负责人替换;
// replacing the key means changing this constant together with
// allowed_signers, NextKeyFingerprint, install.sh, SECURITY.md, README.md
// and docs/GUIDE-zh.md.
const shippedKeyFP = "SHA256:jU+lPusEKAueZbobKBk1MIN+ruBrmyPei8XKAqVfkzA"

func TestEmbeddedTrust(t *testing.T) {
	tr := DefaultTrust()
	if len(tr.Keys) != 1 {
		t.Fatalf("%d release keys embedded, want 1", len(tr.Keys))
	}
	if fp := Fingerprint(tr.Keys[0]); fp != shippedKeyFP {
		t.Fatalf("embedded key is %s, want %s", fp, shippedKeyFP)
	}
	if !fpRe.MatchString(NextKeyFingerprint) || NextKeyFingerprint == shippedKeyFP {
		t.Fatalf("NextKeyFingerprint %q must be a different SHA256 fingerprint", NextKeyFingerprint)
	}
}

// The key is published in four places and checked by three programs. They
// have to be the same key, or a user comparing the README against the
// installer is comparing two different things — and a release signed with
// the key one of them names fails in the others.
func TestTheReleaseKeyIsTheSameEverywhere(t *testing.T) {
	line := strings.TrimSpace(AllowedSigners())
	key := AuthorizedKey(DefaultTrust().Keys[0])

	install := repoFile(t, "deploy/release/install.sh")
	fn := regexp.MustCompile(`(?s)release_allowed_signers\(\) \{\n  echo '([^']*)'\n\}`).FindStringSubmatch(install)
	if fn == nil || fn[1] != line {
		t.Errorf("install.sh's release_allowed_signers does not print internal/release/allowed_signers")
	}
	if !strings.Contains(install, shippedKeyFP) {
		t.Errorf("install.sh does not name the key fingerprint %s", shippedKeyFP)
	}
	if !strings.Contains(install, "echo '"+line+"' > allowed_signers") {
		t.Errorf("install.sh's manual-verification comment does not use the release key")
	}

	for _, doc := range []string{"SECURITY.md", "README.md"} {
		text := repoFile(t, doc)
		for _, want := range []string{line, shippedKeyFP, NextKeyFingerprint, key} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not contain %q", doc, want)
			}
		}
		if !strings.Contains(text, "ssh-keygen -Y verify -f allowed_signers -I "+Identity) {
			t.Errorf("%s does not give the manual verification command", doc)
		}
	}

	// The user guide repeats the key line and its fingerprint in its
	// manual-verification block; a stale copy there is a user checking the
	// installer against a key no release is signed with.
	guide := repoFile(t, "docs/GUIDE-zh.md")
	for _, want := range []string{"echo '" + line + "' > allowed_signers", shippedKeyFP,
		"ssh-keygen -Y verify -f allowed_signers -I " + Identity} {
		if !strings.Contains(guide, want) {
			t.Errorf("docs/GUIDE-zh.md does not contain %q", want)
		}
	}

	// build-release.sh reads the next-key commitment out of keys.go with
	// sed; the line it matches must exist in exactly this form.
	src := repoFile(t, "internal/release/keys.go")
	if !strings.Contains(src, "\nconst NextKeyFingerprint = \""+NextKeyFingerprint+"\"\n") {
		t.Error("keys.go's NextKeyFingerprint line changed shape; build-release.sh reads it with sed")
	}
	build := repoFile(t, "deploy/release/build-release.sh")
	for _, want := range []string{"-n \"$NAMESPACE\"", "NAMESPACE=" + Namespace, "IDENTITY=" + Identity,
		"internal/release/allowed_signers"} {
		if !strings.Contains(build, want) {
			t.Errorf("build-release.sh does not contain %q", want)
		}
	}
}

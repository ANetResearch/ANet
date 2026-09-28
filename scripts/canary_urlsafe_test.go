package scripts

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// URL-safe base64 differs from standard base64 only where the encoding has
// '+' or '/', and ASCII letters, digits and '-' (the alphabet of lib.sh's
// canaries) never produce either; so the b64url case of
// TestTheCanarySearchSeesEveryEncoding is found by the standard needle and
// cannot tell whether the URL-safe search exists at all. A canary with '?'
// and '~' makes the two alphabets differ: a copy held only in URL-safe form
// must still be found, at each of the three alignments. Mutation si1-6
// (canary.py's base64url needles removed) left every canary test green.
func TestTheCanarySearchSeesURLSafeBase64(t *testing.T) {
	needPython(t)
	const c = "anet-canary-url-???~~~>>>0123456789abcdef"
	if base64.URLEncoding.EncodeToString([]byte(c)) == base64.StdEncoding.EncodeToString([]byte(c)) {
		t.Fatal("the test canary does not exercise the URL-safe alphabet")
	}
	dir := t.TempDir()
	canaries := filepath.Join(dir, "canaries.tsv")
	if err := os.WriteFile(canaries, []byte("url\t"+c+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 3; k++ {
		d := filepath.Join(dir, fmt.Sprintf("b64url-%d", k))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		blob := base64.URLEncoding.EncodeToString(append(append(noise(k), c...), noise(5)...))
		if err := os.WriteFile(filepath.Join(d, "blob"), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, code := canaryPy(t, "scan", "--canaries", canaries, "--label", "b64url", d); code != 1 {
			t.Errorf("URL-safe base64 at alignment %d: exit %d, want 1 (found): %s", k, code, out)
		}
	}
}

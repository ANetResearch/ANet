package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// LEC AV-6 (the deploy/release half, EXE-13): what the release ships and
// the website serves — install.sh, SKILL.md, llms.txt and the operator's
// README — teaches no secret on a command line: no bearer token in curl's
// argv, no invite (ANET_INVITE=…, --token, --token-file: there are no
// invitations) and no API key as an argument. install.sh's refusal of
// the removed --token flags is the one place they may be named.
func TestReleaseFilesKeepSecretsOffCommandLines(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(-H|--header)[= ]*["']?(Authorization:\s*Bearer|PAYMENT-SIGNATURE:)`),
		regexp.MustCompile(`Bearer \$\(cat`),
		regexp.MustCompile(`ANET_INVITE=`),
		regexp.MustCompile(`--token([ =]|-file)`),
		regexp.MustCompile(`--api-key[ =]`),
	}
	var files []string
	err := filepath.WalkDir(filepath.Join("..", "deploy", "release"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 6 {
		t.Fatalf("only %d files under deploy/release", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if filepath.Base(f) == "install.sh" && strings.Contains(line, "removed_flag --token") {
				continue // the refusal of the removed flags (exit 2, cli_removed)
			}
			for _, re := range patterns {
				if re.MatchString(line) {
					t.Errorf("%s:%d puts a secret on a command line (%s): %s", f, i+1, re, strings.TrimSpace(line))
				}
			}
		}
	}
}

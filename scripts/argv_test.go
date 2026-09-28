package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// L5 (red team, phase F): the scripts called curl with `-H "Authorization: Bearer $(cat control_token.txt)"`,
// which puts the node's full control credential in curl's argv for every call — readable by every local
// user of the test and production hosts in /proc/<pid>/cmdline (docs/notes/0015: they are multi-user
// machines). A header carrying a credential goes to curl on a file descriptor instead:
// `-H @<(printf 'Authorization: Bearer %s\n' "$tok")` locally, `printf … | curl -H @-` through ssh.
func TestScriptsKeepBearerTokensOffCommandLines(t *testing.T) {
	onArgv := regexp.MustCompile(`(-H|--header)[= ]*["']?Authorization:\s*Bearer`)
	var files []string
	for _, g := range []string{"*.sh", "testnet/*.sh", "mutations/*.sh"} {
		m, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("only %d scripts found", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if onArgv.MatchString(line) {
				t.Errorf("%s:%d puts a bearer token on curl's command line: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

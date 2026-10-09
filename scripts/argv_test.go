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
// user of the test and production hosts in /proc/<pid>/cmdline (an internal note: they are multi-user
// machines). A header carrying a credential goes to curl on a file descriptor instead:
// `-H @<(printf 'Authorization: Bearer %s\n' "$tok")` locally, `printf … | curl -H @-` through ssh. The same
// holds for a signed x402 payment header (PAYMENT-SIGNATURE): whoever presents it first to the gateway
// gets what it pays for.
//
// LEC AV-6 adds what else must not be on a command line here: a token read into a header with
// `Bearer $(cat …)`, and a model API key given as `--api-key`, which 0.3.0 refuses (api_key_on_argv: the
// key goes in on stdin, `--api-key-stdin`); the console's prompts for an operator's agent are scanned
// with the documentation. What the release ships and the website serves has its own,
// wider scan (argv_release_test.go), and the code itself is checked by execargs_ast_test.go.
func TestScriptsKeepBearerTokensOffCommandLines(t *testing.T) {
	onArgv := []*regexp.Regexp{
		regexp.MustCompile(`(-H|--header)[= ]*["']?(Authorization:\s*Bearer|PAYMENT-SIGNATURE:)`),
		regexp.MustCompile(`Bearer \$\(cat`),
		// --api-key in any spelling but --api-key-stdin: with a value, or named as the flag to add.
		regexp.MustCompile(`--api-key([^-\w]|$)`),
	}
	var files []string
	for _, g := range []string{"*.sh", "testnet/*.sh", "mutations/*.sh", "interop/*.sh"} {
		m, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("only %d scripts found", len(files))
	}
	// And what the documentation tells people to type [L5, second pass]: GUIDE §6.8 showed the local A2A
	// token on curl's command line, the pattern the scripts were fixed for.
	docs, err := filepath.Glob("../docs/*.md")
	if err != nil || len(docs) < 5 {
		t.Fatalf("docs: %d found, %v", len(docs), err)
	}
	files = append(files, docs...)
	// And the prompts the console hands to an operator's agent to run (internal/daemon/web).
	pages, err := filepath.Glob("../internal/daemon/web/*.html")
	if err != nil || len(pages) == 0 {
		t.Fatalf("console pages: %d found, %v", len(pages), err)
	}
	files = append(files, pages...)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, re := range onArgv {
				if re.MatchString(line) {
					t.Errorf("%s:%d puts a secret on a command line (%s): %s", f, i+1, re, strings.TrimSpace(line))
				}
			}
		}
	}
}

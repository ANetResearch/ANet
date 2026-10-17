package release

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// MOD-7a: a manifest without the variant this binary says it is gives a typed
// error, which anet update turns into update_variant_unknown.mismatch.
func TestUpdateNoSuchVariantIsTyped(t *testing.T) {
	f := newFixture(t)
	srv := f.server()
	u, exe := f.updater(srv, "0.1.0")
	u.Variant = "nosuch"
	if _, err := u.Check(context.Background()); !errors.Is(err, ErrNoVariant) {
		t.Fatalf("got %v, want ErrNoVariant", err)
	}
	untouched(t, exe)
}

// runInstallSnippet runs body after the named install.sh functions are
// defined, with extra shell prelude, and returns stdout, stderr and the exit
// status.
func runInstallSnippet(t *testing.T, funcs []string, body string) (string, string, int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := "set -eu\n" + installFuncs(t, funcs...) + "\n" + body + "\n"
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "ANET_LANG=en"}
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

var withoutFuncs = []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "check_without"}

// MOD-7a (2): --without is collected once per name, the shell contradiction is
// refused before anything runs, and a name outside the chosen variant stops
// the install.
func TestInstallShWithoutArguments(t *testing.T) {
	stdout, stderr, code := runInstallSnippet(t, withoutFuncs,
		`parse_args --prefix /x --without autoreply,x402 --without=x402,preview; echo "[$WITHOUT]"`)
	if code != 0 || strings.TrimSpace(stdout) != "[autoreply,x402,preview]" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, c := range []struct{ name, args string }{
		{"shell with --without shell", "--shell --without shell"},
		{"--without shell before --shell", "--without=shell --shell"},
		{"no value", "--without"},
		{"empty value", "--without="},
		{"bad characters", "--without 'a;rm'"},
	} {
		_, stderr, code := runInstallSnippet(t, withoutFuncs, "parse_args --prefix /x "+c.args)
		if code != 1 || !strings.Contains(stderr, "Nothing was installed or changed.") {
			t.Errorf("%s: exit %d, stderr %q", c.name, code, stderr)
		}
	}
	// The module must be in the chosen variant's list; shell is accepted on
	// the default variant (anet init takes it as a no-op).
	check := func(mods, without string) (string, int) {
		_, stderr, code := runInstallSnippet(t, withoutFuncs,
			"VARIANT=default; MODS='"+mods+"'; WITHOUT='"+without+"'; check_without")
		return stderr, code
	}
	if _, code := check("a2a,autoreply,x402", "autoreply,x402"); code != 0 {
		t.Errorf("modules of the variant refused")
	}
	if _, code := check("a2a,x402", "shell"); code != 0 {
		t.Errorf("--without shell on the default variant refused")
	}
	if stderr, code := check("a2a,x402", "autoreply"); code != 1 || !strings.Contains(stderr, "no module of that name") ||
		!strings.Contains(stderr, "Nothing was installed or changed.") {
		t.Errorf("a name outside the variant: exit %d, %q", code, stderr)
	}
}

// MOD-7a (2): apply_without runs the staged binary's `init --without`; its
// refusal ends the install with its own words and status, and a success is
// shown.
func TestInstallShApplyWithout(t *testing.T) {
	dir := t.TempDir()
	fake := func(body string) string {
		p := filepath.Join(dir, "anet")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	funcs := append(withoutFuncs, "apply_without")
	log := filepath.Join(dir, "calls")
	bin := fake(`printf '%s\n' "$*" >> ` + log + `; echo "without: autoreply"`)
	stdout, _, code := runInstallSnippet(t, funcs, "WITHOUT=autoreply; apply_without "+bin)
	calls, _ := os.ReadFile(log)
	if code != 0 || strings.TrimSpace(string(calls)) != "init --without autoreply" || !strings.Contains(stdout, "without: autoreply") {
		t.Fatalf("exit %d, calls %q, stdout %q", code, calls, stdout)
	}
	sentence := a2ashape.Sentence("without_configured", a2ashape.English, a2ashape.Args{"key": "auto_reply", "m": "autoreply"})
	bin = fake(`echo '` + sentence + `' >&2; exit 2`)
	_, stderr, code := runInstallSnippet(t, funcs, "WITHOUT=autoreply; apply_without "+bin)
	if code != 2 || !strings.Contains(stderr, sentence) || !strings.Contains(stderr, "Nothing was installed or changed.") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	// No names: the binary is not run at all.
	bin = fake(`echo ran >> ` + log + `; exit 9`)
	os.Remove(log)
	if _, _, code = runInstallSnippet(t, funcs, "WITHOUT=; apply_without "+bin); code != 0 {
		t.Fatalf("empty list: exit %d", code)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("the binary ran with an empty --without list")
	}
	// The order of install_binary: the self-check, then the write of
	// config.json, then the replacement of the installed binary.
	script := repoFile(t, "deploy/release/install.sh")
	i, j, k := strings.Index(script, `check_binary "$STAGED"`), strings.Index(script, `apply_without "$STAGED"`), strings.Index(script, `mv -f "$STAGED" "$DEST"`)
	if !(0 < i && i < j && j < k) {
		t.Errorf("install_binary order: check_binary %d, apply_without %d, mv %d", i, j, k)
	}
}

// MOD-7a (2): the statement at the top of install.sh about the default
// binary is the registered text, in both languages, for the name table as it
// is (the form follows whether autoreply has a tag).
func TestInstallShHeaderStatement(t *testing.T) {
	script := repoFile(t, "deploy/release/install.sh")
	info, ok := module.Lookup("autoreply")
	if !ok {
		t.Fatal("no autoreply in the name table")
	}
	tagged := info.Tag != ""
	for _, lang := range []a2ashape.Language{a2ashape.Chinese, a2ashape.English} {
		want := "#   " + a2ashape.InstallHeader(lang, tagged) + "\n"
		if !strings.Contains(script, want) {
			t.Errorf("install.sh lacks the statement for %v (autoreply tagged: %v):\n%s", lang, tagged, want)
		}
		other := "#   " + a2ashape.InstallHeader(lang, !tagged) + "\n"
		if strings.Contains(script, other) {
			t.Errorf("install.sh carries the statement of the other Tag value for %v", lang)
		}
	}
	if strings.Contains(script, "cannot execute anything on the machine") {
		t.Error("install.sh still says the default binary cannot execute anything on the host")
	}
	// Both forms say what the registered text says.
	for _, tagged := range []bool{false, true} {
		zh, en := a2ashape.InstallHeader(a2ashape.Chinese, tagged), a2ashape.InstallHeader(a2ashape.English, tagged)
		for _, s := range []string{"--without autoreply", "shell"} {
			if !strings.Contains(zh, s) || !strings.Contains(en, s) {
				t.Errorf("tagged=%v: statement lacks %q", tagged, s)
			}
		}
		if strings.Contains(zh, "build.sh --without autoreply") != tagged || strings.Contains(en, "build.sh --without autoreply") != tagged {
			t.Errorf("tagged=%v: the build.sh sentence does not follow the Tag", tagged)
		}
	}
}

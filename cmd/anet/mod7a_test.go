package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/version"
	"github.com/ANetResearch/ANet/module"
)

func bashOut(t *testing.T, env []string, script string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(repoRoot, script)}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
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

// recipes is `tagcheck.sh recipes`: name → tag set.
func recipes(t *testing.T) map[string]string {
	t.Helper()
	out, errs, code := bashOut(t, nil, "scripts/tagcheck.sh", "recipes")
	if code != 0 {
		t.Fatalf("tagcheck.sh recipes: exit %d: %s", code, errs)
	}
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		name, tags, ok := strings.Cut(l, "\t")
		if !ok {
			t.Fatalf("recipes line %q has no tab", l)
		}
		m[name] = tags
	}
	return m
}

// MOD-7a (6): the recipes have one source; the pluggable job of ci.yml and
// the min tier of onboard.sh agree with it.
func TestRecipesMatchCI(t *testing.T) {
	rec := recipes(t)
	names := make([]string, 0, len(rec))
	for n := range rec {
		names = append(names, n)
	}
	sort.Strings(names)
	if want := []string{"agent", "full", "min", "p2p", "paid", "standard"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("recipes %v, want %v", names, want)
	}
	sub, _ := tagLists(t)
	// min is every subtractive tag; full is none.
	var all []string
	for _, m := range keys(sub) {
		all = append(all, "no_"+m)
	}
	sort.Strings(all)
	if rec["min"] != strings.Join(all, ",") {
		t.Errorf("min = %q, want every subtractive tag %q", rec["min"], strings.Join(all, ","))
	}
	if rec["full"] != "" {
		t.Errorf("full = %q, want no tags", rec["full"])
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]bool{}
	for _, m := range pluggableRE.FindAllSubmatch(b, -1) {
		rows[string(m[1])] = true
	}
	for n, tags := range rec {
		if tags == "" {
			continue
		}
		for _, tag := range strings.Split(tags, ",") {
			if !strings.HasPrefix(tag, "no_") || !sub[strings.TrimPrefix(tag, "no_")] {
				t.Errorf("recipe %s names %q, which is not a subtractive tag", n, tag)
			}
		}
		if !rows[tags] {
			t.Errorf("ci.yml pluggable has no row %q (recipe %s)", tags, n)
		}
	}
	// onboard.sh takes its tiers from the recipes and carries none of its own.
	ob, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "onboard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ob), "scripts/tagcheck.sh recipes") {
		t.Error("onboard.sh does not read the recipes from scripts/tagcheck.sh")
	}
	if regexp.MustCompile(`\[(min|standard|paid)\]=`).Match(ob) {
		t.Error("onboard.sh still carries a literal tag set")
	}
}

// build.sh and tagcheck.sh name the same modules as the name table: a name
// with a tag is in `list`, a name without one is in `pending`.
func TestTagcheckPendingMatchesNameTable(t *testing.T) {
	out, _, code := bashOut(t, nil, "scripts/tagcheck.sh", "pending")
	if code != 0 {
		t.Fatal("tagcheck.sh pending failed")
	}
	var want []string
	for _, k := range module.Known() {
		if k.Tag == "" {
			want = append(want, k.Name)
		}
	}
	got := strings.Fields(out)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tagcheck.sh pending = %v, the name table's modules without a Tag = %v", got, want)
	}
}

// MOD-7a (3): build.sh --without a module that has no tag yet says
// build_tag_pending, word for word as the reason table has it, with exit
// status 2 and nothing built; an unknown name or an odd --with is an error of
// its own.
func TestBuildShTagPending(t *testing.T) {
	for _, c := range []struct {
		env  string
		lang a2ashape.Language
	}{{"LANG=C", a2ashape.English}, {"LANG=zh_CN.UTF-8", a2ashape.Chinese}} {
		for _, m := range []string{"preview", "console"} {
			_, stderr, code := bashOut(t, []string{c.env}, "build.sh", "--without", "x402,"+m)
			want := a2ashape.Sentence("build_tag_pending", c.lang, a2ashape.Args{"m": m}) + "\n"
			if code != 2 || stderr != want {
				t.Errorf("%s %s: exit %d, stderr %q; want 2 and %q", c.env, m, code, stderr, want)
			}
		}
	}
	for _, args := range [][]string{{"--without", "nosuch"}, {"--with", "x402"}, {"--without"}, {"--bogus"}} {
		_, stderr, code := bashOut(t, []string{"LANG=C"}, "build.sh", args...)
		if code != 2 || stderr == "" {
			t.Errorf("build.sh %v: exit %d, stderr %q", args, code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "anet")); err == nil {
		t.Error("a refused build.sh left a binary")
	}
}

// MOD-7a (5): TestUpdateKeepsTheVariant. A release whose default has one more
// subtractive module still updates a default; shell never turns into default
// or the other way round.
func TestUpdateKeepsTheVariant(t *testing.T) {
	def := []string{"a2a", "cas", "x402"}
	for _, c := range []struct {
		name             string
		compiled, target []string
		mismatch         bool
	}{
		{"same", def, def, false},
		{"target has one more subtractive module", def, append(append([]string{}, def...), "autoreply"), false},
		{"target has one fewer", def, []string{"a2a", "x402"}, false},
		{"shell binary, default target", append(append([]string{}, def...), "shell"), def, true},
		{"default binary, shell target", def, append(append([]string{}, def...), "shell"), true},
		{"shell to shell", append(append([]string{}, def...), "shell"), append(append([]string{}, def...), "shell", "autoreply"), false},
	} {
		if got := additiveMismatch(c.compiled, c.target); got != c.mismatch {
			t.Errorf("%s: mismatch %v, want %v", c.name, got, c.mismatch)
		}
	}
	// A build from source has no variant: update_variant_unknown, exit 1,
	// before any network request (the base is not even parsed).
	old := version.Variant
	version.Variant = ""
	defer func() { version.Variant = old }()
	r, w, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = w
	err := runUpdate([]string{"--base", "https://127.0.0.1:1"})
	os.Stderr = stderr
	w.Close()
	b, _ := io.ReadAll(r)
	if err != errExit1 {
		t.Fatalf("runUpdate = %v, want errExit1", err)
	}
	want := a2ashape.Sentence("update_variant_unknown", a2ashape.Lang(), nil)
	if !strings.Contains(string(b), want) {
		t.Errorf("stderr %q does not hold %q", b, want)
	}
}

// MOD-7a (4): `anet version` prints the variant line.
func TestVersionPrintsTheVariant(t *testing.T) {
	h := newCLIHome(t, "C")
	out, _, code := runCLI(t, h, []string{"version"})
	if code != 0 || !strings.Contains(out, "\nvariant: none\n") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func readFileT(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// MOD-7a (1): anet init --without / --with, through the command.
func TestInitCommandWithout(t *testing.T) {
	h := newCLIHome(t, "C")
	cfg := filepath.Join(h.data, "config.json")
	out, stderr, code := runCLI(t, h, []string{"init", "--without", "x402,cas"})
	if code != 0 || !strings.Contains(out, "without: cas, x402") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	// TestInitWithoutOnlyAdds, at the command: a second call adds, never drops.
	if out, _, code = runCLI(t, h, []string{"init", "--without", "service", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep struct {
		Without struct {
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
			Without []string `json:"without"`
		} `json:"without"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Without.Added, []string{"service"}) || !reflect.DeepEqual(rep.Without.Without, []string{"cas", "service", "x402"}) {
		t.Errorf("json report %+v", rep.Without)
	}
	// TestInitWithRemovesOneName.
	if out, _, code = runCLI(t, h, []string{"init", "--with", "cas"}); code != 0 || !strings.Contains(out, "without: service, x402") {
		t.Fatalf("--with: exit %d, %q", code, out)
	}
	// Refusals: exit 2, the sentence and its next step, the file untouched.
	before := readFileT(t, cfg)
	for name, args := range map[string][]string{
		"unknown":      {"init", "--without", "nosuch"},
		"no value":     {"init", "--without"},
		"both":         {"init", "--without", "p2p", "--with", "p2p"},
		"positional":   {"init", "x402"},
		"unknown with": {"init", "--with", "nosuch"},
	} {
		if _, stderr, code = runCLI(t, h, args); code != 2 || stderr == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, stderr)
		}
	}
	// TestInitWithoutRefusesConfiguredModule.
	var m map[string]any
	if err := json.Unmarshal(before, &m); err != nil {
		t.Fatal(err)
	}
	m["modules"] = map[string]any{"p2p": map[string]any{}}
	nb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(cfg, nb, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runCLI(t, h, []string{"init", "--without", "p2p"})
	want := a2ashape.Sentence("without_configured", a2ashape.English, a2ashape.Args{"key": "modules.p2p", "m": "p2p"})
	if code != 2 || !strings.Contains(stderr, want) {
		t.Errorf("configured: exit %d, stderr %q, want %q", code, stderr, want)
	}
	if string(readFileT(t, cfg)) != string(nb) {
		t.Error("the refused command changed config.json")
	}
	// daemon_running: exit 1, the file untouched, for --without and --with.
	lock, err := daemon.AcquireDataLock(daemon.NewLayout(h.data))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	for _, args := range [][]string{{"init", "--without", "cas"}, {"init", "--with", "x402"}} {
		_, stderr, code = runCLI(t, h, args)
		want = a2ashape.Sentence("daemon_running", a2ashape.English, a2ashape.Args{"path": h.data})
		if code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	if string(readFileT(t, cfg)) != string(nb) {
		t.Error("init changed config.json under a running daemon")
	}
}

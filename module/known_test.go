package module

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// K-1: the table holds the thirteen names of the release, alphabetically,
// and each row has what the rest of the repository relies on.
func TestKnownNameTable(t *testing.T) {
	want := []string{"a2a", "anetlink", "autoreply", "blackboard", "cas", "console", "mcp", "org", "p2p", "preview",
		"service", "shell", "x402"}
	var got []string
	for _, k := range Known() {
		got = append(got, k.Name)
		if k.Zh == "" || k.En == "" {
			t.Errorf("%s: no names for a sentence", k.Name)
		}
		if k.Additive != (k.Tag == "shell") || (k.Tag != "" && !k.Additive && k.Tag != "no_"+k.Name) {
			t.Errorf("%s: tag %q, additive %v", k.Name, k.Tag, k.Additive)
		}
		if _, ok := Lookup(k.Name); !ok {
			t.Errorf("Lookup(%s) failed", k.Name)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("names %v, want %v", got, want)
	}
	// The two that have no tag yet and whose absent path has landed (console's with MOD-8, autoreply's with
	// MOD-3 -- neither has a tag until MOD-4 and v0.3.x).
	for _, n := range []string{"autoreply", "console"} {
		k, _ := Lookup(n)
		if k.Tag != "" || !k.AbsenceReady {
			t.Errorf("%s: tag %q, AbsenceReady %v", n, k.Tag, k.AbsenceReady)
		}
	}
	if _, ok := Lookup("nothing"); ok {
		t.Error("Lookup found a name that is not there")
	}
	// A copy: changing it does not change the table.
	k := Known()
	k[0].Name = "changed"
	k[0].Grants[0] = "changed"
	if Known()[0].Name != "a2a" || Known()[0].Grants[0] == "changed" {
		t.Error("Known returns the table itself")
	}
}

// The module names in sentences (a2ashape) are the table's.
func TestModuleNamesInSentencesAreTheTables(t *testing.T) {
	for _, k := range Known() {
		if a2ashape.ModuleName(k.Name, a2ashape.Chinese) != k.Zh || a2ashape.ModuleName(k.Name, a2ashape.English) != k.En {
			t.Errorf("%s: a2ashape says %q / %q, the table %q / %q", k.Name, a2ashape.ModuleName(k.Name, a2ashape.Chinese),
				a2ashape.ModuleName(k.Name, a2ashape.English), k.Zh, k.En)
		}
	}
}

func TestNewAvailability(t *testing.T) {
	av, err := NewAvailability([]string{"x402", "cas"})
	if err != nil {
		t.Fatal(err)
	}
	if got := av.Without(); !slices.Equal(got, []string{"cas", "x402"}) {
		t.Errorf("Without = %v, want sorted", got)
	}
	for _, c := range []struct {
		without []string
		rule    string
	}{
		{[]string{"nope"}, ""},
		{[]string{"cas", "cas"}, ""},
	} {
		_, err := NewAvailability(c.without)
		var ref *a2ashape.Refusal
		if !errors.As(err, &ref) || ref.Key != "config_invalid" || ref.Args["key"] != "without" {
			t.Errorf("%v: %v, want config_invalid naming without", c.without, err)
			continue
		}
		if c.rule != "" && ref.Args["rule"] != c.rule {
			t.Errorf("%v: rule %v", c.without, ref.Args["rule"])
		}
	}
	// The zero value turns nothing off, and the value is not shared.
	var zero Availability
	if len(zero.Without()) != 0 {
		t.Error("the zero value has a without")
	}
	if av.Without()[0] != "cas" {
		t.Error("Without changed")
	}
	got := av.Without()
	got[0] = "changed"
	if av.Without()[0] != "cas" {
		t.Error("Without returns the value's own slice")
	}
}

// A module is available when it is compiled in and not in without; the
// reason it is not is one of three.
func TestAvailabilityAndAbsence(t *testing.T) {
	resetRegistry(t)
	Register("cas", func([]byte) (Module, error) { return nil, nil })
	DeclareCompiled("mcp")
	av, _ := NewAvailability([]string{"cas"})
	for _, c := range []struct {
		name   string
		avail  bool
		reason string
	}{
		{"cas", false, AbsentWithout},
		{"mcp", true, ""},
		{"x402", false, AbsentNotCompiled},
		{"shell", false, AbsentNotCompiledAdd},
		{"nothing", false, AbsentNotCompiled},
	} {
		if av.Available(c.name) != c.avail || av.Absence(c.name) != c.reason {
			t.Errorf("%s: available %v (want %v), reason %q (want %q)", c.name, av.Available(c.name), c.avail,
				av.Absence(c.name), c.reason)
		}
	}
	if av.Absent("mcp", "") != nil {
		t.Error("Absent for an available module")
	}
	ref := av.Absent("cas", "configured")
	if ref == nil || ref.Key != "module_absent.configured" || ref.Args["m"] != "cas" || ref.Args["reason"] != AbsentWithout {
		t.Errorf("Absent = %+v", ref)
	}
	// Two values do not share their without.
	other, _ := NewAvailability(nil)
	if !other.Available("cas") {
		t.Error("another value inherited the first one's without")
	}
}

// resetRegistry empties the registry for a test and restores it after.
func resetRegistry(t *testing.T) {
	t.Helper()
	regMu.Lock()
	savedReg, savedExtra := registry, extraCompiled
	registry, extraCompiled = nil, nil
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		registry, extraCompiled = savedReg, savedExtra
		regMu.Unlock()
	})
}

func TestBuildWithSkipsTurnedOffModulesAndRefusesTheirBlocks(t *testing.T) {
	resetRegistry(t)
	called := 0
	Register("cas", func([]byte) (Module, error) { called++; return nil, nil })
	Register("org", func([]byte) (Module, error) { called++; return nil, nil })
	av, _ := NewAvailability([]string{"cas"})
	if _, err := BuildWith(av, nil); err != nil || called != 1 {
		t.Fatalf("BuildWith: %v, %d factories called, want 1 (cas is turned off)", err, called)
	}
	_, err := BuildWith(av, map[string][]byte{"cas": []byte(`{}`)})
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "module_absent.configured" || ref.Args["key"] != "modules.cas" {
		t.Fatalf("a block for a module that is turned off: %v", err)
	}
	// A module that is not in the binary, a name not in the table, one with
	// a top-level key, one that takes no block.
	for name, key := range map[string]string{"x402": "module_absent.configured", "nothing": "config_invalid",
		"autoreply": "config_invalid", "mcp": "config_invalid"} {
		_, err := Build(map[string][]byte{name: []byte(`{}`)})
		if !errors.As(err, &ref) || ref.Key != key {
			t.Errorf("a block for %s: %v, want %s", name, err, key)
		}
	}
}

func TestGrantsAndEvidenceTypes(t *testing.T) {
	x, _ := Lookup("x402")
	for ty, want := range map[string]bool{"anet.payment.authorized": true, "anet.credit.issued": true,
		"anet.supply.proof": true, "anet.supply.proofs": false, "anet.payment.": false, "anet.policy.changed": false,
		"anet.shell.command": false} {
		if x.MayRecord(ty) != want {
			t.Errorf("x402 MayRecord(%s) = %v, want %v", ty, !want, want)
		}
	}
	if !MayRecord("shell", "anet.shell.refused") || MayRecord("cas", "anet.shell.refused") || MayRecord("nothing", "x") {
		t.Error("MayRecord by name")
	}
	if !Granted("p2p", GrantTransportHost) || Granted("p2p", GrantProviders) || Granted("nothing", GrantProviders) {
		t.Error("Granted by name")
	}
	// A policy event is never a module's, whatever its row says.
	for _, k := range Known() {
		for _, ty := range k.EvidenceTypes {
			if strings.HasPrefix(ty, "anet.policy.") {
				t.Errorf("%s lists %s", k.Name, ty)
			}
		}
		if len(k.EvidenceTypes) > 0 && !k.Granted(GrantRecordEvidence) {
			t.Errorf("%s lists evidence types without the grant", k.Name)
		}
		if k.Granted(GrantRecordEvidence) && len(k.EvidenceTypes) == 0 {
			t.Errorf("%s may record evidence but no type", k.Name)
		}
	}
}

func TestRouteOwners(t *testing.T) {
	if m, ok := RouteOwner("POST /redeem"); !ok || m != "x402" {
		t.Errorf("owner of /redeem: %q %v", m, ok)
	}
	if _, ok := RouteOwner("POST /status"); ok {
		t.Error("/status is a kernel route")
	}
	if !OwnsRoute("autoreply", "POST /autoreply") || OwnsRoute("x402", "POST /autoreply") {
		t.Error("OwnsRoute")
	}
	// A route belongs to one module.
	seen := map[string]string{}
	for _, k := range Known() {
		for _, r := range k.Routes {
			if prev, dup := seen[r]; dup {
				t.Errorf("%s is owned by %s and %s", r, prev, k.Name)
			}
			seen[r] = k.Name
		}
	}
}

// repoRoot is this repository's root (the tests of this package run in
// module/).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// tagcheck.sh and the table agree: the same names have tags, and the same
// packages are counted (the table's Packages are what the script's pattern
// calls module/<m> and provider/<m>, plus the two mcp packages).
func TestTagcheckAgreesWithTheTable(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	root := repoRoot(t)
	run := func(args ...string) string {
		cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "tagcheck.sh")}, args...)...)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("tagcheck.sh %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	var tags []string
	for _, k := range Known() {
		if k.Tag != "" {
			tags = append(tags, k.Tag)
		}
	}
	listed := strings.Fields(run("list"))
	sort.Strings(tags)
	sort.Strings(listed)
	if !slices.Equal(tags, listed) {
		t.Errorf("tagcheck.sh list = %v, the table's tags = %v", listed, tags)
	}
	for _, k := range Known() {
		if k.Tag == "" {
			continue
		}
		var inTree []string
		for _, part := range strings.Split(run("pattern", k.Name), "|") {
			if !strings.HasPrefix(part, "module/") && !strings.HasPrefix(part, "provider/") && !strings.HasPrefix(part, "internal/") {
				continue
			}
			if m, _ := filepath.Glob(filepath.Join(root, part, "*.go")); len(m) > 0 {
				inTree = append(inTree, part)
			}
		}
		sort.Strings(inTree)
		pk := slices.Clone(k.Packages)
		sort.Strings(pk)
		if !slices.Equal(inTree, pk) {
			t.Errorf("%s: tagcheck.sh counts %v, the table lists %v", k.Name, inTree, pk)
		}
	}
}

// TestAvailabilityThroughOneFunction (MOD-1, §5.1): the registry is read for
// "is the module there" in one place. The module package has no package
// variable of type Availability, and module.Compiled() is called only where
// a list of what a binary contains is the point: anet version, doctor's
// modules row and the daemon's /status.build go through the value.
func TestAvailabilityThroughOneFunction(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		"cmd/anet/main.go":   true, // anet version
		"cmd/anet/update.go": true, // the modules a release must match
		"cmd/anet/doctor.go": true, // doctor's modules row
	}
	call := regexp.MustCompile(`\bmodule\.Compiled\(\)`)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if call.Match(b) && !allowed[rel] {
			t.Errorf("%s calls module.Compiled(); ask an Availability value", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// No package-level Availability in package module.
	ents, _ := filepath.Glob(filepath.Join(root, "module", "*.go"))
	pkgVar := regexp.MustCompile(`(?m)^var\s+\w+\s+(=\s*)?(module\.)?Availability\b|^var\s+\w+\s*=\s*Availability\{|^var\s+\w+\s*=\s*NewAvailability`)
	for _, f := range ents {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		if pkgVar.Match(b) {
			t.Errorf("%s has a package-level Availability", f)
		}
	}
}

package daemon

// The macOS Seatbelt profile (LEC MS-2, SI-64, G-SBPL), checked on every platform: it is text built from
// paths and a port. The probe that runs it on a Mac is in sandbox_darwin_test.go.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateSeatbeltGolden = flag.Bool("update-seatbelt", false, "rewrite testdata/seatbelt from the code")

// goldenSeatbeltSpec is the fixed input of G-SBPL: an anet binary installed for the user, a claude
// runtime in the user's home and a node from Homebrew, a run and a work dir under the user's cache.
func goldenSeatbeltSpec(port int) seatbeltSpec {
	runtime := []string{"/Users/u/.local/bin", "/Users/u/.local/share/claude/versions/1.0.0",
		"/opt/homebrew/Cellar/node/22.1.0/bin"}
	return seatbeltSpec{
		Init:       "/Users/u/.local/bin/anet",
		RunDir:     "/Users/u/Library/Caches/anet/run/Golden01-0123456789",
		WorkDir:    "/Users/u/Library/Caches/anet/work-sandbox/EaidForGolden001/ix-golden",
		Runtime:    runtime,
		ReadOnly:   seatbeltPackageTrees(runtime),
		Port:       port,
		NeverAllow: []string{"/Users/u/.anet", "/Users/u/.claude", "/Users/u/Library/Caches/anet/work", "/private/tmp/anet-501"},
	}
}

// G-SBPL: the profile for the fixed input, with and without an egress endpoint, is byte for byte the
// vector in testdata/seatbelt.
func TestSeatbeltProfileGolden(t *testing.T) {
	for name, port := range map[string]int{"run": 49152, "no-egress": 0} {
		got, err := buildSeatbeltProfile(goldenSeatbeltSpec(port))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "seatbelt", name+".sb")
		if *updateSeatbeltGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run go test -run %s -update-seatbelt to create it)", err, t.Name())
		}
		if !bytes.Equal([]byte(got), want) {
			t.Errorf("%s differs from the code's output (%d bytes, want %d); if the change is intended, rerun with -update-seatbelt and review the diff",
				path, len(got), len(want))
		}
	}
}

// SI-64, MS-2: no Mach service the profile allows matches a forbidden name (the keychain and security
// daemons, the pasteboard, Launch Services, TCC, the window server), each forbidden name is refused at the
// end of the profile, after every allow, and a list that broke the rule would make every profile fail.
func TestSeatbeltMachServicesAvoidForbiddenNames(t *testing.T) {
	for _, name := range seatbeltMachServices {
		if pat, bad := seatbeltMachForbiddenMatch(name); bad {
			t.Errorf("the allowed Mach service %s matches the forbidden %s", name, pat)
		}
	}
	// The matcher itself, on names each pattern must and must not catch.
	for name, want := range map[string]bool{
		"com.apple.SecurityServer": true, "com.apple.securityd": true, "com.apple.securityd.xpc": true,
		"com.apple.secd": true, "com.apple.security.agent": true, "com.apple.trustd.agent": true,
		"com.apple.pasteboard.1": true, "com.apple.coreservices.launchservicesd": true, "com.apple.lsd.mapdb": true,
		"com.apple.tccd.system": true, "com.apple.windowserver.active": true,
		"com.apple.system.opendirectoryd.libinfo": false, "com.apple.security": false, "com.apple.logd": false,
		"com.apple.SecurityServerX": false, "com.apple.lsd": false,
	} {
		if _, got := seatbeltMachForbiddenMatch(name); got != want {
			t.Errorf("%s: forbidden %v, want %v", name, got, want)
		}
	}
	p, err := buildSeatbeltProfile(goldenSeatbeltSpec(49152))
	if err != nil {
		t.Fatal(err)
	}
	forms := mustParseSBPL(t, p)
	allowAt, denyAt := lastRuleIndex(forms, "allow", "mach-lookup"), lastRuleIndex(forms, "deny", "mach-lookup")
	if allowAt < 0 || denyAt < allowAt {
		t.Fatalf("mach-lookup: last allow at %d, deny at %d", allowAt, denyAt)
	}
	var denied []string
	for _, f := range forms[denyAt].list[2:] {
		denied = append(denied, f.list[0].atom+" "+f.list[1].atom)
	}
	for _, pat := range seatbeltMachForbidden {
		want := "global-name " + pat
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			want = "global-name-prefix " + prefix
		}
		if !slices.Contains(denied, want) {
			t.Errorf("the profile does not refuse %s at its end (denied: %v)", pat, denied)
		}
	}
	// Every allowed name appears, and nothing but those.
	var allowed []string
	for _, f := range forms[allowAt].list[2:] {
		allowed = append(allowed, f.list[1].atom)
	}
	if !slices.Equal(allowed, seatbeltMachServices) {
		t.Errorf("allowed Mach services %v, want %v", allowed, seatbeltMachServices)
	}
	saved := seatbeltMachServices
	t.Cleanup(func() { seatbeltMachServices = saved })
	seatbeltMachServices = append(slices.Clone(saved), "com.apple.securityd.xpc")
	if _, err := buildSeatbeltProfile(goldenSeatbeltSpec(49152)); err == nil {
		t.Error("a profile allowing com.apple.securityd.xpc was built")
	}
}

// MS-2: the profile's shape. It starts from (deny default) and imports nothing; processes are forked and
// executed only from /bin, /usr/bin, path_helper, the anet binary and the runtime, and /usr/bin/security,
// osascript and open are refused after every allow; process information and signals stay within the
// sandbox; the only network is out to the endpoint's port on the loopback (none without an endpoint),
// and binding, accepting and Apple events are refused; files are written only in the work dir, the run's
// home/ and tmp/ and the descriptors a process holds; other processes' arguments cannot be read. What
// Seatbelt allows by default even under (deny default) is refused explicitly: process information outside
// the sandbox (refused before the allow within it), hard links, clones, NVRAM, and the two fcntl commands
// that change a file through a descriptor opened for reading (F_MAKECOMPRESSED 80, F_TRANSFEREXTENTS 110).
func TestSeatbeltProfileHasTheSandboxShape(t *testing.T) {
	spec := goldenSeatbeltSpec(49152)
	p, err := buildSeatbeltProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, "(version 1)\n(deny default)\n") {
		t.Errorf("the profile does not start from (deny default):\n%.80s", p)
	}
	forms := mustParseSBPL(t, p)
	for _, f := range forms {
		head := f.list[0].atom
		if head == "import" || head == "define" || head == "if" {
			t.Errorf("the profile uses %s", f)
		}
		if head == "allow" && f.list[1].atom == "default" {
			t.Errorf("the profile allows default: %s", f)
		}
		if head == "allow" && len(f.list) == 2 && !slices.Contains([]string{"process-fork", "sysctl-read", "file-read-metadata"}, f.list[1].atom) {
			t.Errorf("%s is allowed without a filter", f.list[1].atom)
		}
	}
	filters := func(verb, op string) []string {
		var out []string
		for _, f := range forms {
			if f.list[0].atom == verb && f.list[1].atom == op {
				for _, x := range f.list[2:] {
					out = append(out, x.String())
				}
			}
		}
		return out
	}
	lit := func(p string) string { return "(literal " + strconv.Quote(p) + ")" }
	sub := func(p string) string { return "(subpath " + strconv.Quote(p) + ")" }

	wantExec := []string{sub("/bin"), sub("/usr/bin"), lit("/usr/libexec/path_helper"), lit(spec.Init)}
	for _, r := range spec.Runtime {
		wantExec = append(wantExec, sub(r))
	}
	if got := filters("allow", "process-exec"); !slices.Equal(got, wantExec) {
		t.Errorf("process-exec allows %v, want %v", got, wantExec)
	}
	execAllow, execDeny := lastRuleIndex(forms, "allow", "process-exec"), lastRuleIndex(forms, "deny", "process-exec")
	if execDeny < execAllow {
		t.Errorf("the process-exec refusals come before the allow (%d < %d): the allow would win", execDeny, execAllow)
	}
	for _, prog := range []string{"/usr/bin/security", "/usr/bin/osascript", "/usr/bin/open"} {
		if !slices.Contains(filters("deny", "process-exec"), lit(prog)) {
			t.Errorf("executing %s is not refused", prog)
		}
	}
	if got := filters("allow", "process-info*"); !slices.Equal(got, []string{"(target same-sandbox)"}) {
		t.Errorf("process-info* %v", got)
	}
	// process-info* is allowed by default: a bare refusal must come before the one allow, or the allow
	// restricts nothing.
	infoDeny, infoAllow := -1, lastRuleIndex(forms, "allow", "process-info*")
	for i, f := range forms {
		if f.String() == "(deny process-info*)" && infoDeny < 0 {
			infoDeny = i
		}
	}
	if infoDeny < 0 || infoDeny > infoAllow || lastRuleIndex(forms, "deny", "process-info*") != infoDeny {
		t.Errorf("process information outside the sandbox: (deny process-info*) at %d, the allow at %d", infoDeny, infoAllow)
	}
	lastAllow := -1
	for i, f := range forms {
		if f.list[0].atom == "allow" {
			lastAllow = i
		}
	}
	for _, want := range []string{"(deny file-link)", "(deny file-clone)", "(deny nvram*)", "(deny system-fcntl (fcntl-command 80 110))"} {
		at := slices.IndexFunc(forms, func(f sbplForm) bool { return f.String() == want })
		if at < 0 || at < lastAllow {
			t.Errorf("%s: at %d, the last allow at %d", want, at, lastAllow)
		}
	}
	for _, op := range []string{"file-link", "file-clone", "nvram*", "system-fcntl"} {
		if at := lastRuleIndex(forms, "allow", op); at >= 0 {
			t.Errorf("the profile allows %s", forms[at])
		}
	}
	if got := filters("allow", "signal"); !slices.Equal(got, []string{"(target same-sandbox)"}) {
		t.Errorf("signal %v", got)
	}
	if got := filters("allow", "network-outbound"); !slices.Equal(got, []string{`(remote ip "localhost:49152")`}) {
		t.Errorf("network-outbound %v", got)
	}
	for _, f := range forms {
		op := f.list[1].atom
		if f.list[0].atom == "allow" && (strings.HasPrefix(op, "network") && op != "network-outbound" || op == "appleevent-send" ||
			op == "process-exec-interpreter" || op == "mach-register" || op == "iokit-open" || op == "file-write*" && f.String() != fileWriteRule(spec)) {
			t.Errorf("the profile allows %s", f)
		}
	}
	for _, op := range []string{"network-bind", "network-inbound", "appleevent-send"} {
		if lastRuleIndex(forms, "deny", op) < 0 {
			t.Errorf("%s is not refused at the end", op)
		}
	}
	if got := filters("deny", "sysctl-read"); !slices.Equal(got, []string{`(sysctl-name-prefix "kern.procargs")`}) ||
		lastRuleIndex(forms, "deny", "sysctl-read") < lastRuleIndex(forms, "allow", "sysctl-read") {
		t.Errorf("other processes' arguments: %v", got)
	}
	wantWrites := []string{sub(spec.WorkDir), sub(spec.RunDir + "/home"), sub(spec.RunDir + "/tmp"),
		lit("/dev/null"), lit("/dev/stdout"), lit("/dev/stderr"), sub("/dev/fd")}
	if got := filters("allow", "file-write*"); !slices.Equal(got, wantWrites) {
		t.Errorf("file-write* %v, want %v", got, wantWrites)
	}
	reads := filters("allow", "file-read*")
	for _, want := range []string{sub(spec.RunDir), sub(spec.WorkDir), lit(spec.Init), sub("/opt/homebrew/Cellar"),
		sub("/private/etc/ssl"), lit("/dev/urandom")} {
		if !slices.Contains(reads, want) {
			t.Errorf("file-read* lacks %s", want)
		}
	}
	for _, r := range reads {
		for _, home := range []string{`"/Users/u"`, `"/Users/u/Library"`, `"/Users/u/Library/Caches"`, `"/private/etc"`, `"/"`,
			`"/private/var"`, `"/System"`, `"/System/Volumes/Data"`, `"/usr/local"`, `"/opt/homebrew"`, `"/Library"`} {
			if strings.HasSuffix(r, " "+home+")") {
				t.Errorf("file-read* opens %s", r)
			}
		}
	}

	// Without an endpoint there is no network rule at all.
	none, err := buildSeatbeltProfile(goldenSeatbeltSpec(0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, "(allow network") {
		t.Errorf("a profile without an endpoint allows network:\n%s", none)
	}
}

// fileWriteRule is the one file-write* rule the profile of spec has (the shape test compares it whole).
func fileWriteRule(spec seatbeltSpec) string {
	return fmt.Sprintf(`(allow file-write* (subpath %q) (subpath %q) (subpath %q) (literal "/dev/null") (literal "/dev/stdout") (literal "/dev/stderr") (subpath "/dev/fd"))`,
		spec.WorkDir, spec.RunDir+"/home", spec.RunDir+"/tmp")
}

// MS-2: a profile is built only from absolute, clean paths with no control character, none of which
// overlaps the never-bind list, a work dir apart from the run directory and a port in range. A path the
// profile cannot carry otherwise is escaped.
func TestSeatbeltProfileRefusesUnsafeInput(t *testing.T) {
	for name, adjust := range map[string]func(*seatbeltSpec){
		"relative init":      func(s *seatbeltSpec) { s.Init = "anet" },
		"unclean run dir":    func(s *seatbeltSpec) { s.RunDir = s.RunDir + "/../x" },
		"root work dir":      func(s *seatbeltSpec) { s.WorkDir = "/" },
		"newline":            func(s *seatbeltSpec) { s.WorkDir = s.WorkDir + "\n(allow default)" },
		"runtime in .claude": func(s *seatbeltSpec) { s.Runtime = append(s.Runtime, "/Users/u/.claude/local") },
		"runtime is home":    func(s *seatbeltSpec) { s.Runtime = append(s.Runtime, "/Users/u") },
		"read-only data dir": func(s *seatbeltSpec) { s.ReadOnly = append(s.ReadOnly, "/Users/u/.anet/x") },
		"direct work root":   func(s *seatbeltSpec) { s.WorkDir = "/Users/u/Library/Caches/anet/work/E/ix" },
		"work in run":        func(s *seatbeltSpec) { s.WorkDir = s.RunDir + "/home/w" },
		"port":               func(s *seatbeltSpec) { s.Port = 70000 },
	} {
		spec := goldenSeatbeltSpec(49152)
		adjust(&spec)
		if p, err := buildSeatbeltProfile(spec); err == nil {
			t.Errorf("%s: built\n%s", name, p)
		}
	}
	spec := goldenSeatbeltSpec(49152)
	spec.WorkDir = `/Users/u/Library/Caches/anet/work-sandbox/E/ix "quoted" \ back`
	p, err := buildSeatbeltProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	forms := mustParseSBPL(t, p)
	found := false
	for _, f := range forms {
		for _, x := range f.list {
			if x.String() == "(subpath "+strconv.Quote(spec.WorkDir)+")" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the quoted work dir does not read back:\n%s", p)
	}
}

// EG-3: the profile carries paths and a port, never the run's key, whatever the paths are.
func TestSeatbeltProfileCarriesNoKey(t *testing.T) {
	spec := goldenSeatbeltSpec(49152)
	p, err := buildSeatbeltProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, testRunKey) || strings.Contains(p, "anet:") || strings.Contains(p, egressRoutePrefix) {
		t.Error("the profile names a key or the route")
	}
}

// The Homebrew tree of a runtime installed by it, and nothing for any other runtime.
func TestSeatbeltPackageTrees(t *testing.T) {
	got := seatbeltPackageTrees([]string{"/opt/homebrew/Cellar/node/22.1.0/bin", "/usr/local/Cellar/python@3.12/3.12.4/bin",
		"/opt/homebrew/Cellar/node/22.1.0/lib", "/Users/u/.local/bin", "/opt/homebrew/Cellar/x", "/Cellar/a/b"})
	want := []string{"/opt/homebrew/Cellar", "/usr/local/Cellar", "/Cellar"}
	if !slices.Equal(got, want) {
		t.Errorf("trees %v, want %v", got, want)
	}
}

// --- a reader for the profile's s-expressions, independent of the writer ---

// sbplForm is an atom or a list.
type sbplForm struct {
	atom string // a symbol, a number, or a string literal in Go quoting
	list []sbplForm
	str  bool
}

func (f sbplForm) String() string {
	if f.list == nil {
		if f.str {
			return strconv.Quote(f.atom)
		}
		return f.atom
	}
	parts := make([]string, len(f.list))
	for i, x := range f.list {
		parts[i] = x.String()
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// sbplOps are the operations a profile of ours may name: the ones the writer uses, which a typo would
// otherwise turn into a profile sandbox-exec refuses to load.
var sbplOps = []string{"default", "process-fork", "process-exec", "process-info*", "signal", "sysctl-read",
	"file-read-metadata", "file-read*", "file-write*", "file-ioctl", "system-socket", "mach-lookup",
	"network-outbound", "network-bind", "network-inbound", "appleevent-send", "file-link", "file-clone", "nvram*",
	"system-fcntl"}

// sbplFilters are the filters a profile of ours may use, with their argument kinds ("s" a string, "y" a
// symbol, "N" one or more decimal numbers).
var sbplFilters = map[string]string{"literal": "s", "subpath": "s", "global-name": "s", "global-name-prefix": "s",
	"sysctl-name-prefix": "s", "remote": "ys", "target": "y", "socket-domain": "y", "fcntl-command": "N"}

// parseSBPL reads text into top-level forms and checks each is (version 1) or (allow|deny op filter…)
// with a known operation and well-formed filters; comments start with ";".
func parseSBPL(text string) ([]sbplForm, error) {
	var forms []sbplForm
	i := 0
	var read func() (sbplForm, error)
	skip := func() {
		for i < len(text) {
			switch {
			case text[i] == ';':
				for i < len(text) && text[i] != '\n' {
					i++
				}
			case text[i] == ' ' || text[i] == '\n' || text[i] == '\t':
				i++
			default:
				return
			}
		}
	}
	read = func() (sbplForm, error) {
		skip()
		if i >= len(text) {
			return sbplForm{}, errors.New("unexpected end")
		}
		switch text[i] {
		case '(':
			i++
			f := sbplForm{list: []sbplForm{}}
			for {
				skip()
				if i >= len(text) {
					return f, errors.New("unclosed list")
				}
				if text[i] == ')' {
					i++
					return f, nil
				}
				x, err := read()
				if err != nil {
					return f, err
				}
				f.list = append(f.list, x)
			}
		case ')':
			return sbplForm{}, fmt.Errorf("stray ) at %d", i)
		case '"':
			i++
			var b strings.Builder
			for {
				if i >= len(text) {
					return sbplForm{}, errors.New("unclosed string")
				}
				c := text[i]
				i++
				switch {
				case c == '"':
					return sbplForm{atom: b.String(), str: true}, nil
				case c == '\\' && i < len(text):
					b.WriteByte(text[i])
					i++
				case c < 0x20:
					return sbplForm{}, errors.New("control character in a string")
				default:
					b.WriteByte(c)
				}
			}
		default:
			j := i
			for i < len(text) && !strings.ContainsRune(" \n\t()\";", rune(text[i])) {
				i++
			}
			return sbplForm{atom: text[j:i]}, nil
		}
	}
	for {
		skip()
		if i >= len(text) {
			break
		}
		f, err := read()
		if err != nil {
			return nil, err
		}
		if f.list == nil || len(f.list) < 2 {
			return nil, fmt.Errorf("top-level form %s", f)
		}
		switch head := f.list[0].atom; head {
		case "version":
			if f.String() != "(version 1)" {
				return nil, fmt.Errorf("%s", f)
			}
		case "allow", "deny":
			if !slices.Contains(sbplOps, f.list[1].atom) {
				return nil, fmt.Errorf("unknown operation in %s", f)
			}
			for _, x := range f.list[2:] {
				kinds, ok := sbplFilters[x.list0()]
				if ok && kinds == "N" && len(x.list) >= 2 {
					for _, arg := range x.list[1:] {
						if _, err := strconv.ParseUint(arg.atom, 10, 32); arg.list != nil || arg.str || err != nil {
							return nil, fmt.Errorf("filter %s: %s is not a number", x, arg)
						}
					}
					continue
				}
				if x.list == nil || !ok || len(x.list) != len(kinds)+1 {
					return nil, fmt.Errorf("unknown filter %s", x)
				}
				for k, kind := range kinds {
					arg := x.list[k+1]
					if arg.list != nil || arg.str != (kind == 's') {
						return nil, fmt.Errorf("filter %s: argument %d", x, k+1)
					}
				}
			}
		default:
			return nil, fmt.Errorf("unknown form %s", f)
		}
		forms = append(forms, f)
	}
	return forms, nil
}

func (f sbplForm) list0() string {
	if len(f.list) == 0 {
		return ""
	}
	return f.list[0].atom
}

func mustParseSBPL(t *testing.T, text string) []sbplForm {
	t.Helper()
	forms, err := parseSBPL(text)
	if err != nil {
		t.Fatalf("the profile does not read: %v\n%s", err, text)
	}
	if len(forms) < 2 || forms[0].String() != "(version 1)" || forms[1].String() != "(deny default)" {
		t.Fatalf("the profile does not open with (version 1) (deny default)")
	}
	return forms
}

// lastRuleIndex is the index of the last (verb op …) form, -1 for none.
func lastRuleIndex(forms []sbplForm, verb, op string) int {
	at := -1
	for i, f := range forms {
		if f.list[0].atom == verb && len(f.list) > 1 && f.list[1].atom == op {
			at = i
		}
	}
	return at
}

// FuzzSeatbeltProfile: whatever the paths and port, the builder either refuses them or writes a profile
// that reads back, opens with (deny default), allows only known operations, and names each path as given.
func FuzzSeatbeltProfile(f *testing.F) {
	g := goldenSeatbeltSpec(49152)
	f.Add(g.Init, g.RunDir, g.WorkDir, g.Runtime[0], 49152)
	f.Add("/a", "/b", "/c", "/d\"e\\f", 0)
	f.Add("/a b", "/r", "/w", "/opt/homebrew/Cellar/n/1/bin", 1)
	f.Add("/x\x00", "/r", "/w", "", -1)
	f.Fuzz(func(t *testing.T, init, run, work, rt string, port int) {
		spec := seatbeltSpec{Init: init, RunDir: run, WorkDir: work, Runtime: []string{rt}, Port: port,
			ReadOnly: seatbeltPackageTrees([]string{rt}), NeverAllow: []string{"/never"}}
		p, err := buildSeatbeltProfile(spec)
		if err != nil {
			return
		}
		forms, err := parseSBPL(p)
		if err != nil {
			t.Fatalf("%v\n%s", err, p)
		}
		if forms[1].String() != "(deny default)" || strings.Contains(p, "(allow default)") {
			t.Fatalf("not deny-default:\n%s", p)
		}
		for _, path := range []string{init, run, work} {
			if !strings.Contains(p, sbplString(path)) {
				t.Fatalf("%q is not named as given:\n%s", path, p)
			}
			if pathWithin(path, "/never") || pathWithin("/never", path) {
				t.Fatalf("a never-allowed path was opened: %q", path)
			}
		}
		if port <= 0 && strings.Contains(p, "network-outbound") {
			t.Fatalf("network without a port:\n%s", p)
		}
	})
}

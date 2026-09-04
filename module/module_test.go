package module

import (
	"context"
	"strings"
	"testing"
)

type fakeModule struct {
	name    string
	started bool
	stopped bool
	fail    error
}

func (f *fakeModule) Name() string { return f.name }
func (f *fakeModule) Start(context.Context, Host) error {
	f.started = true
	return f.fail
}
func (f *fakeModule) Stop(context.Context) error { f.stopped = true; return nil }

func reset(t *testing.T) {
	t.Helper()
	regMu.Lock()
	saved := append([]registration(nil), registry...)
	savedOptIn := map[string]bool{}
	for k, v := range optInNames {
		savedOptIn[k] = v
	}
	savedExtra := append([]string(nil), extraCompiled...)
	registry = nil
	optInNames = map[string]bool{}
	extraCompiled = nil
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		registry = saved
		optInNames = savedOptIn
		extraCompiled = savedExtra
		regMu.Unlock()
	})
}

// A module compiled in but not configured stays out of the way: that is the
// ordinary case for every module an operator has not asked for.
func TestCompiledButUnconfiguredIsNotBuilt(t *testing.T) {
	reset(t)
	Register("p2p", func(raw []byte) (Module, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		return &fakeModule{name: "p2p"}, nil
	})
	mods, err := Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 0 {
		t.Fatalf("nothing was configured, so nothing should be built: %+v", mods)
	}
}

// Configuring a module that was compiled out must fail loudly.
//
// The alternative is a daemon that reads "p2p": {...}, ignores it, comes up
// looking healthy, and leaves an operator wondering why nothing is peering.
// Silent capability loss is the failure mode this whole seam exists to make
// impossible.
func TestConfiguredButCompiledOutIsAnError(t *testing.T) {
	reset(t)
	Register("anetlink", func([]byte) (Module, error) { return nil, nil })

	_, err := Build(map[string][]byte{"p2p": []byte(`{"listen":":4001"}`)})
	if err == nil {
		t.Fatal("configuring a module that is not in the build must be refused")
	}
	if !strings.Contains(err.Error(), "no_p2p") {
		t.Errorf("the error should name the tag that would explain it: %v", err)
	}
}

// An additive tag fails the same way but must name a different flag.
//
// `no_shell` does not exist. Sending an operator to look for it costs
// them the time it takes to discover that, and then leaves them no closer
// to the answer, which is that this build was never asked to include the
// module.
func TestConfiguringAnOptInModuleNamesTheTagThatWouldAddIt(t *testing.T) {
	reset(t)
	DeclareOptIn("shell")

	_, err := Build(map[string][]byte{"shell": []byte(`{"commands":{}}`)})
	if err == nil {
		t.Fatal("configuring a module that is not in the build must be refused")
	}
	if !strings.Contains(err.Error(), "-tags shell") {
		t.Errorf("the error must name the tag that would add it: %v", err)
	}
	if strings.Contains(err.Error(), "no_shell") {
		t.Errorf("it must not point at a subtractive tag that does not exist: %v", err)
	}
}

// A compiled-in subsystem that is not a Module gets its own explanation.
//
// MCP is in the binary but has no config block: it is a subcommand. Reusing
// the "not compiled into this build (built with no_mcp?)" message told the
// operator two false things at once — that it is absent, and that a tag is
// the reason — and sent them to remove a tag that would not have helped.
func TestConfiguringACompiledInNonModuleExplainsItself(t *testing.T) {
	reset(t)
	DeclareCompiled("mcp")

	_, err := Build(map[string][]byte{"mcp": []byte(`{}`)})
	if err == nil {
		t.Fatal("a name that takes no config block must still be refused")
	}
	msg := err.Error()
	if strings.Contains(msg, "not compiled into this build") {
		t.Errorf("the message says it is absent, and it is not: %v", err)
	}
	if strings.Contains(msg, "no_mcp") {
		t.Errorf("the message points at a tag that is not the reason: %v", err)
	}
	if !strings.Contains(msg, "subcommand") {
		t.Errorf("the message should say how it IS reached: %v", err)
	}
}

// Compiled() names it, so `anet version` can report it.
func TestDeclareCompiledShowsUpInTheCompiledList(t *testing.T) {
	reset(t)
	Register("service", func([]byte) (Module, error) { return nil, nil })
	DeclareCompiled("mcp")
	got := strings.Join(Compiled(), ",")
	if !strings.Contains(got, "mcp") || !strings.Contains(got, "service") {
		t.Fatalf("Compiled() must list both kinds: %q", got)
	}
}

// The declaration has to survive in a build that contains none of the
// module's code, which is the only build where the message is needed.
func TestOptInNamesAreKnownWithoutTheModuleBeingRegistered(t *testing.T) {
	reset(t)
	DeclareOptIn("shell")
	if !optInName("shell") {
		t.Fatal("an opt-in name must be known while the module is absent")
	}
	for _, n := range Compiled() {
		if n == "shell" {
			t.Fatal("declaring the name must not register the module")
		}
	}
}

// The build must say what it contains, so an operator can check the binary
// rather than the documentation.
func TestCompiledListsWhatIsInTheBuild(t *testing.T) {
	reset(t)
	Register("store", func([]byte) (Module, error) { return nil, nil })
	Register("anetlink", func([]byte) (Module, error) { return nil, nil })

	got := strings.Join(Compiled(), ",")
	if got != "anetlink,store" {
		t.Fatalf("Compiled() = %q, want a sorted list of the build's modules", got)
	}
}

// Two modules claiming one name is a build-time mistake, and it must not be
// resolvable at runtime by whichever init() ran last.
func TestDuplicateRegistrationPanics(t *testing.T) {
	reset(t)
	Register("org", func([]byte) (Module, error) { return nil, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate module name must not be silently accepted")
		}
	}()
	Register("org", func([]byte) (Module, error) { return nil, nil })
}

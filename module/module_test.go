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
	registry = nil
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		registry = saved
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

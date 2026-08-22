//go:build !no_anetlink

package anetlink_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/anetlink"
)

// The factory is the only thing between an operator's typo and a daemon
// that comes up without the capabilities they asked for.
func TestTheFactoryRefusesConfigurationsThatCannotWork(t *testing.T) {
	build := func(raw string) (module.Module, error) {
		return module.BuildOne("anetlink", []byte(raw))
	}
	if m, err := build(""); err != nil || m != nil {
		t.Errorf("compiled in and not configured must be a quiet no-op, got (%v, %v)", m, err)
	}
	if _, err := build(`{`); err == nil {
		t.Error("unparseable config was accepted")
	}
	if _, err := build(`{"timeout_s": 5}`); err == nil {
		t.Error("a config with no socket was accepted — there is nothing to talk to")
	} else if !strings.Contains(err.Error(), "socket") {
		t.Errorf("the operator is not told what is missing: %v", err)
	}
	m, err := build(`{"socket":"/tmp/linkd.sock"}`)
	if err != nil || m == nil {
		t.Fatalf("a valid config was refused: %v", err)
	}
	if m.Name() != "anetlink" {
		t.Errorf("name = %q — it must match the build tag, or no_<name> switches off nothing",
			m.Name())
	}
}

// C1's red line, enforced by the compiler rather than by review.
//
// This module exists to offer whatever hardware ANetLink reaches, and the
// thing it must never do is hand the daemon a notion of a device. If it
// ever grew a method returning devices, or a Host that asked for one, the
// daemon would start knowing about them — and knowing is how the org
// feature reached thirty-five daemon files. A reflection test is blunt,
// and blunt is right: the failure it guards against is somebody adding a
// helpful-looking method.
func TestTheModuleNeverHandsTheDaemonADevice(t *testing.T) {
	typ := reflect.TypeOf(&anetlink.Module{})
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if strings.Contains(strings.ToLower(m.Name), "device") {
			t.Errorf("method %s: the daemon must never learn what a device is", m.Name)
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			if strings.Contains(strings.ToLower(m.Type.Out(j).String()), "device") {
				t.Errorf("method %s returns %s — C1 says capabilities, not devices",
					m.Name, m.Type.Out(j))
			}
		}
	}
	// And what it does implement is exactly the module contract, nothing
	// wider. A module that satisfied Payer, say, would be a device layer
	// that could also spend money.
	var _ module.Module = (*anetlink.Module)(nil)
	if _, ok := any((*anetlink.Module)(nil)).(module.Payer); ok {
		t.Error("the device module implements Payer — it has no business with money")
	}
	if _, ok := any((*anetlink.Module)(nil)).(module.Confidential); ok {
		t.Error("the device module declares confidential tokens it does not have")
	}
}

// Stopping a module that never started must not panic. It is the ordinary
// path when the daemon fails to come up.
func TestStoppingAModuleThatNeverStartedIsSafe(t *testing.T) {
	m, err := module.BuildOne("anetlink", []byte(`{"socket":"/tmp/nope.sock"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Errorf("Stop before Start: %v", err)
	}
}

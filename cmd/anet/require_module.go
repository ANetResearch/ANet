package main

// module_absent.go is the CLI's side of a module that is not available
// (MOD-F3, MOD-F2): the command that belongs to a module decides whether the
// module is available — this binary, and the `without` in config.json —
// before it contacts the daemon or asks anything on the terminal, and says
// module_absent with exit status 2 when it is not. The daemon is the
// authority (it answers 409 module_absent to the same entrances); this check
// only spares the round trip.

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

// absentError is a command's refusal for a module that is not available:
// the sentence as it is printed (no "error:" prefix), and exit status 2.
type absentError struct{ text string }

func (e absentError) Error() string { return e.text }

// availabilityOf is the Availability value of the configuration in layout,
// read without creating it. A configuration that cannot be read, or whose
// `without` does not validate, gives the value with nothing turned off: the
// daemon refuses to start on it and says why.
func availabilityOf(layout daemon.Layout) module.Availability {
	without, err := daemon.WithoutInFile(layout)
	if err != nil {
		return module.Availability{}
	}
	av, err := module.NewAvailability(without)
	if err != nil {
		return module.Availability{}
	}
	return av
}

// requireModule is nil when module m is available, otherwise the
// module_absent refusal.
func requireModule(layout daemon.Layout, m string) error {
	ref := availabilityOf(layout).Absent(m, "")
	if ref == nil {
		return nil
	}
	return absentError{a2ashape.Sentence(ref.Key, a2ashape.Lang(), ref.Args)}
}

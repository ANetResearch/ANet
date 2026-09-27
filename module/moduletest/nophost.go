// Package moduletest holds what a module's tests share.
//
// NopHost is the reason it exists. Every module test needs a module.Host,
// and every one of them used to spell out the whole interface — eight
// copies, each of which had to change the day Host grew a method, and
// did, one merge conflict at a time. A test host now embeds NopHost and
// writes only the methods its module actually uses; a new Host method is
// added here, once.
package moduletest

import (
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// NopHost is a module.Host that grants nothing: no identity, no
// registry, no evidence, no seams, no state directory. Admit lets every
// call through, because a test of a module is not a test of the kernel's
// admission policy.
//
// Embed it and override what the module under test needs:
//
//	type host struct {
//		moduletest.NopHost
//		reg *provider.Registry
//	}
//
//	func (h *host) Providers() *provider.Registry { return h.reg }
//
// Providers returns nil, so a module that registers capabilities needs
// its test host to override it.
type NopHost struct{}

var _ module.Host = NopHost{}

func (NopHost) AID() string                                      { return "" }
func (NopHost) Providers() *provider.Registry                    { return nil }
func (NopHost) RecordEvidence(string, any) error                 { return nil }
func (NopHost) ResolveKEL(string) ([]identity.SignedEvent, bool) { return nil, false }
func (NopHost) PaymentSeam() (module.PaymentSeam, bool)          { return nil, false }
func (NopHost) HubSeam() (module.HubSeam, bool)                  { return nil, false }
func (NopHost) Admit(string, string, int) (func(), string)       { return func() {}, "" }
func (NopHost) DeclareUntrustedBackend()                         {}
func (NopHost) StateDir(string) string                           { return "" }
func (NopHost) TaskSeam() (module.TaskSeam, bool)                { return nil, false }

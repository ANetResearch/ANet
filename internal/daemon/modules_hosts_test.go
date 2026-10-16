package daemon

import "github.com/ANetResearch/ANet/module"

// a2aHost is the Host of the module called a2a, as startModules builds it.
func a2aHost(d *Daemon) hostA2A {
	h, _ := newModuleHost(d, "a2a")
	return h.(hostA2A)
}

// p2pHost is the Host of the module called p2p.
func p2pHost(d *Daemon) hostTransport {
	h, _ := newModuleHost(d, "p2p")
	return h.(hostTransport)
}

func init() {
	// The configuration checks of the a2a and anetlink blocks do not depend
	// on the modules' code (they run before the modules start, in every
	// build), and the daemon's tests do not link that code: they say the
	// modules are compiled in so that a valid block is not refused as the
	// block of a module that is not there.
	module.DeclareCompiled("a2a", "anetlink")
	// The fake modules this package's tests register under names of their
	// own are given the grants they use.
	f := func(name string) (module.Info, bool) {
		switch name {
		case "rt_startup_probe":
			return module.Info{Name: name, Grants: []module.Grant{module.GrantProviders, module.GrantTransportHost}}, true
		case "lifecycle_rec":
			return module.Info{Name: name, Grants: []module.Grant{module.GrantRecordEvidence},
				EvidenceTypes: []string{"test.module.stop"}}, true
		case "testdeclaresuntrusted":
			return module.Info{Name: name, Grants: []module.Grant{module.GrantDeclareUntrust}}, true
		case "lifecycle_fail":
			return module.Info{Name: name}, true
		}
		return module.Info{}, false
	}
	moduleRowForTest.Store(&f)
}

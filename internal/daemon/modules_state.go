package daemon

import (
	"log"
	"path/filepath"

	"github.com/ANetResearch/ANet/module"
)

// moduleStateDir is where a module's own files live, under the data
// directory: modules/<name>/.
const moduleStateDir = "modules"

// StateDir is <data dir>/modules/<name>/, created 0700 (A2A-DESIGN §11.1).
// A name that is not a plain directory name — empty, a path, "." or ".." —
// gets "": a module name is a build-time constant, and one that would
// escape the directory is a bug to surface, not a path to create.
//
// Both levels go through ensurePrivateDir, as the runtime directories do:
// what a module keeps here (the local A2A interface's token among it) is
// a credential, so an existing directory with a wider mode is narrowed,
// and a symbolic link or a directory of another uid is refused rather than
// written through. A directory that cannot be made private is logged and
// gets "" — the path alone would let the module write its token wherever
// the link points.
func (h moduleHost) StateDir(name string) string {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || filepath.IsAbs(name) {
		return ""
	}
	parent := filepath.Join(h.d.layout.Root, moduleStateDir)
	dir := filepath.Join(parent, name)
	for _, p := range []string{parent, dir} {
		if err := ensurePrivateDir(p); err != nil {
			log.Printf("anet: module %s: state directory: %v", name, err)
			return ""
		}
	}
	return dir
}

// TaskSeam is not offered yet.
//
// TODO(A2A-DESIGN §11.1, §12): implement over the interaction store and the
// delegation paths, sharing the control plane's /tasks/* implementation —
// scoped to role=outbound and peer_aid == peerAID, returning
// a2ashape.ProjectStored projections, Watch built on d.Watch (eventbus.go)
// with a2ashape.StatusUpdate / ArtifactUpdates, Pay through the agent-tier
// spending policy. Until then module/a2a sees (nil, false) and does not
// start its listener.
func (h moduleHost) TaskSeam() (module.TaskSeam, bool) { return nil, false }

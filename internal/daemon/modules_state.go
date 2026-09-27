package daemon

import (
	"log"
	"os"
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
// Creation failing is logged and the path returned anyway: the module's own
// first write then fails with an error that names the file, which is more
// use to an operator than an empty string.
func (h moduleHost) StateDir(name string) string {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || filepath.IsAbs(name) {
		return ""
	}
	dir := filepath.Join(h.d.layout.Root, moduleStateDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("anet: module %s: state directory: %v", name, err)
		return dir
	}
	// MkdirAll leaves an existing directory's mode alone. What a module
	// keeps here (the local A2A interface's token among it) is not for
	// other users either way.
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("anet: module %s: state directory: %v", name, err)
	}
	return dir
}

// TaskSeam is the daemon's task seam (taskseam.go), scoped per call to one
// remote agent. It is offered whether or not a hub is configured: a node
// may register with one after start, and until then a send answers
// UnavailableError rather than the interface not existing.
func (h moduleHost) TaskSeam() (module.TaskSeam, bool) { return h.d.TaskSeam(), true }

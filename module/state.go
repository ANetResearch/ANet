package module

import "path/filepath"

// Where a module's own files live (A2A-DESIGN §11.1): Host.StateDir(name)
// is <data dir>/modules/<name>/. Named here so that the kernel, which makes
// the directory, and the readers outside the daemon (`anet doctor`,
// `anet agents wire`), which open files in it without a running daemon,
// agree on one path.
const StateDirName = "modules"

// StatePath is the state directory Host.StateDir gives module name, for a
// reader that has only the data directory.
func StatePath(dataDir, name string) string {
	return filepath.Join(dataDir, StateDirName, name)
}

// The local A2A interface's files in module a2a's state directory
// (A2A-DESIGN §11.1, X5): written by module/a2a when it first serves, read
// by `anet agents wire` (Hermes' a2a_agents entries) and `anet doctor`.
const (
	A2AModuleName = "a2a"
	A2AAddrFile   = "a2a_addr.txt"
	A2ATokenFile  = "a2a_token.txt"
)

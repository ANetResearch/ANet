// Package anethome is the on-disk layout of a user's identities: the
// container ("anet home"), where each named identity's data directory is,
// the walk over all of them, and where a module keeps its own state inside
// a data directory.
//
// It exists so that the code which has to see every identity does not have
// to import the daemon to do it. The daemon's control-port allocator
// (internal/daemon identities.go) and the local A2A interface's port
// allocator (module/a2a addr.go) both skip ports that another identity has
// recorded (A2A-DESIGN §11.1); the second may not import internal/daemon,
// and before this package it walked the directories with a copy of the
// daemon's rules that had already drifted (it did not check identity
// names). The CLI (anet doctor, anet agents wire) reads the local A2A
// interface's files through the same paths the module writes them to.
//
// Standard library only; never internal/daemon or the A2A SDK (SI-8).
//
// The layout:
//
//	<home>                      the "default" identity's data directory
//	<home>/ids/<name>           every other named identity
//	<home>/current              the name `anet id use` selected
//	<data dir>/modules/<module> a module's private state (Host.StateDir)
//	<data dir>/modules/a2a/a2a_addr.txt, a2a_token.txt
//
// A data directory chosen with ANET_DATA_DIR sits outside this layout on
// purpose (tests, scripts); it is found by nothing here.
package anethome

import (
	"os"
	"path/filepath"
	"regexp"
)

const (
	// DefaultName is the identity whose data directory is the home itself,
	// so a single-identity install from before named identities is it.
	DefaultName = "default"
	// IDsDir is the subdirectory of the home holding the other identities.
	IDsDir = "ids"
	// ModulesDir is the subdirectory of a data directory holding each
	// module's private state (internal/daemon moduleHost.StateDir).
	ModulesDir = "modules"

	// A2AModule is the local A2A interface's module name, and so the name
	// of its state directory.
	A2AModule = "a2a"
	// A2AAddrFile and A2ATokenFile are the local A2A interface's address
	// and bearer token, in A2ADir. module/a2a writes them; anet doctor,
	// anet agents wire and other identities' allocators read them.
	A2AAddrFile  = "a2a_addr.txt"
	A2ATokenFile = "a2a_token.txt"
)

// Home is the identity container: $ANET_HOME, else ~/.anet, else ./.anet.
// It is independent of ANET_DATA_DIR, which names one raw data directory.
func Home() string {
	if d := os.Getenv("ANET_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ".anet"
	}
	return filepath.Join(h, ".anet")
}

// nameRe constrains identity names to a filesystem- and URL-safe token.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName reports whether name is a legal identity name. "default" is
// allowed (it maps to the home itself); "ids" is reserved (it is the
// container subdirectory).
func ValidName(name string) bool {
	if name == "" || name == IDsDir || len(name) > 64 {
		return false
	}
	return nameRe.MatchString(name)
}

// Dir is the data directory of a named identity: the home for "" or
// "default", <home>/ids/<name> for any other name. It does not check the
// name; callers taking a name from an operator check ValidName first.
func Dir(name string) string {
	if name == "" || name == DefaultName {
		return Home()
	}
	return filepath.Join(Home(), IDsDir, name)
}

// NameForDir is the reverse of Dir, for diagnostics: "default" for the
// home, the subdirectory name for <home>/ids/<name>, "" for a directory
// outside the layout.
func NameForDir(dir string) string {
	clean := filepath.Clean(dir)
	if clean == filepath.Clean(Home()) {
		return DefaultName
	}
	if filepath.Dir(clean) == filepath.Clean(filepath.Join(Home(), IDsDir)) {
		return filepath.Base(clean)
	}
	return ""
}

// Identity is one candidate identity directory under the home.
type Identity struct {
	Name string
	Dir  string
}

// Identities lists the data directories of every identity the layout can
// hold: the default (the home itself) first, then each subdirectory of
// <home>/ids whose name is a valid identity name, in directory order
// (sorted by name). It reads directory entries only; whether a directory
// holds an initialized identity is the caller's question (internal/daemon
// Layout.Initialized), and a missing home yields just the default.
func Identities() []Identity {
	home := Home()
	out := []Identity{{Name: DefaultName, Dir: home}}
	ents, err := os.ReadDir(filepath.Join(home, IDsDir))
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() && ValidName(e.Name()) {
			out = append(out, Identity{Name: e.Name(), Dir: filepath.Join(home, IDsDir, e.Name())})
		}
	}
	return out
}

// ModuleDir is where the module named module keeps its private state
// inside the data directory dataDir. It only joins paths: the daemon
// creates the directory private (0700) when the module asks for it.
func ModuleDir(dataDir, module string) string {
	return filepath.Join(dataDir, ModulesDir, module)
}

// A2ADir is the local A2A interface's state directory in dataDir, holding
// A2AAddrFile and A2ATokenFile.
func A2ADir(dataDir string) string { return ModuleDir(dataDir, A2AModule) }

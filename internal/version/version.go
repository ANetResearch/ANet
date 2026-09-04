// Package version is the single source of truth for the anet release
// version, shared by both binaries (anet and anet-hub) so they always
// report the same number.
package version

// V is the current anet release version.
//
// Hand-maintained, and therefore says nothing about which build is
// running: every binary cut between two releases reports the same string.
// That is fine for "which release is this" and useless for "is the thing
// I just deployed the thing that is running", which is the question that
// actually comes up.
const V = "0.1.9"

// Commit and BuiltAt are stamped at build time:
//
//	go build -ldflags "-X <this package>.Commit=$(git rev-parse --short HEAD) \
//	                   -X <this package>.BuiltAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// Unstamped builds report "unknown" rather than a plausible-looking
// default. A wrong commit is worse than an absent one: it would let a
// check comparing versions pass while comparing two fabrications.
var (
	Commit  = "unknown"
	BuiltAt = "unknown"
	// Tags is the build tag string this binary was compiled with, empty
	// for a default build.
	//
	// Stamped for the same reason as Commit, and it answers a question
	// Commit cannot: two binaries at the same commit are not the same
	// binary if one of them was built with `-tags shell` and can execute
	// commands on its host. An operator auditing a fleet needs that
	// difference to be readable from the binary rather than inferred
	// from which file they think they copied.
	Tags = ""
)

// Full is the version as a service reports it.
func Full() map[string]string {
	return map[string]string{
		"version":  V,
		"commit":   Commit,
		"built_at": BuiltAt,
		"tags":     Tags,
	}
}

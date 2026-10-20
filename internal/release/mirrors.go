package release

// NPMScope is the npm organisation that carries a copy of every release.
// Each build is one package, @agentnetwork/<asset prefix>-<os>-<cpu> (<os>
// and <cpu> in npm's names: win32 for windows, x64 for amd64), whose
// directory dl/ holds release.json, release.json.sig and the .gz of that
// build exactly as DLPath serves them (deploy/release/npm builds the
// packages). A registry serves the package as a tarball (a SourceNPM
// source), and content delivery networks for npm serve the files of a
// package by path (a SourceFiles source); either way, what is served is
// checked against the signed manifest.
const NPMScope = "@agentnetwork"

// variants are the builds that build-release.sh publishes. A mirror base is
// needed before the manifest is read, so the asset prefix follows the rule
// build-release.sh writes into release.json: anet for the default build and
// anet-<variant> for the others.
var variants = map[string]bool{"default": true, "shell": true}

// npmOS is npm's name (process.platform) of each operating system a
// package is published for, by Go's name of it.
var npmOS = map[string]string{"linux": "linux", "darwin": "darwin", "windows": "win32"}

// NPMPackage is the npm package that carries the build of variant for goos
// and goarch, or "" when no such package is published.
func NPMPackage(variant, goos, goarch string) string {
	nos := npmOS[goos]
	if !variants[variant] || nos == "" {
		return ""
	}
	prefix := "anet"
	if variant != "default" {
		prefix += "-" + variant
	}
	cpu := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if cpu == "" {
		return ""
	}
	return NPMScope + "/" + prefix + "-" + nos + "-" + cpu
}

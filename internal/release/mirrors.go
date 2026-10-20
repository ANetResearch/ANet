package release

// NPMScope is the npm organisation that carries a copy of every release.
// Each build is one package, @agentnetwork/<asset prefix>-<os>-<cpu>, whose
// directory dl/ holds release.json, release.json.sig and the .gz of that
// build exactly as DLPath serves them (deploy/release/npm builds the
// packages). Content delivery networks for npm serve the files of a
// package by path, so the directory of a package is a download base like
// any other: what it serves is checked against the signed manifest.
const NPMScope = "@agentnetwork"

// npmMirrors are the content delivery networks for npm packages, in the
// order they are tried, as a function of the package name. npmmirror
// serves the files of a package only after the package is admitted to its
// list of packages with file access; until then it answers 403 and the
// next mirror is tried.
var npmMirrors = []func(pkg string) string{
	func(pkg string) string { return "https://registry.npmmirror.com/" + pkg + "/latest/files" },
	func(pkg string) string { return "https://cdn.jsdelivr.net/npm/" + pkg + "@latest" },
	func(pkg string) string { return "https://unpkg.com/" + pkg + "@latest" },
}

// variantPrefixes are the asset prefixes of the variants, as build-release.sh
// names them in release.json; a mirror base is needed before the manifest is
// read.
var variantPrefixes = map[string]string{"default": "anet", "shell": "anet-shell"}

// NPMPackage is the npm package that carries the build of variant for goos
// and goarch, or "" when no such package is published.
func NPMPackage(variant, goos, goarch string) string {
	prefix, ok := variantPrefixes[variant]
	if !ok || (goos != "linux" && goos != "darwin") {
		return ""
	}
	cpu := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if cpu == "" {
		return ""
	}
	return NPMScope + "/" + prefix + "-" + goos + "-" + cpu
}

// MirrorBases are the download bases of the npm package of a build, tried
// after DefaultBases. They are empty for a build that has no package.
func MirrorBases(variant, goos, goarch string) []string {
	pkg := NPMPackage(variant, goos, goarch)
	if pkg == "" {
		return nil
	}
	bases := make([]string, 0, len(npmMirrors))
	for _, m := range npmMirrors {
		bases = append(bases, m(pkg))
	}
	return bases
}

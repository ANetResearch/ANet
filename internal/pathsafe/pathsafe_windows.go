//go:build windows

package pathsafe

import (
	"path/filepath"
	"strings"
)

// osSafeName is SafeName's last step on Windows; see sanitizeWindows.
func osSafeName(name string) string { return sanitizeWindows(name) }

// windowsReservedChars cannot appear in a Windows file name; ':' would also address an alternate
// data stream of the file before it.
const windowsReservedChars = `<>:"|?*`

// sanitizeWindows applies the Windows rules to a name SafeName has already made safe elsewhere:
// reserved characters become "_", a device name (the part before the first dot, without trailing
// spaces) gets a "_" prefix, and trailing dots and spaces, which Windows drops silently, are removed
// once more after the length limit.
func sanitizeWindows(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(windowsReservedChars, r) {
			return '_'
		}
		return r
	}, name)
	if windowsDeviceName(name) {
		name = limitName("_" + name)
	}
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "_" {
		return "attachment"
	}
	return name
}

// explorerActiveExts are the extensions of files that Windows Explorer reads, and may act on, when it
// lists or opens the folder that holds them, before the user opens the file: shortcuts and Internet
// shortcuts (an icon location on another host makes Explorer connect to it), Explorer command files,
// libraries, search connectors, application references, settings shortcuts and desktop themes.
var explorerActiveExts = []string{
	".lnk", ".url", ".website", ".scf", ".library-ms", ".searchconnector-ms", ".appref-ms",
	".settingcontent-ms", ".theme", ".themepack", ".desktopthemepack",
}

// explorerInertSuffix is added to such a name: the file is then a text file to Explorer, and its
// original name stays readable.
const explorerInertSuffix = ".txt"

// ExplorerInertName makes a name SafeName returned inert to Windows Explorer for a file written from
// peer data: a name whose extension is one of explorerActiveExts, or the name desktop.ini, compared
// without regard to case, gets the suffix ".txt". Other names are returned unchanged. Outside Windows
// the name is always returned unchanged.
func ExplorerInertName(name string) string {
	lower := strings.ToLower(name)
	active := lower == "desktop.ini"
	for _, ext := range explorerActiveExts {
		if strings.HasSuffix(lower, ext) {
			active = true
			break
		}
	}
	if !active {
		return name
	}
	keep := MaxSafeNameBytes - len(explorerInertSuffix)
	if len(name) > keep {
		name = TruncateUTF8(name, keep)
	}
	return name + explorerInertSuffix
}

// windowsDeviceName reports whether Windows would open name as a device: its part before the first
// dot, without trailing spaces, is CON, PRN, AUX, NUL, COM0-9, LPT0-9 or COM/LPT with a superscript
// digit, in any case.
func windowsDeviceName(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.TrimRight(stem, " ")
	if windowsReservedName(stem) {
		return true
	}
	switch strings.ToUpper(stem) {
	case "COM0", "LPT0", "COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	}
	return false
}

func pathWithin(p, root string) bool {
	pv, pc := windowsPathParts(p)
	rv, rc := windowsPathParts(root)
	if !strings.EqualFold(pv, rv) || len(pc) < len(rc) {
		return false
	}
	for i := range rc {
		if !strings.EqualFold(pc[i], rc[i]) {
			return false
		}
	}
	return true
}

// windowsPathParts splits a path into its volume and its components after filepath.Clean. The
// \\?\ prefix is removed (\\?\C:\x is C:\x, \\?\UNC\srv\share is \\srv\share), and each component
// loses the trailing dots and spaces that Windows ignores, so names Windows treats as the same
// compare equal.
func windowsPathParts(p string) (string, []string) {
	p = filepath.Clean(p)
	switch {
	case len(p) >= 8 && strings.EqualFold(p[:8], `\\?\UNC\`):
		p = `\\` + p[8:]
	case strings.HasPrefix(p, `\\?\`) && len(p) >= 6 && p[5] == ':':
		p = p[4:]
	}
	vol := filepath.VolumeName(p)
	var parts []string
	for _, c := range strings.Split(p[len(vol):], `\`) {
		if t := strings.TrimRight(c, ". "); t != "" {
			parts = append(parts, t)
		} else if c != "" {
			parts = append(parts, c)
		}
	}
	return vol, parts
}

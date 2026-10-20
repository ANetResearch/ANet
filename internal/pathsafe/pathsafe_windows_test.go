//go:build windows

package pathsafe

import (
	"strings"
	"testing"
)

func TestWindowsSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.txt:payload":      "report.txt_payload",
		"report.txt::$DATA":       "report.txt__$DATA",
		`a<b>c"d|e?f*g.txt`:       "a_b_c_d_e_f_g.txt",
		"con.txt":                 "_con.txt",
		"CON":                     "_CON",
		"nul .txt":                "_nul .txt",
		"COM¹.log":                "_COM¹.log",
		"lpt³":                    "_lpt³",
		"com0.txt":                "_com0.txt",
		"LPT0":                    "_LPT0",
		"COM10.txt":               "COM10.txt",
		"console.txt":             "console.txt",
		"?":                       "attachment",
		"name. ":                  "name",
		`C:\dir\file.txt`:         "file.txt",
		"invoice\u202Efdp.exe":    "invoicefdp.exe",
		".envrc":                  "_envrc",
		"plain-name_1.tar.gz":     "plain-name_1.tar.gz",
		"\u00e9t\u00e9 (1).pdf":   "\u00e9t\u00e9 (1).pdf",
		"x:" + "y" + ":z" + ".md": "x_y_z.md",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		strings.Repeat("a", 300) + ":evil.txt",
		"CON" + strings.Repeat(" ", 200) + "x",
		"COM\u00b9" + strings.Repeat(".", 200),
	} {
		got := SafeName(in)
		if len(got) > MaxSafeNameBytes || strings.ContainsAny(got, windowsReservedChars) ||
			strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") || windowsDeviceName(got) {
			t.Errorf("SafeName(%.20q…) = %q", in, got)
		}
	}
}

func TestWindowsPathWithin(t *testing.T) {
	for _, c := range []struct {
		p, root string
		want    bool
	}{
		{`C:\USERS\X\.ANET\a`, `C:\Users\x\.anet`, true},
		{`C:\Users\x\.anet`, `C:\Users\x\.anet`, true},
		{`c:\users\x\.anet\a\b`, `C:\Users\x\.anet`, true},
		{`D:\Users\x\.anet\a`, `C:\Users\x\.anet`, false},
		{`C:\Users\x\.anetwork`, `C:\Users\x\.anet`, false},
		{`C:\Users\x`, `C:\Users\x\.anet`, false},
		{`C:\Users\x\.anet\..\other`, `C:\Users\x\.anet`, false},
		{`C:/Users/x/.anet/a`, `C:\Users\x\.anet`, true},
		{`C:\Users\x\.anet.\a`, `C:\Users\x\.anet`, true},
		{`C:\Users\x\.anet \a`, `C:\Users\x\.anet`, true},
		{`\\?\C:\Users\x\.anet\a`, `C:\Users\x\.anet`, true},
		{`\\server\share\dir\a`, `\\SERVER\Share\dir`, true},
		{`\\?\UNC\server\share\dir\a`, `\\server\share\dir`, true},
		{`\\server\other\dir\a`, `\\server\share\dir`, false},
		{`C:\anything`, `C:\`, true},
		{`\Users\x\.anet\a`, `C:\Users\x\.anet`, false},
	} {
		if got := PathWithin(c.p, c.root); got != c.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", c.p, c.root, got, c.want)
		}
	}
}

// Names that Windows Explorer acts on when it lists a folder get the suffix .txt, in any case; other
// names, and names that only contain such an extension inside, are unchanged.
func TestWindowsExplorerInertName(t *testing.T) {
	for in, want := range map[string]string{
		"invoice.lnk":                     "invoice.lnk.txt",
		"Report.URL":                      "Report.URL.txt",
		"a.website":                       "a.website.txt",
		"x.scf":                           "x.scf.txt",
		"docs.library-ms":                 "docs.library-ms.txt",
		"q.searchConnector-ms":            "q.searchConnector-ms.txt",
		"app.appref-ms":                   "app.appref-ms.txt",
		"s.SettingContent-ms":             "s.SettingContent-ms.txt",
		"t.theme":                         "t.theme.txt",
		"t.themepack":                     "t.themepack.txt",
		"t.desktopthemepack":              "t.desktopthemepack.txt",
		"desktop.ini":                     "desktop.ini.txt",
		"DESKTOP.INI":                     "DESKTOP.INI.txt",
		"report.md":                       "report.md",
		"setup.ini":                       "setup.ini",
		"x.lnk.pdf":                       "x.lnk.pdf",
		"lnk":                             "lnk",
		strings.Repeat("a", 126) + ".lnk": strings.Repeat("a", 124) + ".txt",
	} {
		got := ExplorerInertName(in)
		if got != want {
			t.Errorf("ExplorerInertName(%q) = %q, want %q", in, got, want)
		}
		if len(got) > MaxSafeNameBytes {
			t.Errorf("ExplorerInertName(%q) is %d bytes", in, len(got))
		}
	}
}

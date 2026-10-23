package autoreply

import (
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// importAllowed is the set of non-standard-library packages the module may import: the module package
// (the seams of the Host), the kernel-neutral internal packages the seams are built from, and the
// system call package. Anything else, above all internal/daemon, is a kernel dependency (MOD §7.1).
var importAllowed = map[string]bool{
	"github.com/ANetResearch/ANet/module":             true,
	"github.com/ANetResearch/ANet/internal/a2ashape":  true,
	"github.com/ANetResearch/ANet/internal/evtypes":   true,
	"github.com/ANetResearch/ANet/internal/localday":  true,
	"github.com/ANetResearch/ANet/internal/execwork":  true,
	"github.com/ANetResearch/ANet/internal/pathsafe":  true,
	"github.com/ANetResearch/ANet/internal/anethome":  true,
	"github.com/ANetResearch/ANet/internal/fsperm":    true,
	"github.com/ANetResearch/ANet/internal/procgroup": true,
	// The runner's socket is checked as a service backend's (runner_client.go), and the runner checks its
	// clients (runner_serve.go); the model proxy tells whose a loopback connection is (model_proxy.go); the
	// turn tools' wire form is shared with `anet turn-mcp`. All four are kernel-neutral: standard library
	// and x/sys only.
	"github.com/ANetResearch/ANet/internal/backendconn": true,
	"github.com/ANetResearch/ANet/internal/localpeer":   true,
	"github.com/ANetResearch/ANet/internal/peercred":    true,
	"github.com/ANetResearch/ANet/internal/turntools":   true,
	"github.com/ANetResearch/ANetCore/anetcid":          true,
	"github.com/ANetResearch/ANetCore/delegation":       true,
	"golang.org/x/sys/unix":                             true,
}

// TestAutoReplyModuleImportBoundary (MOD-3's closed assertion, renamed with the move): the module's files
// import only the allowed packages, on Linux, on macOS and on Windows, so it reaches the kernel only through the
// seams of module.Host and a build without the kernel's test helpers still compiles it.
func TestAutoReplyModuleImportBoundary(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ctxt := build.Default
		ctxt.GOOS = goos
		ctxt.CgoEnabled = false
		bp, err := ctxt.ImportDir(".", 0)
		if err != nil {
			t.Fatal(err)
		}
		var bad []string
		for _, name := range bp.GoFiles {
			f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range f.Imports {
				p := strings.Trim(im.Path.Value, `"`)
				if strings.HasPrefix(p, "github.com/ANetResearch/") || strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
					if !importAllowed[p] {
						bad = append(bad, fmt.Sprintf("%s/%s: imports %s", goos, name, p))
					}
				}
			}
		}
		sort.Strings(bad)
		for _, b := range bad {
			t.Error(b)
		}
	}
}

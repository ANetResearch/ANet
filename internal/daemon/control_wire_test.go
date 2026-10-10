package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/wire"
	"github.com/ANetResearch/ANetCore/wire/scan"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// ANet's half of the wire registry checks (ANetCore wire/registry.json).

func anetRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = up
	}
}

// TestWireRegistry (CORE-S3, ANet): every keyasint struct, "anet…" string, relayauth action and
// identity-key signing call in this repository's code matches the registry, or is a v0.2
// leftover listed in ANetCore wire/transition.json with the work item that removes it.
func TestWireRegistry(t *testing.T) {
	ps, err := scan.Check(scan.Config{Dir: anetRoot(t), Repo: "ANet", Tags: [][]string{{"shell"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		t.Error(p.String())
	}
}

// TestControlWireFields (CORE-S5): the control-plane bodies the registry lists (its control section) have
// exactly the registered JSON members. Only these routes are compared; the rest of the control
// plane is free. A row whose Go type does not exist yet, or still has its v0.2 shape, is skipped
// with the work item that brings it to the registered shape; once it matches, the skip has to go.
func TestControlWireFields(t *testing.T) {
	r, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		route string
		alt   string       // the response alternative, when the registry lists several
		req   reflect.Type // nil: not compared (no request body, or not built yet)
		resp  reflect.Type
		todo  string // the work item's marker (wire.TodoMarker) until the Go type has the registered shape
	}{
		// TaskBody is ErrorBody with the members only message_id_reused carries (task_id, state).
		{route: "error body", resp: reflect.TypeFor[a2ashape.TaskBody]()},
		{route: "/status", todo: "v030-todo(UX-15)"},
		{route: "/tasks/events delivery", todo: "v030-todo(RC-5)"},
		{route: "/identity/status", req: reflect.TypeFor[struct{}](), resp: reflect.TypeFor[IdentityStatus]()},
		{route: "/hub-register", alt: "joined", req: reflect.TypeFor[hubRegisterReq](), resp: reflect.TypeFor[hubRegisterResp]()},
		{route: "/payments/status", resp: reflect.TypeFor[SpendStatus](), todo: "v030-todo(PAY-12)"},
		{route: "/agents/card", todo: "v030-todo(PAY-11)"},
		{route: "/agents/list", todo: "v030-todo(PAY-11)"},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		covered[row.route] = true
		t.Run(row.route, func(t *testing.T) {
			c := r.ControlRoute(row.route)
			if c == nil {
				t.Fatalf("%q is not a registered control route", row.route)
			}
			var diff []string
			if row.req != nil {
				diff = append(diff, scan.JSONFields(row.req, c.Request)...)
			}
			if row.resp != nil {
				body := c.Response
				if row.alt != "" {
					body = scan.Alt(c.Response, row.alt)
				}
				diff = append(diff, scan.JSONFields(row.resp, body)...)
			}
			if row.todo != "" {
				if (row.req != nil || row.resp != nil) && len(diff) == 0 {
					t.Fatalf("%s now has the registered shape: drop its %s skip", row.route, row.todo)
				}
				t.Skip(row.todo + "\n" + strings.Join(diff, "\n"))
			}
			if row.req == nil && row.resp == nil {
				t.Fatal("no Go type bound to this route")
			}
			for _, d := range diff {
				t.Error(d)
			}
		})
	}
	for _, c := range r.Control {
		if !covered[c.Route] {
			t.Errorf("registered control route %q has no row here", c.Route)
		}
	}
}

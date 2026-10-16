//go:build !no_x402

package x402

// M-PAY-37: the node-side voucher door is gone (A2A-DESIGN §21). A hub no
// longer sells access and hands out vouchers, so nothing on a node redeems
// one: the payment module opens no public listener, and a provider is handed
// a caller only through the relay door (provider.Call.Via). A config that
// still names voucher_addr is refused at start (config_key_removed, UX-T6);
// here the module itself is checked not to act on it either way.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A module given the old keys serves nothing on the address they name.
func TestTheVoucherDoorDoesNotOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	raw := `{"voucher_addr":"` + addr + `","voucher_url":"https://node.example/x402/redeem"}`
	mod, err := New([]byte(raw))
	if err != nil {
		t.Fatalf("the module refused a config it ignores: %v", err)
	}
	m := mod.(*Module)
	h := newHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := m.Serve(ctx); err != nil {
		t.Fatal(err)
	}
	// A listener is started in the background, if at all: look for a while.
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			t.Fatalf("something listens on voucher_addr %s: the voucher door is open", addr)
		}
	}
}

// Structurally: the module's sources open no listener and serve no HTTP, so
// no configuration can bring a public door back.
func TestThePaymentModuleServesNoHTTP(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			switch {
			case pkg != nil && pkg.Name == "net" && strings.HasPrefix(sel.Sel.Name, "Listen"),
				pkg != nil && pkg.Name == "http" && (sel.Sel.Name == "Server" || sel.Sel.Name == "NewServeMux" ||
					strings.HasPrefix(sel.Sel.Name, "ListenAndServe") || sel.Sel.Name == "Serve" || sel.Sel.Name == "HandleFunc"):
				t.Errorf("%s: the payment module uses %s.%s: it has no public door to serve", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source file read; the check proves nothing")
	}
}

// The provider package names one door that carries a caller, the relay:
// ViaVoucher, the door where CallerAID was the payer a hub attested rather
// than the caller, is gone. Every package-level name starting with Via
// counts, constant or variable: a door brought back as a var is as open as
// one brought back as a const.
func TestTheOnlyDoorIsTheRelay(t *testing.T) {
	dir := filepath.Join("..", "..", "provider")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var vias []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, sp := range gd.Specs {
				for _, n := range sp.(*ast.ValueSpec).Names {
					if strings.HasPrefix(n.Name, "Via") {
						vias = append(vias, n.Name)
					}
				}
			}
		}
	}
	slices.Sort(vias)
	if !slices.Equal(vias, []string{"ViaRelay"}) {
		t.Fatalf("provider declares the doors %v; only ViaRelay may carry a caller (the voucher door is gone)", vias)
	}
}

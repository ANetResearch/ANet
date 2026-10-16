//go:build !no_mcp

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/mcpserv"
	"github.com/ANetResearch/ANet/module"
)

type nopControl struct{}

func (nopControl) Call(context.Context, string, any, any) error { return nil }

// TestMCPToolSetWhenAvailable (MOD-P6): whenever the mcp module is available
// the tools are exactly the fourteen the name table lists, whatever other
// modules are or are not there; the tool set is not computed from them.
func TestMCPToolSetWhenAvailable(t *testing.T) {
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcpserv.New(nopControl{}, "test").Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	slices.Sort(got)
	mcpInfo, _ := module.Lookup("mcp")
	if len(got) != 14 || !slices.Equal(got, mcpInfo.MCPTools) {
		t.Fatalf("tools %v\nname table %v", got, mcpInfo.MCPTools)
	}
}

package anetlink

import (
	"testing"

	"github.com/ANetResearch/ANetCore/coredet/coredettest"
)

// TestEveryStrictDecoderRefusesNullAndUndefined (core CORE-E4, revision 7): every type this
// package decodes with coredet.UnmarshalStrict refuses f6 (null) and 81 f7 (a list holding
// undefined). The rows are the package's call sites, found with go/ast: a new call without a row,
// or a row without a call, fails.
func TestEveryStrictDecoderRefusesNullAndUndefined(t *testing.T) {
	probs, err := coredettest.Check(".", map[string]func() any{
		"decodeInvokeReq &req": func() any { return new(invokeReq) },
		"decodeInvokeResp &r":  func() any { return new(invokeResp) },
		"decodeCapList &l":     func() any { return new(capList) },
		"decodeDescribe &d":    func() any { return new(describe) },
		"decodeRefusal &r":     func() any { return new(refusal) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		t.Error(p)
	}
}

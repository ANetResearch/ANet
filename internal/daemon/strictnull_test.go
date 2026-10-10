package daemon

import (
	"testing"

	"github.com/ANetResearch/ANetCore/coredet/coredettest"

	"github.com/ANetResearch/ANetCore/seal"
)

// TestEveryStrictDecoderRefusesNullAndUndefined (core CORE-E4, revision 7): every type this
// package decodes with coredet.UnmarshalStrict refuses f6 (null) and 81 f7 (a list holding
// undefined). The rows are the package's call sites, found with go/ast: a new call without a row,
// or a row without a call, fails.
//
// It is also this repository's census: every other package whose code decodes with
// UnmarshalStrict has a test of the same name.
func TestEveryStrictDecoderRefusesNullAndUndefined(t *testing.T) {
	probs, err := coredettest.Check(".", map[string]func() any{
		"decodeEncKeyFile &f":          func() any { return new(encKeyFile) },
		"decodeKeyStateNoticeFile &f":  func() any { return new(keyStateNoticeFile) },
		"decodeMboxFile &f":            func() any { return new(mboxKeyFile) },
		"(*Daemon).hubStoredMbox &set": func() any { return new(seal.EncKeySet) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		t.Error(p)
	}
	missing, n, err := coredettest.Census(anetRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no package decodes with UnmarshalStrict: the census did not see the code")
	}
	for _, m := range missing {
		t.Errorf("%s decodes with coredet.UnmarshalStrict and has no %s", m, coredettest.TestName)
	}
}

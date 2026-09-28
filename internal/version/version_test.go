package version_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/release"
	"github.com/ANetResearch/ANet/internal/version"
)

// wire2Release is the first anet release that speaks hub wire 2. A wire-2
// hub refuses older nodes with 426 "requires anet >= 0.2.0", anetpeer
// refuses unversioned frames with "peer requires anet >= 0.2.0", and the
// hub's llms.txt tells an agent to update anything older (A2A-DESIGN §18).
const wire2Release = "0.2.0"

// A binary that speaks wire 2 has to call itself at least 0.2.0.
//
// The version string is what people and programs compare: `anet update`
// refuses a "downgrade" by it, build-release.sh writes it into the signed
// manifest, and an operator reading a 426 compares it with `anet version`.
// A wire-2 build that still said 0.1.x would be told by every hub to
// upgrade to a release it already is, and `anet update` would treat the
// wire-1 0.1.x line as the same series.
func TestAWire2BuildIsAtLeastTheFirstWire2Release(t *testing.T) {
	cmp, err := release.CompareVersions(version.V, wire2Release)
	if err != nil {
		t.Fatalf("version.V = %q does not parse as a release version: %v", version.V, err)
	}
	if hubapi.WireVersion >= 2 && cmp < 0 {
		t.Errorf("this tree speaks hub wire %d but reports anet %s; the first wire-2 release is %s",
			hubapi.WireVersion, version.V, wire2Release)
	}
}

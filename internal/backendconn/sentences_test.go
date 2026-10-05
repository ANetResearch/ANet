package backendconn_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
)

// The listener check's refusals say what 02-ux §1.4 J says, word for word: their Reason (what a module
// logs) is the English sentence of their Code with their Args, which the daemon and doctor render in
// either language. backendconn does not import the reason table (it is shared by modules that must stay
// small, internal/loopguard), so the two are held together here.
func TestPeerRefusalsAreTheReasonTablesSentences(t *testing.T) {
	for _, c := range backendconn.PeerRefusalsForTest("/run/svc/b.sock") {
		var re *backendconn.RefusedError
		if !errors.As(c, &re) || re.Code == "" {
			t.Fatalf("%v is not a coded refusal", c)
		}
		if _, ok := a2ashape.Lookup(re.Code); !ok {
			t.Fatalf("%s is not in the reason table", re.Code)
		}
		args := a2ashape.Args{}
		for k, v := range re.Args {
			args[k] = v
		}
		if want := a2ashape.Sentence(re.Code, a2ashape.English, args); re.Reason != want {
			t.Errorf("%s:\n got %q\nwant %q", re.Code, re.Reason, want)
		}
		if zh := a2ashape.Sentence(re.Code, a2ashape.Chinese, args); zh == re.Reason {
			t.Errorf("%s has no Chinese sentence", re.Code)
		}
	}
}

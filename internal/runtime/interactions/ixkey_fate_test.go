package interactions

import (
	"errors"
	"testing"
)

// P-5 (engine P-ENG-5): ixKeyFate agrees with the table of FS §4.5.4 cell by cell, and every other
// combination — a cause on the wrong side of the task, an unknown cause or role, the zero Cause — is
// ErrBadInput.
func TestIXKeyFateTable(t *testing.T) {
	const bad ixFate = 0
	table := map[Role]map[Cause]ixFate{
		RoleInbound: { // provider
			CauseProviderEnd: fateDelete, // X7a
			CausePeerEnded:   bad,
			CauseLocalEnded:  bad,
			CauseNeverSent:   bad,
			CauseRetention:   fateDelete, // X7e
			CauseClosed:      fateDelete, // X7f
			CauseNotTerminal: fateKeep,
		},
		RoleOutbound: { // requester
			CauseProviderEnd: bad,
			CausePeerEnded:   fateDelete,     // X7b
			CauseLocalEnded:  fateCloseAfter, // X7c
			CauseNeverSent:   fateDelete,     // X7d
			CauseRetention:   fateDelete,     // X7e
			CauseClosed:      fateDelete,     // X7f
			CauseNotTerminal: fateKeep,
		},
	}
	causes := []Cause{CauseProviderEnd, CausePeerEnded, CauseLocalEnded, CauseNeverSent, CauseRetention,
		CauseClosed, CauseNotTerminal, "", "provider-end", "PEER_ENDED"}
	for _, role := range []Role{RoleInbound, RoleOutbound, "", "bystander"} {
		for _, c := range causes {
			want := table[role][c] // bad for an unknown role or cause
			got, err := ixKeyFate(role, c)
			switch {
			case want == bad && !errors.Is(err, ErrBadInput):
				t.Errorf("ixKeyFate(%q, %q) = %d, %v; want ErrBadInput", role, c, got, err)
			case want != bad && (err != nil || got != want):
				t.Errorf("ixKeyFate(%q, %q) = %d, %v; want %d", role, c, got, err, want)
			}
		}
	}
	// Every valid cause is a row of the table, and nothing else is valid.
	for _, c := range causes {
		_, inTable := table[RoleOutbound][c]
		if c.Valid() != inTable {
			t.Errorf("%q.Valid() = %v, but the table says %v", c, c.Valid(), inTable)
		}
	}
}

// When gives the cause only to a terminal target.
func TestCauseWhen(t *testing.T) {
	for _, st := range []State{StateSubmitted, StateWorking, StateInputRequired, StateCompleted, StateFailed,
		StateCanceled, StateRejected} {
		got := CausePeerEnded.When(st)
		want := CauseNotTerminal
		if st.IsTerminal() {
			want = CausePeerEnded
		}
		if got != want {
			t.Errorf("CausePeerEnded.When(%s) = %s, want %s", st, got, want)
		}
	}
}

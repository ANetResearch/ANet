package daemon

import (
	"fmt"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// The in-memory refused list keeps each envelope's reason (DLV-ST-9): the
// reason it was first refused with, until the entry is the oldest past the
// list's capacity, as the set it replaced did.
func TestRefusedLRUKeepsReason(t *testing.T) {
	l := refusedLRU{limit: 3}
	l.add("a", seal.NoteQuotaCallerPerDay)
	l.add("b", "")
	l.add("a", seal.NoteNotAccepting) // the same envelope again: what its sender was told stays
	for key, want := range map[string]string{"a": seal.NoteQuotaCallerPerDay, "b": ""} {
		if got, ok := l.get(key); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", key, got, ok, want)
		}
	}
	if got, ok := l.get("c"); ok || got != "" {
		t.Errorf("c was never refused: %q %v", got, ok)
	}
	l.add("c", seal.NoteDenied)
	l.add("d", seal.NoteBusy) // past the capacity: the first added goes
	if _, ok := l.get("a"); ok {
		t.Error("the oldest entry outlived the capacity")
	}
	for key, want := range map[string]string{"b": "", "c": seal.NoteDenied, "d": seal.NoteBusy} {
		if got, ok := l.get(key); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", key, got, ok, want)
		}
	}

	var def refusedLRU // the zero value has the default capacity
	for i := 0; i <= refusedLimit; i++ {
		def.add(fmt.Sprint(i), seal.NoteBusy)
	}
	if _, ok := def.get("0"); ok {
		t.Errorf("entry 0 outlived %d later ones", refusedLimit)
	}
	if r, ok := def.get("1"); !ok || r != seal.NoteBusy {
		t.Errorf("entry 1: %q %v", r, ok)
	}
}

// A delegation refused in step 9 is remembered with the reason it was
// refused with, in the refused table and in the in-memory list (DLV-ST-8,
// ST-9), and the list takes the table's reason when a replay finds only
// the table.
func TestARefusalIsRecordedWithItsReason(t *testing.T) {
	prov := newTestDaemon(t, "", false) // default: closed
	s := newStranger(t)
	ix := "ix_refused_with_reason"
	env := craft(t, s, prov, seal.TypeDelegate, ix, delegateBody(t, s.ctrl, ix, "a task", ""), nil)
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropNotAccepting {
		t.Fatalf("first delivery: %+v", r)
	}
	mid := mustMID(t, prov, env)
	exact, reason, _, err := prov.ix.Refused(s.aid, mid)
	if err != nil || !exact || reason != seal.NoteNotAccepting {
		t.Fatalf("refused table: exact %v reason %q (%v), want %q", exact, reason, err, seal.NoteNotAccepting)
	}
	if got, ok := prov.refused.get(replayKey(s.aid, mid)); !ok || got != seal.NoteNotAccepting {
		t.Fatalf("in-memory list: %q %v", got, ok)
	}
	prov.refused = refusedLRU{} // forgotten, as after a restart
	if r := receive(t, prov, env); r.reason != dropRefusedReplay {
		t.Fatalf("replay: %+v", r)
	}
	if got, ok := prov.refused.get(replayKey(s.aid, mid)); !ok || got != seal.NoteNotAccepting {
		t.Fatalf("in-memory list after the table answered: %q %v", got, ok)
	}
}

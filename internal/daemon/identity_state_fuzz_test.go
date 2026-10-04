package daemon

import (
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

// P8 (KEL §7.2): for any identity file, notice, notice error, publication
// error, hub row and time, identityStateOf is the table of KEL §4.3.1 (the
// model below, written as data), the gate is closed exactly when the state
// is not ok, an input a row does not read does not change its answer, and a
// notice only gets older; kelResealDue is D-51; and together the gate and the
// trigger keep the notice of every version the engine may transmit within
// the stop line (engine P-ENG-2 (iii)).
//
// go test -run '^$' -fuzz '^FuzzIdentityState$' -fuzztime 10m ./internal/daemon
func FuzzIdentityState(f *testing.F) {
	at := tsNow
	for _, age := range []int64{0, -1, 1, int64(NoticeResealAge), int64(NoticeResealAge) + 1,
		int64(NoticeStopLine), int64(NoticeStopLine) + 1, -int64(NoticeStopLine)} {
		for sel := range uint8(8) {
			f.Add(sel, sel, sel, sel, sel, sel%2 == 0, at, age, uint64(1), uint64(1+sel%2),
				int64(NoticeResealAge)+int64(sel)-4, int64(sel)-4)
		}
	}
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), false, uint64(0), int64(0), uint64(0), uint64(0), int64(0), int64(0))
	f.Add(uint8(255), uint8(255), uint8(255), uint8(255), uint8(255), true, ^uint64(0), int64(-1), ^uint64(0), ^uint64(0), int64(-1), int64(1))
	f.Fuzz(func(t *testing.T, fileSel, noticeSel, errSel, pubSel, hubSel uint8, pending bool,
		observedAt uint64, age int64, sealedKSS, curKSS uint64, sealedAge, newer int64) {
		now, ok := addSigned(observedAt, age)
		if !ok {
			return
		}
		in := identityInputs{File: fuzzFacts(fileSel, pending), Notice: fuzzNotice(noticeSel, observedAt),
			NoticeErr:  pick(errSel, "", noticeErrQuarantine, noticeErrNotAdmitted, "not_witness", "x"),
			PublishErr: pick(pubSel, "", hubKELTSOutOfWindow, "kel_invalid", "kel_fork"),
			HubReason:  pick(hubSel, "", reasonPending, reasonRetrying, reasonRetryingQuota, reasonHubRefused, reasonRegistered),
			Now:        now}

		got := identityStateOf(in)
		if want := stateModel(in); got != want {
			t.Fatalf("identityStateOf(%+v) = %+v; the table says %+v", in, got, want)
		}
		if g := gateOf(got); g.Open != (got.State == IdentityOK) {
			t.Fatalf("gate %+v for state %+v: open must be state == ok", g, got)
		}
		checkUnreadInputs(t, in, got)
		if later, ok := addSigned(now, int64(newer&0xffffffff)); ok {
			in2 := in
			in2.Now = later
			if r1, r2 := staleness(got), staleness(identityStateOf(in2)); r2 < r1 {
				t.Fatalf("at %d the state is %+v, at the later %d %+v: a notice only gets older", now, got, later, identityStateOf(in2))
			}
		}

		sealedAt, ok := addSigned(now, -sealedAge)
		if !ok {
			return
		}
		curObserved, ok := addSigned(sealedAt, newer)
		if !ok {
			return
		}
		row := KELSealed{SealedKSS: sealedKSS, SealedNoticeAt: sealedAt}
		cur := KeyCursor{KeyStateSeq: curKSS, NoticeObservedAt: curObserved, Now: now}
		due := kelResealDue(row, cur)
		t3 := sealedKSS != curKSS
		t4 := now > sealedAt && now-sealedAt > NoticeResealAge && curObserved > sealedAt
		if due != (t3 || t4) {
			t.Fatalf("kelResealDue(%+v, %+v) = %v; T3 %v, T4 %v", row, cur, due, t3, t4)
		}
		if !t3 && curObserved <= sealedAt && due {
			t.Fatalf("kelResealDue(%+v, %+v): no newer notice, yet a new version (M31)", row, cur)
		}

		// The gate of a node whose notice was observed at curObserved, and the
		// trigger with the same notice: a version the engine would transmit
		// as it is (gate open, no reseal due) carries a notice at most the
		// stop line old.
		node := identityInputs{File: witnessedFacts(), Notice: noticeAt(curObserved), Now: now}
		cur.KeyStateSeq = sealedKSS
		if gateOf(identityStateOf(node)).Open && !kelResealDue(row, cur) && sealedAt <= now && now-sealedAt > NoticeStopLine {
			t.Fatalf("gate open, no reseal due, yet the version's notice is %d ms old (row %+v, cursor %+v)", now-sealedAt, row, cur)
		}
	})
}

// stateModel is KEL §4.3.1 as a list of rows: the first that holds decides.
func stateModel(in identityInputs) IdentityState {
	f, n := in.File, in.Notice
	confirmed := n != nil && f.Witness != "" &&
		n.Body.AID == f.AID && n.Body.Seq == f.TipSeq && n.Body.Head == f.TipID && n.Body.Witness == f.Witness
	var age uint64
	if confirmed {
		age = max(in.Now, n.Body.ObservedAt) - n.Body.ObservedAt
	}
	hub := in.HubReason
	if hub == "" {
		hub = "no_hub_yet"
	}
	publishing := "rotation_pending"
	if in.PublishErr == "kel_ts_out_of_window" {
		publishing = "clock_skew"
	}
	stale := map[string]string{"quarantine": "identity.quarantine", "not_admitted": "hub_pending_admission"}[in.NoticeErr]
	if stale == "" {
		stale = "notice_stale"
	}
	rows := []struct {
		holds         bool
		state, reason string
	}{
		{f.State == "deactivated", "deactivated", "identity_deactivated"},
		{f.State != "ok", "stopped", "identity_stopped"},
		{f.Witness == "", "no_hub", hub},
		{f.Pending, "publishing", publishing},
		{!confirmed || age > 6*hourMS-10*minuteMS, "notice_stale", stale},
		{age > 3*hourMS, "ok", "identity.confirm_aging"},
		{true, "ok", "identity.ok"},
	}
	for _, r := range rows {
		if r.holds {
			return IdentityState{r.state, r.reason}
		}
	}
	panic("unreachable")
}

// checkUnreadInputs: an input only the rows below the deciding one read, or
// only another row reads, does not change the answer.
func checkUnreadInputs(t *testing.T, in identityInputs, got IdentityState) {
	t.Helper()
	vary := []struct {
		name  string
		reads []string // the states whose reason or choice this input decides
		mod   func(*identityInputs)
	}{
		{"the notice", []string{IdentityOK, IdentityNoticeStale}, func(in *identityInputs) { in.Notice = nil }},
		{"the notice error", []string{IdentityNoticeStale}, func(in *identityInputs) { in.NoticeErr = "" }},
		{"the publication error", []string{IdentityPublishing}, func(in *identityInputs) { in.PublishErr = "" }},
		{"the hub row", []string{IdentityNoHub}, func(in *identityInputs) { in.HubReason = "" }},
		{"the time", []string{IdentityOK, IdentityNoticeStale}, func(in *identityInputs) { in.Now = 0 }},
	}
	for _, v := range vary {
		in2 := in
		v.mod(&in2)
		if again := identityStateOf(in2); again != got {
			read := false
			for _, s := range v.reads {
				read = read || got.State == s
			}
			if !read {
				t.Fatalf("%+v: changing %s changed %+v to %+v, though %s does not read it", in, v.name, got, again, got.State)
			}
		}
	}
}

// staleness ranks how old the notice is by what the table says of it.
func staleness(s IdentityState) int {
	switch {
	case s.State == IdentityNoticeStale:
		return 2
	case s.Reason == reasonConfirmAging:
		return 1
	}
	return 0
}

func fuzzFacts(sel uint8, pending bool) identityFacts {
	f := witnessedFacts()
	f.Pending = pending
	switch sel % 8 {
	case 1:
		f.State = identity.StateStopped
	case 2:
		f.State = identity.StateDeactivated
	case 3:
		f.State = "" // not a state an identity file holds
	case 4:
		f.Witness, f.TipSeq, f.TipID = "", 0, tsAID // the icp alone
	case 5:
		f.TipSeq, f.TipID = 2, "bevent-2" // a later tip than the notice's
	case 6:
		f.Witness = "bother-hub" // switched hubs
	}
	return f
}

func fuzzNotice(sel uint8, observedAt uint64) *keyStateNotice {
	n := noticeAt(observedAt)
	switch sel % 6 {
	case 0:
		return nil
	case 1:
		n.Body.Seq = 0
	case 2:
		n.Body.Head = "bevent-1-fork"
	case 3:
		n.Body.AID = "banother-node"
	case 4:
		n.Body.Witness = "bother-hub"
	}
	return n
}

func pick(sel uint8, from ...string) string { return from[int(sel)%len(from)] }

// addSigned is u + d, and false when it would leave uint64.
func addSigned(u uint64, d int64) (uint64, bool) {
	if d >= 0 {
		r := u + uint64(d)
		return r, r >= u
	}
	m := uint64(-(d + 1)) + 1
	return u - m, m <= u
}

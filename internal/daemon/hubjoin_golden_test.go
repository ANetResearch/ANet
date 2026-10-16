package daemon

// G-JOIN (LEC §7.1): hub_join.json in each state, the /hub-register
// answers (dry_run's three actions, the four states, and an unprotected
// first join with its recovery reminder) and /status.hub in each state,
// none included. The files are in testdata/hubjoin/; -update-hubjoin
// rewrites them. The node's AID and the local rendering of a time are
// written as <AID> and <TIME>, so the files are the same on every machine.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

var updateHubJoinGolden = flag.Bool("update-hubjoin", false, "rewrite testdata/hubjoin")

// started is the daemon's start time as /status.hub shows it for none, for
// goldenText to replace.
var started string

// goldenHubAID is the hub AID of the samples.
const goldenHubAID = "bafyreihqsa3dn4fbfwbkn2dhwxbz6nmzvqjvhqrfmkmvjdnozqbgpj6vjy"

// goldenJoinRecord is one sample record and the name of its files.
type goldenJoinRecord struct {
	name string
	rec  joinRecord
}

// goldenJoinRecords are a record in each state, at now.
func goldenJoinRecords(now time.Time) []goldenJoinRecord {
	ms := now.UnixMilli()
	base := joinRecord{V: joinRecordVersion, Hub: "https://hub.example", HubAID: goldenHubAID, Since: ms - 60_000}
	pending := base
	pending.State, pending.Reason, pending.Attempts, pending.NextAttemptAt = JoinPendingAdmission, reasonPending, 3, ms+60_000
	pending.WitnessRot = &WitnessRot{NextDigest: bytes.Repeat([]byte{0x5a}, 32), HubAID: goldenHubAID}
	retrying := base
	retrying.State, retrying.Reason, retrying.Why, retrying.Attempts, retrying.NextAttemptAt =
		JoinRetrying, reasonRetrying, whyUnreachable, 1, ms+15_000
	retrying.WitnessRot = pending.WitnessRot
	quota := retrying
	quota.Reason, quota.Why, quota.NextAttemptAt = reasonRetryingQuota, "", ms+3_600_000
	refused := base
	refused.State, refused.Reason, refused.HubCode, refused.HubError, refused.Attempts =
		JoinRefused, reasonHubRefused, "kel_unknown", "The hub does not know this key history.", 1
	registered := base
	registered.State, registered.Reason, registered.RegisteredAt = JoinRegistered, reasonRegistered, ms-60_000
	probation := registered
	probation.Reason, probation.ProbationUntil = reasonProbation, uint64(ms+72*3_600_000)
	return []goldenJoinRecord{
		{"pending_admission", pending}, {"retrying", retrying}, {"retrying_quota", quota},
		{"refused", refused}, {"registered", registered}, {"registered_probation", probation},
	}
}

// goldenNow is the samples' "now": 2026-09-21T14:26:40Z.
var goldenNow = time.UnixMilli(1_790_000_800_000)

// goldenText is v as indented JSON with the machine's parts replaced: the
// node's AID, the local rendering of each time, and when the daemon
// started.
func goldenText(t *testing.T, v any, aid string, times ...time.Time) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if aid != "" {
		s = strings.ReplaceAll(s, aid, "<AID>")
		s = strings.ReplaceAll(s, a2ashape.Short8(aid), "<AID8>")
	}
	for _, tm := range times {
		s = strings.ReplaceAll(s, tm.Local().Format("01-02 15:04"), "<TIME>")
		s = strings.ReplaceAll(s, tm.Local().Format("2006-01-02 15:04"), "<TIME>")
	}
	if started != "" {
		s = strings.ReplaceAll(s, started, "<STARTED>")
	}
	return []byte(s + "\n")
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "hubjoin", name)
	if *updateHubJoinGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update-hubjoin to create it)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the code's output; if the change is intended, rerun with -update-hubjoin and review the diff:\n%s", path, got)
	}
}

func TestGJoin(t *testing.T) {
	d := newTestDaemon(t, "", false)
	now := goldenNow
	lang := a2ashape.English
	started = d.started.UTC().Format(time.RFC3339)
	defer func() { started = "" }()
	var names []string
	for _, g := range goldenJoinRecords(now) {
		r := g.rec
		times := []time.Time{time.UnixMilli(r.NextAttemptAt), time.UnixMilli(int64(r.ProbationUntil))}
		// hub_join.json, as written (with the record's own times).
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := decodeJoinRecord(b); err != nil || back.State != r.State {
			t.Fatalf("%s: the sample does not read back: %v", g.name, err)
		}
		var obj any
		_ = json.Unmarshal(b, &obj)
		checkGolden(t, "hub_join."+g.name+".json", goldenText(t, obj, ""))
		names = append(names, "hub_join."+g.name)
		// /status.hub.
		v := d.viewOfJoin(&r, now, lang)
		checkGolden(t, "status_hub."+g.name+".json", goldenText(t, v, d.AID(), times...))
		// The /hub-register answer, for the four states it answers with.
		if g.name == "retrying_quota" || g.name == "registered_probation" {
			continue
		}
		ans := d.registerAnswerAt(&r, RecoveryState{State: RecoveryProtected}, lang, now)
		checkGolden(t, "register."+g.name+".json", goldenText(t, ans, d.AID(), times...))
	}
	// none: no hub.
	checkGolden(t, "status_hub.none.json", goldenText(t, d.viewOfJoin(nil, now, lang), d.AID()))
	// An unprotected first join waiting for admission: the same state
	// and sentence, and the recovery reminder.
	recs := goldenJoinRecords(now)
	pending := recs[0].rec
	ans := d.registerAnswerAt(&pending, RecoveryState{State: RecoveryUnprotected}, lang, now)
	if ans.Recovery == nil || ans.State != JoinPendingAdmission {
		t.Fatalf("unprotected: %+v", ans)
	}
	plain := d.registerAnswerAt(&pending, RecoveryState{State: RecoveryProtected}, lang, now)
	if ans.State != plain.State || ans.Reason != plain.Reason || ans.Message != plain.Message || ans.Next != plain.Next {
		t.Fatal("unprotected changed the join state's answer")
	}
	checkGolden(t, "register.pending_admission.unprotected.json", goldenText(t, ans, d.AID(), time.UnixMilli(pending.NextAttemptAt)))
	// dry_run's three actions.
	for _, a := range []hubDryRunResp{
		{Hub: "https://hub.example", HubAID: goldenHubAID, Action: joinActionJoin, RecoveryNeeded: true},
		{Hub: "https://hub.example", HubAID: goldenHubAID, Action: joinActionUpdate},
		{Hub: "https://hub.example", HubAID: goldenHubAID, Action: joinActionSwitch},
	} {
		checkGolden(t, "register.dry_run."+a.Action+".json", goldenText(t, a, ""))
	}
	// The directory holds what this test writes, and nothing else.
	entries, err := os.ReadDir(filepath.Join("testdata", "hubjoin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 21 {
		var have []string
		for _, e := range entries {
			have = append(have, e.Name())
		}
		sort.Strings(have)
		t.Errorf("testdata/hubjoin holds %d files: %v", len(entries), have)
	}
	_ = names
}

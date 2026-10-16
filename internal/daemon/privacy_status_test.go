package daemon

// /status.privacy, the projection's retention date in its three places, the
// sentence for a task that is not here, and the 500s of the control plane
// (FS-11; FS §2.10, FS-U4, FS-U5, FS-U7).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func privacyOf(t *testing.T, d *Daemon) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodPost, "/status", nil))
	var st struct {
		Privacy map[string]any `json:"privacy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Privacy == nil {
		t.Fatalf("/status: %d %s", rec.Code, rec.Body)
	}
	return st.Privacy
}

// TestPrivacyRowPriority (FS-U7; mutation 40): the line /status.privacy
// says is the first row that holds, from the repeated rollback to the
// retention.
func TestPrivacyRowPriority(t *testing.T) {
	t.Setenv("LANG", "C")
	_, d, _ := registeredPair(t)
	c := newGateClocks(t, d, uint64(time.Now().UnixMilli()))
	d.setRollbackMarkForTest(nil)
	hubSays(d, c.wall.Load())

	p := privacyOf(t, d)
	if p["reason"] != "retention.summary" || p["clock"] != "ok" || p["deletion"] != "normal" || p["local_state_lost"] != nil ||
		p["content_retention"] != "30d" || p["public_retention"] != "7d" {
		t.Fatalf("a node that is well: %+v", p)
	}
	if !strings.Contains(p["message"].(string), "30 days") {
		t.Fatalf("message %q", p["message"])
	}
	ep := p["epoch_keys"].(map[string]any)
	if ep["published_days_ahead"] != float64(14) || ep["expired_kept"] != float64(0) || ep["kept_reason"] != "" {
		t.Fatalf("epoch_keys %+v", ep)
	}

	// Row 4: messages sealed to keys this node no longer holds.
	d.unknownKeyDrops.note(c.wall.Load())
	if p = privacyOf(t, d); p["reason"] != "keys.unknown_drops" || p["unknown_key_drops_24h"] != float64(1) {
		t.Fatalf("unknown drops: %+v", p)
	}

	// Row 2: the hub's answer is stale for over ten minutes (the check goes
	// stale at ten, so twenty-one in all).
	c.run(21 * 60 * 1000)
	if p = privacyOf(t, d); p["reason"] != "clock.unverified" || p["clock"] != "unverified" || p["deletion"] != "paused" {
		t.Fatalf("a clock the hub did not confirm: %+v", p)
	}
	// A short silence says nothing: the same check, eleven minutes old.
	hubSays(d, c.wall.Load())
	c.run(11 * 60 * 1000)
	if p = privacyOf(t, d); p["reason"] == "clock.unverified" {
		t.Fatalf("eleven minutes without the hub: %+v", p)
	}
	// A skewed clock is told as such, with the difference.
	hubSays(d, c.wall.Load()-20*60*1000)
	c.run(11 * 60 * 1000)
	if p = privacyOf(t, d); p["reason"] != "clock.skewed" || p["clock_skew_s"] != float64(1200) || !strings.Contains(p["message"].(string), " minutes off") {
		t.Fatalf("a skewed clock: %+v", p)
	}

	// Row 1 and row 0 beat the clock.
	at := c.wall.Load() - 3600_000
	if err := d.enc.setRollback(rollbackMark{At: at, Tasks: 2, Logged: true}); err != nil {
		t.Fatal(err)
	}
	p = privacyOf(t, d)
	if p["reason"] != "local_state.detected" || p["next"] != "anet identity rotate --emergency" {
		t.Fatalf("a rollback: %+v", p)
	}
	ls := p["local_state_lost"].(map[string]any)
	if ls["tasks"] != float64(2) || ls["repeated"] != false || ls["at"] != time.UnixMilli(int64(at)).UTC().Format(time.RFC3339) {
		t.Fatalf("local_state_lost %+v", ls)
	}
	if err := d.enc.setRollback(rollbackMark{At: at, PrevAt: at - 3600_000, Tasks: 0, Logged: true}); err != nil {
		t.Fatal(err)
	}
	if p = privacyOf(t, d); p["reason"] != "local_state.repeated" || p["local_state_lost"].(map[string]any)["repeated"] != true {
		t.Fatalf("a second rollback: %+v", p)
	}
	// A silent adoption shows nothing.
	if err := d.enc.setRollback(rollbackMark{At: at, Silent: true, Logged: true}); err != nil {
		t.Fatal(err)
	}
	if p = privacyOf(t, d); p["local_state_lost"] != nil || p["reason"] != "clock.skewed" {
		t.Fatalf("a silent adoption: %+v", p)
	}
}

// setRollbackMarkForTest clears the ring's rollback mark.
func (d *Daemon) setRollbackMarkForTest(m *rollbackMark) {
	d.enc.mu.Lock()
	d.enc.rollback = m
	d.enc.mu.Unlock()
}

// TestPrivacyRowSilentWhileNotRegistered (FS-U7): a node that is not
// registered at its hub does not say its clock is unconfirmed; the hub line
// says why, once.
func TestPrivacyRowSilentWhileNotRegistered(t *testing.T) {
	t.Setenv("LANG", "C")
	d := newTestDaemon(t, silentHub, true)
	c := newGateClocks(t, d, uint64(time.Now().UnixMilli()))
	c.run(30 * 60 * 1000)
	p := privacyOf(t, d)
	if p["clock"] != "unverified" || p["reason"] != "retention.summary" {
		t.Fatalf("a node not registered: %+v", p)
	}
}

// TestKeptLongNeedsTheMailbox (FS-U7 row 3): an expired key kept for the
// mailbox says so after six hours, and not before.
func TestKeptLongNeedsTheMailbox(t *testing.T) {
	t.Setenv("LANG", "C")
	_, d, _ := registeredPair(t)
	c := newGateClocks(t, d, uint64(time.Now().UnixMilli()))
	hubSays(d, c.wall.Load())
	d.enc.mu.RLock()
	k := d.enc.keys[0]
	d.enc.mu.RUnlock()
	due := lastAcceptance(k) + drainMarginMS
	for _, h := range []uint64{1, 7} {
		c.wall.Store(due + h*hourMS)
		hubSays(d, c.wall.Load())
		p := privacyOf(t, d)
		ep := p["epoch_keys"].(map[string]any)
		if ep["expired_kept"].(float64) < 1 || ep["kept_reason"] != "mailbox_not_drained" || ep["oldest_kept_extra_h"].(float64) < float64(h) {
			t.Fatalf("%d hours past: %+v", h, ep)
		}
		if got := p["reason"] == "keys.kept_long"; got != (h >= keptLongExtraH) {
			t.Fatalf("%d hours past: reason %q, epoch_keys %+v, clock %v", h, p["reason"], ep, p["clock"])
		}
	}
}

// TestContentExpiresAtSameInThreePlaces (FS-U4; mutation 36): the control
// plane, the local A2A interface (one agent's scope) and the control plane
// again through a list carry the same date, the one the sweep deletes by.
func TestContentExpiresAtSameInThreePlaces(t *testing.T) {
	t.Setenv("LANG", "C")
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task that ends", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.CancelTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	ix, err := req.ix.Get(id)
	if err != nil || !ix.IsTerminal() {
		t.Fatalf("setup: %+v %v", ix, err)
	}
	at, ok := interactions.ContentExpiresAt(ix.StateAt, interactions.ClassOutbound, req.ContentRetention())
	if !ok {
		t.Fatal("30 days is not a date")
	}
	want := time.UnixMilli(at).UTC().Format(time.RFC3339)

	ctl, err := req.getTask(controlScope, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := peerScope(prov.AID())
	a2a, err := req.getTask(sc, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := req.listTasks(controlScope, taskListReq{})
	if err != nil || len(page.Tasks) != 1 {
		t.Fatalf("list: %+v %v", page, err)
	}
	for place, got := range map[string]any{"control": ctl.Metadata[a2ashape.KeyContentExpiresAt],
		"a2a": a2a.Metadata[a2ashape.KeyContentExpiresAt], "list": page.Tasks[0].Metadata[a2ashape.KeyContentExpiresAt]} {
		if got != want {
			t.Errorf("%s: anet.content_expires_at %v, want %s", place, got, want)
		}
	}
	// A task still working has none.
	id2, err := req.Delegate(ctx, prov.AID(), "a task in flight", nil)
	if err != nil {
		t.Fatal(err)
	}
	if open, err := req.getTask(controlScope, id2, nil); err != nil || open.Metadata[a2ashape.KeyContentExpiresAt] != nil {
		t.Fatalf("a task in flight: %+v %v", open.Metadata, err)
	}
}

// TestNoLocalTaskIsOneSentence (FS-U5): the control plane answers 404 with
// the sentence, the code and the audit command; the A2A interface answers
// TaskNotFound with the same sentence — for a task that never was, one of
// another agent's, and one whose data key is gone.
func TestNoLocalTaskIsOneSentence(t *testing.T) {
	t.Setenv("LANG", "C")
	_, req, prov := registeredPair(t)
	rec := httptest.NewRecorder()
	req.hTasksGet(rec, httptest.NewRequest(http.MethodPost, "/tasks/get", strings.NewReader(`{"task_id":"ix_nothere"}`)))
	var body a2ashape.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := a2ashape.Sentence("no_local_task", a2ashape.English, a2ashape.Args{"task": "ix_nothere", "retention": "30d"})
	if body.Error != want || body.Reason != "no_local_task" || body.Next != "anet audit --interaction ix_nothere" {
		t.Fatalf("body %+v, want %q", body, want)
	}
	sc, _ := peerScope(prov.AID())
	_, err := req.getTask(sc, "ix_nothere", nil)
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Error() != want || !errors.Is(err, a2ashape.ErrTaskNotFound) {
		t.Fatalf("the A2A interface: %v", err)
	}
	if e := ref.A2A(a2ashape.English); e.Name != "TaskNotFoundError" || e.Message != want {
		t.Fatalf("A2A error %+v", e)
	}
}

// TestControlErrorsAreSentences (FS-11; the leftover of FS-9): with the
// store gone, no route of the control plane answers 500 with a Go error;
// each says local_write_failed, in a sentence.
func TestControlErrorsAreSentences(t *testing.T) {
	t.Setenv("LANG", "C")
	_, req, prov := registeredPair(t)
	id, err := req.Delegate(context.Background(), prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	cp := req.ControlHandler("tok").(*controlPlane)
	_ = req.ix.Close()
	saw := 0
	for _, path := range append(fuzzRoutes, "/retention") {
		body := `{"task_id":"` + id + `","interaction_id":"` + id + `","value":"7d","dry_run":true,"aid":"` + prov.AID() + `"}`
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		hr := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(
			context.WithValue(ctx, ctlPortKey{}, "4242"))
		w := httptest.NewRecorder()
		cp.api.ServeHTTP(w, hr)
		cancel()
		if w.Code != http.StatusInternalServerError {
			continue
		}
		saw++
		var e a2ashape.ErrorBody
		if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Reason != "local_write_failed" ||
			!strings.HasPrefix(e.Error, "This node could not write the change to its data directory (") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
		if _, ok := a2ashape.Lookup(e.Reason); !ok {
			t.Errorf("%s: reason %q is not in the table", path, e.Reason)
		}
	}
	if saw == 0 {
		t.Fatal("no route failed: the store was not taken away")
	}
}

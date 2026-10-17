package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// statusOf reads /status from a plane.
func statusFrom(t *testing.T, p *plane) map[string]json.RawMessage {
	t.Helper()
	resp, b := p.req(t, "GET", "/status", "", p.bearer)
	if resp.StatusCode != 200 {
		t.Fatalf("/status %d %s", resp.StatusCode, b)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func summaryOf(t *testing.T, m map[string]json.RawMessage) []a2ashape.SummaryLine {
	t.Helper()
	var s []a2ashape.SummaryLine
	if err := json.Unmarshal(m["summary"], &s); err != nil {
		t.Fatalf("summary: %v (%s)", err, m["summary"])
	}
	return s
}

// UX-T12: on a node with no hub, /status.summary has no identity line (UX-C9)
// and says the inbound, payments and privacy lines of a fresh install; the
// profile text moved to profile.summary.
func TestStatusSummaryOfAFreshNodeWithoutAHub(t *testing.T) {
	needPreviewModule(t)
	p := newPlane(t)
	if _, err := p.d.updateConfig(func(c *Config) error { c.Summary = "my profile"; return nil },
		func(c *Config) { c.Summary = "my profile" }); err != nil {
		t.Fatal(err)
	}
	m := statusFrom(t, p)
	var prof statusProfile
	if err := json.Unmarshal(m["profile"], &prof); err != nil || prof.Summary != "my profile" {
		t.Fatalf("profile %s (%v)", m["profile"], err)
	}
	var topics []string
	lines := summaryOf(t, m)
	for _, l := range lines {
		topics = append(topics, l.Topic)
		if l.Text == "" || l.Level == "" {
			t.Errorf("empty line %+v", l)
		}
		if l.Topic == a2ashape.TopicIdentity {
			t.Errorf("identity line without a hub: %+v", l)
		}
	}
	if got := strings.Join(topics, ","); !strings.Contains(got, "hub,inbound,payments,privacy") {
		t.Fatalf("topics %s", got)
	}
	// The wording is the reason table's, in this process's language.
	lang := a2ashape.Lang()
	want := map[string]string{
		a2ashape.TopicInbound:  a2ashape.Sentence("inbound.summary.closed", lang, a2ashape.Args{"n": 0, "m": 0, "auto_reply": "off"}),
		a2ashape.TopicPrivacy:  a2ashape.Sentence("retention.summary", lang, a2ashape.Args{"retention": "30d"}),
		a2ashape.TopicPayments: p.d.SpendStatus().LimitsSummary(lang),
	}
	for _, l := range lines {
		if w, ok := want[l.Topic]; ok && l.Text != w {
			t.Errorf("%s line %q, want %q", l.Topic, l.Text, w)
		}
	}
	if string(m["preview"]) == "" || !strings.Contains(string(m["preview"]), `"off"`) {
		t.Errorf("preview member %s", m["preview"])
	}
}

// The inbound line follows the policy and the auto-reply configuration; the
// preview line shows only when previews are on.
func TestStatusSummaryFollowsPolicyAndPreview(t *testing.T) {
	needPreviewModule(t)
	p := newPlane(t)
	d := p.d
	if _, err := d.updateConfig(func(c *Config) error {
		in := c.inbound()
		in.Policy = PolicyOpen
		c.Inbound = &in
		c.StreamPreview = "on"
		return nil
	}, func(c *Config) {
		in := c.inbound()
		in.Policy = PolicyOpen
		c.Inbound = &in
		c.StreamPreview = "on"
	}); err != nil {
		t.Fatal(err)
	}
	var inbound, preview string
	for _, l := range summaryOf(t, statusFrom(t, p)) {
		switch l.Topic {
		case a2ashape.TopicInbound:
			inbound = l.Text
		case a2ashape.TopicPreview:
			preview = l.Text
		}
	}
	lang := a2ashape.Lang()
	if inbound != a2ashape.Sentence("inbound.summary.open", lang, a2ashape.Args{"m": 0, "auto_reply": "off"}) {
		t.Errorf("inbound %q", inbound)
	}
	if preview != a2ashape.Sentence("preview.on", lang, nil) {
		t.Errorf("preview %q", preview)
	}
}

// A line of the delivery and sandbox members that another area words is
// carried as worded; nothing waiting hides the delivery line.
func TestSummaryCarriesWordedMembers(t *testing.T) {
	if deliverySaid(nil) != nil || deliverySaid(map[string]any{"message": "x"}) != nil {
		t.Error("delivery line with nothing waiting")
	}
	s := deliverySaid(map[string]any{"message": "3 messages are waiting.", "awaiting_confirmation": float64(3), "level": "ok"})
	if s == nil || s.Text != "3 messages are waiting." || s.Lvl != a2ashape.LevelOK {
		t.Errorf("delivery %+v", s)
	}
	st, bud := sandboxSaid(map[string]any{"state": "ready", "reason": "sandbox.ready", "message": "ready.",
		"budget": map[string]any{"today": float64(2), "runs_per_day": float64(20), "runs_per_peer_per_day": float64(5)}})
	if st == nil || st.Text != "ready." || bud == nil || bud.Args["n"] != 2 || bud.Args["daily"] != 20 {
		t.Errorf("sandbox %+v %+v", st, bud)
	}
	if st, bud := sandboxSaid(map[string]any{"state": "unavailable", "reason": "no_bwrap", "message": "no bwrap"}); st == nil || st.Lvl != a2ashape.LevelFail || bud != nil {
		t.Errorf("unavailable sandbox %+v %+v", st, bud)
	}
	if st, _ := sandboxSaid(nil); st != nil {
		t.Error("sandbox line without a member")
	}
}

// While a hub waits to admit this node, the hub line of the summary says when anet asks it again; once
// the node is registered the sentence is gone.
func TestStatusSummaryNamesTheNextCheckWhileWaitingForAdmission(t *testing.T) {
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.admission = fakeAdmitClosed
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	if code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, `,"next_digest":"`+testDigest+`"`)); code != 200 ||
		out["state"] != JoinPendingAdmission {
		t.Fatalf("join a closed hub: %d %v", code, out)
	}
	rec := d.joinSnapshot()
	if rec == nil || rec.NextAttemptAt == 0 {
		t.Fatalf("no next attempt recorded: %+v", rec)
	}
	for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		var hub *a2ashape.SummaryLine
		lines := d.statusSummary(map[string]any{}, lang)
		for i := range lines {
			if lines[i].Topic == a2ashape.TopicHub {
				hub = &lines[i]
			}
		}
		if hub == nil {
			t.Fatalf("no hub line: %+v", lines)
		}
		tail := a2ashape.Sentence("hub.next_check", lang, a2ashape.Args{"time": time.UnixMilli(rec.NextAttemptAt), "now": time.Now()})
		if !strings.HasSuffix(hub.Text, tail) {
			t.Errorf("%v: hub line %q does not end with %q", lang, hub.Text, tail)
		}
	}
	if w := joinBackoff(rec.Attempts, 0); w > time.Minute {
		t.Errorf("the wait between two questions is %v, more than a minute", w)
	}
}

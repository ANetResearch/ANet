package main

// doctor's clock, keys.epoch, retention and local_state (FS-11; ux §5.2,
// UX-T9): what each says from /status.privacy, and without the daemon.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

func privacyFindings(t *testing.T, layout daemon.Layout, st map[string]any, hub string) []string {
	t.Helper()
	rep := &doctorReport{}
	rep.Control.Running = st != nil
	env := doctorEnv{status: func(string, string) (map[string]any, error) { return st, nil }}
	cfg := daemon.DefaultConfig()
	cfg.HubURL = hub
	var got []string
	say := func(id, status, key string, args a2ashape.Args) {
		got = append(got, id+" "+status+" "+key)
	}
	privacyChecks(say, rep, env, layout, cfg, time.Now())
	got = append(got, "|")
	for _, c := range rep.Checks {
		got = append(got, c.ID+" "+c.Status+" "+c.Reason+" "+c.Deletion)
	}
	return got
}

func TestDoctorPrivacyChecks(t *testing.T) {
	t.Setenv("LANG", "C")
	layout := daemon.NewLayout(t.TempDir())
	privacy := func(m map[string]any) map[string]any {
		base := map[string]any{"content_retention": "30d", "clock": "ok", "clock_skew_s": float64(-3), "deletion": "normal",
			"epoch_keys":            map[string]any{"published_days_ahead": float64(14), "expired_kept": float64(0), "oldest_kept_extra_h": float64(0), "kept_reason": ""},
			"unknown_key_drops_24h": float64(0), "local_state_lost": nil}
		for k, v := range m {
			base[k] = v
		}
		return map[string]any{"privacy": base, "clock": map[string]any{"ts_retries": float64(0)}}
	}
	for _, c := range []struct {
		name string
		st   map[string]any
		want string
	}{
		{"well", privacy(nil), "clock ok clock.ok|keys.epoch ok keys.epoch_ok|retention info retention.summary"},
		{"no hub", privacy(map[string]any{"clock": "no_hub"}), "clock info clock.no_hub|keys.epoch ok keys.epoch_ok|retention info retention.summary"},
		{"unverified for a minute", privacy(map[string]any{"clock": "unverified", "clock_closed_s": float64(60)}),
			"clock ok clock.ok|keys.epoch ok keys.epoch_ok|retention info retention.summary"},
		{"unverified for ten minutes", privacy(map[string]any{"clock": "unverified", "clock_closed_s": float64(600), "deletion": "paused"}),
			"keys.epoch ok keys.epoch_ok|retention info retention.summary|"},
		{"skewed", privacy(map[string]any{"clock": "skewed", "clock_closed_s": float64(900), "clock_skew_s": float64(1200), "deletion": "local_clock"}),
			"keys.epoch ok keys.epoch_ok|retention info retention.summary|"},
		{"kept long", privacy(map[string]any{"epoch_keys": map[string]any{"expired_kept": float64(2), "oldest_kept_extra_h": float64(6), "kept_reason": "held_messages"}}),
			"clock ok clock.ok|keys.epoch warn keys.kept_long|retention info retention.summary"},
		{"kept for the clock only", privacy(map[string]any{"epoch_keys": map[string]any{"expired_kept": float64(2), "oldest_kept_extra_h": float64(30), "kept_reason": "clock"}}),
			"clock ok clock.ok|keys.epoch ok keys.epoch_ok|retention info retention.summary"},
		{"unknown keys", privacy(map[string]any{"unknown_key_drops_24h": float64(3)}),
			"clock ok clock.ok|keys.epoch warn keys.unknown_drops|retention info retention.summary"},
		{"a rollback", privacy(map[string]any{"local_state_lost": map[string]any{"at": "2026-10-10T10:00:00Z", "tasks": float64(0), "repeated": false}}),
			"clock ok clock.ok|keys.epoch ok keys.epoch_ok|retention info retention.summary|local_state warn local_state.detected"},
		{"a second rollback", privacy(map[string]any{"local_state_lost": map[string]any{"at": "2026-10-10T10:00:00Z", "tasks": float64(1), "repeated": true}}),
			"clock ok clock.ok|keys.epoch ok keys.epoch_ok|retention info retention.summary|local_state fail local_state.repeated"},
	} {
		got := privacyFindings(t, layout, c.st, "https://hub.example")
		// The say callback and the report's own checks are told apart by "|":
		// the clock warnings are appended to the report with their deletion.
		line := strings.Join(got, "|")
		line = strings.Replace(line, "||", "|", 1)
		line = strings.TrimSuffix(line, "|")
		want := c.want
		switch c.name {
		case "unverified for ten minutes":
			want = "keys.epoch ok keys.epoch_ok|retention info retention.summary||clock warn clock.unverified paused"
		case "skewed":
			want = "keys.epoch ok keys.epoch_ok|retention info retention.summary||clock warn clock.skewed local_clock"
		}
		if !sameFindings(line, want) {
			t.Errorf("%s:\n got:  %s\n want: %s", c.name, line, want)
		}
	}
}

// sameFindings compares two lists of findings, ignoring the separator
// between what the say callback collected and what went to the report.
func sameFindings(a, b string) bool {
	norm := func(s string) string {
		return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '|' }), "|")
	}
	return norm(a) == norm(b)
}

// TestDoctorPrivacyWithoutTheDaemon: the key ring file and config.json are
// read; a ring that cannot be judged is unknown, a value that is not a
// setting is a fail.
func TestDoctorPrivacyWithoutTheDaemon(t *testing.T) {
	t.Setenv("LANG", "C")
	dir := t.TempDir()
	layout := daemon.NewLayout(dir)
	if got := strings.Join(privacyFindings(t, layout, nil, ""), "|"); !strings.Contains(got, "clock info clock.no_hub") ||
		strings.Contains(got, "keys.epoch") || !strings.Contains(got, "retention info retention.summary") {
		t.Errorf("no hub, no ring: %s", got)
	}
	if got := strings.Join(privacyFindings(t, layout, nil, "https://hub.example"), "|"); !strings.Contains(got, "clock unknown doctor.daemon_down") {
		t.Errorf("a hub, daemon down: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"content_retention":"0d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(privacyFindings(t, layout, nil, ""), "|"); !strings.Contains(got, "retention fail retention_invalid") {
		t.Errorf("an invalid retention: %s", got)
	}
}

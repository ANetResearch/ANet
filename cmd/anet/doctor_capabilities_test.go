package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

type finding struct{ id, status, key string }

func collect() (func(id, status, key string, args a2ashape.Args), *[]finding) {
	var got []finding
	return func(id, status, key string, _ a2ashape.Args) { got = append(got, finding{id, status, key}) }, &got
}

func pol(t *testing.T, cfgJSON string, allow, trust, deny, payees []string) daemon.PolicyState {
	t.Helper()
	var cfg daemon.Config
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	in := cfg.Inbound
	st := daemon.PolicyState{Config: cfg, Allow: allow, Trust: trust, Deny: deny, Payees: payees, Payments: cfg.Payments.Limits()}
	if in != nil {
		st.Inbound = *in
	}
	return st
}

func render(fs []finding) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, fmt.Sprintf("%s/%s/%s", f.id, f.status, f.key))
	}
	return strings.Join(parts, " ")
}

// The capability items of ux §5.2, one row per condition.
func TestCapabilityDoctorItems(t *testing.T) {
	svc := func(timeoutMS int) string {
		return fmt.Sprintf(`"modules":{"service":{"capabilities":[{"id":"c","url":"unix:///x","timeout_ms":%d}]}}`, timeoutMS)
	}
	for _, c := range []struct {
		name  string
		cfg   string
		lists [3][]string
		want  string
	}{
		{"nothing public", `{}`, [3][]string{}, ""},
		{"files not served by the service module", `{"inbound":{"public_capabilities":[{"id":"c","files":{"count":1,"max_bytes":10}}]}}`,
			[3][]string{}, "capabilities.files/fail/capabilities.files_not_service"},
		{"files served by the service module", `{"inbound":{"public_capabilities":[{"id":"c","files":{"count":1,"max_bytes":10}}]},` + svc(1000) + `}`,
			[3][]string{}, ""},
		{"long timeout, one slot", `{"inbound":{"public_capabilities":[{"id":"c","max_inflight":1}]},` + svc(61000) + `}`,
			[3][]string{}, "capabilities.inflight/warn/capabilities.inflight_low"},
		{"timeout at 60 s", `{"inbound":{"public_capabilities":[{"id":"c","max_inflight":1}]},` + svc(60000) + `}`, [3][]string{}, ""},
		{"long timeout, two slots", `{"inbound":{"public_capabilities":[{"id":"c","max_inflight":2}]},` + svc(61000) + `}`, [3][]string{}, ""},
		{"allow_only with an empty list", `{"inbound":{"public_capabilities":[{"id":"c","allow_only":true}]}}`,
			[3][]string{}, "capabilities.allow_only/warn/capabilities.allow_only_empty"},
		{"allow_only with callers", `{"inbound":{"public_capabilities":[{"id":"c","allow_only":true}]}}`,
			[3][]string{{"a"}, {"b"}, nil}, "capabilities.allow_only/info/capabilities.allow_only"},
		{"allow_only, the only caller denied", `{"inbound":{"public_capabilities":[{"id":"c","allow_only":true}]}}`,
			[3][]string{{"a"}, nil, {"a"}}, "capabilities.allow_only/warn/capabilities.allow_only_empty"},
	} {
		say, got := collect()
		capabilitiesChecks(say, pol(t, c.cfg, c.lists[0], c.lists[1], c.lists[2], nil))
		if render(*got) != c.want {
			t.Errorf("%s: %q, want %q", c.name, render(*got), c.want)
		}
	}
}

func TestPreviewDoctorItem(t *testing.T) {
	needModule(t, "autoreply")
	for _, c := range []struct{ cfg, want string }{
		{`{}`, ""},
		{`{"stream_preview":"off"}`, ""},
		{`{"stream_preview":"on","auto_reply":{"backend":"openai"}}`, "preview/info/preview.on"},
		{`{"stream_preview":"on"}`, "preview/info/preview.on preview/info/preview.no_source"},
		{`{"stream_preview":"with_reasoning","auto_reply":{"backend":"exec","agent":"x"}}`,
			"preview/warn/preview.with_reasoning preview/info/preview.no_source"},
	} {
		say, got := collect()
		var cfg daemon.Config
		if err := json.Unmarshal([]byte(c.cfg), &cfg); err != nil {
			t.Fatal(err)
		}
		previewCheck(say, cfg)
		if render(*got) != c.want {
			t.Errorf("%s: %q, want %q", c.cfg, render(*got), c.want)
		}
	}
}

func TestPaymentsDoctorItem(t *testing.T) {
	for _, c := range []struct {
		cfg     string
		payees  []string
		spend   map[string]any
		want    string
		sentTxt string
	}{
		{`{}`, nil, nil, "payments/info/limits_summary.off", ""},
		{`{"payments":{"auto_max":2}}`, nil, nil, "payments/info/limits_summary payments/warn/payments.no_payees", ""},
		{`{"payments":{"agent_max":2,"agent_daily_max":5}}`, []string{"p"}, nil, "payments/info/limits_summary", ""},
		{`{}`, nil, map[string]any{"left_today": float64(7)}, "payments/info/limits_summary.off", "7"},
	} {
		say, got := collect()
		st := pol(t, c.cfg, nil, nil, nil, c.payees)
		paymentsCheck(say, st, c.spend)
		if render(*got) != c.want {
			t.Errorf("%s: %q, want %q", c.cfg, render(*got), c.want)
		}
	}
	// The remainder in the sentence is the running daemon's.
	var args a2ashape.Args
	paymentsCheck(func(_, _, _ string, a a2ashape.Args) { args = a }, pol(t, `{}`, nil, nil, nil, nil), map[string]any{"left_today": float64(7)})
	if args["left"] != uint64(7) {
		t.Errorf("left %v", args["left"])
	}
}

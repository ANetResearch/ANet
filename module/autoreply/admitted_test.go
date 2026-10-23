package autoreply

// auto_reply.admitted (design v0.3.9 §A): how the exec backend runs the turn
// of a peer that inbound.admit admits (module.Turn.Admitted).

import (
	"encoding/json"
	"testing"
)

func TestGateForAdmittedPeers(t *testing.T) {
	exec := func(admitted, untrusted, trusted string) AutoReplyConfig {
		return AutoReplyConfig{Backend: "exec", Agent: "claude", Admitted: admitted, Untrusted: untrusted, Trusted: trusted}
	}
	for _, c := range []struct {
		name              string
		cfg               AutoReplyConfig
		trusted, admitted bool
		mode              string
		counted           bool
	}{
		{"admitted, direct", exec("direct", "", ""), false, true, modeDirect, false},
		{"admitted, sandbox", exec("sandbox", "", ""), false, true, modeSandbox, true},
		{"admitted, off: as untrusted (off)", exec("off", "", ""), false, true, "", false},
		{"admitted, unset: as untrusted (off)", exec("", "", ""), false, true, "", false},
		{"admitted, off: as untrusted (sandbox)", exec("off", "sandbox", ""), false, true, modeSandbox, true},
		{"not admitted, admitted=direct", exec("direct", "", ""), false, false, "", false},
		{"not admitted, admitted=direct, untrusted sandbox", exec("direct", "sandbox", ""), false, false, modeSandbox, true},
		{"trusted wins over admitted", exec("sandbox", "", "direct"), true, true, modeDirect, false},
		{"trusted sandbox", exec("direct", "", "sandbox"), true, false, modeSandbox, false},
		{"openai, admitted", AutoReplyConfig{Model: "m", APIBase: "http://x", Admitted: "direct"}, false, true, modeOpenAI, true},
	} {
		g := gateFor(c.cfg, c.trusted, c.admitted)
		if g.mode != c.mode || g.counted != c.counted || g.trusted != c.trusted {
			t.Errorf("%s: %+v, want mode %q counted %v trusted %v", c.name, g, c.mode, c.counted, c.trusted)
		}
	}
}

func TestAdmittedIsChecked(t *testing.T) {
	for _, v := range []string{"", "off", "direct", "sandbox"} {
		if ref := checkAutoReplyStart(AutoReplyConfig{Admitted: v}); ref != nil {
			t.Errorf("admitted %q refused: %v", v, ref)
		}
	}
	for _, v := range []string{"on", "trusted", "Direct"} {
		ref := checkAutoReplyStart(AutoReplyConfig{Admitted: v})
		if ref == nil || ref.Key != "config_invalid" || ref.Args["key"] != "auto_reply.admitted" {
			t.Errorf("admitted %q: %v, want config_invalid for auto_reply.admitted", v, ref)
		}
		if err := checkAutoReplyBlock(json.RawMessage(`{"backend":"exec","agent":"claude","admitted":"`+v+`"}`), ""); err == nil {
			t.Errorf("the block with admitted %q passes the start check", v)
		}
	}
	// A write merges it like any member.
	cur := &AutoReplyConfig{Backend: "exec", Agent: "claude"}
	next, ref := mergeAutoReply(cur, map[string]json.RawMessage{"admitted": json.RawMessage(`"direct"`)})
	if ref != nil || next.AdmittedMode() != AdmittedDirect {
		t.Fatalf("merge admitted=direct: %+v %v", next, ref)
	}
	if _, ref := mergeAutoReply(cur, map[string]json.RawMessage{"admitted": json.RawMessage(`"always"`)}); ref == nil {
		t.Error("merge admitted=always accepted")
	}
	// Audited and reported.
	if a := auditOfBlock(json.RawMessage(`{"backend":"exec","agent":"claude","admitted":"sandbox"}`)); a["admitted"] != "sandbox" {
		t.Errorf("audit admitted = %v", a["admitted"])
	}
	if a := auditOfBlock(nil); a["admitted"] != "off" {
		t.Errorf("audit without a block: admitted = %v, want off", a["admitted"])
	}
	if s := si5OfBlock(json.RawMessage(`{"admitted":"direct"}`)); s["auto_reply.admitted"] != "direct" || s["auto_reply.untrusted"] != "off" {
		t.Errorf("si5 = %v", s)
	}
	if s := si5OfBlock(nil); s["auto_reply.admitted"] != "off" {
		t.Errorf("si5 without a block = %v", s)
	}
}

package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// LEC §2.3.4: api_key is written, never read back. A request that changes only the allowlist keeps the
// stored key, and no answer of POST /autoreply shows it (`anet autoreply set --egress-allow` prints the
// answer).
func TestAutoReplyAnswerHasNoKey(t *testing.T) {
	d := newTestDaemon(t, "", false)
	const key = "anet-test-key-written-only"
	for _, body := range []string{
		`{"backend":"exec","agent":"cursor","untrusted":"sandbox","api_key":"` + key + `"}`,
		`{"sandbox":{"egress_allow":["pypi.org"]}}`,
		`{"sandbox":{"egress_allow":[]}}`,
	} {
		rec := httptest.NewRecorder()
		d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(body)))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), key) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if got := AutoReplyOf(d.config()).APIKey; got != key {
		t.Errorf("the stored key is %q after changing only the allowlist", got)
	}
}

// LEC §3.2: a config.json edited to hold an entry CN-1 refuses does not start, with the entry's sentence.
func TestEgressAllowIsCheckedAtStart(t *testing.T) {
	for _, tc := range []struct {
		autoReply map[string]any
		reason    string
	}{
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"pypi.org"}}}, ""},
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"PyPI.org"}}}, ""},
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"169.254.169.254"}}}, "egress_allow_invalid"},
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"api.anthropic.com"}}}, "egress_allow_model_host"},
		{map[string]any{"backend": "exec", "agent": "codex", "api_base": "https://llm.example.org/v1", "model": "m",
			"sandbox": map[string]any{"egress_allow": []string{"llm.example.org"}}}, "egress_allow_model_host"},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": tc.autoReply})
		err := CheckConfigForStart(NewLayout(root))
		var cr *ConfigRefusal
		switch {
		case tc.reason == "" && err != nil:
			t.Errorf("%v: %v", tc.autoReply, err)
		case tc.reason != "" && (!errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Key != tc.reason):
			t.Errorf("%v: %v, want %s", tc.autoReply, err, tc.reason)
		}
	}
}

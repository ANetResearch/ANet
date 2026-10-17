package a2ashape

import "testing"

// CodeSuffix names the reason after a sentence; a hub's own code follows
// hub_refused after a colon; a value that is not a code shows nothing.
func TestCodeSuffix(t *testing.T) {
	for _, tc := range []struct{ reason, hub, want string }{
		{"busy", "", " [busy]"},
		{"hub_refused", "quota_full", " [hub_refused:quota_full]"},
		{"hub_refused", "", " [hub_refused]"},
		{"hub_refused", "Bad Code\n", " [hub_refused]"},
		{"busy", "quota_full", " [busy]"},
		{"", "", ""},
		{"Not A Code", "", ""},
		{"x402:PAYMENT", "", ""},
	} {
		if got := CodeSuffix(tc.reason, tc.hub); got != tc.want {
			t.Errorf("CodeSuffix(%q, %q) = %q, want %q", tc.reason, tc.hub, got, tc.want)
		}
	}
}

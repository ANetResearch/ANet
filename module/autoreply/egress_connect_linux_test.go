//go:build linux

package autoreply

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// SI-55, CN-2, CN-5 in a real bubblewrap sandbox: an agent with auto_reply.sandbox.egress_allow = [pypi.org]
// reaches https://pypi.org through the proxy variables it is given — CONNECT on the sandbox's
// 127.0.0.1:3128, bridged to the run's endpoint — and its TLS session passes through to the host the
// daemon resolved and checked; the same request with another TLS name is dropped, and a host not on the
// list is 403. The run's evidence counts each.
func TestSandboxIntegrationConnectsOnlyToAllowedHosts(t *testing.T) {
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	srv, conns := tlsUpstream(t)
	f := newFakeModelService(t)
	_ = testUpstream(t, egressUpstreamAnthropic, f) // the built-in service's address is the fake's
	fn := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: srv.Listener.Addr().String()}
	prev := newEgressConnectNet
	newEgressConnectNet = fn.connectNet
	t.Cleanup(func() { newEgressConnectNet = prev })
	d := newBareDaemon(t)
	req, _ := json.Marshal(sandboxProbeRequest{Egress: true, HostURL: f.srv.URL + "/v1/models", OperatorKey: testOperatorKey,
		ConnectURL: "https://pypi.org/simple/"})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	openSandboxTask(t, d, "ix-connect")
	res, err := d.runSandboxed(ctx, sandboxRequest{
		Cfg: AutoReplyConfig{Backend: "exec", Agent: "claude", Command: self, APIKey: testOperatorKey, APITimeoutSeconds: 50,
			Sandbox: &AutoReplySandboxConfig{EgressAllow: []string{"PyPI.org"}}},
		RC: replyContext{Role: "inbound", InteractionID: "ix-connect",
			Goal: sandboxProbeMarker + string(req) + sandboxProbeEnd},
		Turns: []chatTurn{{Role: "user", Text: "probe"}},
	})
	if err != nil {
		t.Fatalf("sandboxed probe: %v", err)
	}
	var got sandboxProbeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Reply)), &got); err != nil {
		t.Fatalf("probe reply %q: %v", res.Reply, err)
	}
	t.Logf("probe result inside the sandbox: %+v", got)
	if got.Error != "" {
		t.Fatalf("probe: %s", got.Error)
	}
	if got.TunnelStatus != 200 || got.TunnelBody != "upstream pypi.org" {
		t.Errorf("https://pypi.org through the tunnel: %d %q", got.TunnelStatus, got.TunnelBody)
	}
	if !got.WrongNameFailed {
		t.Error("a TLS name other than the CONNECT's went through")
	}
	if got.ConnectStatus != 403 || got.HostReached {
		t.Errorf("CONNECT to a host not allowed: %d; the model service reached directly: %v", got.ConnectStatus, got.HostReached)
	}
	if conns.Load() != 1 {
		t.Errorf("the allowed host saw %d connections, want 1", conns.Load())
	}
	if _, dialed := fn.seen(); len(dialed) != 1 || dialed[0] != netip.AddrPortFrom(testPublicAddr, 443) {
		t.Errorf("dialed %v", dialed)
	}
	if res.Egress == nil || res.Egress.Connect.Allowed["pypi.org"] != 1 || res.Egress.Connect.Refused[egressSNIMismatch] != 1 ||
		res.Egress.Connect.Refused["egress_host_not_allowed"] != 1 || res.Egress.Connect.BytesUp == 0 || res.Egress.Connect.BytesDown == 0 {
		t.Errorf("the run's egress counts: %+v", res.Egress)
	}
	ents, _ := os.ReadDir(sandboxRunRoot())
	if len(ents) != 0 {
		t.Errorf("run directories left behind: %d", len(ents))
	}
}

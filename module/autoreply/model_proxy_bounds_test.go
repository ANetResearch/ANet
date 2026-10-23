package autoreply

// model_proxy_bounds_test.go — what keeps the model proxy available to every run: a body slot per run
// and a deadline for every request body, an answer that streams for longer than any read timeout, no
// connection of a local account other than the node's and the runner's, and a bound per account.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/module"
)

// shortProxyTimeouts shortens the proxy's timeouts for one test; set before the proxy starts.
func shortProxyTimeouts(t *testing.T, header, body, read, idle time.Duration) {
	h, b, r, i := modelProxyHeaderTimeout, proxyBodyTimeout, modelProxyReadTimeout, modelProxyIdleTimeout
	modelProxyHeaderTimeout, proxyBodyTimeout, modelProxyReadTimeout, modelProxyIdleTimeout = header, body, read, idle
	t.Cleanup(func() {
		modelProxyHeaderTimeout, proxyBodyTimeout, modelProxyReadTimeout, modelProxyIdleTimeout = h, b, r, i
	})
}

// trickle opens a connection to the proxy and sends a request whose body never completes.
func trickle(t *testing.T, p *modelProxy, path, cred string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n"+
		"Content-Length: 100000\r\n\r\n{", path, p.addr, cred)
	return c
}

// quickPost is post with a client timeout, reporting the error instead of failing.
func quickPost(url, cred, body string, timeout time.Duration) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// One run's unfinished request bodies hold up its own requests only: another run's model request is
// answered at once, and a body that does not arrive within proxyBodyTimeout is given up, so the run's own
// next request goes through too.
func TestModelProxyBodiesAreBoundedPerRun(t *testing.T) {
	quietLog(t)
	shortProxyTimeouts(t, 5*time.Second, 400*time.Millisecond, 10*time.Second, 30*time.Second)
	up := newFakeUpstream(t, "ok")
	p := newTestProxy(t, up, "sk-upstream", "")
	slow, err := p.begin(proxyRunSpec{Peer: "aidA", Ix: "ixA", Model: true, MaxRequests: 100, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.end()
	other, err := p.begin(proxyRunSpec{Peer: "aidB", Ix: "ixB", Model: true, MaxRequests: 100, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer other.end()
	path := egressRoutePrefix + "plan/chat/completions"
	held := []net.Conn{trickle(t, p, path, slow.key), trickle(t, p, path, slow.key), trickle(t, p, path, slow.key)}
	time.Sleep(100 * time.Millisecond)

	url := p.modelBase() + "/plan/chat/completions"
	start := time.Now()
	if status, err := quickPost(url, other.key, chatBody, 2*time.Second); err != nil || status != http.StatusOK {
		t.Fatalf("another run's request beside one run's unfinished bodies: %d %v", status, err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("another run's request waited %s", d)
	}
	// Each unfinished body is given up after proxyBodyTimeout: the connection is answered and closed.
	for i, c := range held {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Errorf("unfinished body %d: the connection was not ended: %v", i, err)
		}
	}
	if status, err := quickPost(url, slow.key, chatBody, 2*time.Second); err != nil || status != http.StatusOK {
		t.Errorf("the run's own request after its bodies were given up: %d %v", status, err)
	}
}

// A turn tool call whose body never completes is given up after proxyBodyTimeout; a call whose service
// takes longer than that is answered.
func TestTurnCallBodyDeadline(t *testing.T) {
	quietLog(t)
	shortProxyTimeouts(t, 5*time.Second, 300*time.Millisecond, 400*time.Millisecond, 30*time.Second)
	p, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.setService(slowSvc{after: time.Second})
	run, err := p.begin(proxyRunSpec{Peer: "aidA", Ix: "ixA", Turn: true, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer run.end()
	c := trickle(t, p, p.turnURL()[len("http://"+p.addr):]+"/call", run.token)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("the unfinished call was not ended: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the unfinished call was ended after %s", d)
	}
	res, err := turnClient(t, p, run.token).Call(context.Background(), "board.post", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(res), "slow") {
		t.Errorf("a call whose service takes longer than the read timeouts: %s %v", res, err)
	}
}

// slowSvc is a service whose every call takes a while.
type slowSvc struct{ after time.Duration }

func (slowSvc) AgentTools() []module.AgentTool { return []module.AgentTool{{Capability: "board.post"}} }

func (s slowSvc) CallServiceAs(ctx context.Context, _, _ string, _ json.RawMessage, _ string) (json.RawMessage, error) {
	select {
	case <-time.After(s.after):
		return json.RawMessage(`{"answer":"slow"}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// An answer that streams for longer than every read timeout of the proxy reaches the run whole.
func TestModelProxyStreamsPastTheReadTimeouts(t *testing.T) {
	quietLog(t)
	shortProxyTimeouts(t, 200*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond)
	const chunks = 12
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"part%d \"}}]}\n\n", i)
			f.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	p, err := newModelProxy(AutoReplyModelProxyConfig{Listen: "127.0.0.1:0", Upstreams: []ModelProxyUpstream{
		{Name: "plan", APIBase: upstream.URL + "/v1", APIKeyFile: writeKeyFile(t, "k")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	run, err := p.begin(proxyRunSpec{Peer: "aidA", Ix: "ixA", Model: true, MaxRequests: 10, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer run.end()
	req, _ := http.NewRequest(http.MethodPost, p.modelBase()+"/plan/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+run.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got strings.Builder
	text, err := readChatStream(resp.Body, func(string, string) {})
	got.WriteString(text)
	if err != nil || !strings.Contains(got.String(), fmt.Sprintf("part%d", chunks-1)) {
		t.Fatalf("the stream was cut: %q %v", got.String(), err)
	}
}

// A connection of a local account other than the node's and the runner's is closed when it is accepted,
// before it counts; the runner's account is served (and refused without a key, as ever).
func TestModelProxyRefusesOtherAccounts(t *testing.T) {
	quietLog(t)
	up := newFakeUpstream(t, "ok")
	p := newTestProxy(t, up, "sk-upstream", "")
	// A test cannot connect as a second account; localpeer reports the connection from a port it was
	// told to treat as another user's as that user's (uid+1), as the socket table would.
	dialAs := func() (net.Conn, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		local := ln.Addr().(*net.TCPAddr)
		ln.Close()
		restore := localpeer.TreatAsForeignForTest(local.String())
		t.Cleanup(restore)
		d := net.Dialer{LocalAddr: local, Timeout: 2 * time.Second}
		return d.Dial("tcp", p.addr)
	}
	ask := func(c net.Conn) (string, error) {
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprintf(c, "GET %splan/models HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", egressRoutePrefix, p.addr)
		b, err := io.ReadAll(c)
		return string(b), err
	}
	c, err := dialAs()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ask(c); got != "" {
		t.Fatalf("another account's connection was answered: %q", got)
	}
	c.Close()
	if n := p.conns.Load(); n != 0 {
		t.Errorf("%d connections counted after the refusal", n)
	}

	// The runner's account.
	p.setAccounts(func() []int { return []int{os.Getuid() + 1} })
	c, err = dialAs()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, _ := ask(c); !strings.Contains(got, "401") || !strings.Contains(got, "model_proxy_unauthorized") {
		t.Errorf("the runner's account without a key: %q", got)
	}
}

// Keyless connections of other accounts take nothing from the runs, and one account holds at most
// modelProxyMaxConnsPerUID connections while another account's are still taken.
func TestModelProxyConnectionBounds(t *testing.T) {
	quietLog(t)
	var foreign atomic.Bool
	prev := proxyPeerUID
	proxyPeerUID = func(c net.Conn) (int, error) {
		if foreign.Load() {
			return os.Getuid() + 7, nil
		}
		return prev(c)
	}
	up := newFakeUpstream(t, "ok")
	p := newTestProxy(t, up, "sk-upstream", "")
	proxyPeerUID = prev // the proxy took its own
	run, err := p.begin(proxyRunSpec{Peer: "aidB", Ix: "ixB", Model: true, MaxRequests: 100, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer run.end()

	// A stranger opens as many idle connections as the whole bound; each is closed at once.
	foreign.Store(true)
	for i := 0; i < modelProxyMaxConns; i++ {
		c, err := net.Dial("tcp", p.addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Fatalf("a stranger's connection %d was kept: %d %v", i, n, err)
		}
		c.Close()
	}
	foreign.Store(false)
	if status, err := quickPost(p.modelBase()+"/plan/chat/completions", run.key, chatBody, 3*time.Second); err != nil || status != http.StatusOK {
		t.Fatalf("a run's request after a stranger's connections: %d %v", status, err)
	}

	// The node's own account holds its bound of idle connections; the next one is closed, while the
	// runner's account is still served.
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < modelProxyMaxConnsPerUID; i++ {
		c, err := net.Dial("tcp", p.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.conns.Load() < modelProxyMaxConnsPerUID && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := quickPost(p.modelBase()+"/plan/chat/completions", run.key, chatBody, 2*time.Second); err == nil {
		t.Error("a connection over the account's bound was served")
	}
	p.setAccounts(func() []int { return []int{os.Getuid() + 7} })
	foreign.Store(true)
	if status, err := quickPost(p.modelBase()+"/plan/chat/completions", run.key, chatBody, 3*time.Second); err != nil || status != http.StatusOK {
		t.Errorf("the runner's account beside the node's full bound: %d %v", status, err)
	}
}

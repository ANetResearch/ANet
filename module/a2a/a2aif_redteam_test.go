//go:build !no_a2a

package a2a

// a2aif_redteam_test.go: adversarial PoCs for the local A2A interface's
// HTTP face. Each test asserts that the attack SUCCEEDS: a passing test
// means the defect is present.

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// (F32 — a provider's file inline in a stream event broke every a2a-go
// client's stream — is fixed; its regression test is q12_test.go.)

// A recorded port that is taken when the daemon starts is silently given
// up for a new one (addr.go listen: "refusing to start would take the whole
// node down over a port"). The clients wired to the old address (Hermes'
// a2a_agents, with the bearer token in them) do not "stop working" as the
// comment and §11.1 say: they keep sending their requests, each with
// Authorization: Bearer <a2a token>, to whoever now holds the old port. On a
// multi-user host another local user who binds 127.0.0.1:<recorded port>
// while the daemon is down collects the token and then uses it against the
// daemon's new port (scan 43811-45810): tasks as this node, the content of
// every outbound task, agent-tier payments.
func TestRedteamA2AIF_TakenPortMovesAndWiredClientsLeakTheToken(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	// A free port inside the allocator's range, recorded as a first start
	// would have recorded it.
	var squat net.Listener
	var old string
	for p := portBase + 1500; p < portBase+portSpan; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			squat, old = ln, ln.Addr().String()
			break
		}
	}
	if squat == nil {
		t.Skip("no free port in range")
	}
	defer squat.Close()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(old+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := loadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The daemon starts while the other user holds the port: it moves.
	ln, err := listen(dir)
	if err != nil {
		t.Fatalf("listen refused the taken port (fail-closed): %v — defect absent", err)
	}
	defer ln.Close()
	if ln.Addr().String() == old {
		t.Fatal("did not move")
	}

	// The squatter answers like the interface would.
	got := make(chan string, 1)
	go func() {
		c, err := squat.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		got <- req.Header.Get("Authorization")
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
	}()

	// A wired client (Hermes' a2a_agents entry: url + bearer token) fetches
	// its agent's card, as it does on every start.
	req, _ := http.NewRequestWithContext(context.Background(), "GET",
		"http://"+old+agentsPath+"/bafyremoteagent/.well-known/agent-card.json", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	select {
	case h := <-got:
		if h != "Bearer "+token {
			t.Fatalf("squatter saw %q", h)
		}
		t.Logf("daemon moved %s -> %s; the wired client handed the local A2A token to the process on %s",
			old, ln.Addr(), old)
	case <-time.After(5 * time.Second):
		t.Fatal("the squatter saw no request")
	}
}

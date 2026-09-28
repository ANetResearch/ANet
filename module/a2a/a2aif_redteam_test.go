//go:build !no_a2a

package a2a

// a2aif_redteam_test.go: adversarial PoCs for the local A2A interface's
// HTTP face. Each test asserts that the attack SUCCEEDS: a passing test
// means the defect is present.

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The kernel puts a provider's files inline in every task view it gives the
// local A2A interface, stream events included (internal/daemon
// TestRedteamA2AIF_ProviderAttachmentsInlinedWithoutQ12Cap). One file of
// ~8 MiB from the remote provider makes the stream's first event — the Task
// snapshot — a single SSE data line longer than a2a-go's client accepts
// (internal/sse MaxSSETokenSize, 10 MB): SubscribeToTask and
// SendStreamingMessage fail for every a2a-go client for as long as the file
// is in the task, while Q12 said stream events carry only metadata.
func TestRedteamA2AIF_ProviderFileBreaksA2AGoStreams(t *testing.T) {
	for _, binding := range bindings {
		t.Run(string(binding), func(t *testing.T) {
			e := newEnv(t)
			cl, ctx := e.client(agentA, binding)
			e.seam.quote = map[string]any{"accepts": []any{}} // leaves the task waiting (input-required)
			res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("q")})
			if err != nil {
				t.Fatal(err)
			}
			id := string(res.(*a2a.Task).ID)
			// The provider's question, with the file inline, as the kernel
			// projects it for this interface.
			e.seam.mu.Lock()
			ft := e.seam.tasks[id]
			q := a2ashape.Message{ID: "p1", Role: a2ashape.RoleAgent, TaskID: id, ContextID: ft.t.ContextID,
				Parts: []a2ashape.Part{a2ashape.TextPart("see file"),
					a2ashape.RawPart(bytes.Repeat([]byte{'z'}, 8<<20), "big.bin", "application/octet-stream")}}
			e.seam.setState(ft, a2ashape.TaskStateInputRequired, &q)
			e.seam.mu.Unlock()

			var streamErr error
			for _, err := range cl.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id)}) {
				if err != nil {
					streamErr = err
					break
				}
				t.Fatal("the stream delivered an event: defect absent")
			}
			if streamErr == nil || !strings.Contains(streamErr.Error(), "too long") {
				t.Fatalf("stream error %v, want the SSE line limit", streamErr)
			}
			t.Logf("%s: a2a-go client stream fails: %v", binding, streamErr)
		})
	}
}

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

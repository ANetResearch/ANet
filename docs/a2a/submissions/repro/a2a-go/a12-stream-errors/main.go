// A12: how a streaming call that cannot start is refused.
//
// Server: a2asrv writes "200 OK, text/event-stream" before it asks the
// handler for the first event, so SubscribeToTask on a task that does not
// exist is answered with a stream whose one event is the TaskNotFound error.
// a2a-tck (STREAM-SUB-004) counts that as the stream having been opened.
//
// Client: the JSON-RPC transport treats any 200 as SSE and skips lines that
// are not "data:", so an ordinary JSON-RPC error answer (200,
// application/json) is read as an empty stream without an error; for a
// non-200 answer it reports only the HTTP status, not the A2A error in the
// body.
//
// Run: go run ./a12-stream-errors
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// errBody is a JSON-RPC TaskNotFound error answering the request with id.
func errBody(id json.RawMessage) string {
	return `{"jsonrpc":"2.0","id":` + string(id) + `,"error":{"code":-32001,"message":"task not found"}}`
}

func main() {
	// Server side.
	executor := a2asrv.AgentExecutorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	})
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))
	req, err := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"SubscribeToTask","params":{"id":"no-such-task"}}`))
	must(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	srv.Close()
	serverOpens := resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	fmt.Printf("server: SubscribeToTask(unknown task) -> HTTP %d, %s, body %q\n",
		resp.StatusCode, resp.Header.Get("Content-Type"), strings.TrimSpace(string(body)))

	// Client side: three ways a server might refuse the stream.
	answers := []struct {
		name, contentType string
		status            int
		sse               bool
	}{
		{"200, text/event-stream, error event", "text/event-stream", 200, true},
		{"200, application/json error object", "application/json", 200, false},
		{"400, application/json error object", "application/json", 400, false},
	}
	clientMisreads := false
	for _, a := range answers {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in struct{ ID json.RawMessage }
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.Header().Set("Content-Type", a.contentType)
			w.WriteHeader(a.status)
			if a.sse {
				fmt.Fprint(w, "data: "+errBody(in.ID)+"\n\n")
			} else {
				fmt.Fprint(w, errBody(in.ID))
			}
		}))
		transport := a2aclient.NewJSONRPCTransport(stub.URL, stub.Client())
		events, got := 0, "no error"
		for _, err := range transport.SubscribeToTask(context.Background(), a2aclient.ServiceParams{}, &a2a.SubscribeToTaskRequest{ID: "no-such-task"}) {
			if err != nil {
				got = err.Error()
				break
			}
			events++
		}
		stub.Close()
		fmt.Printf("client: %-36s -> %d events, error: %s\n", a.name, events, got)
		if !strings.Contains(got, "task not found") {
			clientMisreads = true
		}
	}
	fmt.Println("expected: the server refuses with the binding's ordinary error; the client surfaces TaskNotFound for every form")

	if serverOpens || clientMisreads {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fmt.Println("setup error:", err)
		os.Exit(2)
	}
}

// A8: docs/topics/extensions.md, "Extension Activation", step 3: "the
// response SHOULD include the A2A-Extensions header, listing all extensions
// that were successfully activated for that request." The JSON-RPC and
// HTTP+JSON handlers never set it (only the gRPC handler returns activated
// extensions, as metadata), so an HTTP client cannot tell whether an
// extension it asked for was applied.
//
// Run: go run ./a8-extensions-response-header
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

var ext = a2a.AgentExtension{URI: "https://example.org/ext/timing/v1"}

// activator activates the extension whenever the client requests it, as the
// e2e "durations" example in a2a-go does, and counts the calls in which the
// server itself reports it as activated.
type activator struct{}

var activated atomic.Int32

func (activator) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if e, ok := a2asrv.ExtensionsFrom(ctx); ok && e.Requested(&ext) {
		e.Activate(&ext)
	}
	return ctx, nil, nil
}

func (activator) After(ctx context.Context, callCtx *a2asrv.CallContext, resp *a2asrv.Response) error {
	if slices.Contains(callCtx.Extensions().ActivatedURIs(), ext.URI) {
		activated.Add(1)
	}
	return nil
}

func main() {
	executor := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("ok")), nil)
		}
	})
	handler := a2asrv.NewHandler(executor, a2asrv.WithCallInterceptors(activator{}))
	jsonrpc := httptest.NewServer(a2asrv.NewJSONRPCHandler(handler))
	defer jsonrpc.Close()
	rest := httptest.NewServer(a2asrv.NewRESTHandler(handler))
	defer rest.Close()

	msg := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}`
	calls := []struct{ name, url, body string }{
		{"JSON-RPC SendMessage", jsonrpc.URL, `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":` + msg + `}`},
		{"JSON-RPC SendStreamingMessage", jsonrpc.URL, `{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":` + msg + `}`},
		{"HTTP+JSON POST /message:send", rest.URL + "/message:send", msg},
		{"HTTP+JSON POST /message:stream", rest.URL + "/message:stream", msg},
	}
	missing := 0
	for _, c := range calls {
		req, err := http.NewRequest("POST", c.url, strings.NewReader(c.body))
		must(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		req.Header.Set("A2A-Extensions", ext.URI)
		resp, err := http.DefaultClient.Do(req)
		must(err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		got := resp.Header.Get("A2A-Extensions")
		if got == "" {
			missing++
			got = "(absent)"
		}
		fmt.Printf("%-32s HTTP %d, response A2A-Extensions: %s\n", c.name, resp.StatusCode, got)
	}
	fmt.Printf("server-side ActivatedURIs() contained the extension in %d of %d calls\n", activated.Load(), len(calls))
	fmt.Printf("expected: A2A-Extensions: %s on every response\n", ext.URI)
	if missing > 0 && activated.Load() > 0 {
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

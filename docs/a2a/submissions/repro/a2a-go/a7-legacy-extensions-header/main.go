// A7: the v1 handlers ignore the A2A 0.3 header X-A2A-Extensions, which
// extension specifications written for 0.3 still prescribe. a2a-x402 v0.2
// §8: "Clients MUST request activation of this extension by including its
// URI in the X-A2A-Extensions HTTP header", and §3.1 recommends
// required: true. Such a client is refused by an a2a-go v2 server.
//
// Run: go run ./a7-legacy-extensions-header
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
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// The a2a-x402 extension URI of spec v0.2.
const x402 = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"

func main() {
	executor := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("ok")), nil)
		}
	})
	caps := &a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: x402, Required: true}}}
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor, a2asrv.WithCapabilityChecks(caps))))
	defer srv.Close()

	legacy := send(srv.URL, "X-A2A-Extensions")
	current := send(srv.URL, "A2A-Extensions")
	fmt.Printf("SendMessage with X-A2A-Extensions: <a2a-x402 URI>: %s (a2a-x402 v0.2 §8 client)\n", legacy)
	fmt.Printf("SendMessage with A2A-Extensions: <a2a-x402 URI>:   %s (control)\n", current)
	fmt.Println("expected: the legacy header is honoured during the transition, as a2acompat/a2av0 does for 0.3 requests")

	if legacy != "ok" && current == "ok" {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func send(url, header string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	must(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set(header, x402)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	must(json.Unmarshal(b, &r))
	if r.Error != nil {
		return fmt.Sprintf("error %d %s", r.Error.Code, r.Error.Message)
	}
	return "ok"
}

func must(err error) {
	if err != nil {
		fmt.Println("setup error:", err)
		os.Exit(2)
	}
}

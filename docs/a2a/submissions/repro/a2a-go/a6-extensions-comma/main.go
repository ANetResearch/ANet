// A6: A2A-Extensions is a comma-separated list (A2A §3.2.6; §11.2: several
// values SHOULD be sent comma-separated in one header field). a2asrv keeps
// the header value unsplit, so a client that requests two extensions in one
// header activates neither, and a request for a required extension made
// that way is refused with ExtensionSupportRequiredError.
//
// Run: go run ./a6-extensions-comma
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

const (
	extA = "https://example.org/ext/a/v1"
	extB = "https://example.org/ext/b/v1"
)

func main() {
	// Unit level: what the server sees for the spec's header form.
	ctx, _ := a2asrv.NewCallContext(context.Background(), a2asrv.NewServiceParams(map[string][]string{
		"A2A-Extensions": {extA + ", " + extB},
	}))
	ext, _ := a2asrv.ExtensionsFrom(ctx)
	requestedA := ext.Requested(&a2a.AgentExtension{URI: extA})
	fmt.Printf("RequestedURIs():  %q\n", ext.RequestedURIs())
	fmt.Printf("Requested(a):     %v (expected true)\n", requestedA)

	// End to end: an agent that requires both extensions.
	executor := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("ok")), nil)
		}
	})
	caps := &a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: extA, Required: true}, {URI: extB, Required: true}}}
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor, a2asrv.WithCapabilityChecks(caps))))
	defer srv.Close()

	oneField := send(srv.URL, []string{extA + "," + extB})
	twoFields := send(srv.URL, []string{extA, extB})
	fmt.Printf("SendMessage, A2A-Extensions: a,b (one field): %s (expected: ok)\n", oneField)
	fmt.Printf("SendMessage, two A2A-Extensions fields:       %s (control)\n", twoFields)

	if !requestedA || oneField != "ok" {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func send(url string, extHeaderLines []string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	must(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	for _, v := range extHeaderLines {
		req.Header.Add("A2A-Extensions", v)
	}
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

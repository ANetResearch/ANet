// A10: a SecurityScheme with a variant a2a-go does not know (one added by a
// later protocol version, say) makes the whole AgentCard fail to unmarshal,
// including the schemes the client does know, so card resolution fails.
// A2A §5.7: "Implementations SHOULD ignore unrecognized fields in messages,
// allowing for forward compatibility as the protocol evolves."
//
// Run: go run ./a10-unknown-securityscheme
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

const card = `{"name":"n","description":"d","version":"1",
  "supportedInterfaces":[{"url":"https://agent.example/a2a","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
  "capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
  "skills":[{"id":"s","name":"s","description":"d","tags":["t"]}],
  "securitySchemes":{"bearer":{"httpAuthSecurityScheme":{"scheme":"Bearer"}},
                     "future":{"someFutureSecurityScheme":{"x":"y"}}},
  "securityRequirements":[{"schemes":{"bearer":{"list":[]}}},{"schemes":{"future":{"list":[]}}}]}`

func main() {
	var c a2a.AgentCard
	errUnmarshal := json.Unmarshal([]byte(card), &c)
	fmt.Printf("json.Unmarshal into a2a.AgentCard: %v\n", errOrOK(errUnmarshal))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, card)
	}))
	defer srv.Close()
	_, errResolve := agentcard.DefaultResolver.Resolve(context.Background(), srv.URL)
	fmt.Printf("agentcard.DefaultResolver.Resolve: %v\n", errOrOK(errResolve))
	fmt.Println("expected: the card parses; the unknown scheme is kept opaque and the bearer requirement stays usable")

	if errUnmarshal != nil || errResolve != nil {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func errOrOK(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// A2: encoding/json on a2a.AgentCard writes null for REQUIRED lists that are
// nil, writes the `optional bool` fields streaming and pushNotifications
// as false when they were never set, and drops an explicit
// extendedAgentCard:false. ProtoJSON never writes null for a repeated field
// and keeps presence for `optional` fields (A2A §5.7, §8.4.1 rule 1).
//
// Run: go run ./a2-json-presence
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func main() {
	// 1. A card built in Go without the REQUIRED lists.
	card := &a2a.AgentCard{Name: "n", Description: "d", Version: "1",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("https://agent.example", a2a.TransportProtocolJSONRPC)},
		Skills:              []a2a.AgentSkill{{ID: "s", Name: "s", Description: "d"}},
	}
	built, err := json.Marshal(card)
	must(err)
	fmt.Printf("built in Go:         %s\n", built)

	// 2. A card that sets extendedAgentCard to false and leaves streaming and
	//    pushNotifications unset, read and written back.
	in := `{"name":"n","description":"d","version":"1",` +
		`"supportedInterfaces":[{"url":"https://agent.example","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
		`"capabilities":{"extendedAgentCard":false},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],` +
		`"skills":[{"id":"s","name":"s","description":"d","tags":["t"]}]}`
	var parsed a2a.AgentCard
	must(json.Unmarshal([]byte(in), &parsed))
	out, err := json.Marshal(&parsed)
	must(err)
	fmt.Printf("input:               %s\n", in)
	fmt.Printf("after a round trip:  %s\n", out)

	nullLists := strings.Contains(string(built), `"defaultInputModes":null`) && strings.Contains(string(built), `"tags":null`)
	invented := strings.Contains(string(out), `"streaming":false`) && strings.Contains(string(out), `"pushNotifications":false`)
	lost := !strings.Contains(string(out), `"extendedAgentCard"`)
	fmt.Printf("REQUIRED lists written as null:                     %v\n", nullLists)
	fmt.Printf("unset optional streaming/pushNotifications written: %v\n", invented)
	fmt.Printf("explicit extendedAgentCard:false dropped:           %v\n", lost)
	fmt.Println("expected (ProtoJSON, A2A §5.7): none of the three")

	if nullLists || invented || lost {
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

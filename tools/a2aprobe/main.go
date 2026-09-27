// a2aprobe is the A2A client of scripts/joint-a2a.sh (A2A-DESIGN §17, the
// joint-a2a row; plan 0014 B5-03).
//
// Every other test of the local A2A interface drives it with a fake kernel
// behind it; this one drives a real requester daemon, through a real hub, to
// a real provider daemon, with a client that knows nothing about anet: a2a-go's
// own client, unmodified — the card resolved with the bearer token, the
// token handed to AuthInterceptor through a CredentialsService, the
// extension activated with a2aext's activator. What it can do, any agent
// with an A2A SDK can do; what it cannot, no such agent can.
//
//	a2aprobe run       --a2a-addr H:P --token-file F --agent AID [options]
//	a2aprobe again     --client FILE --state FILE [options]
//	a2aprobe responder --ctl H:P --token-file F
//	a2aprobe backend   --addr H:P
//
// run is the scenario: the proxy card and its bearer, SendMessage blocking
// and with returnImmediately, SendStreamingMessage, GetTask, ListTasks by
// contextId, CancelTask, a client whose card lost its securityRequirements,
// another agent's path, and the a2a-x402 same-task flow with the payment
// message of §8.7 (payment-submitted without a payload, the option in
// anet.payment.accept) and its three refusals: above the agent tier, an
// option the quote did not offer, a payload of the client's own. It writes
// the client's configuration (the address and the token, as Hermes keeps
// them), the ids of the tasks it made, and the raw JSON-RPC responses of its
// blocking sends for the Hermes contract (internal/a2ashape
// TestHermesReadsJointRecord).
//
// again runs after the requester daemon restarted: the configuration run
// wrote must still reach the agent, and the tasks must still be there.
//
// responder and backend are the provider's side, which a test has to play
// itself: responder is the provider's agent (it answers text tasks over the
// provider's control plane, /tasks/list and /tasks/reply, the routes MCP's
// reply_task uses), backend the service behind the provider's capabilities.
// Neither touches the A2A interface.
//
// Output: one line per check — "PASS id: detail", "FAIL id: detail", or
// "NOTE id: detail" for something to know that is not a failure. The exit
// status is 0 when nothing failed, 1 when a check failed, 2 when the probe
// could not run. Tokens are read from files and never printed.
//
// It links a2a-go and nothing of the daemon: the metadata keys and URIs it
// reads are spelled out here, from the design, so a rename on the daemon's
// side shows up as a failure rather than following along.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "again":
		code = cmdAgain(os.Args[2:])
	case "responder":
		code = cmdResponder(os.Args[2:])
	case "backend":
		code = cmdBackend(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "a2aprobe: unknown command %q\n", os.Args[1])
		usage()
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  a2aprobe run       --a2a-addr H:P --token-file F --agent AID [--nonce S] [--paid SKILL --pricey SKILL]
                     [--registry] [--client-out F] [--state F] [--record F] [--timeout D]
  a2aprobe again     --client F --state F [--record F] [--timeout D]
  a2aprobe responder --ctl H:P --token-file F [--interval D] [--hold MARK]
  a2aprobe backend   --addr H:P
`)
	os.Exit(2)
}

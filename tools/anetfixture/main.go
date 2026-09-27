// anetfixture mints the signed objects a joint run needs.
//
// Two of the daemon's capabilities take an object the CALLER signed:
// blackboard.add takes a CogUnit signed by its author, org.verify takes a
// membership credential signed by its issuer. That is deliberate — a board
// that stamped contributions on arrival would destroy the authorship it
// exists to prove — but it means neither can be driven from the CLI alone,
// and "we tested it in a unit test where both halves are ours" is how a
// seam ships broken.
//
// So this is the agent side of those calls, using the daemon's own
// identity so the provider can resolve the signer's KEL through the hub
// exactly as it would for a real peer.
//
//	anetfixture cogunit       --home DIR [--task T] [--type claim] --body TEXT
//	anetfixture org-genesis   --home DIR [--nonce N]
//	anetfixture org-credential --home DIR --genesis B64 --subject AID [--role member]
//	anetfixture x402-authorize --home DIR --pay-to AID --amount N --network hub:AID [--interaction ID]
//	anetfixture relay-sign    --home DIR --hub URL --action send --method POST --path /relay/send --body-file F
//	anetfixture relay-sign    --home DIR --v1 --action task.create
//	anetfixture seal          --home DIR --hub URL --to AID [--as AID --kel self|claimed] …   (attack.go)
//	anetfixture relay-send    --to AID --envelope B64|@FILE [--home DIR --hub URL] [--p2p ADDR] (attack.go)
//
// The first four print one base64 line, ready for `anet delegate … --args`.
// relay-sign prints request headers, seal an envelope, relay-send one JSON
// line per delivery.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/org"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "cogunit":
		err = cmdCogUnit(os.Args[2:])
	case "org-genesis":
		err = cmdOrgGenesis(os.Args[2:])
	case "org-credential":
		err = cmdOrgCredential(os.Args[2:])
	case "aid":
		err = cmdAID(os.Args[2:])
	case "x402-authorize":
		err = cmdX402Authorize(os.Args[2:])
	case "relay-sign":
		err = cmdRelaySign(os.Args[2:])
	case "seal":
		err = cmdSeal(os.Args[2:])
	case "relay-send":
		err = cmdRelaySend(os.Args[2:])
	case "org-id":
		err = cmdOrgID(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "anetfixture:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"usage: anetfixture cogunit|org-genesis|org-credential|aid|x402-authorize|relay-sign|seal|relay-send|org-id --home DIR [...]")
	os.Exit(2)
}

// load restores the controller a daemon runs as. The fixture signs AS that
// daemon rather than as a throwaway key: an object signed by a key nobody
// can resolve is rejected for the right reason and proves nothing.
func load(home string) (*identity.Controller, error) {
	if home == "" {
		return nil, fmt.Errorf("--home is required (the daemon data dir, e.g. ~/.anet)")
	}
	path := home
	if fi, err := os.Stat(filepath.Join(home, "identity.kel")); err == nil && !fi.IsDir() {
		path = filepath.Join(home, "identity.kel")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return identity.Restore(b)
}

func cmdAID(args []string) error {
	fs := flag.NewFlagSet("aid", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	fmt.Println(c.AID())
	return nil
}

func cmdCogUnit(args []string) error {
	fs := flag.NewFlagSet("cogunit", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir")
	task := fs.String("task", "", "task id this unit belongs to")
	typ := fs.String("type", "claim", "claim/evidence/conclusion/intent/retraction")
	body := fs.String("body", "", "inline payload")
	bodyCID := fs.String("body-cid", "", "CAS ref for a large payload (signed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	u := &blackboard.CogUnit{
		TaskID:  *task,
		Type:    *typ,
		Stamp:   blackboard.NewClock(c.AID()).Now(),
		Body:    []byte(*body),
		BodyCID: *bodyCID,
	}
	if err := u.Sign(c); err != nil {
		return err
	}
	b, err := u.Marshal()
	if err != nil {
		return err
	}
	id, err := u.ID()
	if err != nil {
		return err
	}
	// The id goes to stderr so stdout stays a single pipeable line, and a
	// caller can still assert the board stored the unit it was handed.
	fmt.Fprintln(os.Stderr, "unit id:", id)
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

func cmdOrgGenesis(args []string) error {
	fs := flag.NewFlagSet("org-genesis", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir (its AID becomes the sole founder)")
	nonce := fs.String("nonce", "", "uniqueness nonce: same founders, distinct org")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	g := &org.Genesis{GovernanceRoot: []string{c.AID()}, M: 1, Nonce: *nonce}
	if err := g.Validate(); err != nil {
		return err
	}
	// The genesis travels as its canonical preimage: the org id IS the hash
	// of these bytes, so shipping any other encoding would ship an object
	// that does not hash to the org it claims to be.
	b, err := g.CanonicalPreimage()
	if err != nil {
		return err
	}
	id, err := g.OrgID()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "org id:", id)
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

func cmdOrgCredential(args []string) error {
	fs := flag.NewFlagSet("org-credential", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir (its AID issues, so it must be a founder or admin)")
	genesis := fs.String("genesis", "", "base64 genesis from org-genesis")
	subject := fs.String("subject", "", "AID being granted membership")
	role := fs.String("role", org.RoleMember, "admin/member/guest")
	ttl := fs.Duration("ttl", time.Hour, "validity window")
	skew := fs.Duration("issued-ago", 0, "backdate issuance (for testing the window)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(*genesis)
	if err != nil {
		return fmt.Errorf("--genesis not base64: %w", err)
	}
	var g org.Genesis
	if err := coredet.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("--genesis malformed: %w", err)
	}
	orgID, err := g.OrgID()
	if err != nil {
		return err
	}
	if *subject == "" {
		*subject = c.AID()
	}
	now := time.Now().Add(-*skew).UnixMilli()
	cred := &org.Credential{
		OrgID:    orgID,
		Subject:  *subject,
		Role:     *role,
		IssuedAt: now,
		NotAfter: now + ttl.Milliseconds(),
	}
	if err := cred.Sign(c); err != nil {
		return err
	}
	b, err := org.MarshalCredential(cred)
	if err != nil {
		return err
	}
	cid, err := cred.CID()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "credential cid:", cid)
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

// cmdX402Authorize signs an x402 payment the way an HTTP client would.
//
// The gateway's buyer is not necessarily a daemon — it is whoever holds a
// key and can make an HTTP request — so the joint run needs to be able to
// pay from outside the daemon. Doing it here, with the daemon's own
// identity, means the hub resolves the payer's key history exactly as it
// would for any stranger, and a signature the hub cannot check fails for
// the right reason.
//
// Prints one base64 line: the PAYMENT-SIGNATURE header value.
func cmdX402Authorize(args []string) error {
	fs := flag.NewFlagSet("x402-authorize", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir")
	payTo := fs.String("pay-to", "", "payee AID")
	amount := fs.Uint64("amount", 0, "credits")
	network := fs.String("network", "", "ledger, e.g. hub:<aid>")
	interaction := fs.String("interaction", "", "what this pays for")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *payTo == "" || *amount == 0 || *network == "" {
		return fmt.Errorf("--pay-to, --amount and --network are required")
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	now := time.Now()
	auth := &payment.Authorization{
		PayTo: *payTo, Amount: *amount, Network: *network,
		Nonce:    fmt.Sprintf("fixture-%d", now.UnixNano()),
		IssuedAt: now.UnixMilli(), NotAfter: now.Add(5 * time.Minute).UnixMilli(),
		InteractionID: *interaction,
	}
	if err := auth.Sign(c); err != nil {
		return err
	}
	raw, err := auth.Marshal()
	if err != nil {
		return err
	}
	pp := payment.PaymentPayload{
		X402Version: payment.Version,
		Accepted: payment.PaymentOption{
			Scheme: payment.SchemeCredit, Network: *network,
			Amount: payment.Amount(*amount), Asset: payment.AssetCredit, PayTo: *payTo,
		},
		Payload: map[string]any{"authorization": base64.StdEncoding.EncodeToString(raw)},
	}
	b, err := json.Marshal(pp)
	if err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

// cmdRelaySign signs one hub request as this daemon, with relayauth v2
// (A2A-DESIGN §3.7) unless --v1 is given.
//
// The hub gates every mutating action behind a signature, and several of
// those actions have no client in this suite. v2 binds the signature to one
// request on one hub: the action, the signer, the hub's AID, the time, and
// a hash of the method, the request target and the exact body bytes. So the
// request is named here in full, and the output is the headers that carry
// the signature, one "Name: value" line each, ready for `curl -H @FILE`
// with the body sent as `--data-binary @FILE` of the same file:
//
//	anetfixture relay-sign --home H --hub URL --action send \
//	    --method POST --path /relay/send --body-file req.json > hdr
//	curl -H @hdr -H 'Content-Type: application/json' --data-binary @req.json URL/relay/send
//
// --v1 prints the wire-1 challenge (aid, ts, key_state_seq, sig) as JSON to
// merge into a request body. The hub's taskboard, an additive build tag,
// still authenticates that way; nothing on the hub's own wire-2 endpoints
// accepts it.
//
// A fixture rather than an `anet` subcommand because that is what this
// is for: a check that needs a signature, not a product surface nobody
// asked for.
func cmdRelaySign(args []string) error {
	fs := flag.NewFlagSet("relay-sign", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir")
	action := fs.String("action", "", "action name: send, poll, ack, keys, … (v1: e.g. task.create)")
	hub := fs.String("hub", "", "hub URL; its AID is part of what v2 signs")
	hubAID := fs.String("hub-aid", "", "the hub's AID, instead of asking --hub for it")
	method := fs.String("method", http.MethodPost, "HTTP method as sent")
	path := fs.String("path", "", "request target as sent, path and query (e.g. /relay/send)")
	body := fs.String("body", "", "request body, exactly as sent")
	bodyFile := fs.String("body-file", "", "read the request body from this file (the bytes curl will send)")
	v1 := fs.Bool("v1", false, "wire-1 challenge JSON for the hub taskboard (additive tag)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *action == "" {
		return fmt.Errorf("-action is required")
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	if *v1 {
		ts := uint64(time.Now().UnixMilli())
		sig, seq := c.Sign(relayauth.Preimage(*action, c.AID(), ts))
		out, err := json.Marshal(map[string]any{
			"aid": c.AID(), "ts": ts, "key_state_seq": seq,
			"sig": base64.StdEncoding.EncodeToString(sig),
		})
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	if *path == "" || !strings.HasPrefix(*path, "/") {
		return fmt.Errorf("--path is required and starts with /: v2 signs the request target (the wire-1 form is --v1)")
	}
	if *body != "" && *bodyFile != "" {
		return fmt.Errorf("--body or --body-file, not both")
	}
	raw := []byte(*body)
	if *bodyFile != "" {
		if raw, err = os.ReadFile(*bodyFile); err != nil {
			return err
		}
	}
	if *hubAID == "" {
		if *hub == "" {
			return fmt.Errorf("--hub or --hub-aid is required: v2 signs the hub's AID")
		}
		if *hubAID, err = hubAIDOf(*hub); err != nil {
			return err
		}
	}
	for _, h := range v2Headers(c, *hubAID, *action, strings.ToUpper(*method), *path, raw) {
		fmt.Printf("%s: %s\n", h[0], h[1])
	}
	return nil
}

// cmdOrgID derives an org's id from its genesis.
//
// The id is the hash of the genesis preimage, so anyone holding the
// genesis can compute it — which is exactly why the id is confidential
// and the genesis is not something to publish either. Needed by a check
// that has to know the value a node must never disclose in order to
// verify that it does not.
func cmdOrgID(args []string) error {
	fs := flag.NewFlagSet("org-id", flag.ExitOnError)
	genesis := fs.String("genesis", "", "base64 genesis")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *genesis == "" {
		return fmt.Errorf("--genesis is required")
	}
	raw, err := base64.StdEncoding.DecodeString(*genesis)
	if err != nil {
		return fmt.Errorf("--genesis not base64: %w", err)
	}
	var g org.Genesis
	if err := coredet.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("--genesis malformed: %w", err)
	}
	id, err := g.OrgID()
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

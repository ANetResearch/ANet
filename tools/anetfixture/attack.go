package main

// attack.go is the sending side of the joint run's attack sections
// (A2A-DESIGN SI-4, SI-10, §17):
//
//	anetfixture seal       --home DIR --hub URL --to AID [--type message|delegate] [--ix ID] [--text T]
//	                       [--as AID] [--kel self|claimed] [--no-msg-id]
//	anetfixture relay-send --to AID --envelope B64|@FILE [--home DIR --hub URL] [--p2p ADDR]
//
// seal builds a sealed envelope exactly as a daemon would (seal.Seal, the
// recipient's key set from the hub, verified), except that it lets the
// caller claim someone else's AID as the inner sender. That is the attack
// the hub cannot see: relay v2 authenticates who POSTed the envelope, not
// who the sealed inner message says it is from, so only the receiving
// daemon's steps 6 and 7 stand between a registered stranger and a message
// that claims to come from an allowed peer.
//
// relay-send delivers envelope bytes, sealed by anyone, on the paths a
// daemon uses: POST /relay/send signed with relayauth v2 as --home, and the
// peer wire of an anetpeer process. Given both it hands the same bytes to
// both at the same moment, which is SI-10's "one envelope over p2p and the
// hub is processed once". An envelope captured earlier (anetpeer --tee-dir)
// is a replay; the hub has no way to refuse it and is not asked to.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// fixtureLifetimeMS is exp - ts of what seal builds. Shorter than a
// daemon's 14 days: a joint run replays within minutes, and a test envelope
// that stays deliverable for two weeks outlives the run that made it.
const fixtureLifetimeMS = 24 * 3600 * 1000

// httpc bounds every hub call the fixture makes.
var httpc = &http.Client{Timeout: 20 * time.Second}

// hubGetJSON GETs hub+path and decodes a 200 JSON answer into out.
func hubGetJSON(hub, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(hub, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}

// hubAIDOf asks a hub for its AID, which relay v2 signs over.
func hubAIDOf(hub string) (string, error) {
	var id hubapi.HubIdentity
	if err := hubGetJSON(hub, "/hub/identity", &id); err != nil {
		return "", fmt.Errorf("learn the hub's identity: %w", err)
	}
	if id.AID == "" {
		return "", fmt.Errorf("hub %s served no AID", hub)
	}
	return id.AID, nil
}

// published is an AID's KEL and key set as its hub serves them.
type published struct {
	kel     []identity.SignedEvent
	kelRaw  []byte
	keysRaw []byte
	set     *seal.EncKeySet
}

// fetchPublished reads aid's KEL and signed key set from the hub and checks
// them the way a sending daemon does (§3.5 step 1): the KEL replays to aid
// and the key set is signed under it and names aid. An attack that failed
// because the hub served the wrong keys would fail for the wrong reason.
func fetchPublished(hub, aid string, now uint64) (*published, error) {
	var kr hubapi.KeysResponse
	if err := hubGetJSON(hub, "/agents/"+url.PathEscape(aid)+"/keys", &kr); err != nil {
		return nil, fmt.Errorf("keys of %s: %w", aid, err)
	}
	keysRaw, err1 := base64.StdEncoding.DecodeString(kr.KeySet)
	kelRaw, err2 := base64.StdEncoding.DecodeString(kr.KEL)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("hub served unreadable keys for %s", aid)
	}
	kel, err := seal.ParseKEL(kelRaw)
	if err != nil {
		return nil, fmt.Errorf("KEL of %s: %w", aid, err)
	}
	signed, err := seal.UnmarshalSignedEncKeySet(keysRaw)
	if err != nil {
		return nil, fmt.Errorf("key set of %s: %w", aid, err)
	}
	set, err := seal.VerifyEncKeySet(signed, aid, kel, now)
	if err != nil {
		return nil, fmt.Errorf("key set of %s does not verify: %w", aid, err)
	}
	return &published{kel: kel, kelRaw: kelRaw, keysRaw: keysRaw, set: set}, nil
}

// randomID is prefix + 32 hex characters.
func randomID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// cmdSeal prints one sealed envelope, base64, on stdout; the interaction id
// and the message id go to stderr ("ix: …", "mid: …").
//
// --as claims another inner sender. The envelope is still signed with
// --home's key — the attacker has no other — and --kel says which KEL the
// inner carries: "self" (the attacker's own, so step 6 finds that it
// replays to another AID: from-mismatch) or "claimed" (the claimed
// sender's, fetched from the hub, so step 6 passes and step 7 finds a
// signature the claimed key did not make: bad-sig). The attached key set
// is the claimed sender's in both cases; it is advisory (step 8) and
// checked only after the signature.
func cmdSeal(args []string) error {
	fs := flag.NewFlagSet("seal", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir of the signer")
	hub := fs.String("hub", "", "hub URL the recipient's (and a claimed sender's) keys are read from")
	to := fs.String("to", "", "recipient AID")
	typ := fs.String("type", "message", "message (anet.message/1, a ChatMsg) or delegate (anet.delegate/1, a goal)")
	ix := fs.String("ix", "", "interaction id; required for a message, fresh for a delegate when empty")
	text := fs.String("text", "", "message body, or the delegate's goal")
	as := fs.String("as", "", "inner sender to claim (default: the signer itself)")
	kelOf := fs.String("kel", "self", "inner KEL: self (the signer's) or claimed (the claimed sender's, from the hub)")
	noMsgID := fs.Bool("no-msg-id", false,
		"leave ChatMsg.MsgID empty, so only the envelope's replay record (from, mid) tells two copies apart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hub == "" || *to == "" {
		return fmt.Errorf("--hub and --to are required")
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	from := c.AID()
	if *as != "" {
		from = *as
	}
	now := uint64(time.Now().UnixMilli())

	var body []byte
	var innerType string
	switch *typ {
	case "message", seal.TypeMessage:
		innerType = seal.TypeMessage
		if *ix == "" {
			return fmt.Errorf("--ix is required for a message: it names the interaction it belongs to")
		}
		cm := &delegation.ChatMsg{Kind: delegation.ChatText, Body: *text}
		if !*noMsgID {
			cm.MsgID = randomID("msg_")
		}
		if body, err = cm.Marshal(); err != nil {
			return err
		}
	case "delegate", seal.TypeDelegate:
		innerType = seal.TypeDelegate
		if *ix == "" {
			*ix = randomID("ix_")
		}
		goal := *text
		if goal == "" {
			goal = "fixture task"
		}
		// Signed by the signer, whatever --as claims: nobody but the
		// claimed sender can sign as the claimed sender, which is the
		// point of the exercise.
		td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1},
			Tasks: []tsir.Task{{Intent: tsir.Intent{Summary: goal, Body: goal}}}}
		if err := td.Sign(c); err != nil {
			return err
		}
		doc, err := coredet.Marshal(td)
		if err != nil {
			return err
		}
		if body, err = (&delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: *ix}).Marshal(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("--type %q: want message or delegate", *typ)
	}

	recipient, err := fetchPublished(*hub, *to, now)
	if err != nil {
		return err
	}
	key, err := seal.SelectKey(recipient.set, now)
	if err != nil {
		return fmt.Errorf("no usable key for %s: %w", *to, err)
	}
	sender, err := fetchPublished(*hub, from, now)
	if err != nil {
		return err
	}

	var kel []byte
	var seq uint64
	switch *kelOf {
	case "self":
		if kel, err = identity.MarshalKEL(c.KEL()); err != nil {
			return err
		}
		seq = c.CurrentSeq()
	case "claimed":
		if from == c.AID() {
			return fmt.Errorf("--kel claimed needs --as naming another AID")
		}
		kel = sender.kelRaw
		seq = sender.kel[len(sender.kel)-1].Event.Seq
	default:
		return fmt.Errorf("--kel %q: want self or claimed", *kelOf)
	}
	// The declared key state is part of what is signed, and Seal checks
	// the signer reports the seq the inner declares. Under a claimed KEL
	// the declared seq is the claimed sender's, so the receiver checks the
	// signature against that sender's current key and finds it is not.
	sign := func(pre []byte) ([]byte, uint64) {
		sig, _ := c.Sign(pre)
		return sig, seq
	}
	inner := &seal.SealedInner{From: from, KeyStateSeq: seq, To: *to, Type: innerType, IX: *ix,
		MID: seal.NewMID(), TS: now, Exp: now + fixtureLifetimeMS, Body: body, KEL: kel, Keys: sender.keysRaw}
	env, err := seal.Seal(inner, key, sign)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ix: %s\nmid: %s\n", *ix, hex.EncodeToString(inner.MID))
	fmt.Println(base64.StdEncoding.EncodeToString(env))
	return nil
}

// readEnvelope takes the --envelope value: base64, or @FILE holding it.
func readEnvelope(v string) ([]byte, error) {
	if strings.HasPrefix(v, "@") {
		b, err := os.ReadFile(v[1:])
		if err != nil {
			return nil, err
		}
		v = string(b)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, fmt.Errorf("--envelope is empty")
	}
	env, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("--envelope is not standard base64: %w", err)
	}
	return env, nil
}

// v2Headers returns the relayauth v2 headers for one request (A2A-DESIGN
// §3.7), in a fixed order: the signature covers the action, the signer, the
// hub's AID, the time, and a hash of the method, the request target as sent
// and the exact body bytes. The wire version header rides along because a
// wire-2 hub answers 426 to a /relay/* request without it.
func v2Headers(c *identity.Controller, hubAID, action, method, requestURI string, body []byte) [][2]string {
	ts := uint64(time.Now().UnixMilli())
	sig, seq := c.Sign(relayauth.PreimageV2(action, c.AID(), hubAID, ts, method, requestURI, body))
	return [][2]string{
		{relayauth.HeaderAID, c.AID()},
		{relayauth.HeaderTS, strconv.FormatUint(ts, 10)},
		{relayauth.HeaderSeq, strconv.FormatUint(seq, 10)},
		{relayauth.HeaderSig, relayauth.EncodeSig(sig)},
		{hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion)},
	}
}

// hubOutcome is what the hub answered to one /relay/send.
type hubOutcome struct {
	Code   int    `json:"code"`
	Status string `json:"status,omitempty"`
	ID     int64  `json:"id,omitempty"`
	Error  string `json:"error,omitempty"`
}

// p2pOutcome is what the receiving anetpeer answered to one delivery: OK
// once its daemon acknowledged the envelope.
type p2pOutcome struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// postRelay sends one signed POST /relay/send.
func postRelay(c *identity.Controller, hub, hubAID, to string, env []byte) *hubOutcome {
	body, err := json.Marshal(hubapi.RelaySendRequest{ToAID: to, Envelope: base64.StdEncoding.EncodeToString(env)})
	if err != nil {
		return &hubOutcome{Error: err.Error()}
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(hub, "/")+"/relay/send", bytes.NewReader(body))
	if err != nil {
		return &hubOutcome{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range v2Headers(c, hubAID, relayauth.ActionSend, req.Method, req.URL.RequestURI(), body) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return &hubOutcome{Error: err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := &hubOutcome{Code: resp.StatusCode}
	var ans struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(b, &ans)
	out.ID, out.Status, out.Error = ans.ID, ans.Status, ans.Error
	if resp.StatusCode != http.StatusOK && out.Error == "" {
		out.Error = strings.TrimSpace(string(b))
	}
	return out
}

// peerFrame is the anetpeer peer-wire frame (tools/anetpeer), the fields a
// delivery uses.
type peerFrame struct {
	Op       string `json:"op"`
	V        int    `json:"v,omitempty"`
	ID       string `json:"id,omitempty"`
	To       string `json:"to,omitempty"`
	Envelope string `json:"envelope,omitempty"`
	Error    string `json:"error,omitempty"`
}

// peerWireVersion is the frame version anetpeer accepts.
const peerWireVersion = 2

// peerAddr splits an anetpeer address the way anetpeer does: tcp://host:port
// or host:port is TCP, unix://path or anything with a slash a Unix socket.
func peerAddr(a string) (network, addr string) {
	switch {
	case strings.HasPrefix(a, "tcp://"):
		return "tcp", strings.TrimPrefix(a, "tcp://")
	case strings.HasPrefix(a, "unix://"):
		return "unix", strings.TrimPrefix(a, "unix://")
	case strings.Contains(a, "/"):
		return "unix", a
	case strings.Contains(a, ":"):
		return "tcp", a
	default:
		return "unix", a
	}
}

// deliverP2P hands the envelope to the anetpeer listening at addr, as
// another peer would, and waits for its answer. That process hands it to
// its daemon and answers only once the daemon acknowledged it (or refused
// it for good), so OK means the receiving pipeline has decided.
func deliverP2P(addr, to string, env []byte) *p2pOutcome {
	network, a := peerAddr(addr)
	c, err := net.DialTimeout(network, a, 8*time.Second)
	if err != nil {
		return &p2pOutcome{Error: err.Error()}
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	f := peerFrame{Op: "send", V: peerWireVersion, ID: randomID("fixture-"), To: to,
		Envelope: base64.StdEncoding.EncodeToString(env)}
	if err := json.NewEncoder(c).Encode(f); err != nil {
		return &p2pOutcome{Error: err.Error()}
	}
	var reply peerFrame
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return &p2pOutcome{Error: err.Error()}
	}
	if reply.Error != "" {
		return &p2pOutcome{Error: reply.Error}
	}
	return &p2pOutcome{OK: true}
}

// errPathFailed makes the exit status say a path did not take the envelope;
// the JSON on stdout says which and why.
var errPathFailed = errors.New("a delivery path did not take the envelope (see the JSON on stdout)")

// cmdRelaySend delivers one envelope and prints what each path answered as
// one JSON line: {"hub":{"code":200,"status":"queued","id":7},"p2p":{"ok":true}}.
//
// With both --hub and --p2p the two deliveries are prepared first and then
// released together, so the receiving daemon sees the same bytes from two
// transports as close to the same moment as two processes can arrange.
// The hub copy still waits in the mailbox until the next poll; the exact
// interleaving is covered by the daemon's unit tests, this proves the
// deployed binaries agree.
func cmdRelaySend(args []string) error {
	fs := flag.NewFlagSet("relay-send", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir of the hub sender (relay v2 signer; must be registered at --hub)")
	hub := fs.String("hub", "", "hub URL: POST /relay/send there")
	p2p := fs.String("p2p", "", "anetpeer peer address (socket path or tcp://host:port): deliver there too")
	to := fs.String("to", "", "recipient AID")
	envArg := fs.String("envelope", "", "the envelope, standard base64, or @FILE holding it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("--to is required")
	}
	if *hub == "" && *p2p == "" {
		return fmt.Errorf("--hub, --p2p or both: there is no path to deliver on")
	}
	env, err := readEnvelope(*envArg)
	if err != nil {
		return err
	}
	var c *identity.Controller
	var hubAID string
	if *hub != "" {
		if c, err = load(*home); err != nil {
			return err
		}
		if hubAID, err = hubAIDOf(*hub); err != nil {
			return err
		}
	}

	var out struct {
		Hub *hubOutcome `json:"hub,omitempty"`
		P2P *p2pOutcome `json:"p2p,omitempty"`
	}
	release := make(chan struct{})
	var wg sync.WaitGroup
	if *hub != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			out.Hub = postRelay(c, *hub, hubAID, *to, env)
		}()
	}
	if *p2p != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			out.P2P = deliverP2P(*p2p, *to, env)
		}()
	}
	close(release)
	wg.Wait()

	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	if (out.Hub != nil && out.Hub.Code != http.StatusOK) || (out.P2P != nil && !out.P2P.OK) {
		return errPathFailed
	}
	return nil
}

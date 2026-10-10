package daemon

// hubfake_test.go is an in-memory stand-in for the Hub service (a separate repository; only its wire
// types live in internal/hubapi). The daemon tests run against this fake instead of a real Hub. It
// implements the endpoints the daemon's HTTP client calls, with wire shapes identical to the real Hub
// at wire 3 (core §2.12, CORE-V1), including every refusal path of the relay:
//
//   - X-ANet-Wire: 3 on every answer; 426 wire_too_old (with required_wire and required_anet) to a
//     write, or any /relay/* request, that states an older wire or none, 400 wire_too_new to one that
//     states a newer one;
//   - relayauth v2 on every signed endpoint: the X-ANet-* headers are verified against the caller's
//     registered KEL (the body KEL for /register), with the hub AID, method, request target and raw
//     body bound, a ±5 minute window and a replay cache — 401 on any failure;
//   - POST /relay/session (relayauth session): a session id and a session key sealed to the caller's
//     one-time key (core L7), 403 admission_required for an AID not registered here;
//   - POST /relay/send v3: the envelope as the body, X-ANet-Access byte for byte its outer key 7, no
//     identity header (400 identity_headers_not_allowed); a first contact under a session MAC (401
//     session_invalid), a send under a capability checked with seal.VerifyAccess against the owner's
//     mailbox key (403 access_invalid); 400 bad_envelope, 404 recipient_unknown for a first contact to
//     an AID not registered here, 413 over the envelope cap, 429 rate_limited (with Retry-After) over
//     the per-sender budget, 507 over the per-recipient mailbox cap; the same envelope still queued is
//     a 200 duplicate under its first rid;
//   - GET/POST /agents/{aid}/keys for the agents registered here: keys in base64url with prev_seq,
//     the publisher rule of FS-H4 (the stored set again is unchanged; a set that does not follow the
//     stored one, by prev_seq or seq, is 409 keys_rollback with stored_seq), SSR-W9 refused with 400
//     bad_request; /register applies the same rule to enc_keys before it writes anything;
//   - every answer with a Date header (net/http's), or the one dateFn gives;
//   - POST /directory:bucket (sealed-sender §2.9, SSR-W17 to SSR-W21): every agent registered here with a
//     key set has an entry sealed under its AID (dirbox), filed under its directory hash; a query names a
//     prefix of at most dir_max_bits bits (400 bucket_too_narrow with max_bits above), and is answered with
//     the entries of the bucket, this hub's and its peer fakes', paged, partial when a peer is down;
//   - GET /hub/identity, with dir_max_bits;
//   - GET /hub/kels (KEL H-16): this hub's key history, its peer fakes' and the ones a test sets.
//
// Relay messages are OPAQUE: the fake stores the envelope bytes keyed by to_aid and nothing else. It
// applies seal.ParseOuter, the structural check the real hub applies, and never opens a payload — a
// fake that read payloads could make a test pass that the real hub would fail.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	evidence "github.com/ANetResearch/ANet/internal/evidencev1" // v030-todo(PAY-14): evidence v2
	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/regpow"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// fakeHubAgent is one registered agent's row in the fake registry.
type fakeHubAgent struct {
	view hubapi.AgentView
	kel  []byte
	// keyset is the agent's SignedEncKeySet encoding, as last accepted.
	keyset []byte
}

// fakeHubMsg is one queued relay message: the real hub's relay_message row
// at wire 2 (id, to_aid, payload, size, created_at). No sender, no kind, no
// interaction id.
type fakeHubMsg struct {
	id        int64
	toAID     string
	payload   []byte
	createdAt string
	// rid and digest are the wire-3 row's: the id the sender was told, and
	// the env_digest a duplicate is recognised by.
	rid    []byte
	digest [32]byte
}

// fakeSession is one open first-contact session.
type fakeSession struct {
	aid string
	key []byte
	exp time.Time
}

// fakeHubRequest is one request the fake got, as a test inspects it: the
// connection it came on (remote), and whether that connection resumed a
// TLS session.
type fakeHubRequest struct {
	method, path string
	header       http.Header
	remote       string
	resumed      bool
	// query is the raw query; body the request body, when it is at most
	// fakeHubKeepBody bytes (bodyLen is its length either way).
	query   string
	body    []byte
	bodyLen int
}

// fakeHubKeepBody bounds the request bodies the fake keeps for a test.
const fakeHubKeepBody = 1 << 20

// fakeHub is the in-memory Hub: registry + relay mailboxes + verified-review store
// + the credit ledger it is custodian of.
type fakeHub struct {
	mu      sync.Mutex
	nextID  int64
	agents  map[string]*fakeHubAgent
	mailbox []*fakeHubMsg
	reviews map[string]hubapi.ReviewView // interaction_id → stored review (one per interaction)
	// The credit side. Balances are the hub's, which is the whole point of
	// the custody note in pay.go — the fake holds them the same way.
	self    *identity.Controller
	balance map[string]uint64
	entries map[string][]map[string]any
	settled map[string]string // authorization id → transaction, so replay is idempotent
	// settledReceipt is each settled authorization's signed receipt, so a
	// replay answers with the original (A2A-DESIGN §8.5).
	settledReceipt map[string]string
	// bindings maps payer + binding to the authorization that settled it:
	// one settlement per (payer, interaction_id) (§8.5).
	bindings map[string]string
	// redemptions is each account's withdrawals, oldest first, as the real
	// hub's credit_redemption rows (GET /agents/{aid}/redemptions).
	redemptions map[string][]map[string]any
	// settleBodies are the raw /x402/settle request bodies, in order, so a
	// test can check what the hub was told (SI-1).
	settleBodies [][]byte
	// settleFaults are applied to the next /x402/settle calls, one each:
	// "pending" answers settlement_pending without settling, "settle-then-pending"
	// settles and then answers settlement_pending (the hub committed after
	// the caller stopped listening), "drop" closes the connection before
	// settling, "settle-then-drop" settles and then closes it,
	// "foreign-receipt" settles nothing and answers success, flagged as a
	// replay, with the receipt of an authorization settled earlier.
	settleFaults []string
	// departedKEL keeps a deregistered agent's key history, so a receipt it
	// signed before leaving can still be verified.
	departedKEL map[string][]byte
	// relaySends counts deliveries the hub actually carried, so a test can
	// tell "the hub delivered it" from "something else did".
	relaySends int
	// relayPolls counts the /relay/poll requests each AID made, so a test
	// can tell how many requests a poll round costs the hub.
	relayPolls map[string]int
	// relayPollTries counts every /relay/poll request by the AID it is
	// signed as, before the signature is checked: a poll of an AID the hub
	// does not know is refused and never reaches relayPolls.
	relayPollTries map[string]int
	// relayServed is, per AID, the ids of the envelopes each poll handed
	// out, in order: which envelopes a node read, not only how often it
	// asked.
	relayServed map[string][]int64
	// lastSeen is when each agent last collected its mail — the real hub's
	// agent.last_seen_at, updated by register and by every poll. It is what
	// /relay/send answers recipient_quiet from, so a fake without it makes
	// the whole liveness contract invisible on this side.
	lastSeen map[string]time.Time

	// wire is the X-ANet-Wire version this hub states (default 2). Setting
	// it to 1 makes the fake answer like a pre-wire-2 hub.
	wire int
	// Relay limits. Zero means the default (envelope cap) or no limit.
	maxEnvelope int // 413 above this many envelope bytes
	senderLimit int // 429 once a sender has sent this many envelopes
	mailboxCap  int // 507 once a recipient holds this many undelivered envelopes
	// pollPage answers at most this many envelopes per poll (0: the
	// caller's limit): the real hub's byte budget (PollBudget) cuts a page
	// of large envelopes short, down to one.
	pollPage int
	// pollAll answers a poll with every envelope after the cursor,
	// whatever the limit asked: a round larger than one ack request takes.
	pollAll bool
	// ackRequests is the items of every /relay/ack request, in order;
	// ackFail answers the request of that index (from 1) 503 and deletes
	// nothing.
	ackRequests [][]hubapi.RelayAckItem
	ackFail     int
	// relayDacks is the delivery tokens acks left, by handle (DLV-H-3:
	// written only for a row the ack deleted).
	relayDacks map[string]string
	// relayDown makes /relay/send answer 503: the hub is up but not
	// carrying mail, the temporary failure a sender retries.
	relayDown bool
	// dropSends makes /relay/send answer 200 queued with a rid and keep
	// nothing: a hub that loses what it took (DLV T4).
	dropSends bool
	// sendHook, when set, is asked about every /relay/send whose envelope
	// and credential passed, before the hub queues it, and its answer is
	// what the hub does (a scripted hub: the engine's model test).
	sendHook func(env []byte) fakeSendAnswer
	// statusRequests are the items of every /relay/status request, in
	// order, and statusHeaders their headers; statusDown answers them 503;
	// statusHook, when set, answers an item instead of the mailbox and the
	// tokens (ok false: the usual answer): a hub that lies (E-T24).
	statusRequests [][]hubapi.RelayStatusItem
	statusHeaders  []http.Header
	statusDown     bool
	statusHook     func(it hubapi.RelayStatusItem) (hubapi.RelayStatusResult, bool)
	// identityDown makes GET /hub/identity answer 503: the hub is up and
	// carrying mail, and a node that has not learned its identity yet
	// cannot learn it now.
	identityDown bool
	// retryAfter is the Retry-After the 429 answer carries ("" is "1").
	retryAfter string
	sendsBy    map[string]int
	// sigSeen is the relayauth replay cache: (aid, signature) pairs seen
	// inside the skew window.
	sigSeen map[string]bool
	// keysGets counts GET /agents/{aid}/keys.
	keysGets int
	// keysOverride makes GET /agents/{aid}/keys answer with someone else's
	// material: a hostile hub substituting keys.
	keysOverride map[string]hubapi.KeysResponse
	// The directory (bucket.go of the real hub). dirMaxBits is the
	// dir_max_bits this hub states and enforces (default 0: the whole
	// directory, as the real hub's below 64 entries); dirAnswerMaxBits, when
	// set, is what a bucket answer states instead (a hub that says one thing
	// and does another). dirPage answers at most that many entries a page.
	// dirDown answers 503; dirPartial marks every answer partial; dirPad adds
	// that many entries of nobody to every answer. dirOverride replaces an
	// agent's entry (a hostile hub substituting an entry), dirExtra files
	// more entries under an agent's hash, dirHide leaves an agent out.
	// bucketQueries is every query, in order.
	dirMaxBits       int
	dirAnswerMaxBits *int
	dirPage          int
	dirDown          bool
	dirPartial       bool
	dirPad           int
	dirOverride      map[string]dirbox.DirEntry
	dirExtra         map[string][]dirbox.DirEntry
	dirHide          map[string]bool
	dirRaw           []hubapi.DirBucketEntry
	bucketQueries    []hubapi.DirBucketRequest
	// homeMustBe makes a first contact to an AID answer 404
	// recipient_unknown unless its X-ANet-Home names the hub given: a hub
	// that finds the recipient only at the home the hint names.
	homeMustBe map[string]string
	// refuseKeySets makes the hub refuse every key set it is sent
	// (keys_status invalid at /register, 400 at POST /agents/{aid}/keys):
	// a hub that does not hold this node's mailbox signing key.
	refuseKeySets bool
	// forgedRollback, while not empty, answers each POST /agents/{aid}/keys
	// with 409 keys_rollback naming its first stored_seq, then drops it:
	// a hub that lies about the set it holds (red team A6).
	forgedRollback []uint64
	// keyPosts counts POST /agents/{aid}/keys requests; keyBodies keeps
	// their bodies.
	keyPosts  int
	keyBodies [][]byte
	// dateFn, when set, is the time every answer's Date header states: a
	// hub whose clock is off, or one a test drives.
	dateFn func() time.Time
	// pollMore, when set, is the more every poll answer states: a hub that
	// always says more is queued (red team A1), or never does.
	pollMore *bool
	// whilePolling, when set, runs while the hub answers a poll, before
	// the answer is written: time a test lets pass between the request and
	// the answer.
	whilePolling func()
	// keysAt, when set, is the time (unix ms) key sets are verified at, the
	// hub's clock for them: a test whose nodes live days ahead moves it
	// with them.
	keysAt func() uint64
	// peers are other fake hubs whose buckets this hub's answers merge,
	// standing in for /fed/v3/directory:bucket (SSR-W19); a peer whose
	// dirDown is set makes the answer partial (SSR-W21).
	peers []*fakeHub
	// hubKELs are hub key histories (identity.MarshalKEL) GET /hub/kels
	// lists besides this hub's own and its peer fakes', by AID: the hubs a
	// test makes this one federate with; hubKELReads counts the reads.
	hubKELs     map[string][]byte
	hubKELReads int
	// authFailures counts 401 answers, so a test can tell "refused by the
	// hub" from "never sent".
	authFailures int
	// registerKeys is the keys_status of each agent's last registration.
	registerKeys map[string]string
	// a2aCards holds each agent's admitted A2A network card (raw bytes) and
	// a2aMarks its params.seq high water, the admission hub brief H1
	// describes: a2acard.Verify, then CheckHighWater. registerCards is the
	// card_status of each agent's last registration, and a2aCardSends
	// counts registrations that carried a card.
	a2aCards      map[string][]byte
	a2aMarks      map[string]a2acard.Mark
	registerCards map[string]string
	a2aCardSends  int
	// a2aWithdrawals counts registrations that carried a2a_card: null.
	a2aWithdrawals int

	// Admission (hRegister): the mode, the AIDs the operator admitted, the
	// bits of open registration's proof of work and its key, and whether
	// today's places of the caller's network are spent.
	admission  string
	admitted   map[string]bool
	powBits    int
	powKey     [32]byte
	quotaSpent bool
	// probation is each open-registration account's end of probation.
	probation map[string]uint64
	// registerScript are answers /register gives, in order, instead of its
	// rules; registerCalls and checkOnlyCalls count the requests, and
	// registerKELs keeps the KEL of every registration (not check_only).
	registerScript []fakeRegisterAnswer
	registerCalls  int
	checkOnlyCalls int
	registerKELs   [][]byte
	// revocations are the withdrawn delivery credentials each owner
	// pushed (POST /mailbox/revoke; cap_id B64 → held), counted in the
	// /register answer (sealed-sender SSR-D21). revokeBatches is the
	// number of ids of every accepted request; revokeFail answers that
	// many requests 503 first; revokeFull answers 429
	// revocation_list_full (revoke_push_test.go).
	revocations   map[string]map[string]bool
	revokeBatches []int
	revokeFail    int
	revokeFull    bool

	// notices makes the hub sign witness notices (KEL H-10, D-50): the
	// /register and /relay/poll answers of an agent whose KEL tip names this
	// hub as its witness carry the notice for that tip, observed at
	// noticeAt() (the hub's clock when nil); any other agent's carry
	// key_state_notice_error not_witness. noticeErr, when set, is answered
	// instead of every notice (quarantine, not_admitted).
	notices   bool
	noticeAt  func() uint64
	noticeErr string
	// noSuiteNotices leaves the directory entries of agents the suite
	// witness witnesses without its notice; by default each carries one
	// (suiteNoticeLocked), as a peer hub passes its notice on, and
	// /hub/kels lists the suite witness's key history.
	noSuiteNotices bool

	// sessions are the open first-contact sessions by session id;
	// sessionLife is the life the next one gets (0: an hour, the real
	// hub's); sessionOpens counts the sessions opened; sessionRefuse, when
	// set, is the reason /relay/session refuses with (403
	// admission_required, 429 rate_limited).
	sessions      map[string]*fakeSession
	sessionLife   time.Duration
	sessionOpens  int
	sessionRefuse string
	// sessionHook, when set, runs after a session is opened and before
	// its answer is written (outside the lock): a test holds the answer
	// in flight there.
	sessionHook func()
	// requests is every request the fake got, in order; setCookie makes
	// every answer set a cookie, as a hub that tracked its callers would.
	requests  []fakeHubRequest
	setCookie bool
	// sendViaHub, when set, is the via_hub of every send's answer: the
	// envelope was queued at that hub (DLV-W-12a).
	sendViaHub string
	// closeConns ends every connection after its answer, so every request
	// comes on a new one.
	closeConns bool

	// registry serves GET /a2a/v1/agents (hubfake_registry_test.go); off,
	// the fake is a hub without the A2A registry.
	registry bool
}

// noticeForLocked is what the hub answers about aid's key state: its
// notice (base64url) or why there is none. Nothing when notices is off.
// The caller holds h.mu.
func (h *fakeHub) noticeForLocked(aid string) (notice, noticeErr string) {
	if !h.notices {
		return "", ""
	}
	if h.noticeErr != "" {
		return "", h.noticeErr
	}
	a, ok := h.agents[aid]
	if !ok {
		return "", "not_admitted"
	}
	kel, err := identity.UnmarshalKEL(a.kel)
	if err != nil {
		return "", ""
	}
	states, err := identity.Replay(kel)
	if err != nil || states[len(states)-1].Witness != h.self.AID() {
		return "", "not_witness"
	}
	at := uint64(time.Now().UnixMilli())
	if h.noticeAt != nil {
		at = h.noticeAt()
	}
	tip := kel[len(kel)-1]
	b, err := identity.SignNotice(h.self, identity.NoticeBody{AID: aid, Seq: tip.Event.Seq, Head: tip.EventID, ObservedAt: at})
	if err != nil {
		return "", ""
	}
	return base64.RawURLEncoding.EncodeToString(b), ""
}

// suiteNoticeLocked is the notice of the suite witness (the conformance
// hub, identitytest.Witness) for the tip of kel (its encoding), observed at
// noticeAt() (the hub's clock when nil), or nil when kel names another
// witness or none: what an entry of an agent the suite witness witnesses
// carries when a peer hub passes it on (KEL §2.7). The caller holds h.mu.
func (h *fakeHub) suiteNoticeLocked(kelBytes []byte) []byte {
	kel, err := identity.UnmarshalKEL(kelBytes)
	if err != nil {
		return nil
	}
	states, err := identity.Replay(kel)
	if err != nil || len(states) == 0 || states[len(states)-1].Witness != identitytest.Witness {
		return nil
	}
	at := uint64(time.Now().UnixMilli())
	if h.noticeAt != nil {
		at = h.noticeAt()
	}
	tip := kel[len(kel)-1]
	n, err := identity.SignNotice(identity.SuiteController(), identity.NoticeBody{AID: states[len(states)-1].AID,
		Seq: tip.Event.Seq, Head: tip.EventID, ObservedAt: at})
	if err != nil {
		return nil
	}
	return n
}

// fakeRegisterResponse is the /register answer with the notice fields.
type fakeRegisterResponse struct {
	hubapi.RegisterResponse
	KeyStateNotice      string `json:"key_state_notice,omitempty"`
	KeyStateNoticeError string `json:"key_state_notice_error,omitempty"`
}

// fakeHubDefaultMaxEnvelope is the real hub's per-envelope cap (96 MiB).
const fakeHubDefaultMaxEnvelope = 96 << 20

// fakeHubQuietAfter mirrors aghub.QuietAfter: how long without collecting
// mail before the hub reports a recipient as quiet.
const fakeHubQuietAfter = time.Hour

// fakeHubRoundDuration renders a gap the way the real hub's roundDuration
// does, so the sentence a test sees is the sentence production produces.
func fakeHubRoundDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}

// newFakeHub starts an httptest server backed by a fresh fake Hub and cleans it up with the test.
func newFakeHub(t testing.TB) *httptest.Server { return startFakeHub(t, false) }

// startFakeHub is newFakeHub, over TLS when tlsOn (httptest's certificate;
// a daemon trusts it after setHubRoots).
func startFakeHub(t testing.TB, tlsOn bool) *httptest.Server {
	t.Helper()
	self, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{
		agents: map[string]*fakeHubAgent{}, reviews: map[string]hubapi.ReviewView{},
		departedKEL: map[string][]byte{},
		self:        self,
		balance:     map[string]uint64{}, entries: map[string][]map[string]any{},
		settled: map[string]string{}, settledReceipt: map[string]string{}, bindings: map[string]string{},
		lastSeen: map[string]time.Time{},
		wire:     hubapi.WireVersion,
		sendsBy:  map[string]int{}, sigSeen: map[string]bool{}, sessions: map[string]*fakeSession{},
		keysOverride: map[string]hubapi.KeysResponse{},
		registerKeys: map[string]string{},
		a2aCards:     map[string][]byte{}, a2aMarks: map[string]a2acard.Mark{},
		registerCards: map[string]string{},
		admitted:      map[string]bool{}, powBits: 4, probation: map[string]uint64{},
		// It witnesses the agents whose key history names it, and every
		// answer about their key state carries its notice (KEL H-10, D-50):
		// a test node is confirmed, and its gate open, once it joined.
		notices: true,
	}
	// The hub is an agent on its own registry, so its settlement
	// signatures can be checked the same way everyone else's are.
	selfKEL, err := identity.MarshalKEL(self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	h.agents[self.AID()] = &fakeHubAgent{
		view: hubapi.AgentView{AID: self.AID(), Name: "hub"}, kel: selfKEL}
	var srv *httptest.Server
	if tlsOn {
		srv = httptest.NewTLSServer(h.handler())
	} else {
		srv = httptest.NewServer(h.handler())
	}
	t.Cleanup(srv.Close)
	hubsByURL.Store(srv.URL, h)
	return srv
}

// fakeHubAt returns the fake behind a server URL.
func fakeHubAt(t testing.TB, url string) *fakeHub {
	t.Helper()
	v, ok := hubsByURL.Load(url)
	if !ok {
		t.Fatal("no fake hub at " + url)
	}
	return v.(*fakeHub)
}

// hubsByURL lets a test reach the fake behind a server URL.
var hubsByURL sync.Map

// relayCount reports how many deliveries this hub carried.
func relayCountFor(url string) int {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relaySends
}

// relayServedFor reports the ids of the envelopes this hub's polls handed
// to aid, in the order it handed them out.
func relayServedFor(url, aid string) []int64 {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return nil
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.relayServed[aid]...)
}

// mailboxIDsFor reports the ids of the envelopes this hub holds for aid.
func mailboxIDsFor(url, aid string) []int64 {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return nil
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []int64
	for _, m := range h.mailbox {
		if m.toAID == aid {
			out = append(out, m.id)
		}
	}
	return out
}

// relayPollsFor reports how many mailbox polls this hub answered for aid.
func relayPollsFor(url, aid string) int {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relayPolls[aid]
}

// relayPollTriesFor reports how many /relay/poll requests signed as aid
// this hub got, answered or refused.
func relayPollTriesFor(url, aid string) int {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relayPollTries[aid]
}

func (h *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", h.hRegister)
	mux.HandleFunc("POST /agents/{aid}/deregister", h.hDeregister)
	mux.HandleFunc("POST /agents/{aid}/visibility", h.hVisibility)
	mux.HandleFunc("POST /profile", h.hProfile)
	mux.HandleFunc("GET /agents", h.hAgents)
	mux.HandleFunc("GET /agents/{aid}", h.hAgent)
	mux.HandleFunc("GET "+hubapi.RegistryAgentsPath, h.hRegistry)
	mux.HandleFunc("POST /reviews", h.hUploadReview)
	mux.HandleFunc("POST "+hubapi.RelaySessionPath, h.hRelaySession)
	mux.HandleFunc("POST /relay/send", h.hRelaySend)
	mux.HandleFunc("POST /relay/poll", h.hRelayPoll)
	mux.HandleFunc("POST /relay/ack", h.hRelayAck)
	mux.HandleFunc("POST "+hubapi.RelayStatusPath, h.hRelayStatus)
	mux.HandleFunc("GET /agents/{aid}/keys", h.hKeysGet)
	mux.HandleFunc("POST /agents/{aid}/keys", h.hKeysPost)
	mux.HandleFunc("POST "+hubapi.DirectoryBucketPath, h.hDirBucket)
	mux.HandleFunc("POST "+hubapi.MailboxRevokePath, h.hMailboxRevoke)
	// The facilitator half of the contract. A fake that serves only the
	// endpoints the happy path happens to touch is how a seam gets shipped
	// unconnected — the real hub answers these, so this one does too.
	mux.HandleFunc("GET /hub/identity", h.hIdentity)
	mux.HandleFunc("GET /hub/kels", h.hHubKELs)
	mux.HandleFunc("GET /agents/{aid}/kel", h.hAgentKEL)
	mux.HandleFunc("POST /x402/settle", h.hSettle)
	mux.HandleFunc("GET /agents/{aid}/balance", h.hBalance)
	mux.HandleFunc("GET /agents/{aid}/ledger", h.hLedgerRead)
	mux.HandleFunc("GET /agents/{aid}/redemptions", h.hRedemptions)
	mux.HandleFunc("POST /x402/redeem", h.hRedeem)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := fakeHubRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), remote: r.RemoteAddr,
			resumed: r.TLS != nil && r.TLS.DidResume, query: r.URL.RawQuery, bodyLen: len(body)}
		if len(body) <= fakeHubKeepBody {
			rec.body = body
		}
		h.mu.Lock()
		wire, cookie := h.wire, h.setCookie
		if h.dateFn != nil {
			w.Header().Set("Date", h.dateFn().UTC().Format(http.TimeFormat))
		}
		h.requests = append(h.requests, rec)
		if h.closeConns {
			w.Header().Set("Connection", "close")
		}
		h.mu.Unlock()
		w.Header().Set(hubapi.WireVersionHeader, strconv.Itoa(wire))
		if cookie {
			http.SetCookie(w, &http.Cookie{Name: "hubtrack", Value: "caller-" + r.URL.Path, Path: "/"})
		}
		relay := strings.HasPrefix(r.URL.Path, "/relay/")
		declared, err := strconv.Atoi(r.Header.Get(hubapi.WireVersionHeader))
		switch {
		case wire >= 3 && err == nil && declared > wire:
			fakeHubRefuse(w, http.StatusBadRequest, "wire_too_new", nil)
			return
		case wire >= 3 && (err != nil || declared < wire) && (relay || r.Method != http.MethodGet && r.Method != http.MethodHead):
			// The wire-3 contract (CORE-V1): every write and the relay
			// plane, refused with what to install.
			fakeHubRefuse(w, http.StatusUpgradeRequired, "wire_too_old",
				map[string]any{"required_wire": wire, "required_anet": fmt.Sprintf("0.%d.0", wire)})
			return
		case wire == 2 && relay && (err != nil || declared < 2):
			// A wire-2 hub refused /relay/* from a daemon that stated no
			// version or an older one.
			fakeHubJSON(w, http.StatusUpgradeRequired, map[string]string{"error": "this hub requires anet >= 0.2.0 (wire 2)"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// readBody reads a request body under a cap, answering 413 over it.
func (h *fakeHub) readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "body unreadable"})
		return nil, false
	}
	if len(b) > limit {
		fakeHubJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
		return nil, false
	}
	return b, true
}

// verifyAuth checks relayauth v2 headers against kel (or, when kel is nil,
// against the registered KEL of the header AID) for action, over the raw
// body the handler read. It answers 401 itself and returns "" on failure.
func (h *fakeHub) verifyAuth(w http.ResponseWriter, r *http.Request, body []byte, action string, kel []byte) string {
	fail := func(why string) string {
		h.mu.Lock()
		h.authFailures++
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: " + why})
		return ""
	}
	aid := r.Header.Get(hubapi.HeaderAID)
	ts, err1 := strconv.ParseUint(r.Header.Get(hubapi.HeaderTS), 10, 64)
	seq, err2 := strconv.ParseUint(r.Header.Get(hubapi.HeaderSeq), 10, 64)
	sigHdr := r.Header.Get(hubapi.HeaderSig)
	sig, err3 := relayauth.DecodeSig(sigHdr)
	if aid == "" || err1 != nil || err2 != nil || err3 != nil {
		return fail("missing or malformed X-ANet-* headers")
	}
	now := uint64(time.Now().UnixMilli())
	if ts+relayauth.MaxSkewMillis < now || ts > now+relayauth.MaxSkewMillis {
		return fail("timestamp outside the window")
	}
	if kel == nil {
		// a.kel is read under the lock: a register rewrites it.
		h.mu.Lock()
		a := h.agents[aid]
		if a != nil {
			kel = a.kel
		}
		h.mu.Unlock()
		if a == nil {
			return fail("unknown agent")
		}
	}
	events, err := identity.UnmarshalKEL(kel)
	if err != nil {
		return fail("KEL undecodable")
	}
	pre := relayauth.PreimageV2(action, aid, h.self.AID(), ts, r.Method, r.URL.RequestURI(), body)
	if err := identity.VerifyObject(events, aid, seq, ts, pre, sig); err != nil {
		return fail("signature: " + err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sigSeen[aid+"|"+sigHdr] {
		h.authFailures++
		fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: replayed signature"})
		return ""
	}
	h.sigSeen[aid+"|"+sigHdr] = true
	return aid
}

func fakeHubJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeRegisterReq is the hub's POST /register body (core §2.12.2), decoded
// strictly as the real hub decodes it (CORE-E7): a member it does not list
// is 400 bad_request, except "invite", which is 400 invite_removed.
type fakeRegisterReq struct {
	AID     string          `json:"aid"`
	Name    string          `json:"name"`
	Caps    []string        `json:"caps"`
	Summary string          `json:"summary"`
	Readme  string          `json:"readme"`
	KEL     string          `json:"kel"`
	EncKeys string          `json:"enc_keys,omitempty"`
	PrevSeq *uint64         `json:"prev_seq,omitempty"`
	A2ACard json.RawMessage `json:"a2a_card,omitempty"`
	PoW     *struct {
		Seed  string `json:"seed"`
		Nonce string `json:"nonce"`
	} `json:"pow,omitempty"`
	CheckOnly bool `json:"check_only,omitempty"`
}

// The fake's admission modes (hub-payment-privacy PAY-A1 step 4). The
// default admits every AID, as a hub whose operator admitted everyone the
// tests register; closed admits only h.admitted; open asks a newcomer for a
// proof of work of h.powBits bits (few, so a test solves it at once).
const (
	fakeAdmitAll    = ""
	fakeAdmitClosed = "closed"
	fakeAdmitOpen   = "open"
)

// fakeRegisterAnswer is a scripted /register answer: the status, the
// Retry-After and the body the next request gets instead of the rules.
type fakeRegisterAnswer struct {
	status     int
	retryAfter string
	body       map[string]any
	// drop closes the connection without an answer.
	drop bool
}

// fakeHubRefuse writes a wire-3 refusal: {reason, error, …}.
func fakeHubRefuse(w http.ResponseWriter, status int, reason string, extra map[string]any) {
	body := map[string]any{"reason": reason, "error": "fake hub: " + reason}
	for k, v := range extra {
		body[k] = v
	}
	fakeHubJSON(w, status, body)
}

// hRegister mirrors the Hub's POST /register at wire 3 (core §2.12.2,
// hub-payment-privacy PAY-A1 and the WIRE3 fixtures register*.json): a
// strict body; the KEL in base64url; relayauth v2 against that KEL, which
// must replay to the registrant's AID and extend any KEL already held; the
// admission gate for an AID not registered here, with check_only answered
// before anything is written; the card gate; enc_keys reported per field.
func (h *fakeHub) hRegister(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	h.mu.Lock()
	h.registerCalls++
	var scripted *fakeRegisterAnswer
	if len(h.registerScript) > 0 {
		scripted = &h.registerScript[0]
		h.registerScript = h.registerScript[1:]
	}
	h.mu.Unlock()
	if scripted != nil {
		if scripted.drop {
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
		}
		if scripted.retryAfter != "" {
			w.Header().Set("Retry-After", scripted.retryAfter)
		}
		fakeHubJSON(w, scripted.status, scripted.body)
		return
	}
	var req fakeRegisterReq
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var probe map[string]json.RawMessage
		if json.Unmarshal(body, &probe) == nil {
			if _, inv := probe["invite"]; inv {
				fakeHubRefuse(w, http.StatusBadRequest, "invite_removed", nil)
				return
			}
		}
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	if req.AID == "" || req.KEL == "" {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	// enc_keys is base64url and comes with prev_seq (PAY-A1 step 3).
	var encKeys []byte
	if req.EncKeys != "" {
		var err error
		if encKeys, err = hubapi.DecodeB64(req.EncKeys, -1); err != nil || req.PrevSeq == nil {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
			return
		}
	}
	var powSeed, powNonce []byte
	if req.PoW != nil {
		var e1, e2 error
		powSeed, e1 = base64.RawURLEncoding.Strict().DecodeString(req.PoW.Seed)
		powNonce, e2 = base64.RawURLEncoding.Strict().DecodeString(req.PoW.Nonce)
		if e1 != nil || e2 != nil || len(powSeed) != 32 || len(powNonce) < 1 || len(powNonce) > regpow.MaxNonce {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
			return
		}
	}
	kelBytes, err := base64.RawURLEncoding.Strict().DecodeString(req.KEL)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "kel_invalid", nil)
		return
	}
	kelEvents, err := identity.UnmarshalKEL(kelBytes)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "kel_invalid", nil)
		return
	}
	if got, err := replayedAID(kelEvents); err != nil || got != req.AID {
		fakeHubRefuse(w, http.StatusBadRequest, "kel_invalid", nil)
		return
	}
	if aid := h.verifyAuth(w, r, body, relayauth.ActionRegister, kelBytes); aid != req.AID {
		if aid != "" {
			fakeHubRefuse(w, http.StatusUnauthorized, "unauthorized", nil)
		}
		return
	}
	// The admission gate, for an AID not registered here (PAY-A1 step 4).
	h.mu.Lock()
	_, known := h.agents[req.AID]
	mode, bits := h.admission, h.powBits
	listed := h.admitted[req.AID]
	if req.CheckOnly {
		h.checkOnlyCalls++
	} else {
		h.registerKELs = append(h.registerKELs, append([]byte(nil), kelBytes...))
	}
	h.mu.Unlock()
	checkStatus := "admitted"
	newcomer := false
	if !known && !listed {
		switch mode {
		case fakeAdmitClosed:
			fakeHubRefuse(w, http.StatusForbidden, "admission_required", map[string]any{"aid": req.AID})
			return
		case fakeAdmitOpen:
			checkStatus, newcomer = "open", true
			if !req.CheckOnly {
				seed := h.powSeedNow()
				var nonce []byte
				if powSeed != nil && bytes.Equal(powSeed, seed[:]) {
					nonce = powNonce
				}
				if nonce == nil || !regpow.Check(h.self.AID(), req.AID, seed, nonce, bits) {
					fakeHubRefuse(w, http.StatusForbidden, "pow_required", map[string]any{"challenge": map[string]any{
						"seed": base64.RawURLEncoding.EncodeToString(seed[:]), "bits": bits,
						"not_after": regpow.NotAfter(regpow.Hour(uint64(time.Now().UnixMilli())))}})
					return
				}
				h.mu.Lock()
				quota := h.quotaSpent
				h.mu.Unlock()
				if quota {
					w.Header().Set("Retry-After", "3600")
					fakeHubRefuse(w, http.StatusTooManyRequests, "registration_quota", nil)
					return
				}
			}
		}
	}
	h.mu.Lock()
	if prior, ok := h.agents[req.AID]; ok {
		old, _ := identity.UnmarshalKEL(prior.kel)
		if err := identity.ExtendsKEL(old, kelEvents); err != nil {
			h.mu.Unlock()
			fakeHubRefuse(w, http.StatusConflict, "kel_fork", nil)
			return
		}
		// The key set must follow the stored one (PAY-A1 step 9, FS-H4):
		// otherwise 409 keys_rollback, and nothing is written.
		if encKeys != nil {
			if stored, ok := keysRollbackLocked(prior, encKeys, *req.PrevSeq); ok {
				h.mu.Unlock()
				fakeHubRefuse(w, http.StatusConflict, hubapi.KeysRollbackReason, map[string]any{"stored_seq": stored})
				return
			}
		}
	}
	h.mu.Unlock()
	if req.CheckOnly {
		fakeHubJSON(w, http.StatusOK, map[string]string{"status": checkStatus})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[req.AID]
	if !ok {
		a = &fakeHubAgent{view: hubapi.AgentView{
			AID:          req.AID,
			RegisteredAt: time.Now().UTC().Format(time.RFC3339),
		}}
		h.agents[req.AID] = a
		if newcomer {
			until := uint64(time.Now().Add(72 * time.Hour).UnixMilli())
			h.probation[req.AID] = until
		}
	}
	a.view.Name = req.Name
	a.view.Caps = req.Caps
	a.kel = kelBytes
	// Registering counts as being seen: the real hub writes last_seen_at on
	// register too, so a node that just joined is never reported quiet.
	h.lastSeen[req.AID] = time.Now()
	revoked := uint64(len(h.revocations[req.AID]))
	out := hubapi.RegisterResponse{AID: req.AID, Status: "registered", KeysStatus: hubapi.KeysStatusAbsent, CardStatus: "absent",
		Revocations: &revoked}
	if until, ok := h.probation[req.AID]; ok {
		out.ProbationUntil, out.Trial = &until, "pending"
	}
	if encKeys != nil {
		out.KeysStatus, out.KeysError, _ = h.admitKeysLocked(a, encKeys, *req.PrevSeq, kelEvents)
	}
	h.registerKeys[req.AID] = out.KeysStatus
	switch {
	case string(bytes.TrimSpace(req.A2ACard)) == hubapi.WithdrawCard:
		// an internal note item: null withdraws the card (and, as on the real hub, its
		// high-water mark goes with the row).
		h.a2aWithdrawals++
		delete(h.a2aCards, req.AID)
		delete(h.a2aMarks, req.AID)
		out.CardStatus = hubapi.CardStatusWithdrawn
	case len(req.A2ACard) > 0:
		h.a2aCardSends++
		out.CardStatus, out.CardError = h.admitA2ACardLocked(req.AID, req.A2ACard, kelEvents)
	}
	h.registerCards[req.AID] = out.CardStatus
	ans := fakeRegisterResponse{RegisterResponse: out}
	ans.KeyStateNotice, ans.KeyStateNoticeError = h.noticeForLocked(req.AID)
	fakeHubJSON(w, http.StatusOK, ans)
}

// powSeedNow is the fake's seed of this hour (regpow.Seed under its key).
func (h *fakeHub) powSeedNow() [32]byte {
	return regpow.Seed(h.powKey[:], regpow.Hour(uint64(time.Now().UnixMilli())))
}

// admitA2ACardLocked admits an A2A network card for a registering agent
// (A2A-DESIGN §3.7, §10.3; hub brief H1): verified against the submitted
// KEL, then the params.seq rule against the stored mark. The caller holds
// h.mu.
func (h *fakeHub) admitA2ACardLocked(aid string, raw []byte, kel []identity.SignedEvent) (string, string) {
	v, err := a2acard.Verify(raw, func(a string) ([]identity.SignedEvent, error) {
		if a != aid {
			return nil, fmt.Errorf("not the registrant")
		}
		return kel, nil
	}, uint64(time.Now().UnixMilli()))
	if err != nil {
		return hubapi.CardStatusInvalid, err.Error()
	}
	if v.AID != aid {
		return hubapi.CardStatusInvalid, "card is signed for another AID"
	}
	var stored *a2acard.Mark
	if m, ok := h.a2aMarks[aid]; ok {
		stored = &m
	}
	dec, err := a2acard.CheckHighWater(stored, v.Mark())
	if err != nil {
		return hubapi.CardStatusConflict, err.Error()
	}
	h.a2aCards[aid] = append([]byte(nil), raw...)
	h.a2aMarks[aid] = v.Mark()
	if dec == a2acard.Same {
		return hubapi.CardStatusUnchanged, ""
	}
	return hubapi.CardStatusOK, ""
}

// keysRollbackLocked is the prev_seq rule of FS-H4 for raw against what a
// holds, as the real hub's followsStored applies it: the stored set again
// (same seq, same set) follows whatever prev_seq says; otherwise a stored
// seq above prevSeq, or a seq not above the stored one, does not follow.
// It returns the stored seq when raw does not follow. The caller holds
// h.mu.
func keysRollbackLocked(a *fakeHubAgent, raw []byte, prevSeq uint64) (uint64, bool) {
	if len(a.keyset) == 0 {
		return 0, false
	}
	signed, err := seal.UnmarshalSignedEncKeySet(raw)
	if err != nil {
		return 0, false
	}
	prev, err := seal.UnmarshalSignedEncKeySet(a.keyset)
	if err != nil {
		return 0, false
	}
	var ps, ns seal.EncKeySet
	if coredet.Unmarshal(prev.Set, &ps) != nil || coredet.Unmarshal(signed.Set, &ns) != nil {
		return 0, false
	}
	if ns.Seq == ps.Seq && bytes.Equal(signed.Set, prev.Set) {
		return 0, false
	}
	if ps.Seq > prevSeq || ns.Seq <= ps.Seq {
		return ps.Seq, true
	}
	return 0, false
}

// admitKeysLocked applies the publisher rules of §3.1 to a key set for a
// registered agent and returns the per-field status (hubapi.KeysStatus*)
// and, for a refusal, why; rollback is the stored seq when the set does not
// follow the stored one (keysRollbackLocked), with status "". The caller
// holds h.mu.
func (h *fakeHub) admitKeysLocked(a *fakeHubAgent, raw []byte, prevSeq uint64, kel []identity.SignedEvent) (string, string, *uint64) {
	signed, err := seal.UnmarshalSignedEncKeySet(raw)
	if err != nil {
		return hubapi.KeysStatusInvalid, "undecodable", nil
	}
	if stored, ok := keysRollbackLocked(a, raw, prevSeq); ok {
		return "", "the stored set is newer", &stored
	}
	at := uint64(time.Now().UnixMilli())
	if h.keysAt != nil {
		at = h.keysAt()
	}
	set, err := seal.VerifyEncKeySet(signed, a.view.AID, kel, at)
	if err != nil {
		return hubapi.KeysStatusInvalid, err.Error(), nil
	}
	if h.refuseKeySets {
		return hubapi.KeysStatusInvalid, "this hub refuses key sets (test)", nil
	}
	// A seq the hub's signed column cannot hold, as the real hub refuses it.
	if set.Seq > 1<<63-1 {
		return hubapi.KeysStatusInvalid, "seq above 2^63 - 1", nil
	}
	// SSR-W9, as the real hub applies it: the mailbox epoch is below 2^53,
	// never falls, and rises whenever the mailbox key changes.
	if set.Mbox.Epoch >= 1<<53 {
		return hubapi.KeysStatusInvalid, "mbox epoch past 2^53", nil
	}
	if len(a.keyset) > 0 {
		if prev, err := seal.UnmarshalSignedEncKeySet(a.keyset); err == nil {
			var ps seal.EncKeySet
			if coredet.Unmarshal(prev.Set, &ps) == nil {
				if set.Mbox.Epoch < ps.Mbox.Epoch ||
					!bytes.Equal(set.Mbox.Pub, ps.Mbox.Pub) && set.Mbox.Epoch == ps.Mbox.Epoch ||
					bytes.Equal(set.Mbox.Pub, ps.Mbox.Pub) && set.Mbox.Epoch != ps.Mbox.Epoch {
					return hubapi.KeysStatusInvalid, "mbox epoch must rise when the mailbox key changes", nil
				}
				if ps.Seq == set.Seq {
					// The same set, maybe signed again (a KEL rotation).
					a.keyset = append([]byte(nil), raw...)
					return hubapi.KeysStatusUnchanged, "", nil
				}
			}
		}
	}
	a.keyset = append([]byte(nil), raw...)
	return hubapi.KeysStatusOK, "", nil
}

// hKeysPost mirrors POST /agents/{aid}/keys (FS-H4): signed by the AID
// itself, {keys: base64url, prev_seq} decoded strictly, verified against the
// registered KEL; 200 ok or unchanged, 409 keys_rollback with stored_seq,
// 400 bad_request for anything else.
func (h *fakeHub) hKeysPost(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionKeys, nil)
	if aid == "" {
		return
	}
	if aid != r.PathValue("aid") {
		fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		return
	}
	var req struct {
		Keys    string  `json:"keys"`
		PrevSeq *uint64 `json:"prev_seq"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Keys == "" || req.PrevSeq == nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	raw, err := hubapi.DecodeB64(req.Keys, -1)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keyPosts++
	h.keyBodies = append(h.keyBodies, append([]byte(nil), body...))
	if len(h.forgedRollback) > 0 {
		stored := h.forgedRollback[0]
		h.forgedRollback = h.forgedRollback[1:]
		fakeHubRefuse(w, http.StatusConflict, hubapi.KeysRollbackReason, map[string]any{"stored_seq": stored})
		return
	}
	a := h.agents[aid]
	kel, err := identity.UnmarshalKEL(a.kel)
	if err != nil {
		fakeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": "stored kel unreadable"})
		return
	}
	switch st, why, stored := h.admitKeysLocked(a, raw, *req.PrevSeq, kel); {
	case stored != nil:
		fakeHubRefuse(w, http.StatusConflict, hubapi.KeysRollbackReason, map[string]any{"stored_seq": *stored})
	case st == hubapi.KeysStatusOK, st == hubapi.KeysStatusUnchanged:
		fakeHubJSON(w, http.StatusOK, hubapi.KeysPublishResponse{AID: aid, KeysStatus: st})
	default:
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"reason": "bad_request", "error": "The key set was not accepted: " + why + "."})
	}
}

// hKeysGet mirrors GET /agents/{aid}/keys: the agents registered here,
// else 404, binary values in base64url (wire 3, CORE-E5). The daemon asks
// it of its own AID only (SSR-W16).
func (h *fakeHub) hKeysGet(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.keysGets++
	h.mu.Unlock()
	aid := r.PathValue("aid")
	if out, ok := h.keysFor(aid); ok {
		fakeHubJSON(w, http.StatusOK, out)
		return
	}
	fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "no keys for " + aid})
}

// keysFor answers a keys lookup for aid, registered here.
func (h *fakeHub) keysFor(aid string) (hubapi.KeysResponse, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if out, ok := h.keysOverride[aid]; ok {
		return out, true
	}
	if a := h.agents[aid]; a != nil && len(a.keyset) > 0 {
		return hubapi.KeysResponse{AID: aid, KeySet: hubapi.B64(a.keyset), KEL: hubapi.B64(a.kel)}, true
	}
	return hubapi.KeysResponse{}, false
}

// hDirBucket mirrors POST /directory:bucket (sealed-sender §2.9; ANetHub
// internal/aghub/directory.go): no identity header; prefix and bits, the
// prefix well formed; bits at most dir_max_bits (400 bucket_too_narrow
// with max_bits); the bucket's entries, this hub's and its peers', in hash
// order, a page at a time; partial when a peer is down.
func (h *fakeHub) hDirBucket(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(hubapi.HeaderAID) != "" || r.Header.Get(hubapi.HeaderSig) != "" || r.Header.Get(hubapi.HeaderSession) != "" {
		fakeHubRefuse(w, http.StatusBadRequest, "identity_headers_not_allowed", nil)
		return
	}
	body, ok := h.readBody(w, r, 4<<10)
	if !ok {
		return
	}
	var req hubapi.DirBucketRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	h.mu.Lock()
	h.bucketQueries = append(h.bucketQueries, req)
	down, maxBits, page, partial, pad := h.dirDown, h.dirMaxBits, h.dirPage, h.dirPartial, h.dirPad
	stated := maxBits
	if h.dirAnswerMaxBits != nil {
		stated = *h.dirAnswerMaxBits
	}
	peers := append([]*fakeHub(nil), h.peers...)
	h.mu.Unlock()
	if down {
		fakeHubRefuse(w, http.StatusServiceUnavailable, "unavailable", nil)
		return
	}
	prefix, err := hubapi.DecodeB64(req.Prefix, -1)
	if err != nil || req.Bits < 0 || dirbox.CheckPrefix(prefix, req.Bits) != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	if req.Bits > maxBits {
		fakeHubRefuse(w, http.StatusBadRequest, "bucket_too_narrow", map[string]any{"max_bits": maxBits})
		return
	}
	entries := h.bucketEntries(prefix, req.Bits)
	for _, p := range peers {
		p.mu.Lock()
		pdown := p.dirDown
		p.mu.Unlock()
		if pdown {
			partial = true
			continue
		}
		entries = append(entries, p.bucketEntries(prefix, req.Bits)...)
	}
	for range pad {
		box := make([]byte, 64)
		_, _ = rand.Read(box)
		entries = append(entries, hubapi.DirBucketEntry{H: hubapi.B64(randomInBucket(prefix, req.Bits)), Box: hubapi.B64(box)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].H < entries[j].H })
	start := 0
	if req.Cursor != "" {
		n, err := strconv.Atoi(req.Cursor)
		if err != nil || n < 0 || n > len(entries) {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
			return
		}
		start = n
	}
	out := hubapi.DirBucketResponse{MaxBits: stated, Partial: partial, Entries: entries[start:]}
	if page > 0 && len(out.Entries) > page {
		out.Entries = out.Entries[:page]
		out.Next = strconv.Itoa(start + page)
	}
	if out.Entries == nil {
		out.Entries = []hubapi.DirBucketEntry{}
	}
	fakeHubJSON(w, http.StatusOK, out)
}

// randomInBucket is a directory hash of nobody in the bucket (prefix, bits).
func randomInBucket(prefix []byte, bits int) []byte {
	h := make([]byte, dirbox.HashLen)
	_, _ = rand.Read(h)
	n := bits / 8
	copy(h, prefix[:n])
	if r := bits % 8; r != 0 {
		mask := byte(0xff << (8 - r))
		h[n] = prefix[n]&mask | h[n]&^mask
	}
	return h
}

// bucketEntries are this hub's own entries in the bucket (prefix, bits):
// one for each agent registered here with a key set, sealed under its AID
// (dirbox.Seal), home this hub, its card and the notice for its KEL's tip
// when notices are on; overridden, added to or left out as the knobs say.
func (h *fakeHub) bucketEntries(prefix []byte, bits int) []hubapi.DirBucketEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hubapi.DirBucketEntry
	add := func(aid string, e dirbox.DirEntry) {
		hh, err := dirbox.Hash(aid)
		if err != nil || !dirbox.InBucket(hh, prefix, bits) {
			return
		}
		if len(e.Notice) == 0 && !h.noSuiteNotices {
			e.Notice = h.suiteNoticeLocked(e.KEL)
		}
		box, err := dirbox.Seal(aid, &e, nil)
		if err != nil {
			return
		}
		out = append(out, hubapi.DirBucketEntry{H: hubapi.B64(hh), Box: hubapi.B64(box)})
	}
	for aid, a := range h.agents {
		if len(a.keyset) == 0 || h.dirHide[aid] {
			continue
		}
		e := dirbox.DirEntry{AID: aid, Home: h.self.AID(), KEL: a.kel, Keys: a.keyset, Card: h.a2aCards[aid]}
		if n, _ := h.noticeForLocked(aid); n != "" {
			e.Notice, _ = base64.RawURLEncoding.DecodeString(n)
		}
		if o, ok := h.dirOverride[aid]; ok {
			e = o
		}
		add(aid, e)
	}
	for aid, o := range h.dirOverride {
		if a := h.agents[aid]; a == nil || len(a.keyset) == 0 {
			add(aid, o)
		}
	}
	for aid, es := range h.dirExtra {
		for _, e := range es {
			add(aid, e)
		}
	}
	for _, e := range h.dirRaw {
		if hh, err := hubapi.DecodeB64(e.H, dirbox.HashLen); err == nil && dirbox.InBucket(hh, prefix, bits) {
			out = append(out, e)
		}
	}
	return out
}

// setDir changes the fake's directory under its lock.
func (h *fakeHub) setDir(f func(h *fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dirOverride == nil {
		h.dirOverride = map[string]dirbox.DirEntry{}
	}
	if h.dirExtra == nil {
		h.dirExtra = map[string][]dirbox.DirEntry{}
	}
	if h.dirHide == nil {
		h.dirHide = map[string]bool{}
	}
	f(h)
}

// queries are the bucket queries this hub got, in order.
func (h *fakeHub) queries() []hubapi.DirBucketRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubapi.DirBucketRequest(nil), h.bucketQueries...)
}

// hDeregister mirrors POST /agents/{aid}/deregister.
//
// The fake had no deregister at all, so hub-leave answered 404 here while
// working against the real hub — the same shape of gap as the card gate:
// a fake that implements only the happy path agrees with whatever the code
// does. It removes the routing and keeps the evidence, which is the real
// hub's rule: reviews and balances record things that happened, and
// deleting them to tidy the directory would be rewriting history.
func (h *fakeHub) hDeregister(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	h.mu.Lock()
	_, known := h.agents[aid]
	h.mu.Unlock()
	if !known {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionDeregister, nil); who != aid {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.agents[aid]
	// Undelivered mail is reported rather than dropped silently: somebody
	// sent work and is waiting for it.
	undelivered := 0
	for _, m := range h.mailbox {
		if m.toAID == aid {
			undelivered++
		}
	}
	delete(h.agents, aid)
	h.departedKEL[aid] = a.kel // evidence stays verifiable after the routing goes
	fakeHubJSON(w, http.StatusOK, map[string]any{
		"aid": aid, "status": "deregistered", "undelivered": undelivered,
	})
}

// hProfile mirrors POST /profile (relayauth v2, action profile).
func (h *fakeHub) hProfile(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	// Decoded strictly, as the real hub decodes it: a member it does not
	// list (pricing, gone in 0.3.0) is 400 bad_request.
	var req struct {
		AID     string `json:"aid"`
		Summary string `json:"summary"`
		Readme  string `json:"readme"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	if req.AID == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "aid required"})
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionProfile, nil); who != req.AID {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the profile's agent"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[req.AID]
	if !ok {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "agent not registered"})
		return
	}
	a.view.Summary, a.view.Readme = req.Summary, req.Readme
	fakeHubJSON(w, http.StatusOK, map[string]any{"aid": req.AID, "status": "profile_updated"})
}

// hVisibility mirrors POST /agents/{aid}/visibility (relayauth v2).
func (h *fakeHub) hVisibility(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionVisibility, nil); who != r.PathValue("aid") {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		}
		return
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"status": "set"})
}

// viewWithAggregates fills the derived fields (Listed + rating aggregate) the way the real Hub does.
// Caller must hold h.mu.
func (h *fakeHub) viewWithAggregates(a *fakeHubAgent) hubapi.AgentView {
	v := a.view
	v.Listed = len(v.Caps) > 0 || v.Summary != "" || v.Readme != ""
	var sum, n int
	for _, rv := range h.reviews {
		if rv.SubjectAID == v.AID {
			sum += rv.Rating
			n++
		}
	}
	v.ReviewCount = n
	if n > 0 {
		v.AvgRating = float64(sum) / float64(n)
	}
	return v
}

// hAgents mirrors GET /agents?q= — LISTED agents only, case-insensitive substring filter.
func (h *fakeHub) hAgents(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	h.mu.Lock()
	defer h.mu.Unlock()
	agents := []hubapi.AgentView{}
	for _, a := range h.agents {
		v := h.viewWithAggregates(a)
		if !v.Listed {
			continue
		}
		if q != "" {
			hay := strings.ToLower(v.AID + " " + v.Name + " " + strings.Join(v.Caps, " ") + " " + v.Summary + " " + v.Readme)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		agents = append(agents, v)
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// hAgent mirrors GET /agents/{aid} — one agent + its reviews (newest first).
func (h *fakeHub) hAgent(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[aid]
	if !ok {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "hub: agent " + aid + " not found"})
		return
	}
	reviews := []hubapi.ReviewView{}
	for _, rv := range h.reviews {
		if rv.SubjectAID == aid {
			reviews = append(reviews, rv)
		}
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].CreatedAt > reviews[j].CreatedAt })
	fakeHubJSON(w, http.StatusOK, map[string]any{"agent": h.viewWithAggregates(a), "reviews": reviews})
}

// hUploadReview mirrors POST /reviews after the content removal (A2A-DESIGN §9, ANetHub
// hUploadReview): the body is the receipt and the review and nothing else; a body that still carries
// request_doc or deliverable is refused with 400, as the hub refuses it. It keeps the structural
// checks (interlock, rating range, comment length, one review per interaction) and skips the two
// signature verifications. The stored view carries no content and says so.
func (h *fakeHub) hUploadReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		hubapi.UploadReviewRequest
		RequestDoc  string `json:"request_doc"`
		Deliverable string `json:"deliverable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Receipt == "" || req.Review == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt + review required"})
		return
	}
	if req.RequestDoc != "" || req.Deliverable != "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "this hub takes no task content with a review"})
		return
	}
	rcBytes, err1 := base64.StdEncoding.DecodeString(req.Receipt)
	rvBytes, err2 := base64.StdEncoding.DecodeString(req.Review)
	if err1 != nil || err2 != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt/review not base64"})
		return
	}
	rc, err := evidence.UnmarshalReceipt(rcBytes)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt undecodable"})
		return
	}
	rv, err := evidence.UnmarshalReview(rvBytes)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "review undecodable"})
		return
	}
	if !rv.ValidRating() {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "rating out of range"})
		return
	}
	if len([]rune(rv.Comment)) > 280 {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "comment over 280 characters"})
		return
	}
	if rc.InteractionID != rv.InteractionID || rv.ReviewerAID != rc.RequesterAID || rv.SubjectAID != rc.ProviderAID {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt/review interlock mismatch"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.reviews[rv.InteractionID]; dup {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction already reviewed"})
		return
	}
	h.reviews[rv.InteractionID] = hubapi.ReviewView{
		InteractionID:  rv.InteractionID,
		SubjectAID:     rv.SubjectAID,
		ReviewerAID:    rv.ReviewerAID,
		Rating:         rv.Rating,
		Comment:        rv.Comment,
		ReceiptCID:     rv.ReceiptCID,
		RequestCID:     rc.RequestCID,
		ResultCID:      rc.ResultCID,
		ContentBinding: hubapi.ContentBindingUnverified,
		CompletedAt:    rc.CompletedAt,
		CreatedAt:      rv.CreatedAt,
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"interaction_id": rv.InteractionID, "status": "accepted"})
}

// hRelaySession mirrors POST /relay/session (sealed-sender §4.2.3, core
// L7): relayauth session by a registered AID, the body {epk} strictly, a
// new random session id and key, the key sealed to epk under the hub's AID
// and the session id. The session lives sessionLife (an hour).
func (h *fakeHub) hRelaySession(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 4<<10)
	if !ok {
		return
	}
	h.mu.Lock()
	_, registered := h.agents[r.Header.Get(hubapi.HeaderAID)]
	refuse := h.sessionRefuse
	h.mu.Unlock()
	if !registered {
		fakeHubRefuse(w, http.StatusForbidden, "admission_required", map[string]any{"aid": r.Header.Get(hubapi.HeaderAID)})
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionSession, nil)
	if aid == "" {
		return
	}
	switch refuse {
	case "":
	case "rate_limited":
		w.Header().Set("Retry-After", "60")
		fakeHubRefuse(w, http.StatusTooManyRequests, refuse, nil)
		return
	default:
		fakeHubRefuse(w, http.StatusForbidden, refuse, nil)
		return
	}
	var req hubapi.SessionRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	epk, err := hubapi.DecodeB64(req.EPK, relayauth.SessionPublicKeyLen)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	sid, key := make([]byte, relayauth.SessionIDLen), make([]byte, relayauth.SessionKeyLen)
	_, _ = rand.Read(sid)
	_, _ = rand.Read(key)
	enc, ct, err := relayauth.SealSessionKey(epk, h.self.AID(), sid, key)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	h.mu.Lock()
	life := h.sessionLife
	if life == 0 {
		life = time.Hour
	}
	exp := time.Now().Add(life)
	h.sessions[string(sid)] = &fakeSession{aid: aid, key: key, exp: exp}
	h.sessionOpens++
	hook := h.sessionHook
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	fakeHubJSON(w, http.StatusOK, hubapi.SessionResponse{SID: hubapi.B64(sid), Exp: uint64(exp.UnixMilli()),
		Enc: hubapi.B64(enc), CT: hubapi.B64(ct)})
}

// forgetSessions ends every session, as a hub that restarted.
func (h *fakeHub) forgetSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = map[string]*fakeSession{}
}

// hRelaySend mirrors POST /relay/send at wire 3 (sealed-sender §4.2.1, in
// its order where it matters to a sender): the headers (Content-Type, no
// identity header, X-ANet-Access, the session headers of a first contact
// or none under a capability), the envelope (ParseOuter, and X-ANet-Access
// byte for byte its outer key 7), the credential (the session's MAC over
// the request, or seal.VerifyAccess under the owner's mailbox key), the
// recipient, a duplicate still queued, the sender's budget, the mailbox.
// Like the real hub it reads nothing past the outer envelope.
func (h *fakeHub) hRelaySend(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	maxEnv, down := h.maxEnvelope, h.relayDown
	h.mu.Unlock()
	if down {
		fakeHubRefuse(w, http.StatusServiceUnavailable, "internal", nil)
		return
	}
	if maxEnv == 0 {
		maxEnv = fakeHubDefaultMaxEnvelope
	}
	if r.Header.Get("Content-Type") != hubapi.EnvelopeContentType {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	for _, hd := range []string{hubapi.HeaderAID, hubapi.HeaderSeq, hubapi.HeaderSig} {
		if len(r.Header.Values(hd)) > 0 {
			fakeHubRefuse(w, http.StatusBadRequest, "identity_headers_not_allowed", nil)
			return
		}
	}
	accessRaw, err := hubapi.DecodeB64(r.Header.Get(hubapi.HeaderAccess), -1)
	if err != nil || len(accessRaw) == 0 || len(r.Header.Get(hubapi.HeaderAccess)) > hubapi.MaxAccessHeader {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_envelope", nil)
		return
	}
	hasSession := r.Header.Get(hubapi.HeaderSession) != "" || r.Header.Get(hubapi.HeaderMAC) != ""
	env, err := io.ReadAll(io.LimitReader(r.Body, int64(maxEnv)+1))
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	if len(env) > maxEnv {
		fakeHubRefuse(w, http.StatusRequestEntityTooLarge, "too_large", nil)
		return
	}
	// The structural check the real hub makes, and the only look at the
	// envelope it takes: the outer header, never the ciphertext.
	outer, err := seal.ParseOuter(env)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_envelope", nil)
		return
	}
	if want, err := coredet.Marshal(outer.Access); err != nil || !bytes.Equal(want, accessRaw) {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_envelope", nil)
		return
	}
	digest, err := seal.EnvDigest(env)
	if err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_envelope", nil)
		return
	}
	var sender string
	switch outer.Access.Kind {
	case seal.AccessKindSession:
		sid, err1 := relayauth.DecodeSessionID(r.Header.Get(hubapi.HeaderSession))
		mac, err2 := relayauth.DecodeMAC(r.Header.Get(hubapi.HeaderMAC))
		ts, err3 := strconv.ParseUint(r.Header.Get(hubapi.HeaderTS), 10, 64)
		h.mu.Lock()
		sess := h.sessions[string(sid)]
		h.mu.Unlock()
		if err1 != nil || err2 != nil || err3 != nil || sess == nil || !time.Now().Before(sess.exp) ||
			ts+relayauth.MaxSkewMillis < uint64(time.Now().UnixMilli()) ||
			relayauth.VerifySessionMAC(sess.key, sid, ts, r.Method, r.URL.RequestURI(), r.Header.Get(hubapi.HeaderHome), env, mac) != nil {
			fakeHubRefuse(w, http.StatusUnauthorized, "session_invalid", nil)
			return
		}
		sender = sess.aid
	default:
		if hasSession {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
			return
		}
		h.mu.Lock()
		owner := h.agents[outer.To]
		var keyset []byte
		if owner != nil {
			keyset = owner.keyset
		}
		h.mu.Unlock()
		mbox, ok := fakeHubMbox(keyset)
		if !ok || seal.VerifyAccess(outer.Access, digest, mbox, outer.To, uint64(time.Now().UnixMilli())) != nil {
			fakeHubRefuse(w, http.StatusForbidden, "access_invalid", nil)
			return
		}
		sender = "cap:" + hex.EncodeToString(capIDOf(outer.Access))
	}
	h.mu.Lock()
	hook := h.sendHook
	h.mu.Unlock()
	if hook != nil && hook(env).answer(w) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.agents[outer.To]; !ok {
		fakeHubRefuse(w, http.StatusNotFound, "recipient_unknown", nil)
		return
	}
	if want, ok := h.homeMustBe[outer.To]; ok && outer.Access.Kind == seal.AccessKindSession && r.Header.Get(hubapi.HeaderHome) != want {
		fakeHubRefuse(w, http.StatusNotFound, "recipient_unknown", nil)
		return
	}
	answer := func(status string, rid []byte) {
		out := hubapi.RelaySendResponse{Status: status, RID: hubapi.B64(rid), ViaHub: h.sendViaHub}
		// Queued either way, and the sender is told when the recipient has
		// not collected its mail in a long time. A never-seen agent is
		// unknown, not quiet.
		if seen, ok := h.lastSeen[outer.To]; ok && time.Since(seen) > fakeHubQuietAfter {
			out.RecipientQuiet = true
		}
		fakeHubJSON(w, http.StatusOK, out)
	}
	for _, m := range h.mailbox {
		if m.toAID == outer.To && m.digest == digest {
			answer(hubapi.RelayDuplicate, m.rid)
			return
		}
	}
	if h.senderLimit > 0 && h.sendsBy[sender] >= h.senderLimit {
		ra := h.retryAfter
		if ra == "" {
			ra = "1"
		}
		w.Header().Set("Retry-After", ra)
		fakeHubRefuse(w, http.StatusTooManyRequests, "rate_limited", nil)
		return
	}
	if h.mailboxCap > 0 {
		held := 0
		for _, m := range h.mailbox {
			if m.toAID == outer.To {
				held++
			}
		}
		if held >= h.mailboxCap {
			fakeHubRefuse(w, http.StatusInsufficientStorage, "mailbox_full", nil)
			return
		}
	}
	h.sendsBy[sender]++
	h.relaySends++
	h.nextID++
	rid := make([]byte, hubapi.RIDLen)
	_, _ = rand.Read(rid)
	if h.dropSends {
		answer(hubapi.RelayQueued, rid)
		return
	}
	h.mailbox = append(h.mailbox, &fakeHubMsg{
		id: h.nextID, toAID: outer.To, payload: env, rid: rid, digest: digest,
		createdAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	answer(hubapi.RelayQueued, rid)
}

// fakeHubMbox is the mailbox signing key of a stored key set.
func fakeHubMbox(keyset []byte) (seal.Mbox, bool) {
	signed, err := seal.UnmarshalSignedEncKeySet(keyset)
	if err != nil {
		return seal.Mbox{}, false
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(signed.Set, &set); err != nil || set.Mbox.Epoch == 0 {
		return seal.Mbox{}, false
	}
	return set.Mbox, true
}

// backdateLastSeen makes an agent look like it stopped collecting its mail
// `ago` ago, the way aghub.SetLastSeenForTest does on the real hub — the
// only way to exercise the quiet path without waiting an hour.
func backdateLastSeen(url, aid string, ago time.Duration) {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSeen[aid] = time.Now().Add(-ago)
}

// hRelayPoll mirrors POST /relay/poll — undelivered envelopes for the
// authenticated caller with an id above after_id (0: all), oldest first.
func (h *fakeHub) hRelayPoll(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	h.mu.Lock()
	if h.relayPollTries == nil {
		h.relayPollTries = map[string]int{}
	}
	h.relayPollTries[r.Header.Get(hubapi.HeaderAID)]++
	h.mu.Unlock()
	aid := h.verifyAuth(w, r, body, relayauth.ActionPoll, nil)
	if aid == "" {
		return
	}
	var req hubapi.RelayPollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if req.AfterID < 0 {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "after_id must not be negative"})
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pollPage > 0 && h.pollPage < limit {
		limit = h.pollPage
	}
	if h.pollAll {
		limit = len(h.mailbox) + 1
	}
	// Collecting mail IS the liveness signal the real hub records (SeenPolling).
	h.lastSeen[aid] = time.Now()
	if h.relayPolls == nil {
		h.relayPolls = map[string]int{}
	}
	h.relayPolls[aid]++
	out := []hubapi.RelayMessage{}
	more := false
	// h.mailbox is in id order: ids come from nextID and rows are appended.
	for _, m := range h.mailbox {
		if m.toAID != aid || m.id <= req.AfterID {
			continue
		}
		if len(out) >= limit {
			// The page is full and a row follows it (FS-H5).
			more = true
			break
		}
		var accepted uint64
		if at, err := time.Parse(time.RFC3339Nano, m.createdAt); err == nil {
			accepted = uint64(at.UnixMilli())
		}
		out = append(out, hubapi.RelayMessage{ID: m.id, Envelope: hubapi.B64(m.payload), AcceptedAt: accepted})
		if h.relayServed == nil {
			h.relayServed = map[string][]int64{}
		}
		h.relayServed[aid] = append(h.relayServed[aid], m.id)
	}
	if h.pollMore != nil {
		more = *h.pollMore
	}
	if h.whilePolling != nil {
		h.whilePolling()
	}
	resp := hubapi.RelayPollResponse{Messages: out, More: more}
	resp.KeyStateNotice, resp.KeyStateNoticeError = h.noticeForLocked(aid)
	fakeHubJSON(w, http.StatusOK, resp)
}

// hRelayAck mirrors POST /relay/ack (DLV §2.4.2) — delete the acked rows,
// scoped to the caller's own mailbox, and keep the delivery token an item
// carries only for a row this request deleted (DLV-H-3). A request the real
// hub refuses whole is refused whole here: an unknown member (400
// bad_request), no items or more than 1000, an item without an id, a
// handle without a dack or the other way round, or a value that is not
// strict base64url of 16 and 80 bytes (400 bad_ack_item, DLV-W-14).
func (h *fakeHub) hRelayAck(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionAck, nil)
	if aid == "" {
		return
	}
	var req struct {
		Items []struct {
			ID     *int64  `json:"id"`
			Handle *string `json:"handle"`
			DAck   *string `json:"dack"`
		} `json:"items"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"reason": "bad_request", "error": "bad request"})
		return
	}
	badItem := func(i int) {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"reason": "bad_ack_item",
			"error": fmt.Sprintf("item %d: an id is required, handle (16 bytes) and dack (80 bytes) go together, and at most 1000 items are taken", i)})
	}
	if len(req.Items) == 0 || len(req.Items) > hubapi.RelayAckMaxItems {
		badItem(0)
		return
	}
	items := make([]hubapi.RelayAckItem, len(req.Items))
	for i, it := range req.Items {
		if it.ID == nil || (it.Handle == nil) != (it.DAck == nil) {
			badItem(i)
			return
		}
		items[i].ID = *it.ID
		if it.Handle != nil {
			if _, err := hubapi.DecodeB64(*it.Handle, hubapi.HandleLen); err != nil {
				badItem(i)
				return
			}
			if _, err := hubapi.DecodeB64(*it.DAck, hubapi.DAckLen); err != nil {
				badItem(i)
				return
			}
			items[i].Handle, items[i].DAck = *it.Handle, *it.DAck
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ackRequests = append(h.ackRequests, items)
	if h.ackFail == len(h.ackRequests) {
		fakeHubJSON(w, http.StatusServiceUnavailable, map[string]string{"reason": "unavailable", "error": "try again"})
		return
	}
	byID := map[int64]hubapi.RelayAckItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	kept := h.mailbox[:0]
	acked, stored := 0, 0
	for _, m := range h.mailbox {
		if it, ok := byID[m.id]; ok && m.toAID == aid {
			acked++
			if it.Handle != "" {
				if h.relayDacks == nil {
					h.relayDacks = map[string]string{}
				}
				h.relayDacks[it.Handle] = it.DAck
				stored++
			}
			continue
		}
		kept = append(kept, m)
	}
	h.mailbox = kept
	fakeHubJSON(w, http.StatusOK, hubapi.RelayAckResponse{Acked: acked, Stored: stored})
}

// hRelayStatus mirrors POST /relay/status (DLV §2.4.3): anonymous — a
// request with an identity header is refused — and, for each item, queued
// while its rid is in a mailbox, else done with the token an ack left under
// its handle, else unknown.
func (h *fakeHub) hRelayStatus(w http.ResponseWriter, r *http.Request) {
	for _, hd := range []string{hubapi.HeaderAID, hubapi.HeaderSeq, hubapi.HeaderSig} {
		if len(r.Header.Values(hd)) > 0 {
			fakeHubRefuse(w, http.StatusBadRequest, "identity_headers_not_allowed", nil)
			return
		}
	}
	body, ok := h.readBody(w, r, 16<<10)
	if !ok {
		return
	}
	var req hubapi.RelayStatusRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	if len(req.Items) == 0 || len(req.Items) > hubapi.RelayStatusMaxItems {
		fakeHubRefuse(w, http.StatusBadRequest, "bad_status_query", nil)
		return
	}
	for _, it := range req.Items {
		if _, err := hubapi.DecodeB64(it.RID, hubapi.RIDLen); err != nil {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_status_query", nil)
			return
		}
		if _, err := hubapi.DecodeB64(it.Handle, hubapi.HandleLen); err != nil {
			fakeHubRefuse(w, http.StatusBadRequest, "bad_status_query", nil)
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statusRequests = append(h.statusRequests, req.Items)
	h.statusHeaders = append(h.statusHeaders, r.Header.Clone())
	if h.statusDown {
		w.Header().Set("Retry-After", "1")
		fakeHubRefuse(w, http.StatusServiceUnavailable, "unavailable", nil)
		return
	}
	out := hubapi.RelayStatusResponse{Items: make([]hubapi.RelayStatusResult, len(req.Items))}
	for i, it := range req.Items {
		if h.statusHook != nil {
			if res, ok := h.statusHook(it); ok {
				out.Items[i] = res
				continue
			}
		}
		rid, _ := hubapi.DecodeB64(it.RID, hubapi.RIDLen)
		out.Items[i] = hubapi.RelayStatusResult{State: hubapi.RelayStatusUnknown}
		for _, m := range h.mailbox {
			if bytes.Equal(m.rid, rid) {
				out.Items[i] = hubapi.RelayStatusResult{State: hubapi.RelayStatusQueued}
				break
			}
		}
		if out.Items[i].State == hubapi.RelayStatusUnknown {
			if dack, ok := h.relayDacks[it.Handle]; ok {
				out.Items[i] = hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: dack}
			}
		}
	}
	fakeHubJSON(w, http.StatusOK, out)
}

// fakeSendAnswer is what a scripted hub does with a /relay/send
// (fakeHub.sendHook).
type fakeSendAnswer struct {
	kind fakeSendKind
	// status, code and retryAfter are the refusal of fakeSendRefuse.
	status     int
	code       string
	retryAfter string
}

type fakeSendKind int

const (
	// fakeSendPass: the hub's own rules.
	fakeSendPass fakeSendKind = iota
	// fakeSendAccept: 200 queued with a fresh rid, and the envelope kept
	// nowhere.
	fakeSendAccept
	// fakeSendBadRID: 200 queued with a rid that is not 16 bytes.
	fakeSendBadRID
	// fakeSendRefuse: the refusal given.
	fakeSendRefuse
	// fakeSendReset: the connection is closed without an answer, after
	// the hub read the request.
	fakeSendReset
)

// answer writes a's answer to w and reports whether it did (false for
// fakeSendPass).
func (a fakeSendAnswer) answer(w http.ResponseWriter) bool {
	switch a.kind {
	case fakeSendAccept, fakeSendBadRID:
		rid := make([]byte, hubapi.RIDLen)
		_, _ = rand.Read(rid)
		if a.kind == fakeSendBadRID {
			rid = rid[:5]
		}
		fakeHubJSON(w, http.StatusOK, hubapi.RelaySendResponse{Status: hubapi.RelayQueued, RID: hubapi.B64(rid)})
	case fakeSendRefuse:
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		fakeHubRefuse(w, a.status, a.code, nil)
	case fakeSendReset:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return true
			}
		}
		panic(http.ErrAbortHandler)
	default:
		return false
	}
	return true
}

// statusCalls is how many /relay/status requests the hub at url got.
func statusCalls(url string) int {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.statusRequests)
}

// queuedFor returns the envelopes the hub holds for toAID, oldest first.
func queuedFor(t testing.TB, srv *httptest.Server, toAID string) [][]byte {
	t.Helper()
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]byte
	for _, m := range h.mailbox {
		if m.toAID == toAID {
			out = append(out, append([]byte(nil), m.payload...))
		}
	}
	return out
}

// onlyQueuedEnvelope returns the single envelope the hub holds for toAID —
// the bytes a real second poll would deliver.
func onlyQueuedEnvelope(t *testing.T, srv *httptest.Server, toAID string) []byte {
	t.Helper()
	q := queuedFor(t, srv, toAID)
	if len(q) != 1 {
		t.Fatalf("%d envelopes queued for %s, want 1", len(q), toAID)
	}
	return q[0]
}

// injectEnvelope puts arbitrary bytes into a mailbox, the way anyone who
// can reach the hub can: the hub does not authenticate what is inside.
func injectEnvelope(t *testing.T, srv *httptest.Server, toAID string, env []byte) {
	t.Helper()
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	h.mailbox = append(h.mailbox, &fakeHubMsg{id: h.nextID, toAID: toAID, payload: env,
		createdAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

// clearMailbox drops everything queued for toAID.
func clearMailbox(t testing.TB, srv *httptest.Server, toAID string) {
	t.Helper()
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.mailbox[:0]
	for _, m := range h.mailbox {
		if m.toAID != toAID {
			kept = append(kept, m)
		}
	}
	h.mailbox = kept
}

// hIdentity is how a node learns which hub's ledger its credits live on.
// The AID goes into every authorization's network field, so a payment
// signed for one hub cannot be replayed at another.
func (h *fakeHub) hIdentity(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	down := h.identityDown
	h.mu.Unlock()
	if down {
		fakeHubJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "identity unavailable"})
		return
	}
	kel, err := identity.MarshalKEL(h.self.KEL())
	if err != nil {
		fakeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.mu.Lock()
	wire := h.wire
	maxBits := h.dirMaxBits
	h.mu.Unlock()
	fakeHubJSON(w, http.StatusOK, hubapi.HubIdentity{AID: h.self.AID(), KEL: hubapi.B64(kel), Wire: &wire, DirMaxBits: &maxBits})
}

// hHubKELs mirrors GET /hub/kels (KEL H-16): this hub's own key history
// first, then its peer fakes' and the ones a test set (hubKELs), by AID,
// base64url.
func (h *fakeHub) hHubKELs(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.hubKELReads++
	others := map[string][]byte{}
	if !h.noSuiteNotices {
		if kel, err := identity.MarshalKEL(identity.SuiteController().KEL()); err == nil {
			others[identitytest.Witness] = kel
		}
	}
	for aid, kel := range h.hubKELs {
		others[aid] = kel
	}
	peers := append([]*fakeHub(nil), h.peers...)
	h.mu.Unlock()
	for _, p := range peers {
		if kel, err := identity.MarshalKEL(p.self.KEL()); err == nil {
			others[p.self.AID()] = kel
		}
	}
	own, err := identity.MarshalKEL(h.self.KEL())
	if err != nil {
		fakeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type entry struct {
		AID string `json:"aid"`
		KEL string `json:"kel"`
	}
	out := []entry{{AID: h.self.AID(), KEL: hubapi.B64(own)}}
	aids := make([]string, 0, len(others))
	for aid := range others {
		if aid != h.self.AID() {
			aids = append(aids, aid)
		}
	}
	sort.Strings(aids)
	for _, aid := range aids {
		out = append(out, entry{AID: aid, KEL: hubapi.B64(others[aid])})
	}
	w.Header().Set("Cache-Control", "max-age=300")
	fakeHubJSON(w, http.StatusOK, map[string]any{"hubs": out})
}

// setHubKEL has the hub list kel as the key history of the hub aid in GET
// /hub/kels (nil removes it).
func (h *fakeHub) setHubKEL(aid string, kel []identity.SignedEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hubKELs == nil {
		h.hubKELs = map[string][]byte{}
	}
	if kel == nil {
		delete(h.hubKELs, aid)
		return
	}
	b, err := identity.MarshalKEL(kel)
	if err != nil {
		panic(err)
	}
	h.hubKELs[aid] = b
}

// hubKELReadCount is how many times GET /hub/kels was read.
func (h *fakeHub) hubKELReadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hubKELReads
}

// grant credits an account, standing in for the admin grant and the
// registration allowance the real hub has.
func (h *fakeHub) grant(aid string, amount uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.balance[aid] += amount
	h.entryLocked(aid, "grant", int64(amount), "registration grant")
}

// entryLocked appends a ledger entry in the real hub's shape (credit_entry:
// a signed delta, the reason — the authorization id for a settlement — and
// the time), plus kind, which only this fake has, for debitsOn. h.mu held.
func (h *fakeHub) entryLocked(aid, kind string, delta int64, reason string) {
	h.entries[aid] = append(h.entries[aid], map[string]any{
		"delta": delta, "reason": reason, "at": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind})
}

func balanceOf(url, aid string) uint64 {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.balance[aid]
}

// hSettle is the facilitator: verify the payer's signature over the
// authorization, move the credit, and sign a receipt saying it did.
//
// It verifies rather than trusting, unlike the rest of this fake, because
// verification is the property under test — a settlement that took an
// unsigned authorization would let the paid-work test pass with the
// signature stripped out.
func (h *fakeHub) hSettle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, payment.SettlementResponse{Success: false, ErrorReason: payment.ReasonMalformed})
		return
	}
	h.mu.Lock()
	h.settleBodies = append(h.settleBodies, body)
	fault := ""
	if len(h.settleFaults) > 0 {
		fault, h.settleFaults = h.settleFaults[0], h.settleFaults[1:]
	}
	h.mu.Unlock()
	drop := func() {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	switch fault {
	case "drop":
		drop()
		return
	case "pending":
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false,
			ErrorReason: payment.ReasonSettlementPending})
		return
	case "foreign-receipt":
		h.mu.Lock()
		var tx, rec string
		for id, r := range h.settledReceipt {
			tx, rec = h.settled[id], r
			break
		}
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: true, Transaction: tx,
			Extensions: map[string]any{payment.ExtReceipt: rec, payment.ExtReplayed: true}})
		return
	}
	// The real hub's request shape (payment.FacilitatorRequest): without
	// paymentRequirements it refuses, as ANetHub readFacilitatorRequest does.
	var req payment.FacilitatorRequest
	if err := json.Unmarshal(body, &req); err != nil || req.PaymentPayload == nil {
		fakeHubJSON(w, http.StatusBadRequest, payment.SettlementResponse{
			Success: false, ErrorReason: payment.ReasonMalformed})
		return
	}
	if req.PaymentRequirements == nil {
		fakeHubJSON(w, http.StatusBadRequest, payment.SettlementResponse{
			Success: false, ErrorReason: payment.ReasonInvalidRequirements})
		return
	}
	pp, want := req.PaymentPayload, req.PaymentRequirements
	encoded, _ := pp.Payload["authorization"].(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: payment.ReasonMalformed})
		return
	}
	auth, err := payment.UnmarshalAuthorization(raw)
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: payment.ReasonMalformed})
		return
	}
	// ANetHub refusedSettlement: the refused authorization's id is the
	// transaction, with its payer and amount. The daemon moves the id to
	// extensions["anet.auth_id"] before a receipt list carries it (Q18).
	refusedID, _ := auth.ID()
	refuse := func(reason string) {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false, ErrorReason: reason,
			Network: auth.Network, Transaction: refusedID, Payer: auth.Payer, Amount: payment.Amount(auth.Amount)})
	}
	// ANetHub CheckRequirements, term for term.
	wantAmount, aerr := payment.ParseAmount(want.Amount)
	accepted, perr := payment.ParseAmount(pp.Accepted.Amount)
	switch {
	case aerr != nil || want.PayTo == "" || want.Network == "":
		refuse(payment.ReasonInvalidRequirements)
		return
	case want.Scheme != payment.SchemeCredit || pp.Accepted.Scheme != want.Scheme:
		refuse(payment.ReasonUnsupportedScheme)
		return
	case pp.Accepted.Network != want.Network || auth.Network != want.Network:
		refuse(payment.ReasonNetworkMismatch)
		return
	case auth.PayTo != want.PayTo || pp.Accepted.PayTo != auth.PayTo:
		refuse(payment.ReasonPayeeMismatch)
		return
	case perr != nil || accepted != auth.Amount || auth.Amount < wantAmount:
		refuse(payment.ReasonInvalidAmount)
		return
	}
	h.mu.Lock()
	payer := h.agents[auth.Payer]
	var payerKEL []byte
	if payer != nil {
		payerKEL = payer.kel
	}
	h.mu.Unlock()
	if payer == nil {
		refuse(payment.ReasonUnknownPayer)
		return
	}
	kel, err := identity.UnmarshalKEL(payerKEL)
	if err != nil {
		refuse(payment.ReasonSettlementFailed)
		return
	}
	authID, err := auth.ID()
	if err != nil {
		refuse(payment.ReasonMalformed)
		return
	}
	// A settled authorization answers with its original receipt at any
	// time, before the window is checked (§8.5).
	h.mu.Lock()
	if tx, done := h.settled[authID]; done {
		rec := h.settledReceipt[authID]
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: true, Transaction: tx, Network: auth.Network, Payer: auth.Payer,
			Amount:     payment.Amount(auth.Amount),
			Extensions: map[string]any{payment.ExtReceipt: rec, payment.ExtReplayed: true}})
		return
	}
	h.mu.Unlock()
	if err := auth.Verify(kel, time.Now().UnixMilli()); err != nil {
		reason := payment.ReasonInvalidSignature
		if errors.Is(err, payment.ErrExpired) || errors.Is(err, payment.ErrBadWindow) {
			reason = payment.ReasonExpiredPayment
		}
		refuse(reason)
		return
	}
	h.mu.Lock()
	bindKey := auth.Payer + "\x00" + auth.InteractionID
	if prior, taken := h.bindings[bindKey]; taken && auth.InteractionID != "" && prior != authID {
		tx := h.settled[prior]
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false,
			ErrorReason: payment.ReasonDuplicateBinding, Network: auth.Network, Transaction: authID,
			Payer: auth.Payer, Amount: payment.Amount(auth.Amount),
			Extensions: map[string]any{payment.ExtOriginalTransaction: tx}})
		return
	}
	if h.balance[auth.Payer] < auth.Amount {
		h.mu.Unlock()
		refuse(payment.ReasonInsufficientFunds)
		return
	}
	h.balance[auth.Payer] -= auth.Amount
	h.balance[auth.PayTo] += auth.Amount
	// The anet-credit scheme's transaction is the authorization id, as the
	// real hub answers; a requester holds a settlement to it (F11).
	tx := authID
	h.settled[authID] = tx
	if auth.InteractionID != "" {
		h.bindings[bindKey] = authID
	}
	h.entryLocked(auth.Payer, "debit", -int64(auth.Amount), tx)
	h.entryLocked(auth.PayTo, "credit", int64(auth.Amount), tx)
	h.mu.Unlock()

	rec := &payment.Receipt{
		AuthID: authID, Payer: auth.Payer, PayTo: auth.PayTo,
		Amount: auth.Amount, Network: auth.Network, SettleAt: time.Now().UnixMilli(),
	}
	if err := rec.Sign(h.self); err != nil {
		refuse(payment.ReasonSettlementFailed)
		return
	}
	recRaw, err := rec.Marshal()
	if err != nil {
		refuse(payment.ReasonSettlementFailed)
		return
	}
	recB64 := base64.StdEncoding.EncodeToString(recRaw)
	h.mu.Lock()
	h.settledReceipt[authID] = recB64
	h.mu.Unlock()
	switch fault {
	case "settle-then-drop":
		drop()
		return
	case "settle-then-pending":
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false,
			ErrorReason: payment.ReasonSettlementPending})
		return
	}
	fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
		Success: true, Transaction: tx, Network: auth.Network, Payer: auth.Payer,
		Amount:     payment.Amount(auth.Amount),
		Extensions: map[string]any{payment.ExtReceipt: recB64},
	})
}

// settleFaultsOn queues faults for the next settle calls of the hub at url.
func settleFaultsOn(url string, faults ...string) {
	v, _ := hubsByURL.Load(url)
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settleFaults = append(h.settleFaults, faults...)
}

// settleBodiesOn returns the /x402/settle bodies the hub at url received.
func settleBodiesOn(url string) [][]byte {
	v, _ := hubsByURL.Load(url)
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.settleBodies...)
}

// debitsOn counts the debits the hub at url made from aid.
func debitsOn(url, aid string) int {
	v, _ := hubsByURL.Load(url)
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.entries[aid] {
		if e["kind"] == "debit" {
			n++
		}
	}
	return n
}

func (h *fakeHub) hBalance(w http.ResponseWriter, r *http.Request) {
	// The account holder only (relayauth v2), as the real hub serves it.
	if who := h.verifyAuth(w, r, nil, relayauth.ActionBalance, nil); who != r.PathValue("aid") {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: not the account holder"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// "credits", the field the real hub uses. A fake answering a
	// different key is a fake that certifies a wire the system does not
	// have — which is exactly how the daemon shipped reading a balance of
	// zero off a funded account.
	fakeHubJSON(w, http.StatusOK, map[string]any{
		"aid": r.PathValue("aid"), "credits": h.balance[r.PathValue("aid")]})
}

func (h *fakeHub) hLedgerRead(w http.ResponseWriter, r *http.Request) {
	if who := h.verifyAuth(w, r, nil, relayauth.ActionLedger, nil); who != r.PathValue("aid") {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: not the account holder"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	es := h.entries[r.PathValue("aid")]
	if es == nil {
		es = []map[string]any{}
	}
	var sum int64
	for _, e := range es {
		sum += e["delta"].(int64)
	}
	// The real hub's page shape: entries, and total and sum over the
	// whole account (the fake serves every entry, so never truncated).
	fakeHubJSON(w, http.StatusOK, map[string]any{"aid": r.PathValue("aid"), "entries": es,
		"total": len(es), "sum": sum, "returned": len(es)})
}

// hRedemptions lists an account's withdrawals, newest first, to the
// account holder only (relayauth v2, action "redemptions"), as ANetHub
// hRedemptions does.
func (h *fakeHub) hRedemptions(w http.ResponseWriter, r *http.Request) {
	if who := h.verifyAuth(w, r, nil, relayauth.ActionRedemptions, nil); who != r.PathValue("aid") {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: not the account holder"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	all := h.redemptions[r.PathValue("aid")]
	out := make([]map[string]any, 0, len(all))
	var sum uint64
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
		sum += all[i]["amount"].(uint64)
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"redemptions": out, "total": len(all), "sum": sum,
		"returned": len(out)})
}

// hRedeem takes credit out of circulation the way ANetHub Store.Redeem
// does: a payment authorization signed by the account holder to the hub,
// settled once (a second presentation answers with the first record), one
// settlement per binding ("redeem:"+reference), a ledger entry on both
// rows under the authorization id, and a receipt signed by the hub.
func (h *fakeHub) hRedeem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PaymentPayload *payment.PaymentPayload `json:"paymentPayload"`
		Reference      string                  `json:"reference"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.PaymentPayload == nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": payment.ReasonMalformed})
		return
	}
	refuse := func(reason string) {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": reason})
	}
	encoded, _ := req.PaymentPayload.Payload["authorization"].(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		refuse(payment.ReasonMalformed)
		return
	}
	auth, err := payment.UnmarshalAuthorization(raw)
	if err != nil {
		refuse(payment.ReasonMalformed)
		return
	}
	authID, err := auth.ID()
	if err != nil {
		refuse(payment.ReasonMalformed)
		return
	}
	h.mu.Lock()
	payer := h.agents[auth.Payer]
	// Copied under the lock: a registration of the payer rewrites its
	// kel meanwhile (hRegister), and an unlocked read raced with it.
	var payerKEL []byte
	if payer != nil {
		payerKEL = payer.kel
	}
	for _, prior := range h.redemptions[auth.Payer] {
		if prior["auth_id"] == authID {
			h.mu.Unlock()
			fakeHubJSON(w, http.StatusOK, prior)
			return
		}
	}
	h.mu.Unlock()
	if payer == nil {
		refuse(payment.ReasonUnknownPayer)
		return
	}
	if auth.PayTo != h.self.AID() {
		refuse(payment.ReasonPayeeMismatch)
		return
	}
	kel, err := identity.UnmarshalKEL(payerKEL)
	if err != nil || auth.Verify(kel, time.Now().UnixMilli()) != nil {
		refuse(payment.ReasonInvalidSignature)
		return
	}
	rec := &payment.Receipt{AuthID: authID, Payer: auth.Payer, PayTo: auth.PayTo,
		Amount: auth.Amount, Network: auth.Network, SettleAt: time.Now().UnixMilli()}
	if err := rec.Sign(h.self); err != nil {
		refuse(payment.ReasonSettlementFailed)
		return
	}
	recRaw, err := rec.Marshal()
	if err != nil {
		refuse(payment.ReasonSettlementFailed)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	bindKey := auth.Payer + "\x00" + auth.InteractionID
	if prior, taken := h.bindings[bindKey]; taken && auth.InteractionID != "" && prior != authID {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": payment.ReasonDuplicateBinding})
		return
	}
	if h.balance[auth.Payer] < auth.Amount {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": payment.ReasonInsufficientFunds})
		return
	}
	h.balance[auth.Payer] -= auth.Amount
	h.balance[auth.PayTo] += auth.Amount
	h.settled[authID] = authID
	if auth.InteractionID != "" {
		h.bindings[bindKey] = authID
	}
	h.entryLocked(auth.Payer, "redeem", -int64(auth.Amount), authID)
	h.entryLocked(auth.PayTo, "redeem", int64(auth.Amount), authID)
	out := map[string]any{"auth_id": authID, "aid": auth.Payer, "amount": auth.Amount,
		"reference": req.Reference, "at": time.Now().UTC().Format(time.RFC3339Nano),
		"receipt": base64.StdEncoding.EncodeToString(recRaw)}
	if h.redemptions == nil {
		h.redemptions = map[string][]map[string]any{}
	}
	h.redemptions[auth.Payer] = append(h.redemptions[auth.Payer], out)
	fakeHubJSON(w, http.StatusOK, out)
}

func (h *fakeHub) hAgentKEL(w http.ResponseWriter, r *http.Request) {
	h.serveAgentKEL(w, r.PathValue("aid"))
}

func (h *fakeHub) serveAgentKEL(w http.ResponseWriter, aid string) {
	h.mu.Lock()
	a := h.agents[aid]
	var kel []byte
	if a != nil {
		kel = a.kel
	}
	h.mu.Unlock()
	if a == nil {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "unknown agent"})
		return
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{
		"aid": aid, "kel": hubapi.B64(kel), "registered": true})
}

// grantOn credits an account on the fake hub behind url.
func grantOn(url, aid string, amount uint64) {
	if v, ok := hubsByURL.Load(url); ok {
		v.(*fakeHub).grant(aid, amount)
	}
}

func hubAIDOf(url string) string {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return ""
	}
	return v.(*fakeHub).self.AID()
}

func hubKELOf(t *testing.T, url string) []identity.SignedEvent {
	t.Helper()
	v, ok := hubsByURL.Load(url)
	if !ok {
		t.Fatal("no fake hub at " + url)
	}
	return v.(*fakeHub).self.KEL()
}

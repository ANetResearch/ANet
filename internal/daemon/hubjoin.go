package daemon

// hubjoin.go is the node side of joining a hub (v0.3.0 LEC §4.3.10, AD-1 to
// AD-9): one join loop per node that asks the hub whether it would take this
// AID (check_only), lets the identity area designate the hub as the
// identity's witness (the rot hook), registers, and keeps the registration
// current afterwards. The join state it keeps is what `anet status`, MCP
// node_status and the send gate read (AD-2, AD-6).
//
// There is no invitation and no secret anywhere in it: a hub that admits by
// AID answers admission_required naming the AID, and the operator runs
// `anet-hub admit <AID>`.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/regpow"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// The join states: hub.state in /status and /hub-register (LEC AD-2). The
// value depends only on what the hub answered; whether the identity has a
// recovery passphrase is not a join state.
const (
	// JoinNone: no hub is configured.
	JoinNone = "none"
	// JoinPendingAdmission: the hub answered admission_required; the loop
	// retries in the background and finishes on its own once the operator
	// admits the AID.
	JoinPendingAdmission = "pending_admission"
	// JoinRetrying: a temporary failure before the first registration
	// finished; the loop retries in the background.
	JoinRetrying = "retrying"
	// JoinRefused: an answer that will not change by retrying; the loop
	// stops until `anet hub-register` runs again or the daemon restarts.
	JoinRefused = "refused"
	// JoinRegistered: the hub accepted the registration.
	JoinRegistered = "registered"
)

// JoinHooks are where the identity area hooks into a first join (LEC AD-4;
// KEL D-12, D-13). The join loop calls Prepare only after the hub confirmed
// it would take this AID (check_only answered admitted or open), and Adopt
// once after the hub accepted the registration Prepare's KEL was sent in.
// An identity that already has a witness registers without them.
//
// KEL-9 implements them (identity_ops.go); fakeJoinHooks stands in for them
// in tests.
type JoinHooks interface {
	// Prepare builds the rot that designates hubAID as the identity's
	// witness, saves it before it returns (reusing one already saved, so
	// a retry never builds a second rot for the same hub and key event),
	// and returns the KEL to register with (identity.MarshalKEL bytes).
	Prepare(ctx context.Context, hubAID string) (kel []byte, err error)
	// Adopt takes the registration the hub accepted: the rot becomes the
	// identity's accepted tip. It is idempotent: the loop may call it
	// again for the same answer after a crash.
	Adopt(ctx context.Context, resp RegisterResult) error
}

// RegisterResult is a /register answer the hub accepted (200, core
// §2.12.2), with what was sent to get it: what Adopt is given.
type RegisterResult struct {
	// Hub is the hub's URL and HubAID its AID.
	Hub    string
	HubAID string
	// KEL is the key history that was registered (identity.MarshalKEL
	// bytes): Prepare's, or the node's own when its identity already had
	// this hub as its witness.
	KEL []byte
	// The answer's fields (core §2.12.2).
	AID        string
	Status     string
	KeysStatus string
	KeysError  string
	CardStatus string
	CardError  string
	// ProbationUntil (unix ms) and Trial are there only for an account
	// that came in through open registration or a sign-up window.
	ProbationUntil *uint64
	Trial          string
	// KeyStateNotice is key_state_notice as the hub sent it (base64url of
	// the notice's bytes) and KeyStateNoticeError key_state_notice_error;
	// the identity area reads them (KEL D-50).
	KeyStateNotice      string
	KeyStateNoticeError string
	// Raw is the whole answer body, for the fields this struct does not
	// name.
	Raw json.RawMessage
}

// WitnessRot is hub_join.json's witness_rot (KEL §2.9): the commitment the
// witness designation rot will carry and the hub it designates. Public
// material only: next_digest commits to a key nobody can derive from what
// the data directory holds once the recovery passphrase is set (KEL D-10).
type WitnessRot struct {
	// NextDigest is the 32-byte next_digest (standard base64 in the file).
	NextDigest []byte `json:"next_digest"`
	// HubAID is the AID of the hub the rot designates.
	HubAID string `json:"hub_aid"`
}

// --- hub_join.json (LEC §3.3) ---

// joinRecordVersion is hub_join.json's "v".
const joinRecordVersion = 1

// joinRecord is hub_join.json: the join state of the configured hub, kept
// across restarts so a node that was waiting for admission goes on waiting,
// and one that registered stays registered while its hub is away (AD-5).
//
// Besides the members of LEC §3.3 it keeps what the state's sentence shows
// (why, version, probation_until, hub_error) and when this node first
// registered there (registered_at): after that, a temporary failure no
// longer changes the state (AD-5, P-JOIN (d)).
type joinRecord struct {
	V      int    `json:"v"`
	Hub    string `json:"hub"`
	HubAID string `json:"hub_aid"`
	State  string `json:"state"`
	// Reason is the state's sentence key (ux §1.4 F, E2).
	Reason string `json:"reason"`
	// Why is hub_retrying's {why} (a2ashape phrase set hub_why).
	Why string `json:"why,omitempty"`
	// HubCode is the hub's code behind hub_refused; HubError its sentence,
	// kept only when the code is not in this node's reason table
	// (CORE-K20).
	HubCode  string `json:"hub_code,omitempty"`
	HubError string `json:"hub_error,omitempty"`
	// Version is wire_too_old's {version}: the anet release the hub needs.
	Version string `json:"version,omitempty"`
	// Since is when State or Reason last changed (unix ms).
	Since int64 `json:"since"`
	// Attempts counts the attempts that failed in a row; the next one waits
	// joinBackoff(Attempts, Retry-After).
	Attempts int `json:"attempts"`
	// NextAttemptAt is when the loop tries again (unix ms); 0: it does not.
	NextAttemptAt int64 `json:"next_attempt_at"`
	// RegisteredAt is when this node first registered at this hub (unix
	// ms); 0: not yet.
	RegisteredAt int64 `json:"registered_at,omitempty"`
	// ProbationUntil is the end of the new account's probation (unix ms),
	// from the hub's answer; 0: none.
	ProbationUntil uint64 `json:"probation_until,omitempty"`
	// WitnessRot is the identity area's material for the witness
	// designation (KEL D-10, D-11); KEL D-12 removes it after Adopt.
	WitnessRot *WitnessRot `json:"witness_rot,omitempty"`
}

// hubJoinPath is <data>/hub_join.json.
func (l Layout) hubJoinPath() string { return filepath.Join(l.Root, "hub_join.json") }

// maxJoinRecord bounds hub_join.json: a few hundred bytes in practice.
const maxJoinRecord = 64 << 10

// errJoinRecord is a hub_join.json this binary cannot use.
var errJoinRecord = errors.New("anet: hub_join.json is not valid")

// decodeJoinRecord reads hub_join.json strictly: one JSON object, only the
// members above, of version 1, each value of its form. A file that does not
// pass is not used (the loop starts over from config.json's hub_url); it is
// never half-read.
func decodeJoinRecord(b []byte) (*joinRecord, error) {
	if len(b) > maxJoinRecord {
		return nil, fmt.Errorf("%w: %d bytes", errJoinRecord, len(b))
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r joinRecord
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: %v", errJoinRecord, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: data after the object", errJoinRecord)
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	return &r, nil
}

// check reports a record whose values are not of their form.
func (r *joinRecord) check() error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", errJoinRecord, what) }
	switch {
	case r.V != joinRecordVersion:
		return bad("v")
	case checkHubURL(r.Hub) != nil:
		return bad("hub")
	case r.HubAID != "" && !validAgentID(r.HubAID):
		return bad("hub_aid")
	case !joinStates[r.State]:
		return bad("state")
	case !joinReasons[r.Reason]:
		return bad("reason")
	case r.Why != "" && !joinWhy[r.Why]:
		return bad("why")
	case r.HubCode != "" && !reasonCodeRe.MatchString(r.HubCode):
		return bad("hub_code")
	case r.Version != "" && !validWireTooOldVersion(r.Version):
		return bad("version")
	case len(r.HubError) > maxHubErrorBytes:
		return bad("hub_error")
	case r.Since < 0 || r.Attempts < 0 || r.NextAttemptAt < 0 || r.RegisteredAt < 0 || r.ProbationUntil > math.MaxInt64:
		return bad("time")
	}
	if w := r.WitnessRot; w != nil && (len(w.NextDigest) != nextDigestLen || !validAgentID(w.HubAID)) {
		return bad("witness_rot")
	}
	return nil
}

// loadJoinRecord is hub_join.json, or nil when there is none.
func loadJoinRecord(l Layout) (*joinRecord, error) {
	b, err := os.ReadFile(l.hubJoinPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeJoinRecord(b)
}

// saveJoinRecord writes hub_join.json (0600, atomically).
func saveJoinRecord(l Layout, r *joinRecord) error {
	if err := l.EnsureRoot(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(l.hubJoinPath(), append(b, '\n'), 0o600)
}

// removeJoinRecord deletes hub_join.json; one that is not there is gone.
func removeJoinRecord(l Layout) error {
	if err := os.Remove(l.hubJoinPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// nextDigestLen is the length of next_digest (SHA-256, KEL §2.3).
const nextDigestLen = 32

// maxHubErrorBytes bounds the hub's sentence kept beside hub_refused.
const maxHubErrorBytes = 600

var (
	// reasonCodeRe is a code's literal form (core CORE-K19).
	reasonCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)
	// anetVersionRe is an anet release number as a hub names it in
	// wire_too_old's required_anet.
	anetVersionRe = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-[0-9A-Za-z.]{1,20})?$`)
)

// joinStates are the states a record may hold; JoinNone is no record.
var joinStates = map[string]bool{JoinPendingAdmission: true, JoinRetrying: true, JoinRefused: true, JoinRegistered: true}

// The sentence keys of the join states (ux §1.4 F, E2; LEC AD-2).
const (
	reasonRegistered      = "registered"
	reasonProbation       = "registered.probation"
	reasonPending         = "hub_pending_admission"
	reasonRetrying        = "hub_retrying"
	reasonRetryingQuota   = "hub_retrying.quota"
	reasonHubRefused      = "hub_refused"
	reasonClockSkew       = "clock_skew"
	reasonDeactivated     = "identity_deactivated"
	reasonStopped         = "identity_stopped"
	reasonSwitch          = "hub_switch_requires_rotation"
	reasonWireTooOld      = "wire_too_old"
	reasonWireTooNew      = "wire_too_new"
	reasonNoHub           = "no_hub_yet"
	reasonHubUnreachable  = "hub_unreachable"
	reasonInviteRemoved   = "invite_removed"
	reasonConfigInvalid   = "config_invalid"
	reasonWitnessHubLeave = "witness_hub_leave"
)

// joinReasons are the sentence keys a record may hold.
var joinReasons = map[string]bool{
	reasonRegistered: true, reasonProbation: true, reasonPending: true, reasonRetrying: true,
	reasonRetryingQuota: true, reasonHubRefused: true, reasonClockSkew: true, reasonDeactivated: true,
	reasonStopped: true, reasonSwitch: true, reasonWireTooOld: true, reasonWireTooNew: true,
}

// The {why} of hub_retrying: the keys of a2ashape's hub_why phrase set.
const (
	whyUnreachable = "unreachable" // no HTTP answer
	whyInternal    = "internal"    // 5xx, or an answer that is not the contract's
	whyPow         = "pow"         // the proof of work could not be done or was not accepted
	whyBusy        = "busy"        // 429, or a signature the hub took for a replay
	whyLocal       = "local"       // this node could not prepare the registration
)

var joinWhy = map[string]bool{whyUnreachable: true, whyInternal: true, whyPow: true, whyBusy: true, whyLocal: true}

// joinRefusals maps the hub's final answers to the user's codes (ux UX-R6);
// any other 4xx is hub_refused with the hub's code.
var joinRefusals = map[string]string{
	"aid_deactivated":      reasonDeactivated,
	"kel_fork":             reasonStopped,
	"kel_rollback":         reasonStopped,
	"witness_not_this_hub": reasonSwitch,
	"wire_too_old":         reasonWireTooOld,
	"wire_too_new":         reasonWireTooNew,
}

// checkHubURL is nil for a hub address anet can join: an absolute http://
// or https:// URL with a host and nothing after the path (LEC §2.3.2).
func checkHubURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return err
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("not http:// or https://")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("no host")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return errors.New("more than an address")
	case strings.ContainsAny(s, " \t\r\n"):
		return errors.New("white space")
	}
	return nil
}

// --- the retry rhythm (AD-3) ---

// joinSchedule is the wait after the n-th failed attempt in a row; after
// the last, every 10 minutes.
var joinSchedule = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
	5 * time.Minute, 10 * time.Minute}

const (
	// joinBackoffMax is the longest wait of the schedule: once admitted, a
	// node finishes joining at most this long after (AD-3).
	joinBackoffMax = 10 * time.Minute
	// joinRetryAfterMax bounds a hub's Retry-After: registration_quota
	// names the next UTC midnight, never more than a day away, and a hub
	// asking for longer would park the join for good.
	joinRetryAfterMax = 24 * time.Hour
)

// joinBackoff is the wait before the next attempt after attempts failed in
// a row (AD-3, P-BACKOFF): 15 s, 30 s, 1, 2, 5 and 10 minutes, then every
// 10 minutes; never less than the hub's Retry-After (bounded to a day).
func joinBackoff(attempts int, retryAfter time.Duration) time.Duration {
	d := joinBackoffMax
	if i := attempts - 1; i < len(joinSchedule) {
		if i < 0 {
			i = 0
		}
		d = joinSchedule[i]
	}
	if retryAfter > joinRetryAfterMax {
		retryAfter = joinRetryAfterMax
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// --- reading the hub's answers (HN-1 to HN-5, ux UX-R6) ---

// hubReply is one answer of the hub to a join request, or none.
type hubReply struct {
	// Status is the HTTP status; 0 when no HTTP answer arrived (DNS,
	// connection, TLS, or the attempt's time ran out).
	Status int
	// RetryAfter is the Retry-After header as sent, and Wire the hub's
	// X-ANet-Wire.
	RetryAfter string
	Wire       string
	Body       []byte
	// Err is why there is no answer, when Status is 0.
	Err error
	// Local: this node failed before anything was sent (Status 0).
	Local bool
}

// joinPhase is the request a hub answer is to.
type joinPhase uint8

const (
	// phaseCheck: the check_only question (HN-2).
	phaseCheck joinPhase = iota + 1
	// phaseCheckPow: the question asked again with a proof of work.
	phaseCheckPow
	// phaseRegister: the registration (HN-3).
	phaseRegister
	// phaseRegisterPow: the registration sent again with a proof of work.
	phaseRegisterPow
)

func (p joinPhase) check() bool { return p == phaseCheck || p == phaseCheckPow }
func (p joinPhase) pow() bool   { return p == phaseCheckPow || p == phaseRegisterPow }

// answerKind is what a hub answer means to the join loop.
type answerKind uint8

const (
	// ansAdmitted and ansOpen answer check_only: the hub would take this AID.
	ansAdmitted answerKind = iota + 1
	ansOpen
	// ansRegistered: the registration was accepted.
	ansRegistered
	// ansAdmissionRequired: the operator has to admit the AID first.
	ansAdmissionRequired
	// ansPowRequired: a proof of work is needed (challenge, if one this
	// node can solve).
	ansPowRequired
	// ansTransient: try again later (why).
	ansTransient
	// ansQuota: registration_quota, today's places of the network are spent.
	ansQuota
	// ansClockSkew: the hub refused the time of the request.
	ansClockSkew
	// ansRefused: a final answer (refusal is the user's code).
	ansRefused
)

// powChallenge is pow_required's challenge (hub-payment-privacy §2.2).
type powChallenge struct {
	Seed     [32]byte
	Bits     int
	NotAfter uint64
}

// powProof is /register's pow.
type powProof struct {
	Seed  [32]byte
	Nonce []byte
}

// joinAnswer is a hub answer as the join loop reads it: by the reason code
// alone, never by the hub's sentence (HN-1).
type joinAnswer struct {
	phase joinPhase
	kind  answerKind
	// code is the hub's reason code (only in a code's form).
	code string
	// refusal is the user's code of an ansRefused.
	refusal string
	// why is the {why} of an ansTransient.
	why string
	// hubError is the hub's sentence, kept for hub_refused with a code
	// outside this node's reason table (CORE-K20).
	hubError string
	// retryAfter is the hub's Retry-After.
	retryAfter time.Duration
	// challenge is pow_required's challenge when this node can solve it.
	challenge *powChallenge
	// version is wire_too_old's required_anet.
	version string
	// reg is the accepted registration (ansRegistered).
	reg hubapi.RegisterResponse
	// probationUntil and trial are the registration's.
	probationUntil *uint64
	trial          string
	notice         string
	noticeError    string
}

// transientAnswer is "try again later" for why.
func transientAnswer(phase joinPhase, why string) joinAnswer {
	return joinAnswer{phase: phase, kind: ansTransient, why: why}
}

// classifyHubReply reads one hub answer (HN-1, HN-4, ux UX-R6). It is a
// pure function of the answer: the reason code decides, the hub's sentence
// is kept only to be shown beside hub_refused when this node does not know
// the code. An answer outside the contract (a 2xx that does not parse, a
// status no rule names) is a temporary failure: the hub misbehaving now is
// no reason to give up for good.
func classifyHubReply(phase joinPhase, r hubReply) joinAnswer {
	a := joinAnswer{phase: phase}
	if r.Status == 0 {
		if r.Local {
			return transientAnswer(phase, whyLocal)
		}
		return transientAnswer(phase, whyUnreachable)
	}
	a.retryAfter = parseRetryAfter(r.RetryAfter)
	if we := hubWireCheck("", r); we != nil && r.Status != 426 {
		// An answer that states another wire, a 2xx included, is a wire
		// refusal, never a success (core CORE-V1).
		a.kind, a.refusal, a.code, a.version = ansRefused, reasonWireTooOld, "wire_too_old", we.version
		if we.hubOlder {
			a.refusal, a.code, a.version = reasonWireTooNew, "wire_too_new", ""
		}
		return a
	}
	if r.Status >= 200 && r.Status < 300 {
		return classifySuccess(a, r.Body)
	}
	var body struct {
		Reason       string          `json:"reason"`
		Error        string          `json:"error"`
		Challenge    json.RawMessage `json:"challenge"`
		RequiredAnet string          `json:"required_anet"`
	}
	_ = json.Unmarshal(r.Body, &body)
	code := ""
	if reasonCodeRe.MatchString(body.Reason) {
		code = body.Reason
	}
	a.code = code
	switch {
	case r.Status == 426:
		// The hub's own version check (core §2.12.1): whatever the body.
		a.kind, a.refusal, a.code = ansRefused, reasonWireTooOld, "wire_too_old"
		if anetVersionRe.MatchString(body.RequiredAnet) {
			a.version = body.RequiredAnet
		}
		return a
	case r.Status >= 500 && r.Status <= 599:
		a.kind, a.why = ansTransient, whyInternal
		return a
	case r.Status == 429:
		if code == "registration_quota" {
			a.kind = ansQuota
		} else {
			a.kind, a.why = ansTransient, whyBusy
		}
		return a
	case r.Status < 400 || r.Status > 499:
		a.kind, a.why = ansTransient, whyInternal
		return a
	}
	switch code {
	case "admission_required":
		a.kind = ansAdmissionRequired
		return a
	case "pow_required":
		a.kind = ansPowRequired
		a.challenge = parseChallenge(body.Challenge)
		return a
	case "kel_ts_out_of_window", "ts_stale":
		// A time the hub refused: the user's part is the clock (ux UX-R6;
		// ts_stale is the same refusal of the signature's time, HPP RC1).
		a.kind = ansClockSkew
		return a
	case "replayed":
		// A signature the hub took for one it had seen: the next attempt is
		// signed afresh.
		a.kind, a.why = ansTransient, whyBusy
		return a
	}
	a.kind = ansRefused
	if u, ok := joinRefusals[code]; ok {
		a.refusal = u
		return a
	}
	a.refusal = reasonHubRefused
	if code == "" {
		// A hub that gave no code in a code's form: its status stands in,
		// so the sentence still names something the operator can look up.
		a.code = "http_" + strconv.Itoa(r.Status)
	}
	if !knownReasonCode(a.code) {
		a.hubError = cleanHubSentence(body.Error)
	}
	return a
}

// classifySuccess reads a 2xx: check_only's {status} or a registration.
func classifySuccess(a joinAnswer, b []byte) joinAnswer {
	if a.phase.check() {
		var c struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &c) == nil {
			switch c.Status {
			case "admitted":
				a.kind = ansAdmitted
				return a
			case "open":
				a.kind = ansOpen
				return a
			}
		}
		return transientAnswer(a.phase, whyInternal)
	}
	var reg struct {
		hubapi.RegisterResponse
		ProbationUntil      *uint64 `json:"probation_until"`
		Trial               string  `json:"trial"`
		KeyStateNotice      string  `json:"key_state_notice"`
		KeyStateNoticeError string  `json:"key_state_notice_error"`
	}
	if json.Unmarshal(b, &reg) != nil || reg.Status != "registered" {
		return transientAnswer(a.phase, whyInternal)
	}
	a.kind, a.reg = ansRegistered, reg.RegisterResponse
	a.probationUntil, a.trial = reg.ProbationUntil, reg.Trial
	a.notice, a.noticeError = reg.KeyStateNotice, reg.KeyStateNoticeError
	return a
}

// parseChallenge is pow_required's challenge, or nil when it is not one
// this node can solve: a 32-byte seed in base64url and 1 to regpow.Bits
// bits (every hub asks for regpow.Bits; Solve refuses more, PAY-1).
func parseChallenge(raw json.RawMessage) *powChallenge {
	var c struct {
		Seed     string `json:"seed"`
		Bits     int    `json:"bits"`
		NotAfter uint64 `json:"not_after"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Bits < 1 || c.Bits > regpow.Bits {
		return nil
	}
	seed, err := base64.RawURLEncoding.Strict().DecodeString(c.Seed)
	if err != nil || len(seed) != 32 {
		return nil
	}
	ch := &powChallenge{Bits: c.Bits, NotAfter: c.NotAfter}
	copy(ch.Seed[:], seed)
	return ch
}

// parseRetryAfter is a Retry-After in seconds; 0 when absent or not a
// number of seconds.
func parseRetryAfter(v string) time.Duration {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if n > int64(joinRetryAfterMax/time.Second) {
		return joinRetryAfterMax
	}
	return time.Duration(n) * time.Second
}

// cleanHubSentence is the hub's English sentence as it may be shown: one
// line of printable text, bounded. The work is bounded too: a hub may
// answer with megabytes of text (its body is read up to maxHubResponse),
// and only a prefix a few times the bound is looked at, so what a hub
// sends cannot keep the join, and the registration lock it holds, busy.
func cleanHubSentence(s string) string {
	if len(s) > 4*maxHubErrorBytes {
		s = s[:4*maxHubErrorBytes] // a rune cut in two becomes U+FFFD below
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxHubErrorBytes {
		end := 0
		for i, r := range s {
			n := utf8.RuneLen(r)
			if i+n > maxHubErrorBytes {
				break
			}
			end = i + n
		}
		s = s[:end]
	}
	return s
}

// --- the join state machine (AD-2, AD-3, AD-5; P-JOIN) ---

// joinAct is what the loop does next after an answer.
type joinAct uint8

const (
	// actWait: this attempt is over; the record says when the next one is.
	actWait joinAct = iota + 1
	// actPrepare: the hub would take this AID; have the identity area
	// prepare the witness designation (Prepare), then register.
	actPrepare
	// actSolve: solve the challenge and send the same request again with
	// the proof of work, once; this is not a failed attempt (HN-2, HN-3).
	actSolve
	// actAdopt: registered; hand the answer to Adopt.
	actAdopt
)

// joinStep is the join state machine: the record after answer a, and what
// to do next. It is a pure function of its arguments (P-JOIN):
//
//   - only an admitted or open answer to check_only leads to Prepare;
//   - registered follows only an accepted registration;
//   - once this node has registered at the hub, a temporary failure leaves
//     the state as it is (a registered node does not stop sending because
//     its hub was away for a minute, AD-5): retrying is only ever a state
//     before the first registration;
//   - a final answer stops the loop until `anet hub-register` or a
//     restart; every other answer schedules the next attempt by
//     joinBackoff.
func joinStep(r joinRecord, a joinAnswer, now time.Time) (joinRecord, joinAct) {
	switch a.kind {
	case ansAdmitted, ansOpen:
		if a.phase.check() {
			return r, actPrepare
		}
		a = transientAnswer(a.phase, whyInternal)
	case ansRegistered:
		if !a.phase.check() {
			reason := reasonRegistered
			r.ProbationUntil = 0
			if p := a.probationUntil; p != nil && *p > uint64(now.UnixMilli()) && *p <= math.MaxInt64 {
				reason, r.ProbationUntil = reasonProbation, *p
			}
			r.set(JoinRegistered, reason, now)
			r.Why, r.HubCode, r.HubError, r.Version = "", "", "", ""
			r.Attempts, r.NextAttemptAt = 0, 0
			if r.RegisteredAt == 0 {
				r.RegisteredAt = now.UnixMilli()
			}
			return r, actAdopt
		}
		a = transientAnswer(a.phase, whyInternal)
	case ansPowRequired:
		if !a.phase.pow() && a.challenge != nil {
			return r, actSolve
		}
		// The proof of work was asked for again, or for more than a node
		// does: a temporary failure (HN-3).
		a = joinAnswer{phase: a.phase, kind: ansTransient, why: whyPow, retryAfter: a.retryAfter}
	}
	r.Attempts++
	switch a.kind {
	case ansAdmissionRequired:
		r.set(JoinPendingAdmission, reasonPending, now)
		r.Why, r.HubCode, r.HubError, r.Version = "", "", "", ""
	case ansRefused:
		r.set(JoinRefused, a.refusal, now)
		r.Why, r.HubCode, r.HubError, r.Version = "", "", "", ""
		switch a.refusal {
		case reasonHubRefused:
			r.HubCode, r.HubError = a.code, a.hubError
		case reasonWireTooOld:
			r.Version = a.version
		}
		r.NextAttemptAt = 0
		return r, actWait
	default: // ansTransient, ansQuota, ansClockSkew
		if r.RegisteredAt == 0 {
			reason, why := reasonRetrying, a.why
			switch a.kind {
			case ansQuota:
				reason, why = reasonRetryingQuota, ""
			case ansClockSkew:
				reason, why = reasonClockSkew, ""
			}
			r.set(JoinRetrying, reason, now)
			r.Why, r.HubCode, r.HubError, r.Version = why, "", "", ""
		}
	}
	r.NextAttemptAt = now.Add(joinBackoff(r.Attempts, a.retryAfter)).UnixMilli()
	return r, actWait
}

// set changes the state and its sentence, and Since when either changed.
func (r *joinRecord) set(state, reason string, now time.Time) {
	if r.State != state || r.Reason != reason {
		r.Since = now.UnixMilli()
	}
	r.State, r.Reason = state, reason
}

// --- one join attempt (AD-4) ---

// joinHubAPI is the hub as one join attempt talks to it: the daemon's HTTP
// client, or a script in tests.
type joinHubAPI interface {
	// checkOnly asks /register with check_only (HN-2), with a proof of
	// work when pow is set.
	checkOnly(ctx context.Context, pow *powProof) hubReply
	// register registers with kel (identity.MarshalKEL bytes), with a
	// proof of work when pow is set (HN-3).
	register(ctx context.Context, kel []byte, pow *powProof) hubReply
}

// preparedKELs remembers, for the life of the process, the KEL Prepare
// returned per (hub AID, tip seq), so that the retries of one join ask for
// the witness designation once (P-JOIN (b)); KEL-9 keeps the rot itself on
// disk across restarts (pending.event).
type preparedKELs struct {
	mu  sync.Mutex
	kel map[string][]byte
}

func preparedKey(hubAID string, tip uint64) string { return hubAID + "|" + strconv.FormatUint(tip, 10) }

func (p *preparedKELs) get(k string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.kel[k]
	return b, ok
}

func (p *preparedKELs) put(k string, b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.kel == nil {
		p.kel = map[string][]byte{}
	}
	p.kel[k] = b
}

func (p *preparedKELs) drop(k string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.kel, k)
}

// joinEnv is what one attempt needs besides the record.
type joinEnv struct {
	hub   joinHubAPI
	hooks JoinHooks
	// hubURL, hubAID and aid name the two ends.
	hubURL, hubAID, aid string
	// witnessed: the identity's witness is already this hub, so it
	// registers its own KEL without the hooks (AD-4, action update).
	witnessed bool
	// ownKEL is the identity's KEL as it stands (MarshalKEL bytes).
	ownKEL []byte
	// tip is the seq of the identity's accepted tip.
	tip      uint64
	prepared *preparedKELs
	// solve finds a proof of work for a challenge (regpow.Solve in the
	// daemon); its context is the attempt's (60 s).
	solve func(ctx context.Context, ch powChallenge) ([]byte, error)
	now   func() time.Time
	// logf reports what the user does not need to act on (nil: nothing).
	logf func(format string, a ...any)
}

func (e joinEnv) log(format string, a ...any) {
	if e.logf != nil {
		e.logf(format, a...)
	}
}

// joinOutcome is an attempt's result: the record, and the registration
// the hub accepted, if it did.
type joinOutcome struct {
	rec joinRecord
	reg *RegisterResult
}

// runJoinAttempt makes one attempt (AD-4): for an identity whose witness
// is not yet this hub, check → rot → register; for one whose witness it
// is, register. A pow_required is answered once with a proof of work.
func runJoinAttempt(ctx context.Context, r joinRecord, e joinEnv) joinOutcome {
	now := e.now
	kel := e.ownKEL
	key := preparedKey(e.hubAID, e.tip)
	if !e.witnessed {
		a := classifyHubReply(phaseCheck, e.hub.checkOnly(ctx, nil))
		var act joinAct
		r, act = joinStep(r, a, now())
		if act == actSolve {
			proof, why := solveChallenge(ctx, e, *a.challenge)
			if proof == nil {
				r, _ = joinStep(r, transientAnswer(phaseCheckPow, why), now())
				return joinOutcome{rec: r}
			}
			a = classifyHubReply(phaseCheckPow, e.hub.checkOnly(ctx, proof))
			r, act = joinStep(r, a, now())
		}
		if act != actPrepare {
			return joinOutcome{rec: r}
		}
		var ok bool
		if kel, ok = e.prepared.get(key); !ok {
			var err error
			if kel, err = e.hooks.Prepare(ctx, e.hubAID); err != nil || len(kel) == 0 {
				e.log("anet: could not prepare joining %s: %v", e.hubURL, err)
				r, _ = joinStep(r, transientAnswer(phaseCheck, whyLocal), now())
				return joinOutcome{rec: r}
			}
			e.prepared.put(key, kel)
		}
	}
	a := classifyHubReply(phaseRegister, e.hub.register(ctx, kel, nil))
	r, act := joinStep(r, a, now())
	if act == actSolve {
		proof, why := solveChallenge(ctx, e, *a.challenge)
		if proof == nil {
			r, _ = joinStep(r, transientAnswer(phaseRegisterPow, why), now())
			return joinOutcome{rec: r}
		}
		a = classifyHubReply(phaseRegisterPow, e.hub.register(ctx, kel, proof))
		r, act = joinStep(r, a, now())
	}
	if a.kind == ansClockSkew && !e.witnessed {
		// The rot's time was refused (kel_ts_out_of_window): the identity
		// area signs it again on the next Prepare (KEL D-36).
		e.prepared.drop(key)
	}
	if act != actAdopt {
		return joinOutcome{rec: r}
	}
	res := RegisterResult{Hub: e.hubURL, HubAID: e.hubAID, KEL: kel, AID: a.reg.AID, Status: a.reg.Status,
		KeysStatus: a.reg.KeysStatus, KeysError: a.reg.KeysError, CardStatus: a.reg.CardStatus,
		CardError: a.reg.CardError, ProbationUntil: a.probationUntil, Trial: a.trial,
		KeyStateNotice: a.notice, KeyStateNoticeError: a.noticeError}
	if !e.witnessed {
		if err := e.hooks.Adopt(ctx, res); err != nil {
			// The hub holds the registration; the identity has not taken it
			// yet. The rot stays prepared and the next attempt, soon,
			// registers it again (the hub answers it unchanged) and adopts.
			e.log("anet: %s accepted the registration, but adopting it failed: %v", e.hubURL, err)
			r.Attempts++
			r.NextAttemptAt = now().Add(joinBackoff(r.Attempts, 0)).UnixMilli()
			return joinOutcome{rec: r, reg: &res}
		}
		e.prepared.drop(key)
	}
	return joinOutcome{rec: r, reg: &res}
}

// solveChallenge is a proof of work for ch, or nil and the {why} of the
// failure: a proof of work that could not be found in the attempt's time
// is the proof of work not accepted (HN-3).
func solveChallenge(ctx context.Context, e joinEnv, ch powChallenge) (*powProof, string) {
	nonce, err := e.solve(ctx, ch)
	if err != nil || len(nonce) == 0 {
		e.log("anet: proof of work for %s: %v", e.hubURL, err)
		return nil, whyPow
	}
	return &powProof{Seed: ch.Seed, Nonce: nonce}, ""
}

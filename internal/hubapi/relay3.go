package hubapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/ANetResearch/ANetCore/relayauth"
)

// Relay v3 (core §2.12, sealed-sender §2.6, §2.7). A send carries no
// sender: a first contact (outer access kind 0) goes under a session the
// sender opened with its signature and a MAC only it and its hub can make;
// every later message (kind 1) under a capability the recipient issued,
// which names no sender at all. The request body of a send is the envelope
// itself.

// The relay routes of this file.
const (
	RelaySessionPath = "/relay/session"
	RelaySendPath    = "/relay/send"
)

// EnvelopeContentType is the Content-Type of a /relay/send body: the
// envelope's own bytes, not JSON (core §2.12.1).
const EnvelopeContentType = "application/vnd.anet.envelope+cbor"

// The request headers of a send (core §2.12.1). The values are the
// relayauth constants where relayauth defines them, restated so the whole
// hub contract of this daemon can be read in one package.
const (
	// HeaderAccess is base64url of outer key 7, byte for byte the access
	// the envelope carries, at most MaxAccessHeader characters.
	HeaderAccess = "X-ANet-Access"
	// HeaderSession and HeaderMAC are a first contact's session id and
	// session MAC; the MAC binds HeaderTS (relayauth L8).
	HeaderSession = relayauth.HeaderSession
	HeaderMAC     = relayauth.HeaderMAC
	// HeaderHome is the optional hint of the recipient's home hub, a hub
	// AID; a first contact's MAC covers it.
	HeaderHome = relayauth.HeaderHome
)

// MaxAccessHeader is the longest X-ANet-Access a hub takes (1 KiB, core
// §2.12.1). A kind-1 access is about 400 characters.
const MaxAccessHeader = 1024

// SessionRequest is the body of POST /relay/session, signed with relayauth
// action session: the one-time X25519 public key the hub seals the session
// key to (core L7), base64url.
type SessionRequest struct {
	EPK string `json:"epk"`
}

// SessionResponse is its answer: the session id, when the session ends
// (unix ms) and the session key sealed to epk (enc, ct), base64url.
type SessionResponse struct {
	SID string `json:"sid"`
	Exp uint64 `json:"exp"`
	Enc string `json:"enc"`
	CT  string `json:"ct"`
}

// The sizes of the sealed session key in a SessionResponse: the HPKE
// encapsulated key and the ciphertext of the 32-byte key with its tag.
const (
	SessionEncLen = 32
	SessionCTLen  = relayauth.SessionKeyLen + 16
)

// ErrBadSession is a session answer this daemon cannot use: a member
// missing, not strict base64url, not of its size, or an exp of 0.
var ErrBadSession = errors.New("hubapi: the session answer is malformed")

// Decode checks the answer's members strictly (CORE-E5) and returns the
// session id, the end of the session and the sealed key. The key still has
// to open under the one-time key the request carried
// (relayauth.OpenSessionKey), which is what binds the answer to the
// request.
func (r SessionResponse) Decode() (sid []byte, exp uint64, enc, ct []byte, err error) {
	if sid, err = DecodeB64(r.SID, relayauth.SessionIDLen); err != nil {
		return nil, 0, nil, nil, fmt.Errorf("%w: sid: %v", ErrBadSession, err)
	}
	if enc, err = DecodeB64(r.Enc, SessionEncLen); err != nil {
		return nil, 0, nil, nil, fmt.Errorf("%w: enc: %v", ErrBadSession, err)
	}
	if ct, err = DecodeB64(r.CT, SessionCTLen); err != nil {
		return nil, 0, nil, nil, fmt.Errorf("%w: ct: %v", ErrBadSession, err)
	}
	if r.Exp == 0 {
		return nil, 0, nil, nil, fmt.Errorf("%w: no exp", ErrBadSession)
	}
	return sid, r.Exp, enc, ct, nil
}

// ErrorBody is a wire-3 refusal (core G1): the reason code, which is all a
// daemon branches on, and one English sentence, shown only for a code the
// daemon does not know (CORE-K20). A refusal may carry more members
// (required_wire, required_anet, max_bits, …), read by whoever needs them.
type ErrorBody struct {
	Reason string `json:"reason"`
	Error  string `json:"error"`
}

// reasonCode is the shape of every hub reason code (core CORE-K19).
var reasonCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

// ParseErrorBody reads a refusal body: the reason when it is a code in its
// one shape, and the sentence. A body that is not one (a proxy's page, an
// older hub's {"error"}) gives no reason, and its error member when it has
// one.
func ParseErrorBody(b []byte) (reason, sentence string) {
	var e ErrorBody
	if json.Unmarshal(b, &e) != nil {
		return "", ""
	}
	if reasonCode.MatchString(e.Reason) {
		reason = e.Reason
	}
	return reason, e.Error
}

package daemon

// relay_send.go is the daemon's POST /relay/send of hub wire 3
// (sealed-sender SSR-D28, §4.3.6; core §2.12).
//
// The request body is the envelope itself. Its outer key 7 says how it
// goes: a first contact (kind 0) on the signed side under a session MAC
// (session_client.go), with X-ANet-Access, X-ANet-Session, X-ANet-TS and
// X-ANet-MAC; a message under a capability its recipient issued (kind 1) on
// the anonymous side with X-ANet-Access alone. Neither carries X-ANet-AID,
// X-ANet-Seq or X-ANet-Sig: a send names no sender in its headers. Both may
// carry X-ANet-Home, the hint of the recipient's home hub.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// relaySendResult is a hub's 200 to one /relay/send (core §2.12.2): queued
// or a duplicate of an envelope already waiting, the rid it is queued under
// (HasRID false when the answer carried none this node can read: the
// envelope is then taken as handed over without one, DLV-W-12b), the hub it
// was queued at when that is another (ViaHub, an AID), and whether the
// recipient has stopped collecting its mail.
type relaySendResult struct {
	Status string
	RID    [hubapi.RIDLen]byte
	HasRID bool
	ViaHub string
	Quiet  bool
}

// relaySendV3 sends env to this node's hub under choice, the credential
// accessFor chose for this attempt (env's outer key 7 must be of its kind),
// and returns the hub's answer. handed reports whether the send itself got
// a connection (engine ENG-A4): opening a session does not count, nothing
// of the envelope is in it.
//
// A first contact whose session the hub no longer knows (401
// session_invalid) opens another and is sent once more; one that gets no
// session is errSessionUnavailable. Any other refusal is a *hubError of
// path /relay/send.
func (d *Daemon) relaySendV3(ctx context.Context, env []byte, choice AccessChoice) (res relaySendResult, handed bool, err error) {
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" {
		return res, false, errors.New("anet: no hub configured")
	}
	access, outer, err := outerAccess(env)
	if err != nil {
		return res, false, fmt.Errorf("anet: not an envelope to send: %w", err)
	}
	kind := outer.Access.Kind
	if !choice.Send || choice.Kind != kind {
		return res, false, fmt.Errorf("anet: the envelope goes under access kind %d, not the kind %d chosen", kind, choice.Kind)
	}
	home := choice.Home
	if kind == seal.AccessKindSession {
		home = d.kindZeroHome(outer.To, choice.HintHome)
	}
	if !identity.ValidAID(home) {
		// A hint that is not an AID would be refused whole (400); without
		// one the hub resolves the home itself.
		home = ""
	}
	// The hub's identity first, outside the trace: learning it carries no
	// byte of the envelope (as for every signed call, hubSigned).
	if _, _, err := d.hubIdentity(ctx, hub); err != nil {
		return res, false, err
	}
	for attempt := 0; ; attempt++ {
		var sess *firstContactSession
		if kind == seal.AccessKindSession {
			if sess, err = d.firstContactSession(ctx, hub); err != nil {
				return res, handed, err
			}
		}
		req, err := newRelaySendRequest(ctx, hub, env, access, home, sess, time.Now())
		if errors.Is(err, errSessionGone) {
			// The session was dropped between handing it out and the MAC
			// (another send's session_invalid, a newer session, an
			// emergency rotation): nothing was sent; once more under the
			// current one, which a zeroed session never is.
			d.dropSession(hub, sess)
			if attempt == 0 {
				continue
			}
			return res, handed, &sessionUnavailableError{err}
		}
		if err != nil {
			return res, handed, err
		}
		tctx, got := hubapi.TraceHanded(req.Context())
		rep := d.hubExchange(req.WithContext(tctx), hub, hubapi.RelaySendPath, int(kind))
		handed = handed || got()
		if rep.Status == 0 {
			if rep.Local {
				return res, handed, rep.Err
			}
			return res, handed, fmt.Errorf("anet: hub %s: %w", hubapi.RelaySendPath, rep.Err)
		}
		if we := hubWireCheck(hub, rep); we != nil {
			return res, handed, we
		}
		if rep.Status == http.StatusOK {
			return parseRelaySendAnswer(rep.Body), handed, nil
		}
		herr := newHubError(hubapi.RelaySendPath, rep)
		if sess != nil && attempt == 0 && rep.Status == http.StatusUnauthorized && herr.reason == "session_invalid" {
			// The hub restarted, or dropped the session: once more under a
			// new one (SSR-D27).
			d.dropSession(hub, sess)
			continue
		}
		return res, handed, herr
	}
}

// newRelaySendRequest builds one POST /relay/send of env: the body is the
// envelope, X-ANet-Access its outer key 7 byte for byte, X-ANet-Home the
// hint when there is one; under a session (a first contact) the session id,
// the time and the MAC over all of it (relayauth L8). Nothing else of this
// node is in it.
func newRelaySendRequest(ctx context.Context, hub string, env, access []byte, home string,
	sess *firstContactSession, now time.Time) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+hubapi.RelaySendPath, bytes.NewReader(env))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", hubapi.EnvelopeContentType)
	req.Header.Set(hubapi.HeaderAccess, hubapi.B64(access))
	if home != "" {
		req.Header.Set(hubapi.HeaderHome, home)
	}
	if sess != nil {
		ts := uint64(now.UnixMilli())
		mac, err := sess.mac(ts, req.Method, req.URL.RequestURI(), home, env)
		if err != nil {
			return nil, fmt.Errorf("anet: session MAC: %w", err)
		}
		req.Header.Set(hubapi.HeaderSession, relayauth.EncodeSessionID(sess.sid))
		req.Header.Set(hubapi.HeaderTS, strconv.FormatUint(ts, 10))
		req.Header.Set(hubapi.HeaderMAC, relayauth.EncodeMAC(mac))
	}
	return req, nil
}

// parseRelaySendAnswer reads a 200 to /relay/send. The hub took the
// envelope whatever the body says; what the body adds is read strictly:
// a rid that is not 16 bytes of strict base64url is no rid, a via_hub that
// is not an AID is no via_hub, and an unknown status reads as queued.
func parseRelaySendAnswer(body []byte) relaySendResult {
	var out hubapi.RelaySendResponse
	if json.Unmarshal(body, &out) != nil {
		return relaySendResult{Status: hubapi.RelayQueued}
	}
	res := relaySendResult{Status: hubapi.RelayQueued, Quiet: out.RecipientQuiet}
	if out.Status == hubapi.RelayDuplicate {
		res.Status = hubapi.RelayDuplicate
	}
	if rid, err := out.RIDBytes(); err == nil {
		res.RID, res.HasRID = rid, true
	}
	if identity.ValidAID(out.ViaHub) {
		res.ViaHub = out.ViaHub
	}
	return res
}

// outerAccess returns outer key 7 of env as its bytes (X-ANet-Access must
// be the same bytes, core §2.12.1) and the parsed outer envelope. The bytes
// are the CoreDet encoding of the access ParseOuter read, which is what
// seal.SetAccess wrote into every envelope this node sends; an envelope
// spelled otherwise is refused by the hub as bad_envelope, not delivered
// under another credential.
func outerAccess(env []byte) ([]byte, *seal.SealedEnvelope, error) {
	outer, err := seal.ParseOuter(env)
	if err != nil {
		return nil, nil, err
	}
	if outer.Access == nil {
		return nil, nil, errors.New("the envelope has no access")
	}
	raw, err := coredet.Marshal(outer.Access)
	if err != nil {
		return nil, nil, err
	}
	return raw, outer, nil
}

// homeHint is the X-ANet-Home of a first contact (SSR-D13): the witness of
// the establishment event at the tip of the recipient's KEL (core X-SS-4),
// else the home of any valid capability the recipient gave this node, else
// the home its directory or registry entry names, else none.
func homeHint(kelWitness, capHome, dirHome string) string {
	for _, h := range []string{kelWitness, capHome, dirHome} {
		if identity.ValidAID(h) {
			return h
		}
	}
	return ""
}

// kindZeroHome is homeHint for a first contact to `to`, with capHome the
// home of a capability `to` gave (AccessChoice.HintHome).
func (d *Daemon) kindZeroHome(to, capHome string) string {
	witness := ""
	if kel, ok := d.peerKEL(to); ok {
		if states, err := identity.Replay(kel); err == nil && len(states) > 0 {
			witness = states[len(states)-1].Witness
		}
	}
	return homeHint(witness, capHome, d.peerDirHome(to))
}

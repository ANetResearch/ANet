package daemon

// Fuzz targets for the two answers the wire-3 hub client decodes
// (session_client.go, relay_send.go). The hub is not trusted: whatever it
// answers, nothing panics, and nothing out of shape is taken.

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// FuzzSessionAnswer: an answer to POST /relay/session opens a session only
// when its id is 16 bytes, its sealed key opens under this node's one-time
// key for this hub and this id, and its end is set; the session then never
// outlives sessionMaxLife, and its key is the one the hub sealed.
func FuzzSessionAnswer(f *testing.F) {
	hub, err := identitytest.Hub()
	if err != nil {
		f.Fatal(err)
	}
	esk, epk, err := relayauth.NewSessionKey()
	if err != nil {
		f.Fatal(err)
	}
	sid, key := make([]byte, relayauth.SessionIDLen), make([]byte, relayauth.SessionKeyLen)
	_, _ = rand.Read(sid)
	_, _ = rand.Read(key)
	enc, ct, err := relayauth.SealSessionKey(epk, hub.AID(), sid, key)
	if err != nil {
		f.Fatal(err)
	}
	now := time.Now()
	good, _ := json.Marshal(hubapi.SessionResponse{SID: hubapi.B64(sid), Exp: uint64(now.Add(time.Hour).UnixMilli()),
		Enc: hubapi.B64(enc), CT: hubapi.B64(ct)})
	for _, s := range []string{string(good), `{}`, `{"sid":"","exp":1}`, `null`, `[]`,
		`{"sid":"` + hubapi.B64(sid) + `=","exp":1,"enc":"` + hubapi.B64(enc) + `","ct":"` + hubapi.B64(ct) + `"}`,
		`{"sid":"` + hubapi.B64(sid) + `","exp":18446744073709551615,"enc":"` + hubapi.B64(enc) + `","ct":"` + hubapi.B64(ct) + `"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		s, err := openSessionAnswer(esk, hub.AID(), body, now)
		if err != nil {
			if s != nil {
				t.Fatal("a session with an error")
			}
			return
		}
		if len(s.sid) != relayauth.SessionIDLen || len(s.key) != relayauth.SessionKeyLen || s.hubAID != hub.AID() {
			t.Fatalf("session of sid %d, key %d bytes, hub %q", len(s.sid), len(s.key), s.hubAID)
		}
		if !bytes.Equal(s.key, key) || !bytes.Equal(s.sid, sid) {
			t.Fatal("a session key other than the one the hub sealed for this id opened")
		}
		if s.exp.After(now.Add(sessionMaxLife)) {
			t.Fatalf("a session believed open until %v", s.exp)
		}
	})
}

// FuzzRelaySendAnswer: whatever a hub's 200 to /relay/send says, the
// status read is queued or duplicate, a rid is taken only in its one
// spelling of 16 bytes, and via_hub only when it is an AID.
func FuzzRelaySendAnswer(f *testing.F) {
	other, err := identitytest.Hub()
	if err != nil {
		f.Fatal(err)
	}
	rid := hubapi.B64(bytes.Repeat([]byte{7}, hubapi.RIDLen))
	for _, s := range []string{
		`{"status":"queued","rid":"` + rid + `","recipient_quiet":false}`,
		`{"status":"duplicate","rid":"` + rid + `","via_hub":"` + other.AID() + `","recipient_quiet":true}`,
		`{"status":"forwarded","rid":"AAAA","via_hub":"https://x"}`, `{"rid":null}`, `[]`, ``, `{"status":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		res := parseRelaySendAnswer(body)
		if res.Status != hubapi.RelayQueued && res.Status != hubapi.RelayDuplicate {
			t.Fatalf("status %q", res.Status)
		}
		if res.HasRID {
			var out hubapi.RelaySendResponse
			if json.Unmarshal(body, &out) != nil || out.RID != hubapi.B64(res.RID[:]) {
				t.Fatalf("rid %x taken from %q", res.RID, body)
			}
		} else if res.RID != [hubapi.RIDLen]byte{} {
			t.Fatal("a rid without HasRID")
		}
		if res.ViaHub != "" && !identity.ValidAID(res.ViaHub) {
			t.Fatalf("via_hub %q taken", res.ViaHub)
		}
	})
}

package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"
)

// FuzzRxTokenFor holds the token table to its rules for any verdict (P-ENG-6):
// no token for a one-shot envelope, a temporary failure or one whose first
// layer did not open; tok(1) never carries a note; a tok(2) note's reason is
// a code or empty, and its wait is within the note's bound; and the answer
// is a function of the verdict alone.
func FuzzRxTokenFor(f *testing.F) {
	f.Add(uint8(2), uint8(9), dropUnknownIX, false, true, "", uint32(0))
	f.Add(uint8(2), uint8(9), dropNotAccepting, false, true, seal.NoteQuotaCallerPerDay, uint32(90_000))
	f.Add(uint8(2), uint8(13), dropRefusedReplay, false, true, "Not A Code", uint32(5))
	f.Add(uint8(1), uint8(18), "", true, true, "", uint32(0))
	f.Add(uint8(3), uint8(12), seal.ReasonWitnessUnknown, false, true, "", uint32(0))
	f.Fuzz(func(t *testing.T, class, step uint8, reason string, oneShot, layer1 bool, note string, retry uint32) {
		v := rxVerdict{Class: rxClass(class%4 + 0), Step: rxStep(step), Reason: reason, OneShot: oneShot, Layer1: layer1,
			Note: seal.Note{Reason: note, RetryAfterS: retry}}
		got := rxTokenFor(v)
		if got != rxTokenFor(v) {
			t.Fatal("not a function of the verdict")
		}
		if (v.OneShot || !v.Layer1 || v.Class == rxTransient) && got.Make {
			t.Fatalf("%+v answered %+v", v, got)
		}
		if !got.Make {
			if got != (tokenDecision{}) {
				t.Fatalf("no token, but %+v", got)
			}
			return
		}
		switch got.Outcome {
		case seal.OutcomeProcessed:
			if got.Note != (seal.Note{}) {
				t.Fatalf("tok(1) with a note: %+v", got)
			}
		case seal.OutcomeRefused:
			if got.Note.Reason != "" && !seal.ValidNoteReason(got.Note.Reason) {
				t.Fatalf("a note reason that is no code: %q", got.Note.Reason)
			}
			if got.Note.RetryAfterS > seal.MaxRetryAfterS {
				t.Fatalf("a wait past the bound: %d", got.Note.RetryAfterS)
			}
			if _, err := (seal.AckKey{1}).MakeDAck(got.Outcome, got.Note); err != nil {
				t.Fatalf("a token the note cannot be made into: %v", err)
			}
		default:
			t.Fatalf("outcome %d", got.Outcome)
		}
	})
}

// rxPathsFuzz is one worker's provider, the requester whose task it holds,
// and three other senders: a stranger, one whose witness the provider does
// not know, and one impersonating the requester.
type rxPathsFuzz struct {
	req, prov *Daemon
	id        string
	senders   []sender
	n         int
}

// FuzzReceivePathsAgree is P-ENG-7 over generated envelopes: the same
// message — the same sender, type, interaction, body and faults, sealed
// twice under two message ids — goes once through the hub mailbox and once
// over a direct transport, and the two answers agree. Before the first
// layer opens the hub path drops it without a token and the direct path
// refuses it for now; from there on both paths give the same class and the
// same outcome and note (engine §4.8, ENG-V3).
func FuzzReceivePathsAgree(f *testing.F) {
	quietLog(f)
	_, req, prov := registeredPair(f)
	id, err := req.Delegate(context.Background(), prov.AID(), "a task", nil)
	if err != nil {
		f.Fatal(err)
	}
	settleDelivery(f, req, prov)
	roomyRxBuckets(prov)
	unknown, _ := unknownWitnessSender(f)
	mallory := newStranger(f)
	z := &rxPathsFuzz{req: req, prov: prov, id: id,
		senders: []sender{senderOf(req), mallory, unknown, impersonating(mallory, req)}}
	text, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: "x"}).Marshal()
	f.Add(uint8(0), uint8(0), uint8(0), text, int8(0), uint8(0))
	f.Add(uint8(1), uint8(1), uint8(1), []byte(nil), int8(0), uint8(0))
	f.Add(uint8(2), uint8(0), uint8(0), text, int8(0), uint8(0))
	f.Add(uint8(3), uint8(0), uint8(0), text, int8(0), uint8(0))
	f.Add(uint8(0), uint8(0), uint8(0), text, int8(-30), uint8(0))
	f.Add(uint8(0), uint8(0), uint8(0), text, int8(0), uint8(1))
	f.Add(uint8(0), uint8(0), uint8(0), text, int8(0), uint8(2))
	f.Add(uint8(0), uint8(4), uint8(2), []byte{0xa0}, int8(0), uint8(4))
	f.Fuzz(func(t *testing.T, who, kind, where uint8, body []byte, days int8, faults uint8) {
		s := z.senders[int(who)%len(z.senders)]
		types := []string{seal.TypeMessage, seal.TypeDelegate, seal.TypeStatus, seal.TypeResult, delegation.TypeCardGet, "anet.bogus/1"}
		typ := types[int(kind)%len(types)]
		z.n++
		ix := func(path string) string {
			switch where % 3 {
			case 0:
				return z.id
			case 1:
				return fmt.Sprintf("ix_fuzz_%d_%s", z.n, path)
			}
			return ""
		}
		bodyFor := func(path string) []byte {
			if typ == seal.TypeDelegate && len(body) == 0 {
				return delegateBody(t, s.ctrl, ix(path), "x", "")
			}
			return body
		}
		// Whole days, so the two copies, sealed a moment apart, fall on the
		// same side of every time limit.
		at := uint64(time.Now().UnixMilli()) + uint64(int64(days)*24*3600*1000)
		o := func(path string) craftOpt {
			return craftOpt{oneShot: faults&4 != 0, edit: func(in *seal.SealedInner) {
				in.IX = ix(path)
				in.TS, in.Exp = at, at+messageLifetimeMS
				if faults&4 != 0 {
					in.Exp = at + 10*60*1000
				}
				if faults&1 != 0 {
					in.Keys = nil
				}
				if faults&2 != 0 {
					in.Notices = [][]byte{{0xa0}}
				}
			}}
		}
		hubEnv, hubAck := craftWith(t, s, z.prov, typ, ix("hub"), bodyFor("hub"), o("hub"))
		dirEnv, dirAck := craftWith(t, s, z.prov, typ, ix("direct"), bodyFor("direct"), o("direct"))
		r := receive(t, z.prov, hubEnv)
		ho, hn, hok := tokenOf(t, r, hubAck)
		dack, err := receiveDirect(t, z.prov, dirEnv)
		do, dn, dok := tokenOfDack(t, dack, dirAck)
		if (err == nil) != r.ack() {
			t.Fatalf("hub path %+v; direct path err %v", r, err)
		}
		if hok != dok || ho != do || hn != dn {
			t.Fatalf("hub path %s: %v %d %+v; direct path: %v %d %+v", r.reason, hok, ho, hn, dok, do, dn)
		}
	})
}

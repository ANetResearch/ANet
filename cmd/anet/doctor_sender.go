package main

// doctor_sender.go is doctor's sender privacy checks (ux §5.2, sealed-sender
// §5.4 SSR-U4): sender_privacy.mailbox_key, .session, .grants and .hub,
// shown once this node is registered at a hub, and p2p.lan, shown only when
// it is on. The first four need what only the running daemon knows, and
// read /status.sender_privacy (UX-K2); p2p.lan reads config.json. Every
// finding is a sentence of the reason table (UX-K7); none promises that the
// hub cannot tell who sent a message (SSR-U5).

import (
	"encoding/json"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// senderPrivacyView is /status.sender_privacy (SS §4.8 SSR-U2), as doctor
// reads it.
type senderPrivacyView struct {
	MailboxRegistered      bool   `json:"mailbox_registered"`
	SessionOK              bool   `json:"session_ok"`
	Held                   int    `json:"held"`
	Issued                 int    `json:"issued"`
	UnpushedRevocationAgeS uint64 `json:"unpushed_revocation_age_s"`
	UnpushedRevocations1h  int    `json:"unpushed_revocations_1h"`
	RefusalDowngrades24h   int    `json:"refusal_downgrades_24h"`
	SmallBuckets24h        int    `json:"small_buckets_24h"`
	HubEpochInvalid        bool   `json:"hub_epoch_invalid"`
}

// The thresholds of SS §5.4.
const (
	// withdrawalLateS: a withdrawal the hub has not had for over an hour.
	withdrawalLateS = 3600
	// hubRefusalsWarn: unexplained refusals that cost a delivery credential
	// in the last 24 hours. One is what a peer taking you off its list
	// looks like; three is a pattern worth telling.
	hubRefusalsWarn = 3
	// hubSmallBucketsWarn: one directory group under 32 entries is enough.
	hubSmallBucketsWarn = 1
)

// senderPrivacyIDs are the checks that read /status.sender_privacy, in the
// order of ux §5.2.
var senderPrivacyIDs = []string{"sender_privacy.mailbox_key", "sender_privacy.session", "sender_privacy.grants",
	"sender_privacy.hub"}

// senderFinding is one finding of the sender privacy checks: the reason
// table's key and its values, or no key for an ok without a sentence.
type senderFinding struct {
	id, status, key string
	args            a2ashape.Args
}

// senderPrivacyFindings is SS §5.4 over /status.sender_privacy: one finding
// per check, in the order of ux §5.2. sender_privacy.hub says the first that
// holds of: an impossible mailbox signing key number, three or more
// downgrades after unexplained refusals in 24 hours, a small directory
// group.
func senderPrivacyFindings(sp senderPrivacyView) []senderFinding {
	out := make([]senderFinding, 0, len(senderPrivacyIDs))
	if sp.MailboxRegistered {
		out = append(out, senderFinding{id: "sender_privacy.mailbox_key", status: stOK})
	} else {
		out = append(out, senderFinding{id: "sender_privacy.mailbox_key", status: stWarn, key: "sender_privacy.mailbox_pending"})
	}
	if sp.SessionOK {
		out = append(out, senderFinding{id: "sender_privacy.session", status: stOK})
	} else {
		out = append(out, senderFinding{id: "sender_privacy.session", status: stWarn, key: "session_unavailable"})
	}
	if sp.UnpushedRevocationAgeS >= withdrawalLateS {
		out = append(out, senderFinding{id: "sender_privacy.grants", status: stWarn, key: "sender_privacy.withdrawals_pending",
			args: a2ashape.Args{"n": max(sp.UnpushedRevocations1h, 1)}})
	} else {
		out = append(out, senderFinding{id: "sender_privacy.grants", status: stInfo, key: "sender_privacy.grants_summary",
			args: a2ashape.Args{"n": sp.Held, "m": sp.Issued}})
	}
	hub := senderFinding{id: "sender_privacy.hub", status: stOK}
	switch {
	case sp.HubEpochInvalid:
		hub = senderFinding{id: hub.id, status: stWarn, key: "sender_privacy.hub_epoch_invalid"}
	case sp.RefusalDowngrades24h >= hubRefusalsWarn:
		hub = senderFinding{id: hub.id, status: stWarn, key: "sender_privacy.hub_refusals",
			args: a2ashape.Args{"n": sp.RefusalDowngrades24h}}
	case sp.SmallBuckets24h >= hubSmallBucketsWarn:
		hub = senderFinding{id: hub.id, status: stWarn, key: "sender_privacy.hub_small_bucket"}
	}
	return append(out, hub)
}

// senderPrivacyChecks adds the four sender_privacy checks (ux §5.2): none
// before this node is registered at a hub (UX-K6), each unknown with
// doctor.daemon_down while the daemon is not running (UX-K2).
func senderPrivacyChecks(say func(id, status, key string, args a2ashape.Args),
	add func(id, status, detail, hint string), rep *doctorReport, env doctorEnv, layout daemon.Layout, cfg daemon.Config) {
	if cfg.HubURL == "" {
		return
	}
	var st map[string]any
	if rep.Control.Running && env.status != nil {
		st, _ = env.status(rep.Control.Addr, layout.ControlTokenPath())
	}
	if st == nil {
		for _, id := range senderPrivacyIDs {
			say(id, stUnknown, doctorDaemonDown, nil)
		}
		return
	}
	if hub, _ := st["hub"].(map[string]any); hub["state"] != daemon.JoinRegistered {
		return // not registered (yet): the hub check says why
	}
	raw, ok := st["sender_privacy"]
	if !ok {
		return
	}
	b, err := json.Marshal(raw)
	var sp senderPrivacyView
	if err != nil || json.Unmarshal(b, &sp) != nil {
		return
	}
	for _, f := range senderPrivacyFindings(sp) {
		if f.key == "" {
			add(f.id, f.status, "", "")
			continue
		}
		say(f.id, f.status, f.key, f.args)
	}
}

// p2pLANCheck is p2p.lan: shown only while modules.p2p.lan is true, when
// the p2p transport also dials local-network addresses (SS §4.4, §5.1).
func p2pLANCheck(say func(id, status, key string, args a2ashape.Args), cfg daemon.Config) {
	raw, ok := cfg.Modules["p2p"]
	if !ok {
		return
	}
	var p2p struct {
		LAN bool `json:"lan"`
	}
	if json.Unmarshal(raw, &p2p) == nil && p2p.LAN {
		say("p2p.lan", stInfo, "p2p.lan_on", nil)
	}
}

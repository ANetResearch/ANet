package daemon

// sender_privacy.go is what the control plane tells about sender privacy
// (sealed-sender §4.8, §5.2): for each peer on the lists, whether this
// node's next message to it carries this node's identity (POST /peers/list,
// SSR-U1, SSR-U3); and the facts doctor's sender_privacy checks read
// (/status.sender_privacy, SSR-U2, ux §5.2). The sentences are the reason
// table's; nothing here promises that the hub cannot tell who sent a message
// (SSR-U5).

import (
	"net/http"
	"sort"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The sentences of the sender identity column (ux §1.4 M5, SS §5.2).
const (
	senderHiddenKey      = "peers.sender_hidden"
	senderVisibleKey     = "peers.sender_visible"
	senderVisiblePaidKey = "peers.sender_visible.paid"
)

// senderIdentity is SSR-U3 for one peer: this node's messages to aid go
// without its identity exactly when it holds a peer capability aid gave it
// that it may use now (heldValid: not given up, not about to expire, under
// aid's current mailbox epoch when this node knows it) and that does not
// say aid prices something. A valid one that does is the .paid sentence:
// new tasks to aid go as first contacts (SS §4.3.3, M17). One-shot
// capabilities say nothing about the next message. peerEpoch is the
// mailbox epoch of aid's key set as this node holds it (0: none).
func senderIdentity(aid string, held []interactions.HeldCap, peerEpoch, now uint64) (hidden bool, key string) {
	row := accessRow{To: aid, PeerEpoch: peerEpoch}
	for i := range held {
		h := &held[i]
		if h.Scope != interactions.CapScopePeer || !heldValid(h, row, now) {
			continue
		}
		if h.Pay {
			return false, senderVisiblePaidKey
		}
		return true, senderHiddenKey
	}
	return false, senderVisibleKey
}

// PeerView is one peer of POST /peers/list: its AID, the lists it is on
// (allow, trust, deny, in that order) and the sender identity column
// (SSR-U1): sender_hidden, and the sentence that says it in this process's
// language.
type PeerView struct {
	AID              string   `json:"aid"`
	Lists            []string `json:"lists"`
	SenderHidden     bool     `json:"sender_hidden"`
	SenderHiddenText string   `json:"sender_hidden_text"`
}

// PeersList is the answer of POST /peers/list: the inbound policy and the
// three lists as before, and every listed peer once, by AID.
type PeersList struct {
	InboundState
	Peers []PeerView `json:"peers"`
}

// PeersList is the inbound state with the sender identity of every peer on
// a list. A peer whose capabilities cannot be read is shown as visible: the
// column never says more than this node knows.
func (d *Daemon) PeersList() PeersList {
	st := d.InboundStatus()
	lists := map[string][]string{}
	for _, l := range []struct {
		name string
		aids []string
	}{{ListAllow, st.Allow}, {ListTrust, st.Trust}, {ListDeny, st.Deny}} {
		for _, aid := range l.aids {
			lists[aid] = append(lists[aid], l.name)
		}
	}
	aids := make([]string, 0, len(lists))
	for aid := range lists {
		aids = append(aids, aid)
	}
	sort.Strings(aids)
	now, lang := d.nowMS(), a2ashape.Lang()
	out := PeersList{InboundState: st, Peers: make([]PeerView, 0, len(aids))}
	for _, aid := range aids {
		hidden, key := false, senderVisibleKey
		if held, err := d.ix.HeldCaps(aid); err == nil {
			hidden, key = senderIdentity(aid, held, d.peerMboxEpoch(aid, ""), now)
		} else {
			d.logOnce("peers-list-held", "anet: peers list: cannot read the delivery credentials held: %v", err)
		}
		out.Peers = append(out.Peers, PeerView{AID: aid, Lists: lists[aid], SenderHidden: hidden,
			SenderHiddenText: a2ashape.Sentence(key, lang, nil)})
	}
	return out
}

// hPeersList is POST /peers/list: the inbound policy, the three peer lists
// and, for each listed peer, the sender identity column (SSR-U1).
func (d *Daemon) hPeersList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.PeersList())
}

// revocationPendingMS is how long a withdrawal may wait for the hub before
// doctor's sender_privacy.grants warns (SS §5.4: over an hour).
const revocationPendingMS uint64 = 3600 * 1000

// senderPrivacy is /status.sender_privacy (SSR-U2): what doctor's
// sender_privacy checks decide on (SS §5.4, ux §5.2). The last three and
// mailbox_registered live in memory only and start again with the daemon.
// unpushed_revocations_1h is the {n} of sender_privacy.withdrawals_pending:
// how many withdrawals waited an hour or more.
type senderPrivacy struct {
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

// senderPrivacyStatus reads /status.sender_privacy at now (milliseconds).
// A count the store cannot give is 0, and logged once.
func (d *Daemon) senderPrivacyStatus(now uint64) senderPrivacy {
	var sp senderPrivacy
	sp.MailboxRegistered, sp.HubEpochInvalid = d.mailboxStatus()
	sp.SessionOK = d.sessionOK(d.config().HubURL)
	sp.RefusalDowngrades24h = d.refusalDowngrades24h(now)
	sp.SmallBuckets24h = d.smallBuckets24h(now)
	held, issued, err := d.ix.CapsCounts(now)
	if err == nil {
		sp.Held, sp.Issued = held, issued
	}
	at, ok, err2 := d.ix.OldestUnpushedRevocation()
	if err2 == nil && ok && now > at {
		sp.UnpushedRevocationAgeS = (now - at) / 1000
	}
	var err3 error
	if now >= revocationPendingMS {
		sp.UnpushedRevocations1h, err3 = d.ix.UnpushedRevocationsBefore(now - revocationPendingMS)
	}
	for _, e := range []error{err, err2, err3} {
		if e != nil {
			d.logOnce("status-sender-privacy", "anet: /status.sender_privacy: %v", e)
			break
		}
	}
	return sp
}

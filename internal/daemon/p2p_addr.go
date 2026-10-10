package daemon

// p2p_addr.go is the daemon's side of direct (p2p) delivery: where a peer may
// be dialled, and what modules.p2p takes (sealed-sender SSR-P2, SSR-P4, §5.1).
//
// A recipient says where it may be dialled in the contact-class peer
// capability it gives this node (Grant key 5, SSR-D8): the address is the
// recipient's own choice, and only a contact gets one. The p2p module asks
// for it before every send (module.TransportHost.P2PAddr) and the address
// travels in the send frame to the peer process, which dials it and nothing
// else. Before it leaves the daemon, an IP literal in a refused range, or the
// name localhost, is left out (internal/p2paddr): such an address is never
// handed to the peer process, and the message goes through the hub.

import (
	"encoding/json"
	"path/filepath"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/p2paddr"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func init() { configChecks = append(configChecks, checkP2PConfig) }

// cntP2PAddrRefused counts the addresses of held capabilities left out
// because no dial may go to them (SSR-P4; ux §1.3 counters).
const cntP2PAddrRefused = "p2p-addr-refused"

// P2PAddr is module.TransportHost's: where aid may be dialled directly now.
func (h moduleHost) P2PAddr(aid string) (string, bool) { return h.d.p2pAddr(aid, h.d.nowMS()) }

// p2pAddr is the p2p address of a peer capability aid gave this node that
// is valid at now (heldValid: not given up, not about to expire, under aid's
// current mailbox epoch) and of the contact class, when its address passes
// the literal check with this node's LAN setting; "" and false otherwise.
func (d *Daemon) p2pAddr(aid string, now uint64) (string, bool) {
	if aid == "" || aid == d.AID() || d.ix == nil {
		return "", false
	}
	held, err := d.ix.HeldCaps(aid)
	if err != nil {
		return "", false
	}
	row := accessRow{To: aid, PeerEpoch: d.peerMboxEpoch(aid, "")}
	lan := p2pLAN(d.config())
	for i := range held {
		h := &held[i]
		if h.Scope != interactions.CapScopePeer || h.Class != seal.CapClassContact || h.P2P == "" || !heldValid(h, row, now) {
			continue
		}
		if err := p2paddr.LiteralRefused(h.P2P, lan); err != nil {
			d.countErr(cntP2PAddrRefused, err)
			continue
		}
		return h.P2P, true
	}
	return "", false
}

// p2pLAN is modules.p2p.lan: whether a direct delivery may be dialled at a
// private address. Off unless the block says true.
func p2pLAN(cfg Config) bool {
	raw, ok := cfg.Modules["p2p"]
	if !ok {
		return false
	}
	var mc struct {
		LAN bool `json:"lan"`
	}
	return json.Unmarshal(raw, &mc) == nil && mc.LAN
}

// checkP2PConfig is the ConfigCheck of modules.p2p: the peer process's
// socket, when given, is an absolute path (the daemon dials it from wherever
// it was started; a block without one is the module's own "socket is
// required"), lan is true or false, and advertise is an address a contact
// can dial. Each is refused at start with config_invalid naming the key, rather
// than found out at every dial or left out of every grant.
func checkP2PConfig(_ Config, raw map[string]any) *a2ashape.Refusal {
	mods, _ := raw["modules"].(map[string]any)
	p, ok := mods["p2p"].(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := p["socket"]; ok {
		if s, ok := v.(string); !ok || !filepath.IsAbs(s) {
			return configInvalid("modules.p2p.socket", ruleText(`须是 anetpeer --socket 的绝对路径，如 "/run/anet/peer.sock"`,
				`it is the absolute path of anetpeer --socket, such as "/run/anet/peer.sock"`))
		}
	}
	if v, ok := p["lan"]; ok && v != nil {
		if _, ok := v.(bool); !ok {
			return configInvalid("modules.p2p.lan", ruleText("只能是 true 或 false", "it is true or false"))
		}
	}
	if v, ok := p["advertise"]; ok && v != nil {
		if s, ok := v.(string); !ok || p2paddr.CheckAdvertise(s) != nil {
			return configInvalid("modules.p2p.advertise", ruleText(
				`须是其他机器能拨通的 host:port，端口 1–65535，如 "peer.example.org:39100"`,
				`it is a host:port other machines can dial, the port from 1 to 65535, such as "peer.example.org:39100"`))
		}
	}
	return nil
}

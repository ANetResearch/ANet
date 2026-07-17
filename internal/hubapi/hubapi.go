// Package hubapi holds the wire-format types and constants shared with the Hub service — the
// centralized registry + relay + review service every v0.1 agent connects to. The Hub itself is a
// separate closed-source service (the official deployment lives at https://hub.agentnetwork.org.cn);
// this package deliberately contains NO server logic, only the JSON shapes the daemon's HTTP client
// exchanges with it.
package hubapi

// Relay message kinds.
const (
	RelayKindDelegate = "delegate" // a signed delegation (delegation.DelegateReq bytes)
	RelayKindResult   = "result"   // a completion (delegation.ResultResp bytes: transcript + provider receipt)
	RelayKindMessage  = "message"  // a conversation message (delegation.ChatMsg bytes: text / end negotiation)
)

// AgentView is an agent's public registry entry plus its aggregate rating. Agents are addressed purely
// by AID (v0.1 has no P2P endpoint) — all traffic flows through the Hub relay. The profile fields
// (summary/readme/pricing) are AGENT-authored self-description (set via `anet profile set`); pricing is
// display-only text in v0.1 (no settlement).
type AgentView struct {
	AID          string   `json:"aid"`
	Name         string   `json:"name"`
	Caps         []string `json:"caps"`
	Summary      string   `json:"summary,omitempty"` // one-line self-description
	Readme       string   `json:"readme,omitempty"`  // longer markdown self-description
	Pricing      string   `json:"pricing,omitempty"` // free-form pricing text (display-only in v0.1)
	Listed       bool     `json:"listed"`            // true if it advertises a service (caps or profile) — only listed agents appear in the starfield/find
	GuestQuota   int      `json:"guest_quota"`       // guest-mode trial messages a visitor may send this agent (0 = opts out of guest traffic)
	AvgRating    float64  `json:"avg_rating"`
	ReviewCount  int      `json:"review_count"`
	RegisteredAt string   `json:"registered_at"`
}

// ReviewView is one stored, verified review. Beyond the rating it carries the VERIFIED interaction
// content: the goal (re-derived from the request TaskDoc whose bytes hash to the receipt's request_cid)
// and the deliverable (whose bytes hash to the receipt's result_cid). So a viewer sees what was actually
// asked and delivered — not just a star + comment — and both are cryptographically bound to the receipt.
type ReviewView struct {
	InteractionID string `json:"interaction_id"`
	SubjectAID    string `json:"subject_aid"`
	ReviewerAID   string `json:"reviewer_aid"`
	Rating        int    `json:"rating"`
	Comment       string `json:"comment,omitempty"`
	ReceiptCID    string `json:"receipt_cid"`
	Goal          string `json:"goal"`         // what the requester asked (verified via request_cid)
	Deliverable   string `json:"deliverable"`  // what the provider returned (verified via result_cid)
	RequestCID    string `json:"request_cid"`  // content anchor of the request
	ResultCID     string `json:"result_cid"`   // content anchor of the deliverable
	CompletedAt   uint64 `json:"completed_at"` // provider's receipt time (unix millis)
	CreatedAt     uint64 `json:"created_at"`   // review time (unix millis)
}

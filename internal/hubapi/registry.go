package hubapi

import "encoding/json"

// The hub's A2A registry (A2A-DESIGN §10.5):
//
//	GET /a2a/v1/agents?skill=&tag=&q=&cursor=&limit=
//	GET /a2a/v1/agents/{aid}/card   (the card's bytes; ETag, max-age=300)
//	GET /agents/{aid}/jwks.json     (the jku of the card, derived from the KEL)
//
// The daemon reads the listing whole, a page at a time, and matches skill,
// tag and free text against the cards itself (sealed-sender SSR-D16): what
// an operator looks for says what they are about to do, and the hub has no
// need to know it. skill, tag and q exist for the hub's web UI.
// The member names follow A2A's camelCase, unlike the hub's older
// snake_case routes, because external A2A clients read this listing too.
// Mirrors ANetHub internal/aghub/registry.go, pinned on both sides.

// Registry query parameters and paths.
const (
	RegistryAgentsPath = "/a2a/v1/agents"
	// RegistryMaxLimit is the largest page the hub returns; a larger limit
	// is clamped, and the default page is 50.
	RegistryMaxLimit = 200
	// CardVerificationOK is the cardVerification of every entry: the hub
	// lists only cards that verified at admission and still verify
	// against the agent's current KEL.
	CardVerificationOK = "ok"
)

// A2AAgentEntry is one entry of GET /a2a/v1/agents. Card is the agent's
// signed A2A AgentCard as it sent it; everything else is the hub's
// statement, which a consumer that needs more than the hub's word
// replaces by verifying Card against the agent's KEL (a2acard.Verify).
type A2AAgentEntry struct {
	AID              string          `json:"aid"`
	Card             json.RawMessage `json:"card"`
	CardVerification string          `json:"cardVerification"`
	VerifiedAt       string          `json:"verifiedAt"` // RFC 3339
	HomeHub          string          `json:"homeHub"`    // origin of the hub the agent is registered at
	LastSeen         string          `json:"lastSeen,omitempty"`
	Quiet            bool            `json:"quiet"`
	ReviewCount      int             `json:"reviewCount"`
	AvgRating        float64         `json:"avgRating"`
	// KEL is the agent's key history and Keys its signed encryption key
	// set, base64url (core G2); KeyStateNotice the witness notice of the
	// KEL's tip, when there is one. A daemon that verifies them keeps the
	// entry as a discovery (sealed-sender SSR-D15 step 2).
	KEL            string `json:"kel"`
	Keys           string `json:"keys"`
	KeyStateNotice string `json:"key_state_notice,omitempty"`
}

// A2AAgentList is the GET /a2a/v1/agents response. NextCursor, when set,
// is passed back as cursor for the next page.
type A2AAgentList struct {
	Agents     []A2AAgentEntry `json:"agents"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

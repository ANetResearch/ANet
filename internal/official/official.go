// Package official says which agents the anet project itself runs — the
// public agents of A2A-DESIGN §15 — so that a list of agents can mark them
// ("anet.official": true) and a user can tell the project's echo agent from
// a stranger who registered the same name.
//
// The answer comes from one document, the official manifest: the AID, name,
// hub and capabilities of each official agent, a sequence number and a
// validity period. It is signed with the release key, in a namespace of its
// own (release.OfficialNamespace):
//
//	ssh-keygen -Y sign -n anet-official@agentnetwork.org.cn manifest.json
//
// and compiled into the binary (embed.go). Nothing else is believed: not a
// name, not a card, not a hub's registry, not the hub admin's official
// list. Only the AID decides, because the AID is the one thing a name-alike
// cannot have — messages to it are sealed to keys its own history
// certifies. A manifest that does not verify, or has expired, marks no one.
//
// The mark is a label and nothing more. It grants no admission, trust,
// payment or channel of any kind: an official agent is reached, admitted
// and paid exactly like any other agent, and the hub admin plane learns
// nothing from the manifest (it registers official agents by id, aid, hub
// and caps on its own, A2A-DESIGN §15 [C39]).
//
// This package depends on the standard library and internal/release only;
// it does not import a2a-go (SI-8).
package official

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/release"
)

// Schema is the manifest format this package reads. Another is refused.
const Schema = "anet-official/1"

// Key is the name the mark goes by wherever an agent is shown: control
// plane /find and /agents/list (MCP list_agents, get_agent_card), the local
// A2A interface's agent list, and the anet-origin params of a proxy card.
const Key = "anet.official"

// Limits on what is read before and after verification.
const (
	MaxManifestBytes = 256 << 10
	MaxAgents        = 256
	maxNameBytes     = 128
	maxCaps          = 64
)

// timeLayout is the one timestamp form of the manifest: UTC, seconds, "Z"
// (the release manifest's form).
const timeLayout = "2006-01-02T15:04:05Z"

var (
	idRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	aidRe = regexp.MustCompile(`^[a-z0-9]{1,128}$`) // the AID alphabet a kid carries (ANetCore a2acard)
	capRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	fpRe  = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// ErrExpired is a correctly signed manifest whose validity has run out.
var ErrExpired = errors.New("the official manifest has expired")

// Entry is one official agent.
type Entry struct {
	// ID is the operator's name for the deployment (anet-echo-e); Name is
	// what the agent is called. Neither decides anything: a name-alike
	// with another AID is not official.
	ID   string `json:"id"`
	Name string `json:"name"`
	// AID is the agent's identity, and the only field Lookup matches.
	AID string `json:"aid"`
	// Hub is the hub the agent is registered at.
	Hub string `json:"hub"`
	// Caps are the capabilities it serves to anyone.
	Caps []string `json:"caps"`
}

// Manifest is the verified official manifest.
type Manifest struct {
	Schema string `json:"schema"`
	// Seq increases with every manifest published (build-release.sh
	// --official adds one to the committed manifest's). It orders
	// manifests for people — anet doctor reports it — and is not checked
	// against anything here: a binary carries exactly one manifest, and
	// what keeps an old one from being brought back is `anet update`
	// refusing to install an older release, and expires_at.
	Seq       uint64 `json:"seq"`
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
	// KeyFingerprint names the signing key. It is checked against the key
	// inside the signature, so it cannot say something the signature does
	// not.
	KeyFingerprint string  `json:"key_fingerprint"`
	Agents         []Entry `json:"agents"`

	expires time.Time
	byAID   map[string]Entry
}

// Verify checks sig over raw — an SSHSIG by a key trust accepts, made in
// release.OfficialNamespace — and parses the result. Nothing in raw is
// read before the signature over it has verified. It does not look at the
// clock: CheckFresh and Lookup do.
func Verify(raw, sig []byte, trust release.Trust) (*Manifest, error) {
	if len(raw) > MaxManifestBytes {
		return nil, fmt.Errorf("official manifest: %d bytes, more than a manifest can be", len(raw))
	}
	if len(sig) > release.MaxSigBytes {
		return nil, fmt.Errorf("official manifest signature: %d bytes, more than a signature can be", len(sig))
	}
	s, err := trust.VerifyIn(release.OfficialNamespace, raw, sig)
	if err != nil {
		return nil, fmt.Errorf("official manifest signature: %w", err)
	}
	m, err := parse(raw)
	if err != nil {
		return nil, err
	}
	if fp := release.Fingerprint(s.PublicKey); m.KeyFingerprint != fp {
		return nil, fmt.Errorf("official manifest: key_fingerprint %s, but it is signed by %s", m.KeyFingerprint, fp)
	}
	return m, nil
}

// parse decodes and validates a manifest. Unknown members are refused: a
// reader that skips what it does not understand has stopped reading what
// the signer meant.
func parse(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("official manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("official manifest: trailing data after the manifest")
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("official manifest: %w", err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Schema != Schema {
		return fmt.Errorf("schema %q, this anet reads %q", m.Schema, Schema)
	}
	if m.Seq == 0 {
		return errors.New("seq must be at least 1")
	}
	issued, err := time.Parse(timeLayout, m.IssuedAt)
	if err != nil {
		return fmt.Errorf("issued_at %q is not YYYY-MM-DDTHH:MM:SSZ", m.IssuedAt)
	}
	expires, err := time.Parse(timeLayout, m.ExpiresAt)
	if err != nil {
		return fmt.Errorf("expires_at %q is not YYYY-MM-DDTHH:MM:SSZ", m.ExpiresAt)
	}
	if !expires.After(issued) {
		return errors.New("expires_at is not after issued_at")
	}
	if !fpRe.MatchString(m.KeyFingerprint) {
		return fmt.Errorf("key_fingerprint %q is not SHA256:<base64>", m.KeyFingerprint)
	}
	if m.Agents == nil {
		return errors.New("agents is missing (an empty list is written [])")
	}
	if len(m.Agents) > MaxAgents {
		return fmt.Errorf("%d agents, more than %d", len(m.Agents), MaxAgents)
	}
	byAID := make(map[string]Entry, len(m.Agents))
	ids := make(map[string]bool, len(m.Agents))
	for i, e := range m.Agents {
		if err := e.validate(); err != nil {
			return fmt.Errorf("agents[%d]: %w", i, err)
		}
		if _, dup := byAID[e.AID]; dup {
			return fmt.Errorf("agents[%d]: aid %s is listed twice", i, e.AID)
		}
		if ids[e.ID] {
			return fmt.Errorf("agents[%d]: id %s is listed twice", i, e.ID)
		}
		byAID[e.AID], ids[e.ID] = e, true
	}
	m.expires, m.byAID = expires, byAID
	return nil
}

func (e Entry) validate() error {
	if !idRe.MatchString(e.ID) {
		return fmt.Errorf("id %q must match %s", e.ID, idRe)
	}
	if !aidRe.MatchString(e.AID) {
		return fmt.Errorf("aid %q is not an agent id", e.AID)
	}
	if e.Name == "" || len(e.Name) > maxNameBytes || !utf8.ValidString(e.Name) {
		return fmt.Errorf("name must be 1 to %d bytes of UTF-8", maxNameBytes)
	}
	for _, r := range e.Name {
		if unicode.IsControl(r) {
			return errors.New("name contains a control character")
		}
	}
	u, err := url.Parse(e.Hub)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("hub %q is not an http(s) base URL", e.Hub)
	}
	if e.Caps == nil {
		return errors.New("caps is missing (an empty list is written [])")
	}
	if len(e.Caps) > maxCaps {
		return fmt.Errorf("%d caps, more than %d", len(e.Caps), maxCaps)
	}
	for _, c := range e.Caps {
		if !capRe.MatchString(c) {
			return fmt.Errorf("capability %q is not a capability id", c)
		}
	}
	return nil
}

// Expires is expires_at.
func (m *Manifest) Expires() time.Time { return m.expires }

// CheckFresh refuses a manifest whose validity has run out. Expiry is what
// retires an official identity that must stop being vouched for — a key
// lost, a deployment withdrawn — from binaries that are never updated.
func (m *Manifest) CheckFresh(now time.Time) error {
	if !now.Before(m.expires) {
		return fmt.Errorf("%w: it expired at %s and it is now %s (update anet: `anet update`)",
			ErrExpired, m.ExpiresAt, now.UTC().Format(timeLayout))
	}
	return nil
}

// Lookup returns the official entry for aid, when the manifest lists
// exactly that AID and has not expired at now. It never matches on a name
// or id. A nil manifest — none verified — lists no one.
func (m *Manifest) Lookup(aid string, now time.Time) (Entry, bool) {
	if m == nil || aid == "" || !now.Before(m.expires) {
		return Entry{}, false
	}
	e, ok := m.byAID[aid]
	e.Caps = append([]string(nil), e.Caps...)
	return e, ok
}

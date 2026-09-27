// Package netcard assembles an anet node's A2A network card (A2A-DESIGN
// §10.1–§10.2): the AgentCard a hub verifies at /register and lists in its
// directory, as unsigned JSON in publish form. Signing, the seq counter and
// publication are the daemon's (internal/daemon/a2a_card.go); this package
// is pure functions so that the daemon and a contract test that
// cross-checks against a2a-go build the same bytes.
//
// Publish form (A2A specification §8.4.1; ANetCore a2acard.CheckPublishForm):
// REQUIRED members are present and non-empty, members holding a default
// value ("", false, [], {}) are left out, numbers are decimal strings, and
// capabilities.streaming and pushNotifications are always written. A card
// in this form yields one signing payload under the specification's rule,
// a2a-python's and a2a-go's, so it verifies in all three. Build checks the
// result with a2acard.CheckPublishForm and returns nothing else.
//
// The package imports no A2A library: the kernel must not (SI-8).
package netcard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/a2acard"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// ProtocolVersion is the A2A protocol version every interface on a network
// card states.
const ProtocolVersion = "1.0"

// DefaultDescription is the card description of a node whose operator set
// no summary. AgentCard.description is REQUIRED and "" is not in publish
// form.
const DefaultDescription = "An anet agent. It is reached through the anet relay binding, " +
	"with end-to-end encrypted, signed messages; a generic A2A client reaches it through a local anet daemon."

// Descriptions of the two extensions the kernel always writes.
const (
	cardExtDescription     = "anet identity (AID) and card sequence; the card is signed under the AID's current KEL key"
	evidenceExtDescription = "completed tasks carry a provider-signed receipt and anet.* evidence metadata"
)

// ErrNoSkill is returned by Build for a card without skills. A2A requires at
// least one, and a node with no public skill publishes no card (§10.1).
var ErrNoSkill = errors.New("netcard: no skill, so no card")

// Skill is one AgentSkill.
type Skill struct {
	ID          string
	Name        string
	Description string
	Tags        []string
	Examples    []string
	// InputModes and OutputModes override the card's defaults; nil, or
	// equal to the defaults, leaves them out.
	InputModes  []string
	OutputModes []string
}

// Input is everything a network card says.
type Input struct {
	AID string
	// Name and Description fall back to DefaultName and
	// DefaultDescription; both are cut to the a2acard limits.
	Name        string
	Description string
	Version     string
	// HubURL is the hub the card is published to; the relay interface is
	// RelayURL(HubURL).
	HubURL string
	// Seq, IssuedAtMs and NotBeforeMs are the anet-card params.
	Seq, IssuedAtMs, NotBeforeMs uint64
	// DefaultModes are defaultInputModes and defaultOutputModes; nil means
	// DefaultMode.
	DefaultModes []string
	Skills       []Skill
	// Extensions and Interfaces are module contributions (module.
	// CardContributor), each already accepted by Extension or Interface.
	Extensions []map[string]any
	Interfaces []map[string]any
}

// RelayURL is the url of the relay interface for a hub: its base URL with
// /relay appended, where the relay endpoints live (A2A-DESIGN §3.7).
func RelayURL(hubURL string) string { return trimHub(hubURL) + "/relay" }

// JKU is the jku of a card published to hubURL: the hub's JWKS for aid
// (A2A-DESIGN §10.3). A statement of the hub's, weaker than the KEL.
func JKU(hubURL, aid string) string {
	return trimHub(hubURL) + "/agents/" + url.PathEscape(aid) + "/jwks.json"
}

func trimHub(hubURL string) string { return strings.TrimRight(strings.TrimSpace(hubURL), "/") }

// DefaultName is the card name of a node whose operator set none.
func DefaultName(aid string) string {
	if len(aid) > 12 {
		aid = aid[:12]
	}
	return "anet agent " + aid
}

// DefaultMode is the default input and output mode of a network card: the
// media type of C1 arguments and effects. Every public skill is a
// capability call; natural-language tasks are not public (A2A-DESIGN §5).
const DefaultMode = "application/json"

// SkillFor describes capability capID as a skill: what the provider
// declares (provider.Described), with the gaps filled from the id
// (provider.SkillInfoOf).
func SkillFor(p provider.CapabilityProvider, capID string) Skill {
	info := provider.SkillInfoOf(p, capID)
	return Skill{ID: capID, Name: info.Name, Description: info.Description, Tags: info.Tags,
		Examples: info.Examples, InputModes: info.InputModes, OutputModes: info.OutputModes}
}

// normalizeSkill puts a skill in publish form: non-empty name, description
// and tags within the a2acard limits, and no list that is empty or repeats
// the card default.
func normalizeSkill(s Skill, defModes []string) Skill {
	if strings.TrimSpace(s.Name) == "" {
		s.Name = a2acard.DefaultSkillName(s.ID)
	}
	if strings.TrimSpace(s.Description) == "" {
		s.Description = a2acard.DefaultSkillDescription(s.ID)
	}
	s.Description = truncateUTF8(s.Description, a2acard.MaxDescriptionBytes)
	s.Tags = nonEmpty(s.Tags)
	if len(s.Tags) == 0 {
		s.Tags = a2acard.DefaultSkillTags(s.ID)
	}
	if len(s.Tags) > a2acard.MaxTagsPerSkill {
		s.Tags = s.Tags[:a2acard.MaxTagsPerSkill]
	}
	s.Examples = nonEmpty(s.Examples)
	s.InputModes = nonEmpty(s.InputModes)
	if equalStrings(s.InputModes, defModes) {
		s.InputModes = nil
	}
	s.OutputModes = nonEmpty(s.OutputModes)
	if equalStrings(s.OutputModes, defModes) {
		s.OutputModes = nil
	}
	return s
}

// card is the wire shape. Member order is irrelevant (the signed form is
// RFC 8785), the omitempty tags are what make it publish form.
type card struct {
	Name                string           `json:"name"`
	Description         string           `json:"description"`
	SupportedInterfaces []map[string]any `json:"supportedInterfaces"`
	Version             string           `json:"version"`
	Capabilities        caps             `json:"capabilities"`
	DefaultInputModes   []string         `json:"defaultInputModes"`
	DefaultOutputModes  []string         `json:"defaultOutputModes"`
	Skills              []skill          `json:"skills"`
}

type caps struct {
	// The relay binding is store-and-forward: no server-sent events and no
	// push callbacks reach a node through it. Both are proto3 optional and
	// written even when false, as a2a-go does.
	Streaming         bool             `json:"streaming"`
	PushNotifications bool             `json:"pushNotifications"`
	Extensions        []map[string]any `json:"extensions,omitempty"`
}

type skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// Build returns the unsigned network card. Interfaces come relay first,
// then contributions ordered by (protocolBinding, url); extensions come
// anet-card, anet-evidence, then contributions ordered by uri, a repeated
// uri kept once. The result passes a2acard.CheckPublishForm.
func Build(in Input) ([]byte, error) {
	if in.AID == "" || trimHub(in.HubURL) == "" {
		return nil, errors.New("netcard: AID and hub are required")
	}
	if len(in.Skills) == 0 {
		return nil, ErrNoSkill
	}
	if len(in.Skills) > a2acard.MaxSkills {
		return nil, fmt.Errorf("netcard: %d skills, a card holds at most %d", len(in.Skills), a2acard.MaxSkills)
	}
	defModes := nonEmpty(in.DefaultModes)
	if len(defModes) == 0 {
		defModes = []string{DefaultMode}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = DefaultName(in.AID)
	}
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = DefaultDescription
	}
	version := strings.TrimSpace(in.Version)
	if version == "" {
		version = "0"
	}
	c := card{
		Name:        truncateUTF8(name, a2acard.MaxNameBytes),
		Description: truncateUTF8(desc, a2acard.MaxDescriptionBytes),
		SupportedInterfaces: []map[string]any{{
			"url": RelayURL(in.HubURL), "protocolBinding": a2acard.BindingRelayURI,
			"protocolVersion": ProtocolVersion, "tenant": in.AID,
		}},
		Version:            version,
		DefaultInputModes:  defModes,
		DefaultOutputModes: defModes,
		Skills:             make([]skill, 0, len(in.Skills)),
	}
	seenSkill := map[string]bool{}
	for _, s := range in.Skills {
		if s.ID == "" || seenSkill[s.ID] {
			return nil, fmt.Errorf("netcard: skill id %q is empty or repeated", s.ID)
		}
		seenSkill[s.ID] = true
		s = normalizeSkill(s, defModes)
		c.Skills = append(c.Skills, skill{ID: s.ID, Name: s.Name, Description: s.Description, Tags: s.Tags,
			Examples: s.Examples, InputModes: s.InputModes, OutputModes: s.OutputModes})
	}

	ifaces := append([]map[string]any(nil), in.Interfaces...)
	sort.SliceStable(ifaces, func(i, j int) bool {
		bi, bj := ifaces[i]["protocolBinding"].(string), ifaces[j]["protocolBinding"].(string)
		if bi != bj {
			return bi < bj
		}
		return ifaces[i]["url"].(string) < ifaces[j]["url"].(string)
	})
	c.SupportedInterfaces = append(c.SupportedInterfaces, ifaces...)

	c.Capabilities.Extensions = []map[string]any{
		{"uri": a2acard.ExtCardURI, "description": cardExtDescription, "params": map[string]any{
			"aid":       in.AID,
			"seq":       strconv.FormatUint(in.Seq, 10),
			"issuedAt":  strconv.FormatUint(in.IssuedAtMs, 10),
			"notBefore": strconv.FormatUint(in.NotBeforeMs, 10),
		}},
		{"uri": module.ExtEvidenceURI, "description": evidenceExtDescription},
	}
	exts := append([]map[string]any(nil), in.Extensions...)
	sort.SliceStable(exts, func(i, j int) bool { return exts[i]["uri"].(string) < exts[j]["uri"].(string) })
	seenExt := map[string]bool{a2acard.ExtCardURI: true, module.ExtEvidenceURI: true}
	for _, e := range exts {
		uri := e["uri"].(string)
		if seenExt[uri] {
			continue
		}
		seenExt[uri] = true
		c.Capabilities.Extensions = append(c.Capabilities.Extensions, e)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	if err := a2acard.CheckPublishForm(out); err != nil {
		return nil, fmt.Errorf("netcard: %w", err)
	}
	return out, nil
}

// Extension accepts one contributed extension and returns it in publish
// form: uri, then description, required and params only when they hold
// something ("required": false and "description": "" disappear). It
// refuses a kernel URI (anet-card, anet-evidence), an unknown member, and
// params holding a number, null, "", [] or {} anywhere: numbers on the
// card are decimal strings, and the others are removed by some verifiers
// and kept by others, so no signature over them verifies everywhere.
func Extension(in map[string]any) (map[string]any, error) {
	uri, _ := in["uri"].(string)
	if strings.TrimSpace(uri) == "" {
		return nil, errors.New("extension has no uri")
	}
	if uri == a2acard.ExtCardURI || uri == module.ExtEvidenceURI {
		return nil, fmt.Errorf("%s is written by the kernel", uri)
	}
	out := map[string]any{"uri": uri}
	for k, v := range in {
		switch k {
		case "uri":
		case "description":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s: description is not a string", uri)
			}
			if s != "" {
				out[k] = s
			}
		case "required":
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("%s: required is not a bool", uri)
			}
			if b {
				out[k] = true
			}
		case "params":
			if v == nil {
				continue
			}
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: params is not an object", uri)
			}
			if len(m) == 0 {
				continue
			}
			if why := paramsProblem(m, "params"); why != "" {
				return nil, fmt.Errorf("%s: %s", uri, why)
			}
			out[k] = m
		default:
			return nil, fmt.Errorf("%s: unknown member %q", uri, k)
		}
	}
	return out, nil
}

// Interface accepts one contributed interface for the card of aid and
// returns it in publish form (tenant only when set). It refuses the relay
// binding (the kernel writes it), an anet binding whose tenant is not aid,
// an unknown member, and a URL that is not absolute or whose host is
// loopback, unspecified or a local socket: 127.0.0.1 never enters a
// network card (§10.1).
func Interface(in map[string]any, aid string) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range []string{"url", "protocolBinding", "protocolVersion"} {
		s, _ := in[k].(string)
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("interface %s is missing or empty", k)
		}
		out[k] = s
	}
	for k, v := range in {
		switch k {
		case "url", "protocolBinding", "protocolVersion":
		case "tenant":
			s, ok := v.(string)
			if !ok {
				return nil, errors.New("interface tenant is not a string")
			}
			if s != "" {
				out[k] = s
			}
		default:
			return nil, fmt.Errorf("interface: unknown member %q", k)
		}
	}
	switch out["protocolBinding"] {
	case a2acard.BindingRelayURI:
		return nil, errors.New("the relay interface is written by the kernel")
	case module.BindingP2PURI:
		if out["tenant"] != aid {
			return nil, fmt.Errorf("a direct interface routes by tenant, which must be %s", aid)
		}
	}
	if why := unpublishableURL(out["url"].(string)); why != "" {
		return nil, errors.New(why)
	}
	return out, nil
}

// unpublishableURL says why an interface URL must not enter a network
// card, or "" when it may.
func unpublishableURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return strconv.Quote(raw) + " is not an absolute URL"
	}
	if u.Scheme == "unix" || u.Host == "" {
		return strconv.Quote(raw) + " names no network host"
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return strconv.Quote(raw) + " is a loopback address"
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return strconv.Quote(raw) + " is a loopback or unspecified address"
	}
	return ""
}

// paramsProblem says what in a params value is not in publish form, or "".
func paramsProblem(v any, path string) string {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return path + " is an empty object"
		}
		for k, e := range t {
			if why := paramsProblem(e, path+"."+k); why != "" {
				return why
			}
		}
	case []any:
		if len(t) == 0 {
			return path + " is an empty array"
		}
		for i, e := range t {
			if why := paramsProblem(e, path+"["+strconv.Itoa(i)+"]"); why != "" {
				return why
			}
		}
	case []map[string]any:
		if len(t) == 0 {
			return path + " is an empty array"
		}
		for i, e := range t {
			if why := paramsProblem(e, path+"["+strconv.Itoa(i)+"]"); why != "" {
				return why
			}
		}
	case []string:
		if len(t) == 0 {
			return path + " is an empty array"
		}
		for i, e := range t {
			if e == "" {
				return path + "[" + strconv.Itoa(i) + "] is an empty string"
			}
		}
	case string:
		if t == "" {
			return path + " is an empty string"
		}
	case bool:
	case nil:
		return path + " is null"
	default:
		return fmt.Sprintf("%s is a %T; numbers on the card are decimal strings", path, v)
	}
	return ""
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

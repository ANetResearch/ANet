package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"
)

// a2a.card.validate: an A2A 1.0 AgentCard checker.
//
// The structure rules come from a2a.proto (AgentCard and the messages it
// holds), transcribed into protoMessages below. The card is checked the
// way a proto-based implementation will read it: required fields, types,
// oneofs, and members the schema does not have (which such an
// implementation drops, so a signature over them cannot verify there).
//
// Signatures are verified only against keys the caller supplies. Fetching
// jku would make this agent a URL fetcher, which the official agents are
// not (A2A-DESIGN §15), and a key fetched from the card's own jku proves
// only that the card and the key came from the same place.

type cardArgs struct {
	Card     json.RawMessage `json:"card"`
	CardJSON *string         `json:"card_json"`
	// JWKS is a JSON Web Key Set ({"keys": [...]}) holding the signing keys.
	JWKS json.RawMessage `json:"jwks"`
	// KEL is an anet key event log (identity.MarshalKEL, base64), for
	// signatures whose kid is did:anet:<AID>#<seq>.
	KEL string `json:"kel"`
	// NowMS is the time the anet rules are checked at (notBefore); the
	// default is the time of the call.
	NowMS *int64 `json:"now_ms"`
}

type sigReport struct {
	Index     int    `json:"index"`
	Alg       string `json:"alg,omitempty"`
	Kid       string `json:"kid,omitempty"`
	Jku       string `json:"jku,omitempty"`
	Typ       string `json:"typ,omitempty"`
	Result    string `json:"result"` // verified, invalid, unverifiable, malformed
	Payload   string `json:"payload,omitempty"`
	KeySource string `json:"key_source,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type payloadReport struct {
	AsGivenSHA256         string   `json:"as_given_sha256"`
	DefaultsRemovedSHA256 string   `json:"defaults_removed_sha256"`
	Differ                bool     `json:"differ"`
	DefaultsPresent       []string `json:"defaults_present,omitempty"`
}

type anetReport struct {
	// Result is accepted, rejected or unverifiable (the card passed every
	// rule that needs no key, and no KEL was supplied).
	Result string `json:"result"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	AID    string `json:"aid,omitempty"`
	Seq    string `json:"seq,omitempty"`
}

type cardResult struct {
	Valid      bool           `json:"valid"`
	Profile    string         `json:"profile"`
	Errors     int            `json:"errors"`
	Warnings   int            `json:"warnings"`
	Issues     []issue        `json:"issues"`
	Truncated  bool           `json:"truncated,omitempty"`
	Signatures []sigReport    `json:"signatures"`
	Payload    *payloadReport `json:"payload,omitempty"`
	Anet       *anetReport    `json:"anet,omitempty"`
}

// Bounds on the work one check may do. Each signature is verified with
// every candidate key over two payloads, each a hash of the whole card, and
// each did:anet key is found by replaying the KEL: with no bound, a 160 KiB
// card of 700 signatures and 16 keys ran for fifteen seconds. Real cards
// carry one or two signatures and KELs of a few events; anet hubs admit at
// most a2acard.MaxSignatures signatures, and so many are checked here.
const (
	maxCheckedSignatures = a2acard.MaxSignatures
	maxKELEvents         = 256
)

func handleCardValidate(ctx context.Context, e *env, body []byte) (any, error) {
	var a cardArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	var raw []byte
	switch {
	case a.CardJSON != nil && len(a.Card) > 0:
		return nil, badArgs("give \"card\" or \"card_json\", not both")
	case a.CardJSON != nil:
		raw = []byte(*a.CardJSON)
	case len(a.Card) > 0:
		raw = a.Card
	default:
		return nil, badArgs("\"card\" (object) or \"card_json\" (text) is required")
	}
	var kel []identity.SignedEvent
	if a.KEL != "" {
		b, err := decodeBase64(a.KEL)
		if err != nil {
			return nil, badArgs("\"kel\": %v", err)
		}
		if kel, err = identity.UnmarshalKEL(b); err != nil {
			return nil, badArgs("\"kel\" is not an anet KEL: %v", err)
		}
		if len(kel) > maxKELEvents {
			return nil, badArgs("\"kel\" has %d events; at most %d are checked", len(kel), maxKELEvents)
		}
	}
	var keys []jwk
	if len(a.JWKS) > 0 {
		var err error
		if keys, err = parseJWKS(a.JWKS); err != nil {
			return nil, badArgs("\"jwks\": %v", err)
		}
	}
	now := e.now().UnixMilli()
	if a.NowMS != nil {
		now = *a.NowMS
	}
	return checkCardCtx(ctx, raw, keys, kel, now)
}

func checkCard(raw []byte, keys []jwk, kel []identity.SignedEvent, nowMS int64) *cardResult {
	res, _ := checkCardCtx(context.Background(), raw, keys, kel, nowMS)
	return res
}

// checkCardCtx checks a card, and gives up with errBudget when ctx is done
// between two signatures.
func checkCardCtx(ctx context.Context, raw []byte, keys []jwk, kel []identity.SignedEvent, nowMS int64) (*cardResult, error) {
	res := &cardResult{Profile: "a2a", Signatures: []sigReport{}}
	var is issues
	defer func() {
		res.Issues, res.Truncated = is.out(), is.truncated
		for _, i := range res.Issues {
			switch i.Severity {
			case sevError:
				res.Errors++
			case sevWarning:
				res.Warnings++
			}
		}
		res.Valid = res.Errors == 0
	}()

	if _, err := a2acard.Canonicalize(raw); err != nil {
		// Duplicate member names, lone surrogates, numbers out of range:
		// JSON that different parsers read differently cannot carry a
		// signature every verifier agrees on.
		is.add(sevError, "", "not_i_json", "the card is not I-JSON (RFC 7493), so its canonical form is undefined: %v", err)
	}
	v, _, serr := parseStrictJSON(string(raw))
	if serr != nil {
		is.add(sevError, "", "syntax", "not JSON: %s at byte %d", serr.msg, serr.offset)
		return res, nil
	}
	card, ok := v.(map[string]any)
	if !ok {
		is.add(sevError, "", "type", "an AgentCard is a JSON object")
		return res, nil
	}
	if len(raw) > a2acard.MaxCardBytes {
		is.add(sevWarning, "", "size", "the card is %d bytes; anet hubs admit cards up to %d", len(raw), a2acard.MaxCardBytes)
	}

	checkMessage(card, "AgentCard", "", &is)
	checkLegacyFields(card, &is)
	checkCardSemantics(card, &is)

	// Default values (§8.4.1) and the two payloads a verifier may compute.
	stripped, removed := stripDefaults(card, "AgentCard", "")
	delete(stripped, "signatures")
	asGiven, err1 := a2acard.SigningPayload(raw)
	sb, _ := json.Marshal(stripped)
	strippedPayload, err2 := a2acard.Canonicalize(sb)
	if err1 == nil && err2 == nil {
		h1, h2 := sha256.Sum256(asGiven), sha256.Sum256(strippedPayload)
		res.Payload = &payloadReport{AsGivenSHA256: hex.EncodeToString(h1[:]), DefaultsRemovedSHA256: hex.EncodeToString(h2[:]),
			Differ: h1 != h2}
		if len(removed) > 20 {
			removed = removed[:20]
		}
		res.Payload.DefaultsPresent = removed
		if h1 != h2 {
			is.add(sevWarning, "", "defaults_present",
				"the card carries fields at their default values (%s); A2A §8.4.1 removes them before signing and a2a-go does not, "+
					"so a signature over one form fails in verifiers that compute the other", strings.Join(removed, ", "))
		}
	}

	// Signatures.
	sigs, _ := card["signatures"].([]any)
	for i, s := range sigs {
		if i == maxCheckedSignatures {
			is.add(sevWarning, "/signatures", "signatures_not_checked",
				"the card carries %d signatures; only the first %d were checked", len(sigs), maxCheckedSignatures)
			break
		}
		if ctx.Err() != nil {
			return res, errBudget
		}
		res.Signatures = append(res.Signatures, checkSignature(i, s, asGiven, strippedPayload, keys, kel, &is))
	}
	if len(sigs) == 0 {
		is.add(sevInfo, "/signatures", "unsigned", "the card is not signed; A2A clients SHOULD verify at least one signature before trusting a card")
	}
	if ctx.Err() != nil {
		return res, errBudget
	}

	// The anet admission rules, for a card that claims to be an anet card.
	if hasExtension(card, a2acard.ExtCardURI) {
		res.Profile = "anet"
		res.Anet = checkAnetRules(raw, kel, nowMS, &is)
	}
	return res, nil
}

// Proto field kinds.
type fieldKind uint8

const (
	kString fieldKind = iota
	kBool
	kMessage
	kRepeatedString
	kRepeatedMessage
	kMapMessage
	kMapString
	kStruct
)

type protoField struct {
	kind     fieldKind
	required bool // field_behavior REQUIRED
	optional bool // proto3 "optional": explicit presence
	msg      string
}

// protoMessages is a2a.proto (A2A 1.0) for AgentCard and what it holds, by
// JSON member name. oneofs lists the members of which exactly one must be
// set.
var protoMessages = map[string]map[string]protoField{
	"AgentCard": {
		"name":                 {kind: kString, required: true},
		"description":          {kind: kString, required: true},
		"supportedInterfaces":  {kind: kRepeatedMessage, required: true, msg: "AgentInterface"},
		"provider":             {kind: kMessage, msg: "AgentProvider"},
		"version":              {kind: kString, required: true},
		"documentationUrl":     {kind: kString, optional: true},
		"capabilities":         {kind: kMessage, required: true, msg: "AgentCapabilities"},
		"securitySchemes":      {kind: kMapMessage, msg: "SecurityScheme"},
		"securityRequirements": {kind: kRepeatedMessage, msg: "SecurityRequirement"},
		"defaultInputModes":    {kind: kRepeatedString, required: true},
		"defaultOutputModes":   {kind: kRepeatedString, required: true},
		"skills":               {kind: kRepeatedMessage, required: true, msg: "AgentSkill"},
		"signatures":           {kind: kRepeatedMessage, msg: "AgentCardSignature"},
		"iconUrl":              {kind: kString, optional: true},
	},
	"AgentInterface": {
		"url":             {kind: kString, required: true},
		"protocolBinding": {kind: kString, required: true},
		"tenant":          {kind: kString},
		"protocolVersion": {kind: kString, required: true},
	},
	"AgentProvider": {
		"url":          {kind: kString, required: true},
		"organization": {kind: kString, required: true},
	},
	"AgentCapabilities": {
		"streaming":         {kind: kBool, optional: true},
		"pushNotifications": {kind: kBool, optional: true},
		"extensions":        {kind: kRepeatedMessage, msg: "AgentExtension"},
		"extendedAgentCard": {kind: kBool, optional: true},
	},
	"AgentExtension": {
		"uri":         {kind: kString},
		"description": {kind: kString},
		"required":    {kind: kBool},
		"params":      {kind: kStruct},
	},
	"AgentSkill": {
		"id":                   {kind: kString, required: true},
		"name":                 {kind: kString, required: true},
		"description":          {kind: kString, required: true},
		"tags":                 {kind: kRepeatedString, required: true},
		"examples":             {kind: kRepeatedString},
		"inputModes":           {kind: kRepeatedString},
		"outputModes":          {kind: kRepeatedString},
		"securityRequirements": {kind: kRepeatedMessage, msg: "SecurityRequirement"},
	},
	"AgentCardSignature": {
		"protected": {kind: kString, required: true},
		"signature": {kind: kString, required: true},
		"header":    {kind: kStruct},
	},
	"SecurityRequirement": {
		"schemes": {kind: kMapMessage, msg: "StringList"},
	},
	"StringList": {
		"list": {kind: kRepeatedString},
	},
	"SecurityScheme": {
		"apiKeySecurityScheme":        {kind: kMessage, msg: "APIKeySecurityScheme"},
		"httpAuthSecurityScheme":      {kind: kMessage, msg: "HTTPAuthSecurityScheme"},
		"oauth2SecurityScheme":        {kind: kMessage, msg: "OAuth2SecurityScheme"},
		"openIdConnectSecurityScheme": {kind: kMessage, msg: "OpenIdConnectSecurityScheme"},
		"mtlsSecurityScheme":          {kind: kMessage, msg: "MutualTlsSecurityScheme"},
	},
	"APIKeySecurityScheme": {
		"description": {kind: kString},
		"location":    {kind: kString, required: true},
		"name":        {kind: kString, required: true},
	},
	"HTTPAuthSecurityScheme": {
		"description":  {kind: kString},
		"scheme":       {kind: kString, required: true},
		"bearerFormat": {kind: kString},
	},
	"OAuth2SecurityScheme": {
		"description":       {kind: kString},
		"flows":             {kind: kMessage, required: true, msg: "OAuthFlows"},
		"oauth2MetadataUrl": {kind: kString},
	},
	"OpenIdConnectSecurityScheme": {
		"description":      {kind: kString},
		"openIdConnectUrl": {kind: kString, required: true},
	},
	"MutualTlsSecurityScheme": {
		"description": {kind: kString},
	},
	"OAuthFlows": {
		"authorizationCode": {kind: kMessage, msg: "AuthorizationCodeOAuthFlow"},
		"clientCredentials": {kind: kMessage, msg: "ClientCredentialsOAuthFlow"},
		"implicit":          {kind: kMessage, msg: "ImplicitOAuthFlow"},
		"password":          {kind: kMessage, msg: "PasswordOAuthFlow"},
		"deviceCode":        {kind: kMessage, msg: "DeviceCodeOAuthFlow"},
	},
	"AuthorizationCodeOAuthFlow": {
		"authorizationUrl": {kind: kString, required: true},
		"tokenUrl":         {kind: kString, required: true},
		"refreshUrl":       {kind: kString},
		"scopes":           {kind: kMapString, required: true},
		"pkceRequired":     {kind: kBool},
	},
	"ClientCredentialsOAuthFlow": {
		"tokenUrl":   {kind: kString, required: true},
		"refreshUrl": {kind: kString},
		"scopes":     {kind: kMapString, required: true},
	},
	"ImplicitOAuthFlow": {
		"authorizationUrl": {kind: kString},
		"refreshUrl":       {kind: kString},
		"scopes":           {kind: kMapString},
	},
	"PasswordOAuthFlow": {
		"tokenUrl":   {kind: kString},
		"refreshUrl": {kind: kString},
		"scopes":     {kind: kMapString},
	},
	"DeviceCodeOAuthFlow": {
		"deviceAuthorizationUrl": {kind: kString, required: true},
		"tokenUrl":               {kind: kString, required: true},
		"refreshUrl":             {kind: kString},
		"scopes":                 {kind: kMapString, required: true},
	},
}

// oneofMessages are messages whose members form a single oneof.
var oneofMessages = map[string]bool{"SecurityScheme": true, "OAuthFlows": true}

// deprecatedFields are members a2a.proto marks deprecated.
var deprecatedFields = map[string]map[string]string{
	"OAuthFlows": {"implicit": "use authorizationCode with PKCE", "password": "use authorizationCode with PKCE or deviceCode"},
}

// checkMessage checks obj against message msg of protoMessages.
func checkMessage(obj map[string]any, msg, path string, is *issues) {
	fields := protoMessages[msg]
	for _, name := range sortedKeys(obj) {
		if _, known := fields[name]; !known {
			is.add(sevWarning, ptr(path, name), "unknown_field",
				"%s has no member %q; an implementation that parses the card into the A2A schema drops it "+
					"(and a signature that covers it then fails there)", msg, name)
		}
	}
	set := 0
	for _, name := range sortedKeys(fields) {
		f := fields[name]
		p := ptr(path, name)
		val, present := obj[name]
		if present && val == nil {
			// JSON null is the default value in proto JSON.
			present = false
			is.add(sevWarning, p, "null", "%s is null; proto JSON reads null as the default value", name)
		}
		if !present {
			if f.required {
				is.add(sevError, p, "required", "%s.%s is required", msg, name)
			}
			continue
		}
		set++
		if why, dep := deprecatedFields[msg][name]; dep {
			is.add(sevWarning, p, "deprecated", "%s is deprecated in A2A 1.0: %s", name, why)
		}
		checkField(val, f, name, p, is)
	}
	if oneofMessages[msg] {
		switch {
		case set == 0:
			is.add(sevError, path, "oneof", "%s must set exactly one of %s", msg, strings.Join(sortedKeys(fields), ", "))
		case set > 1:
			is.add(sevError, path, "oneof", "%s sets %d members of a oneof; exactly one is allowed", msg, set)
		}
	}
}

func checkField(val any, f protoField, name, p string, is *issues) {
	switch f.kind {
	case kString:
		s, ok := val.(string)
		if !ok {
			is.add(sevError, p, "type", "%s must be a string", name)
		} else if f.required && s == "" {
			is.add(sevWarning, p, "empty", "%s is required but empty", name)
		}
	case kBool:
		if _, ok := val.(bool); !ok {
			is.add(sevError, p, "type", "%s must be a boolean", name)
		}
	case kMessage:
		m, ok := val.(map[string]any)
		if !ok {
			is.add(sevError, p, "type", "%s must be an object (%s)", name, f.msg)
			return
		}
		checkMessage(m, f.msg, p, is)
	case kStruct:
		if _, ok := val.(map[string]any); !ok {
			is.add(sevError, p, "type", "%s must be an object", name)
		}
	case kRepeatedString:
		arr, ok := val.([]any)
		if !ok {
			is.add(sevError, p, "type", "%s must be an array of strings", name)
			return
		}
		if f.required && len(arr) == 0 {
			is.add(sevWarning, p, "empty", "%s is required but empty", name)
		}
		for i, e := range arr {
			if s, ok := e.(string); !ok {
				is.add(sevError, ptr(p, i), "type", "%s[%d] must be a string", name, i)
			} else if s == "" {
				is.add(sevWarning, ptr(p, i), "empty", "%s[%d] is empty", name, i)
			}
		}
	case kRepeatedMessage:
		arr, ok := val.([]any)
		if !ok {
			is.add(sevError, p, "type", "%s must be an array of %s", name, f.msg)
			return
		}
		if f.required && len(arr) == 0 {
			is.add(sevWarning, p, "empty", "%s is required but empty", name)
		}
		for i, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				is.add(sevError, ptr(p, i), "type", "%s[%d] must be an object (%s)", name, i, f.msg)
				continue
			}
			checkMessage(m, f.msg, ptr(p, i), is)
		}
	case kMapMessage:
		m, ok := val.(map[string]any)
		if !ok {
			is.add(sevError, p, "type", "%s must be an object of %s", name, f.msg)
			return
		}
		for _, k := range sortedKeys(m) {
			sub, ok := m[k].(map[string]any)
			if !ok {
				is.add(sevError, ptr(p, k), "type", "%s[%q] must be an object (%s)", name, k, f.msg)
				continue
			}
			checkMessage(sub, f.msg, ptr(p, k), is)
		}
	case kMapString:
		m, ok := val.(map[string]any)
		if !ok {
			is.add(sevError, p, "type", "%s must be an object of strings", name)
			return
		}
		for _, k := range sortedKeys(m) {
			if _, ok := m[k].(string); !ok {
				is.add(sevError, ptr(p, k), "type", "%s[%q] must be a string", name, k)
			}
		}
	}
}

// legacyFields are A2A 0.3 AgentCard members replaced in 1.0.
var legacyFields = map[string]string{
	"url":                               "use supportedInterfaces[].url",
	"preferredTransport":                "use supportedInterfaces[].protocolBinding (the first entry is preferred)",
	"additionalInterfaces":              "use supportedInterfaces",
	"protocolVersion":                   "use supportedInterfaces[].protocolVersion",
	"supportsAuthenticatedExtendedCard": "use capabilities.extendedAgentCard",
	"security":                          "use securityRequirements",
}

func checkLegacyFields(card map[string]any, is *issues) {
	for _, k := range sortedKeys(legacyFields) {
		if _, ok := card[k]; ok {
			is.add(sevWarning, ptr("", k), "a2a_0_3_field", "%q is an A2A 0.3 field: %s", k, legacyFields[k])
		}
	}
}

var coreBindings = map[string]bool{"JSONRPC": true, "GRPC": true, "HTTP+JSON": true}

func checkCardSemantics(card map[string]any, is *issues) {
	if ifaces, ok := card["supportedInterfaces"].([]any); ok {
		for i, it := range ifaces {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			p := ptr("/supportedInterfaces", i)
			binding, _ := m["protocolBinding"].(string)
			rawURL, _ := m["url"].(string)
			if binding != "" && !coreBindings[binding] {
				if _, err := url.Parse(binding); err == nil && strings.Contains(binding, "://") {
					is.add(sevInfo, ptr(p, "protocolBinding"), "custom_binding", "custom protocol binding %s", binding)
				} else {
					is.add(sevWarning, ptr(p, "protocolBinding"), "unknown_binding",
						"protocolBinding %q is not JSONRPC, GRPC or HTTP+JSON; a custom binding should be named by a URI", binding)
				}
			}
			if pv, _ := m["protocolVersion"].(string); pv != "" && !isMajorMinor(pv) {
				is.add(sevWarning, ptr(p, "protocolVersion"), "protocol_version", "protocolVersion %q is not of the form major.minor", pv)
			}
			if rawURL != "" && binding != "GRPC" {
				u, err := url.Parse(rawURL)
				switch {
				case err != nil || !u.IsAbs():
					is.add(sevError, ptr(p, "url"), "url", "url %q is not an absolute URL", rawURL)
				case u.Scheme == "http" && !loopbackHost(u.Host):
					is.add(sevWarning, ptr(p, "url"), "insecure_url", "url %q is not https; A2A requires HTTPS in production", rawURL)
				}
			}
			if binding == a2acard.BindingRelayURI {
				if t, _ := m["tenant"].(string); t == "" {
					is.add(sevError, ptr(p, "tenant"), "tenant", "an anet relay interface routes by tenant; tenant (the AID) is required")
				}
			}
		}
	}
	for _, f := range []string{"defaultInputModes", "defaultOutputModes"} {
		checkModes(card[f], ptr("", f), is)
	}
	schemes, _ := card["securitySchemes"].(map[string]any)
	checkRequirements := func(v any, p string) {
		reqs, _ := v.([]any)
		for i, r := range reqs {
			m, _ := r.(map[string]any)
			sch, _ := m["schemes"].(map[string]any)
			for _, name := range sortedKeys(sch) {
				if _, ok := schemes[name]; !ok {
					is.add(sevError, ptr(ptr(ptr(p, i), "schemes"), name), "undefined_scheme",
						"security requirement names %q, which securitySchemes does not define", name)
				}
			}
		}
	}
	checkRequirements(card["securityRequirements"], "/securityRequirements")
	if skills, ok := card["skills"].([]any); ok {
		seen := map[string]int{}
		for i, s := range skills {
			m, ok := s.(map[string]any)
			if !ok {
				continue
			}
			p := ptr("/skills", i)
			if id, _ := m["id"].(string); id != "" {
				if j, dup := seen[id]; dup {
					is.add(sevError, ptr(p, "id"), "duplicate_skill", "skill id %q is also skills[%d]", id, j)
				}
				seen[id] = i
			}
			if tags, ok := m["tags"].([]any); ok && len(tags) > a2acard.MaxTagsPerSkill {
				is.add(sevWarning, ptr(p, "tags"), "tags", "%d tags; anet hubs index at most %d per skill", len(tags), a2acard.MaxTagsPerSkill)
			}
			checkModes(m["inputModes"], ptr(p, "inputModes"), is)
			checkModes(m["outputModes"], ptr(p, "outputModes"), is)
			checkRequirements(m["securityRequirements"], ptr(p, "securityRequirements"))
		}
		if len(skills) > a2acard.MaxSkills {
			is.add(sevWarning, "/skills", "skills", "%d skills; anet hubs admit at most %d", len(skills), a2acard.MaxSkills)
		}
	}
	if caps, ok := card["capabilities"].(map[string]any); ok {
		exts, _ := caps["extensions"].([]any)
		seen := map[string]int{}
		for i, e := range exts {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			p := ptr("/capabilities/extensions", i)
			uri, _ := m["uri"].(string)
			if uri == "" {
				is.add(sevError, ptr(p, "uri"), "extension_uri", "an extension without a uri cannot be activated or understood")
				continue
			}
			if j, dup := seen[uri]; dup {
				is.add(sevWarning, ptr(p, "uri"), "duplicate_extension", "extension %s is also extensions[%d]", uri, j)
			}
			seen[uri] = i
			if uri == x402ExtensionURI {
				if req, _ := m["required"].(bool); !req {
					is.add(sevInfo, ptr(p, "required"), "x402_optional",
						"a2a-x402 recommends required: true when the agent's skills are paid (spec v0.2 §3.1)")
				}
			}
		}
	}
	if n, _ := card["name"].(string); len(n) > a2acard.MaxNameBytes {
		is.add(sevWarning, "/name", "name", "name is %d bytes; anet hubs admit at most %d", len(n), a2acard.MaxNameBytes)
	}
}

func checkModes(v any, p string, is *issues) {
	arr, _ := v.([]any)
	for i, e := range arr {
		s, _ := e.(string)
		if s != "" && !strings.Contains(s, "/") {
			is.add(sevWarning, ptr(p, i), "media_type", "%q is not a media type (type/subtype)", s)
		}
	}
}

func isMajorMinor(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func hasExtension(card map[string]any, uri string) bool {
	caps, _ := card["capabilities"].(map[string]any)
	exts, _ := caps["extensions"].([]any)
	for _, e := range exts {
		if m, ok := e.(map[string]any); ok && m["uri"] == uri {
			return true
		}
	}
	return false
}

// stripDefaults returns a copy of obj without the members A2A §8.4.1 says
// to drop: implicit-presence fields at their default value ("" / false / 0 /
// empty list / empty map), and nulls. Required fields and fields with
// explicit presence (proto3 optional, messages, oneof members) are kept.
// Unknown members and Struct contents are kept as they are. removed lists
// the JSON Pointers of what was dropped.
func stripDefaults(obj map[string]any, msg, path string) (map[string]any, []string) {
	fields := protoMessages[msg]
	out := make(map[string]any, len(obj))
	var removed []string
	for _, name := range sortedKeys(obj) {
		val := obj[name]
		f, known := fields[name]
		p := ptr(path, name)
		if !known {
			out[name] = val
			continue
		}
		if val == nil {
			if !f.required {
				removed = append(removed, p)
				continue
			}
			out[name] = val
			continue
		}
		if !f.required && !f.optional && !oneofMessages[msg] && isDefault(val, f.kind) {
			removed = append(removed, p)
			continue
		}
		switch f.kind {
		case kMessage:
			if m, ok := val.(map[string]any); ok {
				s, r := stripDefaults(m, f.msg, p)
				out[name], removed = s, append(removed, r...)
				continue
			}
		case kRepeatedMessage:
			if arr, ok := val.([]any); ok {
				cp := make([]any, len(arr))
				for i, e := range arr {
					if m, ok := e.(map[string]any); ok {
						s, r := stripDefaults(m, f.msg, ptr(p, i))
						cp[i], removed = s, append(removed, r...)
					} else {
						cp[i] = e
					}
				}
				out[name] = cp
				continue
			}
		case kMapMessage:
			if m, ok := val.(map[string]any); ok {
				cp := make(map[string]any, len(m))
				for _, k := range sortedKeys(m) {
					if sm, ok := m[k].(map[string]any); ok {
						s, r := stripDefaults(sm, f.msg, ptr(p, k))
						cp[k], removed = s, append(removed, r...)
					} else {
						cp[k] = m[k]
					}
				}
				out[name] = cp
				continue
			}
		}
		out[name] = val
	}
	return out, removed
}

func isDefault(v any, k fieldKind) bool {
	switch k {
	case kString:
		return v == ""
	case kBool:
		return v == false
	case kRepeatedString, kRepeatedMessage:
		a, ok := v.([]any)
		return ok && len(a) == 0
	case kMapMessage, kMapString:
		m, ok := v.(map[string]any)
		return ok && len(m) == 0
	}
	return false
}

// jwk is one public key from a JWKS.
type jwk struct {
	kid string
	kty string
	crv string
	pub crypto.PublicKey
}

func parseJWKS(raw json.RawMessage) ([]jwk, error) {
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, err
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("no keys")
	}
	if len(set.Keys) > 16 {
		return nil, errors.New("more than 16 keys")
	}
	var out []jwk
	for i, k := range set.Keys {
		key, err := parseJWK(k)
		if err != nil {
			return nil, fmt.Errorf("keys[%d]: %v", i, err)
		}
		out = append(out, key)
	}
	return out, nil
}

func b64field(k map[string]any, name string) ([]byte, error) {
	s, _ := k[name].(string)
	if s == "" {
		return nil, fmt.Errorf("%q is missing", name)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("%q is not base64url", name)
	}
	return b, nil
}

func parseJWK(k map[string]any) (jwk, error) {
	out := jwk{}
	out.kid, _ = k["kid"].(string)
	out.kty, _ = k["kty"].(string)
	out.crv, _ = k["crv"].(string)
	switch out.kty {
	case "OKP":
		if out.crv != "Ed25519" {
			return out, fmt.Errorf("OKP curve %q is not supported (Ed25519 only)", out.crv)
		}
		x, err := b64field(k, "x")
		if err != nil {
			return out, err
		}
		if len(x) != ed25519.PublicKeySize {
			return out, errors.New("Ed25519 x is not 32 bytes")
		}
		out.pub = ed25519.PublicKey(x)
	case "EC":
		var curve elliptic.Curve
		switch out.crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return out, fmt.Errorf("EC curve %q is not supported", out.crv)
		}
		x, err := b64field(k, "x")
		if err != nil {
			return out, err
		}
		y, err := b64field(k, "y")
		if err != nil {
			return out, err
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) != size || len(y) != size {
			return out, fmt.Errorf("x and y must be %d bytes for %s", size, out.crv)
		}
		pk, err := ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
		if err != nil {
			return out, fmt.Errorf("EC key: %v", err)
		}
		out.pub = pk
	case "RSA":
		n, err := b64field(k, "n")
		if err != nil {
			return out, err
		}
		e, err := b64field(k, "e")
		if err != nil {
			return out, err
		}
		if len(e) > 4 {
			return out, errors.New("RSA exponent too large")
		}
		N := new(big.Int).SetBytes(n)
		if N.BitLen() < 2048 || N.BitLen() > 8192 {
			return out, fmt.Errorf("RSA modulus is %d bits; 2048 to 8192 accepted", N.BitLen())
		}
		E := int(new(big.Int).SetBytes(e).Int64())
		if E < 3 || E%2 == 0 {
			return out, errors.New("RSA exponent is not an odd number >= 3")
		}
		out.pub = &rsa.PublicKey{N: N, E: E}
	default:
		return out, fmt.Errorf("kty %q is not supported", out.kty)
	}
	return out, nil
}

type jwsHeader struct {
	Alg, Kid, Jku, Typ string
	Crit               bool
}

func parseJWSHeader(protected string) (jwsHeader, error) {
	var h jwsHeader
	raw, err := base64.RawURLEncoding.DecodeString(protected)
	if err != nil {
		return h, errors.New("protected is not base64url (unpadded)")
	}
	v, _, serr := parseStrictJSON(string(raw))
	if serr != nil {
		return h, fmt.Errorf("protected header is not JSON: %s", serr.msg)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return h, errors.New("protected header is not a JSON object")
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"alg", &h.Alg}, {"kid", &h.Kid}, {"jku", &h.Jku}, {"typ", &h.Typ}} {
		if val, ok := m[f.name]; ok {
			s, ok := val.(string)
			if !ok {
				return h, fmt.Errorf("%s is not a string", f.name)
			}
			*f.dst = s
		}
	}
	_, h.Crit = m["crit"]
	return h, nil
}

func checkSignature(i int, s any, asGiven, stripped []byte, keys []jwk, kel []identity.SignedEvent, is *issues) sigReport {
	rep := sigReport{Index: i, Result: "malformed"}
	p := ptr("/signatures", i)
	m, ok := s.(map[string]any)
	if !ok {
		rep.Detail = "not an object"
		return rep
	}
	protected, _ := m["protected"].(string)
	sigB64, _ := m["signature"].(string)
	h, err := parseJWSHeader(protected)
	if err != nil {
		rep.Detail = err.Error()
		is.add(sevError, ptr(p, "protected"), "jws_header", "%v", err)
		return rep
	}
	rep.Alg, rep.Kid, rep.Jku, rep.Typ = h.Alg, h.Kid, h.Jku, h.Typ
	if h.Alg == "" || strings.EqualFold(h.Alg, "none") {
		rep.Detail = "alg is missing or none"
		is.add(sevError, ptr(p, "protected"), "jws_alg", "the protected header must name a signing algorithm (alg)")
		return rep
	}
	if h.Kid == "" {
		is.add(sevError, ptr(p, "protected"), "jws_kid", "the protected header must include kid (A2A §8.4.2)")
	}
	if h.Typ != "" && h.Typ != "JOSE" {
		is.add(sevWarning, ptr(p, "protected"), "jws_typ", "typ is %q; A2A §8.4.2 says it SHOULD be JOSE", h.Typ)
	}
	if h.Jku != "" {
		if u, err := url.Parse(h.Jku); err != nil || u.Scheme != "https" {
			is.add(sevWarning, ptr(p, "protected"), "jws_jku", "jku %q is not an https URL; keys SHOULD be retrieved over secure channels", h.Jku)
		}
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) == 0 {
		rep.Detail = "signature is not base64url (unpadded)"
		is.add(sevError, ptr(p, "signature"), "jws_signature", "signature is not base64url (unpadded)")
		return rep
	}
	if h.Crit {
		rep.Result, rep.Detail = "unverifiable", "the header carries crit, whose extensions this checker does not understand (RFC 7515 §4.1.11)"
		return rep
	}

	var candidates []jwk
	for _, k := range keys {
		if h.Kid != "" && k.kid == h.Kid {
			candidates = append(candidates, k)
		}
	}
	rep.KeySource = "jwks"
	if len(candidates) == 0 && len(keys) == 1 && keys[0].kid == "" {
		candidates = keys
	}
	if len(candidates) == 0 && len(kel) > 0 && strings.HasPrefix(h.Kid, a2acard.KIDPrefix) {
		pub, err := a2acard.CurrentKey(h.Kid, kel)
		if err != nil {
			rep.Result, rep.KeySource, rep.Detail = "invalid", "kel", "kid does not name a current key of the KEL: "+err.Error()
			is.add(sevError, p, "signature_key", "%s", rep.Detail)
			return rep
		}
		candidates, rep.KeySource = []jwk{{kid: h.Kid, kty: "OKP", crv: "Ed25519", pub: pub}}, "kel"
	}
	if len(candidates) == 0 {
		rep.Result, rep.KeySource = "unverifiable", ""
		rep.Detail = "no key supplied for this kid: pass \"jwks\" (or \"kel\" for a did:anet kid); this checker never fetches jku"
		return rep
	}
	lastErr := ""
	for _, k := range candidates {
		for _, pl := range []struct {
			name    string
			payload []byte
		}{{"as_given", asGiven}, {"defaults_removed", stripped}} {
			if pl.payload == nil {
				continue
			}
			input := make([]byte, 0, len(protected)+1+base64.RawURLEncoding.EncodedLen(len(pl.payload)))
			input = append(input, protected...)
			input = append(input, '.')
			input = base64.RawURLEncoding.AppendEncode(input, pl.payload)
			err := verifyJWS(h.Alg, k, input, sig)
			if err == nil {
				rep.Result, rep.Payload = "verified", pl.name
				if pl.name == "defaults_removed" {
					is.add(sevInfo, p, "signed_without_defaults",
						"the signature covers the card with default values removed (A2A §8.4.1); a2a-go verifies the card as given and will reject it")
				}
				return rep
			}
			lastErr = err.Error()
		}
	}
	rep.Result, rep.Detail = "invalid", lastErr
	is.add(sevError, p, "signature_invalid", "signature %d does not verify with the supplied key over either payload: %s", i, lastErr)
	return rep
}

var errUnsupportedAlg = errors.New("unsupported alg")

func verifyJWS(alg string, k jwk, input, sig []byte) error {
	hashFor := func(bits int) (crypto.Hash, []byte) {
		var h crypto.Hash
		switch bits {
		case 256:
			h = crypto.SHA256
		case 384:
			h = crypto.SHA384
		default:
			h = crypto.SHA512
		}
		d := h.New()
		d.Write(input)
		return h, d.Sum(nil)
	}
	switch alg {
	case "EdDSA", "Ed25519":
		pub, ok := k.pub.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("alg %s needs an Ed25519 key, the key is %s", alg, k.kty)
		}
		if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, input, sig) {
			return errors.New("Ed25519 verification failed")
		}
		return nil
	case "ES256", "ES384", "ES512":
		pub, ok := k.pub.(*ecdsa.PublicKey)
		want := map[string]string{"ES256": "P-256", "ES384": "P-384", "ES512": "P-521"}[alg]
		if !ok || k.crv != want {
			return fmt.Errorf("alg %s needs a %s key", alg, want)
		}
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return fmt.Errorf("%s signature must be %d bytes (r||s)", alg, 2*size)
		}
		bits := map[string]int{"ES256": 256, "ES384": 384, "ES512": 512}[alg]
		_, digest := hashFor(bits)
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(pub, digest, r, s) {
			return errors.New("ECDSA verification failed")
		}
		return nil
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		pub, ok := k.pub.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("alg %s needs an RSA key", alg)
		}
		bits := map[byte]int{'2': 256, '3': 384, '5': 512}[alg[2]]
		h, digest := hashFor(bits)
		if alg[0] == 'R' {
			if err := rsa.VerifyPKCS1v15(pub, h, digest, sig); err != nil {
				return errors.New("RSASSA-PKCS1-v1_5 verification failed")
			}
			return nil
		}
		if err := rsa.VerifyPSS(pub, h, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}); err != nil {
			return errors.New("RSASSA-PSS verification failed")
		}
		return nil
	}
	return fmt.Errorf("%w %q", errUnsupportedAlg, alg)
}

// checkAnetRules applies the anet admission rules (ANetCore a2acard.Verify,
// A2A-DESIGN §10.3) to a card that declares the anet-card extension.
func checkAnetRules(raw []byte, kel []identity.SignedEvent, nowMS int64, is *issues) *anetReport {
	resolve := func(aid string) ([]identity.SignedEvent, error) {
		if len(kel) == 0 {
			return nil, errors.New("no KEL supplied")
		}
		return kel, nil
	}
	if nowMS < 0 {
		nowMS = 0
	}
	ver, err := a2acard.Verify(raw, resolve, uint64(nowMS))
	if err == nil {
		return &anetReport{Result: "accepted", AID: ver.AID, Seq: fmt.Sprint(ver.Seq)}
	}
	var ae *a2acard.Error
	code, detail := "", err.Error()
	if errors.As(err, &ae) {
		code, detail = string(ae.Code), ae.Detail
	}
	if code == string(a2acard.CodeKELUnavailable) {
		return &anetReport{Result: "unverifiable", Code: code,
			Detail: "the card passes every anet rule that needs no key; supply \"kel\" to check its signature"}
	}
	is.add(sevError, "", "anet_rejected", "an anet hub would not admit this card: %s", detail)
	return &anetReport{Result: "rejected", Code: code, Detail: detail}
}

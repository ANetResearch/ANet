package provider

import (
	"strings"
	"unicode/utf8"
)

// SkillInfo is what a caller reads before deciding to call a capability:
// the fields of an A2A AgentSkill (a2a.proto AgentSkill) other than its id,
// which is the capability id itself.
//
// It exists because a network card that lists capability ids and nothing
// else tells an A2A client what to call and not what it does. A2A requires
// name, description and tags on every skill; a card built from ids alone
// either invents them or fails the card checks.
type SkillInfo struct {
	// Name is a short human-readable name ("Text digest").
	Name string
	// Description says what the capability does, what it takes and what it
	// returns. It is what an agent reads when choosing a skill.
	Description string
	// Tags are search keywords; a hub indexes skills by them.
	Tags []string
	// Examples are sample requests (a sentence, or the JSON arguments).
	Examples []string
	// InputModes and OutputModes are media types that override the card's
	// defaults for this skill. Empty means the defaults apply.
	InputModes  []string
	OutputModes []string
}

// Described is implemented by a provider that can describe its
// capabilities as A2A skills (A2A-DESIGN §10.2).
//
// Optional. A provider that does not implement it, or that answers false
// for a capability, gets a description derived from the capability id
// (see SkillInfoOf), so every capability can still appear on a card. The
// derived text says only what the id says; a provider that wants to be
// chosen by an agent that has never heard of it should implement this.
//
// The kernel publishes skills only for capabilities in
// inbound.public_capabilities, so describing a capability here does not
// make it callable by anyone.
type Described interface {
	// SkillInfo describes one capability, and reports whether this
	// provider has anything to say about it. Fields left empty are
	// filled by SkillInfoOf.
	SkillInfo(capability string) (SkillInfo, bool)
}

// Limits a derived or declared SkillInfo is held to. They are the card
// limits of ANetCore a2acard (name 128 bytes, description 4096 bytes, at
// most 16 tags per skill), repeated here so that a provider's description
// is checked where it is written rather than where the card fails.
const (
	MaxSkillNameBytes        = 128
	MaxSkillDescriptionBytes = 4096
	MaxSkillTags             = 16
)

// SkillInfoOf returns the skill description of capability as provider p
// states it, with every field A2A requires filled in.
//
// A field p leaves empty is derived from the capability id: the name is
// the id, the tags are the id's dot-separated words (the part before any
// "@" target), and the description says it is an anet capability with that
// id. Values that exceed the card limits are cut at a character boundary,
// so the result always fits a card.
func SkillInfoOf(p CapabilityProvider, capability string) SkillInfo {
	var si SkillInfo
	if d, ok := p.(Described); ok {
		if got, ok := d.SkillInfo(capability); ok {
			si = got
		}
	}
	derived := DeriveSkillInfo(capability)
	if strings.TrimSpace(si.Name) == "" {
		si.Name = derived.Name
	}
	if strings.TrimSpace(si.Description) == "" {
		si.Description = derived.Description
	}
	si.Tags = cleanList(si.Tags)
	if len(si.Tags) == 0 {
		si.Tags = derived.Tags
	}
	if len(si.Tags) > MaxSkillTags {
		si.Tags = si.Tags[:MaxSkillTags]
	}
	si.Name = truncateUTF8(si.Name, MaxSkillNameBytes)
	si.Description = truncateUTF8(si.Description, MaxSkillDescriptionBytes)
	si.Examples = cleanList(si.Examples)
	si.InputModes = cleanList(si.InputModes)
	si.OutputModes = cleanList(si.OutputModes)
	return si
}

// DeriveSkillInfo describes a capability from its id alone: what a card
// says about a capability whose provider says nothing.
//
// "text.digest" gets name "text.digest", tags ["text", "digest"];
// "system.reboot@dahua/camera-1" gets tags ["system", "reboot"]. The name is
// the id rather than a prettified form of it, because the id is what a
// caller must send and a prettified name would be a second spelling of it.
func DeriveSkillInfo(capability string) SkillInfo {
	base := capability
	if i := strings.IndexByte(base, '@'); i >= 0 {
		base = base[:i]
	}
	var tags []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(base, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '/' }) {
		w = strings.ToLower(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		tags = append(tags, w)
	}
	if len(tags) == 0 && capability != "" {
		tags = []string{capability}
	}
	if len(tags) > MaxSkillTags {
		tags = tags[:MaxSkillTags]
	}
	return SkillInfo{
		Name:        truncateUTF8(capability, MaxSkillNameBytes),
		Description: truncateUTF8("anet capability "+capability+" (no description published by its provider)", MaxSkillDescriptionBytes),
		Tags:        tags,
	}
}

// cleanList drops empty and duplicate entries, keeping order.
func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

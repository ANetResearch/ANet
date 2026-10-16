package main

// aidcheck.go is aid_invalid (ux UX-G8a, §1.4 G): every command that takes
// an <aid> takes only a whole AID, never a short form ("…7hq2abcd"), a
// prefix or a name. A name is what the other side says of itself, and a
// prefix can be made to collide on purpose; resolving either would let
// `payees add` or `peers trust` pick the wrong party. So an argument that is
// not an AID is refused before anything is read or asked of the daemon,
// exit status 2, with where to copy a whole one from (anet find, anet peers
// list).

import (
	"strings"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// aidArgs are the arguments of an invocation that name an agent: the <aid>
// of card, delegate, peers allow|trust|deny|remove and payees add|remove,
// and the AID of task list --peer and audit --peer. They are read the way
// each command reads them.
func aidArgs(cmd string, rest []string) []string {
	var out []string
	positional := func(i int, rest []string, subs ...string) {
		pos, _ := splitFlags(rest)
		if len(subs) > 0 {
			if len(pos) == 0 || !contains(subs, pos[0]) {
				return
			}
		}
		if i < len(pos) {
			out = append(out, strings.TrimSpace(pos[i]))
		}
	}
	flag := func(name string, rest []string, subs ...string) {
		pos, flags := splitFlags(rest)
		if len(subs) > 0 && (len(pos) == 0 || !contains(subs, pos[0])) {
			return
		}
		if v, ok := flags[name]; ok && v != "true" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	switch cmd {
	case "card":
		positional(0, rest)
	case "delegate":
		_, r := extractAttach(rest)
		positional(0, r)
	case "peers":
		positional(1, rest, "allow", "trust", "deny", "remove", "rm")
	case "payees":
		positional(1, rest, "add", "allow", "remove", "rm")
	case "task":
		flag("peer", rest, "list", "ls")
	case "audit":
		flag("peer", rest)
	}
	return out
}

// refuseBadAID is aid_invalid and its command for the first <aid> argument
// of the invocation that is not a whole AID, and true; false when there is
// none. The value shown is the user's own input, made safe for a terminal
// and cut to 64 characters (ux §1.4 G).
func refuseBadAID(cmd string, rest []string, lang a2ashape.Language) (string, string, bool) {
	for _, v := range aidArgs(cmd, rest) {
		if identity.ValidAID(v) {
			continue
		}
		args := a2ashape.Args{"value": printable(v, 64)}
		if args["value"] == "" {
			args["value"] = `""`
		}
		return a2ashape.Sentence("aid_invalid", lang, args), a2ashape.Next("aid_invalid", lang, args), true
	}
	return "", "", false
}

package main

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// FuzzAnswerLines feeds the human-line reader (UX-C2) whatever a control plane — or anything holding its
// port — could answer: it never panics, writes at most five lines and the next step, each one line
// with its label, and passes no control or bidirectional-override character to the terminal.
func FuzzAnswerLines(f *testing.F) {
	for _, s := range []string{
		`{"id":"ix_1","status":{"state":"TASK_STATE_FAILED","message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"x"}]}},"metadata":{"anet.reason":"no_local_task"}}`,
		`{"id":"ix_1","status":{"state":"TASK_STATE_INPUT_REQUIRED"},"metadata":{"anet.payment_note":"p","anet.reason":"needs_operator_approval"},"warning":"w","next":"anet pay ix_1"}`,
		`{"warning":"a\nb\u001b[2J","message":"m"}`,
		`[]`, `null`, `{"status":{"state":7}}`, `{"metadata":{"anet.payment_note":{}}}`,
	} {
		f.Add([]byte(s), true)
		f.Add([]byte(s), false)
	}
	f.Fuzz(func(t *testing.T, b []byte, task bool) {
		for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
			lines := answerLines(b, task, lang, time.Unix(1700000000, 0))
			if len(lines) > 6 {
				t.Fatalf("%d lines", len(lines))
			}
			for i, l := range lines {
				for _, r := range l {
					if r == '\n' || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
						t.Fatalf("line %d %q carries %U", i, l, r)
					}
				}
				if strings.HasPrefix(l, nextPrefix) {
					if i != len(lines)-1 || i == 0 {
						t.Fatalf("the next step is not last after a line: %q", lines)
					}
					continue
				}
				if !strings.Contains(l, ": ") {
					t.Fatalf("line %q has no label", l)
				}
			}
		}
	})
}

// FuzzRefuseBadAID feeds the aid_invalid check (UX-G8a) any command line: it never panics; what it lets
// through names only whole AIDs in its <aid> places; what it refuses it says in one line, the value
// made safe for a terminal.
func FuzzRefuseBadAID(f *testing.F) {
	const aid = "bafyreie6k7xot2qpitfcoseuevnuq7zcztdpdgx4coe3p2jlqjjl65hqu4"
	for _, c := range [][4]string{
		{"peers", "allow", aid, ""}, {"peers", "trust", "alice", ""}, {"delegate", "--attach", "/x", aid},
		{"task", "list", "--peer", "x"}, {"audit", "--peer=" + aid, "", ""}, {"payees", "rm", "\x1b[2J", ""},
		{"card", "…7hq2abcd", "", ""}, {"delegate", aid, "--capability", "x"},
	} {
		f.Add(c[0], c[1], c[2], c[3])
	}
	f.Fuzz(func(t *testing.T, cmd, a, b, c string) {
		rest := []string{a, b, c}
		s, next, refused := refuseBadAID(cmd, rest, a2ashape.English)
		if !refused {
			for _, v := range aidArgs(cmd, rest) {
				if !identity.ValidAID(v) {
					t.Fatalf("%s %q: %q passed as an AID", cmd, rest, v)
				}
			}
			return
		}
		if next != "anet find" || s == "" {
			t.Fatalf("refusal %q, next %q", s, next)
		}
		for _, r := range s {
			if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
				t.Fatalf("the refusal %q carries %U", s, r)
			}
		}
	})
}

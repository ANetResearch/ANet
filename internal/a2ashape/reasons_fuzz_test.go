package a2ashape

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzSentence: whatever a placeholder's value, and whatever key is asked
// for, a sentence is valid UTF-8 with no control character, and every
// placeholder of the row is filled; a command is too, or there is none.
func FuzzSentence(f *testing.F) {
	f.Add(uint16(0), "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd", "", false)
	f.Add(uint16(17), "a\nb‮c\x00", "busy.later", true)
	f.Add(uint16(200), "{next}", "not a key", true)
	f.Add(uint16(301), "--rm -rf /", "x402:NOT_THERE", false)
	f.Fuzz(func(t *testing.T, i uint16, val, key string, zh bool) {
		lang := English
		if zh {
			lang = Chinese
		}
		r := reasonTable[int(i)%len(reasonTable)]
		args := Args{}
		for _, tmpl := range []string{r.Zh, r.En, r.Next, r.NextZh} {
			for _, m := range placeholderRe.FindAllStringSubmatch(tmpl, -1) {
				args[m[1]] = val
			}
		}
		check := func(what, s string) {
			if !utf8.ValidString(s) {
				t.Fatalf("%s: not UTF-8: %q", what, s)
			}
			for _, c := range s {
				if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
					t.Fatalf("%s: control or format character %U: %q", what, c, s)
				}
			}
			if !strings.Contains(val, "{") {
				if m := placeholderRe.FindString(s); m != "" {
					t.Fatalf("%s: placeholder %s left: %q", what, m, s)
				}
			}
		}
		check(r.Key(), Sentence(r.Key(), lang, args))
		check(r.Key()+" next", Next(r.Key(), lang, args))
		check(key, Sentence(key, lang, Args{"code": val}))
		_ = ReasonCode(key)
		ref := Refuse(key, args)
		check(key+" body", ref.Body(lang).Error)
		if e := ref.A2A(lang); len(e.Details) != 2 {
			t.Fatalf("%s: %d ErrorInfo", key, len(e.Details))
		}
	})
}

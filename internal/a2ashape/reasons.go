package a2ashape

// reasons.go is the reason table (v0.3.0 ux §1): every sentence anet shows a
// person — a refusal, a failure, a wait, a status line, a terminal prompt, a
// doctor finding — in Chinese and English, with what to do next. The CLI, MCP,
// the local A2A interface, the control plane, the console and doctor take their
// sentences from here and nowhere else (UX-G1), and the same event gives the
// same code and the same words in all of them (UX-G3, SI-79).
//
// A row is a code (or a code and a variant, or a dotted sentence key), its
// classes (ux §1.1), how it reaches A2A, the two templates and the next step.
// Templates hold {placeholders}; their values come only from this node's own
// data, never from text a peer wrote (UX-G2, CUX-T6).
//
// Four functions render (UX-R8), all pure: Sentence, DeliveryText,
// PaymentNote and StatusSummary. The rest of the file is lookups the
// renderers and their callers share, and the table itself, at the end.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// OfficialHubURL is the official public hub (ux §2.6). It is not a setting:
// `anet up` and no_hub_yet name it so that the first command a new node is
// told to run joins a hub that needs nobody's approval.
const OfficialHubURL = "https://hub.agentnetwork.org.cn"

// Language is the language of a sentence.
type Language uint8

// The two languages. English is the zero value: an environment that names
// no language gets English (core G13).
const (
	English Language = iota
	Chinese
)

// Lang is this process's language (core G13): the first non-empty of LC_ALL,
// LC_MESSAGES and LANG; Chinese when it begins with "zh", English otherwise.
// There is no setting and no per-client choice: the daemon renders, and the
// CLI and MCP pass its sentences on unchanged (UX-C7).
func Lang() Language { return LangFrom(os.Getenv) }

// LangFrom is Lang over another environment.
func LangFrom(getenv func(string) string) Language {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(k); v != "" {
			if strings.HasPrefix(v, "zh") {
				return Chinese
			}
			return English
		}
	}
	return English
}

// Args are a template's placeholder values, by name. A value may be a
// string, an integer, a time.Duration (rendered "15 分钟" / "15 minutes"), a
// time.Time (local "MM-DD HH:MM") or, for a placeholder bound to a phrase
// set (an action, a reason short form, a tier), the phrase's key.
//
// Three placeholders fill themselves: {aid8}, {peer8} and {file_aid8} from
// "aid", "peer" and "file_aid"; {official_hub} is OfficialHubURL; {next} is
// the row's own next step. A time is shown with its year when it is more than
// a year from "now" (a time.Time; the clock when absent).
type Args map[string]any

// Class is a set of the classes of ux §1.1: where the sentence appears and
// how A2A carries it.
type Class uint16

// The classes.
const (
	// ClassR: the other side certainly did not run it (TASK_STATE_REJECTED).
	ClassR Class = 1 << iota
	// ClassF: a failed end (TASK_STATE_FAILED).
	ClassF
	// ClassFP: a payment did not settle (x402.payment.status payment-failed).
	ClassFP
	// ClassIP: waiting for someone to decide whether to pay
	// (TASK_STATE_INPUT_REQUIRED, payment-required).
	ClassIP
	// ClassW: waiting, recovers on its own (anet.delivery_wait).
	ClassW
	// ClassM: one message refused, the task unchanged.
	ClassM
	// ClassE: this node refused the request and sent nothing; an A2A error.
	ClassE
	// ClassC: only in the CLI or the control plane's error body.
	ClassC
	// ClassS: a status sentence (/status, doctor).
	ClassS
	// ClassI: information that changes no state.
	ClassI
	// ClassT: a terminal prompt or confirmation line.
	ClassT
	// ClassModel: the egress refusals of ux §1.4 I, read by the agent in the
	// sandbox and never by the user.
	ClassModel
)

// needsNext are the classes whose rows must say what to do next (UX-G2).
const needsNext = ClassR | ClassF | ClassFP | ClassIP | ClassM | ClassW | ClassE | ClassC

// NextKind is what a sentence asks of the user (UX-G2).
type NextKind uint8

// The kinds of next step. Rows of class R, F, FP, IP, M, W, E or C declare
// one other than NextUnset; status, information and prompt rows may.
const (
	NextUnset NextKind = iota
	// NextCommand: run the row's command (Next).
	NextCommand
	// NextNoneNeeded: nothing to do.
	NextNoneNeeded
	// NextAuto: anet is handling it; the sentence says how it ends.
	NextAuto
	// NextRetryLater: send the same again after a while.
	NextRetryLater
	// NextChangeInput: change the request, an argument, a file or this
	// node's condition, then do it again.
	NextChangeInput
	// NextContact: ask the other side or an operator.
	NextContact
	// NextStartOver: start a new task.
	NextStartOver
	// NextFutile: trying again will not help.
	NextFutile
)

// Level is how a status sentence rates what it says: the statuses of
// `anet doctor` (UX-K3) and the level of a /status summary line.
type Level string

// The levels.
const (
	LevelOK      Level = "ok"
	LevelInfo    Level = "info"
	LevelWarn    Level = "warn"
	LevelFail    Level = "fail"
	LevelUnknown Level = "unknown"
)

// A2AKind is the local A2A error a class-E row is reported as (UX-R10,
// core G1).
type A2AKind uint8

// The A2A errors of reason codes.
const (
	A2ANone A2AKind = iota
	// A2AInvalidParams: the request itself is not acceptable (−32602, REST
	// 400 INVALID_ARGUMENT).
	A2AInvalidParams
	// A2AFailedPrecondition: a lasting state of this node, which does not
	// change until the user changes it (−32602, REST 400
	// FAILED_PRECONDITION).
	A2AFailedPrecondition
	// A2ATransient: a passing state; the same call may succeed later
	// (−32603, REST 503 with Retry-After).
	A2ATransient
	// A2ATaskNotFound: TaskNotFoundError (−32001, REST 404).
	A2ATaskNotFound
	// A2AUnsupportedOperation: UnsupportedOperationError (−32004, REST 400).
	A2AUnsupportedOperation
	// A2AExtendedCardNotConfigured: ExtendedAgentCardNotConfiguredError
	// (−32007, REST 400).
	A2AExtendedCardNotConfigured
	// A2AInvalidAgentResponse: InvalidAgentResponseError (−32006, REST 500),
	// the other side answered against the specification.
	A2AInvalidAgentResponse
)

// A2A is how a row reaches A2A beyond what its classes say (ux §1.1).
type A2A struct {
	// Effect is anet.effect_status on a capability task that ends with
	// this row (R and F): UNAVAILABLE, UNVERIFIED or FAILED.
	Effect string
	// X402 is x402.payment.error of an FP row (an a2a-x402 standard code).
	X402 string
	// Err is the local A2A error of an E row.
	Err A2AKind
}

// The anet.effect_status values a row can end a capability task with
// (ANetCore effect.Status).
const (
	effectUnavailable = "UNAVAILABLE"
	effectUnverified  = "UNVERIFIED"
	effectFailed      = "FAILED"
)

// Row is one row of the reason table.
type Row struct {
	// Code is the reason code (the value of anet.reason, of a control-plane
	// reason, of an ErrorInfo), an a2a-x402 code written "x402:<CODE>", or a
	// dotted sentence key that is not a code (a status line, a prompt).
	Code string
	// Variant chooses among a code's sentences without changing the code
	// (UX-G1): the row's key is Code + "." + Variant.
	Variant string
	Class   Class
	// Level rates a status row; empty for the rest.
	Level Level
	A2A   A2A
	// Zh and En are the templates.
	Zh, En string
	// NextKind and Next are the next step. Next is a command template, the
	// same in both languages unless NextZh gives the Chinese form (a <…>
	// hint follows the language, UX-G8a).
	NextKind NextKind
	Next     string
	NextZh   string
	// NoSentence marks a code that has no sentence: the connection is
	// dropped (ux §1.4 I, egress_sni_mismatch and its two siblings).
	NoSentence bool
}

// Key is the row's key: its code, and its variant after a dot.
func (r Row) Key() string {
	if r.Variant == "" {
		return r.Code
	}
	return r.Code + "." + r.Variant
}

// template is the row's template in a language.
func (r Row) template(lang Language) string {
	if lang == Chinese {
		return r.Zh
	}
	return r.En
}

// nextTemplate is the row's command in a language.
func (r Row) nextTemplate(lang Language) string {
	if lang == Chinese && r.NextZh != "" {
		return r.NextZh
	}
	return r.Next
}

// codeRe is the literal form of a code (core CORE-K19).
var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

// rowIndex finds a row by its key; built once from reasonTable.
var rowIndex = func() map[string]int {
	m := make(map[string]int, len(reasonTable))
	for i, r := range reasonTable {
		if _, dup := m[r.Key()]; !dup {
			m[r.Key()] = i
		}
	}
	return m
}()

// Lookup is the row of a key. A variant the table does not have is its
// code's row: a variant only changes words.
func Lookup(key string) (Row, bool) {
	if i, ok := rowIndex[key]; ok {
		return reasonTable[i], true
	}
	if base, _, ok := strings.Cut(key, "."); ok {
		if i, ok := rowIndex[base]; ok {
			return reasonTable[i], true
		}
	}
	return Row{}, false
}

// Rows is the table, in its order.
func Rows() []Row { return append([]Row(nil), reasonTable...) }

// ReasonCode is the code a key stands for in anet.reason, a control-plane
// reason and an ErrorInfo (CORE-K25): the key without its variant. A key the
// table has that is not a code (a status line, a prompt, an "x402:" row) has
// none: "". A code the table does not have is itself, when of legal form.
func ReasonCode(key string) string {
	if r, ok := Lookup(key); ok {
		if codeRe.MatchString(r.Code) {
			return r.Code
		}
		return ""
	}
	base, _, _ := strings.Cut(key, ".")
	if codeRe.MatchString(base) {
		return base
	}
	return ""
}

// Sentence renders a key in a language. A code the table does not have is
// shown with the fallback unknown_code and the code itself (UX-G9); a key
// that is not even of a code's form is not shown at all, only that it was
// refused (refused). A placeholder with no value never shows as a
// placeholder or as empty brackets (UX-G8, UX-T4).
func Sentence(key string, lang Language, args Args) string {
	shown, args := shownKey(key, args)
	r, _ := Lookup(shown)
	return render(r, shown, lang, args)
}

// shownKey is the key whose sentence Sentence shows for key, with the
// arguments it shows it with: key itself when the table has it; for a code
// the table lacks, unknown_code with that code (UX-G9); for anything else,
// refused, with nothing of key in it.
func shownKey(key string, args Args) (string, Args) {
	if _, ok := Lookup(key); ok {
		return key, args
	}
	code, _, _ := strings.Cut(key, ".")
	if !codeRe.MatchString(code) {
		return "refused", nil
	}
	return "unknown_code", Args{"code": code}
}

// Next is a key's next step in a language with its placeholders filled: the
// command to print after "→" (UX-R2) and doctor's hint (UX-K4); empty when
// the row has none, or when a value the command needs is missing (a command
// with a hole in it does not run).
func Next(key string, lang Language, args Args) string {
	r, ok := Lookup(key)
	if !ok {
		return ""
	}
	return renderNext(r, lang, args)
}

// --- rendering ---

// placeholderRe finds a placeholder.
var placeholderRe = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

// missing is what a placeholder with no value becomes: never a placeholder,
// never an empty pair of brackets (UX-T4).
const missing = "—"

// render fills a row's template. key is the key asked for: a variant key
// that fell back to its code's row still chooses its phrase sets.
func render(r Row, key string, lang Language, args Args) string {
	if r.NoSentence {
		return ""
	}
	t := r.template(lang)
	if !present(args, "retry") {
		// UX-G8: no wait time — "在 {retry} 后" is "稍后", "in {retry}" "later".
		if lang == Chinese {
			t = strings.ReplaceAll(t, "在 {retry} 后", "稍后")
		} else {
			t = strings.ReplaceAll(t, "in {retry}", "later")
		}
	}
	if r.Code == "sandbox_start_failed" && !present(args, "error") {
		// bubblewrap said nothing: no brackets, no placeholder (ux §1.4 H).
		t = strings.ReplaceAll(strings.ReplaceAll(t, "（{error}）", ""), " ({error})", "")
	}
	_, variant, _ := strings.Cut(strings.TrimPrefix(key, r.Code), ".")
	if variant == "" {
		variant = r.Variant
	}
	now := time.Now()
	if v, ok := args["now"].(time.Time); ok && !v.IsZero() {
		now = v
	}
	var b strings.Builder
	last := 0
	for _, loc := range placeholderRe.FindAllStringSubmatchIndex(t, -1) {
		name := t[loc[2]:loc[3]]
		var v string
		switch {
		case name == "next":
			if v = renderNext(r, lang, args); v == "" {
				v = missing
			}
		default:
			var ok bool
			if v, ok = value(r.Code, variant, name, lang, args, now); !ok {
				v = fallback(name, lang)
			} else if lang == English && loc[0] == 0 && phraseSetOf(r.Code, variant, name) != "" {
				v = capitalize(v)
			}
		}
		head := t[last:loc[0]]
		if lang == Chinese && startsHan(v) && endsHanSpace(head) {
			head = head[:len(head)-1] // "把 名字未知" is "把名字未知"
		}
		b.WriteString(head)
		b.WriteString(v)
		last = loc[1]
		if lang == Chinese && name == "name" && strings.HasPrefix(t[last:], "模块") && endsASCIIAlnum(v) {
			b.WriteByte(' ') // "MCP 模块", not "MCP模块"
		}
		if lang == Chinese && endsHan(v) && startsSpaceHan(t[last:]) {
			last++ // "{limit} 内" with "15 分钟" is "15 分钟内"
		}
	}
	b.WriteString(t[last:])
	return b.String()
}

// The spaces a Chinese template keeps around a placeholder stand between
// Chinese and a number or Latin text; between two Chinese characters there
// is none (ux §3: "15 分钟内", "30 天后删除").
func startsHan(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.Is(unicode.Han, r)
}

func endsASCIIAlnum(s string) bool {
	if s == "" {
		return false
	}
	c := s[len(s)-1]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func endsHan(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.Is(unicode.Han, r)
}

func endsHanSpace(s string) bool {
	return strings.HasSuffix(s, " ") && endsHan(s[:len(s)-1])
}

func startsSpaceHan(s string) bool {
	return strings.HasPrefix(s, " ") && startsHan(s[1:])
}

// renderNext fills a row's command; "" when it has none or a value is
// missing.
func renderNext(r Row, lang Language, args Args) string {
	t := r.nextTemplate(lang)
	if t == "" {
		return ""
	}
	hole := false
	now := time.Now()
	out := placeholderRe.ReplaceAllStringFunc(t, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := value(r.Code, r.Variant, name, lang, args, now)
		if !ok {
			if name == "official_hub" {
				return OfficialHubURL
			}
			hole = true
			return m
		}
		return v
	})
	if hole {
		return ""
	}
	return out
}

// present reports a non-empty value for name.
func present(args Args, name string) bool {
	v, ok := args[name]
	if !ok || v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case time.Time:
		return !x.IsZero()
	case *uint64:
		return x != nil
	}
	return true
}

// value is the text of placeholder name, and whether it has one.
func value(code, variant, name string, lang Language, args Args, now time.Time) (string, bool) {
	if v, ok := moduleValue(code, variant, name, lang, args); ok {
		return v, true
	}
	switch name {
	case "aid8", "peer8", "file_aid8":
		if present(args, name) {
			return clean(fmt.Sprint(args[name]), 16), true
		}
		src := strings.TrimSuffix(name, "8")
		if !present(args, src) {
			return "", false
		}
		return Short8(fmt.Sprint(args[src])), true
	case "official_hub":
		if !present(args, name) {
			return OfficialHubURL, true
		}
	}
	if !present(args, name) {
		return "", false
	}
	v := args[name]
	if set := phraseSetOf(code, variant, name); set != "" {
		key := fmt.Sprint(v)
		if p, ok := phrase(set, key); ok {
			return p.in(lang), true
		}
		return "", false
	}
	switch x := v.(type) {
	case time.Duration:
		return humanDuration(x, lang), true
	case time.Time:
		return humanTime(x, now), true
	case *uint64:
		return strconv.FormatUint(*x, 10), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	}
	s := fmt.Sprint(v)
	switch name {
	case "retention":
		return humanRetention(s, lang), true
	case "task":
		if !plainID(s) {
			return "<task id>", true
		}
		return s, true
	case "name":
		s = clean(s, maxNameRunes)
		return s, s != ""
	case "value":
		s = clean(s, maxShortRunes)
		return s, s != ""
	case "error":
		// bwrap's first line (sandbox_start_failed) is cut at 120 characters (ux §1.4 H).
		s = clean(s, maxErrorRunes)
		return s, s != ""
	case "code":
		// A code is shown only in a code's form (CORE-K19): a hub's or a
		// peer's "code" that is really a phrase never becomes part of a
		// sentence (UX-G2: no text the other side wrote).
		if !codeRe.MatchString(s) {
			return "", false
		}
		return s, true
	}
	s = clean(s, maxValueRunes)
	return s, s != ""
}

// Bounds on a placeholder value (ux UX-G8 and the notes under §1.4 C4, G).
const (
	maxNameRunes  = 64
	maxErrorRunes = 120
	maxValueRunes = 512
)

// fallback is a placeholder with no value: "名字未知 / unnamed" for a name
// (ux §1.4 M), "<task id>" for a task (UX-G8), and a dash for the rest.
func fallback(name string, lang Language) string {
	switch name {
	case "name":
		if lang == Chinese {
			return "名字未知"
		}
		return "unnamed"
	case "task":
		return "<task id>"
	}
	return missing
}

// Short8 is an AID as it is shown to be recognized, not typed: "…" and its
// last 8 characters (UX-G8). Every AID begins "bafyrei", so its head tells
// none apart.
func Short8(aid string) string {
	r := []rune(aid)
	if len(r) > 8 {
		r = r[len(r)-8:]
	}
	return "…" + string(r)
}

// capitalize upper-cases the first letter of an English phrase that begins
// a sentence ("entering the recovery passphrase has to be confirmed…").
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || !unicode.IsLower(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// humanDuration is a duration for a person (UX-G8): "30 秒", "15 分钟",
// "2 小时 13 分", "3 天 2 小时" / "30 seconds", "15 minutes", "2 h 13 min",
// "3 d 2 h". It rounds up: a wait is never said to be shorter than it is.
func humanDuration(d time.Duration, lang Language) string {
	if d < 0 {
		d = 0
	}
	// Rounded up without adding to d: a wait near the largest Duration (a
	// peer's retry_after_ms, say) must not wrap around to a negative one.
	s := int64(d / time.Second)
	if d%time.Second != 0 {
		s++
	}
	zh := lang == Chinese
	unit := func(n int64, zhw, one, many string) string {
		if zh {
			return strconv.FormatInt(n, 10) + " " + zhw
		}
		if n == 1 {
			return "1 " + one
		}
		return strconv.FormatInt(n, 10) + " " + many
	}
	switch {
	case s < 60:
		return unit(s, "秒", "second", "seconds")
	case s < 3600:
		return unit((s+59)/60, "分钟", "minute", "minutes")
	case s < 86400:
		m := (s + 59) / 60
		h, m := m/60, m%60
		if h == 24 {
			return pair(1, 0, zh, "天", "d", "小时", "h")
		}
		return pair(h, m, zh, "小时", "h", "分", "min")
	}
	h := (s + 3599) / 3600
	return pair(h/24, h%24, zh, "天", "d", "小时", "h")
}

// pair is "a big, b small" with the small part left out when it is 0.
func pair(a, b int64, zh bool, zhBig, enBig, zhSmall, enSmall string) string {
	big, small := enBig, enSmall
	if zh {
		big, small = zhBig, zhSmall
	}
	out := strconv.FormatInt(a, 10) + " " + big
	if b > 0 {
		out += " " + strconv.FormatInt(b, 10) + " " + small
	}
	return out
}

// humanTime is a moment in this node's time zone, "MM-DD HH:MM", with the
// year when it is more than a year from now (UX-G8).
func humanTime(t, now time.Time) string {
	t = t.Local()
	d := t.Sub(now)
	if d < 0 {
		d = -d
	}
	if d > 365*24*time.Hour {
		return t.Format("2006-01-02 15:04")
	}
	return t.Format("01-02 15:04")
}

// humanRetention is a retention setting for a person: "30d" is "30 天" /
// "30 days", "forever" is "永久" / "forever".
func humanRetention(s string, lang Language) string {
	if s == "forever" {
		if lang == Chinese {
			return "永久"
		}
		return "forever"
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if v, err := strconv.ParseUint(n, 10, 32); err == nil {
			if lang == Chinese {
				return n + " 天"
			}
			if v == 1 {
				return "1 day"
			}
			return n + " days"
		}
	}
	return clean(s, maxShortRunes)
}

// --- phrases ---

// phrasePair is a phrase in both languages: the value of a placeholder bound
// to a phrase set.
type phrasePair struct{ zh, en string }

func (p phrasePair) in(lang Language) string {
	if lang == Chinese {
		return p.zh
	}
	return p.en
}

// phraseSetOf is the phrase set a placeholder of a code draws on, "" for a
// plain value.
func phraseSetOf(code, variant, name string) string {
	switch name {
	case "wait":
		return "wait"
	case "action":
		return "tty_action"
	case "how":
		return "rotation_how"
	case "state":
		if code == "card.header" {
			return "card_state"
		}
	case "charge":
		return "charge"
	case "auto_reply":
		return "on_off"
	case "tier":
		if variant == "off" {
			return "tier_off"
		}
		return "tier"
	case "why":
		switch code {
		case "card_price_unavailable":
			return "price_why"
		case "hub_retrying":
			return "hub_why"
		case "egress_allow_invalid":
			return "egress_why"
		case "agent_unsupported":
			return "combo_why"
		case "module_absent":
			return "module_why" // value: moduleValue; here only so that a sentence begins with a capital
		}
	case "fix":
		switch code {
		case "module_absent", "payments.unchargeable", "capabilities.files_not_service.absent":
			return "module_fix"
		}
	case "instead":
		switch code {
		case "cli_removed":
			return "cli_instead"
		case "config_key_removed":
			return "config_instead"
		}
	}
	return ""
}

// phrase is the phrase key in set. price_why also takes the wait codes: a
// price request held back by a wait says why in the wait's words (ux §1.4 D1).
func phrase(set, key string) (phrasePair, bool) {
	if p, ok := phraseSets[set][key]; ok {
		return p, true
	}
	if set == "price_why" {
		p, ok := phraseSets["wait"][key]
		return p, ok
	}
	return phrasePair{}, false
}

// phraseSets are the values of the placeholders bound to a set, by key.
var phraseSets = map[string]map[string]phrasePair{
	// ux §1.4 C2: the short form of each wait, for delivery_unconfirmed.waiting
	// and card_price_unavailable.
	"wait": {
		"rate_limited":        {"你发得太快", "sending too fast"},
		"redelivery_limited":  {"你发得太快", "sending too fast"},
		"strangers_busy":      {"对方的非联系人通道已满", "their inbox for non-contacts is full"},
		"mailbox_full":        {"对方信箱已满", "their mailbox is full"},
		"home_unreachable":    {"连不上对方的 hub", "their hub cannot be reached"},
		"session_unavailable": {"你的 hub 暂不接受首次联系", "your hub is refusing first contacts"},
		"credential_refused":  {"hub 拒绝了投递凭据", "the hub refused the delivery credential"},
		"notice_stale":        {"你的密钥还在等 hub 确认", "your key is waiting for your hub's confirmation"},
		"rotation_pending":    {"密钥轮换正在发布", "a key rotation is being published"},
	},
	// ux §1.4 D1: why there is no signed price.
	"price_why": {
		"timeout":         {"对方暂时没有回应", "the other side did not answer"},
		"not_offered":     {"对方没有给出签名价格", "it gave no signed price"},
		"bad_signature":   {"对方的价格签名无效", "its price signature is invalid"},
		"text_task":       {"文本任务没有签名价", "text tasks have no signed price"},
		"hub_unreachable": {"hub 暂时连不上", "the hub cannot be reached"},
	},
	// ux §1.4 F: why joining a hub has to wait.
	"hub_why": {
		"unreachable": {"hub 暂时连不上", "the hub cannot be reached"},
		"internal":    {"hub 内部出错", "the hub had an internal error"},
		"pow":         {"注册需要的工作量证明没有通过", "the proof of work was not accepted"},
		// LEC EXE-13: 429 rate_limited and a signature taken for a replay
		// (the hub is busy), and a failure on this node before anything was
		// sent; ux §1.4 F names no phrase for them.
		"busy":  {"hub 暂时繁忙", "the hub is busy"},
		"local": {"本节点暂时出错", "this node had an error"},
	},
	// ux §1.4 G: what is wrong with an egress_allow entry.
	"egress_why": {
		"ip":        {"是 IP 地址", "is an IP address"},
		"wildcard":  {"含通配符", "has a wildcard"},
		"port":      {"带端口", "has a port"},
		"local":     {"是本地域名", "is a local name"},
		"non_ascii": {"含非 ASCII 字符，请写成 xn-- 形式", "has non-ASCII characters, write it in xn-- form"},
		// LEC EXE-3: any other shape (a URL, one label, a character a host
		// name cannot have); ux §1.4 G names no phrase for it.
		"not_host": {"不是完整的主机名", "is not a whole host name"},
	},
	// ux §1.4 H: why an agent cannot use the model service address (agent_unsupported.combo).
	"combo_why": {
		"openai_only":    {"codex 只能用 api.openai.com", "codex works only with api.openai.com"},
		"anthropic_only": {"claude 只能用 api.anthropic.com", "claude works only with api.anthropic.com"},
		"no_chat":        {"api.anthropic.com 不提供它用的 chat completions 接口", "api.anthropic.com does not offer the chat completions interface it uses"},
	},
	// ux §1.4 D1: the payment tier, and its form in .off.
	"tier": {
		"auto":  {"自动付款", "your automatic payment limit"},
		"agent": {"agent 付款", "the agent payment limit"},
	},
	"tier_off": {
		"auto":  {"自动付款", "automatic payment"},
		"agent": {"agent 付款", "agent payment"},
	},
	// ux §1.4 E2: what tty_required asks to confirm.
	"tty_action": {
		"recovery_passphrase":     {"输入恢复口令", "entering the recovery passphrase"},
		"set_recovery_passphrase": {"设置恢复口令", "setting the recovery passphrase"},
		"retention":               {"修改保存期", "changing the retention"},
		"pay":                     {"付款", "paying"},
		"inbound_policy":          {"放宽入站策略", "loosening the inbound policy"},
		"trust":                   {"信任对端", "trusting a peer"},
		"approve":                 {"批准待批任务", "approving a held task"},
		"payment_limits":          {"修改付款额度", "changing payment limits"},
		"payees":                  {"修改付款名单", "changing the payee list"},
		"redeem":                  {"兑付", "redeeming"},
		"egress":                  {"放行出网主机", "allowing network hosts"},
		"reasoning_preview":       {"开启推理预览", "turning on reasoning previews"},
	},
	// ux §1.4 M2: how the key was last rotated (last_rotation.mode).
	"rotation_how": {
		"witness":   {"指定见证", "witness"},
		"scheduled": {"计划内", "scheduled"},
		"emergency": {"紧急", "emergency"},
		"recover":   {"恢复", "recover"},
	},
	// ux §1.4 M4: how far a card was checked.
	"card_state": {
		"verified":         {"已验证，密钥已确认", "verified, key confirmed"},
		"verified_pending": {"已验证，密钥暂未确认", "verified, key not confirmed yet"},
		"unverified":       {"未验证", "not verified"},
	},
	// HPP PAY-C15: what a card charges.
	"charge": {
		"free":    {"免费", "free"},
		"partial": {"部分收费", "some skills paid"},
		"paid":    {"收费", "paid"},
	},
	// ux §4.7: auto-reply on or off.
	"on_off": {
		"on":  {"开", "on"},
		"off": {"关", "off"},
	},
	// ux §4.3: what to use instead of a removed command or flag.
	"cli_instead": {
		"x402_authorize": {"hub 网关已下线，付款在任务里进行；请改用 anet delegate 发任务，报价出来后用 anet pay 付款",
			"the hub gateway is gone and payments happen inside the task; use anet delegate, then pay the quote with anet pay"},
		"delegate_pay": {"报价在额度内会自动付，超出时请改用 anet pay <任务>",
			"quotes within your limit are paid automatically; otherwise use anet pay <task>"},
		"profile_pricing": {"价格由签名的扩展卡片自动给出，请删掉这个参数",
			"prices now come from the signed extended card; remove this flag"},
		"hub_register_token": {"hub 已不再使用邀请码；请删掉这个参数，直接运行 anet hub-register <网址>，再把它打印的 admit 命令发给运营者",
			"hubs no longer use invites; remove this flag, run anet hub-register <url> and send the admit command it prints to the operator"},
		"hub_register_accept_delegations": {"请改用 anet inbound policy 设置入站策略",
			"use anet inbound policy to set the inbound policy"},
		"p2p_advertise": {"直连地址只发给你的联系人；需要时请改用 config.json 的 modules.p2p.advertise",
			"direct addresses go only to your contacts; use modules.p2p.advertise in config.json if needed"},
		"accept":     {"请改用 anet inbound policy closed|approve|open", "use anet inbound policy closed|approve|open"},
		"accept_end": {"请改用 anet end <编号>", "use anet end <id>"},
		"install":    {"请改用 anet agents wire <工具>", "use anet agents wire <tool>"},
		"evidence":   {"请改用 anet audit（加 --type 按事件类型筛选）", "use anet audit (add --type to filter by event type)"},
		"require_image": {"视觉服务已改为带文件的公开能力；请删掉这个参数，改用 public_capabilities 的 files",
			"the vision service is now a public capability that takes files; remove this flag and use files in public_capabilities"},
		"payments_payees_file": {"付款名单固定在数据目录的 payees.allow，请改用 anet payees 管理",
			"the payee list lives in payees.allow in the data directory; use anet payees to manage it"},
		"fixed_auto_reply": {"自动回复的轮询间隔与回复次数上限已改为固定值，请删掉这个参数",
			"the auto-reply poll interval and reply cap are now fixed; remove this flag"},
	},
	// ux §2.3: what to do instead of a removed setting.
	"config_instead": {
		"pricing": {"价格由签名的扩展卡片自动给出，请删掉这一项",
			"prices now come from the signed extended card; remove it"},
		"publish_prices": {"价格只在签名的扩展卡片里给出，请删掉这一项",
			"prices appear only in the signed extended card; remove it"},
		"voucher": {"凭证兑付门已删除，请删掉 x402 模块里的这两项",
			"the voucher gate is gone; remove both from the x402 module"},
		"witness_hub": {"余额核对已自动进行，请删掉这两项", "balance checks now run on their own; remove both"},
		"providers":   {"改用 modules.anetlink", "use modules.anetlink instead"},
		"accept_delegations": {"请改用 anet inbound policy 设置入站策略",
			"use anet inbound policy to set the inbound policy"},
		"rotation_grace": {"轮换宽限固定为 1 小时，请删掉这一项", "the rotation grace is fixed at 1 hour; remove it"},
		"list_files": {"名单固定在数据目录的 peers.allow、peers.deny、peers.trust，请删掉这一项",
			"the lists live in peers.allow, peers.deny and peers.trust in the data directory; remove it"},
		"reject_notice": {"拒绝原因已随送达确认带回，请删掉这一项",
			"refusals now travel with the delivery confirmation; remove it"},
		"pending": {"待批队列上限固定（200 个，每个对端 3 个，72 小时），请删掉这一项",
			"the approval queue limits are fixed (200, 3 per peer, 72 hours); remove it"},
		"evidence": {"证据一律只记内容标识，请删掉这一项", "evidence now keeps content ids only; remove it"},
		"require_image": {"请删掉这一项，把视觉服务改成带文件的公开能力（public_capabilities 的 files）",
			"remove it, and turn the vision service into a public capability that takes files (files in public_capabilities)"},
		"error_reply": {"后端失败时对方会看到统一的 backend_failed 说明，请删掉这一项",
			"when the backend fails the requester sees the standard backend_failed text; remove it"},
		"fixed_auto_reply": {"已改为固定值，请删掉这一项", "now fixed; remove it"},
		"payees_file": {"付款名单固定在数据目录的 payees.allow，请改用 anet payees 管理",
			"the payee list lives in payees.allow in the data directory; use anet payees to manage it"},
		"anetlink_timeout": {"已改为固定值，请删掉这一项", "now fixed; remove it"},
		"expected_uid": {"请删掉这一项，改用 expected_user，它也接受数字 uid",
			"remove it and use expected_user, which also takes a numeric uid"},
		"p2p_timeouts": {"已改为固定值，请删掉这一项", "now fixed; remove it"},
		"taskboard": {"hub 共享看板已删除，请删掉 config.json 里的这一块；要把任务交给别人，请改用 anet delegate",
			"the hub task board was removed; delete this block from config.json, and use anet delegate to hand a task to someone"},
		"invite_env": {"hub 已不再使用邀请码，已忽略；请删掉这个环境变量，直接运行 anet hub-register",
			"hubs no longer use invites, so it is ignored; remove it and run anet hub-register"},
	},
}

// --- C1 ---

// c1Reasons maps ANetLink's C1 refusals to the user's codes (ux §1.3, core
// §2.15.3): C1 codes are ANetLink's namespace and never reach a person as
// they are.
var c1Reasons = map[string]string{
	"not_permitted":      "provider_policy",
	"unknown_capability": "capability_not_served",
	"not_found":          "capability_not_served",
	"too_large":          "args_too_large",
	"bad_request":        "backend_failed",
	"unsupported_media":  "backend_failed",
	"internal":           "backend_failed",
	"c1_version":         "backend_failed",
}

// C1Reason is the user code a C1 refusal becomes (ux §1.3). A code outside
// C1 v1 is the backend failing: backend_failed. (c1_version also makes doctor
// anetlink.socket fail; the provider reports that.)
func C1Reason(c1 string) string {
	if r, ok := c1Reasons[c1]; ok {
		return r
	}
	return "backend_failed"
}

// --- labels ---

// Label is a short label in a language: the labels of the status summary
// (ux §4.7), the human lines of a task command (UX-C2), `anet identity
// status` (ux §1.4 M2), the columns of `anet find` (ux §3.1 text B) and of
// `anet peers list` (sealed-sender §5.2). Keys: "status.<topic>",
// "task.<line>", "watch.<tag>", "identity_status.<line>", "find.<column>",
// "peers_list.<column>".
// "" for a key it does not know.
func Label(key string, lang Language) string {
	return labels[key].in(lang)
}

var labels = map[string]phrasePair{
	"status.identity": {"身份", "identity"},
	"status.recovery": {"恢复", "recovery"},
	"status.hub":      {"hub", "hub"},
	"status.delivery": {"送达", "delivery"},
	"status.inbound":  {"接单", "inbound"},
	"status.sandbox":  {"沙箱", "sandbox"},
	"status.payments": {"付款", "payments"},
	"status.privacy":  {"隐私", "privacy"},
	"status.preview":  {"预览", "preview"},

	// LEC §5.2: the rows of `anet autoreply show`, and the line of `anet autoreply set` that says
	// where the sandbox stands.
	"autoreply.backend":   {"自动回复", "auto-reply"},
	"autoreply.trusted":   {"信任对端", "trusted"},
	"autoreply.untrusted": {"未信任", "untrusted"},
	"autoreply.key":       {"API key", "API key"},
	"autoreply.budget":    {"预算", "budget"},

	"task.reason":   {"原因", "reason"},
	"task.delivery": {"送达", "delivery"},
	"task.payment":  {"付款", "payment"},
	"task.kept":     {"保存", "kept"},
	"task.note":     {"提示", "note"},

	// `anet agents wire|unwire`: the labels of the lines under a tool.
	"wire.backup": {"备份", "backup"},
	"wire.note":   {"注意", "note"},

	// `anet task watch` (CUX §4.5): the state tags of its status lines, and
	// the headings of a preview. {time} is HH:MM:SS, {n} a byte count.
	"watch.submitted":      {"已提交", "submitted"},
	"watch.working":        {"处理中", "working"},
	"watch.input_required": {"等待输入", "input required"},
	"watch.completed":      {"已完成", "completed"},
	"watch.failed":         {"已失败", "failed"},
	"watch.rejected":       {"已拒绝", "rejected"},
	"watch.canceled":       {"已取消", "canceled"},
	"watch.delivered":      {"已送达", "delivered"},
	"watch.preview":        {"预览（{time}，前略 {n} 字节）", "preview ({time}, first {n} bytes left out)"},
	"watch.reasoning":      {"推理（{time}，前略 {n} 字节）", "reasoning ({time}, first {n} bytes left out)"},

	"identity_status.identity": {"身份", "identity"},
	"identity_status.key":      {"密钥", "key"},
	"identity_status.witness":  {"见证 hub", "witness hub"},
	"identity_status.recovery": {"恢复文件", "recovery file"},
	"identity_status.state":    {"状态", "state"},

	// ux §3.1 text B: the columns of `anet find`, and the name shown for an
	// agent whose card this node did not verify (its self-description is
	// not shown either).
	"find.name":       {"名字", "name"},
	"find.charge":     {"收费", "charge"},
	"find.key":        {"密钥", "key"},
	"find.skills":     {"能力", "skills"},
	"find.aid":        {"AID", "AID"},
	"find.unverified": {"（未验证）", "(not verified)"},
	// The key column (KEL M-3): key_state fresh or stale.
	"find.key_fresh": {"已确认", "confirmed"},
	"find.key_stale": {"暂未确认", "not confirmed yet"},

	// sealed-sender §5.2: the columns of `anet peers list`; the last says
	// whether this node's messages to the peer carry its identity.
	"peers_list.aid":    {"AID", "AID"},
	"peers_list.lists":  {"名单", "lists"},
	"peers_list.sender": {"发件人身份", "sender identity"},
}

// joinSentences joins two sentences: directly in Chinese, with a space in
// English (UX-R11).
func joinSentences(a, b string, lang Language) string {
	if a == "" || b == "" {
		return a + b
	}
	if lang == Chinese {
		return a + b
	}
	return a + " " + b
}

// --- delivery ---

// Delivery state values (core §2.13), as DeliveryText reads them.
const (
	DeliveryPending     = "pending"
	DeliveryConfirmed   = "confirmed"
	DeliveryUnconfirmed = "unconfirmed"

	DeliveryNotSent     = "not_sent"
	DeliverySent        = "sent"
	DeliveryQueuedAtHub = "queued_at_hub"
)

// DeliveryText is the sentence of a message's delivery state (ux §1.4 C3):
// anet.delivery, anet.delivery_detail, anet.delivery_wait, whether it waits
// for the task's previous message (DLV-S-22), and the moment the task gives
// up on it. A pending delivery with endsAt set ends with delivery.ends_at
// (UX-R11); a terminal task passes a zero endsAt. Empty for values it does
// not know.
func DeliveryText(delivery, detail, wait string, ordered bool, endsAt time.Time, lang Language) string {
	var key string
	switch delivery {
	case DeliveryPending:
		key = waitKey(wait)
		if key == "" {
			switch detail {
			case DeliveryNotSent:
				key = "delivery.not_sent"
				if ordered {
					key = "delivery.not_sent.ordered"
				}
			case DeliverySent:
				key = "delivery.sent"
			case DeliveryQueuedAtHub:
				key = "delivery.queued_at_hub"
			default:
				return ""
			}
		}
	case DeliveryConfirmed:
		return Sentence("delivery.confirmed", lang, nil)
	case DeliveryUnconfirmed:
		switch detail {
		case DeliveryNotSent:
			return Sentence("delivery.unconfirmed.not_sent", lang, nil)
		case DeliverySent, DeliveryQueuedAtHub:
			return Sentence("delivery.unconfirmed.sent", lang, nil)
		}
		return ""
	default:
		return ""
	}
	s := Sentence(key, lang, nil)
	if !endsAt.IsZero() {
		s = joinSentences(s, Sentence("delivery.ends_at", lang, Args{"time": ceilMinute(endsAt), "now": endsAt}), lang)
	}
	return s
}

// waitKey is the sentence key of a wait code (ux §1.4 C2): the code's .wait
// variant when it has one (the identity gate's codes), else the code, when it
// is a wait; "" for none or one the table does not know as a wait.
func waitKey(wait string) string {
	if wait == "" {
		return ""
	}
	if i, ok := rowIndex[wait+".wait"]; ok {
		return reasonTable[i].Key()
	}
	if i, ok := rowIndex[wait]; ok && reasonTable[i].Class&ClassW != 0 {
		return wait
	}
	return ""
}

// ceilMinute rounds up to a whole minute (UX-R11).
func ceilMinute(t time.Time) time.Time {
	if r := t.Truncate(time.Minute); !r.Equal(t) {
		return r.Add(time.Minute)
	}
	return t
}

// outboxDeadline is one outbound row of a task as deliveryEndsAt reads it.
type outboxDeadline struct {
	// delegation marks a delegation this node sent: the row no_response_after
	// gives up on (engine ENG-T1).
	delegation bool
	handed     bool
	handedAt   time.Time
	exp        time.Time
	frozen     bool
}

// deliveryEndsAt is the moment a task gives up on its outbound rows (UX-R11):
// the earliest, over its rows, of min(handed_at + no_response_after, exp) for
// a delegation this node sent that has been handed over, is not frozen and
// has no_response_after on, and of exp for every other row; rounded up to a
// minute. Zero for a terminal task or one with no outbound row.
func deliveryEndsAt(rows []outboxDeadline, noResponseAfter time.Duration, terminal bool) time.Time {
	if terminal {
		return time.Time{}
	}
	var at time.Time
	for _, r := range rows {
		t := r.exp
		if r.delegation && r.handed && !r.frozen && noResponseAfter > 0 {
			if d := r.handedAt.Add(noResponseAfter); t.IsZero() || d.Before(t) {
				t = d
			}
		}
		if t.IsZero() {
			continue
		}
		if at.IsZero() || t.Before(at) {
			at = t
		}
	}
	if at.IsZero() {
		return at
	}
	return ceilMinute(at)
}

// Why an outbound row was given up (DLV-S-35).
const (
	// DeliveryCauseDeadline: the 14-day cutoff (engine A1), or a delegation
	// under ENG-T2.
	DeliveryCauseDeadline = "deadline"
	// DeliveryCauseScan: the no_response_after scan (ENG-T1).
	DeliveryCauseScan = "scan"
)

// DeliveryVariant chooses the variant of delivery_unconfirmed (ux §1.4 C1,
// DLV-S-35) from why the row was given up, the wait code of its last error
// and whether a version of it was last seen queued at a hub. The engine
// calls it in the transaction that gives the row up and stores the result.
func DeliveryVariant(cause, wait string, queued bool) string {
	switch {
	case cause == DeliveryCauseDeadline:
		return "expired"
	case cause == DeliveryCauseScan && wait != "":
		return "waiting"
	case queued:
		return "queued"
	}
	return "sent"
}

// UndeliverableVariant is the variant of undeliverable (engine ENG-U3): ux
// gives undeliverable one neutral sentence, so it is always "". The engine
// stores what it returns, and a variant added later needs no schema change
// (DLV-S-35).
func UndeliverableVariant(wait string, noKeys bool) string { return "" }

// --- payments ---

// PayNote is a payment decision as it was taken (HPP PAY-U5): the snapshot
// stored on the task, which PaymentNote renders without computing anything
// again (M-PAY-19).
type PayNote struct {
	// Code is a key of ux §1.4 D1, D2 or D3.
	Code string `json:"code"`
	// Amount is the quote; Price the signed price, when there is one.
	Amount uint64  `json:"amount"`
	Price  *uint64 `json:"price,omitempty"`
	// Tier is the payment tier: "auto", "agent" or "manual".
	Tier string `json:"tier"`
	// Single and Left are the tier's per-payment limit and what was left
	// today when the decision was taken (after the payment, for paid.*).
	Single uint64 `json:"single"`
	Left   uint64 `json:"left"`
	// Key and Value are the limit setting that would let the payment
	// through, for needs_operator_approval.manual_limit.
	Key   string  `json:"key,omitempty"`
	Value *uint64 `json:"value,omitempty"`
	// Why is the short reason of card_price_unavailable: timeout,
	// not_offered, bad_signature, text_task, hub_unreachable or a wait code.
	Why string `json:"why,omitempty"`
	// Peer is the payee's full AID.
	Peer string    `json:"peer,omitempty"`
	At   time.Time `json:"at"`
	// Task is the task id; not stored, the caller sets it to render.
	Task string `json:"-"`
}

// PaymentNote is the payment sentence of a snapshot (ux §1.4 D1–D3). The
// numbers are the snapshot's: nothing is computed again (HPP PAY-U6).
func PaymentNote(n PayNote, lang Language) string {
	if n.Code == "" {
		return ""
	}
	return Sentence(n.Code, lang, PayNoteArgs(n))
}

// --- status ---

// Said is a sentence a status object chose: its key and arguments.
type Said struct {
	Key  string
	Args Args
	// Text, NextText and Lvl, when Text is set, are the sentence another
	// area already rendered in this process's language (the sandbox state,
	// the outbox message); the summary shows them as they are. Key then
	// only names the sentence.
	Text, NextText string
	Lvl            Level
}

// StatusInput is what the status summary is made of (ux §4.7): for each
// topic, the sentence the area that owns it chose, and the facts that decide
// which lines show. A nil sentence is not shown.
type StatusInput struct {
	// IdentityState is /identity/status.state; no_hub hides the identity
	// line (UX-C9).
	IdentityState string
	Identity      *Said
	// Recovery is /identity/status.recovery.state: none, unprotected or
	// protected. RecoveryResume says the passphrase is set and the second
	// rotation has not run yet (KEL D-16).
	Recovery       string
	RecoveryResume bool
	// Hub is the hub line; HubWire, when set (wire_too_old, wire_too_new),
	// takes its place.
	Hub     Said
	HubWire *Said
	// HubThen is a sentence that follows the hub line (hub.next_check while a hub waits to admit this
	// node).
	HubThen *Said
	// HubRegistered hides the clock sentences of the privacy line when
	// false.
	HubRegistered bool
	Delivery      *Said
	Inbound       Said
	// Sandbox and SandboxBudget make the sandbox line, shown when either
	// trust mode is sandbox.
	Sandbox       *Said
	SandboxBudget *Said
	Payments      Said
	// The privacy line: the first of LocalStateRepeated,
	// LocalStateDetected, Clock (only when HubRegistered) and Retention.
	Retention Said
	Clock     *Said
	// Keys is the sentence of FS-U7 rows 3 and 4: keys.kept_long, else
	// keys.unknown_drops, or nil.
	Keys               *Said
	LocalStateRepeated *Said
	LocalStateDetected *Said
	Preview            *Said
}

// SummaryLine is one line of the status summary: /status.summary[i], which
// `anet status` prints and MCP node_status passes on.
type SummaryLine struct {
	Topic string `json:"topic"`
	Level Level  `json:"level"`
	Text  string `json:"text"`
	Next  string `json:"next,omitempty"`
}

// The summary topics, in their order (ux §4.7).
const (
	TopicIdentity = "identity"
	TopicRecovery = "recovery"
	TopicHub      = "hub"
	TopicDelivery = "delivery"
	TopicInbound  = "inbound"
	TopicSandbox  = "sandbox"
	TopicPayments = "payments"
	TopicPrivacy  = "privacy"
	TopicPreview  = "preview"
)

// SummaryLabel is the label of a summary topic in lang (ux §4.7): the
// first column of an `anet status` line. "" for an unknown topic.
func SummaryLabel(topic string, lang Language) string {
	l, ok := summaryLabels[topic]
	if !ok {
		return ""
	}
	if lang == Chinese {
		return l[0]
	}
	return l[1]
}

var summaryLabels = map[string][2]string{
	TopicIdentity: {"身份", "identity"},
	TopicRecovery: {"恢复", "recovery"},
	TopicHub:      {"hub", "hub"},
	TopicDelivery: {"送达", "delivery"},
	TopicInbound:  {"接单", "inbound"},
	TopicSandbox:  {"沙箱", "sandbox"},
	TopicPayments: {"付款", "payments"},
	TopicPrivacy:  {"隐私", "privacy"},
	TopicPreview:  {"预览", "preview"},
}

// Identity states that StatusSummary reads (/identity/status.state, KEL
// §4.3.1) and the recovery state that adds the recovery line (ux §4.7, line 1a).
const (
	identityNoHub       = "no_hub"
	identityStopped     = "stopped"
	identityDeactivated = "deactivated"
	recoveryUnprotected = "unprotected"
)

// PrivacyLine is the sentence of the privacy line (FS-U7): the first that
// holds of a repeated rollback, a rollback, a clock the hub could not
// confirm for a while (only while the node is registered: the hub line says
// why otherwise), expired keys kept long or messages sealed to deleted keys,
// and, when none does, the retention.
func PrivacyLine(in StatusInput) Said {
	switch {
	case in.LocalStateRepeated != nil && in.LocalStateRepeated.Key != "":
		return *in.LocalStateRepeated
	case in.LocalStateDetected != nil && in.LocalStateDetected.Key != "":
		return *in.LocalStateDetected
	case in.HubRegistered && in.Clock != nil && in.Clock.Key != "":
		return *in.Clock
	case in.Keys != nil && in.Keys.Key != "":
		return *in.Keys
	}
	return in.Retention
}

// StatusSummary is the status summary (ux §4.7): one line per topic that has
// something to say, in the order of the table there. The identity line is
// left out while there is no hub (UX-C9: the hub line says it); the
// recovery line shows whenever the identity has no recovery passphrase yet,
// no hub included, until it is stopped or deactivated (ux §4.7).
func StatusSummary(in StatusInput, lang Language) []SummaryLine {
	var out []SummaryLine
	add := func(topic string, s *Said, then *Said) {
		if s == nil || s.Key == "" {
			return
		}
		l := SummaryLine{Topic: topic, Text: Sentence(s.Key, lang, s.Args), Next: Next(s.Key, lang, s.Args)}
		if r, ok := Lookup(s.Key); ok {
			l.Level = r.Level
		}
		if s.Text != "" {
			l.Text, l.Next = s.Text, s.NextText
			if s.Lvl != "" {
				l.Level = s.Lvl
			}
		}
		if l.Level == "" {
			l.Level = LevelOK
		}
		if then != nil && then.Key != "" {
			l.Text = joinSentences(l.Text, Sentence(then.Key, lang, then.Args), lang)
		}
		out = append(out, l)
	}
	if in.IdentityState != identityNoHub {
		add(TopicIdentity, in.Identity, nil)
	}
	if in.Recovery == recoveryUnprotected && in.IdentityState != identityStopped && in.IdentityState != identityDeactivated {
		key := "identity.unprotected"
		if in.RecoveryResume {
			key = "identity.unprotected.resume"
		}
		add(TopicRecovery, &Said{Key: key}, nil)
	}
	hub := &in.Hub
	if in.HubWire != nil && in.HubWire.Key != "" {
		hub = in.HubWire
	}
	add(TopicHub, hub, in.HubThen)
	add(TopicDelivery, in.Delivery, nil)
	add(TopicInbound, &in.Inbound, nil)
	add(TopicSandbox, in.Sandbox, in.SandboxBudget)
	add(TopicPayments, &in.Payments, nil)
	privacy := PrivacyLine(in)
	add(TopicPrivacy, &privacy, nil)
	add(TopicPreview, in.Preview, nil)
	return out
}

// --- the table ---

// reasonTable is the table, in the order of ux §1.4, §3.4 (g), §4.7 and
// §5.2. Its codes are registered in ANetCore wire/registry.json
// (reasons.user, CORE-K24); TestWireRegistry checks the two agree both ways.
// A sentence key with a dot (a variant, a status line, a prompt, a doctor
// finding) lives here only (CORE-K25, K28).
//
//anet:wire reasons.user complete
var reasonTable = []Row{
	// ux §1.4 A: refused on receipt; the reason travels back with the delivery confirmation (a delegation → R, another message → M).
	{Code: "not_accepting", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方不接受你的任务，没有执行；请对方执行 anet peers allow {aid}。",
		En:       "The other side does not take tasks from you and did not run it; ask them to run: anet peers allow {aid}.",
		NextKind: NextCommand, Next: "anet peers allow {aid}"},
	{Code: "denied", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方已屏蔽你的任务，没有执行；重试无用。",
		En:       "The other side has blocked tasks from you and did not run it; retrying will not help.",
		NextKind: NextFutile},
	{Code: "capability_not_public", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方没有向你开放这项能力，没有执行；请用 anet card {peer} 查看它开放的能力，或请对方把你加入 allow 名单。",
		En:       "The other side does not offer this capability to you and did not run it; see what it offers with anet card {peer}, or ask to be put on its allow list.",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "pending_full", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方的待批队列已满，没有受理；请稍后再发，或请对方先处理待批项。",
		En:       "The other side's approval queue is full, so it was not taken; send it again later, or ask them to clear the queue.",
		NextKind: NextRetryLater},
	{Code: "busy", Class: ClassR | ClassFP, A2A: A2A{Effect: effectUnavailable, X402: "SETTLEMENT_FAILED"},
		Zh:       "对方此刻太忙，没有执行；请在 {retry} 后再发。",
		En:       "The other side is too busy right now and did not run it; send it again in {retry}.",
		NextKind: NextRetryLater},
	{Code: "caller_busy", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方那里仍有一个对同一能力的调用未结束，这次没有执行；请待其结束后再发。",
		En:       "A call to this capability is still running there, so this one was not run; send it again when that one ends.",
		NextKind: NextRetryLater},
	{Code: "quota_caller_per_min", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "本分钟内对这项能力的调用次数已用完，没有执行；请在 {retry} 后再发。",
		En:       "This minute's calls to this capability are used up, so it was not run; send it again in {retry}.",
		NextKind: NextRetryLater},
	{Code: "quota_caller_per_day", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "今天（UTC）对这项能力的调用次数已用完，没有执行；请在 {retry} 后再发。",
		En:       "Today's (UTC) calls to this capability are used up, so it was not run; send it again in {retry}.",
		NextKind: NextRetryLater},
	{Code: "args_too_large", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "参数超过对方的上限，没有执行；请缩短参数后重发（上限见 anet card {peer}）。",
		En:       "The arguments are over the other side's limit, so it was not run; shorten them and send again (limits: anet card {peer}).",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "files_count", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "文件个数不符合这项能力的要求，没有执行；请按 anet card {peer} 列出的个数附文件。",
		En:       "The number of files does not match what this capability takes, so it was not run; attach the number that anet card {peer} lists.",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "files_too_large", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "有文件超过对方的大小上限，没有执行；请压缩后重发（上限见 anet card {peer}）。",
		En:       "A file is over the other side's size limit, so it was not run; make it smaller and send again (limits: anet card {peer}).",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "file_type_not_accepted", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方不接收这种文件类型，没有执行；请换成 anet card {peer} 列出的类型。",
		En:       "The other side does not take this file type, so it was not run; use a type that anet card {peer} lists.",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "bad_request", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方不接受这个请求的格式，没有执行；请升级 anet（anet update）后重发。",
		En:       "The other side cannot accept the form of this request, so it was not run; update anet (anet update) and send again.",
		NextKind: NextCommand, Next: "anet update"},
	{Code: "key_state", Class: ClassR | ClassM, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方无法确认你的身份密钥是最新的，没有接收；请运行 anet doctor 检查你与 hub 的连接。",
		En:       "The other side could not confirm that your identity key is current and did not take it; run anet doctor to check your connection to your hub.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "invalid", Class: ClassR | ClassM, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方无法核验这条消息（时间、签名或身份记录对不上），没有接收；请运行 anet doctor 检查本机时钟与身份。",
		En:       "The other side could not verify this message (time, signature or identity record) and did not take it; run anet doctor to check this computer's clock and identity.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "key_state", Variant: "approve", Class: ClassC,
		Zh:       "对方在你批准之前做了紧急密钥轮换，这个任务可能不是它本人发的，已拒绝；重试无用。",
		En:       "The other side rotated its keys in an emergency before you approved this, so the task may not be from them and was refused; retrying will not help.",
		NextKind: NextFutile},
	{Code: "canceled", Class: ClassI,
		Zh: "没有执行：对方先收到了你的取消。",
		En: "Not run: the other side got your cancel first."},
	{Code: "task_not_found", Class: ClassF | ClassM, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方没有这个任务（已过期或已被清理）；请新建任务。",
		En:       "The other side has no such task (it expired or was removed); start a new task.",
		NextKind: NextStartOver},
	{Code: "task_ended", Class: ClassM,
		Zh:       "这条消息没有被使用：任务在对方那里已经结束；需要的话请新建任务。",
		En:       "This message was not used: the task had already ended on the other side; start a new task if you still need to.",
		NextKind: NextStartOver},
	{Code: "refused", Class: ClassR | ClassM, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方拒收了，没有说明原因；重试无用，可以直接联系对方。",
		En:       "The other side refused it without a reason; retrying will not help, so contact them directly.",
		NextKind: NextFutile},
	{Code: "unknown_code", Class: ClassR | ClassM, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方拒收了（原因码 {code}）；请联系对方了解原因。",
		En:       "The other side refused it (reason code {code}); ask them what it means.",
		NextKind: NextContact},

	// ux §1.4 B: refused or failed after the other side took it (its status message, or this node's verdict).
	{Code: "pending_approval", Class: ClassW,
		Zh:       "对方要先人工批准这个任务，已进入待批队列；批准前不会执行，不需要操作。",
		En:       "The other side approves tasks by hand and has queued this one; it will not run before they approve it, nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "pending_expired", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方运营者 72 小时内没有批准这个任务，已过期，没有执行；需要的话请先联系对方再重发。",
		En:       "The other side's operator did not approve this task within 72 hours, so it expired unrun; contact them before sending it again if you still need it.",
		NextKind: NextContact},
	{Code: "operator_rejected", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方运营者拒绝了这个任务，没有执行；重试无用。",
		En:       "The other side's operator turned this task down, so it was not run; retrying will not help.",
		NextKind: NextFutile},
	{Code: "sandbox_unavailable", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方节点的沙箱此刻不能运行你的任务，没有执行；请稍后再发，或联系对方运营者。",
		En:       "The other side's sandbox cannot run your task right now, so it was not run; send it again later, or contact its operator.",
		NextKind: NextRetryLater},
	{Code: "budget", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方节点今天为未信任对端运行 agent 的次数已用完，没有执行；请在 {retry} 后再发。",
		En:       "The other side has used up today's runs for peers it does not trust, so it was not run; send it again in {retry}.",
		NextKind: NextRetryLater},
	{Code: "provider_policy", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方节点不允许远程调用这项能力，没有执行；请用 anet card {peer} 查看其他能力，或联系对方运营者。",
		En:       "The other side does not allow remote calls to this capability, so it was not run; see its other capabilities with anet card {peer}, or contact its operator.",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "capability_not_served", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方现在没有提供这项能力，没有执行；请用 anet card {peer} 查看它提供的能力。",
		En:       "The other side does not serve this capability now, so it was not run; see what it serves with anet card {peer}.",
		NextKind: NextCommand, Next: "anet card {peer}"},
	{Code: "payments_unavailable", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "这项能力要付款，但对方节点现在收不了款，没有执行；请稍后再试，或联系对方。",
		En:       "This capability is paid, but the other side cannot take payments right now, so it was not run; try again later, or contact them.",
		NextKind: NextRetryLater},
	{Code: "unavailable", Class: ClassR, A2A: A2A{Effect: effectUnavailable},
		Zh:       "对方暂时不能执行这项能力，没有执行；请稍后再发。",
		En:       "The other side cannot run this capability right now, so it was not run; send it again later.",
		NextKind: NextRetryLater},
	{Code: "timeout", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方执行超时，不知道是否已经生效；不要直接重发，先问对方，或用 anet task get {task} 查看。",
		En:       "The other side timed out, and whether it took effect is not known; do not just send it again: ask them first, or check with anet task get {task}.",
		NextKind: NextCommand, Next: "anet task get {task}"},
	{Code: "connection_lost", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方执行时与它的服务断开，不知道是否已经生效；不要直接重发，先问对方。",
		En:       "The other side lost its connection to its service while running this, so whether it took effect is not known; do not just send it again, ask them first.",
		NextKind: NextContact},
	{Code: "interrupted", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方节点在执行中重启，不知道是否已经生效；不要直接重发，先问对方。",
		En:       "The other side restarted while running this, so whether it took effect is not known; do not just send it again, ask them first.",
		NextKind: NextContact},
	{Code: "no_response", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方已确认收到，但 {limit} 内没有任何回应；它可能还在执行，之后到达的答复仍会记下，可用 anet task get {task} 查看。",
		En:       "The other side confirmed it got this but has not answered within {limit}; it may still be running, and a later answer is still recorded: check with anet task get {task}.",
		NextKind: NextCommand, Next: "anet task get {task}"},
	{Code: "backend_failed", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方的服务执行失败，没有说明原因；请稍后重试，或联系对方。",
		En:       "The other side's service failed without saying why; try again later, or contact them.",
		NextKind: NextRetryLater},
	{Code: "bad_image", Class: ClassF, A2A: A2A{Effect: effectFailed},
		Zh:       "对方读不了这张图片（损坏或尺寸过大），没有结果；请换一张图片重发。",
		En:       "The other side could not read the image (damaged or too large), so there is no result; use a different image and send again.",
		NextKind: NextChangeInput},
	{Code: "no_face_in_a", Class: ClassF, A2A: A2A{Effect: effectFailed},
		Zh:       "第一张图片里没有找到人脸，没有结果；请换一张清晰的正脸照片重发。",
		En:       "No face was found in the first image, so there is no result; use a clear, front-facing photo and send again.",
		NextKind: NextChangeInput},
	{Code: "no_face_in_b", Class: ClassF, A2A: A2A{Effect: effectFailed},
		Zh:       "第二张图片里没有找到人脸，没有结果；请换一张清晰的正脸照片重发。",
		En:       "No face was found in the second image, so there is no result; use a clear, front-facing photo and send again.",
		NextKind: NextChangeInput},
	{Code: "result_refused", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方给出的结果与它签名的对话记录不符，本机已丢弃；请联系对方，或重新发起任务。",
		En:       "The other side's result does not match the conversation it signed, so this node discarded it; contact them, or start the task again.",
		NextKind: NextContact},
	{Code: "receipt_mismatch", Class: ClassI,
		Zh: "对方签名的对话记录改动了你说过的话，本机不据此记证据，也不能评价；结果仍可查看。",
		En: "The conversation the other side signed changes what you said, so this node keeps no evidence of it and you cannot review it; you can still read the result."},
	{Code: "deduplicated", Class: ClassI,
		Zh: "这是对刚才那次调用的重试，返回原任务，没有再次发送。",
		En: "This is a retry of the call just made: the original task is returned and nothing was sent again."},

	// ux §1.4 C1: a message this node sent ends undelivered.
	{Code: "delivery_unconfirmed", Variant: "queued", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "{limit} 内没有确认送达：信还在 hub 排队（对方可能不在线），anet 已请对方取消；仍需要的话请稍后重发。",
		En:       "Delivery was not confirmed within {limit}: it is still waiting at the hub (the other side may be offline), and anet asked them to cancel it; send it again later if you still need it.",
		NextKind: NextRetryLater},
	{Code: "delivery_unconfirmed", Variant: "waiting", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "{limit} 内没有确认送达（最后一次：{wait}），anet 已请对方取消；仍需要的话请稍后重发。",
		En:       "Delivery was not confirmed within {limit} (last: {wait}), and anet asked them to cancel it; send it again later if you still need it.",
		NextKind: NextRetryLater},
	{Code: "delivery_unconfirmed", Variant: "sent", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "{limit} 内没有确认送达：hub 没有保存它，或一直连不上，anet 已请对方取消；仍需要的话请稍后重发。",
		En:       "Delivery was not confirmed within {limit}: the hub did not keep it or could not be reached, and anet asked them to cancel it; send it again later if you still need it.",
		NextKind: NextRetryLater},
	{Code: "delivery_unconfirmed", Variant: "expired", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "14 天内没有确认送达，对方仍可能收到过；重发前请先问对方。",
		En:       "Delivery was not confirmed in 14 days, and the other side may still have got it; ask them before sending it again.",
		NextKind: NextContact},
	{Code: "undeliverable", Class: ClassF, A2A: A2A{Effect: effectUnavailable},
		Zh:       "没有发出：消息过期前一直没能离开本机，对方没有收到；请运行 anet status 查看原因后重发。",
		En:       "Not sent: it never left this node before it expired, so the other side never got it; see why with anet status, then send it again.",
		NextKind: NextCommand, Next: "anet status"},
	{Code: "recipient_unknown", Class: ClassF | ClassE | ClassC, A2A: A2A{Effect: effectUnverified, Err: A2AInvalidParams},
		Zh:       "找不到收件方：没有 hub 知道这个 AID；请用 anet find 核对 AID 后重发。",
		En:       "The recipient cannot be found: no hub knows this AID; check it with anet find and send again.",
		NextKind: NextCommand, Next: "anet find"},
	{Code: "recipient_unverifiable", Class: ClassE | ClassC, A2A: A2A{Err: A2ATransient},
		Zh:       "无法核验对方的身份记录或加密钥（密钥历史分叉或签名不符），什么都没有发出；请运行 anet doctor，并请对方确认身份后再发。",
		En:       "The other side's identity record or encryption keys cannot be verified (a forked key history or a bad signature), so nothing was sent; run anet doctor, and ask them to confirm their identity before sending again.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "too_large", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "hub 拒收：超过它的大小上限；请减小附件后重发。",
		En:       "The hub refused it: it is over the hub's size limit; make the files smaller and send again.",
		NextKind: NextChangeInput},
	{Code: "too_large_for_stranger", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "对方只收非联系人 64 MiB 以内的信；请减小附件，或请对方把你加入 allow 名单并回你一条消息后再发。",
		En:       "The other side takes at most 64 MiB from senders who are not yet its contacts; send smaller files, or ask them to put you on their allow list and reply to you once, then send again.",
		NextKind: NextChangeInput},
	{Code: "bad_envelope", Class: ClassF, A2A: A2A{Effect: effectUnverified},
		Zh:       "hub 不接受这条消息的格式；请升级 anet（anet update）后重发。",
		En:       "The hub cannot accept the form of this message; update anet (anet update) and send again.",
		NextKind: NextCommand, Next: "anet update"},

	// ux §1.4 C2: waits (anet.delivery_wait); the task is unchanged and status.message is not written.
	{Code: "rate_limited", Class: ClassW,
		Zh:       "发送过快，hub 要求稍等；anet 会自动重试。",
		En:       "Sending is too fast and the hub asked for a wait; anet retries on its own.",
		NextKind: NextAuto},
	{Code: "redelivery_limited", Class: ClassW,
		Zh:       "发送过快，hub 要求稍等；anet 会自动重试。",
		En:       "Sending is too fast and the hub asked for a wait; anet retries on its own.",
		NextKind: NextAuto},
	{Code: "strangers_busy", Class: ClassW,
		Zh:       "对方接收非联系人来信的通道已满，信在排队；对方取信后会自动送出。",
		En:       "The other side's inbox for senders who are not yet its contacts is full, so this is queued; it goes out on its own once they collect their mail.",
		NextKind: NextAuto},
	{Code: "mailbox_full", Class: ClassW,
		Zh:       "对方信箱已满，信在排队；对方取信后会自动送出。",
		En:       "The other side's mailbox is full, so this is queued; it goes out on its own once they collect their mail.",
		NextKind: NextAuto},
	{Code: "home_unreachable", Class: ClassW,
		Zh:       "暂时连不上对方所在的 hub，信在排队；anet 会自动重试。",
		En:       "The other side's hub cannot be reached right now, so this is queued; anet retries on its own.",
		NextKind: NextAuto},
	{Code: "session_unavailable", Class: ClassW, Level: LevelWarn,
		Zh:       "本节点的 hub 暂时不接受首次联系，信在排队；请运行 anet status 查看 hub 状态。",
		En:       "This node's hub is not taking first contacts right now, so this is queued; run anet status to see the hub's state.",
		NextKind: NextCommand, Next: "anet status"},
	{Code: "credential_refused", Class: ClassW,
		Zh:       "hub 拒绝了对方给你的投递凭据，信在排队；10 分钟后仍被拒就会自动改走首次联系（hub 会因此知道是你发的）。",
		En:       "The hub refused the delivery credential the other side gave you, so this is queued; if it is still refused after 10 minutes it goes on its own as a first contact (and the hub then knows it is from you).",
		NextKind: NextAuto},
	{Code: "notice_stale", Variant: "wait", Class: ClassW,
		Zh:       "本节点的 hub 还没确认最新密钥，信在排队；确认后会自动发出，详见 anet status 的身份行。",
		En:       "This node's hub has not confirmed the current key yet, so this is queued; it goes out on its own once it does (see the identity line of anet status).",
		NextKind: NextAuto},
	{Code: "rotation_pending", Variant: "wait", Class: ClassW,
		Zh:       "密钥轮换正在发布，信在排队；发布完成后会自动发出。",
		En:       "A key rotation is being published, so this is queued; it goes out on its own once that is done.",
		NextKind: NextAuto},

	// ux §1.4 C4: the outbox (/status.delivery.message, doctor delivery.outbox).
	{Code: "outbox.ok", Class: ClassS, Level: LevelOK,
		Zh: "{n} 条消息等待对方确认，最久 {dur}。",
		En: "{n} messages are waiting for confirmation; the oldest for {dur}."},
	{Code: "outbox.stuck", Class: ClassS, Level: LevelWarn,
		Zh:       "{n} 条消息已有 {dur} 没能发出：连不上 hub（最后错误：{error}）；请检查网络与 hub 地址。",
		En:       "{n} messages have not left this node for {dur}: the hub cannot be reached (last error: {error}); check the network and the hub address.",
		NextKind: NextCommand, Next: "anet status"},
	{Code: "outbox.unconfirmed", Class: ClassS, Level: LevelWarn,
		Zh: "{n} 条 24 小时前发出的消息还没有确认：对方一直没有取信；不需要操作，anet 会一直查，直到 14 天后过期。",
		En: "{n} messages sent over 24 hours ago are still unconfirmed: the other side has not collected them; nothing to do, anet keeps checking until they expire after 14 days."},
	{Code: "outbox.none", Class: ClassS, Level: LevelOK,
		Zh: "没有等待确认的消息。",
		En: "No messages are waiting for confirmation."},
	{Code: "outbox.unreadable", Class: ClassS, Level: LevelUnknown,
		Zh:       "无法只读打开本机的出站队列（{error}）；daemon 在运行时，anet status 会显示送达情况。",
		En:       "This node's outbox cannot be opened read-only ({error}); while the daemon runs, anet status shows delivery.",
		NextKind: NextCommand, Next: "anet status"},

	// ux §1.4 C5: what no_response_after means (doctor tasks.no_response).
	{Code: "no_response_after.set", Class: ClassI, Level: LevelOK,
		Zh: "本节点发出的任务 {limit} 内没有确认送达即算失败；确认送达后 {limit} 内对方没有任何回应也算失败，之后到达的答复仍会记下。",
		En: "A task this node sends fails if its delivery is not confirmed within {limit}, or if it is confirmed but the other side does not answer within {limit} after that; a later answer is still recorded."},
	{Code: "no_response_after.off", Class: ClassI, Level: LevelInfo,
		Zh: "no_response_after 为 0：没有确认送达的任务会一直重发，14 天后过期才算失败；已确认而对方一直不回应的任务保持等待，不会自动算失败。",
		En: "no_response_after is 0: a task whose delivery is not confirmed is resent until it expires after 14 days, and a confirmed task the other side never answers keeps waiting and does not fail on its own."},

	// ux §1.4 D1: waiting for a person to decide on a payment (anet.payment_note is status.message).
	{Code: "needs_operator_approval", Variant: "off", Class: ClassIP,
		Zh:       "报价 {amount} credits；{tier}没有开启（额度为 0），没有付款；请执行 anet pay {task}。",
		En:       "Quoted {amount} credits; {tier} is off (limit 0), so nothing was paid; run anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "needs_operator_approval", Variant: "over_limit", Class: ClassIP,
		Zh:       "报价 {amount} credits，超出{tier}额度（单笔 ≤{single}，今日剩 {left}），没有付款；请执行 anet pay {task}。",
		En:       "Quoted {amount} credits, above {tier} (≤{single} per payment, {left} left today), so nothing was paid; run anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "needs_operator_approval", Variant: "manual_limit", Class: ClassIP,
		Zh:       "报价 {amount} credits，也超出人工付款上限（单笔 ≤{single}，今日剩 {left}），没有付款；请先执行 anet payments set {key}={value}，再执行 anet pay {task}。",
		En:       "Quoted {amount} credits, above even the manual limit (≤{single} per payment, {left} left today), so nothing was paid; run anet payments set {key}={value}, then anet pay {task}.",
		NextKind: NextCommand, Next: "anet payments set {key}={value}"},
	{Code: "needs_operator_approval", Variant: "payee_not_allowed", Class: ClassIP,
		Zh:       "报价 {amount} credits，收款方不在付款名单里，没有自动付款；请执行 anet pay {task}（以后要自动付给它：anet payees add {peer}）。",
		En:       "Quoted {amount} credits; the payee is not on your payee list, so nothing was paid automatically; run anet pay {task} (to pay it automatically from now on: anet payees add {peer}).",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "needs_operator_approval", Variant: "payees_unreadable", Class: ClassIP,
		Zh:       "报价 {amount} credits，付款名单读不了，没有自动付款；请执行 anet pay {task}，再用 anet doctor 修复名单。",
		En:       "Quoted {amount} credits; the payee list cannot be read, so nothing was paid automatically; run anet pay {task}, then fix the list with anet doctor.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "quote_above_card_price", Class: ClassIP,
		Zh:       "报价 {amount} credits，高于对方签名的价格 {price} credits，没有自动付款；确认要付请执行 anet pay {task}。",
		En:       "Quoted {amount} credits, above the {price} credits the other side signed, so nothing was paid automatically; to pay anyway run anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "card_price_unavailable", Class: ClassIP,
		Zh:       "报价 {amount} credits，但没有对方签名的价格（{why}），没有自动付款；确认要付请执行 anet pay {task}。",
		En:       "Quoted {amount} credits, but there is no price signed by the other side ({why}), so nothing was paid automatically; to pay anyway run anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},

	// ux §1.4 D2: paid.
	{Code: "paid.auto", Class: ClassI,
		Zh: "报价 {amount} credits，在你的自动付款额度内（单笔 ≤{single}，今日剩 {left}），已自动付款。",
		En: "Quoted {amount} credits, within your automatic limit (≤{single} per payment, {left} left today): paid automatically."},
	{Code: "paid.agent", Class: ClassI,
		Zh: "报价 {amount} credits，在 agent 付款额度内（单笔 ≤{single}，今日剩 {left}），已付款。",
		En: "Quoted {amount} credits, within the agent limit (≤{single} per payment, {left} left today): paid."},
	{Code: "paid.manual", Class: ClassI,
		Zh: "已付 {amount} credits（你在终端确认的，今日剩 {left}）。",
		En: "Paid {amount} credits (you confirmed it in the terminal; {left} left today)."},

	// ux §1.4 D3: a payment that did not settle.
	{Code: "quote_expired", Class: ClassFP, A2A: A2A{X402: "EXPIRED_PAYMENT"},
		Zh:       "报价已过期，没有付款；请重新发起任务，获取新报价。",
		En:       "The quote has expired, so nothing was paid; start the task again for a new quote.",
		NextKind: NextStartOver},
	{Code: "rail_not_payable", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "所选付款选项要在本节点没有余额的 hub 上结算，没有付款；请换一个付款选项。",
		En:       "The chosen option settles at a hub where this node holds no credit, so nothing was paid; use another option.",
		NextKind: NextChangeInput},
	{Code: "option_not_offered", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "所选付款选项不在报价里，没有付款；请改用从 x402.payment.required.accepts 原样复制的一个选项。",
		En:       "The chosen option is not in the quote, so nothing was paid; use one copied unchanged from x402.payment.required.accepts.",
		NextKind: NextChangeInput},
	{Code: "client_payload_unsupported", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "本节点替你签名付款，不接受客户端自带的付款载荷；请改为只发 payment-submitted，不带 x402.payment.payload。",
		En:       "This node signs payments for you and does not take a payload from the client; send it again as payment-submitted without x402.payment.payload.",
		NextKind: NextChangeInput},
	{Code: "no_pending_quote", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "这个任务没有待付的报价，没有付款；不需要操作。",
		En:       "This task has no quote waiting to be paid, so nothing was paid; nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "binding_mismatch", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "对方拒收了这笔付款（授权与这个任务不匹配），没有扣款；请重新发起任务。",
		En:       "The other side refused this payment (the authorization does not match this task), and nothing was charged; start the task again.",
		NextKind: NextStartOver},
	{Code: "payer_mismatch", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "对方拒收了这笔付款（签名人不是这个任务的请求方），没有扣款；请重新发起任务。",
		En:       "The other side refused this payment (it was not signed by this task's requester), and nothing was charged; start the task again.",
		NextKind: NextStartOver},
	{Code: "payee_not_local", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "付款没有结算：处理这笔付款的 hub 上没有这个收款方，没有扣款；请重新付款：anet pay {task}。",
		En:       "Not settled: the payee is not on the hub that handled this payment, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "auth_window_too_long", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "付款授权的有效期超过 24 小时，hub 不接受，没有扣款；请重新付款：anet pay {task}。",
		En:       "The payment authorization is valid for more than 24 hours, which the hub refuses, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "x402:INSUFFICIENT_FUNDS", Class: ClassFP, A2A: A2A{X402: "INSUFFICIENT_FUNDS"},
		Zh:       "余额不足，没有扣款；请用 anet balance 查看余额。",
		En:       "Not enough credit, and nothing was charged; see your balance with anet balance.",
		NextKind: NextCommand, Next: "anet balance"},
	{Code: "x402:INVALID_SIGNATURE", Class: ClassFP, A2A: A2A{X402: "INVALID_SIGNATURE"},
		Zh:       "付款签名没有通过核验，没有扣款；请重新付款：anet pay {task}。",
		En:       "The payment signature did not verify, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "x402:INVALID_SIGNATURE", Variant: "rotated", Class: ClassFP, A2A: A2A{X402: "INVALID_SIGNATURE"},
		Zh:       "这笔授权是用已轮换掉的旧密钥签的，hub 拒收，没有扣款；请重新付款：anet pay {task}。",
		En:       "This authorization was signed with a key you have since rotated away, so the hub refused it, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "x402:EXPIRED_PAYMENT", Class: ClassFP, A2A: A2A{X402: "EXPIRED_PAYMENT"},
		Zh:       "付款授权提交时已经过期，没有扣款；请重新付款：anet pay {task}。",
		En:       "The payment authorization had expired when it arrived, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "x402:DUPLICATE_NONCE", Class: ClassFP, A2A: A2A{X402: "DUPLICATE_NONCE"},
		Zh:       "这笔授权已经用过，没有重复扣款；不需要操作。",
		En:       "This authorization was already used, and nothing was charged twice; nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "x402:NETWORK_MISMATCH", Class: ClassFP, A2A: A2A{X402: "NETWORK_MISMATCH"},
		Zh:       "付款授权签的是另一个网络，没有扣款；请重新付款：anet pay {task}。",
		En:       "The authorization was signed for another network, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "x402:INVALID_AMOUNT", Class: ClassFP, A2A: A2A{X402: "INVALID_AMOUNT"},
		Zh:       "付款金额与报价不符，没有扣款；请重新付款：anet pay {task}。",
		En:       "The amount does not match the quote, and nothing was charged; pay again: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "x402:SETTLEMENT_FAILED", Class: ClassFP, A2A: A2A{X402: "SETTLEMENT_FAILED"},
		Zh:       "付款没有结算成功，没有扣款；请稍后重新付款：anet pay {task}。",
		En:       "The payment did not settle, and nothing was charged; pay again later: anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},

	// ux §1.4 D4: this node's spending refusals (/tasks/pay, MCP submit_payment).
	{Code: "over_single_limit", Class: ClassC,
		Zh:       "{amount} credits 超出单笔上限 {single}，没有付款；要付请先执行 anet payments set {key}={amount}。",
		En:       "{amount} credits is above the per-payment limit {single}, so nothing was paid; to pay it, first run anet payments set {key}={amount}.",
		NextKind: NextCommand, Next: "anet payments set {key}={amount}"},
	{Code: "over_daily_limit", Class: ClassC,
		Zh:       "今天已付 {spent}，再付 {amount} 会超出每日上限 {daily}，没有付款；明天再付，或执行 anet payments set daily_max={need}。",
		En:       "{spent} was already paid today, and {amount} more would pass the daily limit {daily}, so nothing was paid; pay tomorrow, or run anet payments set daily_max={need}.",
		NextKind: NextCommand, Next: "anet payments set daily_max={need}"},
	{Code: "over_agent_daily_limit", Class: ClassC,
		Zh:       "agent 今天已付 {spent}，再付 {amount} 会超出 agent 每日上限 {daily}，没有付款；请让运营者执行 anet pay {task}。",
		En:       "The agent already paid {spent} today, and {amount} more would pass the agent daily limit {daily}, so nothing was paid; ask the operator to run anet pay {task}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "payee_not_allowed", Class: ClassC,
		Zh:       "收款方 {peer8} 不在付款名单里，agent 不能付；请让运营者执行 anet pay {task}，或 anet payees add {peer}。",
		En:       "The payee {peer8} is not on the payee list, so the agent cannot pay it; ask the operator to run anet pay {task}, or anet payees add {peer}.",
		NextKind: NextCommand, Next: "anet pay {task}"},
	{Code: "payees_unreadable", Class: ClassC,
		Zh:       "付款名单读不了，agent 付款已暂停；请运行 anet doctor 查看。",
		En:       "The payee list cannot be read, so agent payments are paused; run anet doctor.",
		NextKind: NextCommand, Next: "anet doctor"},

	// ux §1.4 D5: the limits at a glance.
	{Code: "limits_summary", Class: ClassS, Level: LevelInfo,
		Zh: "自动付款单笔 ≤{auto_max}；agent 付款单笔 ≤{agent_max}，agent 今日剩 {agent_left}；人工付款（anet pay）单笔 ≤{explicit_max}；三档今日合计还能付 {left}；自动与 agent 付款只付给付款名单里的 {n} 个收款方。",
		En: "Automatic payments: ≤{auto_max} each; agent payments: ≤{agent_max} each, {agent_left} left today for the agent; manual payments (anet pay): ≤{explicit_max} each; {left} left today across all three; automatic and agent payments go only to the {n} payees on your list."},
	{Code: "limits_summary", Variant: "off", Class: ClassS, Level: LevelInfo,
		Zh: "自动付款与 agent 付款都关着（额度为 0）；人工付款（anet pay）单笔 ≤{explicit_max}，今日还能付 {left}。",
		En: "Automatic and agent payments are off (limit 0); manual payments (anet pay): ≤{explicit_max} each, {left} left today."},

	// ux §1.4 E1: tasks ended by an identity or local-data event.
	{Code: "identity_stopped", Class: ClassF | ClassE | ClassS, Level: LevelFail, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "这个身份已在别处恢复或轮换，本机已停止收发；是你本人所为就删掉本机副本（anet identity rm {name} --purge），否则说明恢复口令已泄露，请新建身份并告诉联系人。",
		En:       "This identity was recovered or rotated elsewhere, so this node has stopped; if that was you, remove this copy (anet identity rm {name} --purge); if not, your recovery passphrase has leaked: create a new identity and tell your contacts.",
		NextKind: NextCommand, Next: "anet identity rm {name} --purge"},
	{Code: "identity_deactivated", Class: ClassF | ClassE | ClassS, Level: LevelFail, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "这个身份已注销，不能再收发；请新建身份：anet id new <名字>。",
		En:       "This identity is deactivated and can no longer send or receive; create a new one: anet id new <name>.",
		NextKind: NextCommand, Next: "anet id new <name>", NextZh: "anet id new <名字>"},
	{Code: "local_state_lost", Class: ClassF,
		Zh:       "本机数据比这个身份在 hub 上最后登记的旧，这个在途任务已结束，需要请重新发起；如果你没有从备份恢复或复制数据目录，说明这个身份正在别处运行、数据目录可能已泄露，请运行 {next}。",
		En:       "This node's data is older than its identity's last state at the hub, so this task was ended; start it again if needed. Unless you restored a backup or copied the data directory, this identity runs elsewhere and may have leaked: run {next}.",
		NextKind: NextCommand, Next: "anet identity rotate --emergency"},
	{Code: "local_state_lost", Variant: "peer", Class: ClassF,
		Zh:       "对方节点的数据回退到了旧状态（例如从备份恢复），对方已结束这个任务；需要的话请重新发起。",
		En:       "The other node's data went back to an older state (for example a restored backup), and it ended this task; start it again if you still need it.",
		NextKind: NextStartOver},
	{Code: "key_compromise", Class: ClassF,
		Zh:       "已做紧急密钥轮换，为防泄露的密钥继续被使用，在途任务已全部结束；需要的话请重新发起。",
		En:       "An emergency key rotation was made, so all tasks in progress were ended to stop the leaked keys from being used; start them again if still needed.",
		NextKind: NextStartOver},
	{Code: "key_compromise", Variant: "peer", Class: ClassF,
		Zh:       "对方做了紧急密钥轮换并结束了这个任务；重新发起即可，会自动使用对方的新密钥。",
		En:       "The other side rotated its keys in an emergency and ended this task; start it again and its new keys will be used.",
		NextKind: NextStartOver},

	// ux §1.4 E2: identity operations refused (CLI, /identity/*, /hub-register).
	{Code: "tty_required", Class: ClassC,
		Zh:       "{action}需要在你自己的终端里确认；请在终端运行：{command}。",
		En:       "{action} has to be confirmed in your own terminal; run it there: {command}.",
		NextKind: NextCommand, Next: "{command}"},
	{Code: "recovery_file_unprotected", Class: ClassC,
		Zh:       "这份恢复文件还没有设口令，只能与它所在的数据目录一起使用，不能用于恢复；请把整个数据目录复制到这台机器上运行，再运行 anet identity protect。",
		En:       "This recovery file has no passphrase yet and works only with its own data directory, not for recovery; copy the whole data directory to this machine, run anet with it, then run anet identity protect.",
		NextKind: NextCommand, Next: "anet identity protect"},
	{Code: "recovery_file_missing", Class: ClassC,
		Zh:       "找不到恢复文件 {path}；请加 --recovery-file <你另存的那份>。",
		En:       "The recovery file {path} is not there; add --recovery-file <the copy you saved>.",
		NextKind: NextChangeInput},
	{Code: "recovery_passphrase_wrong", Class: ClassC,
		Zh:       "口令不对，恢复文件没有解开；请重新输入口令后重试，或换另一份恢复文件。",
		En:       "Wrong passphrase: the recovery file did not open; try again, or use another copy of the file.",
		NextKind: NextChangeInput},
	{Code: "recovery_wrong_identity", Class: ClassC,
		Zh:       "这份恢复文件属于 {file_aid8}，不是 {aid8}（或文件被改过）；请换成这个身份的恢复文件。",
		En:       "This recovery file belongs to {file_aid8}, not {aid8} (or it was altered); use this identity's recovery file.",
		NextKind: NextChangeInput},
	{Code: "recovery_mismatch", Class: ClassC,
		Zh:       "这份恢复文件是旧的，与身份当前的密钥承诺不符；请换成最近一次生成的恢复文件。",
		En:       "This recovery file is out of date and does not match the identity's current key commitment; use the most recent one.",
		NextKind: NextChangeInput},
	{Code: "passphrase_too_short", Class: ClassC,
		Zh:       "口令至少要 12 个字符；请换一个更长的口令，例如 4 个以上的随机词。",
		En:       "The passphrase needs at least 12 characters; use a longer one, such as four or more random words.",
		NextKind: NextChangeInput},
	{Code: "passphrase_not_utf8", Class: ClassC,
		Zh:       "口令里有无法识别的字符（终端不是 UTF-8 编码）；请把终端切换为 UTF-8（例如 LANG=C.UTF-8）后重试。",
		En:       "The passphrase has characters that cannot be read (the terminal is not using UTF-8); switch the terminal to UTF-8 (for example LANG=C.UTF-8) and try again.",
		NextKind: NextChangeInput},
	{Code: "no_hub_yet", Class: ClassE | ClassC | ClassS, Level: LevelInfo, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "还没有接入 hub，暂时不能收发；请运行 anet hub-register {official_hub} 接入官方公共 hub，或把地址换成你所在组织的 hub。",
		En:       "Not joined to a hub yet, so nothing can be sent or received; run anet hub-register {official_hub} to join the official public hub, or use your organization's hub URL instead.",
		NextKind: NextCommand, Next: "anet hub-register {official_hub}"},
	{Code: "witness_unreachable", Class: ClassC,
		Zh:       "连不上本节点的见证 hub {hub}，计划内轮换需要它在线；请稍后重试，确有泄露请用 anet identity rotate --emergency。",
		En:       "The witness hub {hub} cannot be reached, and a scheduled rotation needs it online; try again later, or if a key has leaked use anet identity rotate --emergency.",
		NextKind: NextRetryLater},
	{Code: "rotation_pending", Class: ClassC | ClassS, Level: LevelWarn,
		Zh:       "上一次密钥轮换还在发布，期间暂停收发（不会丢消息）；anet 会自动重试，可用 anet identity status 查看。",
		En:       "The last key rotation is still being published, so sending and receiving are paused (nothing is lost); anet retries on its own, see anet identity status.",
		NextKind: NextCommand, Next: "anet identity status"},
	{Code: "hub_switch_requires_rotation", Class: ClassC,
		Zh:       "加入这个 hub（首次加入或从别的 hub 换过来）需要一次密钥轮换，要输入恢复口令；请在终端运行 anet hub-register {hub}。",
		En:       "Joining this hub (for the first time, or coming from another hub) needs a key rotation with your recovery passphrase; run anet hub-register {hub} in a terminal.",
		NextKind: NextCommand, Next: "anet hub-register {hub}"},
	{Code: "hub_switch_pending_admission", Class: ClassC,
		Zh:       "新 hub {hub} 还没有准入你的 AID；请把这一行发给它的运营者：anet-hub admit {aid}，批准后再运行 anet hub-register {hub}。",
		En:       "The new hub {hub} has not admitted your AID yet; send this line to its operator: anet-hub admit {aid}, and run anet hub-register {hub} again once they have.",
		NextKind: NextCommand, Next: "anet-hub admit {aid}"},
	{Code: "witness_hub_leave", Class: ClassC,
		Zh:       "{hub} 是你的见证 hub，离开它就没人能确认你的密钥，你将无法收发；要换 hub 请运行 anet hub-register <新 hub>。",
		En:       "{hub} is your witness hub: without it nobody can confirm your key and you could not send or receive; to switch hubs run anet hub-register <new hub>.",
		NextKind: NextCommand, Next: "anet hub-register <new hub>", NextZh: "anet hub-register <新 hub>"},
	{Code: "recover_target_exists", Class: ClassC,
		Zh:       "本机已有名为 {name} 的身份；请加 --name <新名字>。",
		En:       "An identity named {name} already exists here; add --name <new name>.",
		NextKind: NextChangeInput},
	{Code: "recover_not_witness", Class: ClassC,
		Zh:       "{hub} 不是这个身份的见证 hub；请加 --hub <它的见证 hub 地址>。",
		En:       "{hub} is not this identity's witness hub; add --hub <its witness hub URL>.",
		NextKind: NextChangeInput},
	{Code: "clock_skew", Class: ClassC,
		Zh:       "本机时钟与 hub 相差超过 5 分钟；请校准系统时间（例如启用 NTP）后重试。",
		En:       "This computer's clock is more than 5 minutes off from the hub; fix the system time (for example enable NTP) and try again.",
		NextKind: NextChangeInput},
	{Code: "identity_v02", Class: ClassC,
		Zh:       "这是 anet 0.2 的数据目录，0.3.0 不迁移旧数据；请把这个目录移走后运行 anet up 生成新身份。",
		En:       "This data directory is from anet 0.2, and 0.3.0 does not migrate old data; move it away and run anet up to create a new identity.",
		NextKind: NextCommand, Next: "anet up"},
	// A data directory a newer anet wrote (core CORE-V4, FS-S1): not an old
	// one, so the next step is to upgrade, never to move it away.
	{Code: "datadir_too_new", Class: ClassC,
		Zh:       "这个数据目录由更新版本的 anet 写入，本版本打不开；请运行 anet update 升级后再启动（不要删除这个目录）。",
		En:       "This data directory was written by a newer anet, which this version cannot open; run anet update and start again (do not delete the directory).",
		NextKind: NextCommand, Next: "anet update"},
	{Code: "identity_file_invalid", Class: ClassC,
		Zh:       "身份文件 {path} 已损坏，读不出来，anet 没有启动；请从备份恢复这个文件后重试，或在终端运行 anet identity recover 用恢复文件找回身份。",
		En:       "The identity file {path} is damaged and cannot be read, so anet did not start; restore it from a backup and try again, or run anet identity recover in a terminal to get the identity back from its recovery file.",
		NextKind: NextChangeInput},
	{Code: "rotation_emergency_done", Class: ClassI,
		Zh:       "紧急轮换已完成；请检查 peers 名单与 config.json 是否被改过，到各服务商更换模型 API key，重启已接入的编码工具，A2A 客户端请重新运行 anet agents wire --all。",
		En:       "The emergency rotation is done; check your peer lists and config.json for changes, replace model API keys with each provider, restart wired coding tools, and run anet agents wire --all again for A2A clients.",
		NextKind: NextCommand, Next: "anet agents wire --all"},
	{Code: "identity_deactivated", Variant: "unconfirmed", Class: ClassI,
		Zh:       "已在本机注销；你的见证 hub {hub} 还没确认，anet 会自动重试，直到它确认。",
		En:       "Deactivated on this node; your witness hub {hub} has not confirmed it yet, and anet keeps retrying on its own until it does.",
		NextKind: NextAuto},

	// ux §1.4 E3: identity state (/identity/status.message, doctor identity.witness).
	{Code: "identity.ok", Class: ClassS, Level: LevelOK,
		Zh: "{name} {aid8}，密钥已由 {hub} 确认（{ago}前）。",
		En: "{name} {aid8}, key confirmed by {hub} {ago} ago."},
	{Code: "identity.confirm_aging", Class: ClassS, Level: LevelWarn,
		Zh:       "本节点的 hub 已 {dur} 没有确认密钥；请检查与 hub 的连接。",
		En:       "This node's hub has not confirmed its key for {dur}; check the connection to the hub.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "notice_stale", Class: ClassS, Level: LevelFail,
		Zh:       "本节点的 hub 已 {dur} 没有确认密钥，发出的消息在排队；请检查与 hub 的连接，hub 长期不可用时请换 hub。",
		En:       "This node's hub has not confirmed its key for {dur}, so outgoing messages are queued; check the connection to the hub, and switch hubs if it stays down.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "old_hub_unacked", Class: ClassS, Level: LevelInfo,
		Zh: "新 hub 已生效；旧 hub {hub} 还没确认收到这次轮换，anet 会继续重试，不需要操作。",
		En: "The new hub is in use; the old hub {hub} has not confirmed this rotation yet, and anet keeps trying; nothing to do."},
	{Code: "identity.quarantine", Class: ClassS, Level: LevelWarn,
		Zh: "本节点的 hub 刚从备份恢复，正在核对身份记录；不需要操作，最多 30 分钟。",
		En: "This node's hub was just restored from a backup and is checking identity records; nothing to do, it takes at most 30 minutes."},
	{Code: "identity.hub_record_invalid", Class: ClassS, Level: LevelWarn,
		Zh: "本节点的见证 hub {hub} 给出的身份记录验证不过，本机身份不受影响；请把这条信息发给 hub 运营者，持续出现时请换 hub。",
		En: "The witness hub {hub} returned an identity record that does not verify; this node's identity is not affected. Send this message to the hub's operator, and switch hubs if it keeps happening."},
	{Code: "identity.hub_kel_fork", Class: ClassS, Level: LevelWarn,
		Zh: "有 {n} 份对等 hub 的身份记录与本机保存的不一致，已保留原记录；请把这条信息发给你的 hub 运营者。",
		En: "{n} identity records of peer hubs did not match the ones this node keeps, so the kept ones stay; send this message to your hub's operator."},
	{Code: "identity.hub_unauthorized", Class: ClassS, Level: LevelWarn,
		Zh:       "本节点的 hub 最近 {n} 次拒绝了本节点的签名请求；请运行 anet status 查看 hub 状态，持续出现时请换 hub。",
		En:       "This node's hub refused its signed requests {n} times recently; check it with anet status, and switch hubs if this goes on.",
		NextKind: NextCommand, Next: "anet status"},
	{Code: "identity.unprotected", Class: ClassS, Level: LevelWarn,
		Zh:       "身份还没有设恢复口令：数据目录一旦泄露，身份可能被抢走且无法找回。请在终端运行 anet identity protect。",
		En:       "This identity has no recovery passphrase yet: if its data directory leaks, the identity can be taken over and cannot be recovered. Run anet identity protect in a terminal.",
		NextKind: NextCommand, Next: "anet identity protect"},
	{Code: "identity.unprotected.resume", Class: ClassS, Level: LevelWarn,
		Zh:       "设置恢复口令还差最后一步：请在终端运行 anet identity protect 完成。",
		En:       "Setting the recovery passphrase is one step short: run anet identity protect in a terminal to finish.",
		NextKind: NextCommand, Next: "anet identity protect"},

	// ux §1.4 E4: local data, the clock and retention.
	{Code: "no_local_task", Class: ClassE | ClassC, A2A: A2A{Err: A2ATaskNotFound},
		Zh:       "本机没有任务 {task}：任务结束后按保存期删除（现在是 {retention}，公开调用 7 天），也可能是本机的数据比这个任务旧；留下的证据可用 anet audit --interaction {task} 查看。",
		En:       "No task {task} on this node: tasks are deleted after the retention period (now {retention}, public calls 7 days), or this node's data may be older than the task; see what the evidence log kept with anet audit --interaction {task}.",
		NextKind: NextCommand, Next: "anet audit --interaction {task}"},
	{Code: "clock.skewed", Class: ClassS, Level: LevelWarn,
		Zh: "本机时钟与 hub 相差 {dur}，为防误删已放慢删除过期密钥和内容（本机累计运行满 1 天后改按本机时钟删除），前向保密窗口因此变长；请校准系统时间（例如启用 NTP）。",
		En: "This computer's clock is {dur} off from the hub, so deleting expired keys and content has slowed to avoid early deletion (after a day of running it uses this clock), widening the forward-secrecy window; fix the system time (e.g. enable NTP)."},
	{Code: "clock.unverified", Class: ClassS, Level: LevelWarn,
		Zh:       "最近无法与 hub 核对本机时钟，为防误删已放慢删除过期密钥和内容（本机累计运行满 1 天后改按本机时钟删除）；恢复与 hub 的连接后自动恢复。",
		En:       "The clock could not be checked against the hub recently, so deleting expired keys and content has slowed to avoid early deletion (after a day of running it uses this clock); it recovers once the hub can be reached.",
		NextKind: NextCommand, Next: "anet status"},
	{Code: "clock.no_hub", Class: ClassS, Level: LevelInfo,
		Zh: "没有配置 hub，无法核对本机时钟；过期密钥与内容按本机时钟删除，请确保系统时间准确。",
		En: "No hub is configured, so the clock cannot be checked; expired keys and content are deleted by this computer's clock, so keep the system time correct."},
	{Code: "keys.kept_long", Class: ClassS, Level: LevelWarn,
		Zh:       "有 {n} 把过期加密钥已多保留 {dur}，因为 hub 信箱一直没有读完，前向保密窗口因此变长；请检查 hub 是否正常。",
		En:       "{n} expired encryption keys have been kept {dur} longer than usual because the hub mailbox never finished reading, which widens the forward-secrecy window; check that the hub is working.",
		NextKind: NextCommand, Next: "anet status"},
	{Code: "keys.unknown_drops", Class: ClassS, Level: LevelWarn,
		Zh:       "最近 24 小时有 {n} 封信因封给了已删除的密钥而无法打开；发件方会自动重发，持续出现请运行 anet doctor --json 并提交问题报告。",
		En:       "{n} messages in the last 24 hours were sealed to keys this node had already deleted; their senders resend them automatically; if this keeps happening, run anet doctor --json and file a report.",
		NextKind: NextCommand, Next: "anet doctor --json"},
	{Code: "local_state.detected", Class: ClassS, Level: LevelWarn,
		Zh:       "{time} 检测到本机数据回退，当时 {n} 个在途任务已结束；如果你没有从备份恢复或复制数据目录，说明这个身份正在别处运行、数据目录可能已泄露，请运行 {next}。",
		En:       "At {time} this node found its data had gone back to an older state and ended {n} tasks in progress; if you did not restore a backup or copy the data directory, this identity is running elsewhere and its data may have leaked: run {next}.",
		NextKind: NextCommand, Next: "anet identity rotate --emergency"},
	{Code: "local_state.repeated", Class: ClassS, Level: LevelFail,
		Zh:       "7 天内本机数据回退了不止一次，说明这个身份正在另一处运行；如果那是你自己的另一份数据目录，请停掉它，否则数据目录已泄露，请运行 {next}。",
		En:       "This node's data went back to an older state more than once in 7 days, so this identity is running somewhere else; if that is your own other copy, stop it; otherwise the data directory has leaked: run {next}.",
		NextKind: NextCommand, Next: "anet identity rotate --emergency"},
	{Code: "retention.summary", Class: ClassS, Level: LevelInfo,
		Zh: "任务内容的保存期是 {retention}，从任务结束起算（公开调用 7 天）。",
		En: "Task content retention is {retention}, counted from when a task ends (public calls: 7 days)."},
	{Code: "retention.cow", Class: ClassS, Level: LevelInfo,
		Zh: "数据目录在写时复制文件系统上，删除的数据可能仍留在快照里。",
		En: "The data directory is on a copy-on-write filesystem; deleted data may remain in snapshots."},
	{Code: "retention_invalid", Class: ClassC, Level: LevelFail,
		Zh:       "保存期应写成 1 到 3650 的天数加 d，或写 forever；例如 anet retention set 30d。",
		En:       "Write the retention as a number of days from 1 to 3650 followed by d, or forever; for example: anet retention set 30d.",
		NextKind: NextCommand, Next: "anet retention set 30d"},
	{Code: "retention.set", Class: ClassI,
		Zh: "任务内容的保存期现在是 {retention}，从任务结束起算。",
		En: "Task content retention is now {retention}, counted from when a task ends."},
	{Code: "retention.confirm", Class: ClassT,
		Zh: "改为 {retention} 会立即删除 {n} 个已结束超过 {retention} 的任务及其内容，无法恢复；输入 yes 确认：",
		En: "Setting {retention} deletes {n} tasks that ended more than {retention} ago, with their content, and this cannot be undone; type yes to confirm:"},
	{Code: "task.kept_until", Class: ClassI,
		Zh: "内容保存到 {time}（保存期 {retention}）。",
		En: "Content kept until {time} (retention {retention})."},

	// ux §1.4 F: joining a hub and admission.
	{Code: "registered", Class: ClassS, Level: LevelOK,
		Zh: "已在 {hub} 注册。",
		En: "Registered at {hub}."},
	{Code: "registered", Variant: "probation", Class: ClassS, Level: LevelInfo,
		Zh: "已在 {hub} 注册；新账户见习期到 {time}，期间不能上传评价。",
		En: "Registered at {hub}; the new account is on probation until {time} and cannot upload reviews before then."},
	{Code: "hub_pending_admission", Class: ClassE | ClassC | ClassS, Level: LevelWarn, A2A: A2A{Err: A2ATransient},
		Zh:       "这个 hub（{hub}）需要运营者先准入你的 AID；请把这一行发给运营者：anet-hub admit {aid}（批准后本节点会自动完成注册）。",
		En:       "This hub ({hub}) needs its operator to admit your AID first; send this line to the operator: anet-hub admit {aid} (once they do, this node finishes joining on its own).",
		NextKind: NextCommand, Next: "anet-hub admit {aid}"},
	{Code: "hub_retrying", Class: ClassE | ClassC | ClassS, Level: LevelWarn, A2A: A2A{Err: A2ATransient},
		Zh:       "暂时无法在 {hub} 完成注册（{why}），anet 会在 {time} 自动重试，不需要操作。",
		En:       "Could not finish joining {hub} yet ({why}); anet tries again at {time}, nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "hub_retrying", Variant: "quota", Class: ClassE | ClassC | ClassS, Level: LevelWarn, A2A: A2A{Err: A2ATransient},
		Zh:       "暂时无法在 {hub} 完成注册（你所在网段今天的注册名额已用完），anet 会在 {time} 自动重试；着急的话请 hub 运营者执行 anet-hub admit {aid}。",
		En:       "Could not finish joining {hub} yet (today's sign-ups from your network are used up); anet tries again at {time}; if it is urgent, ask the hub operator to run anet-hub admit {aid}.",
		NextKind: NextCommand, Next: "anet-hub admit {aid}"},
	{Code: "hub_refused", Class: ClassE | ClassC | ClassS, Level: LevelFail, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "{hub} 拒绝了注册（{code}），anet 不会再试；请把这条信息发给 hub 运营者。",
		En:       "{hub} refused to register this node ({code}), and anet will not retry; contact the hub's operator with this message.",
		NextKind: NextContact},
	{Code: "hub_publication_blocked", Class: ClassE | ClassC | ClassS, Level: LevelFail, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "注册资料里有组织模块标为保密的内容，本节点没有把它发给 {hub}；请从 agent 名称、能力描述或资料里去掉后重新运行 anet hub-register {hub}。",
		En:       "The registration data has something an organization module marks confidential, so it was not sent to {hub}; remove it from the agent name, capability descriptions or profile, then run anet hub-register {hub} again.",
		NextKind: NextCommand, Next: "anet hub-register {hub}"},
	{Code: "hub_unreachable", Class: ClassC,
		Zh:       "连不上 {hub}，配置没有改动；请检查地址或网络后重新运行 anet hub-register {hub}。",
		En:       "{hub} cannot be reached, and nothing was changed; check the address or the network and run anet hub-register {hub} again.",
		NextKind: NextCommand, Next: "anet hub-register {hub}"},
	{Code: "hub_unreachable", Variant: "leave", Class: ClassC,
		Zh:       "连不上 {hub}，没有离开，配置没有改动；请检查网络后重新运行 anet hub-leave。",
		En:       "{hub} cannot be reached, so this node has not left it and nothing was changed; check the network and run anet hub-leave again.",
		NextKind: NextCommand, Next: "anet hub-leave"},
	{Code: "invite_removed", Class: ClassC,
		Zh:       "hub 已不再使用邀请码；请直接运行 anet hub-register {hub}，再把它打印的 admit 命令发给 hub 运营者。",
		En:       "Hubs no longer use invites; run anet hub-register {hub} and send the admit command it prints to the hub's operator.",
		NextKind: NextCommand, Next: "anet hub-register {hub}"},
	{Code: "wire_too_old", Class: ClassE | ClassS, Level: LevelFail, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "本节点的 anet 比 {hub} 要求的旧（需要 {version}）；请运行 anet update。",
		En:       "This node's anet is older than {hub} needs ({version}); run anet update.",
		NextKind: NextCommand, Next: "anet update"},
	{Code: "wire_too_new", Class: ClassE | ClassS, Level: LevelFail, A2A: A2A{Err: A2AFailedPrecondition},
		Zh:       "{hub} 的版本比本节点旧，双方不能通信；请联系 hub 运营者升级，或换一个 hub。",
		En:       "{hub} runs an older version than this node, so they cannot talk; ask the hub's operator to upgrade, or use another hub.",
		NextKind: NextContact},
	{Code: "probation", Class: ClassC,
		Zh:       "新账户见习期（到 {time}）内不能上传评价；anet 会在期满后自动上传，不需要操作。",
		En:       "New accounts cannot upload reviews during probation (until {time}); anet uploads it afterwards on its own, nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "review_exists", Class: ClassC,
		Zh:       "这次交互已经评价过了；每次交互只能评价一次，重试无用。",
		En:       "This interaction has already been reviewed; each can be reviewed once, so retrying will not help.",
		NextKind: NextFutile},
	{Code: "requester_commit_mismatch", Class: ClassC,
		Zh:       "只有这次交互的请求方能评价它；重试无用。",
		En:       "Only this interaction's requester can review it; retrying will not help.",
		NextKind: NextFutile},
	{Code: "receipt_not_reviewable", Class: ClassC,
		Zh:       "这张回执不能用于评价：它缺少评价承诺，或签名与评价对不上；只能评价用 anet 0.3.0 发起的任务，重试无用。",
		En:       "This receipt cannot be used for a review: it has no review commitment, or its signatures do not match the review; only tasks started with anet 0.3.0 can be reviewed, and retrying will not help.",
		NextKind: NextFutile},
	{Code: "review_window_closed", Class: ClassC,
		Zh:       "交互结束已超过 30 天，不能再评价；重试无用。",
		En:       "The interaction ended more than 30 days ago and can no longer be reviewed; retrying will not help.",
		NextKind: NextFutile},
	{Code: "review.queued", Class: ClassI,
		Zh: "评价已签好，将在 6 小时内的随机时刻自动上传（让别人更难按时间对应到某次交互）。",
		En: "The review is signed and will be uploaded at a random time within 6 hours, to make it harder to match it to an interaction by timing."},

	// ux §1.4 G: refused at once by this node; nothing was sent.
	{Code: "message_id_reused", Variant: "open", Class: ClassE | ClassC, A2A: A2A{Err: A2AInvalidParams},
		Zh:       "这个 messageId 已用于进行中的任务 {task}，内容不同，本次没有发送；如果是重试，请等待任务 {task}；如果是新请求，请换一个新的随机 messageId。",
		En:       "This messageId is already used by the open task {task} with different content, so nothing was sent; if this is a retry, wait for task {task}; if it is a new request, use a new random messageId.",
		NextKind: NextChangeInput},
	{Code: "message_id_reused", Variant: "followup", Class: ClassE | ClassC, A2A: A2A{Err: A2AInvalidParams},
		Zh:       "这个 messageId 已用于任务 {task} 上内容不同的一条消息，本次没有发送；新消息请换一个新的随机 messageId。",
		En:       "This messageId is already used by a different message on task {task}, so nothing was sent; use a new random messageId for a new message.",
		NextKind: NextChangeInput},
	{Code: "args_reserved_key", Class: ClassE | ClassC, A2A: A2A{Err: A2AInvalidParams},
		Zh:       "参数里以 $ 开头的键是 anet 保留的，本次没有发送；请改名后重发。",
		En:       "Keys starting with $ are reserved by anet, so nothing was sent; rename them and send again.",
		NextKind: NextChangeInput},
	{Code: "preview_off", Class: ClassC,
		Zh:       "本节点的预览开关是关的，没有发送；需要时运行 anet preview on。",
		En:       "Previews are off on this node, so nothing was sent; run anet preview on if you want them.",
		NextKind: NextCommand, Next: "anet preview on"},
	{Code: "task_not_open", Class: ClassC,
		Zh:       "这个任务已结束，或不是发给本节点的文本任务，预览没有发送；不需要操作。",
		En:       "This task has ended or is not a text task sent to this node, so the preview was not sent; nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "not_requested", Class: ClassC,
		Zh:       "请求方这一轮没有要预览，这一帧没有发送；不需要操作。",
		En:       "The requester did not ask for a preview this turn, so this frame was not sent; nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "extended_card_not_offered", Class: ClassE, A2A: A2A{Err: A2AUnsupportedOperation},
		Zh:       "对你来说这个 agent 只有公开卡片，不需要操作；只对名单开放的能力，要在你与它第一次往来之后才会显示，已知能力名时可以直接调用。",
		En:       "For you this agent has only its public card, nothing to do; skills open only to its lists show up after your first exchange with it, and you can call one directly if you know its name.",
		NextKind: NextNoneNeeded},
	{Code: "extended_card_not_configured", Class: ClassE, A2A: A2A{Err: A2AExtendedCardNotConfigured},
		Zh:       "这个 agent 给你的扩展卡片与公开卡片相同（没有价格，也没有多出的能力），不需要操作。",
		En:       "This agent's extended card for you is the same as its public card (no prices and no extra skills), nothing to do.",
		NextKind: NextNoneNeeded},
	{Code: "extended_card_unavailable", Class: ClassE, A2A: A2A{Err: A2ATransient},
		Zh:       "暂时取不到这个 agent 的扩展卡片（对方没有回应或太忙），下面是它的公开卡片；请稍后再试。",
		En:       "This agent's extended card cannot be fetched right now (no answer, or it is busy), so its public card is shown; try again later.",
		NextKind: NextRetryLater},
	{Code: "extended_card_unavailable", Variant: "bad_signature", Class: ClassE, A2A: A2A{Err: A2AInvalidAgentResponse},
		Zh:       "这个 agent 的扩展卡片签名无效，已丢弃，下面是它的公开卡片；请联系对方。",
		En:       "This agent's extended card has an invalid signature and was discarded, so its public card is shown; contact them.",
		NextKind: NextContact},
	{Code: "daemon_not_running", Class: ClassC,
		Zh:       "anet 没有在运行（数据目录 {path}）；请运行 anet up。",
		En:       "anet is not running (data directory {path}); run anet up.",
		NextKind: NextCommand, Next: "anet up"},
	{Code: "daemon_not_answering", Class: ClassC,
		Zh:       "anet 正在运行（数据目录 {path}），但 {dur} 内没有应答，什么都没有改；请稍后再试。",
		En:       "anet is running (data directory {path}) but did not answer within {dur}, so nothing was changed; try again later.",
		NextKind: NextRetryLater},
	{Code: "local_write_failed", Class: ClassC,
		Zh:       "本节点没能把这次改动写进数据目录（{error}）；请检查磁盘空间与数据目录的权限后重试。",
		En:       "This node could not write the change to its data directory ({error}); check the disk space and the data directory's permissions and try again.",
		NextKind: NextChangeInput},
	{Code: "aid_invalid", Class: ClassC,
		Zh:       "{value} 不是完整的 AID；请从 anet find 或 anet peers list 的 AID 列复制完整 AID 后重试。",
		En:       "{value} is not a full AID; copy the full AID from the AID column of anet find or anet peers list and try again.",
		NextKind: NextCommand, Next: "anet find"},
	{Code: "cli_removed", Class: ClassC,
		Zh:       "{what} 已在 0.3.0 删除；{instead}。",
		En:       "{what} was removed in 0.3.0; {instead}.",
		NextKind: NextChangeInput},
	{Code: "config_key_removed", Class: ClassC, Level: LevelFail,
		Zh:       "配置项 {key} 在 0.3.0 已删除；{instead}。",
		En:       "The setting {key} was removed in 0.3.0; {instead}.",
		NextKind: NextChangeInput},
	{Code: "config_invalid", Class: ClassC, Level: LevelFail,
		Zh:       "配置项 {key} 的值不合法：{rule}；请改正后重试。",
		En:       "The setting {key} is not valid: {rule}; fix it and try again.",
		NextKind: NextChangeInput},
	{Code: "module_absent", Class: ClassC | ClassE, A2A: A2A{Err: A2AUnsupportedOperation},
		Zh:       "{why}，什么都没有做；{fix}。",
		En:       "{why}, so nothing was done; {fix}.",
		NextKind: NextChangeInput},
	{Code: "module_absent", Variant: "configured", Class: ClassC | ClassS, Level: LevelFail,
		Zh:       "配置里有 {key}，但{why}，anet 没有启动；请删掉 {key}，或{fix}。",
		En:       "The configuration has {key}, but {why}, so anet did not start; remove {key}, or {fix}.",
		NextKind: NextChangeInput},
	{Code: "module_absent", Variant: "payment", Class: ClassIP,
		Zh:       "这个任务要付 {amount} credits，但{why}，什么都没有签；要付款请{fix}后运行 anet pay {task}，不需要了就运行 anet task cancel {task}。",
		En:       "This task asks for {amount} credits, but {why}, so nothing was signed; to pay, {fix} and run anet pay {task}; otherwise run anet task cancel {task}.",
		NextKind: NextCommand, Next: "anet task cancel {task}"},
	{Code: "daemon_running", Class: ClassC,
		Zh:       "anet 正在运行（数据目录 {path}），这项改动要在它停止后做，什么都没有改；请先运行 anet down，再重试。",
		En:       "anet is running (data directory {path}); this change can only be made while it is stopped, so nothing was changed; run anet down first, then try again.",
		NextKind: NextCommand, Next: "anet down"},
	{Code: "without_configured", Class: ClassC,
		Zh:       "配置里有 {key}，先删掉它再关闭{name}模块（{m}）；什么都没有改。",
		En:       "The configuration has {key}; remove it before turning off the {name} module ({m}). Nothing was changed.",
		NextKind: NextChangeInput},
	{Code: "update_variant_unknown", Class: ClassC,
		Zh:       "这个 anet 是自建的，anet update 只更新发布版本，什么都没有下载；请按原来的方式重新构建，或用 install.sh 改装发布版本。",
		En:       "This anet was built locally, and anet update only updates released builds, so nothing was downloaded; rebuild it the way you built it, or install a released build with install.sh.",
		NextKind: NextChangeInput},
	{Code: "update_variant_unknown", Variant: "mismatch", Class: ClassC,
		Zh:       "这个 anet 标为 {variant} 版本，但它的模块组合（{modules}）与发布的 {variant} 版本不同，anet update 不会换成别的组合，什么都没有下载；请用 install.sh 改装发布版本。",
		En:       "This anet is marked as the {variant} build, but its set of modules ({modules}) differs from the released {variant} build, and anet update will not swap it for a different set, so nothing was downloaded; install a released build with install.sh.",
		NextKind: NextChangeInput},
	{Code: "build_tag_pending", Class: ClassC,
		Zh:       "这个版本还不能在构建时去掉{name}模块（{m}），随 v0.3.x 提供；现在可以用 --without {m} 在配置里关闭它，什么都没有构建。",
		En:       "This version cannot leave the {name} module ({m}) out at build time yet; that comes in v0.3.x. For now you can turn it off in the configuration with --without {m}; nothing was built.",
		NextKind: NextChangeInput},
	{Code: "api_key_on_argv", Class: ClassC,
		Zh:       "API key 不接受命令行参数；这把 key 已出现在命令行里（本机其他用户与 shell 历史可能看到），没有保存；请到服务商更换它、清理 shell 历史，再改用 {next} 读入新 key。",
		En:       "API keys are not taken on the command line; this one has appeared there (local users and your shell history may have it) and was not saved; replace it with the provider, clear your shell history, then use {next}.",
		NextKind: NextCommand, Next: "anet autoreply set --api-key-stdin"},
	{Code: "api_key_empty", Class: ClassC,
		Zh:       "读到的 API key 是空的；请检查文件内容后重试。",
		En:       "The API key read was empty; check the file and try again.",
		NextKind: NextChangeInput},
	{Code: "egress_allow_invalid", Class: ClassC,
		Zh:       "egress_allow 只接受完整主机名（例如 pypi.org），{value} {why}；请改正后重试。",
		En:       "egress_allow takes whole host names only (such as pypi.org), and {value} {why}; fix it and try again.",
		NextKind: NextChangeInput},
	{Code: "egress_allow_model_host", Class: ClassC,
		Zh:       "{value} 是模型服务主机，不能放进 egress_allow（否则沙箱里的 agent 能拿自己的 key 直连它）；请删掉这一项，模型请求本来就由本节点代发。",
		En:       "{value} is a model service host and cannot be in egress_allow (the sandboxed agent could reach it with a key of its own); remove it from egress_allow, since model requests already go through this node.",
		NextKind: NextChangeInput},
	{Code: "egress_allow_too_many", Class: ClassC,
		Zh:       "egress_allow 最多 64 个主机；请删减后重试。",
		En:       "egress_allow takes at most 64 hosts; remove some and try again.",
		NextKind: NextChangeInput},
	{Code: "api_base_insecure", Class: ClassC,
		Zh:       "--api-base 用 http 时只能指向本机回环地址（127.0.0.1、::1、localhost）；其他地址请改用 https。",
		En:       "An http --api-base must point at this machine's loopback (127.0.0.1, ::1, localhost); use https for anything else.",
		NextKind: NextChangeInput},
	{Code: "work_dir_invalid", Class: ClassC,
		Zh:       "auto_reply.work_dir 不能位于数据目录或沙箱工作目录之内，也不能包含它们；请换一个目录。",
		En:       "auto_reply.work_dir cannot be inside the data directory or the sandbox work directory, or contain either of them; use another directory.",
		NextKind: NextChangeInput},
	{Code: "budget_invalid", Class: ClassC,
		Zh:       "预算必须是 0 到 1000 的整数，且每个对端的上限不能大于全局上限；请改正后重试。",
		En:       "Budgets must be whole numbers from 0 to 1000, and the per-peer one cannot exceed the global one; fix them and try again.",
		NextKind: NextChangeInput},

	// ux §1.4 H: the local sandbox.
	{Code: "sandbox.ready", Class: ClassS, Level: LevelOK,
		Zh: "未信任对端的任务在沙箱中运行：只能经本节点访问模型，不能联网。",
		En: "Tasks from untrusted peers run in the sandbox: they reach models only through this node and have no network."},
	{Code: "sandbox.ready_egress", Class: ClassS, Level: LevelOK,
		Zh: "未信任对端的任务在沙箱中运行：只能经本节点访问模型；可联网访问 {n} 个主机（{hosts}），每个都可能被用来外发数据。",
		En: "Tasks from untrusted peers run in the sandbox: they reach models only through this node, and may reach {n} hosts ({hosts}), each of which could be used to send data out."},
	{Code: "sandbox.off", Class: ClassS, Level: LevelInfo,
		Zh: "不为未信任对端运行本机 agent（untrusted=off，或预算设为 0）。",
		En: "Local agents are not run for untrusted peers (untrusted=off, or a budget of 0)."},
	{Code: "sandbox.trusted_direct", Class: ClassS, Level: LevelInfo,
		Zh: "信任的对端直接以你的用户身份运行本机 agent，等同授予你的用户权限。",
		En: "Trusted peers run local agents directly as your user, which is the same as giving them your user's rights."},
	{Code: "sandbox.trusted_sandbox", Class: ClassS, Level: LevelInfo,
		Zh: "信任的对端也在沙箱中运行本机 agent，不计入预算。",
		En: "Trusted peers also run local agents in the sandbox, outside the run budget."},
	{Code: "no_bwrap", Class: ClassS, Level: LevelFail,
		Zh: "本机没有 bubblewrap，沙箱不可用；请安装 bubblewrap 包后重试。",
		En: "bubblewrap is not installed, so the sandbox is unavailable; install the bubblewrap package and try again."},
	{Code: "userns_blocked", Class: ClassS, Level: LevelFail,
		Zh:       "Ubuntu 的 AppArmor 默认禁止 bubblewrap 创建用户命名空间，沙箱不可用；请执行：{next}",
		En:       "Ubuntu's AppArmor stops bubblewrap from creating user namespaces by default, so the sandbox is unavailable; run: {next}",
		NextKind: NextCommand, Next: "anet doctor --apparmor-profile | sudo tee /etc/apparmor.d/anet-bwrap >/dev/null && sudo apparmor_parser -r /etc/apparmor.d/anet-bwrap"},
	{Code: "userns_unavailable", Class: ClassS, Level: LevelFail,
		Zh:       "本机的内核或容器不允许创建用户命名空间，沙箱不可用；请换到允许的主机，或设 --untrusted off。",
		En:       "This kernel or container does not allow user namespaces, so the sandbox is unavailable; use a host that does, or set --untrusted off.",
		NextKind: NextCommand, Next: "anet autoreply set --untrusted off"},
	{Code: "netns_failed", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱自检失败（网络隔离）；请运行 anet doctor 查看详情。",
		En:       "The sandbox self-check failed (network isolation); run anet doctor for details.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "seccomp_failed", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱自检失败（系统调用过滤）；请运行 anet doctor 查看详情。",
		En:       "The sandbox self-check failed (system call filter); run anet doctor for details.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "fs_isolation_failed", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱自检失败（文件与进程隔离）；请运行 anet doctor 查看详情。",
		En:       "The sandbox self-check failed (file and process isolation); run anet doctor for details.",
		NextKind: NextCommand, Next: "anet doctor"},
	{Code: "netns_failed", Variant: "doctor", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱自检失败（网络隔离：{detail}），沙箱不可用，未信任对端的任务不会运行；请运行 anet doctor --json 并提交问题报告，或设 --untrusted off。",
		En:       "The sandbox self-check failed (network isolation: {detail}), so the sandbox is unavailable and tasks from untrusted peers will not run; run anet doctor --json and file a report, or set --untrusted off.",
		NextKind: NextCommand, Next: "anet doctor --json"},
	{Code: "seccomp_failed", Variant: "doctor", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱自检失败（系统调用过滤：{detail}），沙箱不可用，未信任对端的任务不会运行；请运行 anet doctor --json 并提交问题报告，或设 --untrusted off。",
		En:       "The sandbox self-check failed (system call filter: {detail}), so the sandbox is unavailable and tasks from untrusted peers will not run; run anet doctor --json and file a report, or set --untrusted off.",
		NextKind: NextCommand, Next: "anet doctor --json"},
	{Code: "fs_isolation_failed", Variant: "doctor", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱自检失败（文件与进程隔离：{detail}），沙箱不可用，未信任对端的任务不会运行；请运行 anet doctor --json 并提交问题报告，或设 --untrusted off。",
		En:       "The sandbox self-check failed (file and process isolation: {detail}), so the sandbox is unavailable and tasks from untrusted peers will not run; run anet doctor --json and file a report, or set --untrusted off.",
		NextKind: NextCommand, Next: "anet doctor --json"},
	{Code: "sandbox_start_failed", Class: ClassS, Level: LevelFail,
		Zh:       "bubblewrap 没能建立沙箱（{error}），沙箱不可用；请运行 anet doctor --json 并提交问题报告，或设 --untrusted off。",
		En:       "bubblewrap could not set up the sandbox ({error}), so the sandbox is unavailable; run anet doctor --json and file a report, or set --untrusted off.",
		NextKind: NextCommand, Next: "anet doctor --json"},
	{Code: "agent_unsupported", Class: ClassS, Level: LevelFail,
		Zh: "{name} 不能在沙箱里运行（它不能把模型请求交给本节点代发）；请换用 {agents}，或设 --untrusted off。",
		En: "{name} cannot run in the sandbox (it cannot send its model requests through this node); use {agents} instead, or set --untrusted off."},
	{Code: "agent_unsupported", Variant: "combo", Class: ClassS, Level: LevelFail,
		Zh: "{name} 在沙箱里不能用这个模型服务地址（{why}）；请改或去掉 --api-base，或设 --untrusted off。",
		En: "{name} cannot use this model service address in the sandbox ({why}); change or remove --api-base, or set --untrusted off."},
	{Code: "no_api_key", Class: ClassS, Level: LevelFail,
		Zh:       "沙箱里的 agent 由本节点代为调用模型，需要 API key；请执行 anet autoreply set --api-key-stdin < 存放 key 的文件。",
		En:       "In the sandbox this node calls the model for the agent and needs an API key; run anet autoreply set --api-key-stdin < file-with-the-key.",
		NextKind: NextCommand, Next: "anet autoreply set --api-key-stdin"},
	{Code: "no_api_base", Class: ClassS, Level: LevelFail,
		Zh: "{name} 需要模型服务地址；请执行 anet autoreply set --api-base <URL>。",
		En: "{name} needs a model service address; run anet autoreply set --api-base <URL>."},
	{Code: "no_model", Class: ClassS, Level: LevelFail,
		Zh: "自定义模型服务需要指定模型；请执行 anet autoreply set --model <名称>。",
		En: "A custom model service needs a model name; run anet autoreply set --model <name>."},
	{Code: "platform_unsupported", Class: ClassS, Level: LevelFail,
		Zh: "本平台没有沙箱（只支持 Linux 与 macOS）；Windows 请在 WSL2 中运行 anet。",
		En: "There is no sandbox on this platform (only Linux and macOS); on Windows, run anet in WSL2."},
	{Code: "platform_unsupported.pending", Class: ClassS, Level: LevelFail,
		Zh:       "macOS 沙箱随 v0.3.x 提供，本版本在 macOS 上没有沙箱，未信任对端的任务不会运行；可设 --untrusted off，或在 Linux 上运行 anet。",
		En:       "The macOS sandbox comes in v0.3.x; this version has no sandbox on macOS, so tasks from untrusted peers will not run; set --untrusted off, or run anet on Linux.",
		NextKind: NextCommand, Next: "anet autoreply set --untrusted off"},
	{Code: "no_sandbox_exec", Class: ClassS, Level: LevelFail,
		Zh:       "本机找不到 /usr/bin/sandbox-exec，沙箱不可用；请设 --untrusted off。",
		En:       "/usr/bin/sandbox-exec is missing, so the sandbox is unavailable; set --untrusted off.",
		NextKind: NextCommand, Next: "anet autoreply set --untrusted off"},
	{Code: "no_cgroup_limits", Class: ClassS, Level: LevelWarn,
		Zh: "沙箱没有内存与进程数上限（本机没有用户级 systemd），其余限制照常生效；要启用请开启用户级 systemd（loginctl enable-linger）。",
		En: "The sandbox has no memory or process limits (no user systemd here); the other limits still apply; to add them, enable user systemd (loginctl enable-linger)."},
	{Code: "sandbox.budget", Class: ClassS, Level: LevelOK,
		Zh: "今天已为未信任对端运行 {n}/{daily} 次（每个对端至多 {per_peer} 次）。",
		En: "{n}/{daily} runs for untrusted peers today (at most {per_peer} per peer)."},

	// ux §1.4 I: egress refusals the agent in the sandbox reads, never shown to the user. Both languages
	// begin with the literal prefix "anet: " (LEC §2.6.4: the message begins "anet: "), half-width.
	{Code: "egress_host_not_allowed", Class: ClassModel,
		Zh: "anet: 本节点没有为这类任务放行 {host}；需要联网才能完成时，请在回复里说明做不到。",
		En: "anet: this node has not allowed {host} for this kind of task; if the task needs it, say in your reply that you cannot do it."},
	{Code: "egress_port", Class: ClassModel,
		Zh: "anet: 只允许连接 443 端口。",
		En: "anet: only port 443 is allowed."},
	{Code: "egress_private_address", Class: ClassModel,
		Zh: "anet: {host} 解析到内网或保留地址，已拒绝。",
		En: "anet: {host} resolves to a private or reserved address, so it was refused."},
	{Code: "egress_plain_http", Class: ClassModel,
		Zh: "anet: 只能经 CONNECT 访问放行的 https 主机，或调用本节点的模型路由。",
		En: "anet: only CONNECT to an allowed https host, or this node's model route, is available."},
	{Code: "egress_unauthorized", Class: ClassModel,
		Zh: "anet: 缺少本次运行的凭据。",
		En: "anet: this run's credential is missing."},
	{Code: "egress_tunnels", Class: ClassModel,
		Zh: "anet: 同时打开的连接太多；请关掉一些后重试。",
		En: "anet: too many connections are open at once; close some and try again."},
	{Code: "egress_too_large", Class: ClassModel,
		Zh: "anet: 超过本次运行的大小上限（{n} MiB）。",
		En: "anet: this is over the run's size limit ({n} MiB)."},
	{Code: "egress_upstream_unreachable", Class: ClassModel,
		Zh: "anet: 本节点暂时连不上模型服务；请稍后再试，或根据已有结果作答。",
		En: "anet: this node cannot reach the model service right now; try again later, or answer with what you have."},
	{Code: "egress_host_unresolved", Class: ClassModel,
		Zh: "anet: 放行的主机 {host} 现在解析不到地址；请稍后再试，或在回复里说明做不到。",
		En: "anet: the allowed host {host} does not resolve right now; try again later, or say in your reply that you cannot do it."},
	{Code: "policy_server_tool", Class: ClassModel,
		Zh: "anet: 沙箱里不允许模型服务端工具 {type}（它会绕过本节点的网络限制）；请改用本机工具。",
		En: "anet: the server-side model tool {type} is not allowed in the sandbox (it would bypass this node's network limits); use a local tool instead."},
	{Code: "policy_mcp_servers", Class: ClassModel,
		Zh: "anet: 沙箱里不允许 MCP 连接器（mcp_servers）。",
		En: "anet: MCP connectors (mcp_servers) are not allowed in the sandbox."},
	{Code: "policy_field", Class: ClassModel,
		Zh: "anet: 沙箱里不允许请求字段 {field}。",
		En: "anet: the request field {field} is not allowed in the sandbox."},
	{Code: "policy_path", Class: ClassModel,
		Zh: "anet: 沙箱里只允许调用 {paths}。",
		En: "anet: only {paths} can be called from the sandbox."},
	{Code: "policy_encoding", Class: ClassModel,
		Zh: "anet: 请求体必须是未压缩的 JSON 对象。",
		En: "anet: the request body must be an uncompressed JSON object."},
	{Code: "run_budget_requests", Class: ClassModel,
		Zh: "anet: 本次运行的模型请求已达上限 {n} 次；请根据已有结果作答。",
		En: "anet: this run has used its {n} model requests; answer with what you have."},
	{Code: "run_budget_upload", Class: ClassModel,
		Zh: "anet: 本次运行向外发送的数据已达上限 {n} MiB；请根据已有结果作答。",
		En: "anet: this run has sent its {n} MiB of outgoing data; answer with what you have."},
	{Code: "egress_sni_mismatch", Class: ClassModel,
		NoSentence: true},
	{Code: "egress_ech", Class: ClassModel,
		NoSentence: true},
	{Code: "egress_not_tls", Class: ClassModel,
		NoSentence: true},

	// ux §1.4 J: local backends and C1, for this node's operator.
	{Code: "peer_cred_unsupported", Class: ClassS, Level: LevelFail,
		Zh: "本平台读不到 {path} 对端进程的用户身份，已拒绝连接；若确认只有你能改动这条路径，可在这个后端配置 \"peer_check\": \"path-only\"。",
		En: "This platform cannot read who is on the other end of {path}, so the connection was refused; if only you can change that path, set \"peer_check\": \"path-only\" for this backend."},
	{Code: "peer_uid_mismatch", Class: ClassS, Level: LevelFail,
		Zh: "{path} 的监听进程以 uid {n} 运行，不是预期的 {name}；请检查是谁在监听，或设置 expected_user。",
		En: "The process listening on {path} runs as uid {n}, not the expected {name}; check who is listening, or set expected_user."},
	{Code: "c1_version", Class: ClassS, Level: LevelFail,
		Zh: "ANetLink 与 anet 的 C1 版本不一致；请把两者升级到同一发布。",
		En: "ANetLink and anet speak different C1 versions; upgrade both to the same release."},
	{Code: "c1_uid_refused", Class: ClassS, Level: LevelInfo,
		Zh: "拒绝 uid {n} 连接 c1.sock（不在 allow_users 中）；如需允许，在 ANetLink 配置 c1.allow_users 加入该用户。",
		En: "Refused uid {n} on c1.sock (not in allow_users); to allow it, add the user to c1.allow_users in the ANetLink config."},

	// ux §1.4 K: audit findings (anet audit-hub, doctor payments.audit).
	{Code: "balance_mismatch", Class: ClassS, Level: LevelFail,
		Zh: "hub 在 {time} 报告你的余额是 {amount}，按上一纪元与账目应为 {need}；请运行 anet audit-hub --json > proof.json 保存证据，并联系 hub 运营者。",
		En: "On {time} the hub reported your balance as {amount}, but the previous period and the ledger say {need}; save the evidence with anet audit-hub --json > proof.json and contact the hub's operator."},
	{Code: "omitted_from_ledger", Class: ClassS, Level: LevelFail,
		Zh: "hub 签过收据的一笔 {amount} credits 不在账目里；请保存 anet audit-hub --json 的输出，并联系 hub 运营者。",
		En: "A {amount}-credit entry the hub signed a receipt for is missing from the ledger; save the output of anet audit-hub --json and contact the hub's operator."},
	{Code: "unknown_debit", Class: ClassS, Level: LevelFail,
		Zh: "账目里有一笔 {amount} credits 的扣款，本节点没有签过对应的授权；请保存证据，并联系 hub 运营者。",
		En: "The ledger shows a {amount}-credit debit this node never authorized; save the evidence and contact the hub's operator."},
	{Code: "proof_invalid", Class: ClassS, Level: LevelFail,
		Zh: "hub 给的余额证明与它公开签名的记录不符（{code}）；请保存证据，并考虑把余额兑付回 hub 或更换 hub。",
		En: "The hub's balance proof does not match the record it signed publicly ({code}); save the evidence, and consider redeeming your balance or switching hubs."},
	{Code: "proof_unavailable", Class: ClassS, Level: LevelFail,
		Zh: "hub 拒绝给出 {time} 的余额证明，而你当时已注册；请联系 hub 运营者。",
		En: "The hub refused to give a balance proof for {time}, when you were registered; contact the hub's operator."},
	{Code: "proof_expired", Class: ClassS, Level: LevelWarn,
		Zh: "有 {n} 个纪元没来得及核对（hub 只保留 14 天）；已从 {time} 起重新核对，保持 daemon 运行即可避免。",
		En: "{n} periods could not be checked in time (the hub keeps 14 days); checking resumed from {time}; keeping the daemon running avoids this."},

	// ux §1.4 M1: terminal confirmations and common lines.
	{Code: "confirm.yes", Class: ClassT,
		Zh: "输入 yes 确认：",
		En: "Type yes to confirm:"},
	{Code: "confirm.declined", Class: ClassI,
		Zh: "没有确认，什么都没有改。",
		En: "Not confirmed; nothing changed."},
	{Code: "up.started", Class: ClassI,
		Zh: "anet {version} 已在后台运行：身份 {name} {aid8}（数据目录 {path}）。",
		En: "anet {version} is running in the background: identity {name} {aid8} (data directory {path})."},
	{Code: "allow_confirm", Class: ClassT,
		Zh: "把 {name}（{peer}）加入 allow 名单：它可以给本节点发任务。",
		En: "Add {name} ({peer}) to the allow list: it can send tasks to this node."},
	{Code: "approve_confirm", Class: ClassT,
		Zh: "批准待批任务 {task}（来自 {name}，{peer}）：批准后它会在本节点执行。",
		En: "Approve the held task {task} (from {name}, {peer}): it will then run on this node."},
	{Code: "inbound.confirm_approve", Class: ClassT,
		Zh: "放宽入站策略为 approve：名单外对端的任务先进待批队列，由你批准后执行。",
		En: "Loosen the inbound policy to approve: tasks from peers not on your lists wait in the approval queue until you approve them."},
	{Code: "inbound.confirm_open", Class: ClassT,
		Zh: "放宽入站策略为 open：任何人都能给你发任务。",
		En: "Loosen the inbound policy to open: anyone can send you tasks."},
	{Code: "limits_confirm", Class: ClassT,
		Zh: "修改本节点的付款额度：",
		En: "Change this node's payment limits:"},
	{Code: "limits_confirm", Variant: "line", Class: ClassT,
		Zh: "{key}  {from} → {to}",
		En: "{key}  {from} → {to}"},
	{Code: "payee_confirm", Class: ClassT,
		Zh: "把 {name}（{peer}）加入付款名单：自动付款（单笔 ≤{auto_max}）与 agent 付款（单笔 ≤{agent_max}，每日 ≤{daily}）可以付给它。",
		En: "Add {name} ({peer}) to the payee list: automatic payments (≤{auto_max} each) and agent payments (≤{agent_max} each, ≤{daily} a day) can go to it."},
	{Code: "redeem_confirm", Class: ClassT,
		Zh: "把 {amount} credits 兑付回 hub {hub}（人工付款上限：单笔 ≤{single}，今日剩 {left}）。",
		En: "Redeem {amount} credits back to the hub {hub} (manual limit: ≤{single} per payment, {left} left today)."},
	{Code: "preview.confirm_reasoning", Class: ClassT,
		Zh: "开启推理预览：要预览的请求方也会收到推理内容。",
		En: "Turn on reasoning previews: requesters who ask for previews also get the reasoning."},

	// ux §1.4 M2: identity prompts and lines.
	{Code: "identity.prompt.first_join", Class: ClassT,
		Zh: "首次接入 hub：请设置恢复口令。以后轮换密钥、换 hub、在别的机器上恢复身份都要用它；anet 不保存口令，忘了无法找回。",
		En: "First time joining a hub: set a recovery passphrase. You will need it to rotate keys, switch hubs or recover this identity on another machine; anet does not store it, and it cannot be recovered if forgotten."},
	{Code: "identity.prompt.passphrase", Class: ClassT,
		Zh: "恢复口令：",
		En: "Recovery passphrase:"},
	{Code: "identity.prompt.new_passphrase", Class: ClassT,
		Zh: "新的恢复口令：",
		En: "New recovery passphrase:"},
	{Code: "identity.prompt.again", Class: ClassT,
		Zh: "再输一次：",
		En: "Type it again:"},
	{Code: "identity.prompt.mismatch", Class: ClassT,
		Zh: "两次输入不一致，请重新输入。",
		En: "The two entries differ; enter it again."},
	{Code: "identity.recovery_written", Class: ClassI,
		Zh: "已写入恢复文件 {path}（口令加密）。请另存一份到别处。",
		En: "Recovery file written to {path} (passphrase-encrypted). Keep a copy somewhere else."},
	{Code: "identity.recovery_file_note", Class: ClassI,
		Zh: "这份文件加上你的口令，可以轮换、恢复或注销这个身份；单独这份文件无法使用。请另存一份到别处。",
		En: "With your passphrase, this file can rotate, recover or deactivate this identity; on its own it is useless. Keep a copy somewhere else."},
	{Code: "identity.recovery_file_note_unprotected", Class: ClassI,
		Zh: "这份文件与同目录的 recovery_local.key 一起就能轮换或接管这个身份；请尽快在终端运行 anet identity protect 设置口令。",
		En: "Together with recovery_local.key in the same directory, this file can rotate or take over this identity; run anet identity protect in a terminal soon to set a passphrase."},
	{Code: "identity.protect.confirm", Class: ClassT,
		Zh: "为 {name}（{aid8}）设置恢复口令：以后轮换密钥、换 hub、在别的机器上恢复身份都要用它；anet 不保存口令，忘了无法找回。设好后，数据目录里未设口令的恢复文件会作废并删除。",
		En: "Set a recovery passphrase for {name} ({aid8}): it is needed to rotate keys, switch hubs or recover this identity; anet does not store it and it cannot be recovered. The unprotected recovery file in the data directory is then voided and deleted."},
	{Code: "identity.protect.resume", Class: ClassT,
		Zh: "上次设置恢复口令只完成了一半；请输入刚才设的恢复口令，完成最后一次轮换。",
		En: "Setting the recovery passphrase stopped halfway last time; enter the passphrase you set then to finish the last rotation."},
	{Code: "identity.protect.done", Class: ClassI,
		Zh: "已设置恢复口令：密钥序号 {from} → {to}，{hub} 已确认；数据目录里未设口令的恢复文件已删除，此前复制走的那份不能再轮换或接管这个身份。",
		En: "Recovery passphrase set: key number {from} → {to}, confirmed by {hub}; the unprotected recovery file in the data directory is deleted, and a copy taken earlier can no longer rotate or take over this identity."},
	{Code: "identity.protect.done_unjoined", Class: ClassI,
		Zh: "已设置恢复口令；数据目录里未设口令的恢复文件已删除，接入 hub 时会用新的恢复文件。",
		En: "Recovery passphrase set; the unprotected recovery file in the data directory is deleted, and joining the hub will use the new one."},
	{Code: "identity.protect.already", Class: ClassI,
		Zh: "这个身份已经设置了恢复口令，不需要操作；要更换口令请运行 anet identity rotate --new-recovery。",
		En: "This identity already has a recovery passphrase, nothing to do; to change it, run anet identity rotate --new-recovery."},
	{Code: "identity.status.key", Class: ClassI,
		Zh: "序号 {n}，上次轮换 {time}（{how}）",
		En: "number {n}, last rotated {time} ({how})"},
	{Code: "identity.status.key_never", Class: ClassI,
		Zh: "序号 0，还没有轮换过",
		En: "number 0, never rotated"},
	{Code: "identity.status.witness", Class: ClassI,
		Zh: "{hub}，密钥已确认（{ago}前）",
		En: "{hub}, key confirmed {ago} ago"},
	{Code: "identity.status.recovery", Class: ClassI,
		Zh: "{path}（口令加密）",
		En: "{path} (passphrase-encrypted)"},
	{Code: "identity.status.recovery_elsewhere", Class: ClassI,
		Zh: "不在缺省位置 {path}；若你已另存一份，不需要处理",
		En: "not at the default place {path}; if you saved a copy elsewhere, nothing to do"},
	{Code: "identity.status.recovery_unprotected", Class: ClassI,
		Zh: "{path}（未设口令，与数据目录在一起）",
		En: "{path} (no passphrase, kept with the data directory)"},
	{Code: "identity.rotate.confirm", Class: ClassT,
		Zh: "将轮换 {name}（{aid8}）的签名密钥；对方会在收到你的下一条消息时自动更新，进行中的任务不受影响。",
		En: "This rotates the signing key of {name} ({aid8}); others update when they get your next message, and tasks in progress are not affected."},
	{Code: "identity.rotate.done", Class: ClassI,
		Zh: "已轮换：密钥序号 {from} → {to}，{hub} 已确认。",
		En: "Rotated: key number {from} → {to}, confirmed by {hub}."},
	{Code: "identity.rotate.emergency_summary.1", Class: ClassT,
		Zh: "紧急轮换 {name}（{aid8}）：用于私钥或数据目录可能已经泄露的情况。将会：",
		En: "Emergency rotation of {name} ({aid8}): for when the private key or the data directory may have leaked. It will:"},
	{Code: "identity.rotate.emergency_summary.2", Class: ClassT,
		Zh: "· 立即停用旧密钥：已知道你新密钥的对方立即拒收旧密钥签的消息，其余对方最迟 6 小时后拒收",
		En: "· stop the old key at once: peers that know your new key refuse messages signed with the old one right away, the rest within 6 hours"},
	{Code: "identity.rotate.emergency_summary.3", Class: ClassT,
		Zh: "· 更换加密钥，作废你发出的全部投递凭据",
		En: "· replace your encryption keys and void every delivery credential you have issued"},
	{Code: "identity.rotate.emergency_summary.4", Class: ClassT,
		Zh: "· 结束所有进行中的任务（对方会看到你做了紧急密钥轮换）",
		En: "· end every task in progress (the other side sees that you rotated keys in an emergency)"},
	{Code: "identity.rotate.emergency_summary.5", Class: ClassT,
		Zh: "· 更换本机控制令牌与 A2A 令牌，并更换恢复口令",
		En: "· replace this node's control and A2A tokens, and the recovery passphrase"},
	{Code: "identity.rotate.emergency_summary.5.unprotected", Class: ClassT,
		Zh: "· 更换本机控制令牌与 A2A 令牌，并设置恢复口令（数据目录里未设口令的恢复文件随即删除）",
		En: "· replace this node's control and A2A tokens, and set a recovery passphrase (the unprotected recovery file in the data directory is then deleted)"},
	{Code: "identity.rotate.emergency_witness_down", Class: ClassT,
		Zh: "本节点的见证 hub {hub} 现在连不上：紧急轮换会先停用本机的旧密钥，等它恢复后自动发布，期间暂停收发。若它已永久关闭，请按 Ctrl-C 取消，先换 hub（anet hub-register <新 hub>）再做紧急轮换。",
		En: "Witness hub {hub} is unreachable: the old key stops here now and the rotation is published when the hub is back; sending and receiving pause until then. If it is gone for good, press Ctrl-C and switch hubs first (anet hub-register <new hub>)."},
	{Code: "identity.rotate.emergency_done_seq", Class: ClassI,
		Zh: "已完成：密钥序号 {from} → {to}。",
		En: "Done: key number {from} → {to}."},
	{Code: "identity.switch.confirm", Class: ClassT,
		Zh: "把见证 hub 从 {old} 换到 {hub}，需要一次密钥轮换。",
		En: "Switching your witness hub from {old} to {hub} takes one key rotation."},
	{Code: "identity.switch.done", Class: ClassI,
		Zh: "已换到 {hub}（密钥序号 {from} → {to}）。",
		En: "Switched to {hub} (key number {from} → {to})."},
	{Code: "identity.switch.old_acked", Class: ClassI,
		Zh: "旧 hub {old} 已确认收到这次轮换；anet 会继续收取旧信箱里的信 15 天，之后自动离开。",
		En: "The old hub {old} confirmed this rotation; anet keeps collecting mail there for 15 days, then leaves it on its own."},
	{Code: "identity.recover.start", Class: ClassT,
		Zh: "恢复身份 {aid8}（见证 hub {hub}）。",
		En: "Recovering identity {aid8} (witness hub {hub})."},
	{Code: "identity.recover.new_passphrase", Class: ClassT,
		Zh: "恢复会同时更换恢复口令。",
		En: "Recovering also replaces the recovery passphrase."},
	{Code: "identity.recover.done", Class: ClassI,
		Zh: "已恢复为本机身份 {name}（数据目录 {path}）。新的恢复文件在 {file}，请另存一份到别处；旧的恢复文件已作废。",
		En: "Recovered as local identity {name} (data directory {path}). The new recovery file is {file}; keep a copy somewhere else. The old recovery file no longer works."},
	{Code: "identity.recover.old_stopped", Class: ClassI,
		Zh: "旧机器上的这个身份已停用；对方最迟 6 小时后只认新密钥。旧机器上的任务记录与名单不在这里。",
		En: "This identity has stopped working on the old machine; within 6 hours everyone accepts only the new key. Task records and lists from the old machine are not here."},
	{Code: "identity.deactivate.confirm", Class: ClassT,
		Zh: "注销 {name}（{aid8}）是永久的：任何人都不会再接受这个身份的签名，也无法撤销。",
		En: "Deactivating {name} ({aid8}) is permanent: nobody will accept this identity's signatures again, and it cannot be undone."},
	{Code: "identity.deactivate.tasks", Class: ClassT,
		Zh: "有 {n} 个进行中的任务会直接结束，对方不会收到通知；需要的话先结束它们再注销。",
		En: "{n} tasks in progress will end at once without telling the other side; finish them first if you need to."},
	{Code: "identity.deactivate.type_prefix", Class: ClassT,
		Zh: "输入 AID 末 8 个字符确认：",
		En: "Type the last 8 characters of the AID to confirm:"},
	{Code: "identity.deactivate.done", Class: ClassI,
		Zh: "已注销。本机数据保留在 {path}，不需要可删除：anet identity rm {name} --purge",
		En: "Deactivated. Local data stays in {path}; to remove it: anet identity rm {name} --purge"},
	{Code: "identity.rm.not_deactivated", Class: ClassT,
		Zh: "删除本机数据不会注销这个身份；要注销请先运行 anet identity deactivate。",
		En: "Removing local data does not deactivate this identity; to deactivate it, run anet identity deactivate first."},
	{Code: "identity.rm.dip_unacked", Class: ClassT,
		Zh: "本节点的见证 hub 还没确认这次注销；现在删除后 anet 不会再重试，别人可能继续给这个身份发信。",
		En: "The witness hub has not confirmed the deactivation yet; after removal anet stops retrying, and others may keep sending to this identity."},

	// ux §1.4 M3: auto-reply and the sandbox.
	{Code: "autoreply.updated", Class: ClassI,
		Zh: "已更新自动回复。",
		En: "Auto-reply updated."},
	{Code: "autoreply.updated_key", Class: ClassI,
		Zh: "已更新自动回复（API key 已设置）。",
		En: "Auto-reply updated (API key set)."},
	{Code: "autoreply.summary", Class: ClassS,
		Zh: "{backend} / {name}",
		En: "{backend} / {name}"},
	{Code: "autoreply.key_set", Class: ClassS,
		Zh: "已设置。",
		En: "Set."},
	{Code: "autoreply_test.pass_direct", Class: ClassI,
		Zh: "direct：通过（回复 {n} 字）。",
		En: "direct: passed ({n}-character reply)."},
	{Code: "autoreply_test.pass_sandbox", Class: ClassI,
		Zh: "sandbox：通过（经本节点调用模型 {n} 次，联网连接 {m} 次）。",
		En: "sandbox: passed ({n} model calls through this node, {m} network connections)."},
	{Code: "autoreply_test.fail", Class: ClassI,
		Zh: "{path}：没有通过——{why}",
		En: "{path}: failed — {why}"},
	{Code: "trust_confirm.direct", Class: ClassT,
		Zh: "信任 {name}（{peer}）：它的任务会直接以你的用户身份运行本机 agent，等同授予你的用户权限，也包括它转来的第三方内容。",
		En: "Trust {name} ({peer}): its tasks will run local agents directly as your user, which is the same as giving it your user's rights, including for third-party content it forwards."},
	{Code: "trust_confirm.sandbox", Class: ClassT,
		Zh: "信任 {name}（{peer}）：它的任务会在沙箱中运行本机 agent，不计入预算。",
		En: "Trust {name} ({peer}): its tasks will run local agents in the sandbox, outside the run budget."},
	{Code: "trust_confirm.no_agent", Class: ClassT,
		Zh: "信任 {name}（{peer}）：本节点没有自动回复，不会为它运行本机 agent；信任只决定本机后端是否接收它的任务。",
		En: "Trust {name} ({peer}): this node has no auto-reply and will not run a local agent for it; trust only decides whether local backends take its tasks."},
	{Code: "egress_confirm", Class: ClassT,
		Zh: "放行出网主机 {hosts}：未信任对端的任务可以经这些主机向外发送数据，anet 只核对主机名，不检查内容。",
		En: "Allow network hosts {hosts}: tasks from untrusted peers could send data out through them; anet checks the host name only, not the content."},
	{Code: "trusted_direct_confirm", Class: ClassT,
		Zh: "改回直接运行：信任的对端将以你的用户身份运行本机 agent，等同授予你的用户权限。",
		En: "Switch back to direct runs: trusted peers will run local agents as your user, which is the same as giving them your user's rights."},
	{Code: "autoreply.prompt.api_key", Class: ClassT,
		Zh: "API key：",
		En: "API key:"},

	// ux §1.4 M4: cards, payments, audit and balance.
	{Code: "card.header", Class: ClassI,
		Zh: "{name}  {peer}（{state}）  {charge}",
		En: "{name}  {peer} ({state})  {charge}"},
	{Code: "card.price", Class: ClassI,
		Zh: "{skill}  {price} credits",
		En: "{skill}  {price} credits"},
	{Code: "card.price_free", Class: ClassI,
		Zh: "{skill}  免费",
		En: "{skill}  free"},
	{Code: "card.price_unknown", Class: ClassI,
		Zh: "{skill}  —",
		En: "{skill}  —"},
	{Code: "card.footer", Class: ClassI,
		Zh: "价格由对方签名，取于 {time}；付款时以任务里的报价为准，高于这里的价格不会自动付。",
		En: "Prices are signed by the other side, fetched {time}; payment follows the quote in the task, and a quote above these prices is not paid automatically."},
	{Code: "card.footer_free", Class: ClassI,
		Zh: "以上能力都不收费。",
		En: "None of the skills above charge."},
	{Code: "card.preview", Class: ClassI,
		Zh: "这个 agent 会在写答复时发预览；用 anet delegate --watch 可以看到。",
		En: "This agent sends previews while it writes its reply; anet delegate --watch shows them."},
	{Code: "pay.prompt_quote", Class: ClassT,
		Zh: "报价 {amount} credits，付给 {peer}（{name}），能力 {skill}。",
		En: "Quoted {amount} credits, to {peer} ({name}), for {skill}."},
	{Code: "pay.prompt_price", Class: ClassT,
		Zh: "对方签名的价格是 {price} credits。",
		En: "The price the other side signed is {price} credits."},
	{Code: "pay.prompt_price_higher", Class: ClassT,
		Zh: "对方签名的价格是 {price} credits，这次报价更高。",
		En: "The price the other side signed is {price} credits; this quote is higher."},
	{Code: "pay.prompt_no_price", Class: ClassT,
		Zh: "没有取到对方签名的价格。",
		En: "No price signed by the other side was fetched."},
	{Code: "pay.prompt_limits", Class: ClassT,
		Zh: "人工付款上限：单笔 ≤{single}，今日剩 {left}。",
		En: "Manual limit: ≤{single} per payment, {left} left today."},
	{Code: "audit.chain_ok", Class: ClassI,
		Zh: "hub 发放链：{n} 个纪元，签名与链接都有效，与本机上次记录的链头一致。",
		En: "The hub's issuance chain: {n} periods, signatures and links valid, matching the head this node last recorded."},
	{Code: "audit.proofs_ok", Class: ClassI,
		Zh: "余额证明：{n} 个纪元核对通过，最新 {time} 余额 {amount} credits。",
		En: "Balance proofs: {n} periods checked, latest {time} balance {amount} credits."},
	{Code: "audit.none", Class: ClassI,
		Zh: "没有发现问题。",
		En: "No problems found."},
	{Code: "balance.summary", Class: ClassI,
		Zh: "余额 {amount} credits（托管在 hub {hub}）。",
		En: "Balance {amount} credits (held at the hub {hub})."},
	{Code: "balance.probation", Class: ClassI,
		Zh: "新账户见习期到 {time}；期满后自动发 20 credits 体验额度。",
		En: "The new account is on probation until {time}; 20 trial credits are paid automatically after that."},
	{Code: "balance.trial_pending", Class: ClassI,
		Zh: "见习期已结束；体验额度待发（今天的名额已用完），anet 会自动再试，不需要操作。",
		En: "Probation is over; the trial credits are still to come (today's allowance is used up), and anet tries again on its own, nothing to do."},
	{Code: "balance.trial_granted", Class: ClassI,
		Zh: "见习期已结束；体验额度 {amount} credits 已于 {time} 到账。",
		En: "Probation is over; {amount} trial credits arrived at {time}."},

	// ux §1.4 M5: peer lists and previews.
	{Code: "peers.sender_hidden", Class: ClassS,
		Zh: "信里不带你的身份（hub 仍可能凭网络地址和时间推断）",
		En: "your messages carry no sender identity (the hub may still infer it from network address and timing)"},
	{Code: "peers.sender_visible", Class: ClassS,
		Zh: "下一封信 hub 会直接知道是你（对方回信后自动改为不带身份）",
		En: "the hub will know directly that your next message is from you (this changes on its own once they reply)"},
	{Code: "peers.sender_visible.paid", Class: ClassS,
		Zh: "对方的服务可能收费：发新任务时 hub 会直接知道是你",
		En: "they may charge: the hub will know directly that new tasks are from you"},
	{Code: "preview.off", Class: ClassS,
		Zh: "预览关：不向请求方发送答复片段。",
		En: "Previews off: no reply snapshots are sent to requesters."},

	// ux §1.4 C3: delivery state sentences (DeliveryText); keys named after the values they render.
	{Code: "delivery.not_sent", Class: ClassI,
		Zh: "尚未发出：暂时连不上 hub，anet 会继续重试。",
		En: "Not sent yet: the hub cannot be reached; anet keeps trying."},
	{Code: "delivery.not_sent.ordered", Class: ClassI,
		Zh: "尚未发出：等这个任务的上一条消息确认后自动发出。",
		En: "Not sent yet: it goes out on its own once the task's previous message is confirmed."},
	{Code: "delivery.sent", Class: ClassI,
		Zh: "已发出，等待对方节点确认。",
		En: "Sent; waiting for the other side's node to confirm it."},
	{Code: "delivery.queued_at_hub", Class: ClassI,
		Zh: "已在 hub 排队，等对方取信（对方可能不在线）。",
		En: "Waiting at the hub for the other side to pick it up (they may be offline)."},
	{Code: "delivery.confirmed", Class: ClassI,
		Zh: "已送达：对方节点已确认。",
		En: "Delivered: the other side's node confirmed it."},
	{Code: "delivery.unconfirmed.not_sent", Class: ClassI,
		Zh: "没有发出：消息从未离开本机。",
		En: "Not sent: it never left this node."},
	{Code: "delivery.unconfirmed.sent", Class: ClassI,
		Zh: "没有确认送达：anet 已停止等待，对方仍可能收到过。",
		En: "Not confirmed: anet stopped waiting, and it may still have arrived."},
	{Code: "delivery.ends_at", Class: ClassI,
		Zh: "到 {time} 仍未送达，任务会以失败结束。",
		En: "If it is still not delivered by {time}, the task fails."},

	{Code: "hub.next_check", Class: ClassI,
		Zh: "anet 将在 {time} 再次向 hub 确认。",
		En: "anet asks the hub again at {time}."},

	// ux §3.4 (g): what an agent is told (SKILL.md, llms.txt and site.json carry it word for word).
	{Code: "agent_hint.join", Class: ClassI,
		Zh: "没有终端也能用 anet hub-register 完成接入；请把它打印的每一句原样转告用户（含恢复口令提醒与 admit 行），不要反复注册。恢复口令只能由用户本人在终端用 anet identity protect 设置，不要代为输入。",
		En: "Join with anet hub-register even without a terminal; relay each line it prints to the user as is (passphrase reminder, admit line); do not register again. Only the user, not you, sets the passphrase, via anet identity protect in a terminal."},

	// ux §4.7: status summary lines.
	{Code: "inbound.summary.closed", Class: ClassS, Level: LevelOK,
		Zh: "只接受 allow 名单里 {n} 个对端的任务；公开 {m} 项能力；自动回复{auto_reply}。",
		En: "Takes tasks only from the {n} peers on the allow list; {m} public capabilities; auto-reply {auto_reply}."},
	{Code: "inbound.summary.approve", Class: ClassS, Level: LevelOK,
		Zh: "名单外对端的任务先进待批队列（现有 {k} 个待批）；公开 {m} 项能力；自动回复{auto_reply}。",
		En: "Tasks from peers not on the list wait for approval ({k} waiting now); {m} public capabilities; auto-reply {auto_reply}."},
	{Code: "inbound.summary.open", Class: ClassS, Level: LevelOK,
		Zh: "接受任何人的任务；公开 {m} 项能力；自动回复{auto_reply}。",
		En: "Takes tasks from anyone; {m} public capabilities; auto-reply {auto_reply}."},
	{Code: "preview.on", Class: ClassS, Level: LevelInfo,
		Zh: "预览开：要预览的请求方每 5 秒以上收到一帧答复片段。",
		En: "Previews on: requesters who ask get a snapshot of the reply every 5 seconds or more."},
	{Code: "preview.with_reasoning", Class: ClassS, Level: LevelWarn,
		Zh: "预览开（含推理）：推理内容也会发给要求它的请求方。",
		En: "Previews on, with reasoning: reasoning is also sent to requesters who ask for it."},

	// ux §5.2: sentences only doctor says.
	{Code: "identity.recovery_note", Class: ClassS, Level: LevelInfo,
		Zh: "恢复口令只在轮换、换 hub、恢复与注销时需要；忘记口令后这些操作都做不了，身份仍可正常收发。恢复文件不在缺省位置时，若你已另存则不需要处理。",
		En: "The recovery passphrase is needed only to rotate, switch hubs, recover or deactivate; without it those are impossible, but the identity keeps working. If the recovery file is not in the default place and you saved a copy, nothing to do."},
	{Code: "identity.recovery_perm", Class: ClassS, Level: LevelWarn,
		Zh:       "恢复文件 {path} 的权限不是 0600；请执行 chmod 600 {path}。",
		En:       "The recovery file {path} is not mode 0600; run chmod 600 {path}.",
		NextKind: NextCommand, Next: "chmod 600 {path}"},
	{Code: "identity.recovery_leftover", Class: ClassS, Level: LevelWarn,
		Zh:       "有一份没有完成的新恢复文件 {path}；运行任意 anet identity 命令会自动收尾。",
		En:       "There is an unfinished new recovery file {path}; any anet identity command finishes it.",
		NextKind: NextCommand, Next: "anet identity status"},
	{Code: "sender_privacy.mailbox_pending", Class: ClassS, Level: LevelWarn,
		Zh: "本机的信箱签名钥还没登记到 hub，anet 会自动重新发布；超过 1 小时仍如此，请检查与 hub 的连接。",
		En: "This node's mailbox signing key is not registered at the hub yet, and anet republishes it on its own; if this lasts over an hour, check your connection to the hub."},
	{Code: "clock.ts_retries", Class: ClassS, Level: LevelWarn,
		Zh: "最近 1 小时有 {n} 次签名时间被 hub 判为落后，本机时钟可能在跳变；请启用 NTP。",
		En: "In the last hour the hub found this node's signing time behind {n} times, so the clock may be jumping; enable NTP."},
	{Code: "payments.no_payees", Class: ClassS, Level: LevelWarn,
		Zh:       "已开自动付款或 agent 付款，但付款名单是空的，自动付款不会发生；请用 anet payees add <AID> 加入收款方。",
		En:       "Automatic or agent payments are on, but the payee list is empty, so nothing is paid automatically; add payees with anet payees add <AID>.",
		NextKind: NextCommand, Next: "anet payees add <AID>"},
	{Code: "doctor.daemon_down", Class: ClassS, Level: LevelUnknown,
		Zh:       "daemon 没在运行；运行 anet up 后再查。",
		En:       "The daemon is not running; run anet up, then check again.",
		NextKind: NextCommand, Next: "anet up"},
	{Code: "clock.ok", Class: ClassS, Level: LevelOK,
		Zh: "本机时钟与 hub 相差 {n} 秒。",
		En: "This computer's clock is {n} seconds off from the hub."},
	{Code: "sender_privacy.grants_summary", Class: ClassS, Level: LevelInfo,
		Zh: "本节点持有 {n} 个对方给的投递凭据（向对方发信时信里不带本节点的身份）；已给出 {m} 个。",
		En: "This node holds {n} delivery credentials from others (its messages to them carry no sender identity); it has issued {m}."},
	{Code: "sender_privacy.withdrawals_pending", Class: ClassS, Level: LevelWarn,
		Zh: "有 {n} 个投递凭据的收回超过 1 小时没送到 hub；anet 会自动重试，请检查与 hub 的连接。",
		En: "{n} withdrawn delivery credentials have not reached the hub for over an hour; anet keeps retrying on its own; check your connection to the hub."},
	{Code: "sender_privacy.hub_refusals", Class: ClassS, Level: LevelWarn,
		Zh: "最近 24 小时有 {n} 个投递凭据被 hub 拒绝且查不出原因，相关的信已改走首次联系，hub 因此知道是你发的；如果反复出现，请考虑换用其他 hub。",
		En: "In the last 24 hours the hub refused {n} delivery credentials for no reason anet could find, so those messages went as first contacts and the hub knows they are from you; if this keeps happening, consider another hub."},
	{Code: "sender_privacy.hub_small_bucket", Class: ClassS, Level: LevelWarn,
		Zh: "hub 返回的目录分组少于 32 条，更容易猜出你在查谁；anet 已自动改用更宽的分组，如果反复出现，请考虑换用其他 hub。",
		En: "The hub returned a directory group with fewer than 32 entries, which makes it easier to guess whom you look up; anet now uses wider groups on its own, and if this keeps happening, consider another hub."},
	{Code: "sender_privacy.hub_epoch_invalid", Class: ClassS, Level: LevelWarn,
		Zh: "hub 报告的信箱签名钥序号大得不可能，anet 已从 1 重新编号；如果信箱签名钥一直登记不上，请联系 hub 运营者。",
		En: "The hub reported an impossibly large mailbox signing key number, so anet started again from 1; if the mailbox signing key stays unregistered, contact the hub operator."},
	{Code: "p2p.lan_on", Class: ClassS, Level: LevelInfo,
		Zh: "p2p 会拨局域网地址（LAN 模式已开）；只在局域网实验室这样设置。",
		En: "p2p dials local-network addresses (LAN mode is on); use this only in a LAN lab."},
	{Code: "keys.epoch_ok", Class: ClassS, Level: LevelOK,
		Zh: "加密钥已提前 14 天发布，没有多保留的过期密钥。",
		En: "Encryption keys are published 14 days ahead, and no expired keys are being kept."},
	{Code: "capabilities.files_not_service", Class: ClassS, Level: LevelFail,
		Zh: "能力 {name} 声明了 files，但不是由 service 模块提供；请改由 service 模块提供，或去掉 files。",
		En: "The capability {name} declares files but is not served by the service module; serve it through the service module, or remove files."},
	{Code: "payments.unchargeable", Class: ClassS, Level: LevelWarn,
		Zh: "本节点有标了价的能力（{n} 个），但{why}，这些能力的调用都会被拒绝；请{fix}，或去掉价格。",
		En: "This node has priced capabilities ({n}), but {why}, so calls to them will be refused; {fix}, or remove the prices."},
	{Code: "capabilities.files_not_service.absent", Class: ClassS, Level: LevelFail,
		Zh: "能力 {name} 声明了 files，但{why}，没有能接收文件的服务；请{fix}，或去掉 files。",
		En: "The capability {name} declares files, but {why}, so nothing can receive the files; {fix}, or remove files."},
	{Code: "capabilities.inflight_low", Class: ClassS, Level: LevelWarn,
		Zh: "能力 {name} 的超时超过 60 秒而 max_inflight 小于 2，一个调用就能占满；请调大 max_inflight。",
		En: "The capability {name} has a timeout over 60 seconds and max_inflight below 2, so one call fills it; raise max_inflight."},
	{Code: "capabilities.allow_only", Class: ClassS, Level: LevelInfo,
		Zh: "{name} 只对名单开放（{n} 个调用方）。",
		En: "{name} is open only to your lists ({n} callers)."},
	{Code: "capabilities.allow_only_empty", Class: ClassS, Level: LevelWarn,
		Zh:       "{name} 只对名单开放，但 allow 与 trust 名单都是空的，没有人能调用；请用 anet peers allow <AID> 加入调用方。",
		En:       "{name} is open only to your lists, but the allow and trust lists are both empty, so nobody can call it; add callers with anet peers allow <AID>.",
		NextKind: NextCommand, Next: "anet peers allow <AID>"},
	{Code: "preview.no_source", Class: ClassS, Level: LevelInfo,
		Zh: "预览开关已开，但没有 openai 自动回复，预览只来自 /tasks/preview。",
		En: "Previews are on, but there is no openai auto-reply, so previews come only from /tasks/preview."},
	{Code: "payments.audit_ok", Class: ClassS, Level: LevelOK,
		Zh: "余额证明最近一次核对通过（{time}）。",
		En: "The last balance-proof check passed ({time})."},
	{Code: "payments.reviews_pending", Class: ClassS, Level: LevelInfo,
		Zh: "{n} 条评价等待上传。",
		En: "{n} reviews are waiting to be uploaded."},
	{Code: "payments.reviews_stuck", Class: ClassS, Level: LevelWarn,
		Zh: "有评价超过 7 天没能上传；请检查与 hub 的连接。",
		En: "Some reviews have not been uploaded for over 7 days; check your connection to the hub."},
	{Code: "payments.card_failed", Class: ClassS, Level: LevelFail,
		Zh:       "有标价的能力，但扩展卡片生成或验签失败（{code}）；请运行 anet doctor --json 并提交问题报告。",
		En:       "There are priced capabilities, but the extended card failed to build or verify ({code}); run anet doctor --json and file a report.",
		NextKind: NextCommand, Next: "anet doctor --json"},
	{Code: "payments.price_invalid", Class: ClassS, Level: LevelFail,
		Zh: "能力 {name} 的价格不在 1 到 2^63−1 之间；请改正 config.json。",
		En: "The price of the capability {name} is not between 1 and 2^63−1; fix it in config.json."},
	{Code: "payments.card_no_public", Class: ClassS, Level: LevelWarn,
		Zh: "只有名单内的能力标了价，本节点没有公开能力，所以没有扩展卡片：名单里的调用方看不到签名的价格，每笔都要手动付款；要让他们自动付款，请至少公开一个能力。",
		En: "Only allow-listed capabilities are priced and none is public, so there is no extended card: listed callers see no signed price and pay each task by hand; to let them pay automatically, make at least one capability public."},
	{Code: "autoreply.off", Class: ClassS, Level: LevelInfo,
		Zh: "自动回复关。",
		En: "Auto-reply is off."},
	{Code: "sandbox.proxy_env", Class: ClassS, Level: LevelWarn,
		Zh: "daemon 的环境里设了代理变量（{key}）；沙箱不会使用它，请确认这是你想要的。",
		En: "A proxy variable ({key}) is set in the daemon's environment; the sandbox does not use it, so check that this is what you want."},
	{Code: "sandbox.worst_case", Class: ClassS, Level: LevelInfo,
		Zh: "每天至多 {daily} 次运行，每次至多 60 次模型请求、上行 16 MiB。",
		En: "At most {daily} runs a day, each with at most 60 model requests and 16 MiB sent."},
	{Code: "backends.path_only", Class: ClassS, Level: LevelWarn,
		Zh: "后端 {name} 设为 path-only，只按路径核实对端。",
		En: "The backend {name} is set to path-only, so its peer is checked by path only."},
	{Code: "anetlink.not_remote", Class: ClassS, Level: LevelWarn,
		Zh: "公开能力 {name} 在 ANetLink 里不接受远端调用（c1.remote 没有匹配），别人调用会得到 provider_policy；请在 ANetLink 配置 c1.remote 放行，或不公开它。",
		En: "The public capability {name} does not take remote calls in ANetLink (no c1.remote match), so callers get provider_policy; allow it with c1.remote in the ANetLink config, or stop publishing it."},

	// `anet agents wire|unwire|status` (package agentwire): the status words, the lines
	// of a result, the notes, and every refusal wire can end with. The wire_ codes are
	// class C; a path, a tool name, a line number or an operating-system error is the
	// only text a placeholder takes, never a line of a file the user wrote.
	{Code: "wire.status.written", Class: ClassI,
		Zh: "已写入",
		En: "written"},
	{Code: "wire.status.up_to_date", Class: ClassI,
		Zh: "已是最新",
		En: "up to date"},
	{Code: "wire.status.skipped", Class: ClassI,
		Zh: "未安装，已跳过",
		En: "not installed, skipped"},
	{Code: "wire.status.conflict", Class: ClassI,
		Zh: "冲突",
		En: "conflict"},
	{Code: "wire.status.removed", Class: ClassI,
		Zh: "已移除",
		En: "removed"},
	{Code: "wire.status.not_wired", Class: ClassI,
		Zh: "未接入",
		En: "not wired"},
	{Code: "wire.status.failed", Class: ClassI,
		Zh: "失败",
		En: "failed"},
	{Code: "wire.head.wire", Class: ClassI,
		Zh: "anet agents wire（anet {bin}，数据目录 {path}）",
		En: "anet agents wire (anet {bin}, data directory {path})"},
	{Code: "wire.head.unwire", Class: ClassI,
		Zh: "anet agents unwire（anet {bin}，数据目录 {path}）",
		En: "anet agents unwire (anet {bin}, data directory {path})"},
	{Code: "wire.head.status", Class: ClassI,
		Zh: "anet agents（anet {bin}，数据目录 {path}）",
		En: "anet agents (anet {bin}, data directory {path})"},
	{Code: "wire.footer.restart", Class: ClassI,
		Zh: "重启这些工具（或新开会话）后生效；它们调用 anet mcp，需要本机 daemon 在运行（anet up）。",
		En: "Restart these tools (or start a new session) for the change to take effect. They call anet mcp, which needs the local daemon to be running (anet up)."},
	{Code: "wire.footer.undo", Class: ClassI,
		Zh: "撤销：anet agents unwire <工具>（或 --all）",
		En: "To undo: anet agents unwire <tool> (or --all)"},
	{Code: "wire.state.current", Class: ClassS, Level: LevelOK,
		Zh: "已接入，最新",
		En: "wired, up to date"},
	{Code: "wire.state.stale", Class: ClassS, Level: LevelWarn,
		Zh: "已接入，需要更新（{command}）",
		En: "wired, needs an update ({command})"},
	{Code: "wire.state.not_installed", Class: ClassS, Level: LevelInfo,
		Zh: "未安装",
		En: "not installed"},
	{Code: "wire.state.not_wired", Class: ClassS, Level: LevelInfo,
		Zh: "未接入（{command}）",
		En: "not wired ({command})"},
	{Code: "wire.state.conflict", Class: ClassS, Level: LevelWarn,
		Zh: "冲突：{detail}",
		En: "conflict: {detail}"},
	{Code: "wire.state.unchecked", Class: ClassS, Level: LevelUnknown,
		Zh: "无法检查：{detail}",
		En: "cannot be checked: {detail}"},
	{Code: "wire.state.token_mode", Class: ClassS, Level: LevelWarn,
		Zh: "{path} 含令牌，但权限是 {value}（应为 0600；{command} 会收紧）",
		En: "{path} holds a token but its permissions are {value} (they should be 0600; {command} tightens them)"},
	{Code: "wire.state.a2a_ok", Class: ClassS, Level: LevelOK,
		Zh: "a2a {aid}：正常",
		En: "a2a {aid}: in order"},
	{Code: "wire.state.a2a_changed", Class: ClassS, Level: LevelWarn,
		Zh: "a2a {aid}：端口或令牌已变（{command}）",
		En: "a2a {aid}: the port or the token has changed ({command})"},
	{Code: "wire.state.a2a_unknown", Class: ClassS, Level: LevelUnknown,
		Zh: "a2a {aid}：无法核对（{detail}）",
		En: "a2a {aid}: cannot be verified ({detail})"},
	{Code: "wire.pending.a2a_stale", Class: ClassS, Level: LevelWarn,
		Zh: "a2a_agents 指向旧的端口或令牌（--refresh）",
		En: "a2a_agents points at an old port or token (--refresh)"},
	{Code: "wire.change.guide_write", Class: ClassI,
		Zh: "写入 anet 指引块：{path}",
		En: "write the anet guide block: {path}"},
	{Code: "wire.change.guide_drop", Class: ClassI,
		Zh: "删除 anet 指引块：{path}",
		En: "remove the anet guide block: {path}"},
	{Code: "wire.change.legacy_drop", Class: ClassI,
		Zh: "删除旧版 anet 指引块：{path}",
		En: "remove the legacy anet guide block: {path}"},
	{Code: "wire.change.openclaw_drop", Class: ClassI,
		Zh: "删除旧版 anet 指引块（OpenClaw 不在支持的工具内）：{path}",
		En: "remove the legacy anet guide block (OpenClaw is not a supported tool): {path}"},
	{Code: "wire.change.legacy_file_drop", Class: ClassI,
		Zh: "删除旧版规则文件：{path}",
		En: "remove the legacy rules file: {path}"},
	{Code: "wire.change.claude_cli_add", Class: ClassI,
		Zh: "claude mcp add -s user anet（{path}）",
		En: "claude mcp add -s user anet ({path})"},
	{Code: "wire.change.claude_cli_remove", Class: ClassI,
		Zh: "claude mcp remove -s user anet（{path}）",
		En: "claude mcp remove -s user anet ({path})"},
	{Code: "wire.change.claude_file_add", Class: ClassI,
		Zh: "MCP 服务 anet（user 作用域）→ {path}（claude 不在 PATH，直接改文件）",
		En: "MCP server anet (user scope) → {path} (claude is not on PATH, so the file was edited directly)"},
	{Code: "wire.change.mcp_drop", Class: ClassI,
		Zh: "删除 MCP 服务 anet：{path}",
		En: "remove the MCP server anet: {path}"},
	{Code: "wire.change.guide_file_write", Class: ClassI,
		Zh: "技能 → {path}",
		En: "Claude Code skill → {path}"},
	{Code: "wire.change.guide_file_drop", Class: ClassI,
		Zh: "删除技能：{path}",
		En: "remove the Claude Code skill: {path}"},
	{Code: "wire.change.toml_write", Class: ClassI,
		Zh: "[mcp_servers.anet] → {path}",
		En: "[mcp_servers.anet] → {path}"},
	{Code: "wire.change.toml_drop", Class: ClassI,
		Zh: "删除 [mcp_servers.anet]：{path}",
		En: "remove [mcp_servers.anet]: {path}"},
	{Code: "wire.change.json_write", Class: ClassI,
		Zh: "{key}.anet → {path}",
		En: "{key}.anet → {path}"},
	{Code: "wire.change.json_drop", Class: ClassI,
		Zh: "删除 {key}.anet：{path}",
		En: "remove {key}.anet: {path}"},
	{Code: "wire.change.hermes_mcp", Class: ClassI,
		Zh: "mcp_servers.anet → {path}",
		En: "mcp_servers.anet → {path}"},
	{Code: "wire.change.hermes_a2a", Class: ClassI,
		Zh: "a2a_agents（{n} 个，指向 {host}）→ {path}",
		En: "a2a_agents ({n} entries, pointing at {host}) → {path}"},
	{Code: "wire.change.hermes_both", Class: ClassI,
		Zh: "mcp_servers.anet、a2a_agents（{n} 个，指向 {host}）→ {path}",
		En: "mcp_servers.anet, a2a_agents ({n} entries, pointing at {host}) → {path}"},
	{Code: "wire.change.hermes_drop_mcp", Class: ClassI,
		Zh: "删除 mcp_servers.anet：{path}",
		En: "remove mcp_servers.anet: {path}"},
	{Code: "wire.change.hermes_drop_a2a", Class: ClassI,
		Zh: "删除 a2a_agents（含令牌）：{path}",
		En: "remove a2a_agents (with its token): {path}"},
	{Code: "wire.change.hermes_drop_both", Class: ClassI,
		Zh: "删除 mcp_servers.anet、a2a_agents（含令牌）：{path}",
		En: "remove mcp_servers.anet and a2a_agents (with its token): {path}"},
	{Code: "wire.change.hermes_drop_entries", Class: ClassI,
		Zh: "删除 a2a_agents 条目 {n} 个：{path}",
		En: "remove {n} a2a_agents entries: {path}"},
	{Code: "wire.change.token_mode", Class: ClassI,
		Zh: "{path} 含本机 A2A 令牌，权限收紧为 0600",
		En: "{path} holds the local A2A token; its permissions are tightened to 0600"},
	{Code: "wire.note.not_detected", Class: ClassI,
		Zh: "未检测到该工具，按要求照写",
		En: "the tool was not detected; written as requested"},
	{Code: "wire.note.a2a_skipped", Class: ClassI,
		Zh: "--a2a 未写入：没有找到 Hermes（设置 HERMES_HOME，或明确写 hermes）",
		En: "--a2a was not written: Hermes was not found (set HERMES_HOME, or name hermes explicitly)"},
	{Code: "wire.note.hermes_toolset", Class: ClassI,
		Zh: "Hermes 的 a2a 工具集默认关闭：运行 hermes tools enable a2a --platform cli 后，a2a_call 才可见",
		En: "The Hermes a2a toolset is off by default: a2a_call is visible only after hermes tools enable a2a --platform cli"},
	{Code: "wire.note.codex_override", Class: ClassI,
		Zh: "{path} 存在：Codex 优先读取它而不是 AGENTS.md，anet 写入 AGENTS.md 的指引不会生效；请把指引并入该文件，或移走它",
		En: "{path} exists: Codex reads it instead of AGENTS.md, so the guide anet wrote to AGENTS.md has no effect; merge the guide into that file or move it away"},
	{Code: "wire.note.workbuddy_docs", Class: ClassI,
		Zh: "WorkBuddy 的重载方式与工具调用超时（默认 30 秒）以官方文档为准：{url}；anet 只写了 MCP 条目，没有写指引文件",
		En: "How WorkBuddy reloads, and its tool-call timeout (30 seconds by default), are described in its documentation: {url}. anet wrote only the MCP entry, not a guide file"},
	{Code: "wire.note.foreign_entry", Class: ClassI,
		Zh: "{path} 的 {key}.anet 不是 anet 写的，未改动",
		En: "{key}.anet in {path} was not written by anet and was left unchanged"},
	{Code: "wire.note.foreign_toml", Class: ClassI,
		Zh: "{path} 第 {line} 行的 mcp_servers.anet 不是 anet 写的，未改动",
		En: "mcp_servers.anet at {path}:{line} was not written by anet and was left unchanged"},
	{Code: "wire.note.a2a_absent", Class: ClassI,
		Zh: "a2a_agents 里没有 anet 写的这些条目",
		En: "a2a_agents has none of these entries written by anet"},
	{Code: "wire_tools_failed", Class: ClassC,
		Zh:       "有 {n} 个工具没有完成；请按上面各行处理后重试。",
		En:       "{n} of the tools could not be done; follow the lines above, then try again.",
		NextKind: NextChangeInput},
	{Code: "wire_home_not_found", Class: ClassC,
		Zh:       "找不到家目录；请设置 HOME 后重试。",
		En:       "The home directory cannot be found; set HOME and try again.",
		NextKind: NextChangeInput},
	{Code: "wire_path_not_absolute", Variant: "bin", Class: ClassC,
		Zh:       "anet 可执行文件的路径必须是绝对路径（得到 {path}）；请改用已安装的 anet 的完整路径。",
		En:       "The path of the anet binary must be absolute (got {path}); use the full path of the installed binary.",
		NextKind: NextChangeInput},
	{Code: "wire_path_not_absolute", Variant: "data", Class: ClassC,
		Zh:       "数据目录必须是绝对路径（得到 {path}）；请改用绝对路径。",
		En:       "The data directory must be an absolute path (got {path}); use an absolute path.",
		NextKind: NextChangeInput},
	{Code: "wire_bad_aid", Class: ClassC,
		Zh:       "{value} 不是 AID（小写字母与数字，8 到 128 个字符）；请改用 anet find 显示的 AID。",
		En:       "{value} is not an AID (lowercase letters and digits, 8 to 128 characters); use an AID that anet find shows.",
		NextKind: NextChangeInput},
	{Code: "wire_unknown_tool", Class: ClassC,
		Zh:       "不认识的工具 {name}（支持：{value}）；请改用其中之一。",
		En:       "{name} is not a supported tool (supported: {value}); use one of them.",
		NextKind: NextChangeInput},
	{Code: "wire_a2a_needs_hermes", Variant: "wire", Class: ClassC,
		Zh:       "--a2a 写的是 Hermes 的 a2a_agents；请加上 hermes（或 --all）。",
		En:       "--a2a writes Hermes' a2a_agents entries; add hermes (or --all) to the command.",
		NextKind: NextChangeInput},
	{Code: "wire_a2a_needs_hermes", Variant: "unwire", Class: ClassC,
		Zh:       "--a2a 删除的是 Hermes 的 a2a_agents 条目；请加上 hermes（或 --all）。",
		En:       "--a2a removes Hermes' a2a_agents entries; add hermes (or --all) to the command.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Class: ClassC,
		Zh:       "{path} 里已有一个不是 anet 写的 {key}.anet；请删除或改名后再运行 wire。",
		En:       "{path} already has an entry {key}.anet that anet did not write; remove or rename it, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "claude", Class: ClassC,
		Zh:       "{path} 里已有一个不是 anet 写的 mcpServers.anet；请删除或改名（claude mcp remove -s user anet）后再运行 wire。",
		En:       "{path} already has an entry mcpServers.anet that anet did not write; remove or rename it (claude mcp remove -s user anet), then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "toml", Class: ClassC,
		Zh:       "{path} 第 {line} 行已定义了一个不是 anet 写的 mcp_servers.anet；请删除或改名后再运行 wire。",
		En:       "{path}:{line} already defines an entry mcp_servers.anet that anet did not write; remove or rename it, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "toml_inline", Class: ClassC,
		Zh:       "{path} 第 {line} 行的 mcp_servers 是行内表，TOML 不允许再向它追加 [mcp_servers.anet]；请改用 [mcp_servers.<名字>] 表的写法后再运行 wire。",
		En:       "mcp_servers at {path}:{line} is an inline table, and TOML does not allow [mcp_servers.anet] to be added to it; use [mcp_servers.<name>] tables instead, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "yaml_entry", Class: ClassC,
		Zh:       "{path} 第 {line} 行的 {key} 下已有一个不是 anet 写的 {name} 条目；请删除或改名后再运行 wire。",
		En:       "{key} at {path}:{line} already has an entry {name} that anet did not write; remove or rename it, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "yaml_top", Class: ClassC,
		Zh:       "{path} 的顶层不是映射（第 {line} 行不是键值对），anet 无法加入 {key}；请改用映射写法后再运行 wire。",
		En:       "The top level of {path} is not a mapping (line {line} is not key: value), so anet cannot add {key}; use the mapping form, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "yaml_inline", Class: ClassC,
		Zh:       "{path} 第 {line} 行的 {key} 用的是行内写法，anet 无法在不解析 YAML 的情况下加入条目；请改用块写法后再运行 wire。",
		En:       "{key} at {path}:{line} is written inline, and anet cannot add entries to it without parsing YAML; use the block form, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_conflict", Variant: "yaml_shape", Class: ClassC,
		Zh:       "{path} 第 {line} 行的 {key} 不是 anet 能加入条目的映射；请改用块映射写法后再运行 wire。",
		En:       "{key} at {path}:{line} is not a mapping that anet can add entries to; use the block mapping form, then run wire again.",
		NextKind: NextChangeInput},
	{Code: "wire_json_invalid", Class: ClassC,
		Zh:       "{path} 不是合法的 JSON，anet 不改它；请修复或从备份恢复后重试。",
		En:       "{path} is not valid JSON, so anet leaves it unchanged; repair it or restore a backup, then try again.",
		NextKind: NextChangeInput},
	{Code: "wire_not_plain_json", Class: ClassC,
		Zh:       "{path} 不是纯 JSON（可能含注释），anet 不改写它；请加入下面的条目。",
		En:       "{path} is not plain JSON (it may contain comments), so anet does not rewrite it; add the entry below by hand.",
		NextKind: NextChangeInput},
	{Code: "wire_json_shape", Class: ClassC,
		Zh:       "{path} 里的 {key} 不是 JSON 对象，anet 不改它；请改成对象后重试。",
		En:       "{key} in {path} is not a JSON object, so anet leaves it unchanged; change it to an object and try again.",
		NextKind: NextChangeInput},
	{Code: "wire_block_malformed", Variant: "second", Class: ClassC,
		Zh:       "{path} 第 {line} 行又开始了一个 anet 托管块；请手动删掉其中一个。",
		En:       "A second anet managed block starts at {path}:{line}; remove one of them by hand.",
		NextKind: NextChangeInput},
	{Code: "wire_block_malformed", Variant: "orphan_end", Class: ClassC,
		Zh:       "{path} 第 {line} 行的托管块结束标记没有对应的开始标记；请删除该标记或补上开始标记。",
		En:       "The end marker of the managed block at {path}:{line} has no start marker; remove the marker or add the start marker.",
		NextKind: NextChangeInput},
	{Code: "wire_block_malformed", Variant: "no_end", Class: ClassC,
		Zh:       "{path} 第 {line} 行开始的托管块没有结束标记；请删除该块或补上结束标记。",
		En:       "The managed block that starts at {path}:{line} has no end marker; add the end marker or remove the block.",
		NextKind: NextChangeInput},
	{Code: "wire_changed_during_run", Class: ClassC,
		Zh:       "{path} 在 anet 读取之后被改动了，未写入；请等写入它的程序结束后重试。",
		En:       "{path} changed after anet read it, so nothing was written; try again.",
		NextKind: NextChangeInput},
	{Code: "wire_dangling_symlink", Class: ClassC,
		Zh:       "{path} 是指向不存在文件的符号链接，anet 不替换它；请修正链接后重试。",
		En:       "{path} is a symbolic link to a file that does not exist, and anet does not replace it; fix the link and try again.",
		NextKind: NextChangeInput},
	{Code: "wire_backup_failed", Class: ClassC,
		Zh:       "无法备份 {path}（{error}），未做任何改动；请修复权限或路径后重试。",
		En:       "{path} cannot be backed up ({error}), so nothing was changed; fix the permissions or the path and try again.",
		NextKind: NextChangeInput},
	{Code: "wire_backup_limit", Class: ClassC,
		Zh:       "{path} 在一秒内的备份过多；请稍后再试。",
		En:       "There are too many backups of {path} within one second; try again in a moment.",
		NextKind: NextRetryLater},
	{Code: "wire_io_failed", Class: ClassC,
		Zh:       "无法读写 {path}（{error}）；请修复权限或路径后重试。",
		En:       "{path} cannot be read or written ({error}); fix the permissions or the path and try again.",
		NextKind: NextChangeInput},
	{Code: "wire_cli_failed", Class: ClassC,
		Zh:       "{command} 执行失败（{error}）；请排除该命令报告的问题后重试。",
		En:       "{command} failed ({error}); fix what that command reports and try again.",
		NextKind: NextChangeInput},
	{Code: "wire_verify_failed", Variant: "add", Class: ClassC,
		Zh:       "claude 命令报告成功，但 {path} 里并没有 mcpServers.anet；请检查该文件后重试。",
		En:       "The claude command reported success, but {path} has no mcpServers.anet afterwards; try again.",
		NextKind: NextChangeInput},
	{Code: "wire_verify_failed", Variant: "remove", Class: ClassC,
		Zh:       "claude 命令报告成功，但 {path} 里仍有 mcpServers.anet；请检查该文件后重试。",
		En:       "The claude command reported success, but {path} still has mcpServers.anet afterwards; try again.",
		NextKind: NextChangeInput},
	{Code: "wire_a2a_not_started", Class: ClassC,
		Zh:       "找不到 {path}：本机 A2A 接口还没有启动过（-tags no_a2a 的构建没有这个模块）；请先运行 anet up，再运行本命令。",
		En:       "{path} does not exist: the local A2A interface has not been started yet (a -tags no_a2a build has no such module); run anet up first, then run this command again.",
		NextKind: NextCommand, Next: "anet up"},
	{Code: "wire_a2a_addr_invalid", Class: ClassC,
		Zh:       "{path} 的内容不是回环地址上的主机与端口（本机 A2A 接口只在 127.0.0.1、localhost 或 [::1] 上监听）；请运行 anet stop 和 anet up 重新写出。",
		En:       "{path} does not hold a host:port on a loopback address (the local A2A interface listens only on 127.0.0.1, localhost or [::1]); run anet stop and anet up to write it again.",
		NextKind: NextChangeInput},
	{Code: "wire_a2a_token_invalid", Class: ClassC,
		Zh:       "{path} 的内容不是一个令牌；请运行 anet stop 和 anet up 重新生成。",
		En:       "{path} does not hold a token; run anet stop and anet up to generate it again.",
		NextKind: NextChangeInput},
	{Code: "wire_a2a_port_foreign", Variant: "user", Class: ClassC,
		Zh:       "本机 A2A 接口的端口 {host} 被另一个本机用户（uid {n}）的进程占着，现在写入令牌会交给它；请让它释放端口，运行 anet stop 和 anet up 后再运行本命令。",
		En:       "The port {host} of the local A2A interface is held by a process of another local user (uid {n}), and writing the token now would hand it to that process; have it release the port, run anet stop and anet up, then run this command again.",
		NextKind: NextChangeInput},
	{Code: "wire_a2a_port_foreign", Variant: "record", Class: ClassC,
		Zh:       "本机 A2A 接口没有启动，它记录的端口 {host} 被别的进程占着（见 {path}），现在写入令牌会交给它；请让占用者释放端口，运行 anet stop 和 anet up 后再运行本命令。",
		En:       "The local A2A interface is down because another process holds its recorded port {host} (see {path}), and writing the token now would hand it to that process; have the holder release the port, run anet stop and anet up, then run this command again.",
		NextKind: NextChangeInput},
}

// --- modules (MOD-1) ---

// moduleNames are the names of the optional modules in a sentence ("付款" /
// "payment"). module.Known() carries the same two columns; a test in
// package module holds the two together.
var moduleNames = map[string]phrasePair{
	"a2a":        {"本机 A2A 接口", "local A2A interface"},
	"anetlink":   {"设备能力", "device capability"},
	"autoreply":  {"自动回复", "auto-reply"},
	"blackboard": {"共享黑板", "shared blackboard"},
	"cas":        {"内容存储", "content store"},
	"console":    {"控制台", "console"},
	"mcp":        {"MCP", "MCP"},
	"org":        {"组织", "organisation"},
	"p2p":        {"直连", "direct delivery"},
	"preview":    {"预览", "preview"},
	"service":    {"服务", "service"},
	"shell":      {"远程命令", "remote command"},
	"x402":       {"付款", "payment"},
}

// ModuleName is a module's name as a sentence shows it, in a language; the
// module's own name when the table has none.
func ModuleName(m string, lang Language) string {
	if p, ok := moduleNames[m]; ok {
		return p.in(lang)
	}
	return m
}

// moduleWhy and moduleFix are {why} and {fix} of module_absent and its
// relatives, by reason (module.Absent* constants). {name} and {m} inside
// them are the module's name and its tag-form name.
var moduleWhy = map[string]phrasePair{
	"not_compiled": {"这个 anet 不含{name}模块（{m}）", "this anet does not include the {name} module ({m})"},
	"not_compiled_add": {"这个 anet 不含{name}模块（{m}），它只在 --shell 版本里",
		"this anet does not include the {name} module ({m}), which comes only in the --shell build"},
	"without": {"本节点关闭了{name}模块（{m}；config.json 的 without）",
		"this node has turned off the {name} module ({m}; without in config.json)"},
}

var moduleFix = map[string]phrasePair{
	"not_compiled":     {"改装缺省版本（install.sh）", "install the default build (install.sh)"},
	"not_compiled_add": {"用 install.sh --shell 安装", "install with install.sh --shell"},
	"without":          {"运行 anet down、anet init --with {m}、anet up", "run anet down, anet init --with {m}, anet up"},
}

func init() {
	// The same phrases as phrase sets, for the lint that checks every
	// sentence carries its next step (reasons_lint_test.go).
	phraseSets["module_why"], phraseSets["module_fix"] = moduleWhy, moduleFix
}

// configRules are the phrases of config_invalid's {rule} that name a module.
var configRules = map[string]phrasePair{
	"without_not_ready": {"这个构建还不能关闭{name}模块（{m}）", "this build cannot turn off the {name} module ({m}) yet"},
	"top_level_key":     {"请写顶层 auto_reply / stream_preview", "write the top-level auto_reply / stream_preview"},
}

// moduleCodes are the codes whose {why} and {fix} (and, for the first
// group, {name}) are about the module in Args["m"].
func moduleCode(code, variant string) (nameIsModule, ok bool) {
	switch code {
	case "module_absent", "without_configured", "build_tag_pending":
		return true, true
	case "payments.unchargeable":
		return false, true
	case "capabilities.files_not_service.absent":
		return false, true
	}
	return false, false
}

// moduleValue is the value of a placeholder of the codes above and of
// config_invalid's module rules.
func moduleValue(code, variant, name string, lang Language, args Args) (string, bool) {
	m := ""
	if present(args, "m") {
		m = fmt.Sprint(args["m"])
	}
	fill := func(t string) string {
		t = strings.ReplaceAll(t, "{name}", ModuleName(m, lang))
		return strings.ReplaceAll(t, "{m}", m)
	}
	if code == "config_invalid" && name == "rule" && present(args, "rule") {
		if p, ok := configRules[fmt.Sprint(args["rule"])]; ok {
			return fill(p.in(lang)), true
		}
		return "", false
	}
	nameIsModule, ok := moduleCode(code, variant)
	if !ok {
		return "", false
	}
	switch name {
	case "name":
		if nameIsModule && m != "" {
			return ModuleName(m, lang), true
		}
	case "m":
		if m != "" {
			return clean(m, maxNameRunes), true
		}
	case "why", "fix":
		set := moduleWhy
		if name == "fix" {
			set = moduleFix
		}
		if p, ok := set[fmt.Sprint(args["reason"])]; ok && m != "" {
			return fill(p.in(lang)), true
		}
		return "", false
	}
	return "", false
}

// OutboxDeadline is one outbound row of a task as DeliveryEndsAt reads it
// (the projection in package stored fills it).
type OutboxDeadline struct {
	Delegation, Handed, Frozen bool
	HandedAt, Exp              time.Time
}

// DeliveryEndsAt is deliveryEndsAt for a caller outside this package.
func DeliveryEndsAt(rows []OutboxDeadline, noResponseAfter time.Duration, terminal bool) time.Time {
	in := make([]outboxDeadline, 0, len(rows))
	for _, r := range rows {
		in = append(in, outboxDeadline{delegation: r.Delegation, handed: r.Handed, handedAt: r.HandedAt, exp: r.Exp, frozen: r.Frozen})
	}
	return deliveryEndsAt(in, noResponseAfter, terminal)
}

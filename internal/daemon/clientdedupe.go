package daemon

// clientdedupe.go recognises a local client's retry of a message it sent
// (correctness-ux CUX-D1 to D10; A2A §3.3: "Agents may utilize the messageId
// to detect duplicate messages").
//
// A message that names a messageId is stored with two plain columns beside
// it: client_key = SHA-256("anet-client-key/v1" ‖ 0x00 ‖ messageId) (core
// L37), by which a later message under the same id finds it, and
// client_digest = HMAC-SHA256(mac_key, "anet-client-msg/v1" ‖ 0x00 ‖ canon)
// (core L38), which says whether the later message has the same content.
// mac_key is derived from the task's data key (core L36), so the digest says
// nothing about the content to anyone without that key, and a task whose
// content has expired cannot be matched any more.
//
// The rule has two sentences: while the task is open, the same id must carry
// the same content (then the call is answered with the task and nothing is
// sent) or is refused (MESSAGE_ID_REUSED, nothing sent); once the task has
// ended, the same id is a new request — except the retry of a call that was
// cut off before its answer, of a task that may have run, within 10 minutes
// of its end (CUX-D4). A follow-up under an id already used on its task is
// the same message (same content) or a refusal, whatever the task's state.
//
// MCP sends no message ids and the console's /delegate and /message carry
// none: neither takes part (CUX-D9, D10).

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// The two labels of a client message's retry key (core L37, L38).
const (
	clientKeyLabel = "anet-client-key/v1"
	clientMsgLabel = "anet-client-msg/v1"
)

// clientRetryWindow is how long after a task ended the retry of a call that
// was not answered is still given that task (CUX-D4).
const clientRetryWindow = 10 * time.Minute

// previewExtensionURI is the preview extension (core §2.13). A client that
// asks for previews on one attempt and not on the next sends the same
// message, so it is left out of the content (CUX-D1).
const previewExtensionURI = "https://agentnetwork.org.cn/a2a/ext/anet-preview/v1"

// clientKey is a client messageId's key (core L37).
func clientKey(messageID string) []byte {
	h := sha256.New()
	h.Write([]byte(clientKeyLabel))
	h.Write([]byte{0})
	h.Write([]byte(messageID))
	return h.Sum(nil)
}

// clientDigest is the digest of a client message's canonical content under
// one task's mac_key (core L38).
func clientDigest(macKey, canon []byte) []byte {
	m := hmac.New(sha256.New, macKey)
	m.Write([]byte(clientMsgLabel))
	m.Write([]byte{0})
	m.Write(canon)
	return m.Sum(nil)
}

// The kinds of client message the content distinguishes.
const (
	clientKindText    = "text"
	clientKindSkill   = "skill"
	clientKindPayment = "payment"
)

// clientContent is what a client message's canon covers (CUX-D1): what the
// message asks for, never how it was sent (messageId, contextId, taskId, a
// preview, return_immediately, history_length).
type clientContent struct {
	kind string
	// texts are the text of a text message after parseTaskInput, in order.
	texts []string
	// files are its attachments: name, media type and content id.
	files []delegation.Attachment
	// skill and args are a capability call's.
	skill string
	args  map[string]any
	// status and accept are a payment message's: x402.payment.status and
	// the option chosen (anet.payment.accept, absent on a rejection).
	status string
	accept any
	// meta is the client's message metadata, less the a2a.*, anet.* and
	// x402.* keys.
	meta map[string]any
	// ext are the extensions the client activated, less the preview one.
	ext []string
}

// inputContent is the content of a text message or a capability call.
func inputContent(in *taskInput, msg a2ashape.Message, ext []string) clientContent {
	c := clientContent{kind: clientKindText, texts: in.texts, files: in.atts, meta: msg.Metadata, ext: ext}
	if in.capID != "" {
		c.kind, c.skill, c.args = clientKindSkill, in.capID, in.args
	}
	return c
}

// paymentContent is the content of a payment message (CUX-D6): the decision
// and the option chosen.
func paymentContent(msg a2ashape.Message, ext []string) clientContent {
	status, _ := msg.Metadata[x402a2a.KeyStatus].(string)
	c := clientContent{kind: clientKindPayment, status: status, meta: msg.Metadata, ext: ext}
	if v, ok := msg.Metadata[x402a2a.KeyAccept]; ok && v != nil {
		c.accept = v
	}
	return c
}

// clientCanon is the canonical bytes of c (CUX-D1, core L38): a JSON object
// with RFC 8785's member order and string escaping and no whitespace, whose
// numbers are the client's own literals (json.Number), never re-encoded as
// doubles — a double would make 9007199254740993 and 9007199254740992 the
// same content, and merge two different requests. Every member of a kind is
// always present, empty or not:
//
//	text     {"ext", "files", "kind", "meta", "texts", "v"}
//	skill    {"args", "ext", "files", "kind", "meta", "skill", "v"}
//	payment  {"ext", "kind", "meta", "v", "x402": {"accept"?, "status"}}
func clientCanon(c clientContent) ([]byte, error) {
	files := make([]any, 0, len(c.files))
	for _, f := range c.files {
		files = append(files, map[string]any{"name": f.Name, "mime": f.Mime, "cid": f.CID})
	}
	obj := map[string]any{
		"v":    json.Number("1"),
		"kind": c.kind,
		"meta": clientMetaOnly(c.meta),
		"ext":  clientExtensions(c.ext),
	}
	switch c.kind {
	case clientKindText:
		texts := make([]any, 0, len(c.texts))
		for _, t := range c.texts {
			texts = append(texts, t)
		}
		obj["texts"], obj["files"] = texts, files
	case clientKindSkill:
		args := c.args
		if args == nil {
			args = map[string]any{}
		}
		obj["skill"], obj["args"], obj["files"] = c.skill, args, files
	case clientKindPayment:
		x := map[string]any{"status": c.status}
		if c.accept != nil {
			x["accept"] = c.accept
		}
		obj["x402"] = x
	default:
		return nil, fmt.Errorf("anet: client message of kind %q", c.kind)
	}
	return canonJSON(obj)
}

// clientMetaOnly is the client's own metadata: m without the a2a.*, anet.*
// and x402.* keys, which say how the message was sent or what anet does with
// it, and are taken apart elsewhere (skill, args, the payment decision).
func clientMetaOnly(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if strings.HasPrefix(k, "a2a.") || strings.HasPrefix(k, "anet.") || strings.HasPrefix(k, "x402.") {
			continue
		}
		out[k] = v
	}
	return out
}

// clientExtensions are the activated extensions, sorted, each once, without
// the preview extension.
func clientExtensions(ext []string) []any {
	s := make([]string, 0, len(ext))
	for _, e := range ext {
		if e != previewExtensionURI {
			s = append(s, e)
		}
	}
	slices.Sort(s)
	s = slices.Compact(s)
	out := make([]any, len(s))
	for i, e := range s {
		out[i] = e
	}
	return out
}

// canonJSON writes v in the canonical form clientCanon describes. v is first
// read back through encoding/json with UseNumber, so any Go value encodes as
// its JSON would (an int as its digits, a json.Number as its literal), and
// only JSON's own values reach the writer.
func canonJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var plain any
	if err := dec.Decode(&plain); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := writeCanon(&b, plain); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanon(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(x.String())
	case string:
		writeCanonString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanon(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonString(b, k)
			b.WriteByte(':')
			if err := writeCanon(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("anet: canonical JSON of a %T", v)
	}
	return nil
}

// utf16Less orders two member names by their UTF-16 code units (RFC 8785
// §3.2.3), which differs from their UTF-8 bytes above U+FFFF.
func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	return slices.Compare(ua, ub) < 0
}

// writeCanonString writes s as RFC 8785 §3.2.2.2 does: the two-character
// escapes for ", \, backspace, tab, line feed, form feed and carriage
// return, \u00xx (lower case) for the other control characters, and every
// other character as itself in UTF-8.
func writeCanonString(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// clientMark is one client message's retry key: the key of its messageId
// and its canonical content, whose digest is taken under the mac_key of the
// task the message is stored on.
type clientMark struct {
	key   []byte
	canon []byte
}

// newClientMark is the retry key of a message with id messageID and content
// c; nil when the client named no id (such a message takes no part).
func newClientMark(messageID string, c clientContent) (*clientMark, error) {
	if messageID == "" {
		return nil, nil
	}
	canon, err := clientCanon(c)
	if err != nil {
		return nil, a2ashape.Errorf(a2ashape.ErrInvalidParams, "the message cannot be read: %v", err)
	}
	return &clientMark{key: clientKey(messageID), canon: canon}, nil
}

// stamp gives rec the mark's key and digest under task ix's mac_key, in the
// transaction that stores rec (and created ix): the message and its retry
// key are written together, or neither is. A nil mark leaves rec alone.
func (c *clientMark) stamp(tx *interactions.Tx, ix string, rec *interactions.MessageRecord) error {
	if c == nil {
		return nil
	}
	mk, err := tx.MACKey(ix)
	if err != nil {
		return err
	}
	defer clear(mk)
	rec.ClientKey, rec.ClientDigest = c.key, clientDigest(mk, c.canon)
	return nil
}

// clientVerdict is decideClientMessage's answer.
type clientVerdict int

const (
	// clientFresh: no earlier copy counts — a new task, or a new follow-up.
	clientFresh clientVerdict = iota
	// clientRetry: the same message again; answer with its task, send
	// nothing, and say so (anet.deduplicated).
	clientRetry
	// clientReusedOpen: the id of an open task's message, with other
	// content (message_id_reused.open). Nothing is sent or written.
	clientReusedOpen
	// clientReusedFollowUp: on a follow-up, the id of another message of
	// the task (message_id_reused.followup).
	clientReusedFollowUp
)

func (v clientVerdict) String() string {
	switch v {
	case clientFresh:
		return "fresh"
	case clientRetry:
		return "retry"
	case clientReusedOpen:
		return "reused.open"
	case clientReusedFollowUp:
		return "reused.followup"
	}
	return fmt.Sprintf("clientVerdict(%d)", int(v))
}

// clientPrior is the earlier message a client key found, as
// decideClientMessage reads it.
type clientPrior struct {
	// followUp: the new message names a task (the follow-up path).
	followUp bool
	// state, reason and stateAt are the task's: its state, the anet.reason
	// it ended with, and when its state was last written.
	state   interactions.State
	reason  string
	stateAt time.Time
	// digest is the stored digest; unanswered is the stored mark (CUX-D5).
	digest     []byte
	unanswered bool
}

// decideClientMessage is CUX-D3: what a message under a client id already
// used becomes. prior is the earlier copy (nil: none counts), newDigest the
// new message's digest under the same task's mac_key, now the store's clock.
// It reads nothing and writes nothing.
func decideClientMessage(prior *clientPrior, newDigest []byte, now time.Time) clientVerdict {
	if prior == nil {
		return clientFresh
	}
	same := hmac.Equal(prior.digest, newDigest)
	switch {
	case prior.followUp && same:
		return clientRetry
	case prior.followUp:
		return clientReusedFollowUp
	case !prior.state.IsTerminal() && same:
		return clientRetry
	case !prior.state.IsTerminal():
		return clientReusedOpen
	case same && mayHaveRun(prior.state, prior.reason) && prior.unanswered && now.Sub(prior.stateAt) <= clientRetryWindow:
		// CUX-D4: the call was cut off before its answer, the task may
		// have run, and it ended moments ago — the client is retrying the
		// call it does not know the outcome of.
		return clientRetry
	}
	// A finished task: the same id is a new request (an internal note item), so a client
	// that reuses ids is never answered with an old result.
	return clientFresh
}

// mayHaveRun reports whether a terminal task may have been carried out:
// completed, or failed for any reason but undeliverable (a message that
// never left this node, engine §2.8). rejected and canceled were not run.
func mayHaveRun(st interactions.State, reason string) bool {
	switch st {
	case interactions.StateCompleted:
		return true
	case interactions.StateFailed:
		return reason != a2ashape.ReasonUndeliverable
	}
	return false
}

// clientMatch is an earlier copy of a client message and what it makes of
// the new one.
type clientMatch struct {
	ix      *interactions.Interaction
	verdict clientVerdict
}

// judgeClientMessage finds the earlier copy of cm that q selects and decides
// what cm is (CUX-D2, D3). The caller holds the message's send lock. A task
// whose data key is gone is no copy at all.
func (d *Daemon) judgeClientMessage(cm *clientMark, q interactions.ClientKeyQuery) (clientMatch, error) {
	if cm == nil {
		return clientMatch{verdict: clientFresh}, nil
	}
	q.Key = cm.key
	hit, err := d.ix.FindByClientKey(q)
	if errors.Is(err, interactions.ErrNotFound) {
		return clientMatch{verdict: clientFresh}, nil
	}
	if err != nil {
		return clientMatch{}, err
	}
	mk, err := d.ix.MACKey(hit.Interaction.ID)
	if errors.Is(err, interactions.ErrContentGone) {
		return clientMatch{verdict: clientFresh}, nil
	}
	if err != nil {
		return clientMatch{}, err
	}
	digest := clientDigest(mk, cm.canon)
	clear(mk)
	ix := hit.Interaction
	reason, _ := decodeMeta([]byte(ix.ResultMeta))[a2ashape.KeyReason].(string)
	prior := &clientPrior{followUp: q.TaskID != "", state: ix.State, reason: reason,
		stateAt: time.UnixMilli(ix.StateAt), digest: hit.Digest, unanswered: hit.Unanswered}
	return clientMatch{ix: ix, verdict: decideClientMessage(prior, digest, time.UnixMilli(d.ix.NowMS()))}, nil
}

// reusedRefusal is the refusal of a message under an id already used for
// other content (CUX-A4, D7): .open on a new task, .followup on a follow-up.
func reusedRefusal(m clientMatch) error {
	key := "message_id_reused.open"
	if m.verdict == clientReusedFollowUp {
		key = "message_id_reused.followup"
	}
	ref := a2ashape.Refuse(key, a2ashape.Args{"task": m.ix.ID})
	ref.Task = &a2ashape.RefusalTask{ID: m.ix.ID, ContextID: m.ix.ContextID, State: a2ashape.StateOf(m.ix.State)}
	return ref
}

// answered is a send's client mark, kept to the end of the call: when the
// call's request ends before it is answered, the message is marked
// unanswered (CUX-D5), under the same send lock as the lookup.
type answered struct {
	lock func() func() // takes the message's send lock
	ix   string
	key  []byte
}

// noteUnanswered marks the message unanswered when ctx ended before the
// answer: the client disconnected or gave up. The mark is taken under the
// send lock, so a retry's lookup sees it or comes after it.
func (d *Daemon) noteUnanswered(a *answered) {
	if a == nil || a.ix == "" {
		return
	}
	unlock := a.lock()
	defer unlock()
	if err := d.ix.MarkClientUnanswered(a.ix, a.key); err != nil {
		d.logOnce("client-unanswered", "anet: %s: mark the client's message unanswered: %v", a.ix, err)
	}
}

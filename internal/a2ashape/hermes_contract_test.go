package a2ashape_test

// hermes_contract_test.go is the Hermes row of the A2A-DESIGN §17 contract
// table (plan 0014 U1, B5-03): Hermes' A2A client, pointed at this node's
// local A2A interface by `anet agents wire --a2a` (§13.1), must find the
// answer in what a blocking SendMessage returns.
//
// Hermes' plugin (hermes-agent plugins/platforms/a2a, commit b1bb9031) calls
// SendMessage once — no configuration, so the call blocks until the task is
// terminal or waits for input — and hands its model one string built from
// the response by tools._reply_text_from_result: the text of the first
// artifact that has any, else the text of status.message. It reads nothing
// else: not metadata, not history, not the state beyond a one-word header.
//
// The functions prefixed py port that path line by line, down to Python's
// json.dumps for data parts and str() for odd results, so the assertions
// below state exactly what the model reads. testdata/hermes_a2a_call.json
// holds the output of Hermes' own code on the same responses
// (testdata/hermes_golden.py, which runs the plugin with only its HTTP call
// stubbed); TestHermesPortMatchesHermes holds the port to it.
//
// The contract cases project tasks the way the A2A interface does
// (artifacts, inline files, safe names), run them through a2a-go as
// module/a2a does, wrap them in the JSON-RPC response a2a-go's server
// writes, and read them with the port. A case with a gap is a projection
// shape Hermes cannot read well; the test pins what Hermes gets today, and
// the gap text says what goes wrong. The projection is not changed here.
//
// TestHermesRequestShape checks the other direction (what Hermes sends, as
// a2a-go reads it), and TestHermesAsBackend what anet meets when Hermes is
// the provider-side backend of §11.6. docs/notes/0018 has the findings.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// ---------------------------------------------------------------------------
// Python values, as json.loads returns them.
//
// nil is None, bool is bool, pyInt and pyFloat are int and float, string is
// str, []any is list and *pyDict is dict.

// pyDict is a dict from json.loads: keys keep their first position, a
// repeated key its last value.
type pyDict struct {
	keys []string
	vals map[string]any
}

// get is dict.get(k, def): def only when the key is absent, not when its
// value is None.
func (d *pyDict) get(k string, def any) any {
	if v, ok := d.vals[k]; ok {
		return v
	}
	return def
}

func (d *pyDict) has(k string) bool {
	_, ok := d.vals[k]
	return ok
}

func newPyDict() *pyDict { return &pyDict{vals: map[string]any{}} }

// pyInt keeps an integer's digits: Python ints are unbounded.
type pyInt string

type pyFloat float64

// pyError is a Python exception: its message is str(e).
type pyError struct{ msg string }

func (e *pyError) Error() string { return e.msg }

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case pyInt:
		return "int"
	case pyFloat:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *pyDict:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

func pyNoAttr(v any, attr string) error {
	return &pyError{fmt.Sprintf("'%s' object has no attribute '%s'", pyTypeName(v), attr)}
}

// pyGet is v.get(k, def), which raises unless v is a dict.
func pyGet(v any, k string, def any) (any, error) {
	d, ok := v.(*pyDict)
	if !ok {
		return nil, pyNoAttr(v, "get")
	}
	return d.get(k, def), nil
}

// pyLoads is json.loads.
func pyLoads(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := pyDecode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("Extra data")
	}
	return v, nil
}

func pyDecode(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch x := tok.(type) {
	case json.Delim:
		switch x {
		case '{':
			d := newPyDict()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := pyDecode(dec)
				if err != nil {
					return nil, err
				}
				if !d.has(k) {
					d.keys = append(d.keys, k)
				}
				d.vals[k] = v
			}
			_, err := dec.Token()
			return d, err
		case '[':
			l := []any{}
			for dec.More() {
				v, err := pyDecode(dec)
				if err != nil {
					return nil, err
				}
				l = append(l, v)
			}
			_, err := dec.Token()
			return l, err
		}
		return nil, fmt.Errorf("unexpected %v", x)
	case json.Number:
		s := string(x)
		if strings.ContainsAny(s, ".eE") {
			// float(s); out of range is inf, as in Python.
			f, err := strconv.ParseFloat(s, 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, err
			}
			return pyFloat(f), nil
		}
		if s == "-0" {
			s = "0"
		}
		return pyInt(s), nil
	}
	return tok, nil // string, bool, nil
}

// pyTruthy is bool(v).
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case pyInt:
		return x != "0"
	case pyFloat:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *pyDict:
		return len(x.keys) > 0
	}
	return true
}

// pyOr is `a or b or ...`: the first truthy operand, else the last.
func pyOr(vs ...any) any {
	for _, v := range vs[:len(vs)-1] {
		if pyTruthy(v) {
			return v
		}
	}
	return vs[len(vs)-1]
}

// pyIter is `for x in v`.
func pyIter(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case *pyDict:
		out := make([]any, len(x.keys))
		for i, k := range x.keys {
			out[i] = k
		}
		return out, nil
	case string:
		var out []any
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &pyError{fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// pyIsSpace is str.isspace for one character: Go's unicode.IsSpace plus
// the four information separators Python also strips.
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyFloatRepr is float.__repr__ (the shortest round-tripping digits,
// exponent form below 1e-4 and from 1e16).
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	sign := ""
	if math.Signbit(f) {
		sign, f = "-", -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	mant, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	decpt := x + 1
	if decpt <= -4 || decpt > 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		return sign + m + fmt.Sprintf("e%+03d", decpt-1)
	}
	switch {
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	}
	return sign + digits[:decpt] + "." + digits[decpt:]
}

// pyDumps is json.dumps(v, ensure_ascii=False): ", " and ": " separators,
// keys in insertion order.
func pyDumps(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case pyInt:
		return string(x)
	case pyFloat:
		f := float64(x)
		switch {
		case math.IsInf(f, 1):
			return "Infinity"
		case math.IsInf(f, -1):
			return "-Infinity"
		case math.IsNaN(f):
			return "NaN"
		}
		return pyFloatRepr(f)
	case string:
		return pyJSONString(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyDumps(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyDict:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = pyJSONString(k) + ": " + pyDumps(x.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// pyJSONString is json's encode_basestring (ensure_ascii=False).
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyStr is str(v), which an f-string also uses.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyRepr is repr(v) for the values json.loads makes.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case pyInt:
		return string(x)
	case pyFloat:
		return pyFloatRepr(float64(x))
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		var b strings.Builder
		b.WriteString(q)
		for _, r := range x {
			switch {
			case r == '\\':
				b.WriteString(`\\`)
			case string(r) == q:
				b.WriteString(`\` + q)
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\r':
				b.WriteString(`\r`)
			case r == '\t':
				b.WriteString(`\t`)
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r == ' ' || unicode.IsPrint(r):
				b.WriteRune(r)
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		}
		b.WriteString(q)
		return b.String()
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyDict:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = pyRepr(k) + ": " + pyRepr(x.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// ---------------------------------------------------------------------------
// The port. Each function names the Hermes function it follows; the Python
// is quoted above each step.

const (
	pyStateInputRequired = "TASK_STATE_INPUT_REQUIRED" // protocol.STATE_INPUT_REQUIRED
)

// pyUnwrapSendMessageResponse is protocol.unwrap_send_message_response.
func pyUnwrapSendMessageResponse(result any) any {
	// if isinstance(result, dict):
	if d, ok := result.(*pyDict); ok {
		// if isinstance(result.get("task"), dict): return result["task"]
		if t, ok := d.get("task", nil).(*pyDict); ok {
			return t
		}
		// if isinstance(result.get("message"), dict): return result["message"]
		if m, ok := d.get("message", nil).(*pyDict); ok {
			return m
		}
	}
	return result
}

// pyFileNote is protocol._file_note.
func pyFileNote(fname, body, mtype string) string {
	// label = f"[file: {fname}]" if fname else "[file]"
	label := "[file]"
	if fname != "" {
		label = "[file: " + fname + "]"
	}
	// return f"{label} {body}" + (f" ({mtype})" if mtype else "")
	out := label + " " + body
	if mtype != "" {
		out += " (" + mtype + ")"
	}
	return out
}

// pyJSONOrStr is protocol._json_or_str. json.dumps cannot fail on what
// json.loads made, so default=str and the str() fallback never run.
func pyJSONOrStr(data any) string { return pyDumps(data) }

// pyExtractText is protocol.extract_text.
func pyExtractText(messageOrParams any) (string, error) {
	// msg = message_or_params.get("message", message_or_params)
	mp, ok := messageOrParams.(*pyDict)
	if !ok {
		return "", pyNoAttr(messageOrParams, "get")
	}
	msg := mp.get("message", mp)
	var chunks []string
	// for part in msg.get("parts", []) if isinstance(msg, dict) else []:
	var parts []any
	if md, ok := msg.(*pyDict); ok {
		var err error
		if parts, err = pyIter(md.get("parts", []any{})); err != nil {
			return "", err
		}
	}
	for _, p := range parts {
		// if not isinstance(part, dict): continue
		part, ok := p.(*pyDict)
		if !ok {
			continue
		}
		// if isinstance(txt := part.get("text"), str): chunks.append(txt)
		if txt, ok := part.get("text", nil).(string); ok {
			chunks = append(chunks, txt)
			continue
		}
		// elif isinstance(url := part.get("url"), str) and url:
		//     chunks.append(_file_note(part.get("filename") or part.get("name") or "", url,
		//                              part.get("mediaType") or part.get("mimeType") or ""))
		if url, ok := part.get("url", nil).(string); ok && url != "" {
			chunks = append(chunks, pyFileNote(pyStr(pyOr(part.get("filename", nil), part.get("name", nil), "")), url,
				pyStr(pyOr(part.get("mediaType", nil), part.get("mimeType", nil), ""))))
			continue
		}
		// elif isinstance(v03 := part.get("file"), dict) and isinstance(v03.get("fileWithUri"), str):
		//     chunks.append(_file_note(v03.get("name") or "", v03["fileWithUri"], v03.get("mimeType") or ""))
		if v03, ok := part.get("file", nil).(*pyDict); ok {
			if uri, ok := v03.get("fileWithUri", nil).(string); ok {
				chunks = append(chunks, pyFileNote(pyStr(pyOr(v03.get("name", nil), "")), uri,
					pyStr(pyOr(v03.get("mimeType", nil), ""))))
				continue
			}
		}
		// elif isinstance(part.get("raw"), str):
		//     chunks.append(_file_note(part.get("filename") or "", f"{len(part['raw'])} bytes base64-encoded",
		//                              part.get("mediaType") or ""))
		if raw, ok := part.get("raw", nil).(string); ok {
			chunks = append(chunks, pyFileNote(pyStr(pyOr(part.get("filename", nil), "")),
				fmt.Sprintf("%d bytes base64-encoded", utf8.RuneCountInString(raw)),
				pyStr(pyOr(part.get("mediaType", nil), ""))))
			continue
		}
		// elif (data := part.get("data")) is not None:
		//     chunks.append(f"[data ({part.get('mediaType') or 'application/json'})]\n{_json_or_str(data)}")
		if data := part.get("data", nil); data != nil {
			chunks = append(chunks, "[data ("+pyStr(pyOr(part.get("mediaType", nil), "application/json"))+")]\n"+pyJSONOrStr(data))
		}
	}
	// return "\n".join(chunks).strip()
	return pyStrip(strings.Join(chunks, "\n")), nil
}

// pyReplyTextFromResult is tools._reply_text_from_result.
func pyReplyTextFromResult(result any) (string, error) {
	// result = protocol.unwrap_send_message_response(result)
	result = pyUnwrapSendMessageResponse(result)
	// if not isinstance(result, dict): return str(result)
	d, ok := result.(*pyDict)
	if !ok {
		return pyStr(result), nil
	}
	// for artifact in result.get("artifacts", []) or []:
	arts, err := pyIter(pyOr(d.get("artifacts", []any{}), []any{}))
	if err != nil {
		return "", err
	}
	for _, artifact := range arts {
		// txt = protocol.extract_text(artifact); if txt: return txt
		txt, err := pyExtractText(artifact)
		if err != nil {
			return "", err
		}
		if txt != "" {
			return txt, nil
		}
	}
	// return protocol.extract_text((result.get("status", {}) or {}).get("message") or result)
	status := pyOr(d.get("status", newPyDict()), newPyDict())
	msg, err := pyGet(status, "message", nil)
	if err != nil {
		return "", err
	}
	return pyExtractText(pyOr(msg, d))
}

// pySendTask is tools._send_task from the parsed response on: the reply,
// the context it continues in and the task state. A ValueError is a
// pyValueError; any other exception is a *pyError.
func pySendTask(agentLabel, ctx string, resp any) (reply string, replyCtx, state any, err error) {
	d, ok := resp.(*pyDict)
	if !ok {
		// `"error" in resp` and resp.get on a non-object response; the
		// JSON-RPC binding never sends one.
		return "", nil, nil, pyNoAttr(resp, "get")
	}
	// if "error" in resp:
	//     raise ValueError(f"Peer '{agent_label}' returned an error: {resp['error'].get('message', resp['error'])}")
	if d.has("error") {
		e := d.get("error", nil)
		m, gerr := pyGet(e, "message", e)
		if gerr != nil {
			return "", nil, nil, gerr
		}
		return "", nil, nil, pyValueError{fmt.Sprintf("Peer '%s' returned an error: %s", agentLabel, pyStr(m))}
	}
	// payload = protocol.unwrap_send_message_response(resp.get("result", {}))
	payload := pyUnwrapSendMessageResponse(d.get("result", newPyDict()))
	// reply = _reply_text_from_result(payload)
	if reply, err = pyReplyTextFromResult(payload); err != nil {
		return "", nil, nil, err
	}
	// reply_ctx, state = ctx, ""
	replyCtx, state = ctx, ""
	// if isinstance(payload, dict):
	if pd, ok := payload.(*pyDict); ok {
		// reply_ctx = payload.get("contextId", ctx)
		replyCtx = pd.get("contextId", ctx)
		// state = (payload.get("status") or {}).get("state", "")
		if state, err = pyGet(pyOr(pd.get("status", nil), newPyDict()), "state", ""); err != nil {
			return "", nil, nil, err
		}
	}
	return reply, replyCtx, state, nil
}

// pyValueError is the ValueError _send_task raises for a JSON-RPC error.
type pyValueError struct{ msg string }

func (e pyValueError) Error() string { return e.msg }

// pyHTTPCallErrors is tools._HTTP_CALL_ERRORS.
var pyHTTPCallErrors = map[int]string{
	401: "Error: peer '{agent}' rejected auth (HTTP {code}). Check the configured token.",
	403: "Error: peer '{agent}' rejected auth (HTTP {code}). Check the configured token.",
	429: "Error: peer '{agent}' rate limited us (HTTP 429). Retry later.",
}

// pyA2ACall is tools.a2a_call after the peer is resolved: what the model
// reads. httpStatus is the status of a non-2xx answer (urllib's
// HTTPError), transportErr the message of any other failure to get one.
func pyA2ACall(agent, ctx string, httpStatus int, transportErr string, body []byte) string {
	// except urllib.error.HTTPError as e:
	//     return _HTTP_CALL_ERRORS.get(e.code, "Error: call to '{agent}' failed — HTTP {code}.").format(agent=agent, code=e.code)
	if httpStatus != 0 {
		f, ok := pyHTTPCallErrors[httpStatus]
		if !ok {
			f = "Error: call to '{agent}' failed — HTTP {code}."
		}
		return strings.NewReplacer("{agent}", agent, "{code}", strconv.Itoa(httpStatus)).Replace(f)
	}
	fail := func(e string) string { return fmt.Sprintf("Error: call to '%s' failed — %s.", agent, e) }
	if transportErr != "" {
		return fail(transportErr)
	}
	resp, err := pyLoads(body)
	if err != nil {
		return fail(err.Error())
	}
	// reply, reply_ctx, state = _send_task(agent, peer, message, context_id)
	reply, replyCtx, state, err := pySendTask(agent, ctx, resp)
	var ve pyValueError
	switch {
	case errors.As(err, &ve):
		// except ValueError as e: return str(e)
		return ve.msg
	case err != nil:
		// except Exception as e: return f"Error: call to '{agent}' failed — {e}."
		return fail(err.Error())
	}
	st, ok := state.(string)
	if !ok {
		return fail(pyNoAttr(state, "replace").Error())
	}
	// short_state = state.replace("TASK_STATE_", "").replace("_", "-").lower()
	shortState := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(st, "TASK_STATE_", ""), "_", "-"))
	// header = f"[{agent} · context {reply_ctx}" + (f" · {short_state}" if state else "") + "]"
	header := "[" + agent + " · context " + pyStr(replyCtx)
	if st != "" {
		header += " · " + shortState
	}
	header += "]"
	// body = reply or "(no text reply)"
	out := reply
	if out == "" {
		out = "(no text reply)"
	}
	// if state == protocol.STATE_INPUT_REQUIRED:
	//     body += f"\n\n(The peer needs more input — answer by calling a2a_call again with context_id '{reply_ctx}'.)"
	if st == pyStateInputRequired {
		out += fmt.Sprintf("\n\n(The peer needs more input — answer by calling a2a_call again with context_id '%s'.)", pyStr(replyCtx))
	}
	// return f"{header}\n{body}"
	return header + "\n" + out
}

// hermesRead is what Hermes reads from one SendMessage response body.
func hermesRead(t *testing.T, body []byte) (reply, ctx, state string) {
	t.Helper()
	resp, err := pyLoads(body)
	must(t, err)
	r, c, s, err := pySendTask(hermesAgent, hermesCtx, resp)
	must(t, err)
	return r, pyStr(c), pyStr(s)
}

// ---------------------------------------------------------------------------
// The contract.

const (
	// hermesAgent is the a2a_agents key anet writes: the remote agent's AID.
	hermesAgent = "aidpeer"
	// hermesCtx has the form of the context id Hermes mints for a new
	// conversation (protocol.new_context_id), stored as given (C22).
	hermesCtx = "ctx-5f1e2d3c4b5a6978"
	// hermesRPCID has the form of Hermes' JSON-RPC id (protocol.new_task_id).
	hermesRPCID = "task-0123456789abcdef"
	// hermesClientMsgID has the form of Hermes' messageId (uuid4().hex).
	hermesClientMsgID = "9f2c1e0b7a6d4c3b8e5f1a2b3c4d5e6f"
)

// hermesOpts is how the A2A interface projects a task it returns (the
// daemon's taskseam: artifacts, attachment bytes inline, safe names, the
// provider KEL beside the receipt).
var hermesOpts = a2ashape.Options{Artifacts: true, InlineFiles: true, SafeName: safe, ProviderKEL: "a2VsCg=="}

// sendMessageResponse is the body module/a2a answers a blocking
// SendMessage with: the projection, read and written by a2a-go (contract),
// as the StreamResponse a2a-go's JSON-RPC server puts in "result"
// (a2asrv/jsonrpc.go), encoded by its json.Encoder.
func sendMessageResponse(t *testing.T, task a2ashape.Task) []byte {
	t.Helper()
	sdk := contract(t, task)
	var buf bytes.Buffer
	must(t, json.NewEncoder(&buf).Encode(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Result  any    `json:"result,omitempty"`
	}{"2.0", hermesRPCID, a2a.StreamResponse{Event: sdk}}))
	return buf.Bytes()
}

// hermesTextTask opens an outbound text task the way a Hermes SendMessage
// does: the client's context id kept, its messageId in the metadata.
func hermesTextTask(t *testing.T, st *interactions.Store, ix, goal string) {
	t.Helper()
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: goal,
		RequestCID: "bafyrequest", ContextID: hermesCtx, TaskNonce: "n"}))
	msg(t, st, ix, self, interactions.MsgText, goal, "msg_1", map[string]any{a2ashape.KeyMessageID: hermesClientMsgID})
	setState(t, st, ix, interactions.StateWorking)
}

// finishText ends a text task with a receipt over the given transcript.
func finishText(t *testing.T, st *interactions.Store, ix string, state interactions.State, turns []transcript.Message, meta string) {
	t.Helper()
	tr, err := transcript.EncodeV2("n", turns)
	must(t, err)
	var mb []byte
	if meta != "" {
		mb = []byte(meta)
	}
	must(t, st.Finish(ix, interactions.Finish{State: state, Result: tr, ResultCID: "bafyresult",
		Receipt: receipt(t, ix, self, peer, "bafyresult"), Verified: interactions.VerificationVerified, Meta: mb}))
}

// hermesCapTask is capTask with Hermes' context id.
func hermesCapTask(t *testing.T, st *interactions.Store, ix string, state interactions.State, deliverable, meta string) {
	t.Helper()
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "invoke capability cas.put",
		RequestCID: "bafyrequest", RequestDoc: capDoc(t, "cas.put", `{"key":"k"}`), ContextID: hermesCtx, IsCapability: true}))
	msg(t, st, ix, self, interactions.MsgText, `{"skill":"cas.put","args":{"key":"k"}}`, "msg_req",
		map[string]any{a2ashape.KeyMessageID: hermesClientMsgID})
	if deliverable == "" {
		return
	}
	var mb []byte
	if meta != "" {
		mb = []byte(meta)
	}
	must(t, st.Finish(ix, interactions.Finish{State: state, Result: []byte(deliverable), ResultCID: "bafyresult",
		Receipt: receipt(t, ix, self, peer, "bafyresult"), Verified: interactions.VerificationVerified, Meta: mb}))
}

const hermesQuote = `{"x402Version":2,"accepts":[{"scheme":"anet-credit","network":"hub:did:anet:h","amount":"5","asset":"credit","payTo":"did:anet:peer","maxTimeoutSeconds":600}]}`

// hermesReceiptText reports a reply that is the anet.receipt artifact
// rendered as a data part: evidence, not an answer.
func hermesReceiptText(reply string) bool {
	return strings.HasPrefix(reply, "[data (application/json)]\n{\"completed_at\": ") && strings.Contains(reply, `"receipt": "`)
}

type hermesCase struct {
	name  string
	task  func(t *testing.T) a2ashape.Task
	state string // Hermes' short state in the header
	// reply is the text Hermes reads, exactly; check replaces it where the
	// text is long.
	reply string
	check func(reply string) bool
	// gap, when set, says why this shape does not give Hermes' model a
	// usable answer. The case pins today's text.
	gap string
}

func hermesCases() []hermesCase {
	quote := func(src *a2ashape.Source) {
		src.Interaction.PayState = interactions.PayRequired
		src.Interaction.PayRequired = []byte(hermesQuote)
	}
	return []hermesCase{
		{name: "text completed", state: "completed", reply: "waves fold\nsalt remembers\nthe shore",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "write a haiku")
				msg(t, st, "ix", peer, interactions.MsgText, "waves fold\nsalt remembers\nthe shore", "msg_2", nil)
				finishText(t, st, "ix", interactions.StateCompleted, []transcript.Message{
					{From: "requester", Body: "write a haiku"}, {From: "provider", Body: "waves fold\nsalt remembers\nthe shore"}},
					`{"anet.state":"completed"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text completed, receipt unverified", state: "completed", reply: "ok",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "g")
				tr, _ := transcript.EncodeV2("n", []transcript.Message{{From: "requester", Body: "g"}, {From: "provider", Body: "ok"}})
				must(t, st.Finish("ix", interactions.Finish{State: interactions.StateCompleted, Result: tr, ResultCID: "bafyresult",
					Receipt: receipt(t, "ix", self, peer, "bafyresult"), Verified: interactions.VerificationUnverified}))
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text completed, reply text and a file", state: "completed", reply: "here it is",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "draw a cat")
				seq := msg(t, st, "ix", peer, interactions.MsgText, "here it is", "msg_2", nil)
				must(t, st.AddAttachment("ix", seq, interactions.Attachment{Name: "cat.png", Mime: "image/png", Size: 2, CID: "bafkcat", Data: []byte{9, 9}}))
				finishText(t, st, "ix", interactions.StateCompleted, []transcript.Message{
					{From: "requester", Body: "draw a cat"},
					{From: "provider", Body: "here it is", Attachments: []transcript.Attachment{{Name: "cat.png", Mime: "image/png", Size: 2, CID: "bafkcat"}}}}, "")
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text completed, reply is only a file", state: "completed", check: hermesReceiptText,
			gap: "anet.reply is an empty text part and the file is a later artifact, so Hermes reads the next artifact with text: anet.receipt",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "draw a cat")
				seq := msg(t, st, "ix", peer, interactions.MsgText, "", "msg_2", nil)
				must(t, st.AddAttachment("ix", seq, interactions.Attachment{Name: "cat.png", Mime: "image/png", Size: 2, CID: "bafkcat", Data: []byte{9, 9}}))
				finishText(t, st, "ix", interactions.StateCompleted, []transcript.Message{
					{From: "requester", Body: "draw a cat"},
					{From: "provider", Attachments: []transcript.Attachment{{Name: "cat.png", Mime: "image/png", Size: 2, CID: "bafkcat"}}}}, "")
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text completed, provider never spoke", state: "completed", check: hermesReceiptText,
			gap: "no provider turn means no anet.reply, so the first artifact is anet.receipt and Hermes reads the receipt as the answer",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "ping")
				msg(t, st, "ix", self, interactions.MsgEndRequest, "", "msg_2", nil)
				finishText(t, st, "ix", interactions.StateCompleted, []transcript.Message{{From: "requester", Body: "ping"}}, "")
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text failed with receipt", state: "failed", reply: "I cannot do that: out of scope",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "hack the planet")
				msg(t, st, "ix", peer, interactions.MsgText, "I cannot do that: out of scope", "msg_2", nil)
				finishText(t, st, "ix", interactions.StateFailed, []transcript.Message{
					{From: "requester", Body: "hack the planet"}, {From: "provider", Body: "I cannot do that: out of scope"}},
					`{"anet.state":"failed"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text failed without receipt", state: "failed", reply: "provider error: model quota exhausted",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "g")
				must(t, st.SetFailed("ix", []byte("provider error: model quota exhausted")))
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text rejected by inbound policy", state: "rejected", reply: "this node did not accept the task: not_allowed",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "g")
				msg(t, st, "ix", peer, interactions.MsgStatus, "this node did not accept the task: not_allowed", "st_1",
					map[string]any{a2ashape.KeyReason: "not_allowed"})
				setState(t, st, "ix", interactions.StateRejected)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text rejected, sandbox unavailable", state: "rejected",
			reply: "this node runs its agent for untrusted peers only inside a sandbox, and the sandbox is unavailable",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "g")
				msg(t, st, "ix", peer, interactions.MsgStatus,
					"this node runs its agent for untrusted peers only inside a sandbox, and the sandbox is unavailable", "st_1",
					map[string]any{a2ashape.KeyReason: "sandbox_unavailable"})
				setState(t, st, "ix", interactions.StateRejected)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text canceled, nothing said", state: "canceled", reply: "",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "g")
				msg(t, st, "ix", self, interactions.MsgCancel, "", "msg_2", nil)
				setState(t, st, "ix", interactions.StateCanceled)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text input-required, question", state: "input-required", reply: "about what?",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "write a haiku")
				msg(t, st, "ix", peer, interactions.MsgText, "about what?", "msg_2", nil)
				setState(t, st, "ix", interactions.StateInputRequired)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text input-required, question with a file", state: "input-required",
			reply: "like this?\n[file: safe-cat.png] 4 bytes base64-encoded (image/png)",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "draw a cat")
				seq := msg(t, st, "ix", peer, interactions.MsgText, "like this?", "msg_2", nil)
				must(t, st.AddAttachment("ix", seq, interactions.Attachment{Name: "cat.png", Mime: "image/png", Size: 2, CID: "bafkcat", Data: []byte{9, 9}}))
				setState(t, st, "ix", interactions.StateInputRequired)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "text input-required, payment message with text", state: "input-required", reply: "this costs 5 credits",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesTextTask(t, st, "ix", "g")
				msg(t, st, "ix", peer, interactions.MsgPayment, "this costs 5 credits", "msg_2", map[string]any{
					a2ashape.KeyX402Status: a2ashape.PaymentRequired, a2ashape.KeyX402Required: json.RawMessage(hermesQuote)})
				setState(t, st, "ix", interactions.StateInputRequired)
				src, err := a2ashape.Load(st, "ix")
				must(t, err)
				quote(&src)
				return a2ashape.Project(src, hermesOpts)
			}},
		{name: "capability input-required, payment message", state: "input-required", reply: "",
			gap: "a capability call keeps only a payment message's metadata, so status.message is one empty text part: Hermes reads no text, and no amount",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", "", "", "")
				msg(t, st, "ix", peer, interactions.MsgPayment, "", "msg_2", map[string]any{
					a2ashape.KeyX402Status: a2ashape.PaymentRequired, a2ashape.KeyX402Required: json.RawMessage(hermesQuote)})
				setState(t, st, "ix", interactions.StateInputRequired)
				src, err := a2ashape.Load(st, "ix")
				must(t, err)
				quote(&src)
				return a2ashape.Project(src, hermesOpts)
			}},
		{name: "capability input-required, quote with a message", state: "input-required", reply: "costs 5",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", interactions.StateInputRequired,
					`{"capability":"cas.put","status":"PAYMENT_REQUIRED","message":"costs 5","payment_required":`+hermesQuote+`}`,
					`{"anet.effect_status":"PAYMENT_REQUIRED","anet.state":"input-required"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "capability input-required, quote without a message", state: "input-required", reply: "Payment is required.",
			gap: "the synthesized text names no amount, asset or payee (those are only in x402.payment.required, which Hermes does not read)",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", interactions.StateInputRequired,
					`{"capability":"cas.put","status":"PAYMENT_REQUIRED","payment_required":`+hermesQuote+`}`,
					`{"anet.effect_status":"PAYMENT_REQUIRED","anet.state":"input-required"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "capability completed", state: "completed",
			reply: "[data (application/json)]\n" +
				`{"capability": "cas.put", "nonce": "n", "status": "OK", "value": {"etag": "e1", "size": 3}, "verifiable": true}`,
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", interactions.StateCompleted,
					`{"capability":"cas.put","status":"OK","nonce":"n","verifiable":true,"value":{"size":3,"etag":"e1"}}`,
					`{"anet.effect_status":"OK","anet.state":"completed"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "capability completed, effect unverified", state: "completed",
			reply: "[data (application/json)]\n" +
				`{"capability": "cas.put", "message": "sent, no readback", "status": "UNVERIFIED", "verifiable": false}`,
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", interactions.StateCompleted,
					`{"capability":"cas.put","status":"UNVERIFIED","verifiable":false,"message":"sent, no readback"}`,
					`{"anet.effect_status":"UNVERIFIED","anet.state":"completed"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "capability failed", state: "failed",
			reply: "[data (application/json)]\n" + `{"capability": "cas.put", "message": "boom", "status": "FAILED"}`,
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", interactions.StateFailed, `{"capability":"cas.put","status":"FAILED","message":"boom"}`,
					`{"anet.effect_status":"FAILED","anet.state":"failed"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "capability rejected, busy", state: "rejected",
			reply: "[data (application/json)]\n" + `{"capability": "cas.put", "message": "busy", "status": "UNAVAILABLE"}`,
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", interactions.StateRejected, `{"capability":"cas.put","status":"UNAVAILABLE","message":"busy"}`,
					`{"anet.effect_status":"UNAVAILABLE","anet.retry_after_ms":60000,"anet.state":"rejected"}`)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
		{name: "capability rejected by policy", state: "rejected", reply: "this node did not accept the task: denied",
			task: func(t *testing.T) a2ashape.Task {
				st := openStore(t)
				hermesCapTask(t, st, "ix", "", "", "")
				msg(t, st, "ix", peer, interactions.MsgStatus, "this node did not accept the task: denied", "st_1",
					map[string]any{a2ashape.KeyReason: "denied"})
				setState(t, st, "ix", interactions.StateRejected)
				task, err := a2ashape.ProjectStored(st, "ix", hermesOpts)
				must(t, err)
				return task
			}},
	}
}

// hermesDumpCase is one input of testdata/hermes_golden.py.
type hermesDumpCase struct {
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	ContextID      string `json:"context_id,omitempty"`
	Response       string `json:"response"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	TransportError string `json:"transport_error,omitempty"`
}

// hermesEdgeCases are responses no projection produces, for holding the
// port to Hermes where the contract cases do not reach: older wire forms,
// odd results, every part kind, errors.
func hermesEdgeCases() []hermesDumpCase {
	r := func(name, ctx, result string) hermesDumpCase {
		return hermesDumpCase{Name: "edge: " + name, Agent: hermesAgent, ContextID: ctx,
			Response: `{"jsonrpc":"2.0","id":"x","result":` + result + `}`}
	}
	return []hermesDumpCase{
		r("legacy bare task, v0.3 parts", hermesCtx,
			`{"id":"t","contextId":"c03","kind":"task","status":{"state":"completed"},"artifacts":[{"artifactId":"a","parts":[{"kind":"text","text":"v0.3 reply"}]}]}`),
		r("message result", hermesCtx, `{"message":{"role":"ROLE_AGENT","messageId":"m","parts":[{"text":"direct"}]}}`),
		r("every part kind", hermesCtx, `{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"},"artifacts":[{"artifactId":"a","parts":[`+
			`{"text":"a"},{"text":null},"not a part",{"url":"https://x/y.pdf","filename":"y.pdf","mediaType":"application/pdf"},`+
			`{"url":"https://x/z","name":"z"},{"url":""},{"kind":"file","file":{"name":"f.txt","mimeType":"text/plain","fileWithUri":"https://x/f.txt"}},`+
			`{"file":{"bytes":"AAAA"}},{"raw":"AAECAwQ=","filename":"b.bin","mediaType":"application/octet-stream"},{"raw":"","mediaType":""},`+
			`{"data":{"é":"\u2028<>&\n\"\\\u0001","f":[1.5,1e16,1e15,1e-5,0.0001,-0.0,100,1E2,1e400,-7],"b":[true,false,null],"z":{}},"mediaType":"application/vnd.x+json"},`+
			`{"data":[]},{"data":0},{"data":null}]}]}}`),
		r("empty artifact text, then text", hermesCtx, `{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"},`+
			`"artifacts":[{"artifactId":"a","parts":[{"text":" \n\t"}]},{"artifactId":"b","parts":[{"text":"second"}]}]}}`),
		r("no artifacts, null status message", hermesCtx, `{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_FAILED","message":null}}}`),
		r("null result", hermesCtx, `null`),
		r("list result", hermesCtx, `["a",1,2.5,true,null,{"k":"v","q":"it's"}]`),
		r("v0.3 input-required", hermesCtx, `{"task":{"id":"t","contextId":"c2","status":{"state":"input-required","message":{"parts":[{"kind":"text","text":"which?"}]}}}}`),
		r("python whitespace", hermesCtx, `{"message":{"parts":[{"text":"\u3000 hi \u001c\u0085"}]}}`),
		r("repeated key", hermesCtx, `{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"},`+
			`"artifacts":[{"artifactId":"a","parts":[{"text":"first"}]}],"artifacts":[{"artifactId":"b","parts":[{"text":"second"}]}]}}`),
		r("artifacts not a list", hermesCtx, `{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"},"artifacts":{"parts":[]}}}`),
		r("null parts", hermesCtx, `{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_FAILED","message":{"parts":null}}}}`),
		r("no context sent", "", `{"task":{"id":"t","contextId":"server-ctx","status":{"state":"TASK_STATE_COMPLETED"},"artifacts":[{"artifactId":"a","parts":[{"text":"hi"}]}]}}`),
		{Name: "edge: jsonrpc error", Agent: hermesAgent, ContextID: hermesCtx,
			Response: `{"jsonrpc":"2.0","id":"x","error":{"code":-32001,"message":"Task not found"}}`},
		{Name: "edge: jsonrpc error without message", Agent: hermesAgent, ContextID: hermesCtx,
			Response: `{"jsonrpc":"2.0","id":"x","error":{"code":-32603,"data":[1.0,"x"]}}`},
		{Name: "edge: http 401", Agent: hermesAgent, ContextID: hermesCtx, HTTPStatus: 401},
		{Name: "edge: http 429", Agent: hermesAgent, ContextID: hermesCtx, HTTPStatus: 429},
		{Name: "edge: http 404", Agent: hermesAgent, ContextID: hermesCtx, HTTPStatus: 404},
		{Name: "edge: read timeout", Agent: hermesAgent, ContextID: hermesCtx, TransportError: "timed out"},
	}
}

// TestHermesContract is the §17 contract row: every Task shape a blocking
// SendMessage returns gives Hermes' model an answer, except the pinned gaps.
func TestHermesContract(t *testing.T) {
	var dump []hermesDumpCase
	var gaps []string
	for _, c := range hermesCases() {
		t.Run(c.name, func(t *testing.T) {
			body := sendMessageResponse(t, c.task(t))
			dump = append(dump, hermesDumpCase{Name: c.name, Agent: hermesAgent, ContextID: hermesCtx, Response: string(body)})
			reply, ctx, state := hermesRead(t, body)
			if ctx != hermesCtx {
				t.Errorf("Hermes continues in context %q, want the one it sent, %q", ctx, hermesCtx)
			}
			short := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(state, "TASK_STATE_"), "_", "-"))
			if short != c.state {
				t.Errorf("state %q, want %q", state, c.state)
			}
			if c.check != nil {
				if !c.check(reply) {
					t.Errorf("Hermes reads %q", reply)
				}
			} else if reply != c.reply {
				t.Errorf("Hermes reads\n %q\nwant\n %q", reply, c.reply)
			}
			if c.gap != "" {
				gaps = append(gaps, c.name+": "+c.gap)
			} else if reply == "" && c.state != "canceled" {
				t.Errorf("no text for a %s task and no gap recorded", c.state)
			}
			out := pyA2ACall(hermesAgent, hermesCtx, 0, "", body)
			if want := "[" + hermesAgent + " · context " + hermesCtx + " · " + c.state + "]\n"; !strings.HasPrefix(out, want) {
				t.Errorf("a2a_call output %q", out)
			}
		})
	}
	for _, g := range gaps {
		t.Log("gap: " + g)
	}
	if path := os.Getenv("ANET_HERMES_DUMP"); path != "" {
		b, err := json.MarshalIndent(map[string]any{"cases": append(dump, hermesEdgeCases()...)}, "", " ")
		must(t, err)
		must(t, os.WriteFile(path, b, 0o644))
		t.Logf("wrote %d cases to %s", len(dump)+len(hermesEdgeCases()), path)
	}
}

// hermesGolden is testdata/hermes_a2a_call.json.
type hermesGolden struct {
	Commit string `json:"hermes_plugin_commit"`
	// Cards is the Agent Card Hermes serves, without and with a token.
	Cards map[string]json.RawMessage `json:"cards"`
	// ProviderResponses is what Hermes' server answers SendMessage with.
	ProviderResponses map[string]json.RawMessage `json:"provider_responses"`
	Cases             []struct {
		hermesDumpCase
		Output  string `json:"output"`
		Request *struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout int               `json:"timeout"`
			Body    json.RawMessage   `json:"body"`
		} `json:"request"`
		CardRequest *struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout int               `json:"timeout"`
		} `json:"card_request"`
	} `json:"cases"`
}

func loadHermesGolden(t *testing.T) hermesGolden {
	t.Helper()
	b, err := os.ReadFile("testdata/hermes_a2a_call.json")
	must(t, err)
	var g hermesGolden
	must(t, json.Unmarshal(b, &g))
	if len(g.Cases) == 0 {
		t.Fatal("no cases in testdata/hermes_a2a_call.json")
	}
	return g
}

// TestHermesPortMatchesHermes holds the port to Hermes' own output on the
// recorded responses, and checks that every current case was recorded.
func TestHermesPortMatchesHermes(t *testing.T) {
	g := loadHermesGolden(t)
	recorded := map[string]bool{}
	for _, c := range g.Cases {
		recorded[c.Name] = true
		got := pyA2ACall(c.Agent, c.ContextID, c.HTTPStatus, c.TransportError, []byte(c.Response))
		if got != c.Output {
			t.Errorf("%s:\nport   %q\nhermes %q", c.Name, got, c.Output)
		}
	}
	var missing []string
	for _, c := range hermesCases() {
		if !recorded[c.name] {
			missing = append(missing, c.name)
		}
	}
	for _, c := range hermesEdgeCases() {
		if !recorded[c.Name] {
			missing = append(missing, c.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("not recorded from Hermes (regenerate as testdata/hermes_golden.py says): %q", missing)
	}
}

// TestHermesRequestShape checks what Hermes sends against what the A2A
// interface reads: the card fetched with the bearer token, SendMessage
// posted to the card's JSON-RPC interface with A2A-Version 1.0, a message
// a2a-go reads with no taskId and no configuration — so a new task, and a
// blocking call — and a text part the projection types read back.
func TestHermesRequestShape(t *testing.T) {
	g := loadHermesGolden(t)
	base := "http://127.0.0.1:47100/a2a/v1/agents/" + hermesAgent
	generated := regexp.MustCompile(`^ctx-[0-9a-f]{16}$`)
	n := 0
	for _, c := range g.Cases {
		if c.Request == nil {
			continue
		}
		n++
		if c.CardRequest == nil || c.CardRequest.URL != base+"/.well-known/agent-card.json" ||
			c.CardRequest.Headers["Authorization"] != "Bearer a2a-local-token" || c.CardRequest.Timeout != 30 {
			t.Fatalf("%s: card request %+v", c.Name, c.CardRequest)
		}
		r := c.Request
		if r.URL != base+"/jsonrpc" || r.Timeout != 3600 || r.Headers["A2A-Version"] != "1.0" ||
			r.Headers["Content-Type"] != "application/json" || r.Headers["Authorization"] != "Bearer a2a-local-token" {
			t.Fatalf("%s: request %s %d %v", c.Name, r.URL, r.Timeout, r.Headers)
		}
		var rpc struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		must(t, json.Unmarshal(r.Body, &rpc))
		if rpc.JSONRPC != "2.0" || rpc.Method != "SendMessage" {
			t.Fatalf("%s: %s %s", c.Name, rpc.JSONRPC, rpc.Method)
		}
		var req a2a.SendMessageRequest
		must(t, json.Unmarshal(rpc.Params, &req))
		m := req.Message
		if req.Config != nil || req.Tenant != "" || m == nil || m.TaskID != "" || m.Role != a2a.MessageRoleUser || len(m.Parts) != 1 ||
			m.Parts[0].Text() != "hello" || m.Parts[0].MediaType != "text/plain" {
			t.Fatalf("%s: a2a-go reads %+v / %+v", c.Name, req, m)
		}
		switch {
		case c.ContextID != "" && m.ContextID != c.ContextID:
			t.Fatalf("%s: contextId %q, want %q", c.Name, m.ContextID, c.ContextID)
		case c.ContextID == "" && !generated.MatchString(m.ContextID):
			t.Fatalf("%s: generated contextId %q", c.Name, m.ContextID)
		}
		// module/a2a converts by a JSON round trip into the projection's
		// types.
		var shaped a2ashape.Message
		must(t, json.Unmarshal(jsonOf(t, m), &shaped))
		if shaped.Role != a2ashape.RoleUser || shaped.Parts[0].Kind != a2ashape.PartText || shaped.ContextID != m.ContextID {
			t.Fatalf("%s: projection types read %+v", c.Name, shaped)
		}
	}
	if n == 0 {
		t.Fatal("no recorded request")
	}
}

// TestHermesAsBackend records what the provider-side backend (§11.6) meets
// when Hermes is the backend and anet talks to it with a2a-go: every
// answer Hermes' server gives parses, but its Agent Card does not once a
// token is configured — Hermes writes the pre-1.0 security scheme
// ({"type":"http","scheme":"bearer"}), which a2a-go v2 refuses. The
// backend therefore posts to the configured URL and does not resolve
// Hermes' card.
func TestHermesAsBackend(t *testing.T) {
	g := loadHermesGolden(t)
	var card a2a.AgentCard
	must(t, json.Unmarshal(g.Cards["no_token"], &card))
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC {
		t.Fatalf("no-token card %+v", card)
	}
	if err := json.Unmarshal(g.Cards["token"], &a2a.AgentCard{}); err == nil || !strings.Contains(err.Error(), "unknown security scheme") {
		t.Fatalf("a2a-go reads Hermes' card with a token: err=%v", err)
	}
	want := map[string]struct {
		state a2a.TaskState
		text  string
	}{
		"completed":              {a2a.TaskStateCompleted, "the answer"},
		"completed, empty reply": {a2a.TaskStateCompleted, ""},
		"input-required":         {a2a.TaskStateInputRequired, "which city?"},
		"failed, reply timeout":  {a2a.TaskStateFailed, "[agent did not reply in time]"},
		"rejected, anti-loop":    {a2a.TaskStateRejected, "Anti-loop protection"},
	}
	if len(g.ProviderResponses) != len(want) {
		t.Fatalf("%d provider responses recorded", len(g.ProviderResponses))
	}
	for name, raw := range g.ProviderResponses {
		var resp struct {
			Result a2a.StreamResponse `json:"result"`
		}
		must(t, json.Unmarshal(raw, &resp))
		task, ok := resp.Result.Event.(*a2a.Task)
		w := want[name]
		if !ok || task.Status.State != w.state || task.ContextID != "ctx-anet" {
			t.Fatalf("%s: a2a-go reads %T %+v", name, resp.Result.Event, resp.Result.Event)
		}
		// The answer is in status.message (and, when completed, in one
		// text artifact too); an empty completed reply has neither.
		var text string
		if m := task.Status.Message; m != nil {
			text = m.Parts[0].Text()
		}
		if !strings.HasPrefix(text, w.text) || (w.text == "") != (task.Status.Message == nil) {
			t.Fatalf("%s: status message %+v", name, task.Status.Message)
		}
		if completed := w.state == a2a.TaskStateCompleted && w.text != ""; completed != (len(task.Artifacts) == 1) {
			t.Fatalf("%s: artifacts %+v", name, task.Artifacts)
		}
	}
}

// The port's Python pieces, on values where Python's answer is known.
func TestHermesPyPieces(t *testing.T) {
	for f, want := range map[float64]string{1.5: "1.5", 1e16: "1e+16", 1e15: "1000000000000000.0", 1e-5: "1e-05",
		0.0001: "0.0001", 100: "100.0", 0: "0.0", 1.5e-7: "1.5e-07", 1e100: "1e+100", 123456.789: "123456.789",
		0.1: "0.1", 2.5e22: "2.5e+22"} {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("repr(%v) = %s, want %s", f, got, want)
		}
	}
	if got := pyFloatRepr(math.Copysign(0, -1)); got != "-0.0" {
		t.Errorf("repr(-0.0) = %s", got)
	}
	v, err := pyLoads([]byte(`{"b":1,"a":[1.0,"x\u0001y",null,true],"b":2}`))
	must(t, err)
	if got := pyDumps(v); got != `{"b": 2, "a": [1.0, "x\u0001y", null, true]}` {
		t.Errorf("dumps %s", got)
	}
	if got := pyRepr(v); got != `{'b': 2, 'a': [1.0, 'x\x01y', None, True]}` {
		t.Errorf("repr %s", got)
	}
	if _, err := pyLoads([]byte(`{} x`)); err == nil {
		t.Error("trailing data accepted")
	}
}

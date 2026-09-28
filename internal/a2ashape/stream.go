package a2ashape

// stream.go bounds what one stream event carries.
//
// A stream event is one SSE data line, and a client reads a line up to a
// limit: a2a-go's reader stops at 10 MB (internal/sse MaxSSETokenSize) and
// fails the whole stream, for every event after it too. ByReference keeps
// file bytes out of events (0017 Q12), but the rest of an event is as large
// as a peer makes it: a provider's text reply, the metadata it sends with a
// message, a capability deliverable, the number of files it attaches, the
// length of the history. A reply of 2 MiB of '<' — six bytes each once JSON
// escapes it — broke every a2a-go client's stream exactly as an 8 MiB file
// did (redteam F32, on review). So an event is also held to
// MaxStreamEventBytes as JSON: what does not fit is cut, in an order that
// keeps the newest and most useful first, and what was cut is marked
// anet.truncated, with anet.size saying how large it was. A single-task
// read (GetTask) is not a stream line and still carries all of it.

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// MaxStreamEventBytes bounds one stream event as JSON: well under
// a2a-go's 10 MB line, with room for the binding's envelope around it.
const MaxStreamEventBytes = 8 << 20

// TaskForStream is t as a stream event carries it: files by reference
// (ByReference), and no larger than MaxStreamEventBytes as JSON. The
// task's metadata, status message and artifacts are kept whole while they
// fit, in that order, then as much history as fits, newest first; anything
// else is cut (KeyTruncated). t is not changed.
func TaskForStream(t Task) Task {
	t = ByReference(t)
	budget := MaxStreamEventBytes - 256 - jsonLen(t.ID) - jsonLen(t.ContextID)
	var cut bool
	t.Metadata, budget, cut = fitMetadata(t.Metadata, budget)
	if t.Status.Message != nil {
		m, size := fitMessage(*t.Status.Message, budget)
		t.Status.Message, budget = &m, budget-size
		cut = cut || m.Metadata[KeyTruncated] == true
	}
	if t.Artifacts != nil {
		as := make([]Artifact, len(t.Artifacts))
		for i, a := range t.Artifacts {
			var size int
			as[i], size = fitArtifact(a, budget)
			budget -= size
			cut = cut || as[i].Metadata[KeyTruncated] == true
		}
		t.Artifacts = as
	}
	keep := len(t.History)
	for keep > 0 {
		size := messageLen(t.History[keep-1])
		if size > budget {
			break
		}
		budget -= size
		keep--
	}
	if keep > 0 {
		t.History = t.History[keep:]
		cut = true
	}
	if cut {
		meta := make(map[string]any, len(t.Metadata)+1)
		for k, v := range t.Metadata {
			meta[k] = v
		}
		meta[KeyTruncated] = true
		t.Metadata = meta
	}
	return t
}

// EventForStream is e as a stream carries it (TaskForStream): files by
// reference, and no larger than MaxStreamEventBytes as JSON.
func EventForStream(e TaskEvent) TaskEvent {
	e = EventByReference(e)
	budget := MaxStreamEventBytes - 256
	switch {
	case e.Task != nil:
		t := TaskForStream(*e.Task)
		e.Task = &t
	case e.Message != nil:
		m, _ := fitMessage(*e.Message, budget)
		e.Message = &m
	case e.StatusUpdate != nil:
		su := *e.StatusUpdate
		su.Metadata, budget, _ = fitMetadata(su.Metadata, budget-jsonLen(su.TaskID)-jsonLen(su.ContextID))
		if su.Status.Message != nil {
			m, _ := fitMessage(*su.Status.Message, budget)
			su.Status.Message = &m
		}
		e.StatusUpdate = &su
	case e.ArtifactUpdate != nil:
		au := *e.ArtifactUpdate
		au.Metadata, budget, _ = fitMetadata(au.Metadata, budget-jsonLen(au.TaskID)-jsonLen(au.ContextID))
		au.Artifact, _ = fitArtifact(au.Artifact, budget)
		e.ArtifactUpdate = &au
	}
	return e
}

// maxKeptMetadata is how much metadata a cut message or artifact keeps:
// small metadata (anet.state and the like) stays with the notice.
const maxKeptMetadata = 4 << 10

// fitMetadata is meta if it fits in budget, else only the truncation mark;
// with the budget left, and whether it was cut.
func fitMetadata(meta map[string]any, budget int) (map[string]any, int, bool) {
	size := valueLen(meta)
	if meta == nil || size <= budget {
		return meta, budget - size, false
	}
	cut := map[string]any{KeyTruncated: true}
	return cut, budget - valueLen(cut), true
}

// fitMessage is m if it fits in budget, else m with its parts replaced by a
// notice; with the bytes it takes.
func fitMessage(m Message, budget int) (Message, int) {
	size := messageLen(m)
	if size <= budget {
		return m, size
	}
	m.Parts = []Part{truncationNotice(size)}
	m.Metadata = truncatedMetadata(m.Metadata, size)
	return m, messageLen(m)
}

// fitArtifact is fitMessage for an artifact.
func fitArtifact(a Artifact, budget int) (Artifact, int) {
	size := artifactLen(a)
	if size <= budget {
		return a, size
	}
	a.Parts = []Part{truncationNotice(size)}
	a.Metadata = truncatedMetadata(a.Metadata, size)
	return a, artifactLen(a)
}

// truncationNotice stands for the parts of a message or artifact of size
// bytes that a stream event could not carry.
func truncationNotice(size int) Part {
	p := TextPart(fmt.Sprintf("(%d bytes, too large for a stream event; read the task with GetTask)", size))
	p.Metadata = map[string]any{KeyTruncated: true, KeySize: size}
	return p
}

// truncatedMetadata is the metadata of a cut message or artifact: its own
// when small, with the truncation mark and its size.
func truncatedMetadata(meta map[string]any, size int) map[string]any {
	out := map[string]any{}
	if valueLen(meta) <= maxKeptMetadata {
		for k, v := range meta {
			out[k] = v
		}
	}
	out[KeyTruncated], out[KeySize] = true, size
	return out
}

// --- sizes, as encoding/json writes them ---
//
// Each is what the value takes as JSON, or a little more: the field names
// of the A2A types are counted generously and numbers at their widest, so
// that an estimate within MaxStreamEventBytes is an event within it.

func messageLen(m Message) int {
	n := 160 + jsonLen(m.ID) + jsonLen(m.ContextID) + jsonLen(m.TaskID) + jsonLen(string(m.Role)) +
		valueLen(m.Metadata) + stringsLen(m.Extensions) + stringsLen(m.ReferenceTasks)
	for _, p := range m.Parts {
		n += partLen(p)
	}
	return n
}

func artifactLen(a Artifact) int {
	n := 128 + jsonLen(a.ID) + jsonLen(a.Name) + jsonLen(a.Description) + valueLen(a.Metadata) +
		stringsLen(a.Extensions)
	for _, p := range a.Parts {
		n += partLen(p)
	}
	return n
}

func partLen(p Part) int {
	n := 96 + jsonLen(p.Filename) + jsonLen(p.MediaType) + valueLen(p.Metadata)
	switch p.Kind {
	case PartText:
		n += jsonLen(p.Text)
	case PartRaw:
		n += (len(p.Raw)+2)/3*4 + 2
	case PartURL:
		n += jsonLen(p.URL)
	case PartData:
		n += valueLen(p.Data)
	}
	return n
}

func stringsLen(ss []string) int {
	n := 2
	for _, s := range ss {
		n += jsonLen(s) + 1
	}
	return n
}

// valueLen is the JSON size of a decoded JSON value, or of any other value
// as encoding/json writes it.
func valueLen(v any) int {
	switch v := v.(type) {
	case nil:
		return 4
	case bool:
		return 5
	case string:
		return jsonLen(v)
	case json.Number:
		return len(v)
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return 24
	case map[string]any:
		n := 2
		for k, x := range v {
			n += jsonLen(k) + 1 + valueLen(x) + 1
		}
		return n
	case []any:
		n := 2
		for _, x := range v {
			n += valueLen(x) + 1
		}
		return n
	case []string:
		return stringsLen(v)
	case map[string]string:
		n := 2
		for k, x := range v {
			n += jsonLen(k) + jsonLen(x) + 2
		}
		return n
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0 // not encodable: the event fails at its encoding, not here
	}
	return len(b)
}

// jsonLen is the size of s as a JSON string as encoding/json writes it,
// with its HTML escaping: <, > and & are six bytes each, as are control
// characters, U+2028, U+2029 and each byte that is not UTF-8.
func jsonLen(s string) int {
	n := 2
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t':
				n += 2
			case c < 0x20 || c == '<' || c == '>' || c == '&':
				n += 6
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 6
		case r == ' ' || r == ' ':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}

package a2ashape

// project.go keeps what the projection of a stored interaction (package
// stored, A2A-DESIGN §11.5) shares with the rest of this package: the state
// mapping, the control-metadata test, the attachment URI, the by-reference
// forms of a task and the reasons a key event names. The projection itself
// reads the store's types and lives in internal/a2ashape/stored, which only
// the kernel imports (MOD-5: this package, and with it module, reaches no
// store).

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/ANetResearch/ANetCore/anetcid"

	"github.com/ANetResearch/ANet/internal/ixstate"
)

// StateOf is the A2A state of a stored task state.
func StateOf(st ixstate.State) TaskState {
	switch st {
	case ixstate.Submitted:
		return TaskStateSubmitted
	case ixstate.Working:
		return TaskStateWorking
	case ixstate.InputRequired:
		return TaskStateInputRequired
	case ixstate.Completed:
		return TaskStateCompleted
	case ixstate.Failed:
		return TaskStateFailed
	case ixstate.Canceled:
		return TaskStateCanceled
	case ixstate.Rejected:
		return TaskStateRejected
	}
	return TaskStateUnspecified
}

// StoreState is the stored state an A2A state names, for a ListTasks status
// filter. The store's own spelling ("input-required") is accepted too, for
// a control-plane caller that uses it. ok is false for a state the store
// never holds (auth-required, unspecified): such a filter matches nothing.
// ok is also false for a name that is no state at all; the caller tells the
// two apart with TaskState.Valid, because A2A answers the second with
// InvalidParams rather than an empty page.
func StoreState(s TaskState) (ixstate.State, bool) {
	if st := ixstate.State(s); st.Valid() {
		return st, true
	}
	for _, st := range []ixstate.State{ixstate.Submitted, ixstate.Working,
		ixstate.InputRequired, ixstate.Completed, ixstate.Failed,
		ixstate.Canceled, ixstate.Rejected} {
		if StateOf(st) == s {
			return st, true
		}
	}
	return "", false
}

// ControlMetadata reports whether message metadata makes a text message a
// control message rather than a conversation turn: any x402.* key, or
// anet.state (A2A-DESIGN §4.1 [C29]) — a progress note sent with
// anet.state=working among them. Such a message is neither in the
// transcript a receipt covers nor in the history. There is no exception:
// a provider's reply that completes a task travels in its result
// (ResultResp.reply, correctness-ux CUX-R4), not as a message marked to
// keep the requester waiting.
func ControlMetadata(meta map[string]any) bool {
	for k := range meta {
		if strings.HasPrefix(k, "x402.") {
			return true
		}
	}
	_, state := meta[KeyState]
	return state
}

// AttachmentURI names a stored attachment by the parameters of the control
// plane's GET /attachment route. A projection that does not carry file
// bytes inline gives this as the file part's url; the reader fetches the
// bytes from the control plane (or with `anet pull`).
func AttachmentURI(interactionID, cid string) string {
	return "anet:attachment?interaction_id=" + url.QueryEscape(interactionID) + "&cid=" + url.QueryEscape(cid)
}

// MaxInlineBytes is how many attachment bytes one projected task carries
// inline at most (Options.InlineLimit; an internal note item).
const MaxInlineBytes = 8 << 20

// Placeholder turns part into the metadata form of a file (an internal note item): a
// url part naming the stored attachment, with its content id under
// KeyAttachmentCID. Its name, type and other metadata stay.
func Placeholder(part Part, interactionID, cid string) Part {
	meta := make(map[string]any, len(part.Metadata)+2)
	for k, v := range part.Metadata {
		meta[k] = v
	}
	if _, ok := meta[KeyCID]; !ok {
		meta[KeyCID] = cid
	}
	meta[KeyAttachmentCID] = cid
	part.Metadata = meta
	part.Kind, part.Raw, part.URL = PartURL, nil, AttachmentURI(interactionID, cid)
	return part
}

// String is for diagnostics.
func (k PartKind) String() string {
	switch k {
	case PartText:
		return "text"
	case PartRaw:
		return "raw"
	case PartURL:
		return "url"
	case PartData:
		return "data"
	}
	return fmt.Sprintf("PartKind(%d)", uint8(k))
}

// ByReference is t with every file it carries inline replaced by its
// placeholder (Placeholder): the form a task takes in a stream event, which
// carries a file's metadata and never its bytes (an internal note item). A raw part
// without a content id is named by the CID of its bytes. t is not changed.
func ByReference(t Task) Task {
	if t.Status.Message != nil {
		m := messageByReference(*t.Status.Message, t.ID)
		t.Status.Message = &m
	}
	if t.History != nil {
		h := make([]Message, len(t.History))
		for i, m := range t.History {
			h[i] = messageByReference(m, t.ID)
		}
		t.History = h
	}
	if t.Artifacts != nil {
		as := make([]Artifact, len(t.Artifacts))
		for i, a := range t.Artifacts {
			a.Parts = partsByReference(a.Parts, t.ID)
			as[i] = a
		}
		t.Artifacts = as
	}
	return t
}

// EventByReference is ByReference for one stream event.
func EventByReference(e TaskEvent) TaskEvent {
	switch {
	case e.Task != nil:
		t := ByReference(*e.Task)
		e.Task = &t
	case e.Message != nil:
		m := messageByReference(*e.Message, e.Message.TaskID)
		e.Message = &m
	case e.StatusUpdate != nil && e.StatusUpdate.Status.Message != nil:
		su := *e.StatusUpdate
		m := messageByReference(*su.Status.Message, su.TaskID)
		su.Status.Message = &m
		e.StatusUpdate = &su
	case e.ArtifactUpdate != nil:
		au := *e.ArtifactUpdate
		au.Artifact.Parts = partsByReference(au.Artifact.Parts, au.TaskID)
		e.ArtifactUpdate = &au
	}
	return e
}

func messageByReference(m Message, taskID string) Message {
	if m.TaskID != "" {
		taskID = m.TaskID
	}
	m.Parts = partsByReference(m.Parts, taskID)
	return m
}

func partsByReference(parts []Part, taskID string) []Part {
	var out []Part
	for i, p := range parts {
		if p.Kind != PartRaw {
			continue
		}
		if out == nil {
			out = append([]Part(nil), parts...)
		}
		cid, _ := p.Metadata[KeyCID].(string)
		if cid == "" {
			cid, _ = anetcid.Sum(p.Raw)
		}
		if p.Metadata == nil {
			p.Metadata = map[string]any{KeySize: len(p.Raw)}
		} else if _, ok := p.Metadata[KeySize]; !ok {
			meta := make(map[string]any, len(p.Metadata)+1)
			for k, v := range p.Metadata {
				meta[k] = v
			}
			meta[KeySize] = len(p.Raw)
			p.Metadata = meta
		}
		out[i] = Placeholder(p, taskID, cid)
	}
	if out == nil {
		return parts
	}
	return out
}

// The reasons a rollback of this node's data or an emergency rotation gives
// the tasks it ends (FS-X, ux §1.4 E1).
const (
	ReasonLocalStateLost = "local_state_lost"
	ReasonKeyCompromise  = "key_compromise"
)

// ReasonOf is the code of a reason table key: the key without its variant.
func ReasonOf(key string) string {
	code, _, _ := strings.Cut(key, ".")
	return code
}

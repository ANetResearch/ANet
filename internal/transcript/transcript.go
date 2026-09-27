// Package transcript encodes and reads the conversation record a provider
// signs when it completes a text task: the deliverable whose CID is the
// receipt's ResultCID.
//
// Two versions exist and every reader accepts both:
//
//   - v1 (wire 1): a JSON array of messages.
//   - v2 (A2A-DESIGN §2 X4): {"v":2,"nonce":"…","messages":[…]}. The nonce
//     is the task's 16-byte random anet.nonce (base64url), so the result
//     CID cannot be recomputed from a guess of the conversation; together
//     with the nonce in the request TaskDoc it keeps both CIDs a receipt
//     carries from identifying content to a party that holds only the
//     receipt (a hub that stores reviews).
//
// The encoding is encoding/json of the structs below, field order as
// declared. A golden vector in internal/golden pins the v2 bytes.
package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version2 is the version number of the v2 object.
const Version2 = 2

// Message is one line of a transcript. From is "requester" or "provider".
type Message struct {
	From        string       `json:"from"`
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is an attachment's receipt-bound fingerprint: metadata and the
// content CID, not the bytes.
type Attachment struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	CID  string `json:"cid"`
}

// Transcript is a decoded transcript of either version. Version is 1 for
// the array form; Nonce is empty there.
type Transcript struct {
	Version  int
	Nonce    string
	Messages []Message
}

// v2 is the wire form of a v2 transcript.
type v2 struct {
	V        int       `json:"v"`
	Nonce    string    `json:"nonce"`
	Messages []Message `json:"messages"`
}

// EncodeV2 renders a v2 transcript. A nil message list is encoded as an
// empty array.
func EncodeV2(nonce string, msgs []Message) ([]byte, error) {
	if nonce == "" {
		return nil, errors.New("transcript: v2 needs a nonce")
	}
	if msgs == nil {
		msgs = []Message{}
	}
	return json.Marshal(v2{V: Version2, Nonce: nonce, Messages: msgs})
}

// Parse reads a transcript of either version.
func Parse(b []byte) (Transcript, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return Transcript{}, errors.New("transcript: empty")
	}
	switch b[0] {
	case '[':
		var msgs []Message
		if err := json.Unmarshal(b, &msgs); err != nil {
			return Transcript{}, fmt.Errorf("transcript: v1: %w", err)
		}
		return Transcript{Version: 1, Messages: msgs}, nil
	case '{':
		var t v2
		if err := json.Unmarshal(b, &t); err != nil {
			return Transcript{}, fmt.Errorf("transcript: v2: %w", err)
		}
		if t.V != Version2 {
			return Transcript{}, fmt.Errorf("transcript: unknown version %d", t.V)
		}
		return Transcript{Version: Version2, Nonce: t.Nonce, Messages: t.Messages}, nil
	}
	return Transcript{}, errors.New("transcript: neither a v1 array nor a v2 object")
}

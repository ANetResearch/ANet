package interactions

// tasks.go holds the queries behind the A2A task operations (A2A-DESIGN
// §11.1, §11.5, §12): the scoped lookup every peer-bound operation starts
// with, the (contextId, client messageId) dedupe of SendMessage, and the
// context ownership check.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ClientMessageIDKey is the message metadata key that carries the message
// id an A2A client chose (A2A-DESIGN §11.5). It is not the envelope message
// id: the daemon mints its own for the wire, and a client id is only a
// dedupe key for that client's retries.
const ClientMessageIDKey = "a2a.messageId"

// GetFor returns interaction id only when it has the given role and peer.
// An absent id, another role and another peer are the same ErrNotFound,
// from the same single query: the comparison is part of the lookup, so a
// caller scoped to one peer cannot tell "exists for someone else" from
// "does not exist" (A2A-DESIGN §11.1 [C17]).
func (s *Store) GetFor(id string, role Role, peerAID string) (*Interaction, error) {
	if id == "" || role == "" || peerAID == "" {
		return nil, ErrNotFound
	}
	return scanOne(s.db.QueryRow(`SELECT `+ixColumns+` FROM interaction WHERE id=? AND role=? AND peer_aid=?`,
		id, string(role), peerAID))
}

// ClientMessageQuery selects the interaction that already holds a message
// with a client-chosen message id.
type ClientMessageQuery struct {
	Role      Role
	ContextID string // required
	// PeerAID, when set, limits the search to interactions with that peer.
	PeerAID string
	// TaskID, when set, limits the search to that interaction.
	TaskID string
	// ClientMsgID is the client's message id (required).
	ClientMsgID string
}

// FindByClientMessage returns the interaction in q.ContextID holding a
// message whose metadata carries ClientMessageIDKey == q.ClientMsgID, or
// ErrNotFound. It is how SendMessage recognises a client retry and returns
// the task it already created instead of creating a second one.
func (s *Store) FindByClientMessage(q ClientMessageQuery) (*Interaction, error) {
	if q.ContextID == "" || q.ClientMsgID == "" || q.Role == "" {
		return nil, ErrNotFound
	}
	inner := `SELECT m.interaction_id FROM interaction i JOIN message m ON m.interaction_id = i.id
	           WHERE i.role=? AND i.context_id=?`
	args := []any{string(q.Role), q.ContextID}
	if q.PeerAID != "" {
		inner += ` AND i.peer_aid=?`
		args = append(args, q.PeerAID)
	}
	if q.TaskID != "" {
		inner += ` AND i.id=?`
		args = append(args, q.TaskID)
	}
	// json_valid guards json_extract, which fails on malformed input: a
	// message without metadata stores ''.
	inner += ` AND (CASE WHEN json_valid(m.metadata) THEN json_extract(m.metadata, '$."` + ClientMessageIDKey + `"') END) = ?
	           ORDER BY i.seq, m.seq LIMIT 1`
	args = append(args, q.ClientMsgID)
	return scanOne(s.db.QueryRow(`SELECT `+ixColumns+` FROM interaction WHERE id = (`+inner+`)`, args...))
}

// ContextPeers returns the distinct peers of the interactions with role in
// contextID. SendMessage uses it to check that a context a client names
// belongs to the endpoint it is talking to (A2A-DESIGN §11.1).
func (s *Store) ContextPeers(role Role, contextID string) ([]string, error) {
	if contextID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT DISTINCT peer_aid FROM interaction WHERE role=? AND context_id=?`,
		string(role), contextID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MergeMessageMeta adds keys to the metadata object of one stored message.
// Existing keys other than those in add are kept. It is ErrNotFound when the
// message is not in the interaction.
func (s *Store) MergeMessageMeta(interactionID string, msgSeq int64, add map[string]any) error {
	if len(add) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur string
	err := s.db.QueryRow(`SELECT metadata FROM message WHERE seq=? AND interaction_id=?`, msgSeq, interactionID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	m := map[string]any{}
	if cur != "" {
		if err := json.Unmarshal([]byte(cur), &m); err != nil {
			return fmt.Errorf("interactions: message %d metadata: %w", msgSeq, err)
		}
	}
	for k, v := range add {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE message SET metadata=? WHERE seq=? AND interaction_id=?`, string(b), msgSeq, interactionID)
	return err
}

// StateTime is the time of the interaction's last state write.
func (ix *Interaction) StateTime() time.Time { return time.UnixMilli(ix.StateAt).UTC() }

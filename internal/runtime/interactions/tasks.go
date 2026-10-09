package interactions

// tasks.go holds the queries behind the A2A task operations (A2A-DESIGN
// §11.1, §11.5, §12): the scoped lookup every peer-bound operation starts
// with, the client-key lookup of SendMessage's retry check (CUX-D2), and the
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
// id: the daemon mints its own for the wire. It is kept for the projection
// only; a retry is recognised by the message's client_key (FindByClientKey).
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
	ix, err := scanOne(s.db.QueryRow(`SELECT `+ixColumns+` FROM interaction WHERE id=? AND role=? AND peer_aid=?`,
		id, string(role), peerAID))
	if err != nil {
		return nil, err
	}
	if err := openIX(s.db, ix); err != nil {
		return nil, err
	}
	return ix, nil
}

// ClientKeyQuery selects the messages this node wrote for its own client
// under one client key (core L37; CUX-D2): on a follow-up, those of one task;
// on a new task, those of the peer's tasks, in one context when the client
// named one (a client that gave no contextId cannot repeat the one the
// daemon minted, so its retry is recognised by the peer and the key alone).
type ClientKeyQuery struct {
	// Key is the client key (required).
	Key []byte
	// TaskID is the task of a follow-up. When it is set the rest is not
	// read.
	TaskID string
	// PeerAID is the peer of a new task (required without TaskID).
	PeerAID string
	// ContextID, when set, limits a new task's search to that context.
	ContextID string
}

// ClientMessage is the message a client key found: its task as it is now,
// and the message's digest and answered mark (CUX-D1, D5).
type ClientMessage struct {
	Interaction *Interaction
	Seq         int64
	Digest      []byte
	// Unanswered says the client's call that wrote it ended before it was
	// answered (MarkClientUnanswered).
	Unanswered bool
}

// FindByClientKey returns the newest message that q selects, or ErrNotFound.
// Only this node's outbound tasks are searched, and only the messages this
// node wrote on them — those whose sender is not the interaction's peer. The
// key is the local client's; a peer's message carrying a client id (a
// provider planting the ids it expects the client to use next) is not a
// message this client sent [redteam:F33]. A task whose data key is gone
// (its content expired) is no match: there is nothing left to compare the
// retry with (CUX-D1).
//
// It reads the plain client_key column through idx_msg_client_key; the
// metadata, data-key ciphertext, is never looked into (FS-L3).
func (s *Store) FindByClientKey(q ClientKeyQuery) (*ClientMessage, error) {
	if len(q.Key) == 0 || (q.TaskID == "" && q.PeerAID == "") {
		return nil, ErrNotFound
	}
	query, args := q.findSQL()
	var c ClientMessage
	var ix string
	var unanswered int
	err := s.db.QueryRow(query, args...).Scan(&ix, &c.Seq, &c.Digest, &unanswered)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Unanswered = unanswered != 0
	c.Interaction, err = getIX(s.db, ix)
	if errors.Is(err, ErrContentGone) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// findSQL is FindByClientKey's query: the newest local message under the key
// in the query's scope.
func (q ClientKeyQuery) findSQL() (string, []any) {
	query := `SELECT m.interaction_id, m.seq, m.client_digest, m.client_unanswered
	   FROM message m INDEXED BY idx_msg_client_key JOIN interaction i ON i.id = m.interaction_id
	  WHERE m.client_key = ? AND i.role = ? AND m.sender_aid <> i.peer_aid`
	args := []any{q.Key, string(RoleOutbound)}
	if q.TaskID != "" {
		query += ` AND i.id = ?`
		args = append(args, q.TaskID)
	} else {
		query += ` AND i.peer_aid = ?`
		args = append(args, q.PeerAID)
		if q.ContextID != "" {
			query += ` AND i.context_id = ?`
			args = append(args, q.ContextID)
		}
	}
	return query + ` ORDER BY m.seq DESC LIMIT 1`, args
}

// MarkClientUnanswered records that the client's call that wrote task ix's
// message under client key key ended before the call was answered (CUX-D5):
// a retry of it may then be given the task even after the task ended
// (CUX-D4). A task holds at most one message under one key.
func (s *Store) MarkClientUnanswered(ix string, key []byte) error {
	if ix == "" || len(key) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE message SET client_unanswered = 1
	   WHERE interaction_id = ? AND client_key = ? AND client_unanswered = 0`, ix, key)
	return err
}

// nullBytes is b as a column value: NULL when it is empty.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// contextPeersSQL reads idx_ix_context: left to itself SQLite took the role
// index and read the context_id of every task in the role from its row, past
// the long columns before it (an internal note: about a second per
// SendMessage that names a context, after an hour of traffic).
const contextPeersSQL = `SELECT DISTINCT peer_aid FROM interaction INDEXED BY idx_ix_context WHERE role=? AND context_id=?`

// ContextPeers returns the distinct peers of the interactions with role in
// contextID. SendMessage uses it to check that a context a client names
// belongs to the endpoint it is talking to (A2A-DESIGN §11.1).
func (s *Store) ContextPeers(role Role, contextID string) ([]string, error) {
	if contextID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(contextPeersSQL, string(role), contextID)
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

// MergeMessageMeta adds keys to the metadata object of one stored message;
// a nil value removes its key. Existing keys other than those in add are
// kept. It is ErrNotFound when the message is not in the interaction.
func (s *Store) MergeMessageMeta(interactionID string, msgSeq int64, add map[string]any) error {
	if len(add) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return mergeMessageMeta(s.db, interactionID, msgSeq, add)
}

// MergeMessageMeta is Store.MergeMessageMeta inside the transaction.
func (t *Tx) MergeMessageMeta(interactionID string, msgSeq int64, add map[string]any) error {
	if len(add) == 0 {
		return nil
	}
	return mergeMessageMeta(t.tx, interactionID, msgSeq, add)
}

func mergeMessageMeta(e execer, interactionID string, msgSeq int64, add map[string]any) error {
	var stored string
	err := e.QueryRow(`SELECT metadata FROM message WHERE seq=? AND interaction_id=?`, msgSeq, interactionID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	b, err := boxOf(e, interactionID)
	if err != nil {
		return err
	}
	cur, err := b.getText(ColMessageMetadata, stored)
	if err != nil {
		return err
	}
	m := map[string]any{}
	if cur != "" {
		if err := json.Unmarshal([]byte(cur), &m); err != nil {
			return fmt.Errorf("interactions: message %d metadata: %w", msgSeq, err)
		}
	}
	changed := false
	for k, v := range add {
		if v == nil {
			if _, ok := m[k]; ok {
				delete(m, k)
				changed = true
			}
			continue
		}
		m[k] = v
		changed = true
	}
	if !changed {
		return nil
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sealed, err := b.putText(ColMessageMetadata, string(plain))
	if err != nil {
		return err
	}
	_, err = e.Exec(`UPDATE message SET metadata=? WHERE seq=? AND interaction_id=?`, sealed, msgSeq, interactionID)
	return err
}

// StateTime is the time of the interaction's last state write.
func (ix *Interaction) StateTime() time.Time { return time.UnixMilli(ix.StateAt).UTC() }

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

// ClientMessageQuery selects the interaction that already holds a message
// with a client-chosen message id.
type ClientMessageQuery struct {
	Role Role
	// ContextID limits the search to one context. Empty searches every
	// context of PeerAID (a client that gave no contextId cannot repeat
	// the one the daemon minted, so its retry is recognised by the peer
	// and its message id alone).
	ContextID string
	// PeerAID, when set, limits the search to interactions with that peer.
	PeerAID string
	// TaskID, when set, limits the search to that interaction.
	TaskID string
	// ClientMsgID is the client's message id (required).
	ClientMsgID string
	// OpenOnly limits the search to interactions not in a terminal state:
	// a message id seen on a finished task makes a new one (an internal note item).
	OpenOnly bool
}

// FindByClientMessage returns the interaction holding a message whose
// metadata carries ClientMessageIDKey == q.ClientMsgID, within q.ContextID,
// q.PeerAID and q.TaskID as far as they are set (at least one must be), or
// ErrNotFound. It is how SendMessage recognises a client retry and returns
// the task it already created instead of creating a second one.
//
// Only messages this node wrote count — those whose sender is not the
// interaction's peer. The key is the local client's; a peer's message
// carrying it (a provider planting the ids it expects the client to use
// next) is not a message this client sent [redteam:F33].
//
// The metadata is data-key ciphertext (FS-L2) that no SQL may look into
// (FS-L3), so the candidates in scope are read in order and their metadata
// opened here; the first match wins. v030-todo(UX-1): FindByClientKey
// replaces this with a lookup of the plain client_key column (core L37,
// CUX-D2) through its index.
func (s *Store) FindByClientMessage(q ClientMessageQuery) (*Interaction, error) {
	if q.ClientMsgID == "" || q.Role == "" || (q.ContextID == "" && q.PeerAID == "" && q.TaskID == "") {
		return nil, ErrNotFound
	}
	sql, args := q.findSQL()
	rows, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	type cand struct{ ix, meta string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.ix, &c.meta); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	boxes := newBoxCache(s.db)
	for _, c := range cands {
		if c.meta == "" {
			continue
		}
		b, err := boxes.of(c.ix)
		if errors.Is(err, ErrContentGone) {
			continue // deleted meanwhile: not a task to answer with
		}
		if err != nil {
			return nil, err
		}
		meta, err := b.getText(ColMessageMetadata, c.meta)
		if err != nil {
			return nil, err
		}
		if clientMsgID(meta) == q.ClientMsgID {
			return getIX(s.db, c.ix)
		}
	}
	return nil, ErrNotFound
}

// clientMsgID is the client message id a message's metadata carries, or ""
// (no metadata, metadata that is not a JSON object, an id that is not a
// string).
func clientMsgID(meta string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(meta), &m) != nil {
		return ""
	}
	var id string
	if json.Unmarshal(m[ClientMessageIDKey], &id) != nil {
		return ""
	}
	return id
}

// findSQL is FindByClientMessage's query: the candidate messages, oldest
// task first, with their stored metadata.
func (q ClientMessageQuery) findSQL() (string, []any) {
	inner := `SELECT m.interaction_id, m.metadata FROM interaction i JOIN message m ON m.interaction_id = i.id
	           WHERE i.role=? AND m.sender_aid <> i.peer_aid`
	args := []any{string(q.Role)}
	if q.OpenOnly {
		inner += ` AND i.state NOT IN ` + terminalSQL
	}
	if q.ContextID != "" {
		inner += ` AND i.context_id=?`
		args = append(args, q.ContextID)
	}
	if q.PeerAID != "" {
		inner += ` AND i.peer_aid=?`
		args = append(args, q.PeerAID)
	}
	if q.TaskID != "" {
		inner += ` AND i.id=?`
		args = append(args, q.TaskID)
	}
	inner += ` ORDER BY i.seq, m.seq`
	return inner, args
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

package interactions

// caps.go holds the delivery capabilities of sealed sender (SS §3.2): the
// ones this node issued to its peers (caps_issued) and the ones its peers
// issued to it (caps_held). They live in interactions.db beside the
// interactions so that one transaction can do what the protocol needs done
// at once:
//
//   - a capability is issued with the message version that first carries it
//     (SSR-D10, ENG §2.4 A8), so a capability exists exactly when some bytes
//     may have carried it;
//   - a grant is kept by the transaction that accepts the message carrying
//     it (ENG §4.7 item 4), and only by that one (SSR-W8);
//   - a peer's new key set invalidates the capabilities it signed under an
//     older mailbox key in the transaction that records the key set
//     (SSR-D19);
//   - an emergency rotation clears both tables in its one transaction (core
//     §4.7 E1).
//
// The store keeps the rows and the rules that are about rows (uniqueness,
// no revival of an invalidated capability, pruning). Which capability to
// issue, which one to use and when to give one up is the daemon's
// (SS SSR-D5, SSR-D9, SSR-D11).
//
// Times are unix milliseconds. A capability is identified by its 16-byte
// cap_id (CapBody key 3). A one-shot capability belongs to one interaction
// (scope "ix", the interaction in ix); a peer capability (scope "peer")
// carries ix "" so that UNIQUE(issuer, scope, ix) holds for it too.

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
)

// Scopes of a capability (caps_issued.scope, caps_held.scope).
const (
	// CapScopePeer is a peer capability: reused across interactions.
	CapScopePeer = "peer"
	// CapScopeIX is a one-shot capability: its grant names one interaction
	// (Grant key 4) and it delivers only that interaction's messages.
	CapScopeIX = "ix"
)

// Bounds and retention of the two tables (SS §2.12, SSR-D2).
const (
	// MaxHeldCaps bounds caps_held. Past it PruneCaps evicts peer
	// capabilities, earliest cap_exp first.
	MaxHeldCaps = 100_000
	// IssuedRevokedKeepMS is how long a revoked caps_issued row is kept
	// after revoked_at: as long as the hub keeps the revocation (SSR-H10),
	// so the two counts of SSR-D21 compare like with like.
	IssuedRevokedKeepMS = 31 * 24 * 3600 * 1000
	// HeldInvalidKeepMS is how long a caps_held row is kept after
	// invalid_at. Kept a while, not deleted at once, so that a late copy of
	// the same grant (same cap_id) finds it and does not revive it
	// (SSR-D11 item 5).
	HeldInvalidKeepMS = 24 * 3600 * 1000
)

// maxSQLInt is the largest value an INTEGER column holds. Times, epochs and
// counters above it would wrap negative and break every comparison, so the
// writers refuse them.
const maxSQLInt = math.MaxInt64

// IssuedCap is one caps_issued row: a capability this node signed with its
// mailbox signing key for grantee.
type IssuedCap struct {
	CapID   []byte // CapBody.cap_id, seal.CapIDLen bytes
	Grantee string // the AID the grant was given to
	Class   uint64 // seal.CapClassContact or seal.CapClassStranger
	Scope   string // CapScopePeer or CapScopeIX
	IX      string // the interaction of a one-shot capability; "" for a peer capability
	// HolderSeed is the seed of the holder key, kept because every message
	// to the grantee restates the grant (SSR-D1); HolderPub is its public
	// half, CapBody.holder.
	HolderSeed []byte
	HolderPub  []byte
	Epoch      uint64 // this node's mailbox epoch when it signed the capability
	CapExp     uint64
	IssuedAt   uint64
	// RevokedAt is when this node revoked the capability, 0 while it has
	// not; RevokePushed reports that the hub has the revocation (SSR-D20).
	RevokedAt    uint64
	RevokePushed bool
}

// HeldCap is one caps_held row: a capability a peer (Issuer) granted this
// node, with what this node learnt about using it.
type HeldCap struct {
	Issuer string // CapBody.owner: the AID the capability delivers to
	Scope  string // CapScopePeer or CapScopeIX
	IX     string // Grant key 4 for a one-shot capability; "" for a peer capability
	CapID  []byte // CapBody.cap_id, the identity of the capability
	// Cap is the capability as outer key 7 carries it (seal.Cap encoding);
	// HolderSeed the seed of the holder key that proves possession.
	Cap        []byte
	HolderSeed []byte
	Class      uint64
	Epoch      uint64 // the issuer's mailbox epoch the capability is under
	CapExp     uint64
	Home       string // Grant key 3: the AID of the issuer's hub
	P2P        string // Grant key 5, "" when the grant had none
	Pay        bool   // Grant key 6, false when the grant had none
	// InvalidAt is when this node stopped using the capability, 0 while it
	// may. RefusedN and RefusedSince count the hub's unexplained refusals of
	// it since it was last accepted (SSR-D11); RefusedSince is 0 when there
	// are none.
	InvalidAt    uint64
	RefusedN     uint64
	RefusedSince uint64
	ReceivedAt   uint64
}

// HeldFromGrant is the caps_held row for a grant seal.CheckGrant accepted:
// g and body are what it returned, the issuer is body's owner (the sender
// of the message, which CheckGrant checked).
func HeldFromGrant(g *seal.Grant, body *seal.CapBody, receivedAt uint64) (HeldCap, error) {
	if g == nil || body == nil {
		return HeldCap{}, fmt.Errorf("%w: no grant", ErrBadInput)
	}
	cb, err := g.Cap.Marshal()
	if err != nil {
		return HeldCap{}, fmt.Errorf("%w: grant capability: %v", ErrBadInput, err)
	}
	h := HeldCap{
		Issuer: body.Owner, Scope: CapScopePeer, CapID: body.CapID, Cap: cb, HolderSeed: g.HolderSeed,
		Class: body.Class, Epoch: body.Epoch, CapExp: body.CapExp, Home: g.Home, ReceivedAt: receivedAt,
	}
	if g.IX != nil {
		h.Scope, h.IX = CapScopeIX, *g.IX
	}
	if g.P2P != nil {
		h.P2P = *g.P2P
	}
	if g.Pay != nil {
		h.Pay = *g.Pay
	}
	return h, h.check()
}

// capsSchema is the two capability tables (SS §3.2), part of the v0.3.0 schema (interactions.go):
// v0.3.0 creates them with the store, and nothing older is read (CORE-V4).
var capsSchema = []string{
	`CREATE TABLE caps_issued (
	   cap_id BLOB PRIMARY KEY CHECK (length(cap_id) = 16),
	   grantee TEXT NOT NULL CHECK (grantee != ''),
	   class INTEGER NOT NULL CHECK (class IN (1, 2)),
	   scope TEXT NOT NULL CHECK (scope IN ('peer', 'ix')),
	   ix TEXT NOT NULL DEFAULT '' CHECK ((scope = 'ix') = (ix != '')),
	   holder_seed BLOB NOT NULL CHECK (length(holder_seed) = 32),
	   holder_pub BLOB NOT NULL CHECK (length(holder_pub) = 32),
	   epoch INTEGER NOT NULL CHECK (epoch >= 1),
	   cap_exp INTEGER NOT NULL,
	   issued_at INTEGER NOT NULL,
	   revoked_at INTEGER,
	   revoke_pushed INTEGER NOT NULL DEFAULT 0
	 )`,
	`CREATE INDEX idx_caps_issued_grantee ON caps_issued(grantee, scope)`,
	// The revocations still to push to the hub (SSR-D20).
	`CREATE INDEX idx_caps_issued_unpushed ON caps_issued(revoked_at)
	   WHERE revoked_at IS NOT NULL AND revoke_pushed = 0`,
	`CREATE TABLE caps_held (
	   issuer TEXT NOT NULL CHECK (issuer != ''),
	   scope TEXT NOT NULL CHECK (scope IN ('peer', 'ix')),
	   ix TEXT NOT NULL DEFAULT '' CHECK ((scope = 'ix') = (ix != '')),
	   cap_id BLOB NOT NULL CHECK (length(cap_id) = 16),
	   cap BLOB NOT NULL CHECK (length(cap) > 0),
	   holder_seed BLOB NOT NULL CHECK (length(holder_seed) = 32),
	   class INTEGER NOT NULL CHECK (class IN (1, 2)),
	   epoch INTEGER NOT NULL CHECK (epoch >= 1),
	   cap_exp INTEGER NOT NULL,
	   home TEXT NOT NULL CHECK (home != ''),
	   p2p TEXT NOT NULL DEFAULT '',
	   pay INTEGER NOT NULL DEFAULT 0,
	   invalid_at INTEGER,
	   refused_n INTEGER NOT NULL DEFAULT 0,
	   refused_since INTEGER,
	   received_at INTEGER NOT NULL,
	   UNIQUE (issuer, scope, ix)
	 )`,
	// Pruning by expiry and the eviction past MaxHeldCaps.
	`CREATE INDEX idx_caps_held_exp ON caps_held(scope, cap_exp)`,
	// The one-shot capabilities of an interaction, deleted with it.
	`CREATE INDEX idx_caps_held_ix ON caps_held(ix) WHERE ix != ''`,
}

// checkScope checks a scope and the ix that goes with it.
func checkScope(scope, ix string) error {
	switch {
	case scope == CapScopePeer && ix == "":
		return nil
	case scope == CapScopeIX && ix != "":
		return nil
	case scope == CapScopePeer:
		return fmt.Errorf("%w: a peer capability names no interaction", ErrBadInput)
	case scope == CapScopeIX:
		return fmt.Errorf("%w: a one-shot capability names its interaction", ErrBadInput)
	}
	return fmt.Errorf("%w: capability scope %q", ErrBadInput, scope)
}

func checkClass(class uint64) error {
	if class != seal.CapClassContact && class != seal.CapClassStranger {
		return fmt.Errorf("%w: capability class %d", ErrBadInput, class)
	}
	return nil
}

// checkInts refuses values an INTEGER column cannot hold.
func checkInts(vs ...uint64) error {
	for _, v := range vs {
		if v > maxSQLInt {
			return fmt.Errorf("%w: %d does not fit the store", ErrBadInput, v)
		}
	}
	return nil
}

func (c *IssuedCap) check() error {
	switch {
	case c.Grantee == "":
		return fmt.Errorf("%w: an issued capability names its grantee", ErrBadInput)
	case len(c.CapID) != seal.CapIDLen:
		return fmt.Errorf("%w: cap_id is %d bytes, want %d", ErrBadInput, len(c.CapID), seal.CapIDLen)
	case len(c.HolderSeed) != 32 || len(c.HolderPub) != 32:
		return fmt.Errorf("%w: holder seed and key are 32 bytes", ErrBadInput)
	case c.Epoch == 0:
		return fmt.Errorf("%w: mailbox epoch 0", ErrBadInput)
	}
	if err := checkScope(c.Scope, c.IX); err != nil {
		return err
	}
	if err := checkClass(c.Class); err != nil {
		return err
	}
	return checkInts(c.Epoch, c.CapExp, c.IssuedAt, c.RevokedAt)
}

func (h *HeldCap) check() error {
	switch {
	case h.Issuer == "":
		return fmt.Errorf("%w: a held capability names its issuer", ErrBadInput)
	case len(h.CapID) != seal.CapIDLen:
		return fmt.Errorf("%w: cap_id is %d bytes, want %d", ErrBadInput, len(h.CapID), seal.CapIDLen)
	case len(h.Cap) == 0:
		return fmt.Errorf("%w: a held capability carries its capability", ErrBadInput)
	case len(h.HolderSeed) != 32:
		return fmt.Errorf("%w: holder seed is %d bytes, want 32", ErrBadInput, len(h.HolderSeed))
	case h.Epoch == 0:
		return fmt.Errorf("%w: mailbox epoch 0", ErrBadInput)
	case h.Home == "":
		return fmt.Errorf("%w: a held capability names the issuer's hub", ErrBadInput)
	}
	if err := checkScope(h.Scope, h.IX); err != nil {
		return err
	}
	if err := checkClass(h.Class); err != nil {
		return err
	}
	return checkInts(h.Epoch, h.CapExp, h.ReceivedAt)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// rowsQuerier is what the readers need of a *sql.DB or a *sql.Tx.
type rowsQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// ---- caps_issued

const issuedColumns = `cap_id, grantee, class, scope, ix, holder_seed, holder_pub, epoch, cap_exp, issued_at,
	revoked_at, revoke_pushed`

func scanIssued(sc interface{ Scan(...any) error }) (*IssuedCap, error) {
	var c IssuedCap
	var class, epoch, exp, at int64
	var revoked sql.NullInt64
	var pushed int64
	if err := sc.Scan(&c.CapID, &c.Grantee, &class, &c.Scope, &c.IX, &c.HolderSeed, &c.HolderPub, &epoch,
		&exp, &at, &revoked, &pushed); err != nil {
		return nil, err
	}
	c.Class, c.Epoch, c.CapExp, c.IssuedAt = uint64(class), uint64(epoch), uint64(exp), uint64(at)
	if revoked.Valid {
		c.RevokedAt = uint64(revoked.Int64)
	}
	c.RevokePushed = pushed != 0
	return &c, nil
}

func queryIssued(q rowsQuerier, where string, args ...any) ([]IssuedCap, error) {
	rows, err := q.Query(`SELECT `+issuedColumns+` FROM caps_issued `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssuedCap
	for rows.Next() {
		c, err := scanIssued(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func issuedByID(e execer, capID []byte) (*IssuedCap, error) {
	c, err := scanIssued(e.QueryRow(`SELECT `+issuedColumns+` FROM caps_issued WHERE cap_id = ?`, capID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// PutIssuedCap records a capability this node just signed. It is a new
// capability: a cap_id already present is an error (a renewal keeps the
// cap_id and goes through RenewIssuedCap). It runs in the transaction that
// writes the message version carrying the grant (SSR-D10).
func (t *Tx) PutIssuedCap(c IssuedCap) error {
	if err := c.check(); err != nil {
		return err
	}
	if c.RevokedAt != 0 || c.RevokePushed {
		return fmt.Errorf("%w: a capability is issued unrevoked", ErrBadInput)
	}
	_, err := t.tx.Exec(`INSERT INTO caps_issued(`+issuedColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,NULL,0)`,
		c.CapID, c.Grantee, int64(c.Class), c.Scope, c.IX, c.HolderSeed, c.HolderPub, int64(c.Epoch),
		int64(c.CapExp), int64(c.IssuedAt))
	return err
}

// RenewIssuedCap moves the expiry of an unrevoked capability to capExp: the
// same cap_id and holder key signed again with a later cap_exp (SSR-D6).
// It reports false, changing nothing, for a revoked or unknown capability
// or a capExp that is not later.
func (t *Tx) RenewIssuedCap(capID []byte, capExp uint64) (bool, error) {
	if err := checkInts(capExp); err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`UPDATE caps_issued SET cap_exp = ? WHERE cap_id = ? AND revoked_at IS NULL AND cap_exp < ?`,
		int64(capExp), capID, int64(capExp))
	return affected(res, err)
}

// IssuedCap returns the caps_issued row of capID, or ErrNotFound.
func (s *Store) IssuedCap(capID []byte) (*IssuedCap, error) { return issuedByID(s.db, capID) }

// IssuedCap is Store.IssuedCap inside the transaction.
func (t *Tx) IssuedCap(capID []byte) (*IssuedCap, error) { return issuedByID(t.tx, capID) }

// IssuedCaps returns every caps_issued row for grantee, revoked ones
// included, oldest first.
func (s *Store) IssuedCaps(grantee string) ([]IssuedCap, error) {
	return queryIssued(s.db, `WHERE grantee = ? ORDER BY issued_at, cap_id`, grantee)
}

// IssuedCaps is Store.IssuedCaps inside the transaction.
func (t *Tx) IssuedCaps(grantee string) ([]IssuedCap, error) {
	return queryIssued(t.tx, `WHERE grantee = ? ORDER BY issued_at, cap_id`, grantee)
}

// IssuedFilter selects the capabilities given to one grantee. Scope ""
// and Class 0 select every scope and every class.
type IssuedFilter struct {
	Grantee string
	Scope   string
	Class   uint64
}

func (f IssuedFilter) where() (string, []any, error) {
	if f.Grantee == "" {
		return "", nil, fmt.Errorf("%w: a revocation names its grantee", ErrBadInput)
	}
	w, args := `grantee = ?`, []any{f.Grantee}
	if f.Scope != "" {
		if f.Scope != CapScopePeer && f.Scope != CapScopeIX {
			return "", nil, fmt.Errorf("%w: capability scope %q", ErrBadInput, f.Scope)
		}
		w, args = w+` AND scope = ?`, append(args, f.Scope)
	}
	if f.Class != 0 {
		if err := checkClass(f.Class); err != nil {
			return "", nil, err
		}
		w, args = w+` AND class = ?`, append(args, int64(f.Class))
	}
	return w, args, nil
}

// RevokeIssued revokes the unrevoked capabilities f selects: revoked_at =
// now, revoke_pushed = false, so the push loop takes them to the hub
// (SSR-D20). It returns how many it revoked; one already revoked keeps its
// first revoked_at.
func (t *Tx) RevokeIssued(f IssuedFilter, now uint64) (int64, error) {
	return revokeIssued(t.tx, f, now)
}

func revokeIssued(e execer, f IssuedFilter, now uint64) (int64, error) {
	w, args, err := f.where()
	if err != nil {
		return 0, err
	}
	if err := checkInts(now); err != nil {
		return 0, err
	}
	res, err := e.Exec(`UPDATE caps_issued SET revoked_at = ?, revoke_pushed = 0 WHERE revoked_at IS NULL AND `+w,
		append([]any{int64(now)}, args...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeIssuedTo is revokeIssuedToTx (SS §4.3.8 item 5, ENG-K4): every
// capability given to grantee, peer and one-shot, is revoked. It runs in
// the transaction that merges an emergency rotation of the grantee, whose
// old data directory, with the holder seeds of these capabilities, may be
// in someone else's hands.
func (t *Tx) RevokeIssuedTo(grantee string, now uint64) (int64, error) {
	return t.RevokeIssued(IssuedFilter{Grantee: grantee}, now)
}

// RevokeIssuedCap revokes one capability (abuse, SS §4.3.8 item 2). It
// reports false for an unknown or already revoked capability.
func (t *Tx) RevokeIssuedCap(capID []byte, now uint64) (bool, error) {
	if err := checkInts(now); err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`UPDATE caps_issued SET revoked_at = ?, revoke_pushed = 0 WHERE cap_id = ? AND revoked_at IS NULL`,
		int64(now), capID)
	return affected(res, err)
}

// UnpushedRevocations returns up to limit cap_ids revoked here and not yet
// taken to the hub, earliest revocation first (SSR-D20).
func (s *Store) UnpushedRevocations(limit int) ([][]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT cap_id FROM caps_issued WHERE revoked_at IS NOT NULL AND revoke_pushed = 0
		ORDER BY revoked_at, cap_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OldestUnpushedRevocation returns the revoked_at of the oldest revocation
// the hub does not have yet; ok is false when there is none.
func (s *Store) OldestUnpushedRevocation() (revokedAt uint64, ok bool, err error) {
	var at sql.NullInt64
	err = s.db.QueryRow(`SELECT MIN(revoked_at) FROM caps_issued WHERE revoked_at IS NOT NULL AND revoke_pushed = 0`).Scan(&at)
	if err != nil || !at.Valid {
		return 0, false, err
	}
	return uint64(at.Int64), true, nil
}

// MarkRevocationsPushed records that the hub took these revocations. A
// cap_id that is not revoked here is left alone.
func (s *Store) MarkRevocationsPushed(capIDs [][]byte) (int64, error) {
	var n int64
	err := s.Update(func(t *Tx) error {
		for _, id := range capIDs {
			res, err := t.tx.Exec(`UPDATE caps_issued SET revoke_pushed = 1 WHERE cap_id = ? AND revoked_at IS NOT NULL AND revoke_pushed = 0`, id)
			if err != nil {
				return err
			}
			k, err := res.RowsAffected()
			if err != nil {
				return err
			}
			n += k
		}
		return nil
	})
	return n, err
}

// RevocationsSince counts the revocations made at or after since (pushed
// or not): what a hub that lost nothing holds, for SSR-D21.
func (s *Store) RevocationsSince(since uint64) (int, error) {
	if err := checkInts(since); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM caps_issued WHERE revoked_at IS NOT NULL AND revoked_at >= ?`, int64(since)).Scan(&n)
	return n, err
}

// RequeueRevocations marks every revocation made at or after since as not
// pushed, so the push loop takes them to the hub again (SSR-D21: a hub
// restored from a backup, or one that lost the table).
func (t *Tx) RequeueRevocations(since uint64) (int64, error) {
	if err := checkInts(since); err != nil {
		return 0, err
	}
	res, err := t.tx.Exec(`UPDATE caps_issued SET revoke_pushed = 0 WHERE revoked_at IS NOT NULL AND revoked_at >= ? AND revoke_pushed = 1`,
		int64(since))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearCapsIssued deletes every caps_issued row. Only with a new mailbox
// signing key: the old key's capabilities stop verifying at the hub and at
// their holders once the new key set is published (SSR-W11), which is also
// what makes their revocations moot (SS §4.3.8 item 4, core §4.7 E1).
func (t *Tx) ClearCapsIssued() (int64, error) {
	res, err := t.tx.Exec(`DELETE FROM caps_issued`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- caps_held

const heldColumns = `issuer, scope, ix, cap_id, cap, holder_seed, class, epoch, cap_exp, home, p2p, pay, invalid_at,
	refused_n, refused_since, received_at`

func scanHeld(sc interface{ Scan(...any) error }) (*HeldCap, error) {
	var h HeldCap
	var class, epoch, exp, pay, refused, at int64
	var invalid, since sql.NullInt64
	if err := sc.Scan(&h.Issuer, &h.Scope, &h.IX, &h.CapID, &h.Cap, &h.HolderSeed, &class, &epoch, &exp, &h.Home,
		&h.P2P, &pay, &invalid, &refused, &since, &at); err != nil {
		return nil, err
	}
	h.Class, h.Epoch, h.CapExp, h.RefusedN, h.ReceivedAt = uint64(class), uint64(epoch), uint64(exp), uint64(refused), uint64(at)
	h.Pay = pay != 0
	if invalid.Valid {
		h.InvalidAt = uint64(invalid.Int64)
	}
	if since.Valid {
		h.RefusedSince = uint64(since.Int64)
	}
	return &h, nil
}

func queryHeld(q rowsQuerier, where string, args ...any) ([]HeldCap, error) {
	rows, err := q.Query(`SELECT `+heldColumns+` FROM caps_held `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldCap
	for rows.Next() {
		h, err := scanHeld(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

func heldRow(e execer, issuer, scope, ix string) (*HeldCap, error) {
	h, err := scanHeld(e.QueryRow(`SELECT `+heldColumns+` FROM caps_held WHERE issuer = ? AND scope = ? AND ix = ?`,
		issuer, scope, ix))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return h, err
}

// HeldCap returns the capability issuer granted this node for (scope, ix),
// or ErrNotFound.
func (s *Store) HeldCap(issuer, scope, ix string) (*HeldCap, error) {
	return heldRow(s.db, issuer, scope, ix)
}

// HeldCap is Store.HeldCap inside the transaction.
func (t *Tx) HeldCap(issuer, scope, ix string) (*HeldCap, error) {
	return heldRow(t.tx, issuer, scope, ix)
}

// HeldCaps returns every capability issuer granted this node, invalid ones
// included: the peer capability first, then the one-shot ones by
// interaction.
func (s *Store) HeldCaps(issuer string) ([]HeldCap, error) {
	return queryHeld(s.db, `WHERE issuer = ? ORDER BY scope DESC, ix`, issuer)
}

// HeldCaps is Store.HeldCaps inside the transaction.
func (t *Tx) HeldCaps(issuer string) ([]HeldCap, error) {
	return queryHeld(t.tx, `WHERE issuer = ? ORDER BY scope DESC, ix`, issuer)
}

// PutHeldGrant is putHeldGrantTx (ENG §4.7 item 4, SS §4.3.10): it keeps a
// grant carried by a message the transaction accepts. The row for
// (issuer, scope, ix) is
//
//   - written when there is none;
//   - left alone when the grant is under an older mailbox epoch than the
//     row (a late message from before the issuer replaced its key);
//   - for the same cap_id: brought up to the grant (a renewal moves cap_exp,
//     the issuer may change p2p and pay), keeping the refusal count; but
//     never when the row is invalid (no revival, SSR-D11 item 5), and never
//     to an earlier cap_exp (an older copy of the same capability);
//   - for another cap_id: replaced whole, the refusal count with it.
//
// It reports whether it wrote the row.
func (t *Tx) PutHeldGrant(h HeldCap) (bool, error) {
	if err := h.check(); err != nil {
		return false, err
	}
	cur, err := heldRow(t.tx, h.Issuer, h.Scope, h.IX)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err := t.tx.Exec(`INSERT INTO caps_held(`+heldColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,NULL,0,NULL,?)`,
			h.Issuer, h.Scope, h.IX, h.CapID, h.Cap, h.HolderSeed, int64(h.Class), int64(h.Epoch), int64(h.CapExp),
			h.Home, h.P2P, boolInt(h.Pay), int64(h.ReceivedAt))
		return err == nil, err
	case err != nil:
		return false, err
	}
	if h.Epoch < cur.Epoch {
		return false, nil
	}
	if string(h.CapID) == string(cur.CapID) {
		if cur.InvalidAt != 0 || h.CapExp < cur.CapExp {
			return false, nil
		}
		_, err := t.tx.Exec(`UPDATE caps_held SET cap = ?, holder_seed = ?, class = ?, epoch = ?, cap_exp = ?, home = ?,
			p2p = ?, pay = ?, received_at = ? WHERE issuer = ? AND scope = ? AND ix = ?`,
			h.Cap, h.HolderSeed, int64(h.Class), int64(h.Epoch), int64(h.CapExp), h.Home, h.P2P, boolInt(h.Pay),
			int64(h.ReceivedAt), h.Issuer, h.Scope, h.IX)
		return err == nil, err
	}
	_, err = t.tx.Exec(`UPDATE caps_held SET cap_id = ?, cap = ?, holder_seed = ?, class = ?, epoch = ?, cap_exp = ?,
		home = ?, p2p = ?, pay = ?, invalid_at = NULL, refused_n = 0, refused_since = NULL, received_at = ?
		WHERE issuer = ? AND scope = ? AND ix = ?`,
		h.CapID, h.Cap, h.HolderSeed, int64(h.Class), int64(h.Epoch), int64(h.CapExp), h.Home, h.P2P, boolInt(h.Pay),
		int64(h.ReceivedAt), h.Issuer, h.Scope, h.IX)
	return err == nil, err
}

// heldKey selects one held capability: the row for (issuer, scope, ix),
// and, when capID is not nil, only while it still holds that capability.
// The guard matters to callers that act on the outcome of a send: a grant
// accepted meanwhile may have replaced the capability the send used, and
// what the hub said about the old one says nothing about the new one.
func heldKey(issuer, scope, ix string, capID []byte) (string, []any, error) {
	if err := checkScope(scope, ix); err != nil {
		return "", nil, err
	}
	w, args := `issuer = ? AND scope = ? AND ix = ?`, []any{issuer, scope, ix}
	if capID != nil {
		w, args = w+` AND cap_id = ?`, append(args, capID)
	}
	return w, args, nil
}

// MarkHeldInvalid is markHeldInvalidTx: this node stops using one held
// capability (a downgrade, SSR-D11; the peer capability after a settled
// payment, SS §4.3.8 item 3). capID nil marks whatever capability the row
// holds. It reports false when there was nothing valid to mark; a row
// already invalid keeps its first invalid_at.
func (t *Tx) MarkHeldInvalid(issuer, scope, ix string, capID []byte, now uint64) (bool, error) {
	w, args, err := heldKey(issuer, scope, ix, capID)
	if err != nil {
		return false, err
	}
	if err := checkInts(now); err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`UPDATE caps_held SET invalid_at = ? WHERE invalid_at IS NULL AND `+w,
		append([]any{int64(now)}, args...)...)
	return affected(res, err)
}

// NoteHeldRefused counts one unexplained refusal of a valid held capability
// (SSR-D11 item 3): refused_n + 1, and refused_since = now when it was
// unset. It returns the new count and since; ok is false when the row no
// longer holds capID or is invalid, and nothing changed.
func (t *Tx) NoteHeldRefused(issuer, scope, ix string, capID []byte, now uint64) (n, since uint64, ok bool, err error) {
	if capID == nil {
		return 0, 0, false, fmt.Errorf("%w: a refusal names the capability it refused", ErrBadInput)
	}
	w, args, err := heldKey(issuer, scope, ix, capID)
	if err != nil {
		return 0, 0, false, err
	}
	if err := checkInts(now); err != nil {
		return 0, 0, false, err
	}
	var cnt, at int64
	err = t.tx.QueryRow(`UPDATE caps_held SET refused_n = refused_n + 1, refused_since = COALESCE(refused_since, ?)
		WHERE invalid_at IS NULL AND `+w+` RETURNING refused_n, refused_since`,
		append([]any{int64(now)}, args...)...).Scan(&cnt, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return uint64(cnt), uint64(at), true, nil
}

// ClearHeldRefused resets the refusal count of a held capability the hub
// accepted (SSR-D11 item 4). It reports whether the row held capID.
func (t *Tx) ClearHeldRefused(issuer, scope, ix string, capID []byte) (bool, error) {
	if capID == nil {
		return false, fmt.Errorf("%w: an acceptance names the capability it accepted", ErrBadInput)
	}
	w, args, err := heldKey(issuer, scope, ix, capID)
	if err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`UPDATE caps_held SET refused_n = 0, refused_since = NULL WHERE `+w, args...)
	return affected(res, err)
}

// InvalidateOlderEpoch is invalidateOlderEpochTx (SSR-D19, ENG §4.7 item
// 7): issuer published a key set whose mailbox epoch is epoch, so the
// capabilities it signed under an earlier epoch no longer deliver. It runs
// in the transaction that records the key set, and returns how many it
// invalidated.
func (t *Tx) InvalidateOlderEpoch(issuer string, epoch, now uint64) (int64, error) {
	return invalidateOlderEpoch(t.tx, issuer, epoch, now)
}

func invalidateOlderEpoch(e execer, issuer string, epoch, now uint64) (int64, error) {
	if issuer == "" {
		return 0, fmt.Errorf("%w: an issuer is named", ErrBadInput)
	}
	if err := checkInts(epoch, now); err != nil {
		return 0, err
	}
	res, err := e.Exec(`UPDATE caps_held SET invalid_at = ? WHERE issuer = ? AND epoch < ? AND invalid_at IS NULL`,
		int64(now), issuer, int64(epoch))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// InvalidatePeerCaps is invalidatePeerCapsTx (KEL D-44, ENG-K4): issuer
// rotated in an emergency, so every capability it granted this node, peer
// and one-shot, stops being used, and the direct address its grants taught
// this node is forgotten. It runs in the transaction that merges the
// rotation and returns how many rows it invalidated.
func (t *Tx) InvalidatePeerCaps(issuer string, now uint64) (int64, error) {
	return invalidatePeerCaps(t.tx, issuer, now)
}

func invalidatePeerCaps(e execer, issuer string, now uint64) (int64, error) {
	if issuer == "" {
		return 0, fmt.Errorf("%w: an issuer is named", ErrBadInput)
	}
	if err := checkInts(now); err != nil {
		return 0, err
	}
	res, err := e.Exec(`UPDATE caps_held SET invalid_at = ? WHERE issuer = ? AND invalid_at IS NULL`, int64(now), issuer)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := e.Exec(`UPDATE caps_held SET p2p = '' WHERE issuer = ? AND p2p != ''`, issuer); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteHeldCapsOfIX deletes the one-shot capabilities of an interaction,
// with the interaction's own cleanup (SSR-D2).
func (t *Tx) DeleteHeldCapsOfIX(ix string) (int64, error) {
	if ix == "" {
		return 0, fmt.Errorf("%w: an interaction is named", ErrBadInput)
	}
	res, err := t.tx.Exec(`DELETE FROM caps_held WHERE scope = 'ix' AND ix = ?`, ix)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearCapsHeld deletes every caps_held row, and with them every direct
// address a grant taught this node (core §4.7 E1, SSR-D22).
func (t *Tx) ClearCapsHeld() (int64, error) {
	res, err := t.tx.Exec(`DELETE FROM caps_held`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- both

// CapsCounts returns how many held capabilities this node may use and how
// many it issued that are in force at now (doctor sender_privacy.grants).
func (s *Store) CapsCounts(now uint64) (held, issued int, err error) {
	if err := checkInts(now); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM caps_held WHERE invalid_at IS NULL AND cap_exp >= ?`, int64(now)).Scan(&held); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM caps_issued WHERE revoked_at IS NULL AND cap_exp >= ?`, int64(now)).Scan(&issued)
	return held, issued, err
}

// CapsPruned is what one PruneCaps run deleted, by rule.
type CapsPruned struct {
	IssuedExpired int64 // unrevoked, past cap_exp
	IssuedRevoked int64 // revoked IssuedRevokedKeepMS ago or more
	HeldExpired   int64 // past cap_exp
	HeldInvalid   int64 // invalid HeldInvalidKeepMS ago or more
	HeldEvicted   int64 // peer capabilities past MaxHeldCaps
}

// PruneCaps applies SSR-D2 at now: an unrevoked issued capability goes
// once its cap_exp has passed, a revoked one IssuedRevokedKeepMS after its
// revocation; a held capability goes once its cap_exp has passed or
// HeldInvalidKeepMS after it was invalidated; past MaxHeldCaps held rows,
// the peer capabilities with the earliest cap_exp go. The daemon runs it
// every five minutes.
func (s *Store) PruneCaps(now uint64) (CapsPruned, error) { return s.pruneCaps(now, MaxHeldCaps) }

func (s *Store) pruneCaps(now uint64, maxHeld int) (CapsPruned, error) {
	var p CapsPruned
	if err := checkInts(now); err != nil {
		return p, err
	}
	n := int64(now)
	cut := func(keep int64) int64 {
		if n < keep {
			return math.MinInt64
		}
		return n - keep
	}
	err := s.Update(func(t *Tx) error {
		steps := []struct {
			into *int64
			q    string
			arg  int64
		}{
			{&p.IssuedExpired, `DELETE FROM caps_issued WHERE revoked_at IS NULL AND cap_exp < ?`, n},
			{&p.IssuedRevoked, `DELETE FROM caps_issued WHERE revoked_at IS NOT NULL AND revoked_at <= ?`, cut(IssuedRevokedKeepMS)},
			{&p.HeldExpired, `DELETE FROM caps_held WHERE cap_exp < ?`, n},
			{&p.HeldInvalid, `DELETE FROM caps_held WHERE invalid_at IS NOT NULL AND invalid_at <= ?`, cut(HeldInvalidKeepMS)},
		}
		for _, st := range steps {
			res, err := t.tx.Exec(st.q, st.arg)
			if err != nil {
				return err
			}
			if *st.into, err = res.RowsAffected(); err != nil {
				return err
			}
		}
		var rows int
		if err := t.tx.QueryRow(`SELECT COUNT(*) FROM caps_held`).Scan(&rows); err != nil {
			return err
		}
		if rows <= maxHeld {
			return nil
		}
		res, err := t.tx.Exec(`DELETE FROM caps_held WHERE rowid IN (
			SELECT rowid FROM caps_held WHERE scope = 'peer' ORDER BY cap_exp, rowid LIMIT ?)`, rows-maxHeld)
		if err != nil {
			return err
		}
		p.HeldEvicted, err = res.RowsAffected()
		return err
	})
	return p, err
}

// affected reports whether an UPDATE changed a row.
func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// epochOfKeySet returns the mailbox epoch of a SignedEncKeySet encoding, or
// 0 when it does not decode. The key set was verified by whoever stores
// it; this only reads the epoch back.
func epochOfKeySet(signed []byte) uint64 {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		return 0
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		return 0
	}
	return set.Mbox.Epoch
}

// ---- what other writes mean for the capabilities

// IssuedGrantees returns every AID this node gave a capability it has not
// revoked, in AID order: the peers whose capabilities the peer lists may
// have to withdraw (SS §4.3.8 item 1, reconcileCapsWithLists).
func (s *Store) IssuedGrantees() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT grantee FROM caps_issued WHERE revoked_at IS NULL ORDER BY grantee`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PushedRevocationsSince counts the revocations made at or after since that
// the hub took (revoke_pushed): what a hub that lost nothing holds of this
// node's revocations at least, for the comparison of SSR-D21.
func (s *Store) PushedRevocationsSince(since uint64) (int, error) {
	if err := checkInts(since); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM caps_issued WHERE revoke_pushed = 1 AND revoked_at IS NOT NULL AND revoked_at >= ?`,
		int64(since)).Scan(&n)
	return n, err
}

// peerWritten applies, in the transaction that writes aid's peer_identity
// row from cur (nil: there was none) to next, what the write means for the
// capabilities this node and aid gave each other:
//
//   - SSR-D19: a key set with a higher mailbox epoch voids the capabilities
//     aid signed under an older one (invalidateOlderEpoch);
//   - ENG-K4, KEL D-44: a KEL that gains an emergency rotation of aid
//     (key 12) drops every capability aid gave this node, and the direct
//     address its grants taught (invalidatePeerCapsTx), and revokes every
//     capability this node gave aid (revokeIssuedToTx): aid's old data
//     directory, with the holder seeds of those capabilities, may be in
//     someone else's hands. A planned rotation changes nothing here.
//
// UpdatePeerIdentity calls it; any other writer of peer_identity (the
// engine's step 10, KEL D-26 refreshes) must too.
func peerWritten(e execer, aid string, cur, next *PeerIdentity, now uint64) error {
	if len(next.KeySet) > 0 && (cur == nil || !bytes.Equal(next.KeySet, cur.KeySet)) {
		if _, err := invalidateOlderEpoch(e, aid, epochOfKeySet(next.KeySet), now); err != nil {
			return err
		}
	}
	if cur == nil || len(cur.KEL) == 0 || bytes.Equal(next.KEL, cur.KEL) {
		return nil
	}
	if !emergencyMerged(cur.KEL, next.KEL) {
		return nil
	}
	if _, err := invalidatePeerCaps(e, aid, now); err != nil {
		return err
	}
	_, err := revokeIssued(e, IssuedFilter{Grantee: aid}, now)
	return err
}

// emergencyMerged reports whether nextKEL, a peer's KEL as this node holds
// it after a write, adds to oldKEL, the KEL it held before, an emergency
// rotation: oldKEL is a prefix of nextKEL (identity.ExtendsKEL, checked
// here because the store does not know how its caller merged) and
// identity.MergedEmergency finds a rot with key 12 past it (KEL D-44). A
// KEL that does not decode or extend merged nothing.
func emergencyMerged(oldKEL, nextKEL []byte) bool {
	old, err := identity.UnmarshalKEL(oldKEL)
	if err != nil {
		return false
	}
	next, err := identity.UnmarshalKEL(nextKEL)
	if err != nil || len(next) <= len(old) {
		return false
	}
	return identity.MergedEmergency(old, next) && identity.ExtendsKEL(old, next) == nil
}

// settledPeerCaps is SS §4.3.8 item 3: an interaction with peer has ended
// with its a2a-x402 payment completed. In that transaction this node
// revokes the peer capabilities it gave peer and stops using the one peer
// gave it, so the next message either way carries a new one (§4.3.2 rule
// 5): the settlement, which the hubs see, links the payer to this
// interaction and not to everything else the two exchange. One-shot
// capabilities are left to expire (§4.3.8, last paragraph).
func settledPeerCaps(e execer, peer string, now int64) error {
	if peer == "" {
		return nil
	}
	if _, err := e.Exec(`UPDATE caps_issued SET revoked_at = ?, revoke_pushed = 0
		WHERE grantee = ? AND scope = 'peer' AND revoked_at IS NULL`, now, peer); err != nil {
		return err
	}
	_, err := e.Exec(`UPDATE caps_held SET invalid_at = ? WHERE issuer = ? AND scope = 'peer' AND invalid_at IS NULL`, now, peer)
	return err
}

// endedSettled runs settledPeerCaps for interaction id when a write just
// made it terminal (moved) and its payment is recorded completed. The write
// that records a completed payment on an ended interaction calls
// settledPeerCaps itself (Tx.SetPayment): together they act once, on the
// write that makes "ended and paid" true.
func endedSettled(e execer, id string, moved bool, now int64) error {
	if !moved {
		return nil
	}
	var peer, pay string
	if err := e.QueryRow(`SELECT peer_aid, pay_state FROM interaction WHERE id=?`, id).Scan(&peer, &pay); err != nil {
		return err
	}
	if pay != PayCompleted {
		return nil
	}
	return settledPeerCaps(e, peer, now)
}

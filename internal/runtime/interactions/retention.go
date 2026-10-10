package interactions

// retention.go deletes the content of finished tasks once their retention has passed (forward-secrecy
// §4.6.2, FS-L5 to FS-L8; engine §5.4). A task's content is kept for the retention that applies to it,
// counted from its terminal state_at and rounded to the UTC day:
//
//   - a non-public task (one this node sent, or one it took from a peer on its lists or approved by
//     its operator) is kept for content_retention, 30 days unless the operator chose otherwise, or for
//     ever;
//   - a public task (a call of one of this node's public capabilities, or a task taken from a stranger
//     under the open policy) is kept 7 days, or content_retention when that is shorter; "forever" does
//     not apply to it.
//
// One function decides whether a task's content may go (contentDeletable); the sweep (PruneTerminal)
// and the count a change of the setting is confirmed with (CountNewlyDeletable) both ask it. A task
// is deleted whole, in one transaction with the others of its batch: its attachments, messages and
// row, the interaction key a local end kept for it (CauseRetention, X7e) and its data key, which
// makes whatever copy of its pages is left unreadable.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Retention is how long the content of a finished non-public task is kept (config.json
// content_retention): a whole number of days from 1 to MaxRetentionDays, or for ever. The zero value
// is no setting at all: the sweep refuses it, so a caller that does not know the setting deletes
// nothing.
type Retention struct {
	days    int
	forever bool
}

const (
	// MaxRetentionDays is the longest retention that is a number of days (ux §2.2).
	MaxRetentionDays = 3650
	// PublicRetentionDays is how long a public task is kept at most (FS §0, core N2).
	PublicRetentionDays = 7
	// DefaultRetentionDays is content_retention when config.json does not say.
	DefaultRetentionDays = 30
)

// RetentionForever keeps non-public tasks for ever.
var RetentionForever = Retention{forever: true}

// RetentionDays is a retention of n days, and whether n is one (1 to MaxRetentionDays).
func RetentionDays(n int) (Retention, bool) {
	if n < 1 || n > MaxRetentionDays {
		return Retention{}, false
	}
	return Retention{days: n}, true
}

// DefaultRetention is the retention of a node whose config.json does not set one.
func DefaultRetention() Retention { return Retention{days: DefaultRetentionDays} }

// ParseRetention reads a content_retention value: "<N>d" with N a whole number from 1 to
// MaxRetentionDays written without a sign or leading zeros, or "forever". Nothing else is one — no
// space, no capital, no other unit — so what the file says is what the node does.
func ParseRetention(s string) (Retention, bool) {
	if s == "forever" {
		return RetentionForever, true
	}
	n, ok := strings.CutSuffix(s, "d")
	if !ok || n == "" || len(n) > 4 || n[0] == '0' {
		return Retention{}, false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return Retention{}, false
		}
	}
	v, err := strconv.Atoi(n)
	if err != nil {
		return Retention{}, false
	}
	return RetentionDays(v)
}

// Valid reports whether r is a setting: days in range, or for ever.
func (r Retention) Valid() bool {
	if r.forever {
		return r.days == 0
	}
	return r.days >= 1 && r.days <= MaxRetentionDays
}

// Forever reports whether r keeps non-public tasks for ever.
func (r Retention) Forever() bool { return r.forever }

// Days is r's number of days, 0 for ever.
func (r Retention) Days() int { return r.days }

// String is r as config.json writes it: "30d" or "forever" ("" for the zero value).
func (r Retention) String() string {
	switch {
	case r.forever:
		return "forever"
	case r.days > 0:
		return strconv.Itoa(r.days) + "d"
	}
	return ""
}

// Public is the retention of a public task under r: PublicRetentionDays, or r when it is shorter.
func (r Retention) Public() Retention {
	if !r.forever && r.days >= 1 && r.days < PublicRetentionDays {
		return r
	}
	return Retention{days: PublicRetentionDays}
}

// Content classes: a finished task's content is swept, and its deletion recorded (core §2.14
// anet.interaction.pruned, trust), under one of these.
const (
	ClassOutbound  = "outbound"     // a task this node sent
	ClassPeer      = TrustPeer      // a task a peer on this node's lists sent
	ClassApproved  = TrustApproved  // a held task the operator approved
	ClassPublic    = TrustPublic    // a stranger's task taken under the open policy
	ClassPublicCap = TrustPublicCap // a call of one of this node's public capabilities
)

// ContentClasses are the classes in the order the sweep reports them.
func ContentClasses() []string {
	return []string{ClassOutbound, ClassPeer, ClassApproved, ClassPublic, ClassPublicCap}
}

// ContentClass is the class of a task in which this node has role, admitted with trust. A task this
// node took whose trust is none of the four is kept as a non-public one (ClassPeer): a class that
// cannot be told keeps the longer retention.
func ContentClass(role Role, trust string) string {
	if role != RoleInbound {
		return ClassOutbound
	}
	switch trust {
	case TrustPeer, TrustApproved, TrustPublic, TrustPublicCap:
		return trust
	}
	return ClassPeer
}

// PublicClass reports whether class is public: its tasks are kept PublicRetentionDays at most.
func PublicClass(class string) bool { return class == ClassPublic || class == ClassPublicCap }

// dayMS is one UTC day in unix milliseconds.
const dayMS = int64(24 * 60 * 60 * 1000)

// utcDayStart is the start of the UTC day of t (unix ms), t itself at midnight.
func utcDayStart(t int64) int64 { return t - ((t%dayMS)+dayMS)%dayMS }

// AppliedRetention is the retention a task of class keeps its content for under r, and false when it
// keeps it for ever (a non-public class under RetentionForever).
func AppliedRetention(class string, r Retention) (Retention, bool) {
	if PublicClass(class) {
		return r.Public(), true
	}
	if r.forever || !r.Valid() {
		return Retention{}, false
	}
	return r, true
}

// RetentionCutoff is the cutoff of a sweep run at now (unix ms) for a task of class (FS-L5): a finished
// task whose state_at is before it is past its retention. It is the start of now's UTC day less the
// retention, so a sweep is a daily one whatever hour it runs at. ok is false when the class keeps its
// content for ever.
func RetentionCutoff(now int64, class string, r Retention) (cutoff int64, ok bool) {
	a, ok := AppliedRetention(class, r)
	if !ok {
		return 0, false
	}
	return utcDayStart(now) - int64(a.days)*dayMS, true
}

// ContentExpiresAt is when the content of a task of class that ended at stateAt becomes the sweep's
// (anet.content_expires_at, FS-L5): the start of the UTC day after state_at plus the retention. The
// sweep at now deletes the task exactly when now has reached it (contentDeletable), and it does so in
// the first maintenance period after, unless an exception holds (FS-L6, FS-L7). ok is false for a class
// kept for ever.
func ContentExpiresAt(stateAt int64, class string, r Retention) (at int64, ok bool) {
	a, ok := AppliedRetention(class, r)
	if !ok {
		return 0, false
	}
	return utcDayStart(stateAt+int64(a.days)*dayMS) + dayMS, true
}

// contentFacts is what contentDeletable reads of a task: its class, its state and when it entered it.
type contentFacts struct {
	Class   string
	State   State
	StateAt int64
}

// contentDeletable is FS-L5 to FS-L7 (engine §5.4): whether the sweep at now, run at the clock gate's
// level, deletes the content of the task described by ix under the setting r. Only at ClockFull and
// ClockLocal (FS-L6); only a finished task past the retention of its class (FS-L5); and not while the
// outbound queue still holds a row of the task (ENG-E2, ENG-S7: the row is resealed from what the task
// holds) or its payment is submitted and not settled (FS-L7: the stored payment is what settles it).
// Pure.
func contentDeletable(ix contentFacts, now int64, level ClockLevel, r Retention, hasOutboxRows, payPending bool) bool {
	if level != ClockFull && level != ClockLocal {
		return false
	}
	if !ix.State.IsTerminal() || hasOutboxRows || payPending {
		return false
	}
	cutoff, ok := RetentionCutoff(now, ix.Class, r)
	return ok && ix.StateAt < cutoff
}

// Pruned counts the rows a sweep deleted for one class.
type Pruned struct {
	Interactions int64
	Messages     int64
	Attachments  int64
}

// pruneBatch is how many finished tasks one transaction of PruneTerminal reads, and so the most it
// deletes (FS-L8). The store lock is held for a transaction, and the receive path waits on it; the
// first sweep after a long stop, or after the setting was shortened, may have a great many rows to
// delete, so it releases the lock between batches. A variable so that a test can use small batches.
var pruneBatch = 500

// PruneTerminal is the retention sweep run at now at the clock gate's level under the setting r
// (FS-L5 to FS-L8): it deletes every finished task contentDeletable lets go, and reports by class
// (ContentClasses) what it deleted. A task goes whole: its attachments, messages and row (with the
// peer key material and the payment record on it), a key a local end kept for it (CauseRetention) and
// its data key. The data key of a task stays while a held delegation of the same id still has its
// content under it (ReleaseDEK's rule); the held item takes the key with it when it goes.
//
// The replay rows are not touched: they are keyed by message and expire on their own (PurgeReplay),
// and they are what refuses a replayed envelope after the task is gone.
//
// The sweep reads the finished tasks old enough for the retention of their class in batches of
// pruneBatch, in the order of their state_at, each in its own transaction: a task the decision keeps
// (a row still queued, a payment not settled) is passed over and asked again by the next sweep. On an
// error the counts of the batches already committed are returned with it. An invalid r is ErrBadInput
// and deletes nothing.
func (s *Store) PruneTerminal(now int64, level ClockLevel, r Retention) (map[string]Pruned, error) {
	out := map[string]Pruned{}
	if !r.Valid() {
		return out, fmt.Errorf("%w: a retention sweep needs a valid setting", ErrBadInput)
	}
	cur := pruneCursor{at: math.MinInt64}
	for {
		done, err := s.pruneBatch(now, level, r, &cur, pruneBatch, out)
		if err != nil {
			return out, fmt.Errorf("interactions: retention sweep: %w", err)
		}
		if done {
			return out, nil
		}
	}
}

// pruneCursor is where a sweep's reading has got to: the (state_at, seq) of the last task read.
type pruneCursor struct{ at, seq int64 }

// candidate is a finished task a sweep reads.
type candidate struct {
	contentFacts
	ID       string
	Seq      int64
	PayState string
}

// querier reads rows, in a transaction or on the store.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// publicSQL is the SQL of PublicClass(ContentClass(role, trust)).
const publicSQL = `(role = 'inbound' AND trust IN ('public','public_cap'))`

// candidates reads up to limit finished tasks after cur, in (state_at, seq) order, whose state_at is
// before the cutoff of their class under r at now: the tasks a sweep has to decide on. A class kept for
// ever has none. The query names no content column (FS-L3), and walks idx_ix_state_at in its order from
// the cursor, so a sweep reads only the tasks old enough for the shortest retention that applies.
func candidates(q querier, now int64, r Retention, cur pruneCursor, limit int) ([]candidate, error) {
	pub, _ := RetentionCutoff(now, ClassPublicCap, r)
	priv, ok := RetentionCutoff(now, ClassPeer, r)
	if !ok {
		priv = math.MinInt64 // no state_at is below it
	}
	rows, err := q.Query(`SELECT id, seq, role, trust, state, state_at, pay_state FROM interaction
	   WHERE (state_at, seq) > (?, ?) AND state_at < ? AND state IN `+terminalSQL+`
	     AND ((`+publicSQL+` AND state_at < ?) OR (NOT `+publicSQL+` AND state_at < ?))
	   ORDER BY state_at, seq LIMIT ?`,
		cur.at, cur.seq, max(pub, priv), pub, priv, limit)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for rows.Next() {
		var c candidate
		var role, state string
		if err := rows.Scan(&c.ID, &c.Seq, &role, &c.Class, &state, &c.StateAt, &c.PayState); err != nil {
			rows.Close()
			return nil, err
		}
		c.Class, c.State = ContentClass(Role(role), c.Class), State(state)
		out = append(out, c)
	}
	return out, closeRows(rows)
}

// deletable reads the facts of c the decision needs beyond its row and asks contentDeletable.
func (c candidate) deletable(q querier, now int64, level ClockLevel, r Retention) (bool, error) {
	queued, err := hasOutboxRows(q, c.ID) // the engine's answer (ENG-E2), as HasOutboxRowsTx gives it
	if err != nil {
		return false, err
	}
	return contentDeletable(c.contentFacts, now, level, r, queued, c.PayState == PaySubmitted), nil
}

// pruneBatch deletes, in one transaction, the tasks contentDeletable lets go among the next limit
// candidates after cur, adds the counts to out and moves cur past them. done is true when fewer than
// limit were read.
func (s *Store) pruneBatch(now int64, level ClockLevel, r Retention, cur *pruneCursor, limit int, out map[string]Pruned) (done bool, err error) {
	got := map[string]Pruned{}
	next := *cur
	err = s.Update(func(t *Tx) error {
		cs, err := candidates(t.tx, now, r, *cur, limit)
		if err != nil {
			return err
		}
		done = len(cs) < limit
		if len(cs) > 0 {
			next = pruneCursor{at: cs[len(cs)-1].StateAt, seq: cs[len(cs)-1].Seq}
		}
		byClass := map[string][]any{}
		for _, c := range cs {
			ok, err := c.deletable(t.tx, now, level, r)
			if err != nil {
				return err
			}
			if ok {
				byClass[c.Class] = append(byClass[c.Class], c.ID)
			}
		}
		for class, ids := range byClass {
			n, err := deleteTasks(t.tx, ids)
			if err != nil {
				return err
			}
			got[class] = n
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	*cur = next
	for class, n := range got {
		p := out[class]
		p.Interactions += n.Interactions
		p.Messages += n.Messages
		p.Attachments += n.Attachments
		out[class] = p
	}
	return done, nil
}

// deleteTasks deletes the tasks ids whole (FS-L8): the key a local end kept for one (CauseRetention),
// their attachments, messages and rows, and then their data keys, except one a held delegation of the
// same id still keeps its content under. It reports the rows deleted.
func deleteTasks(e execer, ids []any) (Pruned, error) {
	var n Pruned
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	if _, err := deleteIXKeysOf(e, in, ids); err != nil {
		return n, err
	}
	for _, d := range []struct {
		q   string
		out *int64
	}{
		{`DELETE FROM attachment WHERE interaction_id IN ` + in, &n.Attachments},
		{`DELETE FROM message WHERE interaction_id IN ` + in, &n.Messages},
		{`DELETE FROM interaction WHERE id IN ` + in, &n.Interactions},
	} {
		res, err := e.Exec(d.q, ids...)
		if err != nil {
			return n, err
		}
		if *d.out, err = res.RowsAffected(); err != nil {
			return n, err
		}
	}
	_, err := e.Exec(`DELETE FROM ix_dek WHERE ix IN `+in+`
	   AND NOT EXISTS (SELECT 1 FROM pending p WHERE p.ix = ix_dek.ix)
	   AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.ix = ix_dek.ix)`, ids...)
	return n, err
}

// CountNewlyDeletable is how many finished tasks the sweep at now would delete under the setting next
// and would not under cur, both at ClockFull: the number a change of content_retention is confirmed
// with ("deletes K tasks", FS §5.2). Lengthening the setting, or setting it to forever, counts 0. An
// invalid cur counts as keeping everything it can; an invalid next is ErrBadInput.
func (s *Store) CountNewlyDeletable(now int64, cur, next Retention) (int64, error) {
	return countNewlyDeletable(s.db, now, cur, next)
}

func countNewlyDeletable(q querier, now int64, cur, next Retention) (int64, error) {
	if !next.Valid() {
		return 0, fmt.Errorf("%w: a count needs a valid setting", ErrBadInput)
	}
	if !cur.Valid() {
		cur = RetentionForever
	}
	var n int64
	c := pruneCursor{at: math.MinInt64}
	for {
		cs, err := candidates(q, now, next, c, pruneBatch)
		if err != nil {
			return 0, err
		}
		for _, x := range cs {
			yes, err := x.deletable(q, now, ClockFull, next)
			if err != nil {
				return 0, err
			}
			if !yes {
				continue
			}
			already, err := x.deletable(q, now, ClockFull, cur)
			if err != nil {
				return 0, err
			}
			if !already {
				n++
			}
		}
		if len(cs) < pruneBatch {
			return n, nil
		}
		c = pruneCursor{at: cs[len(cs)-1].StateAt, seq: cs[len(cs)-1].Seq}
	}
}

// CountNewlyDeletableIn is CountNewlyDeletable on the store in dir while no daemon has it open
// (`anet retention set` with the daemon stopped, FS §5.2 step 3). The file is opened read-only and
// nothing in it is changed; a directory without a store counts 0, and a store this version cannot
// open is the error CheckStore gives.
func CountNewlyDeletableIn(dir string, now int64, cur, next Retention) (int64, error) {
	path := filepath.Join(dir, storeFile)
	if err := CheckStore(dir); err != nil {
		return 0, err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("interactions: %w", err)
	}
	p := filepath.ToSlash(path)
	if filepath.IsAbs(path) && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: p}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, fmt.Errorf("interactions: open %s: %w", path, err)
	}
	defer db.Close()
	return countNewlyDeletable(db, now, cur, next)
}

// OpenIDs is the set of tasks not finished yet: the ones whose exec work directory is still in use
// (the daemon removes the others' within a maintenance period, FS-L11).
func (s *Store) OpenIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM interaction WHERE state NOT IN ` + terminalSQL)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = true
	}
	return out, closeRows(rows)
}

// TruncateWAL runs PRAGMA wal_checkpoint(TRUNCATE) (FS-L8): the pages of the write-ahead log are
// written to the file and the log is cut to nothing, so the bytes of a key deleted since the last one
// are gone from the log as well (secure_delete has overwritten them in the file). It waits for readers
// at most checkpointWait. busyInARow is 0 when the log was truncated, and otherwise how many calls in a
// row have found a reader on an older snapshot: nothing is lost by trying again later, but a log that
// stays busy keeps deleted keys readable, which the caller reports.
func (s *Store) TruncateWAL() (busyInARow int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return s.walBusy, err
	}
	defer conn.Close()
	var was int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&was); err != nil {
		return s.walBusy, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = `+strconv.Itoa(checkpointWaitMS)); err != nil {
		return s.walBusy, err
	}
	// The connection goes back to the pool with the store's own busy timeout.
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA busy_timeout = `+strconv.Itoa(was)) }()
	var busy, frames, done int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done); err != nil {
		return s.walBusy, err
	}
	if busy != 0 {
		s.walBusy++
	} else {
		s.walBusy = 0
	}
	return s.walBusy, nil
}

// checkpointWaitMS is how long TruncateWAL waits for readers: the checkpoint holds off this store's
// writers while it waits, so it gives up early and the next period tries again.
const checkpointWaitMS = 1000

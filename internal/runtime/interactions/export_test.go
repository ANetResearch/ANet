package interactions

import "errors"

// SetPruneBatch sets the batch size of PruneTerminal for a test and returns
// the function that restores it.
func SetPruneBatch(n int) (restore func()) {
	old := pruneBatch
	pruneBatch = n
	return func() { pruneBatch = old }
}

// ExplainList is the query plan of the id query ListPage runs for f; ExplainCount
// that of Count. They let a test see that a listing reads an index, not the rows
// (whose goal, request_doc and result may be megabytes each).
func (s *Store) ExplainList(f ListFilter, limit int) ([]string, error) {
	q, args, err := f.pageSQL(limit)
	if err != nil {
		return nil, err
	}
	return s.explain(q, args)
}

func (s *Store) ExplainCount(f ListFilter) ([]string, error) {
	q, args, err := f.countSQL()
	if err != nil {
		return nil, err
	}
	return s.explain(q, args)
}

func (s *Store) explain(q string, args []any) ([]string, error) {
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, rows.Err()
}

// ExplainFindByClientKey is the query plan of FindByClientKey for q.
func (s *Store) ExplainFindByClientKey(q ClientKeyQuery) ([]string, error) {
	sql, args := q.findSQL()
	return s.explain(sql, args)
}

// ExplainWaiting is the query plan of Waiting.
func (s *Store) ExplainWaiting() ([]string, error) {
	return s.explain(waitingSQL, []any{string(RoleOutbound), string(StateSubmitted), int64(1), int64(0), "", 10})
}

// ExplainContextPeers is the query plan of ContextPeers.
func (s *Store) ExplainContextPeers() ([]string, error) {
	return s.explain(contextPeersSQL, []any{string(RoleOutbound), "c"})
}

// SetBetweenListSteps runs fn between the two reads of ListPage.
func SetBetweenListSteps(fn func()) (restore func()) {
	old := betweenListSteps
	betweenListSteps = fn
	return func() { betweenListSteps = old }
}

// CheckpointTruncate runs PRAGMA wal_checkpoint(TRUNCATE), as the daemon does
// after it deletes a key (FS-L8), so a test can read what the files hold.
func (s *Store) CheckpointTruncate() error {
	var busy, frames, done int
	if err := s.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("wal checkpoint: busy")
	}
	return nil
}

// SetCancelTombCaps lowers the cancel tombstone bounds for a test and
// returns the function that restores them.
func SetCancelTombCaps(perSender, total int) (restore func()) {
	old := cancelTombCaps
	cancelTombCaps.perSender, cancelTombCaps.total = perSender, total
	return func() { cancelTombCaps = old }
}

// CancelTombCount is the number of cancel tombstones stored, expired or not.
func (s *Store) CancelTombCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM cancel_tomb`).Scan(&n)
	return n, err
}

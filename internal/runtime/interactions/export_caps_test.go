package interactions

// Test access to the capability tables (caps.go): their schema as SQLite
// reports it, and raw statements for rows the typed writers refuse.

// TableColumn is one column of a table as PRAGMA table_info reports it.
type TableColumn struct {
	Name    string
	Type    string
	NotNull bool
	Default *string
	PK      bool
}

// TableColumns returns the columns of table in declaration order.
func (s *Store) TableColumns(table string) ([]TableColumn, error) {
	rows, err := s.db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableColumn
	for rows.Next() {
		var c TableColumn
		var notNull, pk int
		if err := rows.Scan(&c.Name, &c.Type, &notNull, &c.Default, &pk); err != nil {
			return nil, err
		}
		c.NotNull, c.PK = notNull != 0, pk != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// TableIndex is one index of a table: whether it is unique and its columns.
type TableIndex struct {
	Name    string
	Unique  bool
	Partial bool
	Columns []string
}

// TableIndexes returns the indexes of table, the automatic ones of PRIMARY
// KEY and UNIQUE constraints included.
func (s *Store) TableIndexes(table string) ([]TableIndex, error) {
	rows, err := s.db.Query(`SELECT name, "unique", partial FROM pragma_index_list(?)`, table)
	if err != nil {
		return nil, err
	}
	var out []TableIndex
	for rows.Next() {
		var ix TableIndex
		var unique, partial int
		if err := rows.Scan(&ix.Name, &unique, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		ix.Unique, ix.Partial = unique != 0, partial != 0
		out = append(out, ix)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cols, err := s.db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, out[i].Name)
		if err != nil {
			return nil, err
		}
		for cols.Next() {
			var n string
			if err := cols.Scan(&n); err != nil {
				cols.Close()
				return nil, err
			}
			out[i].Columns = append(out[i].Columns, n)
		}
		cols.Close()
	}
	return out, nil
}

// ExecRaw runs one statement on the store, bypassing the typed writers.
func (s *Store) ExecRaw(q string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(q, args...)
	return err
}

// CountRows returns the number of rows of table that match where ("" for
// all).
func (s *Store) CountRows(table, where string, args ...any) (int, error) {
	q := `SELECT COUNT(*) FROM ` + table
	if where != "" {
		q += ` WHERE ` + where
	}
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

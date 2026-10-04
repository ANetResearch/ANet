package interactions

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// The schema and user_version 3 commit in one transaction: a creation cut
// short leaves a file with no schema, which the next start takes as new —
// not a half store at user_version 0, which it would refuse as an earlier
// anet's.
func TestTheSchemaIsCreatedInOneTransaction(t *testing.T) {
	dir := t.TempDir()
	saved := schema
	schema = append(append([][]string{}, saved...), []string{`CREATE TABLE cut_short (`})
	_, err := Open(dir)
	schema = saved
	if err == nil || !strings.Contains(err.Error(), "create the schema") {
		t.Fatalf("Open with a failing statement: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, storeFile)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	version, objects, err := schemaState(db)
	db.Close()
	if err != nil || version != 0 || objects != 0 {
		t.Fatalf("after a failed creation: user_version %d, %d schema objects (%v); want 0 and 0", version, objects, err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("the next start: %v", err)
	}
	s.Close()
}

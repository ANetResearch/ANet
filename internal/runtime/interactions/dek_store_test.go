package interactions_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// dbHits returns the files of the store in dir (interactions.db, -wal, -shm) whose bytes contain
// needle.
func dbHits(t *testing.T, dir string, needle []byte) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "interactions.db*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no database files")
	}
	var hits []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, needle) {
			hits = append(hits, filepath.Base(f))
		}
	}
	return hits
}

// FS-L2: the content of a task is in no file of the database — not the main file, not the WAL, not
// the shared-memory file — through every path that writes it. Each encrypted column gets its own
// canary; a canary in a column that is not encrypted (context_id) is found, so the scan reads what
// the store wrote. Every value reads back as written.
func TestNoPlaintextContentInDB(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canary := func(col string) string { return "CANARY-" + col + "-5f0c1e93" }
	cb := func(col string) []byte { return []byte(canary(col)) }
	const plainCanary = "PLAIN-context-canary-9d2b77"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(s.Create(interactions.New{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: canary("goal"),
		RequestCID: "cid_req", RequestDoc: cb("request_doc"), ContextID: plainCanary}))
	must(s.Update(func(tx *interactions.Tx) error {
		return tx.Create(interactions.New{ID: "ix_2", Role: interactions.RoleInbound, PeerAID: "peer",
			Goal: canary("goal_tx"), RequestDoc: cb("request_doc_tx")})
	}))
	seq, _, err := s.AddMessageRecord(interactions.MessageRecord{InteractionID: "ix_1", SenderAID: "me",
		Kind: interactions.MsgText, Body: canary("body"), MsgID: "m1", Metadata: []byte(`{"k":"` + canary("metadata") + `"}`)})
	must(err)
	must(s.MergeMessageMeta("ix_1", seq, map[string]any{interactions.ClientMessageIDKey: canary("merged")}))
	must(s.Update(func(tx *interactions.Tx) error {
		_, _, err := tx.AddMessageRecord(interactions.MessageRecord{InteractionID: "ix_2", SenderAID: "peer",
			Kind: interactions.MsgText, Body: canary("body_tx")})
		return err
	}))
	big := bytes.Repeat(cb("att_data"), 2000) // overflow pages
	must(s.AddAttachment("ix_1", seq, interactions.Attachment{Name: canary("att_name"), Mime: canary("att_mime"),
		Size: int64(len(big)), CID: "bafy-att-1", Data: big}))
	must(s.Finish("ix_2", interactions.Finish{State: interactions.StateCompleted, Result: cb("result"), ResultCID: "cid_res",
		Receipt: []byte("RECEIPT"), Verified: interactions.VerificationVerified, Meta: []byte(`{"m":"` + canary("result_meta") + `"}`)}))
	must(s.Finish("ix_1", interactions.Finish{State: interactions.StateCanceled, Verified: interactions.VerificationUnknown}))
	if ok, err := s.SetLateResult("ix_1", cb("late_result"), "cid_late", []byte("RECEIPT2"), interactions.VerificationUnknown); err != nil || !ok {
		t.Fatalf("late result: %v %v", ok, err)
	}
	must(s.SetReview("ix_1", cb("review")))
	must(s.Update(func(tx *interactions.Tx) error {
		return tx.PutPending(interactions.PendingItem{IX: "ix_p", FromAID: "peer", ArrivedAt: 1, Delegate: cb("delegate"),
			Attachments: []interactions.PendingAttachment{{Name: canary("pending_att"), Mime: "text/plain", Size: 3, CID: "c"}}}, 0, 0)
	}))
	must(s.Update(func(tx *interactions.Tx) error {
		return tx.AppendPendingFollowup("ix_p", interactions.PendingFollowup{Kind: "text", Body: canary("followup"),
			MsgID: "f1", Metadata: json.RawMessage(`{"f":"` + canary("followup_meta") + `"}`), At: 2}, 0)
	}))
	oid, err := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_2", ToAID: "peer", Type: "anet.result/1",
		Body: cb("outbox_body"), MID: []byte("0123456789abcdef")})
	must(err)

	// Read back through the store.
	ix1, err := s.Get("ix_1")
	must(err)
	ix2, err := s.Get("ix_2")
	must(err)
	msgs, err := s.Messages("ix_1")
	must(err)
	att, err := s.AttachmentData("ix_1", "bafy-att-1")
	must(err)
	atts, err := s.Attachments("ix_1")
	must(err)
	p, err := s.GetPending("ix_p")
	must(err)
	ob, err := s.GetOutbox(oid)
	must(err)
	byID, err := s.MessageByMsgID("ix_1", "m1")
	must(err)
	for _, c := range []struct{ name, got, want string }{
		{"goal", ix1.Goal, canary("goal")},
		{"request_doc", string(ix1.RequestDoc), canary("request_doc")},
		{"goal (tx)", ix2.Goal, canary("goal_tx")},
		{"request_doc (tx)", string(ix2.RequestDoc), canary("request_doc_tx")},
		{"result", string(ix2.Result), canary("result")},
		{"result_meta", ix2.ResultMeta, `{"m":"` + canary("result_meta") + `"}`},
		{"late result", string(ix1.Result), canary("late_result")},
		{"review", string(ix1.Review), canary("review")},
		{"body", msgs[0].Body, canary("body")},
		{"metadata", msgs[0].Metadata, `{"a2a.messageId":"` + canary("merged") + `","k":"` + canary("metadata") + `"}`},
		{"by msg id", byID.Body, canary("body")},
		{"att name", att.Name, canary("att_name")},
		{"att mime", att.Mime, canary("att_mime")},
		{"att data", string(att.Data), string(big)},
		{"att list name", atts[0].Name, canary("att_name")},
		{"delegate", string(p.Delegate), canary("delegate")},
		{"pending att", p.Attachments[0].Name, canary("pending_att")},
		{"followup", p.Followups[0].Body, canary("followup")},
		{"followup meta", string(p.Followups[0].Metadata), `{"f":"` + canary("followup_meta") + `"}`},
		{"outbox body", string(ob.Body), canary("outbox_body")},
		{"context (plain)", ix1.ContextID, plainCanary},
	} {
		if c.got != c.want {
			t.Errorf("%s reads back %.80q, want %.80q", c.name, c.got, c.want)
		}
	}

	scan := func(when string) {
		t.Helper()
		if hits := dbHits(t, dir, []byte(plainCanary)); len(hits) == 0 {
			t.Fatalf("%s: the plain canary is in no file: the scan would prove nothing", when)
		}
		for _, needle := range []string{"CANARY-", "5f0c1e93"} {
			if hits := dbHits(t, dir, []byte(needle)); len(hits) != 0 {
				t.Errorf("%s: task content (%q) in %v", when, needle, hits)
			}
		}
	}
	scan("before a checkpoint")
	must(s.CheckpointTruncate())
	scan("after a checkpoint")
}

// sqlKeywords are the words that make a string literal SQL text for the scan below (they are
// written in upper case throughout the store).
var sqlKeywords = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "CREATE": true, "ALTER": true, "DROP": true,
	"PRAGMA": true, "WHERE": true, "AND": true, "OR": true, "ON": true, "SET": true, "FROM": true, "JOIN": true,
	"VALUES": true, "ORDER": true, "GROUP": true, "BY": true, "IN": true, "IS": true, "NOT": true, "LIKE": true,
	"GLOB": true, "EXISTS": true, "LIMIT": true, "CASE": true, "WHEN": true, "THEN": true, "INDEX": true,
	"HAVING": true, "INTO": true, "RETURNING": true, "BETWEEN": true, "MATCH": true,
}

// sqlTok is one token of SQL text.
type sqlTok struct {
	s     string
	ident bool
}

// sqlTokens splits SQL text into identifiers (bare or double-quoted) and single punctuation
// characters; string literals in single quotes are dropped.
func sqlTokens(s string) []sqlTok {
	var out []sqlTok
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			i = j + 1
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			out = append(out, sqlTok{s[i+1 : min(j, len(s))], true})
			i = j + 1
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, sqlTok{s[i:j], true})
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			out = append(out, sqlTok{string(c), false})
			i++
		}
	}
	return out
}

// sqlViolations returns the encrypted column names (in enc) that the SQL text s uses anywhere but
// where a column's value is only read or written: a result column of SELECT or RETURNING, a column
// list of INSERT, the target of an assignment in UPDATE … SET, a column definition (CREATE TABLE,
// ALTER TABLE … ADD COLUMN), or a constant that is a bare column list. Everywhere else — WHERE, ON,
// ORDER BY, GROUP BY, HAVING, CASE, an index definition, an expression — is a predicate, an
// ordering or an index over the column (FS-L3). Text that does not start a statement is judged
// strictly: a fragment such as "AND goal = ?" is a predicate.
func sqlViolations(s string, enc map[string]bool) []string {
	toks := sqlTokens(s)
	sqlish, pureList := false, true
	for _, tk := range toks {
		switch {
		case tk.ident && sqlKeywords[tk.s]:
			sqlish, pureList = true, false
		case !tk.ident && strings.Contains("?=<>!", tk.s):
			sqlish, pureList = true, false
		case !tk.ident && tk.s != "," && tk.s != ".":
			pureList = false
		}
	}
	if !sqlish || pureList {
		return nil
	}
	// A table definition (CREATE TABLE, ALTER TABLE … ADD COLUMN) defines columns; an index
	// definition is judged like any other text.
	if first := toks[0]; first.ident && (first.s == "CREATE" || first.s == "ALTER") {
		index := false
		for _, u := range toks {
			index = index || u.ident && u.s == "INDEX"
		}
		if !index {
			return nil
		}
	}
	const (
		ctxUnknown = iota
		ctxRead    // SELECT, RETURNING result columns
		ctxTable   // FROM, JOIN, UPDATE, INTO table names
		ctxInsert  // INSERT INTO t( … )
		ctxValues
		ctxSet
		ctxPred
	)
	ctx := ctxUnknown
	var bad []string
	for i, tk := range toks {
		if !tk.ident {
			if tk.s == "(" && ctx == ctxTable && i > 0 && toks[i-1].ident && i > 1 && toks[i-2].ident && toks[i-2].s == "INTO" {
				ctx = ctxInsert
			}
			if tk.s == ")" && ctx == ctxInsert {
				ctx = ctxUnknown
			}
			continue
		}
		switch tk.s {
		case "SELECT", "RETURNING":
			ctx = ctxRead
			continue
		case "FROM", "JOIN", "UPDATE", "INTO", "TABLE":
			ctx = ctxTable
			continue
		case "VALUES":
			ctx = ctxValues
			continue
		case "SET":
			ctx = ctxSet
			continue
		case "WHERE", "ON", "AND", "OR", "HAVING", "WHEN", "THEN", "ELSE", "CASE", "BY", "IN", "IS", "NOT", "LIKE",
			"GLOB", "EXISTS", "BETWEEN", "LIMIT", "OFFSET", "USING", "INDEXED", "INDEX", "MATCH", "DISTINCT":
			if ctx == ctxRead && tk.s == "DISTINCT" {
				continue
			}
			ctx = ctxPred
			continue
		}
		if !enc[tk.s] {
			continue
		}
		prevComma := i > 0 && !toks[i-1].ident && toks[i-1].s == ","
		nextAssign := i+1 < len(toks) && !toks[i+1].ident && toks[i+1].s == "=" &&
			(i+2 >= len(toks) || toks[i+2].ident || toks[i+2].s != "=")
		switch ctx {
		case ctxRead, ctxInsert, ctxTable:
			continue
		case ctxSet:
			if nextAssign {
				continue
			}
		case ctxUnknown:
			if prevComma && nextAssign {
				continue // ", col = ?" continuing an UPDATE … SET
			}
		}
		bad = append(bad, tk.s)
	}
	return bad
}

// encryptedNames is the set of bare names of the encrypted columns.
func encryptedNames() map[string]bool {
	m := map[string]bool{}
	for _, c := range interactions.EncryptedColumns() {
		_, col, _ := strings.Cut(string(c), ".")
		m[col] = true
	}
	return m
}

// FS-L3: no encrypted column appears in an SQL predicate, ordering or index of the store. The
// store's own SQL text (every string literal of its non-test files) is scanned, and the indexes of
// a new database are read back from SQLite.
func TestEncryptedColumnsNotInSQLPredicates(t *testing.T) {
	enc := encryptedNames()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	literals := 0
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			tags := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				if fl, ok := n.(*ast.Field); ok && fl.Tag != nil {
					tags[fl.Tag] = true
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || tags[lit] {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
				}
				literals++
				if bad := sqlViolations(s, enc); len(bad) > 0 {
					t.Errorf("%s: SQL uses the encrypted column(s) %v outside a value list (FS-L3): %.120q",
						filepath.Base(name)+":"+strconv.Itoa(fset.Position(lit.Pos()).Line), bad, s)
				}
				return true
			})
		}
	}
	if literals < 100 {
		t.Fatalf("only %d string literals scanned: the scan is not reading the store", literals)
	}

	// The scanner itself: each of these must be reported, and none of the allowed forms.
	for _, c := range []struct {
		sql  string
		want bool
	}{
		{`SELECT seq FROM message WHERE body = ?`, true},
		{`SELECT seq FROM message m WHERE m.metadata LIKE '%x%'`, true},
		{`SELECT id FROM interaction ORDER BY goal`, true},
		{`CREATE INDEX IF NOT EXISTS i ON message(metadata)`, true},
		{`CREATE INDEX IF NOT EXISTS i ON message(seq) WHERE length(body) > 0`, true},
		{`(CASE WHEN json_valid(m.metadata) THEN 1 END)`, true},
		{`metadata) THEN json_extract(`, true},
		{` AND goal = ?`, true},
		{`goal != ''`, true},
		{`SELECT id FROM interaction GROUP BY result HAVING 1`, true},
		{`UPDATE message SET metadata=? WHERE metadata=?`, true},
		{`SELECT seq,body,metadata FROM message WHERE interaction_id=? ORDER BY seq`, false},
		{`INSERT INTO message(interaction_id,body,metadata) VALUES(?,?,?)`, false},
		{`UPDATE interaction SET result=?, result_meta=?, updated_at=? WHERE id=?`, false},
		{`, review=?`, false},
		{`seq,id,goal,request_doc,result`, false},
		{`CREATE TABLE IF NOT EXISTS message (seq INTEGER, body BLOB NOT NULL DEFAULT X'')`, false},
		{`interactions: message %d metadata: %w`, false},
	} {
		if got := len(sqlViolations(c.sql, enc)) > 0; got != c.want {
			t.Errorf("scanner on %q: reported %v, want %v", c.sql, got, c.want)
		}
	}

	// The indexes SQLite holds for a new store: none covers, orders by or filters on an encrypted
	// column of its table.
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	byTable := map[string]map[string]bool{}
	for _, c := range interactions.EncryptedColumns() {
		tbl, col, _ := strings.Cut(string(c), ".")
		if byTable[tbl] == nil {
			byTable[tbl] = map[string]bool{}
		}
		byTable[tbl][col] = true
	}
	rows, err := db.Query(`SELECT name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatal(err)
	}
	type idx struct{ name, tbl, sql string }
	var all []idx
	for rows.Next() {
		var i idx
		if err := rows.Scan(&i.name, &i.tbl, &i.sql); err != nil {
			t.Fatal(err)
		}
		all = append(all, i)
	}
	rows.Close()
	if len(all) < 10 {
		t.Fatalf("only %d indexes read", len(all))
	}
	for _, i := range all {
		cols := byTable[i.tbl]
		for _, tk := range sqlTokens(i.sql) {
			if tk.ident && cols[tk.s] {
				t.Errorf("index %s on %s names the encrypted column %s: %s", i.name, i.tbl, tk.s, i.sql)
			}
		}
		xr, err := db.Query(`SELECT COALESCE(name, '') FROM pragma_index_xinfo(?)`, i.name)
		if err != nil {
			t.Fatal(err)
		}
		for xr.Next() {
			var c string
			if err := xr.Scan(&c); err != nil {
				t.Fatal(err)
			}
			if cols[c] {
				t.Errorf("index %s on %s covers the encrypted column %s", i.name, i.tbl, c)
			}
		}
		xr.Close()
	}
}

// FS-L8 (mutation 16, the data-key half): once a task's data key is deleted and the WAL
// checkpointed, the key's bytes are in none of the database's files. The store holds enough tasks
// for the key table to span several pages, so rows move between pages while it fills.
func TestDeletedKeyBytesAbsentFromDBFiles_DataKey(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const n = 300
	for i := 0; i < n; i++ {
		if err := s.Create(interactions.New{ID: fmt.Sprintf("ix_%03d", i), Role: interactions.RoleInbound, PeerAID: "peer",
			Goal: "g"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		return tx.PutPending(interactions.PendingItem{IX: "ix_held", FromAID: "peer", ArrivedAt: 1, Delegate: []byte("D")}, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	keyOf := func(ix string) []byte {
		t.Helper()
		k, err := s.DEK(ix)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Wipe()
		return k.Bytes()
	}
	victims := map[string][]byte{"ix_000": keyOf("ix_000"), "ix_150": keyOf("ix_150"), "ix_299": keyOf("ix_299"), "ix_held": keyOf("ix_held")}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatal(err)
	}
	for ix, k := range victims {
		if hits := dbHits(t, dir, k); len(hits) == 0 {
			t.Fatalf("the key of %s is not in the database file: the test would prove nothing", ix)
		}
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		for _, ix := range []string{"ix_000", "ix_150", "ix_299"} {
			if ok, err := tx.DeleteDEK(ix); err != nil || !ok {
				return fmt.Errorf("delete %s: %v %v", ix, ok, err)
			}
		}
		ok, err := tx.DeletePending("ix_held")
		if err == nil && !ok {
			err = errors.New("no held item")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for ix := range victims {
		names = append(names, ix)
	}
	sort.Strings(names)
	for _, ix := range names {
		if hits := dbHits(t, dir, victims[ix]); len(hits) != 0 {
			t.Errorf("the deleted key of %s is still in %v", ix, hits)
		}
		if _, err := s.DEK(ix); !errors.Is(err, interactions.ErrContentGone) {
			t.Errorf("key of %s after deletion: %v", ix, err)
		}
	}
	if k := keyOf("ix_001"); len(dbHits(t, dir, k)) == 0 {
		t.Fatal("a key that was not deleted is gone from the files")
	}
}

// FS-L4: a task whose data key is gone reads as deleted: ErrContentGone, which is ErrNotFound to a
// caller that does not ask; a listing leaves it out; nothing is written under it.
func TestContentGoneReadsAsNotFound(t *testing.T) {
	s := open(t)
	for _, id := range []string{"ix_a", "ix_b"} {
		if err := s.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "goal of " + id}); err != nil {
			t.Fatal(err)
		}
	}
	seq, err := s.AddMessage("ix_a", "me", interactions.MsgText, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddAttachment("ix_a", seq, interactions.Attachment{Name: "a.txt", Mime: "text/plain", Size: 1, CID: "c1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	oid, err := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_a", ToAID: "peer", Type: "anet.message/1", Body: []byte("B")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.PutPending(interactions.PendingItem{IX: "ix_p", FromAID: "peer", ArrivedAt: 1, Delegate: []byte("D")}, 0, 0); err != nil {
			return err
		}
		if _, err := tx.DeleteDEK("ix_a"); err != nil {
			return err
		}
		_, err := tx.DeleteDEK("ix_p")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	gone := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, interactions.ErrContentGone) || !errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("%s: %v, want ErrContentGone (an ErrNotFound)", what, err)
		}
	}
	_, err = s.Get("ix_a")
	gone("Get", err)
	_, err = s.GetFor("ix_a", interactions.RoleOutbound, "peer")
	gone("GetFor", err)
	_, err = s.Messages("ix_a")
	gone("Messages", err)
	_, err = s.MessageByMsgID("ix_a", "nope")
	if !errors.Is(err, interactions.ErrNotFound) {
		t.Errorf("MessageByMsgID: %v", err)
	}
	_, err = s.Attachments("ix_a")
	gone("Attachments", err)
	_, err = s.AttachmentData("ix_a", "c1")
	gone("AttachmentData", err)
	_, err = s.GetOutbox(oid)
	gone("GetOutbox", err)
	_, err = s.GetPending("ix_p")
	gone("GetPending", err)
	_, err = s.AddMessage("ix_a", "me", interactions.MsgText, "more")
	gone("AddMessage", err)
	gone("Finish", s.Finish("ix_a", interactions.Finish{State: interactions.StateCompleted, Result: []byte("r"),
		Verified: interactions.VerificationUnknown}))
	gone("SetReview", s.SetReview("ix_a", []byte("rv")))
	if _, err := s.Get("ix_never"); !errors.Is(err, interactions.ErrNotFound) || errors.Is(err, interactions.ErrContentGone) {
		t.Errorf("Get of an id never stored: %v, want plain ErrNotFound", err)
	}
	if err := s.Finish("ix_never", interactions.Finish{State: interactions.StateCompleted, Verified: interactions.VerificationUnknown}); !errors.Is(err, interactions.ErrNotFound) || errors.Is(err, interactions.ErrContentGone) {
		t.Errorf("Finish of an id never stored: %v, want plain ErrNotFound", err)
	}
	list, err := s.List(interactions.RoleOutbound, "", 0, 0)
	if err != nil || len(list) != 1 || list[0].ID != "ix_b" {
		t.Fatalf("List = %v, %v; want ix_b alone", list, err)
	}
	page, err := s.ListPage(interactions.ListFilter{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "ix_b" {
		t.Fatalf("ListPage = %v, %v; want ix_b alone", page.Items, err)
	}
	if held, err := s.ListPending(); err != nil || len(held) != 0 {
		t.Fatalf("ListPending = %v, %v; want nothing", held, err)
	}
}

// FS-L1, FS-L9: a data key is created in the transaction that creates its row, and only then; an
// approval hands a held delegation's key to its interaction; a held item rejected, expired or
// canceled takes its key with it; ReleaseDEK keeps a key something still needs.
func TestDEKLivesAndDiesWithItsRow(t *testing.T) {
	s := open(t)
	macOf := func(ix string) []byte {
		t.Helper()
		k, err := s.MACKey(ix)
		if err != nil {
			t.Fatalf("mac key of %s: %v", ix, err)
		}
		return k
	}
	rollback := errors.New("roll back")
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: "ix_rb", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g"}); err != nil {
			return err
		}
		if _, err := tx.DEK("ix_rb"); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err := s.DEK("ix_rb"); !errors.Is(err, interactions.ErrContentGone) {
		t.Fatalf("a rolled-back creation left a key: %v", err)
	}
	if err := s.Create(interactions.New{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	first := macOf("ix_1")
	if err := s.Create(interactions.New{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "other"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, macOf("ix_1")) {
		t.Fatal("a repeated creation replaced the task's key")
	}
	if ix, _ := s.Get("ix_1"); ix.Goal != "g" {
		t.Fatalf("a repeated creation changed the goal to %q", ix.Goal)
	}
	put := func(ix string) {
		t.Helper()
		if err := s.Update(func(tx *interactions.Tx) error {
			return tx.PutPending(interactions.PendingItem{IX: ix, FromAID: "peer", ArrivedAt: 1, Delegate: []byte("D " + ix)}, 0, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Approval: the held item's key becomes the interaction's.
	put("ix_ok")
	held := macOf("ix_ok")
	if err := s.Update(func(tx *interactions.Tx) error {
		ok, err := tx.TakePending("ix_ok")
		if err != nil || !ok {
			return fmt.Errorf("take: %v %v", ok, err)
		}
		return tx.Create(interactions.New{ID: "ix_ok", Role: interactions.RoleInbound, PeerAID: "peer", Goal: "approved"})
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(held, macOf("ix_ok")) {
		t.Fatal("the approved interaction has another key than its held delegation")
	}
	if ix, err := s.Get("ix_ok"); err != nil || ix.Goal != "approved" {
		t.Fatalf("approved interaction: %v %v", ix, err)
	}
	// Rejection, expiry, cancel: the key goes with the item.
	put("ix_no")
	if ok, err := s.DeletePending("ix_no"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.DEK("ix_no"); !errors.Is(err, interactions.ErrContentGone) {
		t.Fatalf("a deleted held item kept its key: %v", err)
	}
	// ReleaseDEK keeps the key of a task with a row; DeleteDEK does not ask.
	if err := s.Update(func(tx *interactions.Tx) error {
		if ok, err := tx.ReleaseDEK("ix_1"); err != nil || ok {
			return fmt.Errorf("release of a task with a row: %v %v", ok, err)
		}
		if _, err := tx.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.message/1", Body: []byte("B")}); err != nil {
			return err
		}
		if ok, err := tx.DeleteDEK("ix_none"); err != nil || ok {
			return fmt.Errorf("delete of no key: %v %v", ok, err)
		}
		if ok, err := tx.DeleteDEK("ix_1"); err != nil || !ok {
			return fmt.Errorf("delete: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DEK("ix_1"); !errors.Is(err, interactions.ErrContentGone) {
		t.Fatalf("DeleteDEK left the key: %v", err)
	}
}

// The aad binds a stored field to its task and its column: a field copied to another row or column
// does not open, and the store answers an error — never another task's content.
func TestFieldsBoundToTheirRow(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"ix_1", "ix_2"} {
		if err := s.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: "peer", Goal: "goal of " + id,
			RequestDoc: []byte("doc of " + id)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMessage(id, "me", interactions.MsgText, "body of "+id); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db")+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`UPDATE interaction SET goal = (SELECT goal FROM interaction WHERE id = 'ix_1') WHERE id = 'ix_2'`,
		`UPDATE interaction SET request_doc = (SELECT goal FROM interaction WHERE id = 'ix_1') WHERE id = 'ix_1'`,
		`UPDATE message SET metadata = body WHERE interaction_id = 'ix_1'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"ix_1", "ix_2"} {
		if ix, err := s.Get(id); err == nil || errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("Get(%s) = %+v, %v; want an error that is not ErrNotFound", id, ix, err)
		}
	}
	if msgs, err := s.Messages("ix_1"); err == nil {
		t.Errorf("Messages with a body copied into the metadata column: %+v", msgs)
	}
	if msgs, err := s.Messages("ix_2"); err != nil || msgs[0].Body != "body of ix_2" {
		t.Errorf("an untouched task: %+v %v", msgs, err)
	}
}

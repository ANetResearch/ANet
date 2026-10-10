package interactions

// outbox_v3.go is the shape of the outbound queue of v0.3.0 (engine §2.1,
// core §2.16): a row per message to deliver, and a version per sealing of
// it, each with the ack_key its delivery token is checked with.
// v030-todo(ENG-1a): outboxSchemaV3 replaces outboxSchema, and the store's
// methods move to it.

// OutboxRow is one outbox row as read, without its body and envelope.
type OutboxRow struct {
	ID int64
	// IX, ToAID, Type and Kind say what the row is: the interaction, the
	// recipient, the inner type and the engine's row kind (the ChatMsg kind
	// of a message, the state or notice of a status).
	IX, ToAID, Type, Kind string
	// MID is the inner message id every version carries (16 bytes).
	MID []byte
	// TS and Exp are the inner ts and exp (unix ms), fixed at enqueue.
	TS, Exp uint64
	// Attempts counts failed attempts; NextAt is the backoff (unix ms).
	Attempts int
	NextAt   int64
	// LastError is a code, then ": " and the detail (engine ENG-S4).
	LastError string
	CreatedAt int64
	// HandedAt is when a byte of the row first left the process; 0 never.
	HandedAt int64
	// Attempting: a version was written and may be on its way (A8).
	Attempting bool
	// Frozen: the delegation's task was canceled; not transmitted again.
	Frozen bool
	// CheckAt is the next status query (unix ms), 0 for none; CheckN the
	// queued answers since the last hand-over.
	CheckAt int64
	CheckN  int
	// CurVer is the current version, 0 before the first sealing.
	CurVer uint64
	// SealedKID and SealedKidNA are the recipient's epoch key the current
	// version is sealed to and its not_after; SealedKSS and SealedNoticeAt
	// the key state it was signed under and its notice's observed_at.
	SealedKID      []byte
	SealedKidNA    uint64
	SealedKSS      uint64
	SealedNoticeAt uint64
}

// OutboxVer is one version of a row (outbox_ver).
type OutboxVer struct {
	RowID int64
	Ver   uint64
	// AckKey is the version's first-layer export (seal.AckKey bytes).
	AckKey []byte
	// HandedAt is when it was first handed over; 0 never.
	HandedAt int64
	// RID is the hub's id for it; nil when no hub took it with one.
	RID []byte
	// Hub is the hub (URL) it was sent to; ViaHub the hub it was queued at
	// when another (DLV-W-12a).
	Hub, ViaHub string
	// HubState is the last status answer: "", "queued" or "unknown".
	HubState string
}

// outboxSchemaV3 is the v0.3.0 outbound queue. The columns are engine
// §2.1's and no others (ENG-S1): no access column, the credential is made
// again for every attempt (ENG-I2).
var outboxSchemaV3 = []string{
	`CREATE TABLE outbox (
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ix TEXT NOT NULL,
	   to_aid TEXT NOT NULL,
	   typ TEXT NOT NULL,
	   kind TEXT NOT NULL DEFAULT '',
	   mid BLOB NOT NULL,
	   digest BLOB NOT NULL,
	   body BLOB,
	   envelope BLOB,
	   cur_ver INTEGER NOT NULL DEFAULT 0,
	   ts INTEGER NOT NULL,
	   exp INTEGER NOT NULL,
	   attempts INTEGER NOT NULL DEFAULT 0,
	   next_at INTEGER NOT NULL DEFAULT 0,
	   last_error TEXT NOT NULL DEFAULT '',
	   created_at INTEGER NOT NULL,
	   handed_at INTEGER,
	   attempting INTEGER NOT NULL DEFAULT 0,
	   frozen INTEGER NOT NULL DEFAULT 0,
	   check_at INTEGER,
	   check_n INTEGER NOT NULL DEFAULT 0,
	   sealed_kid BLOB,
	   sealed_kid_na INTEGER NOT NULL DEFAULT 0,
	   sealed_kss INTEGER NOT NULL DEFAULT 0,
	   sealed_notice_at INTEGER NOT NULL DEFAULT 0
	 )`,
	`CREATE INDEX idx_outbox_next ON outbox(next_at)`,
	`CREATE INDEX idx_outbox_check ON outbox(check_at) WHERE check_at IS NOT NULL`,
	`CREATE INDEX idx_outbox_exp ON outbox(exp)`,
	`CREATE INDEX idx_outbox_chain ON outbox(ix, to_aid, id)`,
	`CREATE INDEX idx_outbox_dedupe ON outbox(ix, to_aid, typ, digest)`,
	`CREATE TABLE outbox_ver (
	   row_id INTEGER NOT NULL,
	   ver INTEGER NOT NULL,
	   ack_key BLOB NOT NULL,
	   handed_at INTEGER,
	   rid BLOB,
	   via_hub TEXT NOT NULL DEFAULT '',
	   hub TEXT NOT NULL DEFAULT '',
	   hub_state TEXT NOT NULL DEFAULT '',
	   PRIMARY KEY(row_id, ver)
	 )`,
}

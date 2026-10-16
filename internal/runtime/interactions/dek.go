package interactions

// dek.go is the local data key of a task (FS §4.6.1): every interaction and
// every held delegation has one, 32 random bytes in the ix_dek table, created
// in the transaction that creates the row. The task's content — what was
// asked, said, attached and answered (FS-L2, the Column constants below) —
// is stored only as ciphertext under it, so deleting the one key row makes
// all of it unreadable at once, whatever copies of the pages are left.
//
// The key is only ever a root (core CORE-L2, L36):
//
//	aead_key = HKDF-SHA256(ikm = ix_dek, salt = empty, info = "anet-local-aead/v1", L = 32)
//	mac_key  = HKDF-SHA256(ikm = ix_dek, salt = empty, info = "anet-local-mac/v1",  L = 32)
//
// and a stored field is (core L35)
//
//	0x01 || nonce(12) || AES-256-GCM(aead_key, nonce, plaintext,
//	                                 aad = coredet({1: "anet-local/v1", 2: ix, 3: "<table>.<column>"}))
//
// The aad names the task and the column, so a field moved to another row
// or another column does not open. The vector LOCAL-1
// (testdata/vec-local-1.json) pins every byte of it.
//
// Encryption and decryption happen inside this package: callers read and
// write plaintext, as before. A row whose data key is gone reads as
// ErrContentGone, which is ErrNotFound to every caller that does not ask.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/coredet"
)

// DEKSize is the length of a data key in bytes.
const DEKSize = 32

// The labels of core L35 and L36.
const (
	localFieldLabel = "anet-local/v1"      // L35: the type label inside the aad
	localAEADInfo   = "anet-local-aead/v1" // L36: info of aead_key
	localMACInfo    = "anet-local-mac/v1"  // L36: info of mac_key
)

// A stored field is fieldVersion, a nonce and the AES-256-GCM output.
const (
	fieldVersion   = 0x01
	fieldNonceSize = 12
	fieldTagSize   = 16
	// FieldOverhead is how many bytes a stored field is longer than its
	// plaintext.
	FieldOverhead = 1 + fieldNonceSize + fieldTagSize
)

// Column names an encrypted column as "<table>.<column>": the name that goes
// into the aad of its fields.
type Column string

// The columns stored only as data-key ciphertext (FS-L2). No SQL predicate,
// ordering or index may name them (FS-L3).
const (
	ColGoal               Column = "interaction.goal"
	ColRequestDoc         Column = "interaction.request_doc"
	ColResult             Column = "interaction.result"
	ColResultMeta         Column = "interaction.result_meta"
	ColReview             Column = "interaction.review"
	ColMessageBody        Column = "message.body"
	ColMessageMetadata    Column = "message.metadata"
	ColAttachmentName     Column = "attachment.name"
	ColAttachmentMime     Column = "attachment.mime"
	ColAttachmentData     Column = "attachment.data"
	ColPendingDelegate    Column = "pending.delegate"
	ColPendingAttachments Column = "pending.attachments"
	ColPendingFollowups   Column = "pending.followups"
	ColOutboxBody         Column = "outbox.body"
)

// EncryptedColumns returns the encrypted columns in the order FS-L2 lists
// them.
func EncryptedColumns() []Column {
	return []Column{ColGoal, ColRequestDoc, ColResult, ColResultMeta, ColReview,
		ColMessageBody, ColMessageMetadata, ColAttachmentName, ColAttachmentMime, ColAttachmentData,
		ColPendingDelegate, ColPendingAttachments, ColPendingFollowups, ColOutboxBody}
}

// Valid reports whether c is one of the encrypted columns.
func (c Column) Valid() bool {
	for _, e := range EncryptedColumns() {
		if c == e {
			return true
		}
	}
	return false
}

// ErrContentGone is returned for a task or held delegation whose data key is
// gone: its content was deleted (FS-L4), typically by a retention sweep that
// ran while it was being read. It matches ErrNotFound under errors.Is, so a
// caller that answers "no such task" for ErrNotFound answers the same here —
// the user sees no_local_task — and a caller that needs to tell the two
// apart asks for ErrContentGone.
var ErrContentGone = fmt.Errorf("%w: its content was deleted", ErrNotFound)

// errFieldCorrupt is a stored field that does not open under its task's
// data key: a damaged file, or a field copied from another task or column.
var errFieldCorrupt = errors.New("interactions: stored field does not open under its task's data key")

// DEK is one task's data key (ix_dek). Its bytes never leave this package
// except through the derivations: MACKey, and the field codec SealField and
// OpenField. Wipe zeroes it.
type DEK struct {
	k [DEKSize]byte
}

// NewDEK returns a fresh data key from crypto/rand.
func NewDEK() (*DEK, error) {
	d := new(DEK)
	if _, err := rand.Read(d.k[:]); err != nil {
		return nil, fmt.Errorf("interactions: new data key: %w", err)
	}
	return d, nil
}

// dekFrom returns the data key stored as b, which must be DEKSize bytes.
func dekFrom(b []byte) (*DEK, error) {
	if len(b) != DEKSize {
		return nil, fmt.Errorf("interactions: stored data key is %d bytes, want %d", len(b), DEKSize)
	}
	d := new(DEK)
	copy(d.k[:], b)
	return d, nil
}

// Wipe zeroes the key. The DEK must not be used afterwards.
func (k *DEK) Wipe() {
	if k != nil {
		clear(k.k[:])
	}
}

// derive is HKDF-SHA256(ikm = the key, salt = empty, info, L = 32).
func (k *DEK) derive(info string) []byte {
	out, err := hkdf.Key(sha256.New, k.k[:], nil, info, 32)
	if err != nil {
		// HKDF-SHA256 refuses only lengths above 255·32 bytes.
		panic("interactions: hkdf: " + err.Error())
	}
	return out
}

// MACKey is the task's client-digest key (core L36, "anet-local-mac/v1"):
// the HMAC key of client_digest (core L38, CUX-D1). The data key itself is
// never a MAC key (CORE-L2).
func (k *DEK) MACKey() []byte { return k.derive(localMACInfo) }

// aeadKey is the task's field key (core L36, "anet-local-aead/v1").
func (k *DEK) aeadKey() []byte { return k.derive(localAEADInfo) }

// SealField encrypts one value of column col of task ix (core L35) under a
// fresh random nonce and returns the bytes to store.
func (k *DEK) SealField(ix string, col Column, plaintext []byte) ([]byte, error) {
	b, err := k.box(ix)
	if err != nil {
		return nil, err
	}
	return b.sealField(col, plaintext)
}

// OpenField decrypts a value SealField stored for column col of task ix. It
// fails for anything else: another task's or column's field, a changed
// byte, a version other than 1.
func (k *DEK) OpenField(ix string, col Column, stored []byte) ([]byte, error) {
	b, err := k.box(ix)
	if err != nil {
		return nil, err
	}
	return b.openField(col, stored)
}

// fieldBox is the field cipher of one task: AES-256-GCM under its aead_key,
// built once for every field of a row.
type fieldBox struct {
	ix   string
	aead cipher.AEAD
}

// box builds the field cipher of task ix.
func (k *DEK) box(ix string) (*fieldBox, error) {
	if ix == "" || !utf8.ValidString(ix) {
		return nil, fmt.Errorf("%w: a task id (valid UTF-8) is required to seal or open a field", ErrBadInput)
	}
	key := k.aeadKey()
	defer clear(key)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &fieldBox{ix: ix, aead: g}, nil
}

// fieldAAD is the aad of a field of column col of task ix (core L35).
func fieldAAD(ix string, col Column) ([]byte, error) {
	return coredet.Marshal(map[uint64]string{1: localFieldLabel, 2: ix, 3: string(col)})
}

// sealField is SealField with this box's task.
func (b *fieldBox) sealField(col Column, plaintext []byte) ([]byte, error) {
	var nonce [fieldNonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("interactions: field nonce: %w", err)
	}
	return b.sealFieldNonce(col, nonce[:], plaintext)
}

// sealFieldNonce is sealField with the nonce given: the golden vector fixes
// it, nothing else may.
func (b *fieldBox) sealFieldNonce(col Column, nonce, plaintext []byte) ([]byte, error) {
	if !col.Valid() {
		return nil, fmt.Errorf("%w: %q is not an encrypted column", ErrBadInput, col)
	}
	if len(nonce) != fieldNonceSize {
		return nil, fmt.Errorf("%w: field nonce is %d bytes, want %d", ErrBadInput, len(nonce), fieldNonceSize)
	}
	aad, err := fieldAAD(b.ix, col)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(plaintext)+FieldOverhead)
	out = append(out, fieldVersion)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, aad), nil
}

// openField is OpenField with this box's task.
func (b *fieldBox) openField(col Column, stored []byte) ([]byte, error) {
	if !col.Valid() {
		return nil, fmt.Errorf("%w: %q is not an encrypted column", ErrBadInput, col)
	}
	if len(stored) < FieldOverhead || stored[0] != fieldVersion {
		return nil, fmt.Errorf("%w (%s of %s)", errFieldCorrupt, col, b.ix)
	}
	aad, err := fieldAAD(b.ix, col)
	if err != nil {
		return nil, err
	}
	nonce := stored[1 : 1+fieldNonceSize]
	pt, err := b.aead.Open(nil, nonce, stored[1+fieldNonceSize:], aad)
	if err != nil {
		return nil, fmt.Errorf("%w (%s of %s)", errFieldCorrupt, col, b.ix)
	}
	if pt == nil {
		pt = []byte{}
	}
	return pt, nil
}

// dekSchema is the data-key table (FS §2.11). A key row is only ever
// inserted (with the row it belongs to) and deleted; the database is opened
// with secure_delete, so a deleted key's bytes are overwritten in the file.
var dekSchema = []string{
	`CREATE TABLE ix_dek (
	   ix TEXT PRIMARY KEY,
	   key BLOB NOT NULL,
	   created_at INTEGER NOT NULL
	 )`,
}

// loadDEK reads task ix's data key, or ErrContentGone when it has none.
func loadDEK(e execer, ix string) (*DEK, error) {
	var b []byte
	err := e.QueryRow(`SELECT key FROM ix_dek WHERE ix=?`, ix).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContentGone
	}
	if err != nil {
		return nil, err
	}
	defer clear(b)
	return dekFrom(b)
}

// boxOf returns task ix's field cipher, or ErrContentGone when it has no
// data key.
func boxOf(e execer, ix string) (*fieldBox, error) {
	k, err := loadDEK(e, ix)
	if err != nil {
		return nil, err
	}
	defer k.Wipe()
	return k.box(ix)
}

// rowBox is boxOf for a write to interaction id: ErrNotFound when there is
// no such interaction (and no key), ErrContentGone when the row is there and
// its key is not.
func rowBox(e execer, id string) (*fieldBox, error) {
	b, err := boxOf(e, id)
	if !errors.Is(err, ErrContentGone) {
		return b, err
	}
	var one int
	switch qerr := e.QueryRow(`SELECT 1 FROM interaction WHERE id=?`, id).Scan(&one); {
	case errors.Is(qerr, sql.ErrNoRows):
		return nil, ErrNotFound
	case qerr != nil:
		return nil, qerr
	}
	return nil, ErrContentGone
}

// ensureDEK gives task ix a fresh data key unless it has one (FS-L1: the
// key is created in the transaction that creates the row) and returns its
// field cipher. A key already there is kept: a repeated creation of the
// same task, or an approved delegation whose interaction takes over the key
// of the held item (TakePending).
func ensureDEK(e execer, ix string, nowMS int64) (*fieldBox, error) {
	k, err := NewDEK()
	if err != nil {
		return nil, err
	}
	_, err = e.Exec(`INSERT OR IGNORE INTO ix_dek(ix,key,created_at) VALUES(?,?,?)`, ix, k.k[:], nowMS)
	k.Wipe()
	if err != nil {
		return nil, err
	}
	return boxOf(e, ix)
}

// put returns what column col stores for plaintext pt: NULL for nil, an
// empty value for an empty one — there is nothing to hide but that it is
// empty, which the length of a ciphertext would tell as well — and the
// ciphertext otherwise.
func (b *fieldBox) put(col Column, pt []byte) (any, error) {
	switch {
	case pt == nil:
		return nil, nil
	case len(pt) == 0:
		return []byte{}, nil
	}
	return b.sealField(col, pt)
}

// putText is put for a column that holds text and is never NULL.
func (b *fieldBox) putText(col Column, s string) (any, error) {
	if s == "" {
		return []byte{}, nil
	}
	return b.sealField(col, []byte(s))
}

// get returns the plaintext of what column col stores as v: nil for NULL,
// empty for an empty value.
func (b *fieldBox) get(col Column, v []byte) ([]byte, error) {
	switch {
	case v == nil:
		return nil, nil
	case len(v) == 0:
		return []byte{}, nil
	}
	return b.openField(col, v)
}

// getText is get for a text column.
func (b *fieldBox) getText(col Column, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	pt, err := b.openField(col, []byte(v))
	return string(pt), err
}

// openIX replaces the stored content columns of a row scanRows read with
// their plaintext.
func openIX(e execer, ix *Interaction) error {
	b, err := boxOf(e, ix.ID)
	if err != nil {
		return err
	}
	if ix.Goal, err = b.getText(ColGoal, ix.Goal); err != nil {
		return err
	}
	if ix.RequestDoc, err = b.get(ColRequestDoc, ix.RequestDoc); err != nil {
		return err
	}
	if ix.Result, err = b.get(ColResult, ix.Result); err != nil {
		return err
	}
	if ix.ResultMeta, err = b.getText(ColResultMeta, ix.ResultMeta); err != nil {
		return err
	}
	ix.Review, err = b.get(ColReview, ix.Review)
	return err
}

// boxCache holds the field ciphers of the tasks one read decrypts.
type boxCache struct {
	e     execer
	boxes map[string]*fieldBox
}

func newBoxCache(e execer) *boxCache { return &boxCache{e: e, boxes: map[string]*fieldBox{}} }

// of returns task ix's field cipher, or ErrContentGone.
func (c *boxCache) of(ix string) (*fieldBox, error) {
	if b := c.boxes[ix]; b != nil {
		return b, nil
	}
	b, err := boxOf(c.e, ix)
	if err != nil {
		return nil, err
	}
	c.boxes[ix] = b
	return b, nil
}

// DEK returns task ix's data key (an interaction's or a held delegation's),
// or ErrContentGone when it has none. The caller wipes it.
func (s *Store) DEK(ix string) (*DEK, error) { return loadDEK(s.db, ix) }

// DEK is Store.DEK inside the transaction.
func (t *Tx) DEK(ix string) (*DEK, error) { return loadDEK(t.tx, ix) }

// MACKey returns task ix's client-digest key (DEK.MACKey), or ErrContentGone
// when its data key is gone: a candidate task whose content has expired is
// then no match (CUX-D1).
func (s *Store) MACKey(ix string) ([]byte, error) { return macKey(s.db, ix) }

// MACKey is Store.MACKey inside the transaction.
func (t *Tx) MACKey(ix string) ([]byte, error) { return macKey(t.tx, ix) }

func macKey(e execer, ix string) ([]byte, error) {
	k, err := loadDEK(e, ix)
	if err != nil {
		return nil, err
	}
	defer k.Wipe()
	return k.MACKey(), nil
}

// DeleteDEK deletes task ix's data key in the transaction and reports
// whether it had one. From the commit on every field stored under it is
// unreadable; a row of ix still left reads as ErrContentGone. The
// retention sweep calls it with the rows it deletes (FS-L8).
func (t *Tx) DeleteDEK(ix string) (bool, error) {
	res, err := t.tx.Exec(`DELETE FROM ix_dek WHERE ix=?`, ix)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReleaseDEK deletes task ix's data key when nothing stored still needs it:
// no interaction, held delegation or outbox row of ix (FS-L1, engine
// ENG-S7). A path that deletes the last of those calls it in the same
// transaction. It reports whether the key was deleted.
func (t *Tx) ReleaseDEK(ix string) (bool, error) { return releaseDEK(t.tx, ix) }

func releaseDEK(e execer, ix string) (bool, error) {
	res, err := e.Exec(`DELETE FROM ix_dek WHERE ix=?
	   AND NOT EXISTS (SELECT 1 FROM interaction WHERE id=?)
	   AND NOT EXISTS (SELECT 1 FROM pending WHERE ix=?)
	   AND NOT EXISTS (SELECT 1 FROM outbox WHERE ix=?)`, ix, ix, ix, ix)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TakePending removes held delegation ix for its approval and keeps its data
// key, which the interaction created in the same transaction under the same
// id takes over (FS-L1). It reports whether the item existed. A held item
// rejected, expired or canceled goes with DeletePending, which deletes the
// key with it (FS-L9).
func (t *Tx) TakePending(ix string) (bool, error) {
	res, err := t.tx.Exec(`DELETE FROM pending WHERE ix=?`, ix)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

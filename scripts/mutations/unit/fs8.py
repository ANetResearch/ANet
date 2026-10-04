# fs8.py: forward secrecy §4.6.1, the local data key of a task (v0.3.0 FS §7.3 rows 16 and 31, FS-8) —
# mutation list for scripts/mutations/unitmut.py (see its header for the format).
#
# The numbered ids are rows of FS §7.3: 31, and the data-key half of 16 (FS-6 holds the
# interaction-key half in fs.py). fs8-a to fs8-l hold FS-8's acceptance tests to the rules they check
# (FS-L1 to FS-L4, FS-L9, core L35 and L36). The list sits beside fs.py, which FS-6 creates on its own
# branch, so the two merge without touching each other; it can be folded into fs.py once both are in.

IX = 'ANet:./internal/runtime/interactions'
DAEMON = 'ANet:./internal/daemon'
DEK = 'internal/runtime/interactions/dek.go'

MUTS = [
 dict(
  id='fs-16b', group='FS-L8', repo='ANet',
  desc='interactions.db 不开 secure_delete(删除的数据钥字节留在库文件的空闲区)',
  edits=[
   ('internal/runtime/interactions/interactions.go', '&_pragma=secure_delete(ON)', ''),
  ],
  tests=[(IX, '^TestDeletedKeyBytesAbsentFromDBFiles_DataKey$')],
  full=[IX],
 ),
 dict(
  id='fs-16c', group='FS-L8', repo='ANet',
  desc='删除数据钥后不做 wal_checkpoint(TRUNCATE)(只做 PASSIVE,WAL 中仍留有含钥的旧页)',
  edits=[
   ('internal/runtime/interactions/export_test.go', '`PRAGMA wal_checkpoint(TRUNCATE)`', '`PRAGMA wal_checkpoint(PASSIVE)`'),
  ],
  tests=[(IX, '^TestDeletedKeyBytesAbsentFromDBFiles_DataKey$')],
  full=[IX],
 ),
 dict(
  id='fs-31', group='L36', repo='ANet',
  desc='字段 AEAD 直接用 ix_dek(不经 HKDF)',
  edits=[
   (DEK, '\tkey := k.aeadKey()\n', '\tkey := append([]byte(nil), k.k[:]...)\n'),
  ],
  tests=[(IX, '^TestLOCAL1$')],
  full=[IX],
 ),
 dict(
  id='fs8-a', group='FS-L3', repo='ANet',
  desc='重新给加密列 message.metadata 建索引',
  edits=[
   ('internal/runtime/interactions/interactions.go',
    "\t\treturn fmt.Errorf(\"interactions: migrate msg dedupe index: %w\", err)\n\t}\n",
    "\t\treturn fmt.Errorf(\"interactions: migrate msg dedupe index: %w\", err)\n\t}\n"
    "\tif _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_msg_meta ON message(interaction_id, metadata)`); err != nil {\n"
    "\t\treturn err\n\t}\n"),
  ],
  tests=[(IX, '^TestEncryptedColumnsNotInSQLPredicates$')],
  full=[IX],
 ),
 dict(
  id='fs8-b', group='FS-L3', repo='ANet',
  desc='SQL 谓词里出现加密列(按 body 过滤消息)',
  edits=[
   ('internal/runtime/interactions/interactions.go',
    "`SELECT seq,interaction_id,sender_aid,kind,body,msg_id,metadata,created_at FROM message\n\t\t  WHERE interaction_id=? ORDER BY seq`",
    "`SELECT seq,interaction_id,sender_aid,kind,body,msg_id,metadata,created_at FROM message\n\t\t  WHERE interaction_id=? AND length(body) >= 0 ORDER BY seq`"),
  ],
  tests=[(IX, '^TestEncryptedColumnsNotInSQLPredicates$')],
  full=[IX],
 ),
 dict(
  id='fs8-c', group='FS-L2', repo='ANet',
  desc='消息正文以明文入库',
  edits=[
   ('internal/runtime/interactions/interactions.go',
    '\tbody, err := b.putText(ColMessageBody, m.Body)\n',
    '\tbody, err := any(m.Body), error(nil)\n'),
   ('internal/runtime/interactions/interactions.go',
    '\tif m.Body, err = b.getText(ColMessageBody, m.Body); err != nil {\n\t\treturn err\n\t}\n',
    ''),
  ],
  tests=[(IX, '^TestNoPlaintextContentInDB$')],
  full=[IX],
 ),
 dict(
  id='fs8-d', group='L35', repo='ANet',
  desc='字段的 aad 不含交互 id(一个任务的字段可搬到另一个任务)',
  edits=[
   (DEK, '{1: localFieldLabel, 2: ix, 3: string(col)}', '{1: localFieldLabel, 2: "", 3: string(col)}'),
  ],
  tests=[(IX, '^(TestLOCAL1|TestFieldsBoundToTheirRow)$')],
  full=[IX],
 ),
 dict(
  id='fs8-e', group='FS-L9', repo='ANet',
  desc='待批项被删(拒绝、过期、取消)时不删其数据钥',
  edits=[
   ('internal/runtime/interactions/pending.go',
    '\tif _, err := releaseDEK(t.tx, ix); err != nil {\n\t\treturn false, err\n\t}\n', ''),
  ],
  tests=[(IX, '^(TestDEKLivesAndDiesWithItsRow|TestDeletedKeyBytesAbsentFromDBFiles_DataKey)$')],
  full=[IX],
 ),
 dict(
  id='fs8-f', group='FS-L1', repo='ANet',
  desc='批准待批项时换新数据钥(TakePending 也删钥)',
  edits=[
   (DEK, "func (t *Tx) TakePending(ix string) (bool, error) {\n\tres, err := t.tx.Exec(`DELETE FROM pending WHERE ix=?`, ix)\n",
    "func (t *Tx) TakePending(ix string) (bool, error) {\n\t_, _ = t.tx.Exec(`DELETE FROM ix_dek WHERE ix=?`, ix)\n\tres, err := t.tx.Exec(`DELETE FROM pending WHERE ix=?`, ix)\n"),
  ],
  tests=[(IX, '^TestDEKLivesAndDiesWithItsRow$')],
  full=[IX],
 ),
 dict(
  id='fs8-g', group='FS-L4', repo='ANet',
  desc='缺数据钥的交互行仍按"存在"返回(内容为空)',
  edits=[
   (DEK, '\tb, err := boxOf(e, ix.ID)\n\tif err != nil {\n\t\treturn err\n\t}\n\tif ix.Goal, err',
    '\tb, err := boxOf(e, ix.ID)\n\tif errors.Is(err, ErrContentGone) {\n\t\tix.Goal, ix.RequestDoc, ix.Result, ix.ResultMeta, ix.Review = "", nil, nil, "", nil\n\t\treturn nil\n\t}\n\tif err != nil {\n\t\treturn err\n\t}\n\tif ix.Goal, err'),
  ],
  tests=[(IX, '^TestContentGoneReadsAsNotFound$'), (DAEMON, '^TestContentGoneMapsToNoLocalTask$')],
  full=[IX],
 ),
 dict(
  id='fs8-h', group='FS-L4', repo='ANet',
  desc='ErrContentGone 不再是 ErrNotFound(上层不按"已删除"处理)',
  edits=[
   (DEK, 'var ErrContentGone = fmt.Errorf("%w: its content was deleted", ErrNotFound)',
    'var ErrContentGone = errors.New("interactions: its content was deleted")'),
  ],
  tests=[(DAEMON, '^TestContentGoneMapsToNoLocalTask$')],
  full=[IX],
 ),
 dict(
  id='fs8-i', group='L35', repo='ANet',
  desc='字段 nonce 固定为全零(同一钥下重复使用 nonce)',
  edits=[
   (DEK, '\tif _, err := rand.Read(nonce[:]); err != nil {\n\t\treturn nil, fmt.Errorf("interactions: field nonce: %w", err)\n\t}\n', ''),
  ],
  tests=[(IX, '^TestFieldCodec$')],
  full=[IX],
 ),
 dict(
  id='fs8-j', group='FS-L9', repo='ANet',
  desc='ReleaseDEK 不看出站行(待批项删除时删掉其出站行仍要用的数据钥)',
  edits=[
   (DEK, '\t   AND NOT EXISTS (SELECT 1 FROM outbox WHERE ix=?)`, ix, ix, ix, ix)',
    '\t   AND NOT EXISTS (SELECT 1 FROM outbox WHERE ix=? AND 0)`, ix, ix, ix, ix)'),
  ],
  tests=[(IX, '^TestReleaseDEKKeepsAKeyStillNeeded$')],
  full=[IX],
 ),
 dict(
  id='fs8-k', group='FS-L1', repo='ANet',
  desc='批准待批项时用 DeletePending(删掉待批项的数据钥,交互另建新钥)',
  edits=[
   ('internal/daemon/pending.go', '\t\tok, err := tx.TakePending(ixID)\n', '\t\tok, err := tx.DeletePending(ixID)\n'),
  ],
  tests=[(DAEMON, '^TestHeldDelegationDataKeyFollowsTheApprovalQueue$')],
  full=[],
 ),
 dict(
  id='fs8-l', group='FS-L4', repo='ANet',
  desc='列表在读行的快照之外取数据钥(并发清理使已选中的行从本页消失)',
  edits=[
   ('internal/runtime/interactions/interactions.go', '\t\tswitch err := openIX(tx, ix); {\n', '\t\tswitch err := openIX(s.db, ix); {\n'),
  ],
  tests=[(IX, '^TestListReadsRowsAndKeysFromOneSnapshot$')],
  full=[IX],
 ),
]

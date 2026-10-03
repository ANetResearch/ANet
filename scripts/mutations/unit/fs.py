# fs.py: forward secrecy and local retention (v0.3.0 FS §7.3) — mutation list for scripts/mutations/unitmut.py
# (see its header for the format). The numbered ids are the rows of FS §7.3; the FS packages append theirs.
# FS-6 adds 13 and the secure_delete half of 16, and fs6-a to fs6-f, which hold its acceptance tests to the
# storage rules they check (FS-X7 to FS-X10, FS-R4). Rows 11, 14 and 15 are judged at the daemon by FS-7's
# tests; fs6-c, fs6-d and fs6-e are their storage halves.

IX = 'ANet:./internal/runtime/interactions'

MUT13_SOLO = '''package interactions

import "database/sql"

// soloDB and soloSettle are mutation 13: the key is settled in a transaction of its own, committed before
// the one that ends the task.
var soloDB *sql.DB

func soloSettle(id string, cause Cause) error {
	tx, err := soloDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := closeIXKey(tx, id, cause, 0); err != nil {
		return err
	}
	return tx.Commit()
}
'''

MUTS = [
 dict(
  id='fs-13', group='FS-X7', repo='ANet',
  desc='closeIXKey 移到提交之前的独立事务(终态写入、迟到结果、CloseIXKey 的事务回滚后钥已删)',
  edits=[
   ('internal/runtime/interactions/zz_mut13.go', None, MUT13_SOLO),
   ('internal/runtime/interactions/interactions.go', '\ts := &Store{db: db}\n', '\ts := &Store{db: db}\n\tsoloDB = db\n'),
   ('internal/runtime/interactions/peers.go',
    'func (t *Tx) Finish(id string, f Finish, cause Cause) error { return finish(t.tx, id, f, cause, t.now) }',
    'func (t *Tx) Finish(id string, f Finish, cause Cause) error {\n\tif err := soloSettle(id, cause); err != nil {\n\t\treturn err\n\t}\n\treturn finish(t.tx, id, f, cause, t.now)\n}'),
   ('internal/runtime/interactions/peers.go',
    '\treturn setLateResult(t.tx, id, result, resultCID, receipt, verified, cause, t.now)',
    '\tif err := soloSettle(id, cause); err != nil {\n\t\treturn false, err\n\t}\n\treturn setLateResult(t.tx, id, result, resultCID, receipt, verified, cause, t.now)'),
   ('internal/runtime/interactions/peers.go',
    '\treturn setState(t.tx, id, st, cause, t.now)\n',
    '\tif err := soloSettle(id, cause); err != nil {\n\t\treturn false, err\n\t}\n\treturn setState(t.tx, id, st, cause, t.now)\n'),
   ('internal/runtime/interactions/ixkey.go',
    '\treturn closeIXKey(t.tx, ix, cause, t.now)\n}',
    '\treturn soloSettle(ix, cause)\n}'),
  ],
  tests=[(IX, 'TestIXKeyDeletedOnlyWithCommit')],
  full=[IX],
 ),
 dict(
  id='fs-16a', group='FS-X8', repo='ANet',
  desc='interactions.db 不开 secure_delete(删除的私钥字节留在库文件的空闲页)',
  edits=[
   ('internal/runtime/interactions/interactions.go', '&_pragma=secure_delete(ON)', ''),
  ],
  tests=[(IX, 'TestDeletedKeyBytesAbsentFromDBFiles')],
  full=[IX],
 ),
 dict(
  id='fs6-a', group='FS-X9', repo='ANet',
  desc='close_after 清扫不看时钟门(门级 closed 时照删)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\tif level != ClockFull && level != ClockLocal {\n\t\treturn 0, nil\n\t}\n', ''),
  ],
  tests=[(IX, 'TestCloseAfterSweepGated')],
  full=[IX],
 ),
 dict(
  id='fs6-b', group='FS-X10', repo='ANet',
  desc='DeleteAllIXKeys 漏掉 close_after 中的钥',
  edits=[
   ('internal/runtime/interactions/ixkey.go', 'res, err := t.tx.Exec(`DELETE FROM ix_key`)',
    'res, err := t.tx.Exec(`DELETE FROM ix_key WHERE close_after = 0`)'),
  ],
  tests=[(IX, 'TestCloseInteractionsDeletesAllIXKeys')],
  full=[IX],
 ),
 dict(
  id='fs6-c', group='FS-X7', repo='ANet',
  desc='写终态时接受 CauseNotTerminal(终态路径不处置交互钥;14 的存储一半)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\tif terminal != (cause != CauseNotTerminal) {\n',
    '\tif !terminal && cause != CauseNotTerminal {\n'),
  ],
  tests=[(IX, 'TestTerminalWritesRequireCause|TestIXKeyLifecycleRandom|FuzzIXKeyLifecycle')],
  full=[IX],
 ),
 dict(
  id='fs6-d', group='FS-X7', repo='ANet',
  desc='请求方本地终态即删 E_r(15 的存储一半)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\t\tif !provider {\n\t\t\treturn fateCloseAfter, nil\n',
    '\t\tif !provider {\n\t\t\treturn fateDelete, nil\n'),
  ],
  tests=[(IX, 'TestIXKeyFateTable|TestIXKeyFollowsEachTerminalWrite')],
  full=[IX],
 ),
 dict(
  id='fs6-e', group='FS-R4', repo='ANet',
  desc='peer_rk 被不同的 rk 覆盖(11 的存储一半)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\tif len(cur) > 0 {\n\t\tif RKConflict(cur, rk) {\n\t\t\treturn ErrRKMismatch\n\t\t}\n\t\treturn nil\n\t}\n', ''),
   ('internal/runtime/interactions/ixkey.go', '`UPDATE interaction SET peer_rk=? WHERE id=? AND peer_rk IS NULL`',
    '`UPDATE interaction SET peer_rk=? WHERE id=?`'),
  ],
  tests=[(IX, 'TestRecordPeerRK')],
  full=[IX],
 ),
 dict(
  id='fs6-f', group='FS-X7', repo='ANet',
  desc='只有改变了状态的终态写入才处置交互钥(本地取消后对端终态消息不删 E_r)',
  edits=[
   ('internal/runtime/interactions/interactions.go',
    '\tn, _ := res.RowsAffected()\n\tif st.IsTerminal() {\n',
    '\tn, _ := res.RowsAffected()\n\tif st.IsTerminal() && n > 0 {\n'),
  ],
  tests=[(IX, 'TestIXKeyFollowsEachTerminalWrite')],
  full=[IX],
 ),
]

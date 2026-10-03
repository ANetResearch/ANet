# ux.py: the reason table and the user-visible surfaces (v0.3.0 ux §6.2) — mutation list for
# scripts/mutations/unitmut.py (see its header for the format). UX-5 adds ux mutations 2, 3, 4, 9, 12, 14
# and 17, and the removed-item half of 6 and 7 (a removed key or command that stops being refused), plus the
# CORE-S3 check of the reason table against the wire registry. UX-19 registers the rest of ux §6.2.

A2A = 'ANet:./internal/a2ashape'
CLI = 'ANet:./cmd/anet'

MUTS = [
 dict(
  id='ux-2', group='UX-T2', repo='ANet',
  desc='denied 只写中文不写英文',
  edits=[('internal/a2ashape/reasons.go', 'En:       "The other side has blocked tasks from you and did not run it; retrying will not help.",', 'En: "",')],
  tests=[(A2A, 'TestReasonRowsAreWellFormed')],
  full=[A2A],
 ),
 dict(
  id='ux-3', group='UX-T3', repo='ANet',
  desc='denied 的句子去掉“重试无用”',
  edits=[('internal/a2ashape/reasons.go', 'Zh:       "对方已屏蔽你的任务，没有执行；重试无用。",', 'Zh:       "对方已屏蔽你的任务，没有执行。",')],
  tests=[(A2A, 'TestReasonNextStep')],
  full=[A2A],
 ),
 dict(
  id='ux-4', group='UX-T3', repo='ANet',
  desc='句子里写入 KSN',
  edits=[('internal/a2ashape/reasons.go', 'En:       "The other side has blocked tasks from you and did not run it; retrying will not help.",', 'En:       "The other side has blocked tasks from you and did not run it; the KSN says retrying will not help.",')],
  tests=[(A2A, 'TestReasonSentencesUseNoInternalWord')],
  full=[A2A],
 ),
 dict(
  id='ux-9', group='UX-T9a', repo='ANet',
  desc='doctor 新增一个未登记的 id',
  edits=[('cmd/anet/doctor.go', '\tadd("modules", stInfo, compiledModulesReport(), "")\n',
          '\tadd("modules", stInfo, compiledModulesReport(), "")\n\tadd("modules.extra", stInfo, "x", "")\n')],
  tests=[(CLI, 'TestDoctorOutputForm')],
  full=[CLI],
 ),
 dict(
  id='ux-12', group='UX-T4', repo='ANet',
  desc='delivery_unconfirmed 在有 delivery_wait 时选了 .queued',
  edits=[('internal/a2ashape/reasons.go',
          '\tcase cause == DeliveryCauseScan && wait != "":\n\t\treturn "waiting"\n\tcase queued:\n\t\treturn "queued"\n',
          '\tcase queued:\n\t\treturn "queued"\n\tcase cause == DeliveryCauseScan && wait != "":\n\t\treturn "waiting"\n')],
  tests=[(A2A, 'TestDeliveryVariant')],
  full=[A2A],
 ),
 dict(
  id='ux-14', group='UX-T4', repo='ANet',
  desc='DeliveryText 在任务有放弃时刻时省略 delivery.ends_at',
  edits=[('internal/a2ashape/reasons.go', '\tif !endsAt.IsZero() {\n', '\tif false && !endsAt.IsZero() {\n')],
  tests=[(A2A, 'TestDeliveryEndsAt|TestDeliveryTextIsTheTable|TestUXExamples')],
  full=[A2A],
 ),
 dict(
  id='ux-17', group='UX-T4', repo='ANet',
  desc='{aid8} 改回取 AID 的前 8 个字符',
  edits=[('internal/a2ashape/reasons.go', '\t\tr = r[len(r)-8:]\n', '\t\tr = r[:8]\n')],
  tests=[(A2A, 'TestReasonPlaceholders')],
  full=[A2A],
 ),
 dict(
  id='ux-6r', group='UX-T6', repo='ANet',
  desc='删除项 rotation_grace 不再拒绝启动(从删除键表里去掉)',
  edits=[('internal/daemon/config.go', '\t{"rotation_grace", "rotation_grace"},\n', '')],
  tests=[(CLI, 'TestRemovedKeysStopAnetDaemon')],
  full=[CLI, 'ANet:./internal/daemon'],
 ),
 dict(
  id='ux-7r', group='UX-T7', repo='ANet',
  desc='恢复 anet evidence(从删除命令表里去掉)',
  edits=[('cmd/anet/removed.go', '\t{cmd: "evidence", what: "anet evidence", instead: "evidence"},\n', '')],
  tests=[(CLI, 'TestRemovedCommandsAreRefusedBeforeTheDaemon|TestRemovedCommandsHaveTheirPhrases')],
  full=[CLI],
 ),
 dict(
  id='ux-reg', group='CORE-S3', repo='ANet',
  desc='原因表少一行(review_exists),与登记表 reasons.user 不再相等',
  edits=[('internal/a2ashape/reasons.go', '\t{Code: "review_exists", Class: ClassC,\n\t\tZh:       "这次交互已经评价过了；每次交互只能评价一次，重试无用。",\n\t\tEn:       "This interaction has already been reviewed; each can be reviewed once, so retrying will not help.",\n\t\tNextKind: NextFutile},\n', '')],
  tests=[('ANet:./internal/daemon', 'TestWireRegistry'), (A2A, 'TestReasonTableMatchesTheRegistry|TestReasonTableHasExactlyTheUXKeys')],
  full=[A2A],
 ),
]

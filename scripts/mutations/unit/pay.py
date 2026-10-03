# pay.py: mutations of the hub and payment privacy work that must turn their tests red (M-PAY-*).
# For scripts/mutations/unitmut.py (see its header for the format); entries name the repository
# they edit. Later PAY work items append their rows here.
#
# M-PAY-1 and M-PAY-2 are the ANetCore liability tree (package liabtree). Their key tests are the
# LIAB-1 refusals the golden vector carries for them: a path whose sums were moved between two
# levels (the root sum unchanged), two sibling accounts shown different splits of one parent sum
# (which also catches a node hash over the parent's total alone), and a tree whose sums wrap
# around 2^64.

LIAB_MOVED = [('ANetCore:./liabtree', '^TestLIAB1$/^refuse_moved_(sum|split)$')]
LIAB_WRAP = [('ANetCore:./liabtree', '^TestLIAB1$/^refuse_sum_wraps$')]

MUTS = [
 dict(id='M-PAY-1', group='PAY', repo='ANetCore', desc='负债树节点哈希不覆盖子和(L26 去掉 u64be(L.sum)、u64be(R.sum))',
  edits=[('liabtree/liabtree.go', '\tbinary.BigEndian.PutUint64(b[n:], l.Sum)\n', ''),
         ('liabtree/liabtree.go', '\tbinary.BigEndian.PutUint64(b[n:], r.Sum)\n', '')],
  tests=LIAB_MOVED, full=['ANetCore:./liabtree']),
 dict(id='M-PAY-2', group='PAY', repo='ANetCore', desc='负债树去掉和的溢出检查(addSum 直接相加)',
  edits=[('liabtree/liabtree.go', '\tif a > MaxSum || b > MaxSum || a+b > MaxSum {\n\t\treturn 0, false\n\t}\n', '')],
  tests=LIAB_WRAP, full=['ANetCore:./liabtree']),
]

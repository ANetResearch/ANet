# core.py: CORE-S8 — mutations that must turn TestWireRegistry red in each repository (CORE-1).
# For scripts/mutations/unitmut.py (see its header for the format). Each of the six kinds of change is
# made once in each of the three repositories: change a key number in a keyasint struct, add an
# unregistered key, change a label by one character, reuse a retired key, add an unregistered
# relayauth action, and sign with an identity key outside the registered signers (CORE-L9).
#
# ANetHub has no keyasint struct of its own, so its first two mutations add one bound to a
# registered object with a key the object does not assign.

CORE_T = [('ANetCore:./wire', '^TestWireRegistry$')]
ANET_T = [('ANet:./internal/daemon', '^TestWireRegistry$')]
HUB_T = [('ANetHub:./internal/aghub', '^TestWireRegistry$')]

RETIRED = '''package %s

// zzMutRetired reuses ResultResp key 4, retired in 0.3.0 (the KEL now travels only in inner key 10).
//
//anet:wire ResultResp
type zzMutRetired struct {
	Status string `cbor:"1,keyasint"`
	KEL    []byte `cbor:"4,keyasint,omitempty"`
}
'''

SIGNER = '''package %s

import "github.com/ANetResearch/ANetCore/identity"

// zzMutSign signs with an identity key from a function the registry does not list.
func zzMutSign(c *identity.Controller) []byte {
	sig, _ := c.Sign([]byte("unregistered"))
	return sig
}
'''

BOUND = '''package aghub

// zzMutBound is bound to a registered object but uses key %d.
//
//anet:wire Attachment
type zzMutBound struct {
	Name string `cbor:"1,keyasint"`
	Size int64  `cbor:"3,keyasint"`
	CID  string `cbor:"%d,keyasint"`
}
'''

MUTS = [
 # ---- ANetCore
 dict(id='core-s8-1', group='CORE-S8', repo='ANetCore', desc='aobj.Envelope 的 alg 改用键 6',
  edits=[('aobj/aobj.go', '\tAlg         int    `cbor:"3,keyasint"`', '\tAlg         int    `cbor:"6,keyasint"`')],
  tests=CORE_T, full=['ANetCore:./wire']),
 dict(id='core-s8-2', group='CORE-S8', repo='ANetCore', desc='effect.Evidence 新增未登记的键 9',
  edits=[('effect/effect.go', '\tQuirk string `cbor:"8,keyasint,omitempty"`', '\tQuirk string `cbor:"8,keyasint,omitempty"`\n\tExtra string `cbor:"9,keyasint,omitempty"`')],
  tests=CORE_T, full=['ANetCore:./wire']),
 dict(id='core-s8-3', group='CORE-S8', repo='ANetCore', desc='kid 标签改一个字符(anet-enc-kid/v2)',
  edits=[('seal/suite.go', 'const kidLabel = "anet-enc-kid/v1"', 'const kidLabel = "anet-enc-kid/v2"')],
  tests=CORE_T, full=['ANetCore:./wire']),
 dict(id='core-s8-4', group='CORE-S8', repo='ANetCore', desc='新结构体复用 ResultResp 的退役键 4',
  edits=[('seal/zz_mut_retired.go', None, RETIRED % 'seal')],
  tests=CORE_T, full=['ANetCore:./wire']),
 dict(id='core-s8-5', group='CORE-S8', repo='ANetCore', desc='relayauth 新增未登记的 action zz',
  edits=[('relayauth/relayauth.go', '\tActionKeys = "keys"', '\tActionKeys = "keys"\n\tActionZZ   = "zz"')],
  tests=CORE_T, full=['ANetCore:./wire']),
 dict(id='core-s8-6', group='CORE-S8', repo='ANetCore', desc='signers 段之外新增身份钥签名调用点',
  edits=[('payment/zz_mut_signer.go', None, SIGNER % 'payment')],
  tests=CORE_T, full=['ANetCore:./wire']),
 # ---- ANet
 dict(id='anet-s8-1', group='CORE-S8', repo='ANet', desc='encKeyFile 的 signed 改用键 6',
  edits=[('internal/daemon/enckeys.go', '\tSigned []byte `cbor:"5,keyasint,omitempty"`', '\tSigned []byte `cbor:"6,keyasint,omitempty"`')],
  tests=ANET_T, full=['ANet:./internal/daemon']),
 dict(id='anet-s8-2', group='CORE-S8', repo='ANet', desc='encKeyFile 新增未登记的键 10',
  edits=[('internal/daemon/enckeys.go', '\tSigned []byte `cbor:"5,keyasint,omitempty"`', '\tSigned []byte `cbor:"5,keyasint,omitempty"`\n\tExtra  []byte `cbor:"10,keyasint,omitempty"`')],
  tests=ANET_T, full=['ANet:./internal/daemon']),
 dict(id='anet-s8-3', group='CORE-S8', repo='ANet', desc='PayBind 标签改一个字符(anet/x402-bind/v2)',
  edits=[('internal/x402a2a/x402a2a.go', '\th.Write([]byte("anet/x402-bind/v1"))', '\th.Write([]byte("anet/x402-bind/v2"))')],
  tests=ANET_T, full=['ANet:./internal/daemon']),
 dict(id='anet-s8-4', group='CORE-S8', repo='ANet', desc='新结构体复用 ResultResp 的退役键 4',
  edits=[('module/org/zz_mut_retired.go', None, RETIRED % 'org')],
  tests=ANET_T, full=['ANet:./internal/daemon']),
 dict(id='anet-s8-5', group='CORE-S8', repo='ANet', desc='余额查询改签未登记的 action balance2',
  edits=[('module/x402/pay.go', 'relayauth.ActionBalance, &bal); err != nil {\n\t\treturn', '"balance2", &bal); err != nil {\n\t\treturn')],
  tests=ANET_T, full=['ANet:./internal/daemon']),
 dict(id='anet-s8-6', group='CORE-S8', repo='ANet', desc='signers 段之外新增身份钥签名调用点',
  edits=[('module/org/zz_mut_signer.go', None, SIGNER % 'org')],
  tests=ANET_T, full=['ANet:./internal/daemon']),
 # ---- ANetHub
 dict(id='hub-s8-1', group='CORE-S8', repo='ANetHub', desc='绑定 Attachment 的结构体把 cid 放到键 5(data 的键)',
  edits=[('internal/aghub/zz_mut_bound.go', None, BOUND % (5, 5))],
  tests=HUB_T, full=['ANetHub:./internal/aghub']),
 dict(id='hub-s8-2', group='CORE-S8', repo='ANetHub', desc='绑定 Attachment 的结构体使用未登记的键 7',
  edits=[('internal/aghub/zz_mut_bound.go', None, BOUND % (7, 7))],
  tests=HUB_T, full=['ANetHub:./internal/aghub']),
 dict(id='hub-s8-3', group='CORE-S8', repo='ANetHub', desc='发放事件类型改一个字符',
  edits=[('internal/aghub/issuance.go', '\tEvCreditOpening = "anet.credit.opening"', '\tEvCreditOpening = "anet.credit.openinh"')],
  tests=HUB_T, full=['ANetHub:./internal/aghub']),
 dict(id='hub-s8-4', group='CORE-S8', repo='ANetHub', desc='新结构体复用 ResultResp 的退役键 4',
  edits=[('internal/aghub/zz_mut_retired.go', None, RETIRED % 'aghub')],
  tests=HUB_T, full=['ANetHub:./internal/aghub']),
 dict(id='hub-s8-5', group='CORE-S8', repo='ANetHub', desc='/profile 改验未登记的 action profil',
  edits=[('internal/aghub/server.go', 's.authRegistered(w, r, relayauth.ActionProfile, signedBodyLimit)', 's.authRegistered(w, r, "profil", signedBodyLimit)')],
  tests=HUB_T, full=['ANetHub:./internal/aghub']),
 dict(id='hub-s8-6', group='CORE-S8', repo='ANetHub', desc='signers 段之外新增身份钥签名调用点',
  edits=[('internal/hubid/zz_mut_signer.go', None, SIGNER % 'hubid')],
  tests=HUB_T, full=['ANetHub:./internal/aghub']),
]

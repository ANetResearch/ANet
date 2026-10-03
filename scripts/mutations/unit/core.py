# core.py: CORE-S8 — mutations that must turn TestWireRegistry red in each repository (CORE-1).
# For scripts/mutations/unitmut.py (see its header for the format). Each of the six kinds of change is
# made once in each of the three repositories: change a key number in a keyasint struct, add an
# unregistered key, change a label by one character, reuse a retired key, add an unregistered
# relayauth action, and sign with an identity key outside the registered signers (CORE-L9).
# ANetCore and ANet also hand the signing method on as a method value from an unlisted function
# (the daemon passes d.self.Sign to seal.Seal and the key ring this way): that is signing too.
#
# ANetHub has no keyasint struct of its own, so its first two mutations add one bound to a
# registered object with a key the object does not assign.
#
# CORE-2 (seal v2): each mutation breaks one rule of the outer v2, the Wrap, the inner v2, the
# preimage v2, env_digest or the ack_key, and must turn its unit test and, where the vector
# covers the rule, TestSEALED_2A or TestSEALED_2C red.

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

SIGNER_VALUE = '''package %s

import "github.com/ANetResearch/ANetCore/identity"

func zzMutApply(sign func([]byte) ([]byte, uint64)) []byte {
	sig, _ := sign([]byte("unregistered"))
	return sig
}

// zzMutSignValue hands the identity signing method on from a function the registry does not list.
func zzMutSignValue(c *identity.Controller) []byte { return zzMutApply(c.Sign) }
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
 dict(id='core-s8-7', group='CORE-S8', repo='ANetCore', desc='signers 段之外把签名方法作为方法值交出',
  edits=[('payment/zz_mut_signer_value.go', None, SIGNER_VALUE % 'payment')],
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
 dict(id='anet-s8-7', group='CORE-S8', repo='ANet', desc='signers 段之外把签名方法作为方法值交出',
  edits=[('module/org/zz_mut_signer_value.go', None, SIGNER_VALUE % 'org')],
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
 # ---- CORE-2 (ANetCore seal v2)
 dict(id='core2-1', group='CORE-2', repo='ANetCore', desc='outerKnown 收下外层键 9',
  edits=[('seal/envelope.go', '\touterKeyEnc: true, outerKeyCT: true, outerKeyAccess: true, outerKeyExp: true,', '\touterKeyEnc: true, outerKeyCT: true, outerKeyAccess: true, outerKeyExp: true, 9: true,')],
  tests=[('ANetCore:./seal', '^(TestParseOuter|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-2', group='CORE-2', repo='ANetCore', desc='access kind 2 当作 kind 0 接受',
  edits=[('seal/envelope.go', '\tcase AccessKindSession:\n', '\tcase AccessKindSession, 2:\n')],
  tests=[('ANetCore:./seal', '^(TestParseOuterV2Access|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-3', group='CORE-2', repo='ANetCore', desc='ParseOuter 不理会键 7 的结构错误',
  edits=[('seal/envelope.go', 'if e.Access, err = decodeAccess(raw); err != nil {', 'if e.Access, err = decodeAccess(raw); false && err != nil {')],
  tests=[('ANetCore:./seal', '^(TestParseOuterV2Access|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-4', group='CORE-2', repo='ANetCore', desc='Open 不比较外层键 8 与内层 exp',
  edits=[('seal/envelope.go', '\tif outer.Exp != nil && *outer.Exp != inner.Exp {', '\tif false && outer.Exp != nil && *outer.Exp != inner.Exp {')],
  tests=[('ANetCore:./seal', '^(TestOuterExp|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-5', group='CORE-2', repo='ANetCore', desc='Wrap 的 pad 含非零字节仍接受(FS mutation 38)',
  edits=[('seal/envelope.go', '\t\tif b != 0 {\n\t\t\treturn nil, fail(ReasonBadWrap, "pad has a non-zero byte")', '\t\tif false && b != 0 {\n\t\t\treturn nil, fail(ReasonBadWrap, "pad has a non-zero byte")')],
  tests=[('ANetCore:./seal', '^(TestWrapDecodeRules|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-6', group='CORE-2', repo='ANetCore', desc='Wrap 收下 0–63 未登记的键',
  edits=[('seal/envelope.go', 'if k, ok := firstUnknownCritical(m, wrapKnown); ok {', 'if k, ok := firstUnknownCritical(m, wrapKnown); ok && false {')],
  tests=[('ANetCore:./seal', '^(TestWrapDecodeRules|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-7', group='CORE-2', repo='ANetCore', desc='内层收下退役键 21',
  edits=[('seal/envelope.go', '\tkeyNotice: true, keyKeys: true, keyGrant: true, keyRK: true, keySig: true,', '\tkeyNotice: true, keyKeys: true, keyGrant: true, keyRK: true, keySig: true, 21: true,')],
  tests=[('ANetCore:./seal', '^(TestInnerKeySpace|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-8', group='CORE-2', repo='ANetCore', desc='内层不要求键 11 与键 10 同进同出(C-52)',
  edits=[('seal/envelope.go', '\tcase (in.KEL != nil) != (in.Notices != nil):', '\tcase false && (in.KEL != nil) != (in.Notices != nil):')],
  tests=[('ANetCore:./seal', '^(TestInnerStructure|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-9', group='CORE-2', repo='ANetCore', desc='内层 ix 含 U+0000 仍接受(CORE-E9)',
  edits=[('seal/envelope.go', '\tcase strings.ContainsRune(in.IX, 0):', '\tcase false && strings.ContainsRune(in.IX, 0):')],
  tests=[('ANetCore:./seal', '^(TestInnerStructure|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-10', group='CORE-2', repo='ANetCore', desc='原像键 9 放正文而非其 SHA-256',
  edits=[('seal/fields.go', '\tif m[keyBody], err = encodeField(digest[:]); err != nil {', '\t_ = digest\n\tif m[keyBody], err = encodeField(body); err != nil {')],
  tests=[('ANetCore:./seal', '^(TestSigPreimageFollowsDesign|TestSEALED_2A)$')], full=['ANetCore:./seal']),
 dict(id='core2-11', group='CORE-2', repo='ANetCore', desc='单层原像去掉键 33、34(FS mutation 10 的单层一侧)',
  edits=[('seal/fields.go', '\tm[preKeyIKID], m[preKeyEnc2] = cborNull, cborNull\n', '')],
  tests=[('ANetCore:./seal', '^(TestSigPreimageFollowsDesign|TestSEALED_2A|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-12', group='CORE-2', repo='ANetCore', desc='第一层解封失败也带 ack_key(DLV-C-API-2)',
  edits=[('seal/envelope.go', '\t\treturn nil, failWrap(ReasonDecrypt, err, "hpke open")', '\t\treturn nil, withAck(AckKey{}, failWrap(ReasonDecrypt, err, "hpke open"))')],
  tests=[('ANetCore:./seal', '^(TestOpenRecipientAndKey|TestOuterFieldsAreSigned|TestSEALED_2A)$')], full=['ANetCore:./seal']),
 dict(id='core2-13', group='CORE-2', repo='ANetCore', desc='Wrap 解码失败不带 ack_key(DLV-C-API-1)',
  edits=[('seal/envelope.go', '\tw, err := decodeWrap(pt)\n\tif err != nil {\n\t\treturn nil, withAck(ack, err)', '\tw, err := decodeWrap(pt)\n\tif err != nil {\n\t\treturn nil, err')],
  tests=[('ANetCore:./seal', '^(TestWrapDecodeRules|TestAckKeyOf|TestSEALED_2C)$')], full=['ANetCore:./seal']),
 dict(id='core2-14', group='CORE-2', repo='ANetCore', desc='env_digest 把键 7 也算进去',
  edits=[('seal/envelope.go', '\tdelete(fields, outerKeyAccess)\n', '')],
  tests=[('ANetCore:./seal', '^(TestEnvDigestIgnoresOnlyKey7|TestSEALED_2A)$')], full=['ANetCore:./seal']),
 dict(id='core2-15', group='CORE-2', repo='ANetCore', desc='Wrap 不做 Padmé 填充',
  edits=[('seal/fields.go', '\ttarget := Padme(base)', '\ttarget := base')],
  tests=[('ANetCore:./seal', '^(TestWrapIsPadded|TestSEALED_2A)$')], full=['ANetCore:./seal']),
 dict(id='core2-16', group='CORE-2', repo='ANetCore', desc='第一层 HPKE info 仍用 v1 标签',
  edits=[('seal/envelope.go', 'const HPKELabel = "anet-seal/v2"', 'const HPKELabel = "anet-relay/v1"')],
  tests=[('ANetCore:./seal', '^TestSEALED_2A$')], full=['ANetCore:./seal']),
 dict(id='core2-17', group='CORE-2', repo='ANetCore', desc='ack_key 的导出上下文改一个字符',
  edits=[('seal/envelope.go', 'const DAckExporterContext = "anet-dack/v1"', 'const DAckExporterContext = "anet-dack/v2"')],
  tests=[('ANetCore:./seal', '^TestSEALED_2A$')], full=['ANetCore:./seal']),
 dict(id='core2-18', group='CORE-2', repo='ANetCore', desc='Seal 不写 SealOpts.Exp(一次性信封没有外层键 8)',
  edits=[('seal/envelope.go', '\tif o.Exp != 0 {\n\t\tif outer[outerKeyExp]', '\tif false && o.Exp != 0 {\n\t\tif outer[outerKeyExp]')],
  tests=[('ANetCore:./seal', '^TestOuterExp$')], full=['ANetCore:./seal']),
 dict(id='core2-19', group='CORE-2', repo='ANetCore', desc='rk 不查套件(FS-C11)',
  edits=[('seal/envelope.go', '\tif r.Suite != SuiteX25519 {\n\t\treturn fmt.Errorf("rk suite', '\tif false && r.Suite != SuiteX25519 {\n\t\treturn fmt.Errorf("rk suite')],
  tests=[('ANetCore:./seal', '^(TestInnerStructure|TestSealInputChecks)$')], full=['ANetCore:./seal']),
 dict(id='core2-20', group='CORE-2', repo='ANetCore', desc='交互钥查询出错当作交互已关闭(ENG-V8)',
  edits=[('seal/envelope.go', '\t\t\tif key, err = ix.IXKey(w.ikid); err != nil {', '\t\t\tif key, err = ix.IXKey(w.ikid); false && err != nil {')],
  tests=[('ANetCore:./seal', '^TestWrapDecodeRules$')], full=['ANetCore:./seal']),
 dict(id='core2-21', group='CORE-2', repo='ANetCore', desc='SetAccess 不复核结果能否通过 ParseOuter',
  edits=[('seal/envelope.go', '\tif _, err := ParseOuter(out); err != nil {\n\t\treturn nil, err', '\tif _, err := ParseOuter(out); false && err != nil {\n\t\treturn nil, err')],
  tests=[('ANetCore:./seal', '^TestSetAccess$')], full=['ANetCore:./seal']),
 dict(id='core2-22', group='CORE-2', repo='ANetCore', desc='Open 不把键 11 放进 Opened.Notices(CORE-A2)',
  edits=[('seal/envelope.go', 'Access: outer.Access, RK: inner.RK, Notices: inner.Notices, Grant: inner.Grant,', 'Access: outer.Access, RK: inner.RK, Grant: inner.Grant,')],
  tests=[('ANetCore:./seal', '^(TestSealOpenRoundTrip|TestSEALED_2A)$')], full=['ANetCore:./seal']),
 dict(id='core2-23', group='CORE-2', repo='ANetCore', desc='Seal 出错时仍返回已导出的 ack_key',
  edits=[('seal/envelope.go', 'return nil, ack, fail(ReasonInvalidInput, "signer used key_state_seq', 'return nil, key, fail(ReasonInvalidInput, "signer used key_state_seq')],
  tests=[('ANetCore:./seal', '^TestSealInputChecks$')], full=['ANetCore:./seal']),
]

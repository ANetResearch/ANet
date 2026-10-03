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
# Group CORE-4 (hub wire 3, ANetHub internal/wire3 and the hub's root): each mutation undoes one rule
# of the negotiation (core CORE-V1), the error body (G1, §2.15.1), strict decoding (CORE-E7) or the
# U+0000 checks (CORE-E9), and must turn the unit test of that rule and the testdata/wire3 fixtures red.

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
 # ---- CORE-4: hub wire 3 (ANetHub internal/wire3, the hub's root, /hub/identity, the fixtures)
 dict(id='core4-older', group='CORE-4', repo='ANetHub', desc='CORE-V1:旧线协或缺头的写请求不再回 426',
  edits=[('internal/wire3/wire3.go', '\t\t\tif gated(r) {\n', '\t\t\tif false && gated(r) {\n')],
  tests=[('ANetHub:./internal/wire3', '^TestContractNegotiation$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3', 'ANetHub:./internal/aghub']),
 dict(id='core4-fedget', group='CORE-4', repo='ANetHub', desc='CORE-V1:/fed/* 的 GET 当作只读放行',
  edits=[('internal/wire3/wire3.go', 'range []string{"/relay", "/fed"} {', 'range []string{"/relay"} {')],
  tests=[('ANetHub:./internal/wire3', '^TestGated$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3', 'ANetHub:./cmd/anet-hub']),
 dict(id='core4-newer', group='CORE-4', repo='ANetHub', desc='CORE-V1:更新的线协不再回 400 wire_too_new',
  edits=[('internal/wire3/wire3.go', '\t\tcase declaredNewer:\n\t\t\tWrite(w, Refusal{Reason: ReasonWireTooNew})\n\t\t\treturn\n', '')],
  tests=[('ANetHub:./internal/wire3', '^TestContractNegotiation$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-stamp', group='CORE-4', repo='ANetHub', desc='CORE-V1:成功响应不带 X-ANet-Wire',
  edits=[('internal/wire3/wire3.go', '\t\tw.Header().Set(Header, versionText)\n\t\tif r.Method == http.MethodOptions {', '\t\tif r.Method == http.MethodOptions {')],
  tests=[('ANetHub:./internal/wire3', '^TestContractNegotiation$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-transport', group='CORE-4', repo='ANetHub', desc='CORE-V1:hub 对外请求不带 X-ANet-Wire',
  edits=[('internal/wire3/wire3.go', '\tr2.Header.Set(Header, versionText)\n', '\t_ = versionText\n')],
  tests=[('ANetHub:./internal/wire3', '^TestTransport$'), ('ANetHub:./internal/federation', '^TestPeerRequestsStateTheWire$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-fedclient', group='CORE-4', repo='ANetHub', desc='CORE-V1:联邦客户端不用 wire3.Transport',
  edits=[('internal/federation/federation.go', 'Transport: wire3.Transport(nil)},', 'Transport: func() http.RoundTripper { _ = wire3.Transport; return nil }()},')],
  tests=[('ANetHub:./internal/federation', '^TestPeerRequestsStateTheWire$')],
  full=['ANetHub:./internal/federation']),
 dict(id='core4-root', group='CORE-4', repo='ANetHub', desc='CORE-V1:anet-hub 的根(/hub/identity、/fed/*、模块)不在 Contract 之下',
  edits=[('cmd/anet-hub/mounts.go', 'func hubHandler(root http.Handler) http.Handler { return wire3.Contract(root) }', 'func hubHandler(root http.Handler) http.Handler { _ = wire3.Contract; return root }')],
  tests=[('ANetHub:./cmd/anet-hub', '^TestTheServedHubSpeaksWire3$')],
  full=['ANetHub:./cmd/anet-hub']),
 dict(id='core4-identity', group='CORE-4', repo='ANetHub', desc='§2.12.2:/hub/identity 不带 wire',
  edits=[('internal/hubid/hubid.go', 'KEL: base64.StdEncoding.EncodeToString(i.KEL), Wire: wire3.Version})', 'KEL: base64.StdEncoding.EncodeToString(i.KEL)})')],
  tests=[('ANetHub:./internal/aghub', '^TestWire3Fixtures$'), ('ANetHub:./cmd/anet-hub', '^TestTheServedHubSpeaksWire3$')],
  full=['ANetHub:./internal/aghub']),
 dict(id='core4-required', group='CORE-4', repo='ANetHub', desc='CORE-V1:426 不带 required_anet',
  edits=[('internal/wire3/refusal.go', '\t\textra["required_anet"] = RequiredAnet\n', '')],
  tests=[('ANetHub:./internal/wire3', '^TestWireTooOldNamesWhatToInstall$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-retry', group='CORE-4', repo='ANetHub', desc='G1:等待类拒绝不带 Retry-After',
  edits=[('internal/wire3/refusal.go', '\tif Waits(info.status) {\n', '\tif false && Waits(info.status) {\n')],
  tests=[('ANetHub:./internal/wire3', '^TestRetryAfter$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-status', group='CORE-4', repo='ANetHub', desc='§2.15.1:kid_stale 改用 400',
  edits=[('internal/wire3/reasons.go', '\tReasonKidStale: {http.StatusConflict, nil,', '\tReasonKidStale: {http.StatusBadRequest, nil,')],
  tests=[('ANetHub:./internal/wire3', '^TestReasonTableIsTheRegistry$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-dropped', group='CORE-4', repo='ANetHub', desc='§2.15.1:原因表少一个码(probation)',
  edits=[('internal/wire3/reasons.go', '\tReasonProbation: {http.StatusForbidden, nil,\n\t\t"New accounts cannot upload reviews during probation; retry after it ends."},\n', '')],
  tests=[('ANetHub:./internal/aghub', '^TestWireRegistry$'), ('ANetHub:./internal/wire3', '^TestReasonTableIsTheRegistry$')],
  full=['ANetHub:./internal/aghub']),
 dict(id='core4-unregistered', group='CORE-4', repo='ANetHub', desc='CORE-K19/S3:新增未登记的 hub 原因常量',
  edits=[('internal/wire3/reasons.go', '\tReasonReviewWindowClosed      Reason = "review_window_closed"\n', '\tReasonReviewWindowClosed      Reason = "review_window_closed"\n\tReasonZZ                      Reason = "zz_unregistered"\n')],
  tests=[('ANetHub:./internal/aghub', '^TestWireRegistry$')],
  full=['ANetHub:./internal/aghub']),
 dict(id='core4-unknown', group='CORE-4', repo='ANetHub', desc='CORE-E7:未知成员被接受',
  edits=[('internal/wire3/strict.go', '\t\t\t\treturn &DecodeError{Kind: KindUnknownMember, Member: p}\n', '\t\t\t\tft = nil\n')],
  tests=[('ANetHub:./internal/wire3', '^TestDecodeStrictRefuses$'), ('ANetHub:./internal/aghub', '^(TestWire3Fixtures|TestWire3JSONFields)$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-case', group='CORE-4', repo='ANetHub', desc='CORE-E7:成员名不分大小写',
  edits=[('internal/wire3/strict.go', '\t\t\tif ft, known = fields[key]; !known {', '\t\t\tif ft, known = fields[strings.ToLower(key)]; !known {')],
  tests=[('ANetHub:./internal/wire3', '^TestDecodeStrictRefuses$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-dup', group='CORE-4', repo='ANetHub', desc='CORE-E7:重复成员被接受',
  edits=[('internal/wire3/strict.go', '\t\t\treturn &DecodeError{Kind: KindDuplicateMember, Member: p}\n', '\t\t\t_ = p\n')],
  tests=[('ANetHub:./internal/wire3', '^TestDecodeStrictRefuses$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-trailing', group='CORE-4', repo='ANetHub', desc='CORE-E7:JSON 值之后的数据被接受(走查与标准解码两处都放过)',
  edits=[('internal/wire3/strict.go', '\tif _, err := dec.Token(); err != io.EOF {\n', '\tif _, err := dec.Token(); err != io.EOF && false {\n'),
         ('internal/wire3/strict.go', '\tif err := json.Unmarshal(body, v); err != nil {\n', '\tif err := json.NewDecoder(bytes.NewReader(body)).Decode(v); err != nil {\n')],
  tests=[('ANetHub:./internal/wire3', '^TestDecodeStrictRefuses$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-poll', group='CORE-4', repo='ANetHub', desc='CORE-E7:/relay/poll 改回宽松解码',
  edits=[('internal/aghub/server.go', '\t\tif err := wire3.DecodeStrict(a.Body, &req); err != nil {', '\t\tif err := json.Unmarshal(a.Body, &req); err != nil {')],
  tests=[('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/aghub']),
 dict(id='core4-nulbody', group='CORE-4', repo='ANetHub', desc='CORE-E9:JSON 文本中的 U+0000 被接受',
  edits=[('internal/wire3/strict.go', '\tcase string:\n\t\tif strings.IndexByte(v, 0) >= 0 {', '\tcase string:\n\t\tif false && strings.IndexByte(v, 0) >= 0 {')],
  tests=[('ANetHub:./internal/wire3', '^TestDecodeStrictRefuses$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-nulpath', group='CORE-4', repo='ANetHub', desc='CORE-E9:路径中的 U+0000 被接受',
  edits=[('internal/wire3/nul.go', '\tif err := CheckText("path", r.URL.Path); err != nil {\n\t\treturn err\n\t}\n', '')],
  tests=[('ANetHub:./internal/wire3', '^TestContractRefusesNUL$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-nulheader', group='CORE-4', repo='ANetHub', desc='CORE-E9:X-ANet-* 头中的 U+0000 被接受',
  edits=[('internal/wire3/nul.go', '\t\tif !strings.HasPrefix(strings.ToLower(name), "x-anet-") {', '\t\tif true || !strings.HasPrefix(strings.ToLower(name), "x-anet-") {')],
  tests=[('ANetHub:./internal/wire3', '^TestContractRefusesNUL$'), ('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/wire3']),
 dict(id='core4-method', group='CORE-4', repo='ANetHub', desc='G1:其他方法落到网页时不回 not_found',
  edits=[('internal/aghub/server.go', '\t\tif r.Method != http.MethodGet && r.Method != http.MethodHead {\n\t\t\twire3.Refuse(w, wire3.ReasonNotFound)\n\t\t\treturn\n\t\t}\n', '')],
  tests=[('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/aghub']),
 dict(id='core4-page', group='CORE-4', repo='ANetHub', desc='G1:不存在的网页回文本 404',
  edits=[('internal/aghub/server.go', '\t\tif _, err := fs.Stat(webFS, strings.TrimPrefix(path.Clean(r.URL.Path), "/")); err != nil {\n', '\t\tif _, err := fs.Stat(webFS, strings.TrimPrefix(path.Clean(r.URL.Path), "/")); err != nil && false {\n')],
  tests=[('ANetHub:./internal/aghub', '^TestWire3Fixtures$')],
  full=['ANetHub:./internal/aghub']),
]

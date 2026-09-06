package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

// A chain must be readable by the daemon that wrote it.
//
// It was not. The chain was stored as JSON while an event id is derived
// from the record's CBOR preimage, and CBOR distinguishes a byte string
// from text where JSON does not — so a payload carrying a receipt came back
// as base64 text, the preimage changed, and the id failed to re-derive. The
// ledger verifies before use, so the daemon refused to start against its
// own chain with "tamper/fork?", on the first restart after accepting a
// result.
//
// Latent until then, which is why nothing caught it: every other test
// builds a fresh chain, and a chain is only read back on a restart with
// history in it.
func TestChainWithBinaryPayloadReloads(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evidence.ael.jsonl")

	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the shape that broke it: a result carrying a signed receipt.
	if _, err := led.Append(EvResultAccepted, map[string]any{
		"interaction_id": "ix-1",
		"result_cid":     "bafy...",
		"receipt_bytes":  []byte{0xa1, 0x02, 0x03, 0xff},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.Append(EvDelegationSent, map[string]any{
		"interaction_id": "ix-2", "provider_aid": "aid", "request_cid": "bafy...",
	}); err != nil {
		t.Fatal(err)
	}
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatalf("the daemon must be able to reload its own chain: %v", err)
	}
	defer reopened.Close()

	// And it must continue the chain rather than restart it: a fork in a
	// node's own evidence is the thing the ledger exists to make impossible.
	if reopened.nextSeq != 2 {
		t.Fatalf("nextSeq = %d after two records, want 2", reopened.nextSeq)
	}
	if _, err := reopened.Append(EvCapabilityEffect, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("appending after a reload must work: %v", err)
	}
}

// A chain written by an older daemon still loads, as long as its records
// re-derive. Refusing to start on an upgrade would be a worse failure than
// the one this fixes.
func TestLegacyJSONChainStillLoads(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.ael.jsonl")

	// Write one record in the current format, then re-encode it as the JSON
	// an older daemon would have written.
	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.Append(EvDelegationSent, map[string]any{
		"interaction_id": "ix-1", "provider_aid": "aid", "request_cid": "bafy...",
	}); err != nil {
		t.Fatal(err)
	}
	led.Close()

	line, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := decodeRecord([]byte(trimNewline(string(line))))
	if err != nil {
		t.Fatal(err)
	}
	// What an older daemon wrote: json.Marshal of the same record.
	rec.Payload = plainMap(rec.Payload)
	legacy, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(legacy, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := openEvidenceLedger(path, self); err != nil {
		t.Fatalf("a chain from an older daemon must still load: %v", err)
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// The chain has to be readable by the node that keeps it.
//
// It was not, for its whole existence. Every capability effect, receipt
// and accepted result went on, and nothing read it back — an audit
// substrate whose only reader was the verifier that refuses to start.
// The operator accumulating the evidence was the one person who could not
// look at it.
func TestTheChainCanBeRead(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	led, err := openEvidenceLedger(filepath.Join(t.TempDir(), "e.jsonl"), self)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()

	for i := 0; i < 3; i++ {
		if _, err := led.Append(EvCapabilityEffect, map[string]any{
			"capability": "cas.put", "n": i,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := led.Append(EvResultAccepted, map[string]any{
		"interaction_id": "ix-1",
		// Bytes are why this chain is CBOR; the read surface has to render
		// them as something JSON can carry.
		"receipt_bytes": []byte{0xde, 0xad},
	}); err != nil {
		t.Fatal(err)
	}

	head, recs := led.Evidence(EvidenceQuery{})
	if head.ChainDID != self.DID() {
		t.Errorf("chain did = %q", head.ChainDID)
	}
	if head.Length != 4 {
		t.Errorf("length = %d, want 4", head.Length)
	}
	if head.State != "ACTIVE" {
		t.Errorf("state = %q, want ACTIVE", head.State)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}

	// The links must come back, or a reader can only believe the node.
	for i, r := range recs {
		if r.ID == "" || r.Sig == "" {
			t.Errorf("record %d has no id or signature — nothing to check", i)
		}
		if i > 0 && r.PrevID != recs[i-1].ID {
			t.Errorf("record %d does not link to %d", i, i-1)
		}
	}
	if head.HeadID != recs[len(recs)-1].ID {
		t.Errorf("head id %s is not the last record %s", head.HeadID, recs[len(recs)-1].ID)
	}
	if got := recs[3].Payload["receipt_bytes"]; got != "3q0=" {
		t.Errorf("bytes rendered as %v, want base64 3q0=", got)
	}

	// Filters.
	_, effects := led.Evidence(EvidenceQuery{EventType: EvCapabilityEffect})
	if len(effects) != 3 {
		t.Errorf("type filter returned %d, want 3", len(effects))
	}
	_, since := led.Evidence(EvidenceQuery{Since: 2})
	if len(since) != 2 {
		t.Errorf("since filter returned %d, want 2", len(since))
	}
	// A limit keeps the tail: "what just happened" is the question.
	_, tail := led.Evidence(EvidenceQuery{Limit: 1})
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Errorf("limit returned %+v, want the newest record", tail)
	}
}

// An empty chain is not a broken one, and a node with no ledger at all
// must answer rather than crash.
func TestReadingAnEmptyChain(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	led, err := openEvidenceLedger(filepath.Join(t.TempDir(), "e.jsonl"), self)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	head, recs := led.Evidence(EvidenceQuery{})
	if head.State != "ACTIVE" {
		t.Errorf("an empty chain must read as ACTIVE, got %q", head.State)
	}
	if head.Length != 0 || len(recs) != 0 {
		t.Errorf("empty chain returned %d records, length %d", len(recs), head.Length)
	}
	if head.HeadID != "" {
		t.Errorf("an empty chain has no head, got %q", head.HeadID)
	}
}

// 一次被杀死的写入只损坏它正在写的那条记录, 那是最后一条。守着它不放会让
// 节点下线, 而链的完整性由 Import 单独把关 —— 位置能区分这两件事, 错误类型不能。
//
// 位置 internal/daemon/ledger.go:openEvidenceLedger; 行为 任何一行解码失败都
// 返回错误, 调用方 daemon.go:135 直接放弃启动; 影响 一次 OOM 之后 daemon 在
// systemd 下每 5 秒崩溃重启一次, 节点从网络上消失, 只能手工编辑账本才能恢复;
// 发现方式 RK3588 板子上转换大模型时 OOM killer 落在 append 中途, 实网复现。
func TestATornTrailingRecordDoesNotKeepTheNodeDown(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "e.jsonl")

	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := led.Append(EvCapabilityEffect, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	led.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟半截写入: 追加一条被截断的记录, 不带换行 —— 进程死在 Write 中途就是这样。
	torn := []byte("YmFkIHRvcm4gdGFpbCB3aXRob3V0IGEgdmFsaWQgY2Jvcg")
	if err := os.WriteFile(path, append(append([]byte{}, good...), torn...), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatalf("半截写入不该挡住启动: %v", err)
	}
	defer reopened.Close()

	// 丢失必须记在链上, 而不是只进日志 —— 校验链的人看不到日志。
	_, recs := reopened.Evidence(EvidenceQuery{Limit: 100})
	var gaps int
	for _, r := range recs {
		if r.EventType == EvLedgerGap {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("应有 1 条 %s 事件记录这次丢失, 实得 %d", EvLedgerGap, gaps)
	}
	if len(recs) != 4 {
		t.Fatalf("三条原记录加一条缺口标记应为 4, 实得 %d", len(recs))
	}

	// 修好之后还能继续追加, 且再开一次不再报缺口。
	if _, err := reopened.Append(EvReceipt, map[string]any{"after": "repair"}); err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	again, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatalf("修复后再开应正常: %v", err)
	}
	defer again.Close()
	_, recs2 := again.Evidence(EvidenceQuery{Limit: 100})
	if len(recs2) != 5 {
		t.Fatalf("再开后应为 5 条, 实得 %d", len(recs2))
	}
}

// 中间一行坏掉不可能由崩溃造成 —— 文件是追加式的, 崩溃只能伤到正在写的那条。
// 所以这种形态仍然拒绝启动, 宽容到这里就是纵容篡改。
func TestCorruptionInTheMiddleStillRefusesToLoad(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "e.jsonl")

	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := led.Append(EvCapabilityEffect, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	led.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("前置条件: 应有 3 行, 实得 %d", len(lines))
	}
	lines[1] = []byte("bm90IGEgdmFsaWQgcmVjb3JkIGF0IGFsbCwgaW4gdGhlIG1pZGRsZQ")
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := openEvidenceLedger(path, self); err == nil {
		t.Fatal("中间行损坏必须拒绝启动")
	} else if !strings.Contains(err.Error(), "corrupt line 2 of 3") {
		t.Fatalf("错误应点明是第几行, 实得: %v", err)
	}
}

package a2ashape

import (
	"strings"
	"testing"
	"time"
)

// freshInstall is the fixture of ux §4.7's example: an admitted node that has
// just been initialised, with the recovery passphrase set.
func freshInstall() StatusInput {
	return StatusInput{
		IdentityState: "ok",
		Identity: &Said{Key: "identity.ok", Args: Args{"name": "default", "aid": "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd",
			"hub": "https://hub.example", "ago": 4 * time.Minute}},
		Recovery:      "protected",
		Hub:           Said{Key: "registered", Args: Args{"hub": "https://hub.example"}},
		HubRegistered: true,
		Inbound:       Said{Key: "inbound.summary.closed", Args: Args{"n": 0, "m": 0, "auto_reply": "off"}},
		Payments: Said{Key: "limits_summary.off", Args: Args{"auto_max": 0, "agent_max": 0, "agent_left": 0,
			"explicit_max": 10, "left": 50, "n": 0}},
		Retention: Said{Key: "retention.summary", Args: Args{"retention": "30d"}},
	}
}

// renderSummary prints the lines as `anet status` does: "label  text".
func renderSummary(lines []SummaryLine, lang Language, width int) string {
	var b strings.Builder
	for _, l := range lines {
		label := SummaryLabel(l.Topic, lang)
		w := 0
		for _, r := range label {
			if r > 0x2e80 {
				w += 2
			} else {
				w++
			}
		}
		b.WriteString(label + strings.Repeat(" ", max(2, width-w)) + l.Text + "\n")
		if l.Next != "" {
			b.WriteString("  → " + l.Next + "\n")
		}
	}
	return b.String()
}

// TestStatusSummaryFreshInstall is ux §4.7's example, line for line.
func TestStatusSummaryFreshInstall(t *testing.T) {
	const zh = `身份    default …7hq2abcd，密钥已由 https://hub.example 确认（4 分钟前）。
hub     已在 https://hub.example 注册。
接单    只接受 allow 名单里 0 个对端的任务；公开 0 项能力；自动回复关。
付款    自动付款与 agent 付款都关着（额度为 0）；人工付款（anet pay）单笔 ≤10，今日还能付 50。
隐私    任务内容在任务结束 30 天后删除（公开调用 7 天）。
`
	got := renderSummary(StatusSummary(freshInstall(), Chinese), Chinese, 8)
	// The product uses full-width punctuation where the spec's example has
	// half-width; the words are the spec's.
	norm := func(s string) string {
		return strings.NewReplacer("，", ",", "（", "(", "）", ")", "；", ";", "：", ":").Replace(s)
	}
	if norm(got) != norm(zh) {
		t.Errorf("zh summary:\n%s\nwant:\n%s", got, zh)
	}
	en := StatusSummary(freshInstall(), English)
	if len(en) != 5 {
		t.Fatalf("%d lines in English", len(en))
	}
	if !strings.Contains(en[2].Text, "Takes tasks only from the 0 peers on the allow list; 0 public capabilities; auto-reply off.") {
		t.Errorf("inbound line %q", en[2].Text)
	}
}

// The inbound line has one sentence per policy and says whether auto-reply
// is on.
func TestInboundSummaryByPolicy(t *testing.T) {
	for _, c := range []struct {
		key  string
		args Args
		zh   string
	}{
		{"inbound.summary.approve", Args{"k": 3, "m": 2, "auto_reply": "on"}, "名单外对端的任务先进待批队列（现有 3 个待批）；公开 2 项能力；自动回复开。"},
		{"inbound.summary.open", Args{"m": 0, "auto_reply": "off"}, "接受任何人的任务；公开 0 项能力；自动回复关。"},
	} {
		if got := Sentence(c.key, Chinese, c.args); got != c.zh {
			t.Errorf("%s: %q, want %q", c.key, got, c.zh)
		}
	}
}

// A line another area already worded is shown as it is, with its level and
// its next step.
func TestSummaryShowsAWordedLineAsItIs(t *testing.T) {
	in := freshInstall()
	in.Sandbox = &Said{Key: "sandbox.ready", Text: "Worded elsewhere.", NextText: "anet doctor", Lvl: LevelWarn}
	in.SandboxBudget = &Said{Key: "sandbox.budget", Args: Args{"n": 1, "daily": 20, "per_peer": 5}}
	for _, l := range StatusSummary(in, English) {
		if l.Topic != TopicSandbox {
			continue
		}
		if !strings.HasPrefix(l.Text, "Worded elsewhere.") || !strings.Contains(l.Text, "1/20 runs") || l.Next != "anet doctor" || l.Level != LevelWarn {
			t.Errorf("sandbox line %+v", l)
		}
		return
	}
	t.Fatal("no sandbox line")
}

// The recovery line shows between the identity and hub lines, and not for a
// stopped identity (ux §4.7, line 1a).
func TestNoHubHasNoIdentityLine(t *testing.T) {
	in := freshInstall()
	in.IdentityState = "no_hub"
	in.Hub = Said{Key: "no_hub_yet"}
	in.HubRegistered = false
	for _, l := range StatusSummary(in, English) {
		if l.Topic == TopicIdentity {
			t.Fatalf("an identity line with no hub: %+v", l)
		}
	}
}

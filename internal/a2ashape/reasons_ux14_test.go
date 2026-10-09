package a2ashape

import (
	"regexp"
	"strings"
	"testing"
)

// ux §5.2 (the "next step" column added 2026-10-04): a doctor-only sentence that tells the reader to
// run a command has that command as its next step, so doctor's hint is the command and not "-"
// (UX-K4); one that names no command has none. The command is the same in both languages. [mut] the
// next step of doctor.daemon_down removed → red.
func TestDoctorSentencesNameTheirNextStep(t *testing.T) {
	args := Args{"path": "/home/u/.anet-recovery/x.anetrec", "name": "face.compare", "n": 3, "m": 2,
		"code": "bad_signature", "key": "HTTPS_PROXY", "daily": 100, "time": "now"}
	want := map[string]string{
		"identity.recovery_perm":        "chmod 600 /home/u/.anet-recovery/x.anetrec",
		"identity.recovery_leftover":    "anet identity status",
		"payments.no_payees":            "anet payees add <AID>",
		"doctor.daemon_down":            "anet up",
		"capabilities.allow_only_empty": "anet peers allow <AID>",
		"payments.card_failed":          "anet doctor --json",
	}
	doctorOnly := []string{"identity.recovery_note", "identity.recovery_perm", "identity.recovery_leftover",
		"sender_privacy.mailbox_pending", "clock.ts_retries", "payments.no_payees", "doctor.daemon_down", "clock.ok",
		"sender_privacy.grants_summary", "sender_privacy.withdrawals_pending", "sender_privacy.hub_refusals",
		"sender_privacy.hub_small_bucket", "sender_privacy.hub_epoch_invalid", "p2p.lan_on", "keys.epoch_ok",
		"capabilities.files_not_service", "capabilities.inflight_low", "capabilities.allow_only",
		"capabilities.allow_only_empty", "preview.no_source", "payments.audit_ok", "payments.reviews_pending",
		"payments.reviews_stuck", "payments.card_failed", "payments.price_invalid", "autoreply.off",
		"sandbox.proxy_env", "sandbox.worst_case", "backends.path_only", "anetlink.not_remote"}
	for _, k := range doctorOnly {
		if _, ok := Lookup(k); !ok {
			t.Errorf("%s: no row", k)
			continue
		}
		for _, lang := range []Language{English, Chinese} {
			if got := Next(k, lang, args); got != want[k] {
				t.Errorf("%s (%v): next step %q, want %q", k, lang, got, want[k])
			}
		}
	}
	// Each command is the one its sentence names (identity.recovery_leftover names any anet identity
	// command; the hint is the one that only reads).
	for k, n := range want {
		if k == "identity.recovery_leftover" {
			n = "anet identity"
		}
		for _, lang := range []Language{English, Chinese} {
			s := Sentence(k, lang, args)
			if !strings.Contains(s, n) {
				t.Errorf("%s (%v): the sentence %q does not name %q", k, lang, s, n)
			}
		}
	}
}

// ux UX-G8a, §4.3 (2026-10-04): the <…> hints in the commands a removed command's sentence names follow
// the language — <任务>, <网址>, <编号>, <工具> in Chinese, <task>, <url>, <id>, <tool> in English — and
// each language has as many as the other. [mut] a Chinese hint written in English again → red.
func TestRemovedCommandHintsFollowTheLanguage(t *testing.T) {
	hint := regexp.MustCompile(`<([^<>]+)>`)
	latin := regexp.MustCompile(`^[a-z_]+$`)
	n := 0
	for id, p := range phraseSets["cli_instead"] {
		zh, en := hint.FindAllStringSubmatch(p.zh, -1), hint.FindAllStringSubmatch(p.en, -1)
		if len(zh) != len(en) {
			t.Errorf("%s: %d hints in Chinese, %d in English", id, len(zh), len(en))
		}
		for _, m := range zh {
			n++
			if latin.MatchString(m[1]) {
				t.Errorf("%s: the Chinese sentence has the English hint <%s>: %q", id, m[1], p.zh)
			}
		}
		for _, m := range en {
			if !latin.MatchString(m[1]) {
				t.Errorf("%s: the English sentence has the hint <%s>: %q", id, m[1], p.en)
			}
		}
	}
	if n < 4 {
		t.Fatalf("only %d hints found", n)
	}
	for id, want := range map[string]string{"delegate_pay": "anet pay <任务>", "hub_register_token": "anet hub-register <网址>",
		"accept_end": "anet end <编号>", "install": "anet agents wire <工具>"} {
		if s := Sentence("cli_removed", Chinese, Args{"what": "x", "instead": id}); !strings.Contains(s, want) {
			t.Errorf("%s: %q does not name %q", id, s, want)
		}
	}
}

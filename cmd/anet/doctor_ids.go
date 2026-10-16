package main

// doctor_ids.go is the register of `anet doctor` check ids (v0.3.0 ux §5.2):
// every id doctor may print, and the sentence keys of the reason table its
// findings may use. The id is the stable name tests assert on (UX-K5); an
// area that adds a check adds its id here, with its keys.

import "strings"

// doctorID is one registered check id. An id ending in ".*" stands for every
// id under that prefix (agents.<tool>, hermes.*).
type doctorID struct {
	id string
	// keys are the reason-table keys the check's findings use: ok and info
	// sentences first, then warn, fail and unknown (UX-K7). An id that keeps
	// its v0.2 wording until its area rewrites it has none yet.
	keys []string
}

// doctorIDs is the register, in the order of ux §5.2.
var doctorIDs = []doctorID{
	{id: "version"},
	{id: "version.signature"},
	{id: "modules", keys: []string{"module_absent.configured", "config_invalid"}},
	{id: "config"},
	{id: "config.removed", keys: []string{"config_key_removed"}},
	{id: "si5"},
	{id: "control"},
	{id: "control.token"},
	{id: "identity"},
	{id: "identity.format", keys: []string{"identity_v02", "datadir_too_new", "identity_file_invalid"}},
	// identity.witness says /identity/status's sentence: in no_hub the hub
	// row's (ux UX-C9), and while a key event is published rotation_pending
	// or clock_skew.
	{id: "identity.witness", keys: []string{"identity.ok", "no_hub_yet", "hub_pending_admission", "hub_retrying",
		"hub_refused", "old_hub_unacked", "identity.confirm_aging", "identity.quarantine", "identity.hub_record_invalid",
		"identity.hub_kel_fork", "identity.hub_unauthorized", "rotation_pending", "clock_skew", "notice_stale",
		"identity_stopped", "identity_deactivated"}},
	{id: "identity.recovery", keys: []string{"identity.status.recovery", "identity.recovery_note", "identity.unprotected",
		"identity.unprotected.resume", "identity.recovery_perm", "identity.recovery_leftover"}},
	{id: "identity.rotation", keys: []string{"rotation_pending"}},
	{id: "hub", keys: []string{"registered", "no_hub_yet", "hub_pending_admission", "hub_retrying", "hub_refused",
		"wire_too_old", "wire_too_new"}},
	{id: "hub.probation", keys: []string{"registered.probation", "balance.trial_pending"}},
	{id: "delivery.outbox", keys: []string{"outbox.ok", "outbox.none", "outbox.stuck", "outbox.unconfirmed", "outbox.unreadable"}},
	{id: "tasks.no_response", keys: []string{"no_response_after.set", "no_response_after.off", "config_invalid"}},
	{id: "sender_privacy.mailbox_key", keys: []string{"sender_privacy.mailbox_pending"}},
	{id: "sender_privacy.session", keys: []string{"session_unavailable"}},
	{id: "sender_privacy.grants", keys: []string{"sender_privacy.grants_summary", "sender_privacy.withdrawals_pending"}},
	{id: "sender_privacy.hub", keys: []string{"sender_privacy.hub_epoch_invalid", "sender_privacy.hub_refusals",
		"sender_privacy.hub_small_bucket"}},
	{id: "p2p.lan", keys: []string{"p2p.lan_on"}},
	{id: "clock", keys: []string{"clock.ok", "clock.no_hub", "clock.skewed", "clock.unverified", "clock.ts_retries"}},
	{id: "keys.epoch", keys: []string{"keys.epoch_ok", "keys.kept_long", "keys.unknown_drops"}},
	{id: "retention", keys: []string{"retention.summary", "retention.cow", "retention_invalid"}},
	{id: "local_state", keys: []string{"local_state.detected", "local_state.repeated"}},
	{id: "inbound"},
	{id: "inbound.lists"},
	{id: "capabilities.files", keys: []string{"capabilities.files_not_service", "capabilities.files_not_service.absent"}},
	{id: "capabilities.inflight", keys: []string{"capabilities.inflight_low"}},
	{id: "capabilities.allow_only", keys: []string{"capabilities.allow_only", "capabilities.allow_only_empty"}},
	{id: "preview", keys: []string{"preview.on", "preview.no_source", "preview.with_reasoning"}},
	{id: "payments", keys: []string{"limits_summary", "limits_summary.off", "payments.no_payees"}},
	{id: "payments.audit", keys: []string{"payments.audit_ok", "proof_expired", "balance_mismatch", "omitted_from_ledger",
		"unknown_debit", "proof_invalid", "proof_unavailable"}},
	{id: "payments.reviews", keys: []string{"payments.reviews_pending", "payments.reviews_stuck"}},
	{id: "payments.card", keys: []string{"payments.card_no_public", "payments.card_failed", "payments.price_invalid"}},
	{id: "payments.unchargeable", keys: []string{"payments.unchargeable"}},
	{id: "auto_reply", keys: []string{"autoreply.off", "autoreply.summary", "sandbox.trusted_direct", "sandbox.trusted_sandbox"}},
	{id: "sandbox", keys: []string{"sandbox.ready", "sandbox.ready_egress", "platform_unsupported", "no_bwrap",
		"userns_blocked", "userns_unavailable", "netns_failed", "seccomp_failed", "fs_isolation_failed", "no_sandbox_exec",
		"netns_failed.doctor", "seccomp_failed.doctor", "fs_isolation_failed.doctor", "sandbox_start_failed"}},
	{id: "sandbox.agent", keys: []string{"agent_unsupported", "agent_unsupported.combo"}},
	{id: "sandbox.upstream", keys: []string{"no_api_key", "no_api_base", "no_model", "api_base_insecure"}},
	{id: "sandbox.egress", keys: []string{"sandbox.ready_egress", "sandbox.proxy_env", "egress_allow_invalid",
		"egress_allow_model_host", "egress_allow_too_many"}},
	{id: "sandbox.budget", keys: []string{"sandbox.budget", "sandbox.worst_case", "doctor.daemon_down"}},
	{id: "sandbox.limits", keys: []string{"no_cgroup_limits"}},
	{id: "backends.peer_check", keys: []string{"backends.path_only", "peer_cred_unsupported"}},
	{id: "backend.transport"},
	{id: "a2a.backends"},
	{id: "a2a.backends.retry"},
	{id: "anetlink.socket", keys: []string{"peer_uid_mismatch", "c1_version"}},
	{id: "anetlink.publish", keys: []string{"anetlink.not_remote"}},
	{id: "a2a"},
	{id: "a2a.port"},
	{id: "a2a.token"},
	{id: "agents"},
	{id: "agents.*"},
	{id: "hermes.*"},
	{id: "official"},
}

// doctorDaemonDown is the sentence of a check that needs the running daemon
// while it is not running (UX-K2).
const doctorDaemonDown = "doctor.daemon_down"

// doctorIDKnown reports whether id is registered.
func doctorIDKnown(id string) bool {
	for _, d := range doctorIDs {
		if d.id == id {
			return true
		}
		if p, ok := strings.CutSuffix(d.id, "*"); ok && strings.HasPrefix(id, p) && len(id) > len(p) {
			return true
		}
	}
	return false
}

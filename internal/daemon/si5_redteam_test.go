package daemon

// si5_redteam_test.go: the SI-5 red-team findings (A2A-DESIGN §1, §5, §6)
// and where each one's regression test is now.

// (The migrated-node finding — a stranger whose task a v0.1 node accepted
// under the old accept-anyone default kept the standing of an allowed peer
// after the migration to policy closed — went with the migration itself:
// v0.3.0 opens no store an earlier anet wrote (core CORE-V4), so a v0.1 row
// cannot exist; refuse_v02_test.go checks the refusal.)

// (F7 — on the migrated node the legacy stranger's delegate swap ran a
// capability the node never made public — is fixed; its regression tests
// are redelivery_bind_test.go's swap tests, the migrated row among them
// until v0.3.0 removed the migration.)

// (F9 — SetInboundPolicy put the policy in force before saving it and kept
// it when the save failed — is fixed; its regression test is
// policy_save_test.go.)

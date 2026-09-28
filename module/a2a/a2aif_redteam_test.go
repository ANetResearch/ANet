//go:build !no_a2a

package a2a

// a2aif_redteam_test.go: adversarial PoCs for the local A2A interface's
// HTTP face. Each test asserted that the attack SUCCEEDS; both defects
// found here are fixed, and their regression tests live elsewhere:
//
//   - (F32 — a provider's file inline in a stream event broke every a2a-go
//     client's stream — is fixed; its regression test is q12_test.go.)
//   - (F18 — a recorded port found taken at start was given up for a new
//     one, and wired clients went on handing the bearer token to whoever
//     held the old port — is fixed; its regression test is
//     portsquat_test.go.)

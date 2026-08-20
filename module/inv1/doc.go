// Package inv1 holds INV-1: org-scoped data must never enter the peer-to-peer
// fabric.
//
// The org form is centralised. Every org object travels point-to-point to
// org-central, never over a gossip topic, a DHT, or any other path where a
// third party could see it. That is not a performance choice — an org's
// shared cognition and its credentials are its secrets, and a fabric that
// replicates them by design has no way to take them back.
//
// anet3 enforced this two ways, and both come across:
//
//   - A structural test: org packages must not import a gossip or DHT layer,
//     so there is no code path by which an org object could reach one.
//   - A runtime tripwire: org-scoped types carry the OrgScoped marker, and
//     every publish boundary calls GuardCommonsPublish to reject them.
//
// The second matters more here than it did there. anet4 has a transport
// seam: delivery walks a list, and a module can add a path the daemon knows
// nothing about. That is the refactor the static scan cannot anticipate —
// which is exactly the case the runtime guard was written for.
package inv1

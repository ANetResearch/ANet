package daemon

// The values of auto_reply.untrusted and auto_reply.trusted the kernel itself has to name: `anet init`
// writes the safe default, the inbound policy view reports it and `/status` marks a node that runs
// agents in a sandbox. The auto-reply module owns the setting and keeps its own copy of the words.
const (
	untrustedOff     = "off"
	untrustedSandbox = "sandbox"
	trustedSandbox   = "sandbox"
)

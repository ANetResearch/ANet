package daemon

// published_caps.go limits what a registration tells the hub this node
// serves to its public capabilities (0017 Q14, A2A-DESIGN §10.2): the caps
// list of /register and the ADP card signed with it, like the A2A network
// card, name only entries of inbound.public_capabilities. A capability
// this node serves to its allowed or trusted peers only is theirs to know
// about; listing it in a public directory would advertise a private
// surface and invite calls that the inbound policy refuses anyway.
//
// The operator's declared list (config caps, `hub-register --caps`) is kept
// as it is in the config; it is filtered where it is published.

import "strings"

// publicOnly returns the members of caps that are public capabilities,
// in order.
func (d *Daemon) publicOnly(caps []string) []string {
	public := map[string]bool{}
	for _, p := range d.config().inbound().PublicCapabilities {
		if id := strings.TrimSpace(p.ID); id != "" {
			public[id] = true
		}
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if public[strings.TrimSpace(c)] {
			out = append(out, c)
		}
	}
	return out
}

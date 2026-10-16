package daemon

// card_limits.go is how a public capability's limits and file input show on
// the card (CUX-F30, CUX-F31): the skill's input modes, and a fixed English
// sentence at the end of its description. Only the numbers a caller meets
// are in it; the global limits and max_inflight are not (review §12.4).

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/provider"
)

// publicCapabilityByID is the effective public capability id, or false.
func (d *Daemon) publicCapabilityByID(id string) (PublicCapability, bool) {
	for _, p := range d.config().inbound().PublicCapabilities {
		if p.ID == id {
			return p.limits(), true
		}
	}
	return PublicCapability{}, false
}

// publicSkillFor is the skill a public capability is published as: what its
// provider declares, with the file input modes and the limits sentence.
func (d *Daemon) publicSkillFor(p provider.CapabilityProvider, id string) netcard.Skill {
	sk := netcard.SkillFor(p, id)
	pc, ok := d.publicCapabilityByID(id)
	if !ok {
		return sk
	}
	sk.Limits = capLimitsSentence(pc)
	if pc.Files != nil && pc.Files.declared() {
		sk.InputModes = append([]string{"application/json"}, pc.Files.Types...)
		sk.OutputModes = []string{"application/json"}
	}
	return sk
}

// capLimitsSentence is CUX-F31's text for a capability whose limits have
// their defaults filled in.
func capLimitsSentence(pc PublicCapability) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Limits per caller: %d/min, %d/day, %d at a time.", pc.PerCallerPerMin, pc.PerCallerPerDay, pc.PerCallerInflight)
	if f := pc.Files; f != nil && f.declared() {
		noun := "files"
		if f.Count == 1 {
			noun = "file"
		}
		fmt.Fprintf(&b, " Input: %d %s of %s, each up to %s MiB.", f.Count, noun, strings.Join(f.Types, ", "), mibText(f.MaxBytes))
	}
	if pc.Discard {
		b.WriteString(" Inputs are not stored.")
	}
	return b.String()
}

// mibText is a size in MiB for the limits sentence: whole when it is, else
// one decimal, rounded down so the card never promises more than is taken.
func mibText(n int64) string {
	const mib = 1 << 20
	if n%mib == 0 {
		return strconv.FormatInt(n/mib, 10)
	}
	tenths := max(n*10/mib, 1)
	return fmt.Sprintf("%d.%d", tenths/10, tenths%10)
}

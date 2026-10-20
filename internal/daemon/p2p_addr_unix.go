//go:build !windows

package daemon

import "github.com/ANetResearch/ANet/internal/a2ashape"

// p2pPlatformRefusal refuses modules.p2p on a platform where it cannot work; nil on unix.
func p2pPlatformRefusal() *a2ashape.Refusal { return nil }

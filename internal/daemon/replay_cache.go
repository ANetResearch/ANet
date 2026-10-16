package daemon

import (
	"sync"

	"github.com/ANetResearch/ANetCore/identity"
)

// replayCacheBytes bounds the process's identity.ReplayCache. An envelope from a stranger makes the
// receive pipeline replay the sender's whole KEL about ten times (steps 6 to 9); with the cache a KEL
// is replayed once and answered from memory after that, so a sender cannot turn each envelope into
// milliseconds of CPU by naming a long KEL (T-1 RT-F3). A KEL costs some hundreds of bytes in it, 40 KiB
// at the cap.
const replayCacheBytes = 64 << 20

var installReplayCache sync.Once

// useReplayCache installs the process's identity.ReplayCache, once; one a host installed already is
// kept.
func useReplayCache() {
	installReplayCache.Do(func() {
		if prev := identity.SetReplayCache(identity.NewReplayCache(replayCacheBytes)); prev != nil {
			identity.SetReplayCache(prev)
		}
	})
}

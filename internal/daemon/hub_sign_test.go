package daemon

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"
)

func staleBody(floor int64) []byte {
	return []byte(`{"reason":"ts_stale","error":"stale","floor":` + strconv.FormatInt(floor, 10) + `}`)
}

// PAY-R10: every request the node signs for a hub takes its time from one
// clock per hub, so concurrent requests never share a time.
func TestSignedRequestsNeverShareATime(t *testing.T) {
	_, d, _ := registeredPair(t)
	const n = 200
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "http://hub.invalid/agents/x/balance", nil)
			d.signHubRequest(req, "bafy-hub", relayauth.ActionBalance, nil)
			ts := req.Header.Get("X-ANet-TS")
			mu.Lock()
			if seen[ts] {
				t.Errorf("time %s signed twice", ts)
			}
			seen[ts] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Another hub's clock is its own (PAY-R12): a floor from one hub moves nothing for another.
	now := time.Now().UnixMilli()
	if !d.hubClock.Raise("bafy-hub", now+30_000, now) {
		t.Fatal("a floor inside the skew was refused")
	}
	if got := d.hubClock.Next("bafy-other", now); got != now {
		t.Errorf("another hub's time moved to %d", got)
	}
}

// PAY-R11: ts_stale is answered by signing again once, above the floor; a
// floor beyond what other hubs accept is not adopted. Both are counted, and
// the retries of the last hour are what doctor reads.
func TestAStaleAnswerRaisesTheClockOrIsRefused(t *testing.T) {
	_, d, _ := registeredPair(t)
	now := time.Now().UnixMilli()
	retry, rejected := d.tsStale("bafy-hub", hubReply{Status: 401, Body: staleBody(now + 20_000)})
	if !retry || rejected {
		t.Fatalf("a floor in range: retry %v rejected %v", retry, rejected)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://hub.invalid/agents/x/balance", nil)
	d.signHubRequest(req, "bafy-hub", relayauth.ActionBalance, nil)
	if ts, _ := strconv.ParseInt(req.Header.Get("X-ANet-TS"), 10, 64); ts <= now+20_000 {
		t.Errorf("signed at %d, not above the floor %d", ts, now+20_000)
	}
	retry, rejected = d.tsStale("bafy-hub", hubReply{Status: 401, Body: staleBody(now + 3_600_000)})
	if retry || !rejected {
		t.Fatalf("a floor beyond the skew: retry %v rejected %v", retry, rejected)
	}
	// Other answers are not this.
	for _, rep := range []hubReply{
		{Status: 401, Body: []byte(`{"reason":"replayed"}`)},
		{Status: 403, Body: staleBody(now)},
		{Status: 200, Body: []byte(`{}`)},
	} {
		if r, j := d.tsStale("bafy-hub", rep); r || j {
			t.Errorf("%d %s was taken for ts_stale", rep.Status, rep.Body)
		}
	}
	if got := d.TSRetries(); got != 2 {
		t.Errorf("retries in the last hour = %d, want 2", got)
	}
	if d.rxStats.m[counterTSStaleRetry] != 2 || d.rxStats.m[counterTSFloorRejected] != 1 {
		t.Errorf("counters: %v", d.rxStats.m)
	}
}

func TestRetriesOlderThanAnHourAreForgotten(t *testing.T) {
	var l tsRetryLog
	now := time.Now().UnixMilli()
	l.note(now - 2*3600_000)
	l.note(now - 1000)
	if got := l.recent(now); got != 1 {
		t.Errorf("recent = %d, want 1", got)
	}
}

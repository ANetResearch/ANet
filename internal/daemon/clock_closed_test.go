package daemon

import "testing"

// A hub that keeps answering while its Date is far from the local clock
// keeps the gate closed: privacy.clock_closed_s grows with the time that
// has passed, however often the hub answers (FS §5.4; T-1 F1). Every
// answer used to start the count over.
func TestClockClosedForCountsAcrossAnswersThatStaySkewed(t *testing.T) {
	d := newTestDaemon(t, silentHub, true)
	start := epochOf(d.nowMS()) + 2*hourMS
	clocks := newGateClocks(t, d, start)
	skewed := start - 20*minuteMS
	for i := 0; i < 12; i++ { // an answer every minute, 12 minutes
		hubSays(d, skewed+uint64(i)*minuteMS)
		clocks.run(minuteMS)
	}
	hubSays(d, skewed+12*minuteMS)
	body := d.privacy().body
	got, _ := body["clock_closed_s"].(uint64)
	if got < 11*60 {
		t.Fatalf("clock_closed_s = %v after 12 minutes of skewed answers, want at least 660 (body %v)", body["clock_closed_s"], body)
	}
	if body["clock"] != clockSkewed {
		t.Fatalf("clock = %v, want skewed", body["clock"])
	}
	// One answer that agrees opens the gate and the count starts over.
	hubSays(d, clocks.wall.Load())
	if _, ok := d.privacy().body["clock_closed_s"]; ok {
		t.Fatal("clock_closed_s still reported after an answer that agrees")
	}
}

package autoreply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStripDoneSentinel(t *testing.T) {
	cases := []struct {
		in        string
		wantClean string
		wantDone  bool
	}{
		{"just a reply", "just a reply", false},
		{"delivered\n" + autoReplyDoneSentinel, "delivered", true},
		{autoReplyDoneSentinel, "", true},
		{"a " + autoReplyDoneSentinel + " b", "a  b", true},
	}
	for _, c := range cases {
		clean, done := stripDoneSentinel(c.in)
		if clean != c.wantClean || done != c.wantDone {
			t.Fatalf("stripDoneSentinel(%q) = (%q,%v), want (%q,%v)", c.in, clean, done, c.wantClean, c.wantDone)
		}
	}
}

// outboxReplier is a test backend that drops a file in rc.Outbox (like the exec agent delivering an
// artifact) and returns its reply text — exercising the daemon's outbox → attachment path.
type outboxReplier struct {
	text string
	file string
	data []byte
}

func (r *outboxReplier) Reply(_ context.Context, rc replyContext, _ []chatTurn) (string, error) {
	if rc.Outbox != "" && r.file != "" {
		_ = os.WriteFile(filepath.Join(rc.Outbox, r.file), r.data, 0o600)
	}
	return r.text, nil
}

// execStub installs a stub agent that answers with a fixed line and counts
// its runs in a file.
func execStub(t *testing.T) (runs func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "runs")
	stub := filepath.Join(dir, "stub.sh")
	script := "#!/bin/sh\necho run >> " + count + "\necho EXEC-OK\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setExecCommand(t, stub)
	return func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "run")
	}
}

// sleepyReplier answers after a while, as an exec agent doing real work
// does.
type sleepyReplier struct{ after time.Duration }

func (r sleepyReplier) Reply(ctx context.Context, _ replyContext, _ []chatTurn) (string, error) {
	select {
	case <-time.After(r.after):
		return "done, after a while", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

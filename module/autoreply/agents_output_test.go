package autoreply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// An agent that prints without end (a prompt injection is enough to make one) is killed once it has printed
// more than the bound, in good time and with the daemon holding no more than the bound (T-1 EXE F2).
func TestAgentStdoutFloodIsCutAndKilled(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		stream       string
		max          int
	}{
		{"stdout", `yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA`, "stdout", agentStdoutMax},
		{"stderr", `yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA >&2`, "stderr", agentStderrMax},
		{"children keep the pipe", `(yes BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB & yes CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC &) ; sleep 60`, "stdout", agentStdoutMax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := time.Now()
			out, errb, err := runCmd(ctx, execInvokeOpts{WorkDir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}, nil, "/bin/sh", "-c", tc.script)
			if !errors.Is(err, errAgentOutputTooLarge) {
				t.Fatalf("err = %v, want errAgentOutputTooLarge", err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("the run ended by its timeout, not by the bound: %v", err)
			}
			if d := time.Since(start); d > 15*time.Second {
				t.Errorf("took %v to be killed", d)
			}
			if len(out) > agentStdoutMax || len(errb) > agentStderrMax {
				t.Errorf("kept %d bytes of stdout and %d of stderr; bounds %d and %d", len(out), len(errb), agentStdoutMax, agentStderrMax)
			}
			got := len(out)
			if tc.stream == "stderr" {
				got = len(errb)
			}
			if got != tc.max {
				t.Errorf("kept %d bytes of %s, want exactly the bound %d", got, tc.stream, tc.max)
			}
		})
	}
}

// Output under the bound is kept whole, and a timeout is still a timeout.
func TestAgentOutputUnderTheBoundIsKept(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, errb, err := runCmd(ctx, execInvokeOpts{WorkDir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}, nil, "/bin/sh", "-c",
		`head -c 1000000 /dev/zero | tr '\0' x; echo oops >&2`)
	if err != nil || len(out) != 1000000 || strings.TrimSpace(string(errb)) != "oops" {
		t.Fatalf("out %d err %v stderr %q", len(out), err, errb)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	_, _, err = runCmd(ctx2, execInvokeOpts{WorkDir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}, nil, "/bin/sh", "-c", "sleep 30")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errAgentOutputTooLarge) {
		t.Fatalf("timeout: %v", err)
	}
}

// What an agent prints reaches the log as one line without control characters, cut on a rune boundary
// (T-1 EXE F5), and an output-bound or sandbox refusal passes through cmdError unchanged.
func TestAgentOutputInTheLogIsOneLine(t *testing.T) {
	forged := "boom\n2026/10/16 17:59:59 anet: hub: key rotated, accepted new key AAAA\r\x1b[2J\x1b[H\u0085"
	err := cmdError("/usr/bin/claude", nil, nil, []byte(forged), errors.New("exit status 1"))
	if s := err.Error(); strings.ContainsAny(s, "\n\r\x1b\u0085") || !strings.HasPrefix(s, "claude: boom ") {
		t.Errorf("log text %q", s)
	}
	long := strings.Repeat("好", 300) // 900 bytes
	got := logText(long, 400)
	if !utf8.ValidString(got) || len(got) > 400+len("…") {
		t.Errorf("cut text: valid %v, %d bytes", utf8.ValidString(got), len(got))
	}
	if !errors.Is(cmdError("x", nil, nil, nil, errAgentOutputTooLarge), errAgentOutputTooLarge) {
		t.Error("the output refusal was reworded")
	}
}

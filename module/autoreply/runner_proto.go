package autoreply

// runner_proto.go — the protocol between the node and `anet runner serve` (design v0.3.9 §C), one job per
// connection on the runner's Unix socket:
//
//	node   → runner   one line, the job:  {"agent","model","instructions","conversation",
//	                                       "files":[{"name","mime","data"}],"env":{…},"timeout_s","peer"}
//	runner → node     progress lines:     {"progress":"queued"} {"progress":"started"}
//	                  one final line:     {"exit":"ok","reply":…,"files":[…]}
//	                                      {"exit":"error"|"timeout"|"output_too_large"|"refused","code":…,"detail":…}
//
// Every line is one JSON object ended by "\n" and has a length limit (runnerJobMax, runnerLineMax).
// Closing the connection cancels the job: the runner ends the agent's process group. The node writes
// nothing after the job, so anything it does write is read as a cancellation too.
//
// What the job carries keeps LEC AV-1 on the runner's side: the conversation is what the peer wrote and
// goes on the agent's stdin or into a 0600 file, never on a command line; instructions and the model are
// the node operator's. env carries only ANET_TURN_* and ANET_MODEL_* variables (runnerEnvName), and the
// runner adds nothing else of the job's to the agent's environment. detail is for the node's local log:
// the agent's stderr cut to one line, as a direct run's (cmdError); it never reaches the peer.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/internal/turntools"
)

// runnerJob is the job line.
type runnerJob struct {
	Agent        string            `json:"agent"`
	Model        string            `json:"model,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Conversation string            `json:"conversation"`
	Files        []runnerFile      `json:"files,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	TimeoutS     int               `json:"timeout_s,omitempty"`
	// Peer names the per-peer work directory: the last 16 characters of the peer's AID
	// (execwork.AidShort).
	Peer string `json:"peer"`
}

// runnerFile is one file of a job (an image of the conversation) or of a result (the outbox).
type runnerFile struct {
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Data []byte `json:"data"`
}

// runnerLine is a line the runner sends: a progress line (Progress set) or the final one (Exit set).
type runnerLine struct {
	Progress string       `json:"progress,omitempty"`
	Exit     string       `json:"exit,omitempty"`
	Reply    string       `json:"reply,omitempty"`
	Files    []runnerFile `json:"files,omitempty"`
	// Code names a refusal of the job (exit "refused"): runner_job_invalid or runner_agent_not_allowed.
	Code string `json:"code,omitempty"`
	// Detail is one line for the node's local log.
	Detail string `json:"detail,omitempty"`
}

// The exits of a job.
const (
	runnerExitOK       = "ok"
	runnerExitError    = "error"
	runnerExitTimeout  = "timeout"
	runnerExitTooLarge = "output_too_large"
	runnerExitRefused  = "refused"
)

// Limits of the protocol. A job's text is bounded as the exec backend bounds it (AV-9: 256 KiB of
// conversation, which the frame around it adds to), its files as the images a turn can carry; a result's
// files as the outbox is (maxOutboxFiles, maxOutboxBytes) and its reply as an agent's stdout.
const (
	runnerMaxInstructions = 256 << 10
	runnerMaxConversation = 512 << 10
	runnerMaxFiles        = 16
	runnerMaxFileBytes    = 32 << 20 // a job's files together
	runnerMaxEnv          = 16
	runnerMaxEnvValue     = 4 << 10
	runnerMaxModel        = 256
	// runnerJobMax bounds the job line: the text, the files in base64 and the frame.
	runnerJobMax = runnerMaxInstructions + runnerMaxConversation + runnerMaxFileBytes/3*4 + 1<<20
	// runnerLineMax bounds a line the runner sends: the outbox in base64, the reply and the frame.
	runnerLineMax = maxOutboxBytes/3*4 + agentStdoutMax + 1<<20
	// runnerMaxTimeout bounds a job's timeout_s.
	runnerMaxTimeout = 6 * time.Hour
)

// runnerEnvNameRe is a variable a job may set: ANET_TURN_* or ANET_MODEL_*.
var runnerEnvNameRe = regexp.MustCompile(`^ANET_(TURN|MODEL)_[A-Z0-9_]{1,64}$`)

// runnerEnvName reports a variable name a job may carry.
func runnerEnvName(k string) bool {
	return runnerEnvNameRe.MatchString(k) && (strings.HasPrefix(k, turntools.EnvTurnPrefix) || strings.HasPrefix(k, turntools.EnvModelPrefix))
}

// runnerEnvValue reports a value a job may carry: bounded, one line, no NUL.
func runnerEnvValue(v string) bool {
	return len(v) <= runnerMaxEnvValue && !strings.ContainsAny(v, "\x00\r\n")
}

// errRunnerJob is a job the runner does not take (runner_job_invalid).
var errRunnerJob = errors.New("runner_job_invalid")

// check is errRunnerJob, with what is wrong, for a job outside the protocol's limits. It does not judge
// the agent, which is the runner's own decision (runner_agent_not_allowed).
func (j *runnerJob) check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", errRunnerJob, fmt.Sprintf(format, a...))
	}
	switch {
	case j.Agent == "":
		return bad("no agent")
	case len(j.Model) > runnerMaxModel || strings.ContainsAny(j.Model, "\x00\r\n"):
		return bad("model")
	case len(j.Instructions) > runnerMaxInstructions:
		return bad("instructions over %d bytes", runnerMaxInstructions)
	case len(j.Conversation) > runnerMaxConversation:
		return bad("conversation over %d bytes", runnerMaxConversation)
	case len(j.Files) > runnerMaxFiles:
		return bad("%d files, over %d", len(j.Files), runnerMaxFiles)
	case len(j.Env) > runnerMaxEnv:
		return bad("%d variables, over %d", len(j.Env), runnerMaxEnv)
	case j.TimeoutS < 0 || time.Duration(j.TimeoutS)*time.Second > runnerMaxTimeout:
		return bad("timeout_s")
	case !runnerPeerName(j.Peer):
		return bad("peer")
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range j.Files {
		if !runnerFileName(f.Name) || seen[f.Name] {
			return bad("file name")
		}
		seen[f.Name] = true
		total += len(f.Data)
	}
	if total > runnerMaxFileBytes {
		return bad("files over %d MiB", runnerMaxFileBytes>>20)
	}
	for k, v := range j.Env {
		if !runnerEnvName(k) || !runnerEnvValue(v) {
			return bad("variable %s", logText(k, 80))
		}
	}
	return nil
}

// runnerPeerName is a per-peer directory name: what execwork.AidShort makes of an AID (letters and
// digits, then also - and _).
func runnerPeerName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || (r != '_' && r != '-')) {
			return false
		}
	}
	return true
}

// runnerFileName is a file name that names a file directly in a directory, as SafeName leaves it.
func runnerFileName(s string) bool {
	return s != "" && pathsafe.SafeName(s) == s
}

// writeRunnerLine writes v as one line.
func writeRunnerLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// errRunnerLineTooLong is a line over its limit.
var errRunnerLineTooLong = errors.New("runner: a line is over its length limit")

// readRunnerLine reads one line of at most max bytes (without its "\n") and decodes it into v, refusing
// members v does not have.
func readRunnerLine(br *bufio.Reader, max int, v any) error {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max+1 {
			return errRunnerLineTooLong
		}
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == io.EOF && len(line) == 0 {
			return io.EOF
		}
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSuffix(line, []byte("\n"))))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeNewPrivate writes data to a new file (0600) that must not exist and is not followed if it is a link.
func writeNewPrivate(path string, data []byte) error {
	f, err := fsperm.OpenNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

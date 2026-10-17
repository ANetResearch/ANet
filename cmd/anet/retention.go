package main

// retention.go is `anet retention [--json]` and `anet retention set <N>d|forever` (forward-secrecy
// §5.2, ux §4.2): how long this node keeps the content of a task once it has ended, and the one way to
// change it.
//
// A change is made only from the operator's terminal (tty_required otherwise, with the command to run
// there): a shorter value deletes content for good, and a longer one keeps more of it, so neither is an
// agent's to decide. When the change deletes tasks at once, the terminal is told how many and has to
// answer yes (retention.confirm). With the daemon running, the change goes through it (POST
// /retention, which sweeps at once and records anet.retention.changed); with the daemon stopped, the
// count reads the store read-only and the CLI writes config.json itself, which also repairs a value
// the daemon refused to start with. The daemon then takes the value when it starts.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// retentionUsage is the command's usage line (ux UX-C5: not a reason sentence).
const retentionUsage = "anet retention set <N>d|forever"

// The command's output; tests replace them.
var (
	retentionStdout io.Writer = os.Stdout
	retentionStderr io.Writer = os.Stderr
)

// retentionDaemon is the control plane of this data directory's own daemon, or nil when it is not
// running; tests replace it.
var retentionDaemon = func(layout daemon.Layout) *client {
	up, _ := probeLocalDaemon(layout)
	if !up {
		return nil
	}
	tb, err := os.ReadFile(layout.ControlTokenPath())
	if err != nil {
		return nil
	}
	return &client{base: "http://" + daemon.LocalControlAddr(layout), token: strings.TrimSpace(string(tb)),
		timeout: 2 * time.Minute, dataDir: layout.Root}
}

// retentionNow is the clock the count of a stopped daemon's store is made at; tests replace it.
var retentionNow = func() int64 { return time.Now().UnixMilli() }

func runRetention(layout daemon.Layout, rest []string) error {
	pos, flags := splitFlags(rest)
	switch {
	case len(pos) == 0:
		return showRetention(layout, flags["json"] == "true")
	case len(pos) == 2 && pos[0] == "set" && flags["json"] == "":
		return setRetention(layout, pos[1])
	}
	return usageError{retentionUsage}
}

// showRetention prints the setting in force: the running daemon's, or config.json's.
func showRetention(layout daemon.Layout, asJSON bool) error {
	lang := a2ashape.Lang()
	r, ref, err := currentRetention(layout)
	if err != nil {
		return err
	}
	if ref != nil {
		sayRefusal(retentionStderr, ref.Key, lang, ref.Args)
		return errExit1
	}
	if asJSON {
		b, _ := json.Marshal(map[string]string{"content_retention": r.String(), "public_retention": r.Public().String()})
		fmt.Fprintln(retentionStdout, string(b))
		return nil
	}
	fmt.Fprintln(retentionStdout, a2ashape.Sentence("retention.summary", lang, a2ashape.Args{"retention": r.String()}))
	fmt.Fprintln(retentionStdout, "usage: "+retentionUsage)
	return nil
}

// currentRetention is the setting in force: /status.privacy of the running daemon, else what
// config.json says (30 days without the key). A config.json value that is not a setting is
// retention_invalid.
func currentRetention(layout daemon.Layout) (interactions.Retention, *a2ashape.Refusal, error) {
	if c := retentionDaemon(layout); c != nil {
		b, code, err := c.fetch("/status", nil)
		if err == nil && code == 200 {
			var st struct {
				Privacy struct {
					ContentRetention string `json:"content_retention"`
				} `json:"privacy"`
			}
			if json.Unmarshal(b, &st) == nil {
				if r, ok := daemon.ParseContentRetention(st.Privacy.ContentRetention); ok {
					return r, nil, nil
				}
			}
		}
	}
	r, _, ref, err := daemon.ContentRetentionInFile(layout)
	return r, ref, err
}

// setRetention is `anet retention set <value>` (FS §5.2): the value is checked, a terminal is
// required, the tasks it would delete at once are counted and, if there are any, confirmed on the
// terminal; then the value is put in force.
func setRetention(layout daemon.Layout, value string) error {
	lang := a2ashape.Lang()
	r, ok := daemon.ParseContentRetention(value)
	if !ok {
		sayRefusal(retentionStderr, "retention_invalid", lang, nil)
		return errExit1
	}
	tty, err := openTTY()
	if err != nil {
		args := a2ashape.Args{"action": "retention", "command": typedCommand(commandLine())}
		sayRefusal(retentionStderr, "tty_required", lang, args)
		return errExit1
	}
	defer tty.Close()
	c := retentionDaemon(layout)
	// A daemon that holds the data directory but does not answer would write the old value back over
	// a file changed under it: nothing is touched (KEL-10, daemon_not_answering).
	if c == nil && daemon.DataLockHeld(layout) {
		args := a2ashape.Args{"path": layout.Root, "dur": probeTimeout}
		sayRefusal(retentionStderr, "daemon_not_answering", lang, args)
		return errExit1
	}
	var k int64
	if c != nil {
		var ans struct {
			WouldDelete int64 `json:"would_delete"`
		}
		if err := retentionCall(c, map[string]any{"value": r.String(), "dry_run": true}, &ans); err != nil {
			return err
		}
		k = ans.WouldDelete
	} else {
		cur, _, _, err := daemon.ContentRetentionInFile(layout)
		if err != nil {
			return err
		}
		// A value the daemon refused counts as keeping everything it can: every task the new one
		// deletes is counted.
		if k, err = interactions.CountNewlyDeletableIn(layout.InteractionsDir(), retentionNow(), cur, r); err != nil {
			return err
		}
	}
	if k > 0 {
		prompt := a2ashape.Sentence("retention.confirm", lang, a2ashape.Args{"retention": r.String(), "n": k})
		if !answeredYes(tty, prompt) {
			say(retentionStderr, a2ashape.Sentence("confirm.declined", lang, nil), "")
			return errExit1
		}
	}
	if c != nil {
		var ans struct {
			Message string `json:"message"`
		}
		if err := retentionCall(c, map[string]any{"value": r.String()}, &ans); err != nil {
			return err
		}
		if ans.Message == "" {
			ans.Message = a2ashape.Sentence("retention.set", lang, a2ashape.Args{"retention": r.String()})
		}
		fmt.Fprintln(retentionStdout, ans.Message)
		return nil
	}
	if err := daemon.SetContentRetentionInFile(layout, r); err != nil {
		return err
	}
	fmt.Fprintln(retentionStdout, a2ashape.Sentence("retention.set", lang, a2ashape.Args{"retention": r.String()}))
	return nil
}

// answeredYes shows prompt on the terminal and reports whether the line typed back is "yes". The
// prompt is a whole sentence that ends asking for it (retention.confirm), so nothing is added but the
// space an ASCII colon is followed by.
func answeredYes(tty io.ReadWriter, prompt string) bool {
	if strings.HasSuffix(prompt, ":") {
		prompt += " "
	}
	if _, err := fmt.Fprint(tty, prompt); err != nil {
		return false
	}
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.TrimSpace(strings.ToLower(line)) == "yes"
}

// retentionCall posts body to the daemon's /retention and decodes a 200 answer into out. A refusal is
// said with its sentence and next step (exit 1).
func retentionCall(c *client, body map[string]any, out any) error {
	b, code, err := c.fetch("/retention", body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		var ref a2ashape.ErrorBody
		if json.Unmarshal(b, &ref) == nil && ref.Error != "" {
			say(retentionStderr, ref.Error, ref.Next)
			return errExit1
		}
		return fmt.Errorf("retention: %s", strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("retention: the daemon's answer does not read: " + err.Error())
	}
	return nil
}

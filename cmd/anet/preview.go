package main

// preview.go is `anet preview [off|on|with_reasoning] [--json]` (correctness-ux §4.5, ux UX-C12): whether
// this node, as a provider, sends the requester a snapshot of its reply while it writes it.
//
// Without an argument it says what the setting is and what that means (preview.off, preview.on or
// preview.with_reasoning). With one it puts the setting in force through the daemon (POST /preview),
// which saves config.json first, then applies it and records the change. with_reasoning also sends the
// reasoning to a requester that asks for it, so it is confirmed on the operator's terminal (tty_required
// without one, and the setting is left as it is); off and on need no confirmation.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// previewUsage is the command's usage line (ux UX-C5: not a reason sentence).
const previewUsage = "anet preview [off|on|with_reasoning] [--json]"

func runPreview(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 1 || (flags["json"] != "" && flags["json"] != "true") {
		return usageError{previewUsage}
	}
	lang := a2ashape.Lang()
	body := map[string]any{}
	if len(pos) == 1 {
		mode := strings.TrimSpace(pos[0])
		switch mode {
		case "off", "on":
		case "with_reasoning":
			err := confirmOnTTY("reasoning_preview", a2ashape.Sentence("preview.confirm_reasoning", lang, nil))
			if err == errNoTTY || err == errDeclined {
				return errExit1 // said already (tty.go)
			}
			if err != nil {
				return err
			}
		default:
			return usageError{previewUsage}
		}
		body["mode"] = mode
	}
	out, code, err := c.fetch("/preview", body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return answerError(code, out)
	}
	var ans struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(out, &ans) != nil || ans.Mode == "" {
		return fmt.Errorf("preview: the daemon's answer does not read")
	}
	if flags["json"] == "true" {
		fmt.Fprintln(outW(), string(out))
		return nil
	}
	fmt.Fprintln(outW(), a2ashape.Sentence("preview."+ans.Mode, lang, nil))
	return nil
}

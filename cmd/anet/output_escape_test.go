package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A peer's words reach stdout in the JSON of a task; the C1 controls and the
// bidi controls must not leave this process raw (T-1 F1).
func TestPrintAnswerEscapesTerminalRunes(t *testing.T) {
	text := "a\x1b[2J\u009b31m‮EVIL⁦x‏ "
	raw, _ := json.Marshal(map[string]any{"task_id": "ix_1", "state": "completed", "text": text})
	var buf bytes.Buffer
	old := cliStdout
	cliStdout = &buf
	defer func() { cliStdout = old }()
	if err := printAnswer(raw, 200, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\u009b", "‮", "⁦", "‏", " ", "\x1b"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("raw %U on stdout", []rune(bad)[0])
		}
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil || back["text"] != text {
		t.Errorf("the escaped JSON no longer reads back the same: %v", err)
	}
}

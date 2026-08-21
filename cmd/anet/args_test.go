package main

import (
	"reflect"
	"strings"
	"testing"
)

// Argument parsing is the whole user-facing surface, and it had no tests.
//
// The defect this file was written for: --attach=PATH worked and
// --capability=ID did not, because two helpers on the same command line
// disagreed about whether "=" was a spelling. `anet delegate AID
// --capability=cas.put` produced a flag literally named
// "capability=cas.put", lost the capability, and failed complaining about
// an empty goal — after teaching the user that spelling one flag earlier.
func TestFlagsAcceptBothSpellings(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		pos  []string
		want map[string]string
	}{
		{
			name: "separated",
			argv: []string{"aid-1", "--capability", "cas.put", "--args", `{"body":"aGk="}`},
			pos:  []string{"aid-1"},
			want: map[string]string{"capability": "cas.put", "args": `{"body":"aGk="}`},
		},
		{
			name: "joined by equals",
			argv: []string{"aid-1", "--capability=cas.put", "--args={}"},
			pos:  []string{"aid-1"},
			want: map[string]string{"capability": "cas.put", "args": "{}"},
		},
		{
			name: "mixed on one line",
			argv: []string{"aid-1", "--capability=cas.get", "--args", `{"cid":"bafy"}`},
			pos:  []string{"aid-1"},
			want: map[string]string{"capability": "cas.get", "args": `{"cid":"bafy"}`},
		},
		{
			name: "a bare flag is a boolean",
			argv: []string{"--all", "--pending"},
			want: map[string]string{"all": "true", "pending": "true"},
		},
		{
			name: "a bare flag before another flag stays boolean",
			argv: []string{"--all", "--name", "alice"},
			want: map[string]string{"all": "true", "name": "alice"},
		},
		{
			// A JSON value holds "=" and spaces; only the first "=" splits.
			name: "equals inside the value",
			argv: []string{`--args={"q":"a=b"}`},
			want: map[string]string{"args": `{"q":"a=b"}`},
		},
		{
			// An explicitly empty value is empty, not "true".
			name: "an empty value is empty",
			argv: []string{"--summary="},
			want: map[string]string{"summary": ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, flags := splitFlags(tc.argv)
			if !reflect.DeepEqual(flags, tc.want) {
				t.Errorf("flags = %v, want %v", flags, tc.want)
			}
			if len(pos) != len(tc.pos) || (len(pos) > 0 && !reflect.DeepEqual(pos, tc.pos)) {
				t.Errorf("positional = %v, want %v", pos, tc.pos)
			}
		})
	}
}

// The two helpers run over the same argv, one after the other. They have
// to agree about what a flag looks like, or a flag one of them consumes is
// a positional argument to the other.
func TestAttachAndFlagsAgreeOnOneCommandLine(t *testing.T) {
	argv := []string{"aid-1", "--attach", "/tmp/a.png", "look at this", "--attach=/tmp/b.png"}
	paths, rest := extractAttach(argv)
	if len(paths) != 2 {
		t.Fatalf("attachments = %v, want two", paths)
	}
	if !strings.HasSuffix(paths[0], "/tmp/a.png") || !strings.HasSuffix(paths[1], "/tmp/b.png") {
		t.Errorf("attachment paths = %v", paths)
	}
	pos, flags := splitFlags(rest)
	if len(flags) != 0 {
		t.Errorf("attachments must not survive as flags: %v", flags)
	}
	if got := strings.Join(pos[1:], " "); got != "look at this" {
		t.Errorf("goal = %q, want %q", got, "look at this")
	}
}

// A capability call is addressed by id and carries no goal. This is the
// shape the control API rejected for a whole joint run, because the
// capability was going into the goal text where no resolver looks.
func TestACapabilityCallNeedsNoGoal(t *testing.T) {
	for _, argv := range [][]string{
		{"aid-1", "--capability", "ptz.absolute@onvif/camera-006", "--args", `{"pan":0.4}`},
		{"aid-1", "--capability=ptz.absolute@onvif/camera-006", "--args", `{"pan":0.4}`},
	} {
		pos, flags := splitFlags(argv)
		if flags["capability"] != "ptz.absolute@onvif/camera-006" {
			t.Errorf("%v: capability = %q", argv, flags["capability"])
		}
		if len(pos) != 1 || pos[0] != "aid-1" {
			t.Errorf("%v: the provider AID must be the only positional, got %v", argv, pos)
		}
		if goal := strings.Join(pos[1:], " "); goal != "" {
			t.Errorf("%v: a capability call must leave no goal text, got %q", argv, goal)
		}
	}
}

// --id selects which local identity's daemon the command talks to. Picking
// the wrong one means talking to a daemon that is not yours, which is the
// failure ResolveControlStrict exists to prevent.
func TestGlobalIDIsExtractedFromAnywhere(t *testing.T) {
	for _, argv := range [][]string{
		{"--id", "work", "status"},
		{"status", "--id", "work"},
		{"--id=work", "status"},
	} {
		id, rest := extractGlobalID(argv)
		if id != "work" {
			t.Errorf("%v: id = %q, want work", argv, id)
		}
		if len(rest) != 1 || rest[0] != "status" {
			t.Errorf("%v: rest = %v, want [status]", argv, rest)
		}
	}
	if id, rest := extractGlobalID([]string{"status"}); id != "" || len(rest) != 1 {
		t.Errorf("no --id must leave the line alone: %q %v", id, rest)
	}
}

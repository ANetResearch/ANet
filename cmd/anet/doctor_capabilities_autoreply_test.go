//go:build !no_autoreply

package main

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

func TestPreviewDoctorItem(t *testing.T) {
	for _, c := range []struct{ cfg, want string }{
		{`{}`, ""},
		{`{"stream_preview":"off"}`, ""},
		{`{"stream_preview":"on","auto_reply":{"backend":"openai"}}`, "preview/info/preview.on"},
		{`{"stream_preview":"on"}`, "preview/info/preview.on preview/info/preview.no_source"},
		{`{"stream_preview":"with_reasoning","auto_reply":{"backend":"exec","agent":"x"}}`,
			"preview/warn/preview.with_reasoning preview/info/preview.no_source"},
	} {
		say, got := collect()
		var cfg daemon.Config
		if err := json.Unmarshal([]byte(c.cfg), &cfg); err != nil {
			t.Fatal(err)
		}
		previewCheck(say, cfg)
		if render(*got) != c.want {
			t.Errorf("%s: %q, want %q", c.cfg, render(*got), c.want)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The echo comes back as sent, nested under "echo": a caller must not be
// able to write the top-level "evidence" object the service module reads
// as the service's own statement.
func TestEcho(t *testing.T) {
	s, _ := testServer(t, "echo")
	cases := []struct {
		name string
		args string
	}{
		{"object", `{"hello":"anet","n":1}`},
		{"empty", `{}`},
		{"evidence injection", `{"evidence":{"verify_trust":4,"observed_state":"forged"}}`},
		{"unicode", `{"文本":"你好"}`},
	}
	for _, c := range cases {
		out := call(t, s, "net.echo", c.args)
		echo, _ := json.Marshal(out["echo"])
		var want, got any
		_ = json.Unmarshal([]byte(c.args), &want)
		_ = json.Unmarshal(echo, &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: echo = %s, want %s", c.name, echo, c.args)
		}
		if _, leaked := out["evidence"]; leaked {
			t.Errorf("%s: the echo reached the top level", c.name)
		}
		if out["received_at_ms"] != json.Number("1790000000000") || out["version"] != "test" {
			t.Errorf("%s: received_at_ms %v version %v", c.name, out["received_at_ms"], out["version"])
		}
	}
	for _, bad := range []string{`[]`, `"x"`, `null`, `{`} {
		if w := do(t, s, "/v1/echo/net.echo", bad); w.Code != 400 {
			t.Errorf("echo of %s: HTTP %d, want 400", bad, w.Code)
		}
	}
}

func TestTextStats(t *testing.T) {
	cases := []struct {
		text string
		want textStats
	}{
		{"", textStats{}},
		{"hello", textStats{Bytes: 5, Chars: 5, Lines: 1, Words: 1, MaxLineChars: 5}},
		{"hello world\n", textStats{Bytes: 12, Chars: 12, Lines: 1, Words: 2, MaxLineChars: 11, EndsWithNewline: true}},
		{"a\n\n  \nbb cc\n", textStats{Bytes: 12, Chars: 12, Lines: 4, BlankLines: 2, Words: 3, MaxLineChars: 5, EndsWithNewline: true}},
		{"\n", textStats{Bytes: 1, Chars: 1, Lines: 1, BlankLines: 1, EndsWithNewline: true}},
		{"你好 世界", textStats{Bytes: 13, Chars: 5, Lines: 1, Words: 2, CJKChars: 4, MaxLineChars: 5}},
		{"tab\tsep\r\nx", textStats{Bytes: 10, Chars: 10, Lines: 2, Words: 3, MaxLineChars: 8}},
	}
	for _, c := range cases {
		if got := computeTextStats(c.text); got != c.want {
			t.Errorf("stats(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
	s, _ := testServer(t, "tools")
	if w := do(t, s, "/v1/tools/text.stats", `{}`); w.Code != 400 {
		t.Errorf("missing text: HTTP %d", w.Code)
	}
}

func TestTextDigest(t *testing.T) {
	s, _ := testServer(t, "tools")
	cases := []struct {
		args string
		want map[string]string
		enc  string
		n    string
	}{
		{`{"text":"hello"}`, map[string]string{"sha256": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}, "utf8", "5"},
		{`{"text":"hello","algorithms":["md5","sha1","cid-raw"]}`, map[string]string{
			"md5": "5d41402abc4b2a76b9719d911017c592", "sha1": "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d",
			"cid-raw": "bafkreibm6jg3ux5qumhcn2b3flc3tyu6dmlb4xa7u5bf44yegnrjhc4yeq"}, "utf8", "5"},
		{`{"text":"hello\n","algorithms":["git-blob-sha1"]}`, map[string]string{"git-blob-sha1": "ce013625030ba8dba906f756967f9e9ca394464a"}, "utf8", "6"},
		{`{"text":"","algorithms":["git-blob-sha1","SHA3-256"]}`, map[string]string{
			"git-blob-sha1": "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
			"sha3-256":      "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a"}, "utf8", "0"},
		{`{"text":"abc","algorithms":["sha384","sha512","sha3-512"]}`, map[string]string{
			"sha384":   "cb00753f45a35e8bb5a03d699ac65007272c32ab0eded1631a8b605a43ff5bed8086072ba1e7cc2358baeca134c825a7",
			"sha512":   "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f",
			"sha3-512": "b751850b1a57168a5693cd924b6b096e08f621827444f70d884f5d0240d2712e10e116e9192af3c91a7ec57647e3934057340b4cf408d5a56592f8274eec53f0"}, "utf8", "3"},
		{`{"base64":"AAEC/w=="}`, map[string]string{"sha256": "3d1f57c984978ef98a18378c8166c1cb8ede02c03eeb6aee7e2f121dfeee3e56"}, "base64", "4"},
		{`{"base64":"AAEC_w"}`, map[string]string{"sha256": "3d1f57c984978ef98a18378c8166c1cb8ede02c03eeb6aee7e2f121dfeee3e56"}, "base64", "4"},
	}
	for _, c := range cases {
		out := call(t, s, "text.digest", c.args)
		got := map[string]string{}
		for k, v := range out["digests"].(map[string]any) {
			got[k] = v.(string)
		}
		if !reflect.DeepEqual(got, c.want) || out["encoding"] != c.enc || out["bytes"] != json.Number(c.n) {
			t.Errorf("%s: got %v %v %v", c.args, got, out["encoding"], out["bytes"])
		}
	}
	for _, bad := range []string{
		`{}`, `{"text":"a","base64":"YQ=="}`, `{"base64":"%%%"}`,
		`{"text":"a","algorithms":["crc32"]}`, `{"text":"a","algorithm":"sha256"}`, `{"text":1}`,
	} {
		if w := do(t, s, "/v1/tools/text.digest", bad); w.Code != 400 {
			t.Errorf("%s: HTTP %d, want 400", bad, w.Code)
		}
	}
}

// The paid twin gives byte-for-byte the answer of the free one, so a payer
// can check that the payment bought exactly that computation.
func TestPaidDigestIsTheFreeDigest(t *testing.T) {
	s, _ := testServer(t, "tools,paid")
	for _, args := range []string{
		`{"text":"hello"}`,
		`{"text":"<&>","algorithms":["sha256","cid-raw","git-blob-sha1"]}`,
		`{"base64":"AAEC/w=="}`,
	} {
		free := do(t, s, "/v1/tools/text.digest", args)
		paid := do(t, s, "/v1/paid/demo.digest.paid", args)
		if free.Code != 200 || paid.Code != 200 || free.Body.String() != paid.Body.String() {
			t.Errorf("%s:\nfree %d %s\npaid %d %s", args, free.Code, free.Body, paid.Code, paid.Body)
		}
	}
	// The paid door takes less.
	big := `{"text":"` + strings.Repeat("x", 5000) + `"}`
	if w := do(t, s, "/v1/paid/demo.digest.paid", big); w.Code != 413 {
		t.Errorf("paid digest over 4 KiB: HTTP %d", w.Code)
	}
	if w := do(t, s, "/v1/tools/text.digest", big); w.Code != 200 {
		t.Errorf("free digest of 5 KB: HTTP %d", w.Code)
	}
}

func TestDigestHandlerDirect(t *testing.T) {
	res, err := handleTextDigest(context.Background(), nil, []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := res.(digestResult).Digests["sha256"]; d != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("sha256 = %s", d)
	}
}

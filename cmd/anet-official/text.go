package main

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/anetcid"
)

// handleEcho returns the arguments under "echo". They are nested, never
// merged into the top level: the service module reads a top-level
// "evidence" object as the service's own statement about the call, and an
// echo must not let a caller write that statement.
func handleEcho(_ context.Context, e *env, body []byte) (any, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(body, &args); err != nil || args == nil {
		return nil, badArgs("arguments must be a JSON object")
	}
	return map[string]any{
		"echo":           json.RawMessage(body),
		"bytes":          len(body),
		"received_at_ms": e.now().UnixMilli(),
		"service":        "anet-official",
		"version":        e.version,
	}, nil
}

type textStatsArgs struct {
	Text *string `json:"text"`
}

// textStats is the result of text.stats. The definitions are fixed so that
// anyone can recompute them:
//
//	bytes            UTF-8 bytes
//	chars            Unicode code points
//	lines            lines as an editor shows them: newline-terminated
//	                 lines plus a final unterminated one; 0 for ""
//	blank_lines      lines holding only white space
//	words            maximal runs of non-white-space (unicode.IsSpace)
//	cjk_chars        code points in the Han, Hiragana, Katakana and Hangul scripts
//	max_line_chars   code points in the longest line, without its newline
//	ends_with_newline whether the text ends in "\n"
type textStats struct {
	Bytes           int  `json:"bytes"`
	Chars           int  `json:"chars"`
	Lines           int  `json:"lines"`
	BlankLines      int  `json:"blank_lines"`
	Words           int  `json:"words"`
	CJKChars        int  `json:"cjk_chars"`
	MaxLineChars    int  `json:"max_line_chars"`
	EndsWithNewline bool `json:"ends_with_newline"`
}

func handleTextStats(_ context.Context, _ *env, body []byte) (any, error) {
	var a textStatsArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	if a.Text == nil {
		return nil, badArgs("\"text\" is required")
	}
	return computeTextStats(*a.Text), nil
}

func computeTextStats(s string) textStats {
	st := textStats{Bytes: len(s), Chars: utf8.RuneCountInString(s), EndsWithNewline: strings.HasSuffix(s, "\n")}
	if s == "" {
		return st
	}
	inWord := false
	lineChars := 0
	lineBlank := true
	endLine := func() {
		st.Lines++
		if lineBlank {
			st.BlankLines++
		}
		if lineChars > st.MaxLineChars {
			st.MaxLineChars = lineChars
		}
		lineChars, lineBlank = 0, true
	}
	for _, r := range s {
		if r == '\n' {
			endLine()
			inWord = false
			continue
		}
		lineChars++
		if unicode.IsSpace(r) {
			inWord = false
			continue
		}
		lineBlank = false
		if !inWord {
			st.Words++
			inWord = true
		}
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			st.CJKChars++
		}
	}
	if !st.EndsWithNewline {
		endLine()
	}
	return st
}

type digestArgs struct {
	Text       *string  `json:"text"`
	Base64     *string  `json:"base64"`
	Algorithms []string `json:"algorithms"`
}

// digestAlgorithms are the names text.digest accepts. Every one is in the
// Go standard library or ANetCore; none needs a network or a file.
var digestAlgorithms = map[string]func([]byte) string{
	"sha256":   hexOf(sha256.New),
	"sha384":   hexOf(sha512.New384),
	"sha512":   hexOf(sha512.New),
	"sha3-256": func(b []byte) string { h := sha3.Sum256(b); return hex.EncodeToString(h[:]) },
	"sha3-512": func(b []byte) string { h := sha3.Sum512(b); return hex.EncodeToString(h[:]) },
	"sha1":     hexOf(sha1.New),
	"md5":      hexOf(md5.New),
	// git hashes a blob as "blob <len>\x00<bytes>"; this is the id
	// `git hash-object` prints for the same bytes.
	"git-blob-sha1": func(b []byte) string {
		h := sha1.New()
		h.Write([]byte("blob " + strconv.Itoa(len(b)) + "\x00"))
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	},
	// The CID anet gives opaque bytes (ANetCore anetcid.SumRaw): CIDv1,
	// raw codec, sha2-256, base32 multibase.
	"cid-raw": func(b []byte) string {
		c, err := anetcid.SumRaw(b)
		if err != nil {
			return ""
		}
		return c
	},
}

func hexOf(newHash func() hash.Hash) func([]byte) string {
	return func(b []byte) string {
		h := newHash()
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}
}

// digestResult is the result of text.digest and demo.digest.paid. It has
// no time and no version in it: the paid twin must give byte-for-byte the
// same answer, so that a payer can check what the payment bought.
type digestResult struct {
	Bytes    int               `json:"bytes"`
	Encoding string            `json:"encoding"`
	Digests  map[string]string `json:"digests"`
}

func handleTextDigest(_ context.Context, _ *env, body []byte) (any, error) {
	var a digestArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	var data []byte
	enc := ""
	switch {
	case a.Text != nil && a.Base64 != nil:
		return nil, badArgs("give \"text\" or \"base64\", not both")
	case a.Text != nil:
		data, enc = []byte(*a.Text), "utf8"
	case a.Base64 != nil:
		b, err := decodeBase64(*a.Base64)
		if err != nil {
			return nil, badArgs("\"base64\": %v", err)
		}
		data, enc = b, "base64"
	default:
		return nil, badArgs("\"text\" or \"base64\" is required")
	}
	algs := a.Algorithms
	if len(algs) == 0 {
		algs = []string{"sha256"}
	}
	if len(algs) > len(digestAlgorithms) {
		return nil, badArgs("at most %d algorithms", len(digestAlgorithms))
	}
	out := digestResult{Bytes: len(data), Encoding: enc, Digests: map[string]string{}}
	for _, name := range algs {
		f, ok := digestAlgorithms[strings.ToLower(name)]
		if !ok {
			return nil, badArgs("unknown algorithm %q (have %s)", name, strings.Join(sortedKeys(digestAlgorithms), ", "))
		}
		out.Digests[strings.ToLower(name)] = f(data)
	}
	return out, nil
}

// decodeBase64 accepts standard and URL-safe base64, padded or not.
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errBadBase64
}

var errBadBase64 = badArgs("not valid base64")

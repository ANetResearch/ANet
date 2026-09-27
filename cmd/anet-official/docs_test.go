package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ANetResearch/ANetCore/anetcid"
)

// The corpus CIDs are recomputable from the files alone: each document's
// CID is the raw CID of its bytes, and the corpus CID is the raw CID of the
// sorted "<cid> <source>" manifest.
func TestCorpusCIDsAreRecomputable(t *testing.T) {
	c, err := loadCorpus()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.docs) < 5 {
		t.Fatalf("only %d documents compiled in", len(c.docs))
	}
	var manifest strings.Builder
	for _, src := range c.order {
		b, err := fs.ReadFile(corpusFS, "corpus/"+src)
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := anetcid.SumRaw(b)
		if cid != c.docs[src].CID {
			t.Errorf("%s: CID %s, recomputed %s", src, c.docs[src].CID, cid)
		}
		fmt.Fprintf(&manifest, "%s %s\n", cid, src)
	}
	if want, _ := anetcid.SumRaw([]byte(manifest.String())); want != c.CID || manifest.String() != c.Manifest {
		t.Errorf("corpus CID %s, recomputed %s", c.CID, want)
	}
	for _, must := range []string{"docs/GUIDE-zh.md", "docs/A2A-DESIGN-zh.md", "docs/ARCHITECTURE-zh.md", "deploy/official/README.md"} {
		if _, ok := c.docs[must]; !ok {
			t.Errorf("%s is not in the corpus", must)
		}
	}
	for src := range c.docs {
		if strings.Contains(src, "notes/") {
			t.Errorf("%s: survey notes are not public documentation", src)
		}
	}
}

func smallCorpus(t *testing.T) *corpus {
	t.Helper()
	fsys := fstest.MapFS{
		"c/a.md": {Data: []byte("# Alpha\n\nThe relay carries sealed envelopes.\n\n## Payments\n\nx402.payment.status is required.\nThe hub settles credit.\n")},
		"c/b.md": {Data: []byte("# 测试文档\n\n端到端加密保护任务内容。\n\n## 配额\n\n公开能力有按调用方的配额。\n")},
	}
	c, err := buildCorpus(fsys, "c")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSearch(t *testing.T) {
	c := smallCorpus(t)
	cases := []struct {
		query, source string
		wantFirst     string // source:from of the first hit
	}{
		{"x402.payment.status", "", "a.md:5"},
		{"sealed envelopes", "", "a.md:1"},
		{"端到端加密", "", "b.md:1"},
		{"配额", "", "b.md:5"},
		{"hub", "b.md", ""},
	}
	for _, cs := range cases {
		res, err := c.search(context.Background(), cs.query, 5, cs.source)
		if err != nil {
			t.Fatal(err)
		}
		first := ""
		if len(res.Hits) > 0 {
			first = fmt.Sprintf("%s:%d", res.Hits[0].Source, res.Hits[0].From)
		}
		if first != cs.wantFirst {
			t.Errorf("%q in %q: first hit %s, want %s (%+v)", cs.query, cs.source, first, cs.wantFirst, res.Hits)
		}
		if res.CorpusCID != c.CID {
			t.Errorf("corpus CID missing from the answer")
		}
	}
}

// The same corpus and query give the same answer, every time, on the real
// corpus.
func TestSearchIsDeterministic(t *testing.T) {
	e := testEnv(t)
	args := []byte(`{"query":"public_capabilities 配额 max_args_bytes","k":10}`)
	a, err := handleDocsSearch(context.Background(), e, args)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, _ := handleDocsSearch(context.Background(), e, args)
		if !reflect.DeepEqual(a, b) {
			t.Fatal("two searches gave different answers")
		}
	}
	res := a.(*searchResult)
	if len(res.Hits) == 0 || len(res.Hits) > 10 {
		t.Fatalf("%d hits", len(res.Hits))
	}
	for i, h := range res.Hits {
		if len(h.Snippet) > maxSnippetBytes {
			t.Errorf("hit %d: snippet of %d bytes", i, len(h.Snippet))
		}
		if i > 0 && h.Score > res.Hits[i-1].Score {
			t.Errorf("hits are not ordered by score")
		}
		if _, ok := e.corpus.docs[h.Source]; !ok || h.From < 1 || h.To < h.From {
			t.Errorf("hit %d: bad location %s %d-%d", i, h.Source, h.From, h.To)
		}
	}
	for _, bad := range []string{`{}`, `{"query":"  "}`, `{"query":"x","k":11}`, `{"query":"x","k":-1}`,
		`{"query":"x","source":"nope.md"}`, `{"query":"` + strings.Repeat("a", 600) + `"}`} {
		if _, err := handleDocsSearch(context.Background(), e, []byte(bad)); err == nil {
			t.Errorf("%s: must be refused", bad)
		}
	}
}

func TestDocsGet(t *testing.T) {
	e := testEnv(t)
	get := func(args string) map[string]any {
		t.Helper()
		out, err := handleDocsGet(context.Background(), e, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		b, _ := json.Marshal(out)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	list := get(`{}`)
	docs := list["documents"].([]any)
	if len(docs) != len(e.corpus.docs) || list["corpus_cid"] != e.corpus.CID {
		t.Fatalf("list: %d documents", len(docs))
	}

	d := e.corpus.docs["docs/GUIDE-zh.md"]
	part := get(`{"source":"docs/GUIDE-zh.md","from":2,"to":4}`)
	want := strings.Join(d.lines[1:4], "\n") + "\n"
	if part["content"] != want || part["from"] != 2.0 || part["to"] != 4.0 || part["truncated"] != false || part["cid"] != d.CID {
		t.Errorf("lines 2-4: %+v", part)
	}

	// A long document comes in pieces, each under the bound, and the
	// pieces put together are the document.
	src := "docs/A2A-DESIGN-zh.md"
	var whole strings.Builder
	from := 1
	for i := 0; i < 100 && from > 0; i++ {
		p := get(fmt.Sprintf(`{"source":%q,"from":%d}`, src, from))
		content := p["content"].(string)
		if len(content) > maxGetBytes {
			t.Fatalf("piece of %d bytes", len(content))
		}
		whole.WriteString(content)
		from = 0
		if n, ok := p["next_from"].(float64); ok {
			from = int(n)
		}
	}
	if doc := e.corpus.docs[src]; whole.String() != strings.Join(doc.lines, "\n")+"\n" {
		t.Error("the pieces do not reassemble the document")
	}

	for _, bad := range []string{`{"source":"nope.md"}`, `{"source":"../../etc/passwd"}`, `{"from":3}`,
		`{"source":"docs/GUIDE-zh.md","from":100000}`, `{"source":"docs/GUIDE-zh.md","from":5,"to":4}`} {
		if _, err := handleDocsGet(context.Background(), e, []byte(bad)); err == nil {
			t.Errorf("%s: must be refused", bad)
		}
	}
}

func TestTokenize(t *testing.T) {
	cases := map[string][]string{
		"Hello, World_2!": {"hello", "world_2"},
		"端到端":             {"端", "端到", "到", "到端", "端"},
		"x402.payment":    {"x402", "payment"},
		"a中b":             {"a", "中", "b"},
	}
	for in, want := range cases {
		if got := tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("tokenize(%q) = %q, want %q", in, got, want)
		}
	}
}

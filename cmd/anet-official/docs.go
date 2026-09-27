package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/anetcid"
)

// corpusFS is the documentation docs.search and docs.get serve, compiled in
// at build time. It is refreshed from the repository by
// refresh-corpus.sh; nothing is read from disk or the network at run time,
// so the answer depends on the binary and the question only.
//
//go:embed corpus
var corpusFS embed.FS

// corpus is the loaded documentation with its content identifiers.
//
// Each document's CID is anetcid.SumRaw of its bytes (CIDv1, raw codec,
// sha2-256). The corpus CID is anetcid.SumRaw of the manifest: one line
// "<cid> <source>\n" per document, sorted by source. Anyone holding the
// same files computes the same CIDs, so a docs.search answer can be tied to
// the exact text it was computed from.
type corpus struct {
	CID      string
	Manifest string
	docs     map[string]*document
	order    []string
	chunks   []*chunk
	df       map[string]int // chunks containing each term
	avgLen   float64
}

type document struct {
	Source string
	CID    string
	Title  string
	Bytes  int
	lines  []string // without their newlines
}

// chunk is a searchable passage: a run of lines under one heading, at most
// maxChunkLines long.
type chunk struct {
	doc      *document
	from, to int // 1-based, inclusive
	heading  string
	tf       map[string]int
	length   int
	lower    string
}

const maxChunkLines = 30

var (
	corpusOnce   sync.Once
	loadedCorpus *corpus
	corpusErr    error
)

// loadCorpus loads the embedded corpus once.
func loadCorpus() (*corpus, error) {
	corpusOnce.Do(func() { loadedCorpus, corpusErr = buildCorpus(corpusFS, "corpus") })
	return loadedCorpus, corpusErr
}

func buildCorpus(fsys fs.FS, root string) (*corpus, error) {
	c := &corpus{docs: map[string]*document{}, df: map[string]int{}}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if !utf8.Valid(b) {
			return fmt.Errorf("corpus: %s is not UTF-8", p)
		}
		cid, err := anetcid.SumRaw(b)
		if err != nil {
			return err
		}
		src := strings.TrimPrefix(p, root+"/")
		text := strings.ReplaceAll(string(b), "\r\n", "\n")
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		doc := &document{Source: src, CID: cid, Bytes: len(b), lines: lines}
		for _, l := range lines {
			if strings.HasPrefix(l, "# ") {
				doc.Title = strings.TrimSpace(strings.TrimPrefix(l, "# "))
				break
			}
		}
		c.docs[src] = doc
		c.order = append(c.order, src)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(c.docs) == 0 {
		return nil, errors.New("corpus: no documents compiled in")
	}
	sort.Strings(c.order)
	var m strings.Builder
	for _, src := range c.order {
		fmt.Fprintf(&m, "%s %s\n", c.docs[src].CID, src)
	}
	c.Manifest = m.String()
	if c.CID, err = anetcid.SumRaw([]byte(c.Manifest)); err != nil {
		return nil, err
	}
	total := 0
	for _, src := range c.order {
		for _, ch := range chunkDocument(c.docs[src]) {
			for t := range ch.tf {
				c.df[t]++
			}
			total += ch.length
			c.chunks = append(c.chunks, ch)
		}
	}
	if len(c.chunks) > 0 {
		c.avgLen = float64(total) / float64(len(c.chunks))
	}
	return c, nil
}

// chunkDocument splits a document at headings, and long sections at blank
// lines, into passages of at most maxChunkLines lines.
func chunkDocument(d *document) []*chunk {
	var out []*chunk
	heading := d.Title
	start := 0
	flush := func(end int) { // lines[start:end]
		for start < end && strings.TrimSpace(d.lines[start]) == "" {
			start++
		}
		if start >= end {
			return
		}
		text := strings.Join(d.lines[start:end], "\n")
		ch := &chunk{doc: d, from: start + 1, to: end, heading: heading, tf: map[string]int{}, lower: strings.ToLower(text)}
		for _, t := range tokenize(heading + "\n" + text) {
			ch.tf[t]++
			ch.length++
		}
		out = append(out, ch)
	}
	inFence := false
	for i, l := range d.lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(l, "#") && strings.TrimLeft(l, "#") != l && strings.HasPrefix(strings.TrimLeft(l, "#"), " ") {
			flush(i)
			heading = strings.TrimSpace(strings.TrimLeft(l, "#"))
			start = i
			continue
		}
		if i-start >= maxChunkLines && !inFence && strings.TrimSpace(l) == "" {
			flush(i)
			start = i
		} else if i-start >= 2*maxChunkLines {
			flush(i)
			start = i
		}
	}
	flush(len(d.lines))
	return out
}

// tokenize lowercases and splits text into terms: runs of letters, digits
// and '_' for alphabetic scripts, and overlapping character bigrams for
// CJK text, which has no spaces to split on. Single CJK characters are
// terms too, so a one-character query still matches.
func tokenize(s string) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		for i := range cjk {
			out = append(out, string(cjk[i]))
			if i+1 < len(cjk) {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

type docsSearchArgs struct {
	Query  string `json:"query"`
	K      int    `json:"k"`
	Source string `json:"source"`
}

type searchHit struct {
	Source  string  `json:"source"`
	Lines   string  `json:"lines"`
	From    int     `json:"from"`
	To      int     `json:"to"`
	Heading string  `json:"heading,omitempty"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

type searchResult struct {
	Query     string      `json:"query"`
	CorpusCID string      `json:"corpus_cid"`
	Hits      []searchHit `json:"hits"`
}

const (
	defaultSearchK  = 5
	maxSearchK      = 10
	maxSnippetBytes = 1536
	maxQueryBytes   = 512
)

func handleDocsSearch(ctx context.Context, e *env, body []byte) (any, error) {
	var a docsSearchArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return nil, badArgs("\"query\" is required")
	}
	if len(q) > maxQueryBytes {
		return nil, badArgs("\"query\" is longer than %d bytes", maxQueryBytes)
	}
	k := a.K
	if k == 0 {
		k = defaultSearchK
	}
	if k < 1 || k > maxSearchK {
		return nil, badArgs("\"k\" must be 1-%d", maxSearchK)
	}
	c := e.corpus
	if a.Source != "" {
		if _, ok := c.docs[a.Source]; !ok {
			return nil, badArgs("no document %q (docs.get with no source lists them)", a.Source)
		}
	}
	return c.search(ctx, q, k, a.Source)
}

// search ranks passages by BM25 over the query terms, plus a bonus for each
// whitespace-separated query word that occurs verbatim (case-insensitive)
// in the passage, which is what finds identifiers like
// "x402.payment.status". Ties go to the earlier source and line, so the
// same corpus and query always give the same list.
func (c *corpus) search(ctx context.Context, q string, k int, only string) (*searchResult, error) {
	terms := uniq(tokenize(q))
	words := uniq(strings.Fields(strings.ToLower(q)))
	type scored struct {
		ch    *chunk
		score float64
	}
	var hits []scored
	n := float64(len(c.chunks))
	const k1, b = 1.2, 0.75
	for i, ch := range c.chunks {
		if i%256 == 0 && ctx.Err() != nil {
			return nil, errBudget
		}
		if only != "" && ch.doc.Source != only {
			continue
		}
		score := 0.0
		for _, t := range terms {
			tf := float64(ch.tf[t])
			if tf == 0 {
				continue
			}
			df := float64(c.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(ch.length)/c.avgLen))
		}
		for _, w := range words {
			if len(w) >= 2 && strings.Contains(ch.lower, w) {
				score += 1.5
			}
		}
		if score > 0 {
			hits = append(hits, scored{ch, math.Round(score*10000) / 10000})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].ch.doc.Source != hits[j].ch.doc.Source {
			return hits[i].ch.doc.Source < hits[j].ch.doc.Source
		}
		return hits[i].ch.from < hits[j].ch.from
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	res := &searchResult{Query: q, CorpusCID: c.CID, Hits: []searchHit{}}
	for _, h := range hits {
		from, to, snip := snippet(h.ch, terms, words)
		res.Hits = append(res.Hits, searchHit{Source: h.ch.doc.Source, Lines: fmt.Sprintf("%d-%d", from, to),
			From: from, To: to, Heading: h.ch.heading, Score: h.score, Snippet: snip})
	}
	return res, nil
}

// snippet returns up to maxSnippetBytes of a passage, starting a little
// before its first line that mentions the query.
func snippet(ch *chunk, terms, words []string) (int, int, string) {
	first := ch.from
	for ln := ch.from; ln <= ch.to; ln++ {
		lower := strings.ToLower(ch.doc.lines[ln-1])
		hit := false
		for _, w := range words {
			if strings.Contains(lower, w) {
				hit = true
				break
			}
		}
		if !hit {
			for _, t := range terms {
				if strings.Contains(lower, t) {
					hit = true
					break
				}
			}
		}
		if hit {
			first = ln
			break
		}
	}
	start := max(first-2, ch.from)
	var b strings.Builder
	end := start - 1
	for ln := start; ln <= ch.to; ln++ {
		l := ch.doc.lines[ln-1]
		if b.Len()+len(l)+1 > maxSnippetBytes {
			if b.Len() == 0 {
				b.WriteString(truncateUTF8(l, maxSnippetBytes))
				end = ln
			}
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
		end = ln
	}
	return start, end, b.String()
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type docsGetArgs struct {
	Source string `json:"source"`
	From   int    `json:"from"`
	To     int    `json:"to"`
}

type docEntry struct {
	Source string `json:"source"`
	CID    string `json:"cid"`
	Title  string `json:"title,omitempty"`
	Lines  int    `json:"lines"`
	Bytes  int    `json:"bytes"`
}

type docsListResult struct {
	CorpusCID string     `json:"corpus_cid"`
	Documents []docEntry `json:"documents"`
}

type docsGetResult struct {
	Source     string `json:"source"`
	CID        string `json:"cid"`
	CorpusCID  string `json:"corpus_cid"`
	TotalLines int    `json:"total_lines"`
	From       int    `json:"from"`
	To         int    `json:"to"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
	NextFrom   int    `json:"next_from,omitempty"`
}

// maxGetBytes bounds one docs.get answer.
const maxGetBytes = 16 << 10

func handleDocsGet(_ context.Context, e *env, body []byte) (any, error) {
	var a docsGetArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	c := e.corpus
	if a.Source == "" {
		if a.From != 0 || a.To != 0 {
			return nil, badArgs("\"from\" and \"to\" need a \"source\"")
		}
		out := docsListResult{CorpusCID: c.CID, Documents: []docEntry{}}
		for _, src := range c.order {
			d := c.docs[src]
			out.Documents = append(out.Documents, docEntry{Source: src, CID: d.CID, Title: d.Title, Lines: len(d.lines), Bytes: d.Bytes})
		}
		return out, nil
	}
	d, ok := c.docs[a.Source]
	if !ok {
		return nil, badArgs("no document %q (docs.get with no source lists them)", a.Source)
	}
	from, to := a.From, a.To
	if from == 0 {
		from = 1
	}
	if to == 0 || to > len(d.lines) {
		to = len(d.lines)
	}
	if from < 1 || from > len(d.lines) || to < from {
		return nil, badArgs("lines %d-%d are outside 1-%d", a.From, a.To, len(d.lines))
	}
	res := docsGetResult{Source: d.Source, CID: d.CID, CorpusCID: c.CID, TotalLines: len(d.lines), From: from}
	var b strings.Builder
	last := from - 1
	cut := false
	for ln := from; ln <= to; ln++ {
		l := d.lines[ln-1]
		if b.Len()+len(l)+1 > maxGetBytes {
			if ln == from { // one line longer than the bound
				b.WriteString(truncateUTF8(l, maxGetBytes-1))
				b.WriteByte('\n')
				last, cut = ln, true
			}
			break
		}
		b.WriteString(l)
		b.WriteByte('\n')
		last = ln
	}
	res.To, res.Content = last, b.String()
	if last < to || cut {
		res.Truncated = true
		if last < len(d.lines) {
			res.NextFrom = last + 1
		}
	}
	return res, nil
}

package main

import "time"

// allCapabilities is the first batch of official public capabilities
// (A2A-DESIGN §15). The table is the single source for the routes this
// binary serves and for the daemon configuration it prints
// (service-config): a limit changed here changes both, and the samples in
// deploy/official are checked against it by a test.
//
// Limits are initial values (docs/notes/0010 §5.2), to be calibrated in the
// joint tests. MaxArgsBytes is measured on the JSON arguments, as the
// kernel measures max_args_bytes.
func allCapabilities() []*capability {
	return []*capability{
		{
			ID: "net.echo", Group: "echo",
			Name: "Echo",
			Description: "Returns the JSON arguments unchanged under \"echo\", with the time this backend received " +
				"the call and its version. Use it to check that a delegation reaches an anet agent and that its " +
				"signed result comes back; compare the echo with what you sent. Arguments: any JSON object up to 4 KiB.",
			Tags:         []string{"echo", "ping", "connectivity", "diagnostics"},
			Examples:     []string{`{"hello":"anet"}`, `{"nonce":"4f1c2a","sent_at_ms":1790000000000}`},
			MaxArgsBytes: 4 << 10,
			Timeout:      time.Second,
			Quota:        quota{PerCallerPerMin: 60, PerCallerPerDay: 2000, GlobalPerMin: 1200, MaxInflight: 16},
			Handle:       handleEcho,
		},
		{
			ID: "text.stats", Group: "tools",
			Name: "Text statistics",
			Description: "Counts bytes, characters (Unicode code points), lines, words (runs of non-space), blank " +
				"lines, CJK characters and the longest line of a UTF-8 text. Arguments: {\"text\": string}, up to " +
				"256 KiB of JSON. Deterministic: the same text always gives the same numbers.",
			Tags:         []string{"text", "statistics", "count", "words", "lines"},
			Examples:     []string{`{"text":"hello world\nsecond line\n"}`},
			MaxArgsBytes: 256 << 10,
			Timeout:      2 * time.Second,
			Quota:        quota{PerCallerPerMin: 30, PerCallerPerDay: 1000, GlobalPerMin: 600, MaxInflight: 8},
			Handle:       handleTextStats,
		},
		{
			ID: "text.digest", Group: "tools",
			Name: "Text digest",
			Description: "Hashes text or base64-encoded bytes. Arguments: {\"text\": string} or {\"base64\": string}, " +
				"and optionally \"algorithms\" from sha256 (default), sha384, sha512, sha3-256, sha3-512, sha1, md5, " +
				"git-blob-sha1 (the id git gives the bytes as a blob) and cid-raw (the anet/IPFS CIDv1 raw sha2-256 " +
				"of the bytes). Returns lowercase hex digests. Up to 256 KiB of JSON.",
			Tags:         []string{"hash", "digest", "sha256", "checksum", "cid"},
			Examples:     []string{`{"text":"hello"}`, `{"text":"hello","algorithms":["sha256","git-blob-sha1","cid-raw"]}`},
			MaxArgsBytes: 256 << 10,
			Timeout:      2 * time.Second,
			Quota:        quota{PerCallerPerMin: 30, PerCallerPerDay: 1000, GlobalPerMin: 600, MaxInflight: 8},
			Handle:       handleTextDigest,
		},
		{
			ID: "text.diff", Group: "tools",
			Name: "Unified diff",
			Description: "Line-based unified diff of two texts. Arguments: {\"a\": string, \"b\": string}, optional " +
				"\"context\" (lines around each change, 0-20, default 3), \"a_name\" and \"b_name\" (header labels). " +
				"Returns the diff (cut at 256 KiB, flagged \"truncated\"), whether the texts are identical, and " +
				"counts of added and removed lines. Up to 512 KiB of JSON in total.",
			Tags:         []string{"diff", "text", "compare", "patch", "unified"},
			Examples:     []string{`{"a":"one\ntwo\n","b":"one\n2\n","context":1}`},
			MaxArgsBytes: 512 << 10,
			Timeout:      5 * time.Second,
			Quota:        quota{PerCallerPerMin: 10, PerCallerPerDay: 300, GlobalPerMin: 120, MaxInflight: 4},
			Handle:       handleTextDiff,
		},
		{
			ID: "json.validate", Group: "tools",
			Name: "JSON and JSON Schema validation",
			Description: "Checks JSON syntax (with line and column of the first error, and duplicate member names) " +
				"and, when a schema is given, validates against JSON Schema 2020-12 (draft-07 accepted). Errors carry " +
				"JSON Pointers into the instance and the schema. Arguments: \"json\" (text) or \"instance\" (value), " +
				"optional \"schema\". Only local $ref (\"#...\") is followed; remote references are reported, never " +
				"fetched. Keywords it does not evaluate are listed and make the verdict \"unknown\" rather than \"valid\". " +
				"Up to 512 KiB of JSON.",
			Tags:         []string{"json", "json-schema", "validation", "lint"},
			Examples:     []string{`{"json":"{\"a\": 1, \"a\": 2}"}`, `{"schema":{"type":"object","required":["id"]},"instance":{"name":"x"}}`},
			MaxArgsBytes: 512 << 10,
			Timeout:      5 * time.Second,
			Quota:        quota{PerCallerPerMin: 20, PerCallerPerDay: 600, GlobalPerMin: 300, MaxInflight: 4},
			Handle:       handleJSONValidate,
		},
		{
			ID: "a2a.card.validate", Group: "tools",
			Name: "A2A AgentCard check",
			Description: "Checks an A2A 1.0 AgentCard: required fields and types per a2a.proto, skills, interfaces, " +
				"security requirements, leftover A2A 0.3 fields, members a proto-based parser would drop, and default " +
				"values that A2A §8.4.1 removes before signing. Each JWS signature is reported with its header and is " +
				"verified when a key is supplied (\"jwks\", or \"kel\" for did:anet keys): EdDSA, ES256/384/512, RS256/384/512, " +
				"PS256/384/512, over both the card as given and the card with defaults removed. anet network cards are " +
				"also checked against the anet admission rules. Arguments: \"card\" (object) or \"card_json\" (exact text). " +
				"Keys are never fetched.",
			Tags:         []string{"a2a", "agent-card", "validation", "jws", "signature"},
			Examples:     []string{`{"card_json":"{\"name\":\"Recipe Agent\",...}"}`},
			MaxArgsBytes: 160 << 10,
			Timeout:      2 * time.Second,
			Quota:        quota{PerCallerPerMin: 20, PerCallerPerDay: 600, GlobalPerMin: 300, MaxInflight: 8},
			Handle:       handleCardValidate,
		},
		{
			ID: "a2a.x402.check", Group: "tools",
			Name: "a2a-x402 payment object check",
			Description: "Checks a2a-x402 v0.2 payment data: message metadata (x402.payment.status and the keys each " +
				"status requires), PaymentRequired, PaymentRequirements, PaymentPayload and SettlementResponse objects " +
				"(x402 v2, and v1 where recognisable), error codes, and the status transitions of a sequence of " +
				"metadata objects. Returns a list of issues with JSON Pointers. Arguments: one of \"metadata\", " +
				"\"sequence\" (array of metadata) or \"object\" with optional \"kind\". Up to 64 KiB.",
			Tags:         []string{"a2a", "x402", "payments", "validation"},
			Examples:     []string{`{"metadata":{"x402.payment.status":"payment-completed","x402.payment.receipts":[{"success":true,"transaction":"t1","network":"base"}]}}`},
			MaxArgsBytes: 64 << 10,
			Timeout:      2 * time.Second,
			Quota:        quota{PerCallerPerMin: 20, PerCallerPerDay: 600, GlobalPerMin: 300, MaxInflight: 8},
			Handle:       handleX402Check,
		},
		{
			ID: "docs.search", Group: "docs",
			Name: "Search the anet documentation",
			Description: "Keyword search over anet's public documentation as compiled into this agent (guide, " +
				"architecture, A2A design, contracts, payments, install). Returns up to k (default 5, at most 10) " +
				"passages with source path, line range, heading and a snippet, plus the corpus CID: the same corpus " +
				"and query always give the same hits. Arguments: {\"query\": string, \"k\": int, \"source\": string (optional filter)}.",
			Tags:         []string{"docs", "search", "anet", "a2a", "documentation"},
			Examples:     []string{`{"query":"public_capabilities quota"}`, `{"query":"x402.payment.status","k":3}`},
			MaxArgsBytes: 2 << 10,
			Timeout:      3 * time.Second,
			Quota:        quota{PerCallerPerMin: 20, PerCallerPerDay: 1000, GlobalPerMin: 300, MaxInflight: 8},
			Handle:       handleDocsSearch,
		},
		{
			ID: "docs.get", Group: "docs",
			Name: "Read an anet document",
			Description: "Returns lines of one document in the compiled corpus, with the document's CID and the corpus " +
				"CID, up to 16 KiB per call (\"next_from\" continues). Arguments: {\"source\": path, \"from\": line, " +
				"\"to\": line}, lines 1-based and inclusive. Without \"source\" it lists the documents.",
			Tags:         []string{"docs", "anet", "documentation", "read"},
			Examples:     []string{`{}`, `{"source":"docs/GUIDE-zh.md","from":1,"to":40}`},
			MaxArgsBytes: 1 << 10,
			Timeout:      time.Second,
			Quota:        quota{PerCallerPerMin: 30, PerCallerPerDay: 2000, GlobalPerMin: 600, MaxInflight: 8},
			Handle:       handleDocsGet,
		},
		{
			ID: "demo.digest.paid", Group: "paid",
			Name: "Paid digest (payment demo)",
			Description: "The same computation as text.digest, for a price, to try an a2a-x402 payment end to end: " +
				"payment-required, payment-submitted, payment-completed, with the hub's signed receipt. The result is " +
				"byte-for-byte the result text.digest gives for the same arguments, so what was paid for can be checked. " +
				"Arguments as text.digest, up to 4 KiB.",
			Tags:         []string{"x402", "payments", "demo", "digest"},
			Examples:     []string{`{"text":"hello"}`},
			MaxArgsBytes: 4 << 10,
			Timeout:      2 * time.Second,
			// Paid calls are rate-limited by payment; only the global and
			// in-flight bounds are suggested (docs/notes/0010 §5.4).
			Quota:  quota{GlobalPerMin: 300, MaxInflight: 8},
			Price:  2,
			Handle: handleTextDigest,
		},
	}
}

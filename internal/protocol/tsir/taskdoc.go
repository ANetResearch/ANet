// TaskDoc: the L4 semantic-waist object — the one canonical, content-addressed task
// contract — plus the canonical preimage / CID / sign and the compile() (ASI) entry.
//
// Normative source: design3/spec/tsir-spec.md §2.1 (TaskDoc CDDL), §3.3 (canonical
// preimage = total CID-significance membership), §3.4 (CID), §5.4 (compile projection).
// A conformant Agent Network has exactly ONE task-contract object; any side-struct is a
// derived projection. compile(TSIR) is the only application entry into the runtime.
package tsir

import (
	"errors"

	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/aobj"
	"github.com/ANetResearch/ANet/internal/protocol/coredet"
	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

// VersionPair is the TaskDoc typed version {major, minor} (tsir-spec §2.1).
type VersionPair struct {
	Major uint64 `cbor:"1,keyasint"`
	Minor uint64 `cbor:"2,keyasint,omitempty"`
}

// MetaEntry is a name/value metadata pair (tsir-spec §2.1).
type MetaEntry struct {
	Name  string `cbor:"1,keyasint"`
	Value string `cbor:"2,keyasint"`
}

// Link is a typed reference; rel ∈ supersedes/derived-from/related/template/parent/contract
// (tsir-spec §2.1/§2.5). A "template" link references an AET; "contract" references an ACC.
type Link struct {
	Rel   string `cbor:"1,keyasint"`
	Href  string `cbor:"2,keyasint"`
	Title string `cbor:"3,keyasint,omitempty"`
}

// Intent is the task goal (tsir-spec §2.1).
type Intent struct {
	Summary string `cbor:"1,keyasint,omitempty"`
	Format  string `cbor:"2,keyasint,omitempty"` // "text" / "markdown"
	Body    string `cbor:"3,keyasint"`
}

// Require is a capability requirement (tsir-spec §2.1).
type Require struct {
	ID          string `cbor:"1,keyasint"`
	Type        string `cbor:"2,keyasint,omitempty"` // skill/tool/protocol/resource
	Necessity   string `cbor:"3,keyasint"`           // must/should/optional
	Description string `cbor:"4,keyasint,omitempty"`
}

// Output / Schema describe an acceptance artifact (tsir-spec §2.1).
type Output struct {
	Format  string `cbor:"1,keyasint"` // media-type shape (contains "/")
	MaxSize string `cbor:"2,keyasint,omitempty"`
	Name    string `cbor:"3,keyasint,omitempty"`
}
type Schema struct {
	Type string `cbor:"1,keyasint"` // json-schema / regex
	Body string `cbor:"2,keyasint"`
}

// Accept is a success-criterion entry; type ∈ artifact/test/quantitative/qualitative.
type Accept struct {
	Type        string   `cbor:"1,keyasint"`
	Description string   `cbor:"2,keyasint,omitempty"`
	Outputs     []Output `cbor:"3,keyasint,omitempty"`
	Schema      *Schema  `cbor:"4,keyasint,omitempty"`
}

// Context is supplementary key/value with visibility (tsir-spec §2.1).
type Context struct {
	Key        string `cbor:"1,keyasint"`
	Value      string `cbor:"2,keyasint,omitempty"`
	Src        string `cbor:"3,keyasint,omitempty"`
	Format     string `cbor:"4,keyasint,omitempty"`
	Visibility string `cbor:"5,keyasint,omitempty"` // open/restricted/private
}

// Task is one task unit (tsir-spec §2.1).
type Task struct {
	ID            string     `cbor:"1,keyasint,omitempty"`
	Intent        Intent     `cbor:"10,keyasint"`
	Requires      []Require  `cbor:"11,keyasint,omitempty"`
	Accepts       []Accept   `cbor:"12,keyasint,omitempty"`
	Contexts      []Context  `cbor:"13,keyasint,omitempty"`
	PositiveScope *Predicate `cbor:"14,keyasint,omitempty"`
	NegativeScope *Predicate `cbor:"15,keyasint,omitempty"`
	CouplingHint  int        `cbor:"16,keyasint,omitempty"` // 1 Trace/2 Intent/3 Role/4 Composite
}

// TaskDoc is the canonical task-contract object (tsir-spec §2.1).
type TaskDoc struct {
	Version  VersionPair `cbor:"1,keyasint"`
	Meta     []MetaEntry `cbor:"2,keyasint,omitempty"`
	Links    []Link      `cbor:"3,keyasint,omitempty"`
	Tasks    []Task      `cbor:"4,keyasint"`
	Critical []uint64    `cbor:"7,keyasint,omitempty"` // MUST-understand MINOR keys
	// Envelope is the detached P1 signature. cbor:"-" enforces structurally that it is NEVER inside
	// the TaskDoc map (tsir-spec §2.7 / §3.3): it rides the carrier, not the wire object, and is
	// excluded from the canonical preimage by construction. (Key 14 is the spec's positive_scope slot.)
	Envelope *aobj.Envelope `cbor:"-"`
}

// --- canonical preimage (tsir-spec §3.3): total CID-significance membership ---
//
// version → {1: major} (minor dropped); links[]/tasks[] verbatim when present; critical[]
// (key 7), the envelope, corr_id, MINOR-only fields are EXCLUDED. meta[] is included only
// for registered cid_significant labels — with no meta registry yet, all meta is treated as
// unregistered/cosmetic and excluded (baseline; the registry-flag rule is a follow-up).

type versionMajor struct {
	Major uint64 `cbor:"1,keyasint"`
}

type taskDocPreimage struct {
	Version versionMajor `cbor:"1,keyasint"`
	Links   []Link       `cbor:"3,keyasint,omitempty"`
	Tasks   []Task       `cbor:"4,keyasint"`
}

// CanonicalPreimage returns the CoreDet-CBOR signing/CID preimage (tsir-spec §3.3).
func (d *TaskDoc) CanonicalPreimage() ([]byte, error) {
	return coredet.Marshal(taskDocPreimage{
		Version: versionMajor{Major: d.Version.Major},
		Links:   d.Links,
		Tasks:   d.Tasks,
	})
}

// CID returns the TaskDoc content identifier over the canonical preimage (tsir-spec §3.4).
func (d *TaskDoc) CID() (string, error) {
	pre, err := d.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches a detached owner signature over the canonical preimage (sign-binds-CID).
func (d *TaskDoc) Sign(c *identity.Controller) error {
	pre, err := d.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := c.Sign(pre)
	d.Envelope = &aobj.Envelope{SignerAID: c.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	return nil
}

// Verify checks the owner signature against the owner's KEL (verify-before-compile, §5.2).
//
// msgTime is the object's binding time in unix-millis — the live caller threads the ALP
// datagram msg_time; 0 = unknown/baseline. It feeds the time-aware revocation gate (§5.2):
// a key valid at signing time stays valid for objects time-stamped before its retirement
// (grace), and is REVOKED_KEY for objects bound at/after the rot/dip that superseded it.
func (d *TaskDoc) Verify(kel []identity.SignedEvent, msgTime uint64) error {
	if d.Envelope == nil {
		return errors.New("tsir: unsigned TaskDoc")
	}
	if err := d.Envelope.Validate(); err != nil {
		return err
	}
	pre, err := d.CanonicalPreimage()
	if err != nil {
		return err
	}
	return identity.VerifyObject(kel, d.Envelope.SignerAID, d.Envelope.KeyStateSeq, msgTime, pre, d.Envelope.Sig)
}

// --- compile() — the Application Service Interface entry (tsir-spec §5.4) ---

// CompileResult is the projection compile() hands to the L3 runtime: the four load-bearing
// semantic classes plus the TaskDoc CID. (The full CoordinationContext is ascp-03's; this
// is the TSIR-side seam — the "H_TSIR projection record".)
type CompileResult struct {
	TaskCID             string
	Intent              Intent
	AcceptancePredicate *Predicate // nil ⇒ Intent-Alignment-ineligible (only free-text accepts)
	NegativeScope       *Predicate
	CouplingHint        int
	IntentAlignEligible bool
}

// Compile is the ASI entry compile(TSIR) → projection. It MUST verify before projecting
// (verify-before-compile, §5.2/§5.4) and project at least intent, the acceptance predicate,
// the negative action-scope, and coupling_hint for each task. Predicates are Validate()-checked.
// Multi-task TaskDocs project the first task here (baseline); per-task fan-out is the runtime's.
//
// msgTime is the object's binding time in unix-millis (the live caller threads the ALP datagram
// msg_time; 0 = unknown/baseline) — threaded into Verify's time-aware revocation gate (§5.2).
func Compile(d *TaskDoc, kel []identity.SignedEvent, msgTime uint64) (*CompileResult, error) {
	if err := d.Verify(kel, msgTime); err != nil {
		return nil, err
	}
	if len(d.Tasks) == 0 {
		return nil, errors.New("tsir: TaskDoc has no tasks")
	}
	cid, err := d.CID()
	if err != nil {
		return nil, err
	}
	t := d.Tasks[0]
	// F10 (§2.4 coupling-hint = 1..4): a non-zero coupling_hint outside the frozen 1..4 range
	// is MALFORMED (an enum int outside its frozen range ⇒ MALFORMED, tsir-spec §2.4/§1).
	if t.CouplingHint != 0 && (t.CouplingHint < 1 || t.CouplingHint > 4) {
		return nil, ErrMalformed
	}
	if t.NegativeScope != nil {
		if err := t.NegativeScope.Validate(); err != nil {
			return nil, err
		}
	}
	if t.PositiveScope != nil {
		if err := t.PositiveScope.Validate(); err != nil {
			return nil, err
		}
	}
	// Intent-Alignment eligibility (§3.4/§5.3): a decidable acceptance predicate is required;
	// a task whose accepts are only "qualitative" free text is ineligible.
	eligible := false
	for _, a := range t.Accepts {
		if a.Type != "qualitative" {
			eligible = true
			break
		}
	}
	return &CompileResult{
		TaskCID:             cid,
		Intent:              t.Intent,
		AcceptancePredicate: nil, // built from accepts by the projector (follow-up); eligibility flagged
		NegativeScope:       t.NegativeScope,
		CouplingHint:        t.CouplingHint,
		IntentAlignEligible: eligible,
	}, nil
}

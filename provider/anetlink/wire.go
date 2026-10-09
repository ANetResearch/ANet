package anetlink

// wire.go is C1 v1 as this client speaks it: the objects, keyed as the wire registry (ANetCore
// wire/registry.json) has them, their encoding, and the full check of every object that crosses.
// ANetLink keeps its own copy of these shapes (northbound/c1serv) on an older ANetCore; what keeps
// the two equal is C1-1, the same golden bytes in both repositories, each encoded and decoded by its
// own side (testdata/c1v1, golden_test.go).
//
// Every object received is decoded with coredet.UnmarshalStrict: an unregistered or retired key, a
// duplicate map key, a CBOR tag, text that is not UTF-8 and bytes after the object are refused, as
// ANetLink refuses them. The rules a decoder cannot state (required members, the via/aid pairing)
// follow in the same function, so that "decoded" always means "accepted": each decode* below is the
// entrance for its object, and nothing in this package unmarshals C1 any other way.

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
)

const (
	// contentCBOR is the one media type of C1, both ways.
	contentCBOR = "application/cbor"
	// maxBody bounds a request body (ANetLink answers a larger one 413 too_large) and what this
	// client reads of an answer.
	maxBody = 1 << 20
)

// Wire values of Caller.via. 2 was the voucher door of C1 v0 and is retired: nothing here can
// produce it (callerOf), and ANetLink refuses it like any other value.
const (
	viaLocal uint64 = 0
	viaRelay uint64 = 1
)

// capList is the answer to GET /v1/capabilities.
//
//anet:wire CapList
type capList struct {
	Caps []capEntry `cbor:"1,keyasint"`
}

// capEntry is one capability: its id, opaque here, and whether ANetLink lets a remote caller use it
// (1) or not (0). Remote is a pointer so that a missing key is seen.
//
//anet:wire CapEntry
type capEntry struct {
	ID     string  `cbor:"1,keyasint"`
	Remote *uint64 `cbor:"2,keyasint"`
}

// describe is the answer to GET /v1/describe: the CID of ANetLink's description object, "" for none.
//
//anet:wire Describe
type describe struct {
	CID *string `cbor:"1,keyasint"`
}

// invokeReq is the body of POST /v1/invoke. The required members are pointers so that a missing one
// is seen. There is no field for key 4 (C1 v0's caller_aid, retired), so the decoder refuses it.
//
//anet:wire InvokeReq
type invokeReq struct {
	Capability *string        `cbor:"1,keyasint,omitempty"`
	Args       map[string]any `cbor:"2,keyasint,omitempty"`
	CallID     *string        `cbor:"3,keyasint,omitempty"`
	Caller     *caller        `cbor:"5,keyasint,omitempty"`
}

// caller is who asks: via 0 (a local surface, no aid) or 1 (the relay, with the AID the daemon
// verified). Key 3 is reserved (it was witnessed) and has no field, so it is refused.
//
//anet:wire Caller
type caller struct {
	Via *uint64 `cbor:"1,keyasint,omitempty"`
	AID *string `cbor:"2,keyasint,omitempty"`
}

// invokeResp is the 200 answer to an invoke. Its evidence is effect.Evidence itself, the kernel's
// type with the same keys (EffectEvidence), so a field added there cannot be dropped here.
//
//anet:wire InvokeResp
type invokeResp struct {
	Status   string             `cbor:"1,keyasint"`
	Metrics  map[string]float64 `cbor:"2,keyasint,omitempty"`
	Message  string             `cbor:"3,keyasint,omitempty"`
	Evidence *effect.Evidence   `cbor:"4,keyasint,omitempty"`
}

// refusal is the body of every answer that is not a success: a C1 reason and nothing else.
//
//anet:wire Refusal
type refusal struct {
	Reason string `cbor:"1,keyasint"`
}

// c1Status is every C1 v1 reason (core §2.15.3) with the HTTP status it comes with. These are
// ANetLink's codes; the daemon maps them to its own (a2ashape.C1Reason) and never shows them. A
// reason outside the set, or one with another status, is a far side that does not speak C1 v1.
var c1Status = map[string]int{
	"not_permitted":      403,
	"bad_request":        400,
	"too_large":          413,
	"unsupported_media":  415,
	"unknown_capability": 404,
	"c1_version":         426,
	"not_found":          404,
	"internal":           500,
}

// decodeInvokeReq is ANetLink's entrance for an invoke body, every rule of it: the strict decode,
// then the required members and the via/aid pairing (LEC §2.1.1). This client reads back each of
// its own requests through it before sending (encodeInvokeReq), so a request ANetLink would refuse
// as bad_request never leaves; the golden test and F-C1 hold it to ANetLink's.
func decodeInvokeReq(b []byte) (invokeReq, error) {
	var req invokeReq
	if err := coredet.UnmarshalStrict(b, &req); err != nil {
		return invokeReq{}, err
	}
	if req.Capability == nil {
		return invokeReq{}, errors.New("missing capability (key 1)")
	}
	if req.Caller == nil {
		return invokeReq{}, errors.New("missing caller (key 5)")
	}
	c := req.Caller
	if c.Via == nil {
		return invokeReq{}, errors.New("missing caller.via (key 5.1)")
	}
	switch *c.Via {
	case viaLocal:
		if c.AID != nil {
			return invokeReq{}, errors.New("caller.via is 0 (local) but carries an aid")
		}
	case viaRelay:
		if c.AID == nil || *c.AID == "" {
			return invokeReq{}, errors.New("caller.via is 1 (relay) but carries no aid")
		}
	default:
		return invokeReq{}, fmt.Errorf("caller.via %d is not 0 (local) or 1 (relay)", *c.Via)
	}
	return req, nil
}

// encodeInvokeReq is the body of one call, read back through decodeInvokeReq.
func encodeInvokeReq(req invokeReq) ([]byte, error) {
	b, err := coredet.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := decodeInvokeReq(b); err != nil {
		return nil, fmt.Errorf("the request would be refused: %w", err)
	}
	return b, nil
}

// decodeInvokeResp reads a 200 invoke answer: its status is required.
func decodeInvokeResp(b []byte) (invokeResp, error) {
	var r invokeResp
	if err := coredet.UnmarshalStrict(b, &r); err != nil {
		return invokeResp{}, err
	}
	if r.Status == "" {
		return invokeResp{}, errors.New("missing status (key 1)")
	}
	return r, nil
}

// decodeCapList reads the answer to GET /v1/capabilities: the list is required, and every entry
// has an id and a remote flag.
func decodeCapList(b []byte) (capList, error) {
	var l capList
	if err := coredet.UnmarshalStrict(b, &l); err != nil {
		return capList{}, err
	}
	if l.Caps == nil {
		return capList{}, errors.New("missing caps (key 1)")
	}
	for i, c := range l.Caps {
		if c.ID == "" {
			return capList{}, fmt.Errorf("caps[%d] has no id (key 1)", i)
		}
		if c.Remote == nil {
			return capList{}, fmt.Errorf("caps[%d] (%q) has no remote flag (key 2)", i, c.ID)
		}
	}
	return l, nil
}

// decodeDescribe reads the answer to GET /v1/describe: the cid is required, "" when there is none.
func decodeDescribe(b []byte) (describe, error) {
	var d describe
	if err := coredet.UnmarshalStrict(b, &d); err != nil {
		return describe{}, err
	}
	if d.CID == nil {
		return describe{}, errors.New("missing cid (key 1)")
	}
	return d, nil
}

// decodeRefusal reads a refusal: its reason is required.
func decodeRefusal(b []byte) (refusal, error) {
	var r refusal
	if err := coredet.UnmarshalStrict(b, &r); err != nil {
		return refusal{}, err
	}
	if r.Reason == "" {
		return refusal{}, errors.New("missing reason (key 1)")
	}
	return r, nil
}

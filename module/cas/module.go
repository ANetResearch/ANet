//go:build !no_cas

package cas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// Content-addressed storage, as a module.
//
// anet3's storage was 26,720 lines in one package: point-of-interest
// records, org objects, natural-language indexes, tasks, plasma, knowledge
// records, ontologies, credits, triggers. It was the daemon's entire
// persistence layer rather than a capability, which is why "make storage
// pluggable" has no meaning applied to it as a whole.
//
// What is pluggable is the layer everything else was built on: a store that
// maps a CID to the exact bytes whose hash is that CID, and back. The v3
// runtime had already separated it — 225 lines, filesystem-backed, atomic
// and idempotent writes, verify-on-read — and it came across unchanged
// because the CID construction it relies on is ANetCore's.
//
// Deliberately schema-free. Searchable metadata, logical paths and indexes
// belong to whichever layer needs them; a CAS that knew about org objects
// would be the beginning of the 26,720 lines again.
const name = "cas"

// Capability ids this module offers.
const (
	CapPut  = "cas.put"
	CapGet  = "cas.get"
	CapHas  = "cas.has"
	CapStat = "cas.stat"
)

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Dir == "" {
			return nil, fmt.Errorf("cas: dir is required")
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config says where blobs live.
type Config struct {
	// Dir is the directory the store owns. There is no default: a store
	// that picked its own location would put a node's content somewhere the
	// operator did not choose and cannot find.
	Dir string `json:"dir"`
	// MaxBlobBytes caps one blob. Zero means the default below.
	MaxBlobBytes int64 `json:"max_blob_bytes"`
}

// defaultMaxBlob bounds what a peer can hand this node in one call.
//
// A capability that accepts arbitrary bytes from the network needs a limit,
// and it needs one by default rather than when an operator remembers: the
// failure without it is a node whose disk is filled by someone else.
const defaultMaxBlob = 32 << 20 // 32 MiB

// Module offers one content-addressed store.
type Module struct {
	cfg   Config
	store *Store
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	s, err := Open(m.cfg.Dir)
	if err != nil {
		return fmt.Errorf("cas: open %s: %w", m.cfg.Dir, err)
	}
	m.store = s
	return h.Providers().Register(ctx, &casProvider{m: m})
}

func (m *Module) Stop(context.Context) error { return nil }

// Store exposes the store to a daemon that wants it in-process.
func (m *Module) Store() *Store { return m.store }

func (m *Module) maxBlob() int64 {
	if m.cfg.MaxBlobBytes > 0 {
		return m.cfg.MaxBlobBytes
	}
	return defaultMaxBlob
}

type casProvider struct{ m *Module }

func (p *casProvider) ID() string { return name }

func (p *casProvider) Capabilities(context.Context) ([]string, error) {
	return []string{CapPut, CapGet, CapHas, CapStat}, nil
}

func (p *casProvider) Describe(context.Context) (string, error) { return "", nil }

func (p *casProvider) Health(context.Context) error {
	if p.m.store == nil {
		return fmt.Errorf("cas: not started")
	}
	return nil
}

func (p *casProvider) Invoke(_ context.Context, call provider.Call) (effect.Effect, error) {
	if p.m.store == nil {
		return effect.Effect{}, fmt.Errorf("cas: not started")
	}
	switch call.Capability {
	case CapPut:
		return p.put(call)
	case CapGet:
		return p.get(call)
	case CapHas:
		return p.has(call)
	case CapStat:
		return p.stat(call)
	}
	return effect.Effect{}, fmt.Errorf("cas: unknown capability %q", call.Capability)
}

// put stores bytes and returns the CID they hash to.
//
// The CID is not the caller's to choose — it is derived from the bytes, so
// a peer cannot store content under a name that misdescribes it. That
// property is the entire point of content addressing and it is why the
// effect can be V4: the store read the CID back out of the data itself.
func (p *casProvider) put(call provider.Call) (effect.Effect, error) {
	raw, _ := call.Args["body"].(string)
	if raw == "" {
		return effect.Effect{}, fmt.Errorf("cas: put needs base64 bytes in args.body")
	}
	body, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return effect.Effect{}, fmt.Errorf("cas: body not base64: %w", err)
	}
	if int64(len(body)) > p.m.maxBlob() {
		return effect.Effect{
			Status: effect.Failed,
			Message: fmt.Sprintf("cas: blob is %d bytes, limit is %d",
				len(body), p.m.maxBlob()),
			Evidence: &effect.Evidence{Protocol: name, Requested: CapPut},
		}, nil
	}
	cidStr, err := p.m.store.Put(body)
	if err != nil {
		return effect.Effect{}, err
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"bytes": float64(len(body))}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapPut, NativeAck: true,
			ObservedState: cidStr,
			// The stored bytes hash to this CID — measured from the content,
			// not reported by the writer.
			VerifyTrust: 4,
		},
	}, nil
}

// get returns the bytes for a CID, verified on read.
func (p *casProvider) get(call provider.Call) (effect.Effect, error) {
	cidStr, _ := call.Args["cid"].(string)
	if cidStr == "" {
		return effect.Effect{}, fmt.Errorf("cas: get needs args.cid")
	}
	body, err := p.m.store.Get(cidStr)
	if err != nil {
		return effect.Effect{
			Status: effect.Failed, Message: err.Error(),
			Evidence: &effect.Evidence{Protocol: name, Requested: CapGet, ObservedState: cidStr},
		}, nil
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"bytes": float64(len(body))}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapGet, NativeAck: true,
			ObservedState: base64.StdEncoding.EncodeToString(body),
			// Read back and re-hashed before returning: the bytes are the
			// bytes that CID names, or the read failed.
			VerifyTrust: 4,
		},
	}, nil
}

func (p *casProvider) has(call provider.Call) (effect.Effect, error) {
	cidStr, _ := call.Args["cid"].(string)
	if cidStr == "" {
		return effect.Effect{}, fmt.Errorf("cas: has needs args.cid")
	}
	ok, err := p.m.store.Has(cidStr)
	if err != nil {
		return effect.Effect{}, err
	}
	n := 0.0
	if ok {
		n = 1
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"present": n}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapHas, NativeAck: true,
			ObservedState: cidStr, VerifyTrust: 3,
		},
	}, nil
}

func (p *casProvider) stat(call provider.Call) (effect.Effect, error) {
	cidStr, _ := call.Args["cid"].(string)
	if cidStr == "" {
		return effect.Effect{}, fmt.Errorf("cas: stat needs args.cid")
	}
	size, err := p.m.store.Stat(cidStr)
	if err != nil {
		return effect.Effect{
			Status: effect.Failed, Message: err.Error(),
			Evidence: &effect.Evidence{Protocol: name, Requested: CapStat, ObservedState: cidStr},
		}, nil
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"bytes": float64(size)}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapStat, NativeAck: true,
			ObservedState: cidStr, VerifyTrust: 3,
		},
	}, nil
}

var _ provider.CapabilityProvider = (*casProvider)(nil)

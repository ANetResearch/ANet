// Package voucherv1 is the 0.2 voucher — a hub's signed statement that a capability call is paid
// for — kept while the node-side redemption gate (module/x402) still honours one. ANetCore 0.16
// deleted the type with the hub's x402 gateway (HPP §2.8): a buyer pays the provider's quote through
// the task instead.
//
// v030-todo(PAY-9): the redemption gate and this package are deleted.
package voucherv1

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
)

// Voucher is a hub's signed statement that a specific piece of work has
// been paid for, addressed to the provider that must honour it.
//
// It exists so the paying and the doing can happen in different places. A
// hub can host an x402 resource server — take the 402, settle the credit
// — without becoming a proxy for the work: it hands back a voucher, and
// the client goes to the provider itself. The hub therefore never sees
// the request or the result, which is the same property the relay has and
// worth keeping for the same reason. It cannot leak what it never held.
//
// The cost is real and should be stated: the client must be able to reach
// the provider directly. Where it cannot — the provider is behind a NAT
// with no public face — this shape does not apply, and the ordinary
// delegate-and-pay path through the relay does.
//
// One-time is enforced by the provider, not the hub. The hub cannot know
// whether a voucher was used; the provider is the only party that can,
// because it is the one performing the work. So Nonce is here for the
// provider to remember, and a provider that does not remember it has
// agreed to be paid once for work done twice.
type Voucher struct {
	AuthID     string `cbor:"1,keyasint"` // the settled authorization
	Payer      string `cbor:"2,keyasint"`
	PayTo      string `cbor:"3,keyasint"` // the provider bound to honour it
	Capability string `cbor:"4,keyasint"` // exactly what was bought
	Amount     uint64 `cbor:"5,keyasint"`
	Network    string `cbor:"6,keyasint"`
	NotAfter   int64  `cbor:"7,keyasint"` // unix millis
	Nonce      string `cbor:"8,keyasint"` // the provider's uniqueness key
	// ArgsCID pins the arguments, when the buyer named them at purchase.
	// Empty means the voucher buys the capability rather than one exact
	// call of it. Both are legitimate; which one is in force is something
	// the provider can see rather than assume.
	ArgsCID string `cbor:"9,keyasint,omitempty"`
	// Envelope is the settling hub's detached signature.
	Envelope *aobj.Envelope `cbor:"-"`
}

type voucherPreimage struct {
	AuthID     string `cbor:"1,keyasint"`
	Payer      string `cbor:"2,keyasint"`
	PayTo      string `cbor:"3,keyasint"`
	Capability string `cbor:"4,keyasint"`
	Amount     uint64 `cbor:"5,keyasint"`
	Network    string `cbor:"6,keyasint"`
	NotAfter   int64  `cbor:"7,keyasint"`
	Nonce      string `cbor:"8,keyasint"`
	ArgsCID    string `cbor:"9,keyasint,omitempty"`
}

func (v *Voucher) canonical() voucherPreimage {
	return voucherPreimage{AuthID: v.AuthID, Payer: v.Payer, PayTo: v.PayTo,
		Capability: v.Capability, Amount: v.Amount, Network: v.Network,
		NotAfter: v.NotAfter, Nonce: v.Nonce, ArgsCID: v.ArgsCID}
}

// CanonicalPreimage returns the CoreDet-CBOR signing preimage.
func (v *Voucher) CanonicalPreimage() ([]byte, error) { return coredet.Marshal(v.canonical()) }

// ID is the content identifier over the preimage — a stable name for one
// voucher, and what a provider stores when it marks the voucher spent.
func (v *Voucher) ID() (string, error) {
	pre, err := v.CanonicalPreimage()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Sign attaches the settling hub's signature.
func (v *Voucher) Sign(c *identity.Controller) error {
	pre, err := v.CanonicalPreimage()
	if err != nil {
		return err
	}
	sig, seq := c.Sign(pre)
	v.Envelope = &aobj.Envelope{SignerAID: c.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	return nil
}

// Verify checks the voucher against the hub's key history and pins every
// term the provider is relying on.
//
// expectPayTo is the provider's own AID and expectCapability the work it
// is about to do: passing them is what makes this a check rather than a
// signature test. A voucher that verifies cryptographically but was
// issued for somebody else's capability is a valid signature over a
// statement that does not authorise this work.
//
// expectNetwork must be the provider's own hub. A voucher signed by some
// other hub verifies fine against that hub's KEL and means nothing here:
// the provider would be doing paid work against credit on a ledger it has
// no account on. The caller passes the network it will actually be paid
// on, and a mismatch is refused.
func (v *Voucher) Verify(kel []identity.SignedEvent, expectSigner, expectPayTo,
	expectCapability, expectNetwork string, now int64) error {
	if v.Envelope == nil {
		return errors.New("payment: unsigned voucher")
	}
	if err := v.Envelope.Validate(); err != nil {
		return err
	}
	if expectSigner != "" && v.Envelope.SignerAID != expectSigner {
		return fmt.Errorf("payment: voucher signed by %s, not the expected hub %s",
			v.Envelope.SignerAID, expectSigner)
	}
	if v.PayTo != expectPayTo {
		return fmt.Errorf("payment: voucher is payable to %s, not to this provider", v.PayTo)
	}
	if v.Capability != expectCapability {
		return fmt.Errorf("payment: voucher bought %q, not %q", v.Capability, expectCapability)
	}
	if expectNetwork != "" && v.Network != expectNetwork {
		return fmt.Errorf("payment: voucher settles on %s, not on %s", v.Network, expectNetwork)
	}
	if v.NotAfter != 0 && pastWithSkew(now, v.NotAfter) {
		return errors.New("payment: voucher expired")
	}
	if v.Nonce == "" {
		return errors.New("payment: voucher has no nonce, so it cannot be spent only once")
	}
	pre, err := v.CanonicalPreimage()
	if err != nil {
		return err
	}
	return identity.VerifyObject(kel, v.Envelope.SignerAID, v.Envelope.KeyStateSeq,
		uint64(now), pre, v.Envelope.Sig)
}

type wireVoucher struct {
	Body     voucherPreimage `cbor:"1,keyasint"`
	Envelope *aobj.Envelope  `cbor:"2,keyasint"`
}

// Marshal encodes the voucher with its detached signature.
func (v *Voucher) Marshal() ([]byte, error) {
	return coredet.Marshal(wireVoucher{Body: v.canonical(), Envelope: v.Envelope})
}

// UnmarshalVoucher decodes a wire voucher.
func UnmarshalVoucher(b []byte) (*Voucher, error) {
	var w wireVoucher
	if err := coredet.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &Voucher{
		AuthID: w.Body.AuthID, Payer: w.Body.Payer, PayTo: w.Body.PayTo,
		Capability: w.Body.Capability, Amount: w.Body.Amount, Network: w.Body.Network,
		NotAfter: w.Body.NotAfter, Nonce: w.Body.Nonce, ArgsCID: w.Body.ArgsCID,
		Envelope: w.Envelope,
	}, nil
}

// pastWithSkew reports whether now is more than payment.ClockSkew after t, comparing the difference
// rather than adding the skew to t (which overflowed for a NotAfter near math.MaxInt64).
func pastWithSkew(now, t int64) bool { return now > t && uint64(now)-uint64(t) > payment.ClockSkew }

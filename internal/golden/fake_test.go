package golden_test

import "github.com/ANetResearch/ANetCore/aobj"

func fakeEnvelope() *aobj.Envelope {
	return &aobj.Envelope{
		SignerAID: "did:anet:bgolden0author0aid000000000000000000000000000",
		Alg:       aobj.AlgEdDSA, KeyStateSeq: 0, Sig: []byte("not a real signature"),
	}
}

//go:build !no_x402

package x402

import (
	"encoding/hex"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/coredet"
)

func hexOf(b [32]byte) string { return hex.EncodeToString(b[:]) }

func marshalRecord(r *ael.EventRecord) ([]byte, error) { return coredet.Marshal(r) }

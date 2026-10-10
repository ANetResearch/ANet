package main

import "github.com/ANetResearch/ANet/internal/hubapi"

// hubDecodeB64 is the daemon's rule for a binary value of the wire, which
// this process's decodeB64 must match (TestTheEnvelopeRuleIsTheDaemons).
func hubDecodeB64(s string) ([]byte, error) { return hubapi.DecodeB64(s, -1) }

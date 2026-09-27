//go:build no_x402

package main

import (
	"errors"
	"io"
	"testing"
)

// In a -tags no_x402 build `anet redeem` ends before the terminal
// question: the build has nothing to sign with.
func TestRedeemInANoX402Build(t *testing.T) {
	if paymentsCompiled() {
		t.Fatal("a -tags no_x402 build reports the payment module")
	}
	prev := openTTY
	t.Cleanup(func() { openTTY = prev })
	openTTY = func() (io.ReadWriteCloser, error) {
		t.Fatal("the terminal was opened")
		return nil, errors.New("asked")
	}
	if err := runRedeem(&client{base: "http://127.0.0.1:1"}, []string{"7"}); !errors.Is(err, errNoPaymentModule) {
		t.Fatalf("redeem: %v", err)
	}
}

package main

import (
	"errors"

	"github.com/ANetResearch/ANet/module"
)

// paymentsCompiled reports whether this build has the payment module: a
// -tags no_x402 build has nothing to sign a payment with, so a paying
// command says so before it asks anything on the terminal. Tests replace
// it.
var paymentsCompiled = func() bool { return module.Availability{}.Available("x402") }

// errNoPaymentModule is a paying command in a build without the payment
// module.
var errNoPaymentModule = errors.New("this build has no payment module (-tags no_x402), so it cannot pay or redeem; " +
	"nothing was asked or signed")

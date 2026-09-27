package main

import (
	"errors"
	"slices"

	"github.com/ANetResearch/ANet/module"
)

// paymentsCompiled reports whether this build has the payment module: a
// -tags no_x402 build has nothing to sign a payment with, so a paying
// command says so before it asks anything on the terminal. Tests replace
// it.
var paymentsCompiled = func() bool { return slices.Contains(module.Compiled(), "x402") }

// errNoPaymentModule is a paying command in a build without the payment
// module.
var errNoPaymentModule = errors.New("本构建未编入支付模块(-tags no_x402),不能付款或兑付;没有询问,也没有签任何东西 " +
	"(this build has no payment module; nothing was asked or signed)")

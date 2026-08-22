//go:build !no_x402

// The x402 module: this node can be paid for its work and can pay for
// other people's. Removed by -tags no_x402, which takes the facilitator
// client, the redemption path and the public voucher listener with it.
package main

import _ "github.com/ANetResearch/ANet/module/x402"

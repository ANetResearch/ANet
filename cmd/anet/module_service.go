//go:build !no_service

// The service module: this node offers its own HTTP services as network
// capabilities. Removed by -tags no_service.
package main

import _ "github.com/ANetResearch/ANet/module/service"

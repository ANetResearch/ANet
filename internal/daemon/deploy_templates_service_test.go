//go:build !no_service

package daemon

// The module the deploy templates configure, registered for the check in
// deploy_templates_test.go when this build has it.
import _ "github.com/ANetResearch/ANet/module/service"

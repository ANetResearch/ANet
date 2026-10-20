// Command legacy-anet stands in for the anet 1.1.15 binary of the earlier AgentNetwork npm package in
// the Windows installation test of CI (deploy/release/test-install-e2e.ps1). It answers version like
// that line does, and records each stop in the file named by LEGACY_ANET_MARK, so the test can see that
// install.ps1 stopped it with its own stop command. It does nothing else.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("AgentNetwork v1.1.15")
	case "stop":
		if p := os.Getenv("LEGACY_ANET_MARK"); p != "" {
			if err := os.WriteFile(p, []byte("stopped\n"), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		fmt.Println("daemon stopped")
	default:
		os.Exit(2)
	}
}

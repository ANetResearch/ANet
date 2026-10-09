//go:build !unix

package main

import "os/exec"

func detachFromTerminal(*exec.Cmd) {}

//go:build !no_taskboard

package main

// The hub's shared task board, as a client.
//
// `-tags no_taskboard` leaves it out — the build for a node that does its
// own work and never coordinates through a board.
import _ "github.com/ANetResearch/ANet/module/taskboard"

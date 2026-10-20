//go:build !windows

package main

// restartNodeCmds is the restart of a node as the user types it.
const restartNodeCmds = "anet stop && anet up"

// restartNodeQuoted is restartNodeCmds with each command in backquotes, for a sentence.
const restartNodeQuoted = "`" + restartNodeCmds + "`"

//go:build windows

package main

// restartNodeCmds is the restart of a node as the user types it. Windows PowerShell 5.1 has no &&,
// so the two commands are named one after the other.
const restartNodeCmds = "anet stop, then anet up"

// restartNodeQuoted is restartNodeCmds with each command in backquotes, for a sentence.
const restartNodeQuoted = "`anet stop`, then `anet up`"

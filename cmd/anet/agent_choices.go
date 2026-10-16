package main

// fixedAgentChoices is the list of coding agents `anet agents wire` and the help text name in a build that
// has no auto-reply module and so no agent registry to read it from. In every other build agentChoices
// reads the registry, and TestFixedAgentChoicesMatchTheRegistry keeps this copy equal to it.
const fixedAgentChoices = "cursor|claude|codex|opencode|openclaw|hermes"

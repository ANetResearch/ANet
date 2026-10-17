package main

// fixedAgentChoices is the list of coding agents `anet agents wire` and the help text name in a build that
// has no auto-reply module and so no agent registry to read it from. In every other build agentChoices
// reads the registry, and TestFixedAgentChoicesMatchTheRegistry keeps this copy equal to it.
const fixedAgentChoices = "cursor|claude|codex|opencode|openclaw|hermes"

// wireAgentChoices is the list of coding agents `anet agents wire` writes to. It is not the auto-reply
// list: openclaw is an auto-reply backend only, and wire does not support it. TestWireAgentChoicesMatchAgentwire
// keeps this copy equal to agentwire.Tools() in the builds that have the MCP module.
const wireAgentChoices = "claude|codex|cursor|opencode|hermes|workbuddy"

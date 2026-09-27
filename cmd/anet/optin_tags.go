package main

// The additive build tags, declared where a build that does NOT contain
// them still compiles this file.
//
// No build tag of its own, on purpose. Every other module announces
// itself by registering, and a module removed by `no_<name>` is silent
// because it was removed. An opt-in module is silent because it was never
// added, and those two silences send an operator to two different flags —
// so the name has to survive in a build that has none of the code.
//
// Adding an opt-in module means two files here: the tagged import that
// links it, and this line that names it when it is not linked.
//
// taskboard is here because an operator upgrading a node whose config
// still has a "taskboard" block gets a refusal to start from the default
// build, and that refusal has to name `-tags taskboard` — the flag that
// exists — not `no_taskboard`, the one that used to.
import "github.com/ANetResearch/ANet/module"

func init() { module.DeclareOptIn("shell", "taskboard") }

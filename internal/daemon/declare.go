package daemon

import "github.com/ANetResearch/ANet/module"

// declare.go says, in every build, which optional subsystems live inside
// this package. They are compiled in whatever the tags are: there is no tag
// that removes them yet, so a build cannot lack them, and `anet version`
// and the name table's Availability must still list them.
//
// No build tag, on purpose. v0.3.x (console) deletes its own name from this
// call when the code moves out and registers itself (module.Register) under a
// tag. Preview did it with MOD-6 and autoreply with MOD-4.
func init() { module.DeclareCompiled("console") }

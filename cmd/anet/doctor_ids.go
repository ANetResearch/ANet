package main

// doctor_ids.go is the register of `anet doctor` check ids (v0.3.0 ux §5.2):
// every id doctor may print, and the sentence keys of the reason table its
// findings may use. The id is the stable name tests assert on (UX-K5); an
// area that adds a check adds its id here.

// doctorID is one registered check id. An id ending in ".*" stands for every
// id under that prefix (agents.<tool>, hermes.*).
type doctorID struct {
	id string
	// keys are the reason-table keys the check's findings use: ok and info
	// sentences first, then warn, fail and unknown (UX-K7).
	keys []string
}

// doctorIDs is the register, in the order of ux §5.2.
var doctorIDs []doctorID

// doctorIDKnown reports whether id is registered.
func doctorIDKnown(id string) bool { return false } // v030-todo(UX-5)

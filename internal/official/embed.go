package official

import (
	_ "embed"
	"sync"

	"github.com/ANetResearch/ANet/internal/release"
)

// The official manifest this binary was built with, and its signature.
//
// Written by `deploy/release/build-release.sh --official` from
// deploy/official/official-agents.txt, signed there with the release key
// (ssh-keygen, namespace release.OfficialNamespace) and committed; a
// release build refuses to start when they do not verify or would expire
// before the release does. Editing manifest.json by hand breaks the
// signature, and a binary built from an unsigned or broken pair marks no
// agent official — it does not fail, and it does not guess.
//
//go:embed manifest.json
var embeddedManifest []byte

//go:embed manifest.json.sig
var embeddedSig []byte

var embedded = sync.OnceValues(func() (*Manifest, error) {
	return Verify(embeddedManifest, embeddedSig, release.OfficialTrust())
})

// Embedded is the manifest built into this binary, verified against the
// release key built into it (release.OfficialTrust). It is verified once
// per process. Whether it is still fresh is Lookup's and CheckFresh's
// question, asked at the time of use.
func Embedded() (*Manifest, error) { return embedded() }

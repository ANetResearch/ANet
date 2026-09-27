package release

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The record an update leaves verifies the binary it installed: the
// signature with the trusted key, the binary by its sha256. Everything that
// breaks one of the two is reported unverified, and no record is unknown.
func TestInstalledRecord(t *testing.T) {
	f := newFixture(t)
	srv := f.server()
	u, exe := f.updater(srv, "0.1.10")
	trust := Trust{Keys: []ed25519.PublicKey{f.pub}}

	if got := CheckInstalled(exe, trust, f.now); got.Status != InstalledUnknown || !strings.Contains(got.Detail, "anet update") {
		t.Fatalf("no record: %+v", got)
	}

	ctx := context.Background()
	found, err := u.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(ctx, found); err != nil {
		t.Fatal(err)
	}
	if err := SaveInstalled(exe, found.Raw, found.Sig); err != nil {
		t.Fatal(err)
	}
	mp, sp := InstalledPaths(exe)
	if b, _ := os.ReadFile(mp); string(b) != string(f.manifest) {
		t.Fatal("the record is not the manifest as verified")
	}
	if b, _ := os.ReadFile(sp); string(b) != string(f.sig) {
		t.Fatal("the record's signature is not the one verified")
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	if len(ents) != 3 {
		t.Fatalf("install directory holds %d entries, want the binary and the two record files", len(ents))
	}

	if ok, err := NamesBinary(found.Manifest, exe); !ok || err != nil {
		t.Fatalf("the installed binary is not named by its manifest: %v %v", ok, err)
	}

	got := CheckInstalled(exe, trust, f.now)
	if got.Status != InstalledVerified || got.Version != "0.2.0" || got.Signer != Fingerprint(f.pub) ||
		!strings.HasPrefix(got.Asset, "anet-") || got.Expired {
		t.Fatalf("after the update: %+v", got)
	}
	// Still verified once the manifest has expired, and it says so.
	if got := CheckInstalled(exe, trust, f.now.AddDate(1, 0, 0)); got.Status != InstalledVerified || !got.Expired ||
		!strings.Contains(got.Detail, "expired") {
		t.Fatalf("after expiry: %+v", got)
	}

	// Another key's signature is not the release key's.
	other, _ := newKey(t)
	if got := CheckInstalled(exe, Trust{Keys: []ed25519.PublicKey{other}}, f.now); got.Status != InstalledUnverified {
		t.Fatalf("an untrusted signer: %+v", got)
	}
	// A binary the manifest does not name: rebuilt or replaced since.
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho rebuilt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := CheckInstalled(exe, trust, f.now); got.Status != InstalledUnverified || !strings.Contains(got.Detail, "not one") {
		t.Fatalf("a replaced binary: %+v", got)
	}
	if ok, err := NamesBinary(found.Manifest, exe); ok || err != nil {
		t.Fatalf("a replaced binary is named by the manifest: %v %v", ok, err)
	}
	if err := os.WriteFile(exe, f.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A manifest edited after it was signed.
	if err := os.WriteFile(mp, []byte(strings.Replace(string(f.manifest), "0.2.0", "0.2.1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CheckInstalled(exe, trust, f.now); got.Status != InstalledUnverified {
		t.Fatalf("an edited manifest: %+v", got)
	}
	// A record without its signature.
	if err := os.WriteFile(mp, f.manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sp); err != nil {
		t.Fatal(err)
	}
	if got := CheckInstalled(exe, trust, f.now); got.Status != InstalledUnverified || !strings.Contains(got.Detail, "signature") {
		t.Fatalf("no signature: %+v", got)
	}
	if err := SaveInstalled(exe, nil, f.sig); err == nil {
		t.Fatal("an empty record was saved")
	}
}

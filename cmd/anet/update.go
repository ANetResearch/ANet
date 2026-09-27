package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ANetResearch/ANet/internal/release"
	"github.com/ANetResearch/ANet/internal/version"
	"github.com/ANetResearch/ANet/module"
)

// runUpdate is `anet update [--check] [--base URL]`: fetch the signed
// release manifest, verify it with the release key compiled into this
// binary, and replace this binary with the matching asset (A2A-DESIGN
// §13.2).
//
// It trusts nothing about where the files come from. That is the point of
// having it: a first install over curl | sh trusts the host serving the
// script, and every update after that trusts only the release key.
func runUpdate(rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
		return fmt.Errorf("anet update takes no arguments (got %q)", strings.Join(pos, " "))
	}
	_, checkOnly := flags["check"]

	switch runtime.GOOS {
	case "linux", "darwin":
	default:
		return fmt.Errorf("anet update replaces the running binary in place, which %s does not allow; download the new release by hand", runtime.GOOS)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find this binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("cannot resolve this binary: %w", err)
	}

	// The variant is read off the module registry, not off the build-tag
	// stamp: a module is in the registry because the linker kept it, so a
	// binary that can execute commands updates to the build that can, and
	// one that cannot never updates into one that can.
	compiled := module.Compiled()
	variant := "default"
	if slices.Contains(compiled, "shell") {
		variant = "shell"
	}

	bases := release.DefaultBases
	if b := strings.TrimSpace(os.Getenv("ANET_INSTALL_BASE")); b != "" {
		bases = append([]string{b}, bases...)
	}
	if b, ok := flags["base"]; ok {
		if b == "" || b == "true" {
			return errors.New("--base needs an https:// URL")
		}
		bases = []string{b}
	}

	u := &release.Updater{
		Bases:   bases,
		Trust:   release.DefaultTrust(),
		Current: version.V,
		Variant: variant,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Exe:     exe,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("anet %s (%s variant, %s/%s) at %s\n", version.V, variant, runtime.GOOS, runtime.GOARCH, exe)
	f, err := u.Check(ctx)
	if errors.Is(err, release.ErrExpired) {
		return fmt.Errorf("%w; --base URL checks another mirror", err)
	}
	if err != nil {
		return err
	}
	m := f.Manifest
	fmt.Printf("  manifest  %s%s/release.json\n", f.Base, release.DLPath)
	fmt.Printf("  signed by %s (release key, namespace %s)\n", f.Signer, release.Namespace)
	fmt.Printf("  release   %s  commit %s  released %s  valid until %s\n",
		m.Version, shortCommit(m.Commit), m.ReleasedAt, m.ExpiresAt)
	if !slices.Equal(compiled, f.Modules) {
		fmt.Printf("  modules   this binary: %s\n", strings.Join(compiled, ","))
		fmt.Printf("            release %s: %s\n", variant, strings.Join(f.Modules, ","))
	}

	switch {
	case f.Cmp < 0:
		return fmt.Errorf("%w: the manifest at %s is %s and this binary is %s; not installing it (a mirror serving an older signed release is refused, not followed; --base URL checks another mirror)",
			release.ErrDowngrade, f.Base, m.Version, version.V)
	case f.Cmp == 0:
		fmt.Printf("✓ already at %s\n", m.Version)
		if !checkOnly {
			// A binary installed before installers kept a record gets one
			// here — if it is the release's binary. Beside a source build
			// of the same version the record would vouch for nothing, and
			// doctor would report the binary as replaced.
			switch ok, err := release.NamesBinary(m, exe); {
			case err != nil:
				fmt.Fprintf(os.Stderr, "  note: cannot read %s to compare it with the release (%v); no release record was written\n", exe, err)
			case !ok:
				fmt.Printf("  %s is not a binary of the signed %s release (a source build, or changed since it was installed); no release record was written\n", exe, m.Version)
			default:
				recordRelease(exe, f)
			}
		}
		return nil
	case checkOnly:
		fmt.Printf("→ %s is available; run `anet update` to install it\n", m.Version)
		return nil
	}

	fmt.Printf("  asset     %s.gz (%d bytes)\n", f.AssetName, f.Asset.GzSize)
	if err := u.Apply(ctx, f); err != nil {
		return fmt.Errorf("%w (nothing was changed: %s is still %s)", err, exe, version.V)
	}
	fmt.Printf("✓ updated %s: %s → %s (sha256 of .gz and binary match the signed manifest)\n", exe, version.V, m.Version)
	recordRelease(exe, f)
	fmt.Println("  Daemons already running keep the old binary until restarted:")
	fmt.Println("    anet stop --all && anet up --all")
	return nil
}

// recordRelease keeps the verified manifest and its signature beside the
// binary (<binary>.release.json and .sig), where `anet doctor` verifies
// them again and compares the binary with them. Failing to write them does
// not undo an update that has happened; it is said, and doctor then
// reports the signature as unknown.
func recordRelease(exe string, f *release.Found) {
	if err := release.SaveInstalled(exe, f.Raw, f.Sig); err != nil {
		fmt.Fprintf(os.Stderr, "  note: the release record was not written beside %s (%v); `anet doctor` will report the signature as unknown\n", exe, err)
	}
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
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
//
// `anet update --verify-dir DIR [--installed VERSION]` is the other mode:
// see runVerifyDir.
func runUpdate(rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
		return fmt.Errorf("anet update takes no arguments (got %q)", strings.Join(pos, " "))
	}
	_, verifyDir := flags["verify-dir"]
	_, installed := flags["installed"]
	if verifyDir || installed {
		return runVerifyDir(flags)
	}
	_, checkOnly := flags["check"]

	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		return fmt.Errorf("anet update replaces the running binary in place, which %s does not allow; download the new release by hand", runtime.GOOS)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find this binary: %w", err)
	}
	if exe, err = resolveExe(exe); err != nil {
		return fmt.Errorf("cannot resolve this binary: %w", err)
	}
	// Binaries that an earlier update moved aside (Windows only; a no-op
	// elsewhere). One still in use by a running daemon stays until the
	// next run.
	release.SweepAside(exe)

	// The variant is the name build-release.sh stamped into this binary
	// (version.Variant), and only the release of that name is looked at.
	// A binary without the stamp was built from source: it is not updated,
	// and nothing is downloaded (update_variant_unknown).
	compiled := module.Compiled()
	variant := version.Variant
	if variant == "" {
		return refuseUpdate("update_variant_unknown", a2ashape.Args{})
	}

	// The npm mirrors of this build come after the project's own hosts, in
	// the same order as in install.sh.
	bases := append(append([]string{}, release.DefaultBases...), release.MirrorBases(variant, runtime.GOOS, runtime.GOARCH)...)
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
	if errors.Is(err, release.ErrNoVariant) {
		return refuseUpdate("update_variant_unknown.mismatch", mismatchArgs(variant, compiled))
	}
	if errors.Is(err, release.ErrExpired) {
		return fmt.Errorf("%w; --base URL checks another mirror", err)
	}
	if err != nil {
		return err
	}
	// "The default build never turns into one that can execute commands":
	// the additive modules of this binary must be those of the target.
	// Which subtractive modules differ does not matter (a later default may
	// gain or lose one); the downloaded binary is probed against the
	// manifest in Apply.
	if additiveMismatch(compiled, f.Modules) {
		return refuseUpdate("update_variant_unknown.mismatch", mismatchArgs(variant, compiled))
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
		if errors.Is(err, release.ErrReplaceIncomplete) {
			return err
		}
		return fmt.Errorf("%w (nothing was changed: %s is still %s)", err, exe, version.V)
	}
	fmt.Printf("✓ updated %s: %s → %s (sha256 of .gz and binary match the signed manifest)\n", exe, version.V, m.Version)
	recordRelease(exe, f)
	fmt.Println("  Daemons already running keep the old binary until restarted:")
	printRestartHint(os.Stdout)
	return nil
}

// verifyTrust is the trust --verify-dir checks a release with: the release
// key compiled into this binary. A test replaces it with a test key.
var verifyTrust = release.DefaultTrust

// runVerifyDir is `anet update --verify-dir DIR [--installed VERSION]`.
//
// It fetches nothing and writes nothing. It verifies DIR/release.json and
// its signature with the release key compiled into this binary, and then
// that this binary is the release's build of its own variant for this
// platform: its sha256 and size, its version and its module set (see
// release.VerifyStaged). With --installed, a release older than VERSION is
// refused. On success it prints one JSON line (release.Verified) and
// returns nil; otherwise it prints the reason on stderr and ends with
// status 1.
//
// install.ps1 runs it on the binary it has staged, after checking the
// binary against the SHA-256 values pinned in the signed script: PowerShell
// on the oldest supported Windows can verify neither Ed25519 nor an SSH
// signature, and this binary's verifier is the one `anet update` uses.
func runVerifyDir(flags map[string]string) error {
	const usage = "anet update --verify-dir <dir> [--installed <version>]"
	dir, ok := flags["verify-dir"]
	if !ok || dir == "" || dir == "true" {
		return usageError{usage}
	}
	installed, given := flags["installed"]
	if given && (installed == "" || installed == "true") {
		return usageError{usage}
	}
	for f := range flags {
		if f != "verify-dir" && f != "installed" {
			return usageError{usage}
		}
	}
	fail := func(err error) error {
		fmt.Fprintf(errW(), "anet update --verify-dir: %v\n", err)
		return errExit1
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(fmt.Errorf("cannot find this binary: %w", err))
	}
	self := release.Self{
		Exe:     exe,
		Version: version.V,
		Variant: version.Variant,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Modules: module.Compiled(),
	}
	v, err := release.VerifyStaged(dir, verifyTrust(), time.Now(), self, installed)
	if err != nil {
		return fail(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(outW(), "%s\n", b)
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

// additiveMismatch is true when the additive modules (shell) this binary has
// are not the ones the target build has. Only these decide whether a release
// is a build this binary may turn into.
func additiveMismatch(compiled, target []string) bool {
	return !slices.Equal(additiveOf(compiled), additiveOf(target))
}

// additiveOf is the modules of l that a build tag adds (shell), sorted.
func additiveOf(l []string) []string {
	var out []string
	for _, k := range module.Known() {
		if k.Additive && slices.Contains(l, k.Name) {
			out = append(out, k.Name)
		}
	}
	slices.Sort(out)
	return out
}

func mismatchArgs(variant string, compiled []string) a2ashape.Args {
	mods := strings.Join(compiled, ",")
	if mods == "" {
		mods = "(none)"
	}
	return a2ashape.Args{"variant": variant, "modules": mods}
}

// refuseUpdate says why anet update downloads nothing and ends with status 1.
func refuseUpdate(key string, args a2ashape.Args) error {
	lang := a2ashape.Lang()
	sayRefusal(os.Stderr, key, lang, args)
	return errExit1
}

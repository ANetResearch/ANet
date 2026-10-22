//go:build !no_mcp

package agentwire

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The plugins of every ecosystem are generated from one set of sources by
// deploy/plugins/build.py. The generated tree deploy/plugins/dist is committed,
// so that a marketplace can name a subdirectory of the ANet repository and so
// that the tree can be the root of a mirror repository. Everything below
// checks the sources and the committed tree; nothing here is specific to one
// target, so that a new target module needs no change in this file.

const pluginsDir = "../../deploy/plugins"

func requirePython(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to run deploy/plugins/build.py")
	}
	return py
}

// runScript runs `sh <script> <args>` with HOME set to home, PATH set to path
// and the extra environment env, and returns its output and exit status. The
// automatic installation of anet by the MCP launcher is turned off: it is
// tested with a fake installer by deploy/plugins/tests/test_autoinstall.py, and
// nothing here may download.
func runScript(t *testing.T, script, home, path string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{script}, args...)...)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + path, "ANET_PLUGIN_NO_INSTALL=1"}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// The plugins are published once and do not change with a release of anet:
// their skills and commands defer to the guide of the installed anet (the
// MCP tool guide, `anet guide <topic>`). Every skill and command of every
// generated target names the guide, and every topic that one of them names
// exists in this release.
var pluginTopicRe = regexp.MustCompile("(?:guide ([a-z]+)`|topic `([a-z]+)`)")

func TestPluginsDeferToTheInstalledGuide(t *testing.T) {
	n := 0
	err := filepath.WalkDir(filepath.Join(pluginsDir, "dist"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := filepath.ToSlash(p)
		if !strings.Contains(rel, "/skills/") && !strings.Contains(rel, "/commands/") {
			return nil
		}
		if !strings.HasSuffix(rel, ".md") && !strings.HasSuffix(rel, ".toml") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(b)
		n++
		if !strings.Contains(text, "guide") {
			t.Errorf("%s does not name the guide of the installed anet", rel)
		}
		for _, m := range pluginTopicRe.FindAllStringSubmatch(text, -1) {
			topic := m[1] + m[2]
			if _, ok := Guide(topic); !ok {
				t.Errorf("%s names the guide topic %q, which this release does not have", rel, topic)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Errorf("found %d skills and commands below deploy/plugins/dist", n)
	}
	for _, topic := range []string{"overview", "setup", "status"} {
		if _, ok := Guide(topic); !ok {
			t.Errorf("the permanent guide topic %q is missing", topic)
		}
	}
}

// The committed tree equals a fresh generation.
func TestPluginDistIsCurrent(t *testing.T) {
	py := requirePython(t)
	cmd := exec.Command(py, "-I", filepath.Join(pluginsDir, "build.py"), "--check")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy/plugins/dist is not current:\n%s", out)
	}
}

// The catalogues that Codex and Cursor read from the root of the ANet repository
// equal the files kept with the generator sources.
func TestPluginRepositoryRootCatalogues(t *testing.T) {
	for _, c := range []struct{ src, root string }{
		{"src/codex/repo-root-marketplace.json", "../../.agents/plugins/marketplace.json"},
		{"src/cursor/repo-root-marketplace.json", "../../.cursor-plugin/marketplace.json"},
	} {
		want, err := os.ReadFile(filepath.Join(pluginsDir, c.src))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(c.root)
		if err != nil {
			t.Errorf("%s: %v", c.root, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from deploy/plugins/%s; copy the file again", c.root, c.src)
		}
	}
}

// The generator core: templates, archives, target checks.
func TestPluginGeneratorUnit(t *testing.T) {
	py := requirePython(t)
	if out, err := exec.Command(py, "-I", filepath.Join(pluginsDir, "test_generator.py")).CombinedOutput(); err != nil {
		t.Fatalf("deploy/plugins/test_generator.py: %v\n%s", err, out)
	}
}

// The test module of every target (deploy/plugins/tests/test_<key>.py): manifest,
// host-specific wording, runner behaviour with a fake anet executable, and the
// catalogues. They run without the host program; set the variable that a module
// documents (for example ANET_CLAUDE_CLI) to add the checks that call the host.
func TestPluginTargetTests(t *testing.T) {
	py := requirePython(t)
	files, err := filepath.Glob(filepath.Join(pluginsDir, "tests", "test_*.py"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no target tests below %s/tests: %v", pluginsDir, err)
	}
	for _, f := range files {
		f := f
		t.Run(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "test_"), ".py"), func(t *testing.T) {
			if out, err := exec.Command(py, "-I", f).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", filepath.Base(f), err, out)
			}
		})
	}
}

// Every target builds into an empty directory, and the website catalogues build.
func TestPluginsBuild(t *testing.T) {
	py := requirePython(t)
	for _, args := range [][]string{{"--out"}, {"--site"}} {
		dir := t.TempDir()
		cmd := exec.Command(py, "-I", filepath.Join(pluginsDir, "build.py"), args[0], dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build.py %s: %v\n%s", args[0], err, out)
		}
	}
	out, err := exec.Command(py, "-I", filepath.Join(pluginsDir, "build.py"), "--list").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "zcode\t") {
		t.Errorf("--list: %v, %q", err, out)
	}
}

// The runner of every target applies the rules that the plugins share: the
// MCP launcher takes its settings from the environment, an invalid anetPath is
// an error and no other copy is used, PATH and the install locations are
// searched, and a missing installation points to the setup skill.
func TestPluginRunnersShareTheRules(t *testing.T) {
	var runners []string
	err := filepath.WalkDir(filepath.Join(pluginsDir, "dist"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "anet-run.sh" {
			runners = append(runners, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) == 0 {
		t.Fatal("no anet-run.sh found below deploy/plugins/dist")
	}
	if hostHasAnet(t) {
		t.Skip("an anet executable is installed in a system directory that the runner searches")
	}
	sysPath := "/usr/bin:/bin"
	for _, run := range runners {
		run, _ := filepath.Abs(run)
		mcp := filepath.Join(filepath.Dir(run), "anet-mcp.sh")
		name := strings.TrimPrefix(run, filepath.Join(mustAbs(t, pluginsDir), "dist")+string(filepath.Separator))
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			bin := fakeAnet(t, filepath.Join(t.TempDir(), "custom"))
			local := fakeAnet(t, filepath.Join(home, ".local", "bin"))

			out, code := runScript(t, mcp, home, sysPath, []string{"ANET_PLUGIN_BIN=" + bin, "ANET_PLUGIN_IDENTITY=work"})
			if code != 0 || strings.TrimSpace(out) != bin+" --id work mcp" {
				t.Errorf("anet-mcp.sh: status %d, output %q; want %q", code, out, bin+" --id work mcp")
			}
			// The launcher without an extension, which a host starts by its path, is executable and
			// does the same.
			direct := strings.TrimSuffix(mcp, ".sh")
			if fi, err := os.Stat(direct); err != nil || fi.Mode()&0o100 == 0 {
				t.Errorf("%s is missing or not executable: %v", direct, err)
			} else {
				cmd := exec.Command(direct)
				cmd.Env = []string{"HOME=" + home, "PATH=" + sysPath, "ANET_PLUGIN_BIN=" + bin, "ANET_PLUGIN_IDENTITY=work", "ANET_PLUGIN_NO_INSTALL=1"}
				if got, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(got)) != bin+" --id work mcp" {
					t.Errorf("%s: %v, output %q", direct, err, got)
				}
			}
			missing := filepath.Join(home, "nowhere", "anet")
			out, code = runScript(t, mcp, home, sysPath, []string{"ANET_PLUGIN_BIN=" + missing})
			if code != 1 || !strings.Contains(out, "anetPath") || strings.Contains(out, " mcp") {
				t.Errorf("anet-mcp.sh with a missing anetPath: status %d, output %q", code, out)
			}
			// A relative anetPath would depend on the working directory: it is refused.
			out, code = runScript(t, mcp, home, sysPath, []string{"ANET_PLUGIN_BIN=./anet"})
			if code != 1 || !strings.Contains(out, "absolute path") || strings.Contains(out, " mcp") {
				t.Errorf("anet-mcp.sh with a relative anetPath: status %d, output %q", code, out)
			}
			out, code = runScript(t, run, home, sysPath, nil, "status")
			if code != 0 || strings.TrimSpace(out) != local+" status" {
				t.Errorf("anet-run.sh status: status %d, output %q; want %q", code, out, local+" status")
			}
			out, code = runScript(t, run, home, sysPath, nil, "--resolve")
			for _, want := range []string{"executable: " + local, "identity: default", "runner: sh "} {
				if code != 0 || !strings.Contains(out, want) {
					t.Errorf("--resolve: status %d, output %q lacks %q", code, out, want)
				}
			}
			// A directory named anet on PATH is not an executable.
			shadow := t.TempDir()
			if err := os.Mkdir(filepath.Join(shadow, "anet"), 0o755); err != nil {
				t.Fatal(err)
			}
			out, code = runScript(t, run, home, shadow+":"+sysPath, nil, "status")
			if code != 0 || strings.TrimSpace(out) != local+" status" {
				t.Errorf("a directory named anet on PATH: status %d, output %q; want %q", code, out, local+" status")
			}
			// The runner line of --resolve is a command: it stays valid for a path with a space and a quote.
			odd := filepath.Join(t.TempDir(), "Application Support", "it's")
			if err := os.MkdirAll(odd, 0o755); err != nil {
				t.Fatal(err)
			}
			src, err := os.ReadFile(run)
			if err != nil {
				t.Fatal(err)
			}
			oddRun := filepath.Join(odd, "anet-run.sh")
			if err := os.WriteFile(oddRun, src, 0o644); err != nil {
				t.Fatal(err)
			}
			out, _ = runScript(t, oddRun, home, sysPath, nil, "--resolve")
			var line string
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "runner: ") {
					line = strings.TrimPrefix(l, "runner: ")
				}
			}
			if line == "" {
				t.Fatalf("--resolve printed no runner line: %q", out)
			}
			cmd := exec.Command("/bin/sh", "-c", line+" --resolve")
			cmd.Env = []string{"HOME=" + home, "PATH=" + sysPath}
			if got, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(got), "identity: default") {
				t.Errorf("runner line %q does not run: %v, %q", line, err, got)
			}
			out, code = runScript(t, run, t.TempDir(), sysPath, nil, "status")
			if code != 1 || !strings.Contains(out, "anet-setup") {
				t.Errorf("nothing installed: status %d, output %q", code, out)
			}
		})
	}
}

// hostHasAnet reports whether an anet executable sits in a directory that the
// runner searches and that a test cannot replace; the tests that assume none
// is installed are skipped then, because they would run the real one.
func hostHasAnet(t *testing.T) bool {
	t.Helper()
	for _, d := range []string{"/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin"} {
		if _, err := os.Stat(filepath.Join(d, "anet")); err == nil {
			return true
		}
	}
	return false
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

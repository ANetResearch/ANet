//go:build shell

// Package shell offers operator-approved commands as ANet capabilities.
//
// The case it answers: somebody has a fleet of development machines — a
// board on a bench, a builder in a rack — and wants an agent to run
// things on them and read back what happened. Until now the daemon could
// offer devices through ANetLink and HTTP services through the service
// module, and nothing that touches the machine it runs on.
//
// This is the one module in the suite that executes on the host, so it is
// built to be difficult to leave open by accident:
//
//   - It is the one module in the suite with an ADDITIVE build tag. Every
//     other optional subsystem is in by default and subtracted with
//     `no_<name>`; this one is absent unless somebody builds with
//     `-tags shell`. The difference is deliberate: a tag you must
//     remember to remove protects nobody who did not know to remove it,
//     and the population that would be harmed by shipping this by
//     accident is exactly the population that has never heard of it.
//     It must ALSO be configured before it does anything, so a build
//     that carries it and was never set up still cannot run a command.
//   - The caller allowlist is empty by default and an empty allowlist
//     refuses everyone. A capability that accepts input from the network
//     and executes it has to be closed until somebody opens it, not the
//     other way round. A call carrying no caller identity at all is
//     refused too, unless the operator set allow_local: the zero value of
//     that field must not be the one that admits.
//   - Only commands the operator named in the config can run. Arbitrary
//     commands are a separate switch, off by default, so "the module is
//     configured" and "a root shell is open" are not the same event.
//   - Every invocation goes on this node's evidence chain before its
//     output is returned: who asked, what ran, what it exited with. An
//     execution nobody can point back at is one nobody can dispute.
//
// What it deliberately does NOT do: raise privilege. The command runs as
// whatever user the daemon runs as. A daemon started as root can run root
// commands, which is the operator's decision made at install time and
// visible in the unit file — not something this module can grant.
package shell

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/module"
)

const name = "shell"

// Capability ids. Named commands are addressed by their own id so a card
// advertises what this node will actually do — `shell.run@reboot` rather
// than a bare "it can run things".
const (
	// CapList reports which commands this node offers, and how many
	// callers it admits. An operator can ask a machine what it thinks it
	// is allowed to do rather than reading its config over ssh; a remote
	// caller does not get the roster itself.
	CapList = "shell.list"
	// CapExec runs an arbitrary command. Present only when the operator
	// set allow_arbitrary.
	CapExec = "shell.exec"
)

// runPrefix is the capability id prefix for a named command: the command
// "reboot" is offered as "shell.run@reboot".
const runPrefix = "shell.run@"

// Defaults. Each is a limit that must exist before an operator remembers
// to set it, because the failure without it is a machine that is wedged
// or a reply that never ends.
const (
	defaultTimeout = 30 * time.Second
	maxTimeout     = 30 * time.Minute
	// defaultMaxOutput bounds what comes back. A command that prints a
	// gigabyte would otherwise be carried through the hub relay and into
	// the caller's evidence chain.
	defaultMaxOutput = 64 << 10
	maxMaxOutput     = 4 << 20
)

func init() { module.Register(name, newModule) }

// newModule builds the module from its config block, or builds nothing.
//
// Two separate decisions, and this is where the second one is enforced:
// a build carrying the module still runs no commands until an operator
// writes a config block for it. Being present and being armed are not
// the same state.
func newModule(raw []byte) (module.Module, error) {
	if len(raw) == 0 {
		return nil, nil // compiled in, not configured
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("shell: config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Module{cfg: cfg}, nil
}

// Command is one thing the operator has decided this node may do.
type Command struct {
	// Run is the command line, executed through the shell so an operator
	// can write what they would type. That is a deliberate trade: the
	// operator already chose to allow this exact line, and refusing them
	// a pipe would push them to wrap it in a script and lose the
	// visibility this config gives.
	Run string `json:"run"`
	// Description is what an agent reading the card learns about it.
	Description string `json:"description,omitempty"`
	// Args, when true, appends the caller's args.argv to the command line
	// after shell-quoting each element. Off by default: a command that
	// takes no input from the network is a smaller thing to get wrong.
	Args bool `json:"args,omitempty"`
	// Dir runs it somewhere other than the daemon's working directory.
	Dir string `json:"dir,omitempty"`
	// TimeoutS overrides the module default for this one command.
	TimeoutS int `json:"timeout_s,omitempty"`
}

// Config is the module's block in config.json.
type Config struct {
	// Commands the operator has approved, by name.
	Commands map[string]Command `json:"commands,omitempty"`
	// Allow lists the AIDs that may call. EMPTY MEANS NOBODY.
	//
	// The inline list is static: changing it needs a daemon restart. For
	// revocation that takes effect on the next call, use AllowFile.
	Allow []string `json:"allow,omitempty"`
	// AllowFile is a file of AIDs, one per line, re-read before every
	// call. When set it REPLACES Allow, and editing it revokes without a
	// restart — which is what revocation has to be, because the moment
	// somebody wants to revoke access is not a moment they want to be
	// restarting the thing they are revoking it on.
	//
	// Blank lines and # comments are ignored, so an operator can write
	// down who each line is.
	AllowFile string `json:"allow_file,omitempty"`
	// AllowArbitrary turns on shell.exec — any command the caller sends.
	//
	// Separate from configuring the module at all, so that "I set up a
	// couple of maintenance commands" and "I opened a shell on this box"
	// remain two decisions. On a development board this is the point; on
	// anything else it very likely is not.
	// AllowLocal admits calls that carry no caller identity.
	//
	// Defaults to FALSE, which is the opposite of what convenience would
	// suggest and the same as every other field here: the zero value
	// denies. An empty CallerAID means "this call arrived without a
	// verified identity", and today the only thing that produces one is a
	// test. If a local surface is added later and a call site forgets to
	// populate the field, the difference between admitting and denying by
	// default is the difference between a silent bypass of the whole
	// allowlist and a call that visibly refuses.
	AllowLocal bool `json:"allow_local,omitempty"`

	AllowArbitrary bool `json:"allow_arbitrary,omitempty"`
	// TimeoutS bounds one command. Zero uses the default.
	TimeoutS int `json:"timeout_s,omitempty"`
	// MaxOutputBytes caps captured output. Zero uses the default.
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
	// Shell overrides the interpreter. Default /bin/sh.
	Shell string `json:"shell,omitempty"`
}

func (c Config) validate() error {
	if len(c.Commands) == 0 && !c.AllowArbitrary {
		return fmt.Errorf("shell: no commands declared and allow_arbitrary is off — " +
			"this module would offer nothing")
	}
	for n, cmd := range c.Commands {
		if n == "" {
			return fmt.Errorf("shell: a command with no name")
		}
		if strings.ContainsAny(n, " \t@/") {
			return fmt.Errorf("shell: command name %q may not contain space, @ or /"+
				" — it becomes part of a capability id", n)
		}
		if strings.TrimSpace(cmd.Run) == "" {
			return fmt.Errorf("shell: command %q has nothing to run", n)
		}
	}
	if c.TimeoutS < 0 || time.Duration(c.TimeoutS)*time.Second > maxTimeout {
		return fmt.Errorf("shell: timeout_s must be between 0 and %d", int(maxTimeout.Seconds()))
	}
	if c.MaxOutputBytes < 0 || c.MaxOutputBytes > maxMaxOutput {
		return fmt.Errorf("shell: max_output_bytes must be between 0 and %d", maxMaxOutput)
	}
	if c.AllowFile != "" && len(c.Allow) > 0 {
		return fmt.Errorf("shell: set either allow or allow_file, not both — " +
			"two sources of who may call is one more than can be right")
	}
	return nil
}

// Module offers the approved commands.
type Module struct {
	cfg  Config
	host module.Host
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	m.host = h
	// Said at start, loudly, because a module that executes commands and
	// admits nobody is almost always a half-finished setup rather than an
	// intention — and the operator finds out otherwise from a call that
	// was refused for reasons they have to go and read.
	if who, err := m.allowed(); err != nil {
		return fmt.Errorf("shell: allow_file: %w", err)
	} else if len(who) == 0 {
		fmt.Fprintln(os.Stderr,
			"anet shell: no caller is allowed — set allow or allow_file, "+
				"or this module will refuse every call")
	}
	if m.cfg.AllowArbitrary {
		fmt.Fprintln(os.Stderr,
			"anet shell: allow_arbitrary is ON — an allowed caller can run "+
				"any command as the user this daemon runs as")
	}
	return h.Providers().Register(ctx, &shellProvider{m: m})
}

func (m *Module) Stop(context.Context) error { return nil }

// allowed is the current caller allowlist.
//
// Read fresh on every call when it comes from a file: an allowlist that
// was loaded at startup can only be changed by a restart, and revocation
// that needs a restart is revocation the operator will postpone.
func (m *Module) allowed() (map[string]bool, error) {
	out := map[string]bool{}
	if m.cfg.AllowFile == "" {
		for _, a := range m.cfg.Allow {
			if a = strings.TrimSpace(a); a != "" {
				out[a] = true
			}
		}
		return out, nil
	}
	f, err := os.Open(m.cfg.AllowFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Absent is empty, not an error: deleting the file is a
			// legitimate way to revoke everyone at once, and it should
			// behave like an empty file rather than like a broken node.
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line != "" {
			out[line] = true
		}
	}
	return out, sc.Err()
}

func (m *Module) timeout(c Command) time.Duration {
	if c.TimeoutS > 0 {
		return time.Duration(c.TimeoutS) * time.Second
	}
	if m.cfg.TimeoutS > 0 {
		return time.Duration(m.cfg.TimeoutS) * time.Second
	}
	return defaultTimeout
}

func (m *Module) maxOutput() int {
	if m.cfg.MaxOutputBytes > 0 {
		return m.cfg.MaxOutputBytes
	}
	return defaultMaxOutput
}

func (m *Module) shell() string {
	if m.cfg.Shell != "" {
		return m.cfg.Shell
	}
	return "/bin/sh"
}

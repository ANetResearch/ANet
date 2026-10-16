//go:build !no_preview

package preview

// module.go is the preview module (11-modularity §3.2, MOD-6): the
// module.Module that holds the engine (previews.go), starts it with the
// seam of its Host, and contributes the preview declaration to the cards and
// the preview member to /status. `-tags no_preview` leaves this file, and the
// package with it, out of the binary: nothing registers, and the kernel
// answers for a node without previews (module_absent, frames dropped and
// counted, no declaration).
//
// The Host this module gets is granted PreviewSeam and nothing else: no
// state directory, no evidence, no task seam, no providers. What it cannot
// reach it cannot store (SI-73).

import (
	"context"
	"fmt"
	"sync"

	"github.com/ANetResearch/ANet/module"
)

func init() {
	module.Register(Name, func([]byte) (module.Module, error) { return &Module{}, nil })
}

// Name is the module's name: its build tag without the prefix.
const Name = "preview"

// extensionDescription is the declaration's text on a card.
const extensionDescription = "This agent can send a preview of its reply while it is being written; " +
	"a client that activates the extension receives it as updates of the anet.reply artifact."

// Module is the preview module. Once started it is the node's
// module.Previewer: the methods of the engine are its own.
type Module struct {
	*Previews
	seam module.PreviewSeam

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var (
	_ module.Module            = (*Module)(nil)
	_ module.Previewer         = (*Module)(nil)
	_ module.CardContributor   = (*Module)(nil)
	_ module.StatusContributor = (*Module)(nil)
)

// Name implements module.Module.
func (*Module) Name() string { return Name }

// Start takes the seam out of the Host (a Host that lacks it is a node the
// name table did not grant it to, or one where previews are not available:
// the module does not start) and runs the coalescer until Stop.
func (m *Module) Start(ctx context.Context, h module.Host) error {
	ph, ok := h.(module.PreviewHost)
	if !ok {
		return fmt.Errorf("preview: the Host grants no PreviewHost")
	}
	seam, ok := ph.PreviewSeam()
	if !ok {
		return fmt.Errorf("preview: the Host has no preview seam")
	}
	m.seam = seam
	m.Previews = New(seam.NowMS, nil, func(string) { seam.Dropped() })
	ctx, m.cancel = context.WithCancel(ctx)
	m.Previews.send = func(f Frame) { HandOver(ctx, seam, f) }
	m.Previews.SetSeam(seam.Mode, seam.Published)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.Previews.Run(ctx, m.spawn(ctx))
	}()
	return nil
}

// spawn runs each hand-over on its own, so that a slow hub holds up no other
// task, and Stop waits for them.
func (m *Module) spawn(ctx context.Context) func(func()) {
	return func(f func()) {
		if ctx.Err() != nil {
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			f()
		}()
	}
}

// Stop ends the coalescer and waits for the hand-overs under way; after it
// returns nothing is sent.
func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	return nil
}

// CardExtensions implements module.CardContributor: the preview extension,
// declared when stream_preview is not off (the same for "on" and
// "with_reasoning": reasoning does not go over A2A, CUX-P17); it is not
// required and has no params (CUX-P30). The public card and the extended
// card alike.
func (m *Module) CardExtensions(module.CardContext) []map[string]any {
	if m.seam == nil || normalPreviewMode(m.seam.Mode()) == "" {
		return nil
	}
	return []map[string]any{{"uri": module.ExtPreviewURI, "description": extensionDescription}}
}

// SI5 implements module.StatusContributor: the module holds no value the
// SI-5 map checks.
func (*Module) SI5() map[string]any { return nil }

// CardInterfaces implements module.CardContributor: none.
func (*Module) CardInterfaces(module.CardContext) []map[string]any { return nil }

// StatusMember implements module.StatusContributor: the preview member of
// /status (the setting), which the status summary's preview line reads.
func (m *Module) StatusMember() (string, any) {
	if m.seam == nil {
		return "preview", nil
	}
	return "preview", map[string]any{"mode": m.seam.Mode()}
}

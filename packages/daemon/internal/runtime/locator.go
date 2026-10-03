package runtime

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The runtimes a locator can name. A locator's runtime word is the discriminator: it says which
// backend member carries the rest of the address, and which runtime is able to act on it.
const (
	RuntimeTmux    = "tmux"
	RuntimeSandbox = "sandbox"
)

// Locator is where one agent's process is, in enough detail to find it again after a daemon
// restart: which backend runs it, which claim it belongs to, which incarnation of that claim it
// is, and the backend's own address for it.
//
// It is nested rather than flat, and marshalled by the standard library rather than a hand-written
// marshaller: the backend member is a Go value the backend owns, so adding a runtime adds a
// struct and a member, never a case in a shared codec.
//
// Incarnation is what makes the locator an address of a *process* and not of a slot. Under tmux
// it is the pane's own pid and that process's `/proc/<pid>/stat` start ticks ("<pid>:<ticks>"),
// because a pane id is reissued by a recreated tmux server and a pid is reissued by the kernel;
// under a sandbox it is the pod uid. Two observations of the same claim at different
// incarnations are two different processes, which is how a stale event is fenced.
type Locator struct {
	Runtime     string          `json:"runtime"`
	Claim       claim.Token     `json:"claim"`
	Incarnation string          `json:"incarnation"`
	Tmux        *TmuxLocator    `json:"tmux,omitempty"`
	Sandbox     *SandboxLocator `json:"sandbox,omitempty"`
}

// TmuxLocator is a pane on the daemon's private tmux server: the window it was opened in and the
// pane itself.
type TmuxLocator struct {
	Window string `json:"window"`
	Pane   string `json:"pane"`
}

// SandboxLocator is one agent's sandbox in a cluster. Stage 4 is what fills one in; it is
// declared now so the shape a locator is stored and validated in does not change when it does.
type SandboxLocator struct {
	Namespace string `json:"namespace"`
	// Name is the Agent Sandbox object's name — the sandbox, not the pod behind it, which the
	// cluster may replace. The locator's incarnation is that pod's uid.
	Name string `json:"name"`
}

// Validate is the refusal `encoding/json` cannot make. Unmarshalling leaves an absent member at
// its zero value and reports nothing, so a locator is checked wherever one is read back — the
// store load and the API projection — and a record nothing could be acted on is named out loud
// rather than carried as a process the daemon believes in.
func (l Locator) Validate() error {
	if l.Runtime == "" {
		return fmt.Errorf("locator: no runtime")
	}
	if l.Claim == "" {
		return fmt.Errorf("locator (runtime %q): no claim", l.Runtime)
	}
	if l.Incarnation == "" {
		return fmt.Errorf("locator %s (runtime %q): no incarnation", l.Claim, l.Runtime)
	}
	if l.Tmux != nil && l.Sandbox != nil {
		return fmt.Errorf(
			"locator %s: carries both a tmux and a sandbox member; exactly one backend member belongs to a locator",
			l.Claim,
		)
	}
	switch l.Runtime {
	case RuntimeTmux:
		if l.Tmux == nil {
			return fmt.Errorf(`locator %s: runtime "tmux" with no tmux member`, l.Claim)
		}
		if l.Tmux.Window == "" {
			return fmt.Errorf("locator %s: tmux member with no window", l.Claim)
		}
		if l.Tmux.Pane == "" {
			return fmt.Errorf("locator %s: tmux member with no pane", l.Claim)
		}
	case RuntimeSandbox:
		if l.Sandbox == nil {
			return fmt.Errorf(`locator %s: runtime "sandbox" with no sandbox member`, l.Claim)
		}
		if l.Sandbox.Namespace == "" {
			return fmt.Errorf("locator %s: sandbox member with no namespace", l.Claim)
		}
		if l.Sandbox.Name == "" {
			return fmt.Errorf("locator %s: sandbox member with no name", l.Claim)
		}
	default:
		return fmt.Errorf("locator %s: no runtime answers to %q", l.Claim, l.Runtime)
	}
	return nil
}

package claim

import (
	"fmt"
	"net/http"
	"time"
)

// RegisterRequest is the first call an agent makes: the Oh My Pi plugin reads the boot token off
// the 0600 file the daemon named on its pane and posts it with the session it actually became.
// `OmpSessionFile` is the transcript path that session persisted — the one value `--resume`
// takes, so a relaunch of this claim resumes this agent and no other. `PluginContract` is the
// plugin's `legion.daemonApiVersion`; a daemon that speaks another one refuses the boot.
type RegisterRequest struct {
	BootToken      string `json:"bootToken"`
	SessionID      string `json:"sessionId"`
	OmpSessionFile string `json:"ompSessionFile"`
	AgentID        string `json:"agentId"`
	PluginContract int    `json:"pluginContract"`
}

// RegisterResponse is what the agent learns about itself: the claim it holds, where it holds it,
// the secret its later calls authenticate with, and the task agents its role's prompts dispatch,
// which its capability report measures (LEGION-663). The secret is minted per registration and the
// capability hash is persisted before the response is written, so a daemon that restarts one
// instruction later still recognises this agent. A session that registers with the controller
// capability holds no claim and gets api.ControllerRegisterResponse instead.
type RegisterResponse struct {
	ClaimToken Token  `json:"claimToken"`
	Tree       string `json:"tree"`
	Issue      string `json:"issue"`
	Role       Role   `json:"role"`
	Generation uint64 `json:"generation"`
	Secret     string `json:"secret"`
	// PromptAgents is the sorted names of every task agent the role's prompts dispatch; never
	// null on the wire, so a role whose prompts name none answers an empty list.
	PromptAgents []string `json:"promptAgents"`
}

// ReadyRequest says the agent has finished booting and can be prompted. It is a separate call
// from the registration because registration is the plugin's first act and readiness is its last:
// the role claim on Envoy, the session attribution, and the tool gate all happen in between.
// Capabilities is what the session measured of the live capability rows while it booted
// (LEGION-663); the controller's ready carries none.
type ReadyRequest struct {
	ClaimToken   Token             `json:"claimToken"`
	SessionID    string            `json:"sessionId"`
	Secret       string            `json:"secret"`
	Generation   uint64            `json:"generation"`
	Capabilities *CapabilityReport `json:"capabilities,omitempty"`
}

// CapabilityReport is what a session measured of the live capability rows at boot (LEGION-663):
// when it measured, how long the measuring took, and one row per capability it checked. The
// daemon normalises the rows (capabilities.Normalize) before it keeps them.
type CapabilityReport struct {
	MeasuredAt time.Time       `json:"measuredAt"`
	ElapsedMs  int             `json:"elapsedMs"`
	Rows       []CapabilityRow `json:"rows"`
}

// CapabilityRow is one live row as the session measured it: the capability's name
// (capabilities.Name), whether the check passed, and the fact the check found either way.
type CapabilityRow struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ExitRequest is the agent reporting its own end, with the reason it ended. An exit the daemon
// did not ask for is still a fact about the claim, which is why the reason travels with it.
type ExitRequest struct {
	ClaimToken Token  `json:"claimToken"`
	SessionID  string `json:"sessionId"`
	Secret     string `json:"secret"`
	Generation uint64 `json:"generation"`
	Reason     string `json:"reason"`
}

// Refusal is one of the ways a claim route says no: the status it answers with and the sentence
// it puts in the body. Both halves are a contract with the plugin, which ends its process on a
// 403 or a 409 from the registration (`exitOnRegistrationRefusal` in
// `packages/pi-legion/extensions/legion.ts`) and logs the sentence for the operator, so neither
// can be reworded here alone. A Refusal is an `error`, comparable, and therefore matchable with
// `errors.Is` after a handler has wrapped it with the request it refused.
type Refusal struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (r Refusal) Error() string { return r.Message }

var (
	// InvalidBootToken: the token on the pane is unknown, already spent on another session, or
	// from a launch this claim has moved past.
	InvalidBootToken = Refusal{Status: http.StatusForbidden, Message: "Invalid boot token"}
	// StaleGeneration: the caller is an agent of a generation the claim has left behind — a pane
	// the daemon has already replaced, answering for a claim that has moved on.
	StaleGeneration = Refusal{Status: http.StatusConflict, Message: "Stale generation"}
	// SameAgentRefusal: a relaunch must resume the agent the claim recorded, so a registration
	// that arrives as a different session is refused rather than allowed to take the role over.
	// The plugin's exit rule and its changelog both quote the sentence verbatim.
	SameAgentRefusal = Refusal{
		Status:  http.StatusConflict,
		Message: "Worker respawn must resume the same agent session",
	}
	// InvalidSecret: a ready or an exit that does not carry the secret the claim's registration
	// was issued — or names a claim the daemon does not hold.
	InvalidSecret = Refusal{Status: http.StatusForbidden, Message: "Invalid session secret"}
)

// staleContract renders the opening sentence a stale-caller refusal shares: got is the contract
// the plugin declared, 0 when it declared none; want is the daemon's own (api.DaemonAPIVersion,
// which this package cannot import, so the caller names it). remedy is the clause that tells the
// caller what happens next — StalePluginContract's relaunch, StaleControllerContract's restart
// instruction. The sentence is a contract item — quoted in pi-legion's changelog and its docs —
// and may not be reworded here alone.
func staleContract(got, want int, remedy string) Refusal {
	if got == 0 {
		return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
			"pi-legion registered no daemon API contract; this daemon requires %d: %s", want, remedy)}
	}
	return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
		"pi-legion speaks daemon API contract %d; this daemon requires %d: %s", got, want, remedy)}
}

// StalePluginContract refuses a registration whose plugin speaks a contract this daemon does not:
// the daemon has already recorded it and is relaunching the process onto the current plugin
// (LEGION-164), so the plugin's exit on this 409 costs the claim nothing when the relaunch is its
// first for this contract, and is the charge itself when it is not (a plugin or an image the
// relaunch put the process on is itself behind this daemon).
func StalePluginContract(got, want int) Refusal {
	return staleContract(got, want, "the daemon is relaunching this process onto the current plugin")
}

// StaleControllerContract refuses a call from the operator's controller recorded at another
// contract across a daemon restart (LEGION-164): the operator's controller is not the daemon's to
// relaunch, so the sentence tells the operator to restart it themselves rather than promising a
// relaunch.
func StaleControllerContract(got, want int) Refusal {
	return staleContract(got, want, "run legion controller start from the release built with this daemon")
}

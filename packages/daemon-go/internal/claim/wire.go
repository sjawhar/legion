package claim

import "net/http"

// RegisterRequest is the first call an agent makes: the Oh My Pi plugin reads the boot token off
// the 0600 file the daemon named on its pane and posts it with the session it actually became.
// `OmpSessionFile` is the transcript path that session persisted — the one value `--resume`
// takes, so a relaunch of this claim resumes this agent and no other. `PluginContract` is the
// plugin's `legion.goDaemonApiVersion`; a daemon that speaks another one refuses the boot.
type RegisterRequest struct {
	BootToken      string `json:"bootToken"`
	SessionID      string `json:"sessionId"`
	OmpSessionFile string `json:"ompSessionFile"`
	AgentID        string `json:"agentId"`
	PluginContract int    `json:"pluginContract"`
}

// RegisterResponse is what the agent learns about itself: the claim it holds, where it holds it,
// and the secret its later calls authenticate with. The secret is minted per registration and the
// capability hash is persisted before the response is written, so a daemon that restarts one
// instruction later still recognises this agent.
type RegisterResponse struct {
	ClaimToken Token  `json:"claimToken"`
	Tree       string `json:"tree"`
	Issue      string `json:"issue"`
	Role       Role   `json:"role"`
	Generation uint64 `json:"generation"`
	Secret     string `json:"secret"`
}

// ReadyRequest says the agent has finished booting and can be prompted. It is a separate call
// from the registration because registration is the plugin's first act and readiness is its last:
// the role claim on Envoy, the session attribution, and the tool gate all happen in between.
type ReadyRequest struct {
	ClaimToken Token  `json:"claimToken"`
	SessionID  string `json:"sessionId"`
	Secret     string `json:"secret"`
	Generation uint64 `json:"generation"`
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
// `packages/pi-envoy/extensions/legion.ts`) and logs the sentence for the operator, so neither
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
	// The sentence is the shipped daemon's, verbatim (`packages/daemon/src/daemon/api/http.ts:20`):
	// the plugin's exit rule and its changelog both quote it.
	SameAgentRefusal = Refusal{
		Status:  http.StatusConflict,
		Message: "Worker respawn must resume the same agent session",
	}
)

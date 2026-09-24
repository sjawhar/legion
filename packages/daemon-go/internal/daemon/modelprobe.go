package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
)

// modelPrompt is the round trip's one message; any answer passes, since what is proved is who
// answered, not what.
const modelPrompt = "Reply with the single word ok."

// modelTurnTimeout bounds the round trip, well inside the daemon's per-attempt budget
// (slow_command_timeout_seconds, 300 s by default), which also has to cover the probe pod's node,
// image pull and launch probes.
const modelTurnTimeout = 90 * time.Second

// modelTurnOverlay is the `--config` overlay the round trip runs under. The profile's fallback
// chain would move a failed turn to another alias on the same gateway (Oh My Pi walks it on the
// first error of any kind), and the probe would judge that alias's answer; with fallback off the
// default alias's own answer, or its own error, ends the turn. Oh My Pi's retries are cut to two,
// each waited at most 5 s, so an overloaded or unreachable gateway ends the turn inside
// modelTurnTimeout as that alias's error.
const modelTurnOverlay = `retry:
  modelFallback: false
  maxRetries: 2
  maxDelayMs: 5000
`

// ModelRouteUnavailable is a round trip the gateway did not answer for a reason that says nothing
// about the image — overloaded, rate-limited, unreachable, or a turn cut off at modelTurnTimeout.
// `legion probe-image` exits bootprobe.TransientExit on it, so the daemon's probe Sandbox runs the
// probe again under its own retry rather than refusing the image.
type ModelRouteUnavailable struct{ Detail string }

func (e *ModelRouteUnavailable) Error() string { return e.Detail }

// turnAnswer is what Oh My Pi's `--mode json` stream says about an assistant message at its
// `message_end` event: who answered it, and how it ended.
type turnAnswer struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

// verifyModelRoute is the image's fourth probe, run when `legion probe-image` routed the profile
// through a gateway (modelroute.Install): one print-mode turn on the profile's default role, as a
// phase worker's Oh My Pi runs it — the profile's models, roles and pins, without the plugin, any
// tool, or a session, and without the fallback chain (modelTurnOverlay) — so it proves the route,
// the key and the profile together, where a direct HTTP call would prove the key and pass a broken
// profile. It runs once, bounded by modelTurnTimeout, and passes only when the turn was answered
// by g.model. A turn answered by any other provider or model (Oh My Pi reaching a model past the
// gateway), a profile with no usable model (the key command failing), the gateway refusing the key
// or the model (a 4xx other than 408, 409 and 429), or a turn that answered nothing are refusals.
// The gateway overloaded, rate-limited or unreachable, Oh My Pi dying before it answered, or a turn
// cut off are a ModelRouteUnavailable, which the daemon's retry waits out.
func (g pluginGate) verifyModelRoute(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "legion-model-probe-")
	if err != nil {
		return fmt.Errorf("boot gate: create the model round trip's overlay directory: %w", err)
	}
	defer os.RemoveAll(dir)
	overlay := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(overlay, []byte(modelTurnOverlay), 0o600); err != nil {
		return fmt.Errorf("boot gate: write the model round trip's overlay: %w", err)
	}
	launch := tmux.WithOmpLaunchPrefix(g.prefix, g.invocation)
	script := `exec ` + launch + ` -p --mode json --config "$1" --no-session --no-tools --no-extensions --no-skills --no-rules --no-lsp --no-title "$2"`
	through := fmt.Sprintf("the model round trip through %s, routed to %s,", profileWords(strings.TrimSpace(g.env["OMP_PROFILE"])), g.route)
	turn := g
	turn.timeout = min(g.timeout, modelTurnTimeout)
	r, err := turn.run(ctx, script, overlay, modelPrompt)
	if err != nil {
		return fmt.Errorf("boot gate: run the model round trip: %w", err)
	}
	answer, answered := lastAnswer(r.stdout)
	unavailable := func(detail string) error { return &ModelRouteUnavailable{Detail: through + " " + detail} }
	switch {
	case answered && answer.Provider+"/"+answer.Model != g.model:
		return fmt.Errorf("%s was answered by %s/%s, not %s: the profile reaches a model past the gateway", through, answer.Provider, answer.Model, g.model)
	case answered && answer.StopReason == "error" && definitiveStatus(answer.ErrorMessage):
		return fmt.Errorf("%s: the gateway refused it: %s", through, answer.ErrorMessage)
	case answered && (answer.StopReason == "error" || answer.StopReason == "aborted"):
		return unavailable(fmt.Sprintf("ended %s: %s", answer.StopReason, answer.ErrorMessage))
	case answered:
		return nil
	case r.timedOut:
		return unavailable(r.killed(launch, turn.timeout))
	case r.exit == 0:
		return fmt.Errorf("%s answered nothing (launch command %q exited 0)%s", through, launch, stderrTail(r))
	case strings.Contains(r.stderr, "No API key found"), strings.Contains(r.stderr, "No model available"):
		// Oh My Pi starts on the pinned alias even when the profile's key command fails, and exits
		// naming the provider it has no key for; with no enabled model at all it names enabledModels.
		return fmt.Errorf("%s found no usable model: the profile's key command failed, or no enabled model has a key%s", through, stderrTail(r))
	}
	return unavailable(fmt.Sprintf("launch command %q exited %d before answering%s", launch, r.exit, stderrTail(r)))
}

// lastAnswer is the last assistant message's answer in an Oh My Pi `--mode json` stream, and
// whether there was one. A line that is not such an event is skipped.
func lastAnswer(stream string) (turnAnswer, bool) {
	var answer turnAnswer
	found := false
	scanner := bufio.NewScanner(strings.NewReader(stream))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			Message *struct {
				Role string `json:"role"`
				turnAnswer
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "message_end" || event.Message == nil || event.Message.Role != "assistant" {
			continue
		}
		answer, found = event.Message.turnAnswer, true
	}
	return answer, found
}

// definitiveStatus reports whether a failed turn's error is the gateway's HTTP refusal of what was
// sent — a 4xx other than a timeout (408), a conflict (409) or a rate limit (429) — which Oh My
// Pi reports as the status, a space, and the body.
func definitiveStatus(message string) bool {
	code, _, _ := strings.Cut(message, " ")
	status, err := strconv.Atoi(code)
	if err != nil || len(code) != 3 {
		return false
	}
	return status >= 400 && status < 500 && status != 408 && status != 409 && status != 429
}

// stderrTail is the run's stderr tail as a refusal quotes it: ": <tail>", or nothing.
func stderrTail(r ran) string {
	if r.tail == "" {
		return ""
	}
	return ": " + r.tail
}

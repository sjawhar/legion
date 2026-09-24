package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
)

// modelPrompt is the round trip's one message; any answer passes, since what is proved is who
// answered, not what.
const modelPrompt = "Reply with the single word ok."

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
// tool, or a session — so it proves the route, the key and the profile together, where a direct
// HTTP call would prove the key and pass a broken profile. It passes only when the turn was
// answered by g.model. A turn answered by any other provider or model (Oh My Pi falling back past
// the gateway), a profile with no usable model (the key command failing), the gateway refusing
// the key or the model (a 4xx other than 408, 409 and 429), or a turn that answered nothing are
// answers no retry changes; the gateway overloaded, rate-limited or unreachable, Oh My Pi dying
// before it answered, or a turn the budget cut off are waited out.
func (g pluginGate) verifyModelRoute(ctx context.Context) error {
	launch := tmux.WithOmpLaunchPrefix(g.prefix, g.invocation)
	script := `exec ` + launch + ` -p --mode json --no-session --no-tools --no-extensions --no-skills --no-rules --no-lsp --no-title "$1"`
	through := fmt.Sprintf("the model round trip through %s, routed to %s,", profileWords(strings.TrimSpace(g.env["OMP_PROFILE"])), g.route)
	return bootprobe.Run(ctx, "model route", g.retry, g.log, func(ctx context.Context) bootprobe.Outcome {
		r, err := g.run(ctx, script, modelPrompt)
		if err != nil {
			return bootprobe.Outcome{Refusal: fmt.Errorf("boot gate: run the model round trip: %w", err)}
		}
		answer, answered := lastAnswer(r.stdout)
		switch {
		case !answered && r.timedOut:
			return bootprobe.Outcome{Detail: through + " " + r.killed(launch, g.timeout)}
		case !answered && r.exit == 0:
			return bootprobe.Outcome{Refusal: fmt.Errorf("%s answered nothing (launch command %q exited 0)%s", through, launch, stderrTail(r))}
		case !answered && strings.Contains(r.stderr, "No model available"):
			return bootprobe.Outcome{Refusal: fmt.Errorf("%s found no usable model: the profile's key command failed, or no enabled model has a key%s", through, stderrTail(r))}
		case !answered:
			return bootprobe.Outcome{Detail: fmt.Sprintf("%s: launch command %q exited %d before answering%s", through, launch, r.exit, stderrTail(r))}
		case answer.Provider+"/"+answer.Model != g.model:
			return bootprobe.Outcome{Refusal: fmt.Errorf("%s was answered by %s/%s, not %s: the profile reaches a model past the gateway", through, answer.Provider, answer.Model, g.model)}
		case answer.StopReason == "error" && definitiveStatus(answer.ErrorMessage):
			return bootprobe.Outcome{Refusal: fmt.Errorf("%s: the gateway refused it: %s", through, answer.ErrorMessage)}
		case answer.StopReason == "error" || answer.StopReason == "aborted":
			return bootprobe.Outcome{Detail: fmt.Sprintf("%s ended %s: %s", through, answer.StopReason, answer.ErrorMessage)}
		}
		return bootprobe.Outcome{Passed: true}
	})
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

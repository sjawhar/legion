package supervise

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Budgets are a claim's retry counters, each a separate policy with its own reset point.
//
//   - LaunchFailures: launches that never became a working agent — a spawn or resume the runtime
//     refused, a process that died, one that never registered. Reset only by the agent's ready:
//     a registration alone proves nothing about the next launch.
//   - PromptFailures: prompts the agent refused, acknowledged prompts that started no turn, and
//     prompts refused after their acknowledgement. A prompt lost to the transport, or not sent for
//     want of a connection, is re-queued at no charge. Reset by a started turn.
//   - PromptRetires: processes retired because PromptFailures ran out. Reset by a started turn,
//     never by the relaunch itself — a claim that keeps acknowledging and never turning runs out.
type Budgets struct {
	LaunchFailures int
	PromptFailures int
	PromptRetires  int
}

// Limits are where each budget runs out: three launch failures, three prompt failures, and two
// prompt retirements in the shipped daemon. The caller reads them from its configuration.
type Limits struct {
	LaunchFailures int
	PromptFailures int
	PromptRetires  int
}

// relaunchAfterFailure charges one launch failure for a process that did not survive, and
// relaunches the same session after prev's incarnation — or fails the claim when the budget is
// spent.
func (m *Machine) relaunchAfterFailure(ctx context.Context, prev *runtime.Locator) error {
	m.claim.Budgets.LaunchFailures++
	if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, "launch failures ran out")
	}
	return m.launch(ctx, prev)
}

// chargePrompt counts one prompt failure. At the limit the process is retired — stopped, the
// session kept — and the same session relaunched, or, once retirements run out too, the claim
// fails with its prompt count left at the limit. A stop that fails charges nothing: the next
// failure reaches the limit again and retries the retirement.
func (m *Machine) chargePrompt(ctx context.Context, why string) error {
	failures := m.claim.Budgets.PromptFailures + 1
	if failures < m.deps.Limits.PromptFailures {
		m.claim.Budgets.PromptFailures = failures
		m.log.Warn("supervise: prompt failure", "why", why, "promptFailures", failures,
			"limit", m.deps.Limits.PromptFailures)
		return m.persist(ctx)
	}
	retiring := *m.claim.Locator
	if err := m.deps.Runtime.Stop(ctx, retiring, m.deps.Timeouts.StopGrace); err != nil {
		return fmt.Errorf("retire %s after %d prompt failures: stop: %w", m.claim.Token, failures, err)
	}
	retires := m.claim.Budgets.PromptRetires + 1
	m.claim.Budgets.PromptRetires = retires
	m.log.Warn("supervise: retired after prompt failures", "why", why, "promptFailures", failures,
		"relaunchCycle", retires, "limit", m.deps.Limits.PromptRetires)
	if retires >= m.deps.Limits.PromptRetires {
		m.claim.Budgets.PromptFailures = failures
		return m.fail(ctx, "prompt retirements ran out")
	}
	m.claim.Budgets.PromptFailures = 0
	return m.launch(ctx, &retiring)
}

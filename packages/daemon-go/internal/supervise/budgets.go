package supervise

import (
	"context"
	"fmt"
	"slices"
)

// Budgets are a claim's retry counters, each a separate policy with its own reset point.
//
//   - LaunchFailures: launches that never became a working agent — a spawn or resume the runtime
//     refused, a process that died, one that never registered. Reset only by the agent's ready:
//     a registration alone proves nothing about the next launch.
//   - Deaths: processes that died after their agent was ready and while it had work outstanding —
//     a pending task, whose turn was running or which was sent and not yet begun. The relaunch
//     reaches ready again whatever killed the last process, so ready cannot bound these; an agent
//     that dies before it completes a turn each time it is given the task would otherwise be
//     relaunched for ever. The count is charged against the pending task and reset only when that
//     task ends (retirePending): its turn ends, which a killed Oh My Pi never reports, or a
//     suspension or a phase change retires it. A turn that never ran the task — a notice's —
//     clears nothing. A death with nothing pending is not counted: a parked agent — idle, holding
//     no task — that the environment kills now and then is relaunched however often it happens, and
//     so is one killed in a turn no delivery started, a notice's, whose work waits for whatever
//     next wakes the relaunched agent. Bounded by the launch failure limit.
//   - PromptFailures: prompts the agent refused, acknowledged prompts that started no turn, and
//     prompts refused after their acknowledgement. A prompt lost to the transport, or not sent for
//     want of a connection, is re-queued at no charge. Reset by a started turn.
//   - PromptRetires: processes retired because PromptFailures ran out. Reset by a started turn,
//     never by the relaunch itself — a claim that keeps acknowledging and never turning runs out.
type Budgets struct {
	LaunchFailures int
	Deaths         int
	PromptFailures int
	PromptRetires  int
}

// Limits are where each budget runs out: three launch failures, three prompt failures, and two
// prompt retirements in the shipped daemon. The caller reads them from its configuration.
// LaunchFailures bounds Deaths as well: both count processes that did not survive.
type Limits struct {
	LaunchFailures int
	PromptFailures int
	PromptRetires  int
}

// relaunchAfterFailure charges one launch failure for a process that did not survive, and
// relaunches the same session after it — or fails the claim when the budget is spent.
func (m *Machine) relaunchAfterFailure(ctx context.Context) error {
	m.claim.Budgets.LaunchFailures++
	if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, "launch failures ran out")
	}
	return m.launch(ctx)
}

// chargeDeath counts a process that died after its agent was ready while it had work
// outstanding, and reports whether those deaths have reached the limit.
func (m *Machine) chargeDeath() bool {
	if m.claim.Pending == nil || slices.Contains(unready, m.claim.State) {
		return false
	}
	m.claim.Budgets.Deaths++
	m.log.Warn("supervise: the agent died with work outstanding", "deaths", m.claim.Budgets.Deaths,
		"limit", m.deps.Limits.LaunchFailures)
	return m.claim.Budgets.Deaths >= m.deps.Limits.LaunchFailures
}

// chargePrompt counts one prompt failure. At the limit the process is retired — suspended, the
// session kept — and the same session relaunched, or, once retirements run out too, the claim
// fails with its prompt count left at the limit. A suspension that fails charges nothing: the next
// failure reaches the limit again and retries the retirement.
func (m *Machine) chargePrompt(ctx context.Context, why string) error {
	failures := m.claim.Budgets.PromptFailures + 1
	if failures < m.deps.Limits.PromptFailures {
		m.claim.Budgets.PromptFailures = failures
		m.log.Warn("supervise: prompt failure", "why", why, "promptFailures", failures,
			"limit", m.deps.Limits.PromptFailures)
		return m.persist(ctx)
	}
	if err := m.suspendProcess(ctx); err != nil {
		return fmt.Errorf("retire %s after %d prompt failures: suspend: %w", m.claim.Token, failures, err)
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
	return m.launch(ctx)
}

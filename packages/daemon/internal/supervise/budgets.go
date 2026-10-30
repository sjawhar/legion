package supervise

import (
	"context"
	"fmt"
	"slices"
)

// Budgets are a claim's retry counters, each a separate policy with its own reset point.
//
//   - LaunchFailures: launches that never became a working agent — a spawn or resume the runtime
//     refused, a process that died, one whose agent never registered or registered and never said
//     it was ready. Reset only by the agent's ready: a registration alone proves nothing about the
//     next launch.
//   - Deaths: processes that died after their agent was ready with work outstanding — a pending
//     task, or a turn the agent had begun and not ended, whether or not a task was behind it
//     (chargeDeath). The relaunch reaches ready again whatever killed the last process, so ready
//     cannot bound these; an agent that dies in its turn each time it is woken would otherwise be
//     relaunched for ever. The count ends with the work it was charged against: the task's retire
//     (retirePending) or a turn that ends normally (turnEnded), either of which says the agent got
//     through what it was doing. A death with no turn open and nothing pending is not counted: a
//     parked agent — idle, holding no task — that the environment kills now and then is relaunched
//     however often it happens. Bounded by the launch failure limit.
//   - PromptFailures: prompts the agent refused, acknowledged prompts that started no turn, and
//     prompts refused after their acknowledgement. A prompt lost to the transport, not sent for
//     want of a connection, or refused because the agent is in a turn of its own (Oh My Pi's busy
//     answer, before or after its acknowledgement), is re-queued at no charge. Reset by a started
//     turn.
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
// relaunches the same session after it, once the backoff for the failures charged so far has
// passed (backOff) — or fails the claim when the budget is spent. A relaunch the tree lifecycle
// refuses (its cleanup reserved) is checked before the charge, so it costs none; one whose process
// is already stopped is relaunchRefused's.
func (m *Machine) relaunchAfterFailure(ctx context.Context) error {
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, m.relaunchAfterFailure, err)
	}
	m.claim.Budgets.LaunchFailures++
	if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, "launch failures ran out")
	}
	return m.launchAfter(ctx, true)
}

// chargeDeath counts a process that died after its agent was ready with work outstanding, and
// reports whether those deaths have reached the limit. Work is outstanding when the claim holds a
// task, and also when a turn of the agent's was open at the death — the claim's last turn started
// and has not ended — since a turn an Envoy message began does the issue's work with no task
// behind it, and a root architect killed six times inside its turns was relaunched for ever
// because each death found nothing pending. A death with no turn open and nothing pending is a
// parked agent the environment killed, which is not counted however often it happens.
func (m *Machine) chargeDeath() bool {
	if slices.Contains(unready, m.claim.State) {
		return false
	}
	if m.claim.Pending == nil && !m.turnOpen() {
		return false
	}
	m.claim.Budgets.Deaths++
	m.log.Warn("supervise: the agent died with work outstanding", "deaths", m.claim.Budgets.Deaths,
		"limit", m.deps.Limits.LaunchFailures, "pending", pendingID(m.claim.Pending), "turnOpen", m.turnOpen())
	return m.claim.Budgets.Deaths >= m.deps.Limits.LaunchFailures
}

// turnOpen is whether the agent began a turn it has not ended: the claim is working, which is the
// state a turn's start puts it in and its end takes it out of, or the claim's recorded turn times
// say so. Both are persisted, so the answer survives the restart that rebuilt the machine, and
// the times answer for a claim whose state has already moved on.
func (m *Machine) turnOpen() bool {
	return m.claim.State == StateWorking || m.claim.LastTurnStart.After(m.claim.LastTurnEnd)
}

// chargePrompt counts one prompt failure. At the limit the process is retired — suspended, the
// session kept — and the same session relaunched, or, once retirements run out too, the claim
// fails with its prompt count left at the limit. A suspension that fails charges nothing: the next
// failure reaches the limit again and retries the retirement. So does a relaunch the tree lifecycle
// refuses, checked before the suspension so the process is kept (checkLaunch). A retirement while a
// suspension is held is that suspension's stop instead, charged nothing (suspendHeld).
func (m *Machine) chargePrompt(ctx context.Context, why string) error {
	failures := m.claim.Budgets.PromptFailures + 1
	if failures < m.deps.Limits.PromptFailures {
		m.claim.Budgets.PromptFailures = failures
		m.log.Warn("supervise: prompt failure", "why", why, "promptFailures", failures,
			"limit", m.deps.Limits.PromptFailures)
		return m.persist(ctx)
	}
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	retires := m.claim.Budgets.PromptRetires + 1
	last := retires >= m.deps.Limits.PromptRetires
	retirement := m.suspendProcess
	if last {
		retirement = m.endProcess // the last retirement fails the claim, which does not run again
	} else if err := m.checkLaunch(ctx); err != nil {
		return fmt.Errorf("retire %s after %d prompt failures: %w", m.claim.Token, failures, err)
	}
	if err := retirement(ctx); err != nil {
		return fmt.Errorf("retire %s after %d prompt failures: suspend: %w", m.claim.Token, failures, err)
	}
	m.claim.Budgets.PromptRetires = retires
	m.log.Warn("supervise: retired after prompt failures", "why", why, "promptFailures", failures,
		"relaunchCycle", retires, "limit", m.deps.Limits.PromptRetires)
	if last {
		m.claim.Budgets.PromptFailures = failures
		return m.fail(ctx, "prompt retirements ran out")
	}
	m.claim.Budgets.PromptFailures = 0
	return m.launch(ctx)
}

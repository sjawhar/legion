package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// repoint is the claim's process found alive holding an address a process launched now is not
// handed (runtime.StaleAddress): one of the addresses the runtime hands every new process from the
// daemon's configuration (the worker stream it dials, the daemon's API, NATS, Envoy, Dispatch, the
// secrets broker) moved since this one launched, as each does when the daemon restarts with its key
// changed; the worker stream's keys are `advertise_host`, `bind` when no `advertise_host` is set,
// and `worker_stream_port`. The process keeps what it holds for as long as it runs, and what it
// holds may never reach the daemon or the service again, so there is nothing to wait out. A task
// whose turn the process was running goes back to waiting first (interrupted), the same recovery a
// death in a turn gets: that turn used the addresses the process holds, and the relaunch ends it. A
// held suspension ends here as it does for a death (endHeld): the claim is suspended, not
// relaunched. Otherwise the claim is relaunched at once through the launch path a death uses
// (launch): a Resume of its recorded session, or a Spawn over the claim's existing Sandbox when it
// has not registered yet, onto a process handed the current addresses. The stale observation is
// never charged, unlike a death (chargeDeath, relaunchAfterFailure): the process did nothing wrong.
// A relaunch the runtime refuses is charged as any launch failure is.
func (m *Machine) repoint(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: the process is alive at a stale address; replacing it with one at the current address",
		"incarnation", m.claim.Locator.Incarnation, "detail", observation.Detail)
	if err := m.interrupted(ctx); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx)
	}
	return m.launch(ctx)
}

package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// RequestTurnOutcome is how one turn of this claim's agent ended, as the agent's own plugin
// classified it and posted it on POST /legion/v1/claims/turn-outcome: pi-legion's agent_end hook
// for an Oh My Pi session. The daemon never reads a provider's text, so the class travels and
// the provider's words do not; Category is a refusal's own category (`reasoning_extraction`) and
// is empty for every other outcome (claim.TurnOutcomeRequest).
//
// Sequence is the plugin's own count of the turns it has ended at Generation, from 1. The plugin
// is the one counter: the turn's end on the worker stream (StreamTurnEnd) takes no part in this,
// so no two counters can drift, and a turn no report arrived for leaves no report at all.
//
// Generation is not fenced. A report naming a generation the claim has left is still a turn of
// that generation, and the pair is what says whether it is the newest the claim has sent
// (turnOutcome).
type RequestTurnOutcome struct {
	Claim      claim.Token
	Generation uint64
	Sequence   uint64
	Outcome    string
	Category   string
}

// TurnOutcome is the newest turn outcome the claim's plugin has reported: the class, a refusal's
// category, and the (generation, sequence) it came under.
type TurnOutcome struct {
	Generation uint64
	Sequence   uint64
	Outcome    string
	Category   string
}

// newerThan is whether o came under a later (generation, sequence) than held, which is the order
// the turns ran in. nil is held by a claim that has reported nothing yet.
func (o TurnOutcome) newerThan(held *TurnOutcome) bool {
	switch {
	case held == nil:
		return true
	case o.Generation != held.Generation:
		return o.Generation > held.Generation
	default:
		return o.Sequence > held.Sequence
	}
}

// turnOutcome keeps the newest report the claim has sent and logs it. A report that arrives out
// of order — two turns whose HTTP calls overlap, in either order — leaves the newer one standing,
// which is why the pair the plugin counted travels with the class.
//
// The provider-error retry reads it (dispatch://INFRA-956 R2, providererror.go): a turn a content
// filter refused can clear, because the plugin has already moved the session down its model
// chain, and a turn the provider failed in a way it marked final cannot. A report that lands
// while that decision is waiting for one decides it here, in the same step, rather than at the
// end of the wait.
//
// It is memory only, as the held suspension and `askFirst` are (`Machine`'s fields in
// machine.go): the decision reads the report within two minutes of the turn it is about, and a
// daemon restart in that window leaves no report at all, which leaves the task sitting
// unconfirmed on its claim to be sent again — the same answer the wait reaches. A report is
// worth no store write and no migration for a fact with that life.
func turnOutcome(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestTurnOutcome)
	report := TurnOutcome{Generation: r.Generation, Sequence: r.Sequence, Outcome: r.Outcome, Category: r.Category}
	kept := report.newerThan(m.lastTurnOutcome)
	if kept {
		m.lastTurnOutcome = &report
	}
	m.log.Info("supervise: the agent's plugin reported how a turn ended", "generation", r.Generation,
		"sequence", r.Sequence, "outcome", r.Outcome, "category", r.Category, "kept", kept)
	if !kept {
		return nil
	}
	if _, waiting := m.timers[TimerTurnOutcome]; !waiting {
		return nil
	}
	return m.decideProviderError(ctx)
}

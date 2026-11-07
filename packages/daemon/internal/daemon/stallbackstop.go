package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// backstopStalledTurns ends the turn of every claim the sweep finds in one whose recorded last
// step is older than its role's backstop bound (`worker_stall_backstop_hours`,
// config.Config.StallBackstopFor), over the claims the orphan sweep already read this tick. It is
// the daemon's half of INFRA-1274: the worker's own plugin watches its tool calls and recovers in
// steps, and this acts where that plugin is itself the thing that is broken, since the daemon is
// the only part that outlives it.
//
// It reads the record and nothing else (supervise.StepRecord.LastStep): the last step the daemon
// received, or the turn's own start for a claim whose agent has sent nothing since that turn
// began. A claim whose record holds neither is judged from nothing and left alone, because this
// pass never guesses. It does not ask whether the worker's own ladder is running — the daemon
// cannot see it, since no step of that ladder crosses the wire — and nothing here needs to: the
// configuration refuses a bound below that whole ladder (config's checkStallBackstops), so a
// working plugin always finishes first.
//
// Three claims are passed over. A role whose bound is 0 is switched off by the deployment and is
// never read again. A claim in no turn has no turn to end: a claim's record is never cleared, so
// the step of a turn that ended hours ago still stands on an idle claim, and reading it would end
// a turn nobody is in. A claim in an update round is the round's to carry: the round replaces
// that process once every worker of its pod has answered, and tells the controller when it does
// not finish (reconcilePodUpdates, tellUpdateStalled), so a backstop relaunch under it would
// replace a process the round is waiting on and start its clock again.
//
// The stop itself is the machine's (supervise's backstopStalledTurn), which re-reads the claim's
// own record before it acts and charges nothing. The post is made at every tick that finds the
// claim still stalled, as StalePlugin's is, so a stop the runtime refuses is tried again at the
// next sweep with no timer of its own.
func (s *supervision) backstopStalledTurns(ctx context.Context, claims []supervise.Claim) {
	now := s.plan.clock.Now()
	told := map[claim.Token]time.Time{}
	for _, c := range claims {
		if ctx.Err() != nil {
			return
		}
		bound := s.cfg.StallBackstopFor(c.Role)
		if bound <= 0 || c.State != supervise.StateWorking || c.UpdateRound != "" {
			continue
		}
		last := c.Step.LastStep()
		if last.IsZero() || !last.Add(bound).Before(now) {
			continue
		}
		s.supervisor.post(c.Token, supervise.StallBackstop{Claim: c.Token, Bound: bound, Deadline: last.Add(bound)})
		if s.backstopTold[c.Token].Equal(last) || s.tellStallBackstopped(ctx, c, last, now.Sub(last), bound) {
			told[c.Token] = last
		}
	}
	// What this pass keeps of its own is the one mark above, and it keeps it for the claims that
	// are stalled now: a claim whose turn came back is no longer here, so the next stall of it is
	// told afresh. Nothing of it goes in the store, as the pod-update sweep's own pace does not
	// (podUpdateOpenings): a daemon that restarts mid-stall tells the owner once more, which is
	// the right way round for a notice whose whole purpose is that somebody hears it.
	s.backstopTold = told
}

// tellStallBackstopped tells the owner of the stalled worker's issue — the architect the issue's
// notices reach — that the daemon ended this turn, with the record it read in the notice: how
// long the agent was silent, the bound it passed, when the turn began and the call the record
// names, if any. The call's arguments are in no part of it: their text is in no column either, a
// shell command routinely holding a credential (supervise.StepRecord).
//
// It reports whether the owner was told, which is what the sweep's "told once per stall" mark
// records (backstopStalledTurns): a row the store refuses is told again at the next sweep, as a
// stalled update round's is (tellUpdateStalled). The project controller's own claim has no issue
// and so no owner to tell; the line it logs is the whole of its telling, and the record is in it.
func (s *supervision) tellStallBackstopped(ctx context.Context, c supervise.Claim, last time.Time, silent, bound time.Duration) bool {
	found := stallBackstopRecord(c, last, silent, bound)
	if c.Issue == "" {
		s.log.Warn("supervise: the daemon's stall backstop ended a turn of a claim on no issue; there is no owner to tell",
			"claim", c.Token, "role", c.Role, "record", found)
		return true
	}
	if s.enqueue == nil {
		s.log.Warn("supervise: the daemon's stall backstop ended a turn, and this deployment has no workflow to tell the owner through",
			"claim", c.Token, "issue", c.Issue, "record", found)
		return true
	}
	if err := s.enqueueStallNotice(ctx, c, found); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the owner that the daemon's stall backstop ended a turn; telling again at the next sweep",
				"claim", c.Token, "issue", c.Issue, "error", err)
		}
		return false
	}
	s.log.Warn("supervise: the daemon's stall backstop ended a turn; the owner is told",
		"claim", c.Token, "issue", c.Issue, "role", c.Role, "record", found)
	return true
}

// enqueueStallNotice writes the owner's notice rows, both in one call so a stall told to one
// side and not the other is never told again at the next sweep (tellUpdateStalled's own reason
// for one call). A worker's stall is its issue's architect's to read, as its death is. A tree
// architect's own stall is told to the controller as well, in a row of its own: every other
// notice of the tree reaches that architect, and nobody inside the tree is left to act on it
// (workflow's claimFailed, which tells a failed tree architect's death the same way).
func (s *supervision) enqueueStallNotice(ctx context.Context, c supervise.Claim, found string) error {
	notice := record.Notice{Kind: stallBackstopNotice, Role: c.Role, Reason: found}
	rows := []record.OutboxPayload{notice}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		rows = append(rows, record.ControllerNotice(notice))
	}
	return s.enqueue(ctx, c.Issue, rows...)
}

// stallBackstopNotice is the kind of the notice above (record's validNoticeKind), beside
// `worker-died`, which is what the architect hears of a claim that died rather than stalled.
const stallBackstopNotice = "worker-stalled"

// stallBackstopRecord is what the owner is told and what the log line carries: the record the
// daemon read, in one sentence, and what it did about it. It is the same text either way, so a
// deployment with no workflow leaves the same account in its log as the notice would have
// carried.
func stallBackstopRecord(c supervise.Claim, last time.Time, silent, bound time.Duration) string {
	call := "the daemon saw no call of this turn"
	if c.Step.CallID != "" {
		call = fmt.Sprintf("the last call the daemon saw was %s (%s), which started at %s and has not ended",
			c.Step.Tool, c.Step.CallID, stallInstant(c.Step.StartedAt))
		if !c.Step.EndedAt.IsZero() && !c.Step.EndedAt.Before(c.Step.StartedAt) {
			call = fmt.Sprintf("the last call the daemon saw was %s (%s), which started at %s and ended at %s",
				c.Step.Tool, c.Step.CallID, stallInstant(c.Step.StartedAt), stallInstant(c.Step.EndedAt))
		}
	}
	return fmt.Sprintf("the daemon ended the %s's turn: its agent last stepped at %s, %s ago, past the %s this deployment gives the role (worker_stall_backstop_hours). The turn began at %s, and %s. The task it was serving is still owed to it and its session was brought back on a new process; nothing was charged to the claim. The arguments of a call are never recorded, so they are in no part of this notice.",
		c.Role, stallInstant(last), silent.Round(time.Minute), bound, stallInstant(c.Step.TurnStartedAt), call)
}

// stallInstant is a moment of the record as the notice and the log line write it, and "no time
// the daemon recorded" for a field the record does not hold.
func stallInstant(at time.Time) string {
	if at.IsZero() {
		return "no time the daemon recorded"
	}
	return at.UTC().Format(time.RFC3339)
}

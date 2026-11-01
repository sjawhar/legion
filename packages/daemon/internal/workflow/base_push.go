package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

// basePush records that a pull request's base branch moved (intake.BasePush): the push Envoy
// publishes for a branch that is no issue's, which is the shape of a branch pull requests sit on.
// GitHub recomputes whether each head on that base still merges into it, and sends no event for the
// pull request itself while it does, so a head the record holds as clean can already be behind or
// dirty with nothing in the record saying so. Recording the commit the push left the base at says
// which base the recorded mergeability and merge state were read against, so the next read of the
// pull request is known to be needed rather than waited for; it decides nothing else, and the phase
// of every issue is left exactly where it is. The pull request's own facts - a head, a settlement,
// a review - are what move an issue, and a base that moved is none of them.
//
// One fact serves every pull request on the base, so the fact lock is taken once per (repo, base)
// push however many pull requests sit on it, rather than once per pull request. The first read is
// one indexed lookup of the open pull requests recorded on that base
// (record.Store.OpenPullRequestsOnBase), so a push to a branch Legion records no pull request on -
// every push of the other automation a mapped repository carries - ends the transaction there,
// having written nothing. A pull request whose recorded base is still the empty one no read has
// filled matches no push: the daemon's next read of that pull request - at a handoff, at its
// start, or for a round waiting on the merge state - fills the base, and from then on its base's
// pushes find it.
//
// The one walk both records the move and asks for the read a moved base calls for
// (askMergeStateFor), which is why a base push is the one fact Apply's askMergeState leaves
// alone: what tells a redelivered push from a first delivery is the commit the record held when
// the push arrived, and this walk is the last reader of it.
func (e *Engine) basePush(ctx context.Context, tx pgx.Tx, fact intake.BasePush) (intake.Result, error) {
	open, err := e.store.OpenPullRequestsOnBase(ctx, tx, fact.Repo, fact.Branch)
	if err != nil {
		return intake.Result{}, err
	}
	for _, pr := range open {
		if pr.BaseMoved == fact.After {
			// The same push redelivered, or a push that returned the base to the commit the
			// pending read is already of: the record says what it would say, and the read it
			// calls for was asked for by the delivery that recorded it.
			continue
		}
		// A GitHub fact applies to an issue only when it names the issue's repository
		// (ownPullRequest), which a record written in a clone's spelling can differ from.
		own, err := e.ownPullRequest(ctx, tx, &pr, fact.Repo)
		if err != nil {
			return intake.Result{}, err
		}
		if own == nil {
			continue
		}
		own.BaseMoved = fact.After
		if err := e.store.PutPullRequest(ctx, tx, *own); err != nil {
			return intake.Result{}, err
		}
		// A base that moved is what takes a head from clean to behind, which is the one answer
		// that holds an approved issue in reviewing, so every pull request this push moved is
		// read for. The read is asked for here rather than from Apply's askMergeState because
		// the line above has just overwritten what tells a redelivery from a first delivery: a
		// second walk after the handler would find every pull request on the base recorded at
		// this push's commit and ask for all of their reads again, however many times the push
		// was delivered.
		if err := e.askMergeStateFor(ctx, tx, own); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

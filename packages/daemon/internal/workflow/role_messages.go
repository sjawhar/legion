package workflow

import (
	"context"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// roleRecipient is one role a message is addressed to: issue, whose role it is - a tree
// architect's is its tree's root - role, and row, the issue whose outbox the message is written
// to, which differs from issue only for the architect of another issue's tree.
type roleRecipient struct {
	issue string
	role  claim.Role
	row   string
}

// addressedRole is a recipient the daemon can name: the claim token whose role topic the message
// is published to, and the issue its outbox row belongs to.
type addressedRole struct {
	token claim.Token
	issue string
}

// addressRoles is the one collection the daemon's role messages are addressed by (tellChecks,
// tellGateMoved): the claim token of each recipient, in the order given and each token once, so a
// role named twice - the implementer that also holds the phase, a tree member whose phase is its
// architect's - is told once. A recipient with no role is a phase no worker holds, and is nobody.
//
// unnamable is the policy for a recipient whose claim token cannot be built, which is the whole of
// what the two senders differ in: nil ends the collection with that error, and so the fact's
// transaction, while a function is given the recipient and the error and may answer nil to leave
// that one untold and address the rest. A sender whose message is secondary to the record the
// transaction writes passes a function that logs, since aborting would roll the record back for a
// name the daemon could not spell.
func (e *Engine) addressRoles(project string, recipients []roleRecipient, unnamable func(roleRecipient, error) error) ([]addressedRole, error) {
	projectToken, err := claim.ProjectToken(project)
	if err != nil {
		return nil, err
	}
	addressed := make([]addressedRole, 0, len(recipients))
	for _, recipient := range recipients {
		if recipient.role == "" {
			continue
		}
		token, err := claim.NewToken(projectToken, recipient.issue, recipient.role)
		if err != nil {
			if unnamable == nil {
				return nil, err
			}
			if err := unnamable(recipient, err); err != nil {
				return nil, err
			}
			continue
		}
		if slices.ContainsFunc(addressed, func(a addressedRole) bool { return a.token == token }) {
			continue
		}
		addressed = append(addressed, addressedRole{token: token, issue: recipient.row})
	}
	return addressed, nil
}

// logUnnamable is the unnamable policy of a sender whose message is secondary to the record its
// transaction writes: the holder is logged, on the commit, with what the message was about, and
// the rest are told.
func (e *Engine) logUnnamable(ctx context.Context, what string, attributes ...any) func(roleRecipient, error) error {
	return func(recipient roleRecipient, err error) error {
		e.logOnCommit(ctx, what, append(slices.Clone(attributes), "issue", recipient.issue, "role", recipient.role, "error", err)...)
		return nil
	}
}

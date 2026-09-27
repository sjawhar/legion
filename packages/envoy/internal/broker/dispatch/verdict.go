package dispatch

import (
	"fmt"
	"net/http"
	"strings"
)

// Decision is what one read of an approval ask means for the request waiting on it.
type Decision int

const (
	// Undecided means the ask is still open: the request stays pending.
	Undecided Decision = iota
	Approved
	Denied
)

// Verdict decides a pending approval from a read of its ask. wantAskID and recordedEdit are the
// ask id and edited_at recorded when the ask was opened; allowedApprover is the one login whose
// answer counts. The ask approves only when it is that ask, answered, not edited since it was
// opened, answered by allowedApprover (both compared in CanonicalLogin form), with exactly
// "Approve" selected; any other answer or resolution denies, and reason says why. An ask in a
// state this package does not know is an error — a read to retry, never a decision.
func Verdict(ask Ask, wantAskID string, recordedEdit *string, allowedApprover string) (decision Decision, reason string, err error) {
	switch ask.State {
	case "open":
		return Undecided, "", nil
	case "answered", "resolved":
	default:
		return Undecided, "", fmt.Errorf("ask %q has unrecognized state %q", ask.ID, ask.State)
	}
	switch {
	case ask.ID != wantAskID:
		return Denied, "ask id does not match the request's own ask", nil
	case ask.State != "answered" || ask.Answer == nil:
		return Denied, "ask was " + ask.State + " without an approval", nil
	case !sameEdit(recordedEdit, ask.EditedAt):
		return Denied, "ask was edited after it was opened", nil
	case CanonicalLogin(allowedApprover) == "" || CanonicalLogin(ask.Answer.User) != CanonicalLogin(allowedApprover):
		return Denied, "answered by " + ask.Answer.User + ", not the allowed approver", nil
	case len(ask.Answer.Selected) != 1 || ask.Answer.Selected[0] != "Approve":
		return Denied, "approver chose " + strings.Join(ask.Answer.Selected, ","), nil
	}
	return Approved, "", nil
}

// sameEdit reports whether two edited_at stamps name the same revision, nil being "never edited".
func sameEdit(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Retracted reports whether err, from RetractAsk, leaves the ask closed to answers: retracted
// just now (nil), or past answering already — gone (404), or answered, resolved or on a closed
// issue (409). Anything else, an outage above all, leaves the ask to retract again later.
func Retracted(err error) bool {
	if err == nil {
		return true
	}
	dispatchErr, ok := AsError(err)
	return ok && (dispatchErr.Status == http.StatusNotFound || dispatchErr.Status == http.StatusConflict)
}

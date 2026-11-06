package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// CompactRequest is the controller's compact: the claim on Role of Issue has its Oh My Pi
// session compacted and is then sent the task it owes again. Role is a workflow role or
// `architect`, as a fresh session's is (claim.IsRole): the project controller is no role here.
type CompactRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
}

// CompactResponse is what became of the compaction, and it is the same answer through both
// doors. Compacted is whether Oh My Pi compacted the session; Reason is why it did not, which is
// Oh My Pi's own words for a compaction it refused or skipped ("Already compacted", "Nothing to
// compact (session too small)") and the daemon's for a wait that ran out or a transport that
// failed.
//
// A compaction that did not run is this answer and not a failure status, because the operation
// still did the whole of what it promises: it asked, and it sent the claim the task it owes
// again (supervise's compactEnded). Only a refusal made before anything was asked — a claim in
// no state to be compacted, a Claude Code claim, a role the request may not name — answers 4xx,
// and only there does the claim keep its task unsent.
type CompactResponse struct {
	Compacted bool   `json:"compacted"`
	Reason    string `json:"reason,omitempty"`
}

// The refusals the compaction writes itself, which both doors give word for word (compact).
const (
	// claimNotRunning: the claim's agent is not running, so there is no session to compact and
	// fresh_session is what reaches it.
	claimNotRunning = "CLAIM_NOT_RUNNING"
	// claimNotOnOmp: the role runs on Claude Code, which has no Oh My Pi session at all.
	claimNotOnOmp = "CLAIM_NOT_ON_OMP"
	// compactionFailed: the daemon could not ask at all, or could not give the claim back.
	compactionFailed = "COMPACTION_FAILED"
)

// compactInstructions is what Oh My Pi writes the summary to. Its first purpose is the summary's
// form: Oh My Pi runs the image-based snapcompact method only for a compaction that carries no
// instructions (shimwire.Compact), and a worker resumed on images needs a vision model to read
// its own past, so these instructions are what make the summary text.
//
// A model reads it, so it follows ASD-STE100: one instruction per sentence, active voice, and no
// word with a second reading.
const compactInstructions = "Write the summary as text. " +
	"Keep what the rest of this task needs: the task itself, the decisions you made, the files you changed, " +
	"the commands you ran and what they answered, and what is still open. " +
	"Keep every issue key, file path, branch name, commit id and pull request number you still need. " +
	"Drop the full text of a file you read and the full output of a command that passed."

// claimCompact is the controller's door onto the compaction (compact): the claim is named by its
// issue and role rather than by a token the controller never sees, and the caller is its
// project's controller rather than the operator. The effect is the operator's own, and so are
// its refusals and its answer; only the authentication differs (claimFreshSession).
func (s *server) claimCompact(w http.ResponseWriter, r *http.Request) {
	var req CompactRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"role", req.Role}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue must be a Dispatch issue key")
		return
	}
	// The controller is refused its own claim here, as the fresh session's door refuses it: the
	// token is built from the issue and the role, and no issue's role spells the controller's
	// token (claim.ControllerToken).
	role := claim.Role(req.Role)
	if !claim.IsRole(role) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ROLE", fmt.Sprintf(
			"role must be one of %s; the project's controller is none of them, and the controller does not compact its own session",
			roleList()))
		return
	}
	token, err := claim.NewToken(s.project, req.Issue, role)
	if err != nil {
		s.logFailure("api: build the claim token of a compaction", "issue", req.Issue, "role", req.Role, "error", err)
		writeFailure(w, http.StatusInternalServerError, "CLAIM_TOKEN_FAILED", err.Error())
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "a compaction requires a controller grant")
		return
	}
	m, supervised := s.supervisor.Machine(token)
	if !supervised {
		writeFailure(w, http.StatusNotFound, "NO_CLAIM", "no claim "+string(token))
		return
	}
	answer, ok := s.compact(w, r, m, token)
	if !ok {
		return
	}
	s.log.Info("api: the controller compacted a claim's session", "claim", token, "issue", req.Issue,
		"role", req.Role, "compacted", answer.Compacted, "reason", answer.Reason)
	writeJSON(w, http.StatusOK, answer)
}

// compactClaim is the operator's door onto the compaction: `legion claims compact --claim
// <token>`, which names the claim by its token and is authenticated by the operator's bearer, as
// every other `legion claims` subcommand is. The effect and the answer are the controller's own.
func (s *server) compactClaim(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
		return
	}
	answer, ok := s.compact(w, r, m, token)
	if !ok {
		return
	}
	s.operatorDone(r, token)
	s.log.Info("api: the operator compacted a claim's session", "claim", token,
		"compacted", answer.Compacted, "reason", answer.Reason)
	writeJSON(w, http.StatusOK, answer)
}

// compact is the one compaction effect, which `legion claims compact` (compactClaim) and the
// controller's compact (claimCompact) both run: the claim's Oh My Pi session is compacted, and
// then the task the claim owes is sent to it again.
//
// It is what a claim whose provider keeps refusing its session reaches for first. The session,
// its workspace and everything the worker knows of its issue stay where they are, and what goes
// is the length of the transcript the provider is asked to read — where a fresh session keeps
// nothing at all. The controller's skill has it try this one first and ask for a fresh session
// only when the claim is still refused after it (skills/legion-controller).
//
// It reaches a claim whose agent is running and in no turn, a root architect included
// (agentRunning), and refuses four:
//
//   - a claim that is suspended, stopped or failed runs no agent at all, so there is no session
//     to ask (startsAgain). Such a claim is started again rather than compacted, and the refusal
//     names fresh_session, which is the operation that reaches it;
//   - a claim in a turn is refused as a fresh session is, word for word (refuseInATurn), because
//     a compaction aborts whatever turn its session is running and that turn may be the one that
//     ends the run of provider errors. A claim already being compacted is in no turn, so it is
//     told so in its own words: the compaction in flight is what it waits for;
//   - a claim between states, queued or with a launch in flight, has no agent to ask yet
//     (claimUnsettled);
//   - a claim whose role runs on the Claude Code backend has no Oh My Pi session to compact at
//     all. Which harness a role runs on is the daemon's configuration rather than anything the
//     claim records (`worker_backends`, runtime.Backends), so it is handed to this server as
//     Options.Backend, which the daemon fills from config.Config.BackendFor.
//
// The project's own controller claim is not refused here, where a fresh session of it is
// (freshSession's NOT_THE_CONTROLLER). The asymmetry is the two operations' own, not an
// oversight: a fresh session of the controller would cut the project's one controller off,
// and a compaction cuts nothing off — the session, its process and everything the controller
// holds stay, and only the transcript gets shorter. The controller's own door still cannot name
// it, since `controller` is no role a claim token is built from (claimCompact).
//
// A compaction that ran puts the claim's run of provider-error turns back to zero, and the
// ladder's wait goes with it, so the owed task goes now rather than at the end of a wait
// counted against the transcript the compaction shortened (supervise's compactEnded).
//
// A compaction Oh My Pi refused or skipped is no refusal of this route: the answer says so and
// the owed task has gone again all the same (CompactResponse).
func (s *server) compact(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (CompactResponse, bool) {
	c := m.Claim()
	if s.backend != nil && runtime.Of(s.backend(c.Role)) == runtime.BackendClaude {
		writeFailure(w, http.StatusConflict, claimNotOnOmp, fmt.Sprintf(
			"%s runs on %s, which has no Oh My Pi session to compact; %s is what starts it again",
			token, runtime.Backends[runtime.BackendClaude].Name, freshSessionName))
		return CompactResponse{}, false
	}
	switch {
	case c.State == supervise.StateWorking && c.Compacting:
		writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
			"%s is being compacted already: wait for that compaction to end", token))
		return CompactResponse{}, false
	case c.State == supervise.StateWorking:
		return CompactResponse{}, refuseInATurn(w, token)
	case startsAgain(c.State):
		writeFailure(w, http.StatusConflict, claimNotRunning, fmt.Sprintf(
			"%s is %s, so it runs no Oh My Pi session to compact; %s starts it again instead",
			token, c.State, freshSessionName))
		return CompactResponse{}, false
	case !agentRunning(c.State):
		writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
			"%s is %s: a compaction reaches a claim whose agent is ready or idle; ask again when %s is one of those",
			token, c.State, token))
		return CompactResponse{}, false
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, "compact") {
		return CompactResponse{}, false
	}
	switch err := m.Compact(ctx, compactInstructions); {
	case err == nil:
		return CompactResponse{Compacted: true}, true
	case errors.Is(err, supervise.ErrCompactInstructions):
		// The instructions are this package's own constant, so an empty one is a bug here.
		s.logFailure("api: the compaction carried no instructions", "claim", token, "error", err)
		writeFailure(w, http.StatusInternalServerError, compactionFailed, err.Error())
		return CompactResponse{}, false
	default:
		var refused *supervise.RefusedError
		if errors.As(err, &refused) {
			// A turn, or another compaction, started between the state read above and the take.
			writeFailure(w, http.StatusConflict, claimInATurn, refused.Error())
			return CompactResponse{}, false
		}
		// Oh My Pi refused or skipped the compaction, the wait for it ran out, or the transport
		// failed. The claim has its task again either way, so the caller is told why rather than
		// told the request failed.
		reason := runtime.RefusalReason(err)
		if reason == "" {
			reason = err.Error()
		}
		s.log.Warn("api: the claim's session was not compacted; its owed task was sent again", "claim", token, "reason", reason)
		return CompactResponse{Reason: reason}, true
	}
}

// freshSessionName is the operation a refusal of a compaction names as the one that reaches the
// claim instead, spelled as both doors' callers know it.
const freshSessionName = "fresh_session (`legion claims retry --fresh-session`)"

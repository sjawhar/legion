package api

import (
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// CompactResponse is what became of the compaction, and it is the same answer through both
// doors. Compacted is whether Oh My Pi compacted the session; Reason is why it did not, which
// is Oh My Pi's own words for a compaction it refused or skipped ("Already compacted",
// "Nothing to compact (session too small)") and the daemon's for a wait that ran out or a
// transport that failed, cut to reasonBound.
//
// A compaction that did not run is this answer and not a failure status, because the operation
// still did the whole of what it promises: it asked Oh My Pi, and it gave the claim back and
// sent it the task it owes again, or nothing when it owes none (supervise's compactEnded). So
// this answer always carries the claim back, whatever it says of the compaction, and the CLI
// prints both halves of it in one line (cmd/legion's printCompaction).
//
// Every other outcome is a failure status and gives the claim back nothing here: a refusal made
// before anything was asked — a claim in no state to be compacted, a Claude Code claim, a role
// the request may not name — a take that did not happen at all
// (supervise.ErrCompactNotTaken), an end the store would not write
// (supervise.ErrCompactNotEnded), and a claim that lost its process under the compaction, which
// was given back nothing and sent nothing (supervise.ErrCompactProcessGone).
type CompactResponse struct {
	Compacted bool   `json:"compacted"`
	Reason    string `json:"reason,omitempty"`
}

// reasonBound is how much of a compaction's reason the answer carries. The reason is words the
// daemon did not write: Oh My Pi's for a compaction it refused or skipped, and through that a
// summarizer model's own explanation of a refusal. It reaches the controller's tool result and
// so its next prompt, where no text the daemon did not write belongs
// (dispatch://INFRA-956 R2, supervise's providerErrorCause). The known refusals are one short
// sentence each ("Nothing to compact (session too small)" is 37 bytes), and a transport
// failure's is of the same order, so 200 bytes carries every reason the operation has and no
// model's essay.
const reasonBound = 200

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

// compactOp is the compaction in the words its own refusals name it by, which both of its doors
// give: the role `controller` the controller's door may not name (claimCompact), and the claim
// whose turn a compaction would abort (refuseState). The type is the two recoveries' own
// (controllerOp, freshsession.go).
var compactOp = controllerOp{
	name:             "a compaction",
	notTheController: "the controller does not compact its own session",
	cutsTheTurn:      "a compaction would abort that turn",
}

// claimCompact is the controller's door onto the compaction (compact). The effect is the
// operator's own, and so are its refusals and its answer; only the authentication differs
// (controllerClaim).
func (s *server) claimCompact(w http.ResponseWriter, r *http.Request) {
	target, ok := s.controllerClaim(w, r, compactOp)
	if !ok {
		return
	}
	answer, ok := s.compact(w, r, target.machine, target.token)
	if !ok {
		return
	}
	s.log.Info("api: the controller compacted a claim's session", "claim", target.token, "issue", target.issue,
		"role", target.role, "compacted", answer.Compacted, "reason", answer.Reason)
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
// It reaches a claim whose agent is ready or idle, a root architect included. The machine is the
// one judge of which states take a compaction: this route asks for it and reports the refusal
// the machine answered, written from the state that refusal names (refuseState). Four are
// refused:
//
//   - a claim that is suspended, stopped or failed runs no agent at all, so there is no session
//     to ask (startsAgain). Such a claim is started again rather than compacted, and the refusal
//     names fresh_session, which is the operation that reaches it;
//   - a claim in a turn is refused because a compaction aborts whatever turn its session is
//     running and that turn may be the one that ends the run of provider errors; the refusal
//     names the compaction, so a controller told to compact first is not told to ask for a
//     fresh session instead (refuseInATurn). A claim already being compacted is in no turn, so
//     it is told so in its own words: the compaction in flight is what it waits for;
//   - a claim between states, queued or with a launch in flight, has no agent to ask yet
//     (claimUnsettled);
//   - a claim whose role runs on the Claude Code backend has no Oh My Pi session to compact at
//     all, and it is the one refusal this route makes itself, since no state of the claim says
//     which harness it runs. Which harness a role runs on is the daemon's configuration rather
//     than anything the claim records (`worker_backends`, runtime.Backends), so it is handed to
//     this server as Options.Backend, which the daemon fills from config.Config.BackendFor.
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
// A compaction Oh My Pi refused or skipped is no refusal of this route: the answer says so, the
// claim is back, and whatever task it owes has gone again (CompactResponse). A compaction that
// ran and did not end is a failure of its own: the store would not write the end
// (supervise.ErrCompactNotEnded), or the claim lost its process under the compaction
// (supervise.ErrCompactProcessGone). Neither gave the claim back, so neither is answered as a
// compaction that did not run.
func (s *server) compact(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (CompactResponse, bool) {
	if c := m.Claim(); s.backend != nil && runtime.Of(s.backend(c.Role)) == runtime.BackendClaude {
		writeFailure(w, http.StatusConflict, claimNotOnOmp, fmt.Sprintf(
			"%s runs on %s, which has no Oh My Pi session to compact; %s is what starts it again",
			token, runtime.Backends[runtime.BackendClaude].Name, freshSessionName))
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
	case errors.Is(err, supervise.ErrCompactNotTaken):
		// Nothing was asked of Oh My Pi and nothing was sent: the take could not be written, the
		// claim lost its process before the ask, or its shim is not connected. The caller is told
		// the operation failed rather than told a compaction that did not run, which carries the
		// claim's task with it.
		s.logFailure("api: the claim was not taken for a compaction", "claim", token, "error", err)
		writeFailure(w, http.StatusConflict, compactionFailed, err.Error())
		return CompactResponse{}, false
	case errors.Is(err, supervise.ErrCompactNotEnded):
		// Oh My Pi answered and the write that gives the claim back failed, so the claim is left
		// working with its task unsent. The store's failure is what the caller is told: a 200
		// would promise it the claim is back and whatever task it owed has gone again.
		s.logFailure("api: the compaction ended and the claim could not be given back", "claim", token, "error", err)
		writeFailure(w, http.StatusInternalServerError, compactionFailed, err.Error())
		return CompactResponse{}, false
	case errors.Is(err, supervise.ErrCompactProcessGone):
		// The claim lost its process under the compaction, so the compaction's end gave it back
		// nothing and sent it nothing. What starts it again is its own path's — a relaunch for a
		// claim the daemon relaunches, an operator's retry or resume for one its death failed or
		// a held suspension stopped — and the task it owes goes then. The answer says so in the
		// daemon's own words: what Oh My Pi made of such a compaction is in the machine's log,
		// where no model's words reach the caller's next prompt (reasonBound).
		s.log.Warn("api: the claim lost its process while its session was being compacted", "claim", token)
		writeFailure(w, http.StatusConflict, compactionFailed, fmt.Sprintf(
			"%s lost its process while its session was being compacted; it was not given back, and the task it owes goes when its agent runs again", token))
		return CompactResponse{}, false
	default:
		var refused *supervise.RefusedError
		if errors.As(err, &refused) {
			// The machine takes no compaction in the state it names: a claim in a turn, one
			// already being compacted, one whose agent is not running, one between states. The
			// answer is written from that state (refuseState).
			return CompactResponse{}, refuseState(w, token, refused.State, m.View().Claim.Compacting)
		}
		// Oh My Pi refused or skipped the compaction, the wait for it ran out, or the transport
		// failed. The claim is back and whatever task it owes has gone again, so the caller is
		// told why rather than told the request failed.
		reason := boundReason(runtime.RefusalReason(err))
		if reason == "" {
			reason = boundReason(err.Error())
		}
		s.log.Warn("api: the claim's session was not compacted; the claim is back and whatever task it owes was sent again",
			"claim", token, "reason", reason)
		return CompactResponse{Reason: reason}, true
	}
}

// boundReason is a compaction's reason cut to reasonBound bytes, on a rune boundary so the cut
// leaves no partial character behind, with an ellipsis where it cut so a reader sees that the
// sentence did not end there.
func boundReason(text string) string {
	if len(text) <= reasonBound {
		return text
	}
	cut := reasonBound
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// refuseState is the refusal the machine answered a compaction with, in the words of the state
// that refusal names (supervise.RefusedError.State). compacting is whether the claim is working
// because its session is being compacted rather than because a turn is, which is the one thing
// the state alone does not say (supervise.Claim.Compacting). It always reports false, so a
// caller returns it as its own answer.
func refuseState(w http.ResponseWriter, token claim.Token, state supervise.ClaimState, compacting bool) bool {
	switch {
	case state == supervise.StateWorking && compacting:
		writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
			"%s is being compacted already: wait for that compaction to end", token))
	case state == supervise.StateWorking:
		return refuseInATurn(w, token, compactOp)
	case startsAgain(state):
		writeFailure(w, http.StatusConflict, claimNotRunning, fmt.Sprintf(
			"%s is %s, so it runs no Oh My Pi session to compact; %s starts it again instead",
			token, state, freshSessionName))
	default:
		writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
			"%s is %s: a compaction reaches a claim whose agent is ready or idle; ask again when %s is one of those",
			token, state, token))
	}
	return false
}

// freshSessionName is the operation a refusal of a compaction names as the one that reaches the
// claim instead, spelled as both doors' callers know it.
const freshSessionName = "fresh_session (`legion claims retry --fresh-session`)"

package claudebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The Claude Code half of the turn-outcome report (dispatch://INFRA-956, the spec's path B), as
// packages/pi-legion/src/turn-outcome.ts is Oh My Pi's. Every turn the bridge ends is reported to
// the daemon over HTTP, under a sequence this producer counts itself, with the class read from
// the `result` line that ended the turn.
//
// The report and the turn's `prompt_result` are two halves of one answer, and the daemon reads
// both: the report says the class — whether the provider refused the turn or failed it some other
// way — and the `prompt_result` says what Claude Code returned, its message and the HTTP status
// (bridge.go's result). The rule that acts on a failed turn reads that failed turn's own report,
// the one above the (generation, sequence) the claim held when the error arrived
// (supervise's ownReport), so a turn with no report of its own is a turn the daemon waits two
// minutes over and then sends again — which is the Claude Code worker this report is for:
// without it, an error the provider marked final never held its issue, and `error_max_turns`
// was sent again with no limit.
//
// The caller is the launch's own boot token, the 0600 file the pane is told
// (claim.BootCaller): the pane's registration is claude-envoy's, inside Claude Code, and the
// secret that registration was issued never leaves that process.
//
// The report carries no `category`. The daemon takes one for a refusal and pi-legion sends it
// (claim.TurnOutcomeRequest), and Claude Code does name it — on the `system`
// `model_refusal_no_fallback` line two lines before the result, as `api_refusal_category`
// (testdata/refusal-turn.jsonl:3) — so the bridge could hold it from there to the turn's end. It
// does not: the class is what the decision reads, and the category only words the sentence a
// refused task is sent back behind (supervise's providerErrorCause), which falls back to the
// HTTP status the prompt_result already carries.

const (
	// turnOutcomePath is the route the report is posted to (internal/api's route table).
	turnOutcomePath = "/legion/v1/claims/turn-outcome"
	// reportTimeout bounds one report's call. The body is four short fields and the daemon
	// answers 204 with nothing of its own, so a call that takes longer than this is a daemon
	// restarting or a network that has gone, and the turn it is about is over either way.
	reportTimeout = 10 * time.Second
	// refusalBytes bounds how much of a refusal's body is logged: the daemon's refusals are one
	// sentence (claim.Refusal), and the log is the pane's.
	refusalBytes = 512
)

// stop_reason, terminal_reason and the subtype this reads of a `result` line, as the pinned
// Claude Code (2.1.310) writes them. Measured, not guessed: a refusal's result carries
// `stop_reason` "refusal" and `terminal_reason` "api_error"
// (testdata/refusal-turn.jsonl:5), and a 529's and a 429's carry `stop_reason`
// "stop_sequence" with the same `terminal_reason` (testdata/api-error-529.jsonl:5,
// testdata/api-error-429.jsonl:5), so the refusal is read first and every other failed result
// marked `api_error` is a provider error.
const (
	stopReasonRefusal = "refusal"
	terminalAPIError  = "api_error"
	// subtypeMaxTurns is Claude Code's own end of a turn it ran out of turns in, which is no
	// provider failure and is never reported as one: the provider took every request of that
	// turn. No `result` line of the measurement carries this subtype
	// (dispatch://INFRA-956/artifact/claude-code-2-1-310-refusal-measurement-tgz), so what
	// `terminal_reason` it comes with is unmeasured, and the subtype is excluded by name rather
	// than by trusting a field nothing has measured.
	subtypeMaxTurns = "error_max_turns"
)

// classify is the class the daemon reads for a turn Claude Code ended with res: a refusal the
// provider's content filter made, a provider failure of any other kind, or a turn that ended on
// nothing the provider refused or failed. A refusal is sent to the same session again on a weaker
// model, where it is often taken; a 429 or a malformed request may not clear that way, which is
// why the two never arrive under one word (claim.TurnOutcome).
func classify(res resultLine) claim.TurnOutcome {
	switch {
	case res.stopReason == stopReasonRefusal:
		return claim.TurnRefused
	case res.isError && res.terminalReason == terminalAPIError && res.subtype != subtypeMaxTurns:
		return claim.TurnProviderError
	default:
		return claim.TurnClean
	}
}

// turnReporter posts one report per turn the bridge ends. Everything but send runs on the
// bridge's one loop, so the sequence is counted without a lock.
type turnReporter struct {
	// ctx is the bridge's own termination: a report still in flight when the bridge is told to
	// stop is cancelled with it and logged, the process being on its way out.
	ctx        context.Context
	client     *http.Client
	url        string
	bootToken  string
	generation uint64
	log        *log.Logger
	// sequence is the last number given out, from 1 within generation.
	sequence uint64
}

// newTurnReporter is the reporter for cfg, and nil for a bridge the environment told nothing to
// report with — one run by hand, or a test — which is logged once and reports nothing. A pane
// always carries the three: its runtime sets them (config.go's Configure).
func newTurnReporter(ctx context.Context, cfg Config, logger *log.Logger) *turnReporter {
	var missing []string
	if cfg.DaemonURL == "" {
		missing = append(missing, "LEGION_DAEMON_URL")
	}
	if cfg.BootTokenFile == "" {
		missing = append(missing, "LEGION_BOOT_TOKEN_FILE")
	}
	if cfg.Generation == 0 {
		missing = append(missing, "LEGION_GENERATION")
	}
	if len(missing) > 0 {
		logger.Printf("reporting no turn outcome: the environment names no %s", strings.Join(missing, " and no "))
		return nil
	}
	body, err := os.ReadFile(cfg.BootTokenFile)
	if err != nil {
		logger.Printf("reporting no turn outcome: %v", err)
		return nil
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		logger.Printf("reporting no turn outcome: the boot token file %s is blank", cfg.BootTokenFile)
		return nil
	}
	return &turnReporter{
		ctx:        ctx,
		client:     &http.Client{},
		url:        cfg.DaemonURL + turnOutcomePath,
		bootToken:  token,
		generation: cfg.Generation,
		log:        logger,
	}
}

// report takes this turn's number and sends its class. The number is taken here, on the loop,
// before the send, so two turns can neither take the same number nor take them out of the order
// their turns ended in: the daemon keeps the newest report under (generation, sequence), and a
// report that arrives late lands in its own place.
//
// The send itself runs apart from the loop, which answers Claude Code and the shim while the
// daemon's answer is still coming: a turn ends when Claude Code says it does, not when the daemon
// has taken the report. A send the daemon refuses is logged and ends nothing — there is nothing
// the bridge can do about a refusal, and the turn is over.
func (r *turnReporter) report(outcome claim.TurnOutcome) {
	r.sequence++
	sequence := r.sequence
	body, err := json.Marshal(claim.TurnOutcomeRequest{
		BootCaller: claim.BootCaller{BootToken: r.bootToken},
		Generation: r.generation,
		Sequence:   sequence,
		Outcome:    outcome,
	})
	if err != nil {
		r.log.Printf("could not build the turn outcome report for turn %d: %v", sequence, err)
		return
	}
	go r.send(body, outcome, sequence)
}

// send posts one report, logging what the daemon made of it. The boot token is in the body and
// in no log line.
func (r *turnReporter) send(body []byte, outcome claim.TurnOutcome, sequence uint64) {
	ctx, done := context.WithTimeout(r.ctx, reportTimeout)
	defer done()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		r.log.Printf("could not build the turn outcome request for turn %d: %v", sequence, err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		r.log.Printf("the %s outcome of turn %d did not reach the daemon: %v", outcome, sequence, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		refusal, _ := io.ReadAll(io.LimitReader(response.Body, refusalBytes))
		r.log.Printf("the daemon refused the %s outcome of turn %d with %d: %s", outcome, sequence,
			response.StatusCode, strings.TrimSpace(string(refusal)))
		return
	}
	// The route answers 204 and nothing else, so there is no body to read.
	r.log.Printf("reported turn %d of generation %d as %s", sequence, r.generation, outcome)
}

package going

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/contracts"
	"golang.org/x/time/rate"
)

// charsPerToken is how many characters of a request are counted as one token. It is
// embed.EstimateTokens' own rule, cited rather than imported: that package links the AWS SDK and
// this one stays thin enough for the daemon to speak the same stream. Four characters a token is
// a rough estimate for English prose, which is what pacing a shared quota needs and all it needs;
// it is never exact, and dense text costs more.
const charsPerToken = 4

// answerRules is the system message every run sends: the rules an answer keeps, stated once
// because they never change between runs. It names the bounds and the tool from contracts, so the
// rules the model is given and the bounds the server enforces cannot drift apart.
var answerRules = fmt.Sprintf(`You answer one question from the items you are given, which is what Dispatch holds on it.
Answer in at most %d words. An answer past that is cut where it passes them.
Answer only from the items given to you. When they do not hold the answer, say that Dispatch holds no answer to the question; never answer from anything else you know.
Link each item you use as a markdown link whose target is that item's own href, exactly as it is given to you.
Never link anything else, and write no bare web address, host or email address in your words: a link to any other target reaches the reader as its own visible text alone, with the target dropped, and a word a reader's renderer would make a link of on its own is dropped from the answer whole, with the reader told the answer was changed.
Call %s to search Dispatch again when the items you have do not answer the question. It takes q, the words to search for.`,
	contracts.GoingAnswerWordMax, contracts.GoingSearchTool)

// searchToolSchema is the one tool's arguments: the words to search for, and nothing else. The
// server supplies the project, the limit and the offset, so there is no field for the model to
// widen a search with.
var searchToolSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"q": map[string]any{
			"type":        "string",
			"description": "The words to search Dispatch for.",
		},
	},
	"required":             []string{"q"},
	"additionalProperties": false,
}

// errQuotaPastDeadline is a call the quota cannot pace inside the time the run has left. It wraps
// context.DeadlineExceeded because that is what it is - the run reaching its deadline - which is
// the issue's one deadline ending and needs no second code of its own.
var errQuotaPastDeadline = fmt.Errorf("going: the quota cannot pace this call inside the time left: %w", context.DeadlineExceeded)

// Answerer answers one question from what Dispatch holds. It is built once at boot from the
// settings and used by every request, so its quota pacer is the process's one pacer.
type Answerer struct {
	model Client
}

// AnswererConfig configures an Answerer.
type AnswererConfig struct {
	// Model is the chat client; required.
	Model Client
	// TokensPerMinute is the deployment's quota, which paces this process's calls. Zero or less
	// is no pacing: the endpoint form names no quota.
	TokensPerMinute int
	// Now replaces time.Now; a test drives the pacer with it.
	Now func() time.Time
}

// NewAnswerer checks cfg's shape and returns an Answerer. It makes no call, so a model service
// that is down at boot never refuses boot.
func NewAnswerer(cfg AnswererConfig) (*Answerer, error) {
	if cfg.Model == nil {
		return nil, errors.New("going: an answerer needs a chat client")
	}
	model := cfg.Model
	if cfg.TokensPerMinute > 0 {
		now := cfg.Now
		if now == nil {
			now = time.Now
		}
		model = &pacedClient{
			inner:   cfg.Model,
			now:     now,
			limiter: rate.NewLimiter(rate.Limit(float64(cfg.TokensPerMinute)/60), cfg.TokensPerMinute),
		}
	}
	return &Answerer{model: model}, nil
}

// Request is one question and the two reads the answerer makes through its caller, which is what
// lets this package stay outside internal/ while the reads live in the API package. Both reads
// are required.
type Request struct {
	// Words is the question.
	Words string
	// Search answers one search of the caller's project at the caller's limit and offset: the
	// server supplies those, so the model cannot widen the search past what the caller may read.
	Search func(ctx context.Context, q string) ([]Hit, error)
	// Bodies answers each hit's own stored text, keyed by Hit.Key, already cut to
	// contracts.GoingHitRuneMax runes per hit. A hit it answers nothing for has no body.
	Bodies func(ctx context.Context, hits []Hit) (map[string]string, error)
}

// Run answers Words, emitting the stream's events in order with exactly one terminal event last.
// The error it returns is emit's alone: everything else that fails is an event.
func (a *Answerer) Run(ctx context.Context, request Request, emit func(Event) error) error {
	run := &answerRun{
		request:   request,
		emit:      emit,
		citations: make(map[string]Hit),
		runesLeft: contracts.GoingRequestRuneMax,
	}
	// The server's own seeding search is the first of contracts.GoingSearchMax: the model gets
	// what the question itself found before it is asked anything.
	items, err := run.search(ctx, request.Words)
	if err != nil {
		return run.readFault(ctx, err)
	}

	// One filter for the whole run, so every content delta of every turn is cleared by the same
	// state machine and no half of a link the server would drop ever reaches a reader.
	filter := newAnswerFilter(run.citations, run.send)
	// The loop runs on a context of this run's own, which a failed store read cancels: a search
	// the store could not serve leaves the run nothing further to give the model, so no call is
	// made after it (runLoop checks the context before each one).
	answering, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	run.stop = stopLoop
	result, runErr := runLoop(answering, loopRequest{
		Model:  a.model,
		System: answerRules,
		Input:  []Message{{Role: "user", Content: run.ask(items)}},
		Tools:  []loopTool{run.searchTool()},
		Bounds: loopBounds{Time: contracts.GoingRunWindow, Calls: contracts.GoingModelCallMax},
		OnText: filter.Write,
	})
	// runErr is a callback's error or the context ending. The one callback the loop is given
	// here is the filter's Write, whose error is emit's own, which run.send kept: that is the
	// one error this run hands its caller, and it is handed back unwrapped.
	if run.emitErr != nil {
		return run.emitErr
	}
	// A failed store read ends the run, and it is read before the loop's own fault: the fault
	// is this run having stopped the loop at that read, and what the reader is owed is the
	// read's own code. The run ends here and nowhere else - no second model call was made, and
	// no tool result told the model the search merely found nothing - so a reader is never
	// written a whole answer this run then disowns with an error. Text the filter had already
	// cleared and emitted stays emitted: it was cleared, and an error frame is terminal, so no
	// end follows it.
	if run.readErr != nil {
		return run.readFault(ctx, run.readErr)
	}
	switch result.Fault {
	case loopFaultTime:
		// The reader is told one code for the whole ending, so runErr - the run's window having
		// run out, or a call the quota could not pace inside what was left of it
		// (errQuotaPastDeadline) - is said once here, as the provider's refusal is below. It
		// carries no credential and none of the question's words.
		slog.Warn("going: the run ran out of time with no answer", "error", runErr)
		return run.send(Fault(contracts.GoingErrorAnswerDidNotArrive))
	case loopFaultProviderError:
		// The reader is told ANSWER_FAILED and no more than that, so this is the one place the
		// refusal is said: an operator whose deployment answers 401 or 429 reads it here and
		// nowhere else. The detail is what the model service said of the call, which runLoop
		// built free of any credential, and neither the question's words nor a credential is
		// named beside it.
		slog.Warn("going: the model service refused the run", "detail", result.Detail)
		return run.send(Fault(contracts.GoingErrorAnswerFailed))
	}
	// The model answered, or it spent the call budget: either way what the filter cleared up to
	// here is a whole answer - the budget is the budget - so the run ends through the filter's
	// own Close and not with an error frame.
	return filter.Close()
}

// answerRun is one run's own state: what the model has been given, what is left of the run's
// budgets, and the failures that end the run.
type answerRun struct {
	request Request
	emit    func(Event) error

	// citations is every hit given to the model, with or without a body, keyed by href as the
	// filter reads it. The filter holds this same map, so a hit a later search found is citable.
	citations map[string]Hit
	// searches counts the searches made, the seeding search included; runesLeft is what is left
	// of the run's body budget, and spent says it is gone, so the hits after the first body that
	// did not fit are named to the model without one.
	searches  int
	runesLeft int
	spent     bool

	// readErr is the first store read that failed, which ends the run: Run sends its code as
	// the run's one ending, and stop has already kept the loop from making another call. The
	// first is kept because it is the one that ended the run - a read made after stop answers
	// context.Canceled, which would say the run ran out of time rather than that the store
	// failed.
	readErr error
	// stop ends the loop's model calls. Run holds the cancel of the context it runs the loop
	// on, so a tool that ends the run stops it there rather than letting one more call be made.
	stop context.CancelFunc
	// emitErr is the stream writer's failure, the one failure Run hands back to its caller.
	emitErr error
}

// send hands one event to the stream's writer and keeps its failure, which is the only error a
// run reports to its caller.
func (r *answerRun) send(event Event) error {
	if err := r.emit(event); err != nil {
		r.emitErr = err
		return err
	}
	return nil
}

// readFault ends the run on a store read that failed: the reader is told one code, and the
// read's own error is said here, where the run's time fault and the provider's refusal are
// said, so an operator whose store refused a read of an answer reads why. It is the one place
// that error is logged, and it carries no credential and none of the question's words.
func (r *answerRun) readFault(ctx context.Context, err error) error {
	slog.Warn("going: a store read the answer needed failed", "error", err)
	return r.send(Fault(readFaultCode(ctx, err)))
}

// search makes one search of the run and renders its hits for the model. Every hit found joins
// the citation set, with or without a body; the bodies are spent in rank order against the run's
// one rune budget, so a hit past it is still named to the model, with its kind, its id and its
// title and no body.
func (r *answerRun) search(ctx context.Context, q string) (string, error) {
	r.searches++
	hits, err := r.request.Search(ctx, q)
	if err != nil {
		return "", err
	}
	bodies, err := r.request.Bodies(ctx, hits)
	if err != nil {
		return "", err
	}
	var items strings.Builder
	for _, hit := range hits {
		r.citations[hit.Href] = hit
		body := bodies[hit.Key()]
		if body != "" {
			if runes := utf8.RuneCountInString(body); r.spent || runes > r.runesLeft {
				r.spent = true
				body = ""
			} else {
				r.runesLeft -= runes
			}
		}
		writeItem(&items, hit, body)
	}
	return items.String(), nil
}

// searchTool is the one tool the model may call. Its own Run closure holds the search budget,
// because a tool's budget is the tool's own logic: a search past contracts.GoingSearchMax is
// refused back to the model, in words it can act on, and ends no run. A search the store could
// not serve is the one thing that does end it: the alternative is telling the model to answer
// from what it has and then disowning the answer it writes, which is an answer a reader was
// given and then told to ignore.
func (r *answerRun) searchTool() loopTool {
	return loopTool{
		Name: contracts.GoingSearchTool,
		Description: fmt.Sprintf(
			"Search Dispatch again for the words q. One run searches at most %d times, and the search that found the items you were given was the first.",
			contracts.GoingSearchMax),
		Schema: searchToolSchema,
		Run: func(ctx context.Context, call ToolCall) (string, bool) {
			var arguments struct {
				Q string `json:"q"`
			}
			if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil || strings.TrimSpace(arguments.Q) == "" {
				return "That call named no words to search for. Call it again with q, the words to search Dispatch for.", true
			}
			if r.searches >= contracts.GoingSearchMax {
				return fmt.Sprintf(
					"No search is left: one run searches Dispatch at most %d times, and the search that found the items you were given was the first. Answer from the items you have, or say that Dispatch holds no answer.",
					contracts.GoingSearchMax), false
			}
			items, err := r.search(ctx, arguments.Q)
			if err != nil {
				// The run ends on this read, so the loop makes no further call and this result
				// reaches no model (Answerer.Run reads readErr before the loop's own fault).
				// The first failure stands: a tool call of this same turn, run before the loop
				// reads the ended context, would answer context.Canceled and rename the ending.
				if r.readErr == nil {
					r.readErr = err
				}
				r.stop()
				return "That search could not be served. This run ends here.", true
			}
			if items == "" {
				return "That search found no item. Answer from the items you have, or say that Dispatch holds no answer.", false
			}
			return "That search found these items:\n" + items, false
		},
	}
}

// ask is the question as the model is asked it: the words, then every item the seeding search
// found.
func (r *answerRun) ask(items string) string {
	var out strings.Builder
	out.WriteString("question: ")
	out.WriteString(r.request.Words)
	out.WriteString("\n\nThe items Dispatch holds for it:\n")
	if items == "" {
		out.WriteString("\nNone: the search for this question found nothing.\n")
		return out.String()
	}
	out.WriteString(items)
	return out.String()
}

// writeItem renders one hit as the model reads it: what it is, how it is linked, and its own text
// when the run's budget had room for it.
func writeItem(out *strings.Builder, hit Hit, body string) {
	out.WriteString("\n--- item\nkind: ")
	out.WriteString(hit.Kind)
	out.WriteString("\nid: ")
	out.WriteString(hit.ID)
	out.WriteString("\ntitle: ")
	out.WriteString(hit.Title)
	out.WriteString("\nhref: ")
	out.WriteString(hit.Href)
	out.WriteString("\n")
	if body == "" {
		return
	}
	out.WriteString("body:\n")
	out.WriteString(body)
	out.WriteString("\n")
}

// readFaultCode names what a failed store read ends the run with: the run ran out of time, or the
// store failed.
func readFaultCode(ctx context.Context, err error) string {
	if isDeadline(ctx, err) {
		return contracts.GoingErrorAnswerDidNotArrive
	}
	return contracts.GoingErrorAnswerFailed
}

// pacedClient charges the deployment's quota for a call before it makes it, so this process never
// spends more of that quota a minute than the deployment has: one rate.Limiter, as
// embed.RateLimitedEmbedder paces the embedder, sized at the tokens a minute with a burst of a
// minute's quota, charged with the call's estimated tokens.
//
// The swap to agentloop (dispatch://INFRA-1311) moves the reservation rather than keeping this
// wrapper: agentloop builds its own transport from an Endpoint whose Token closure it calls on
// every HTTP attempt, so the reservation is taken in that closure, which paces the retries of a
// call too - this wrapper cannot, since it sees one Chat and not the attempts inside it.
type pacedClient struct {
	inner   Client
	now     func() time.Time
	limiter *rate.Limiter
}

var _ Client = (*pacedClient)(nil)

// Chat waits out the quota's own pacing and then makes the call. A reservation that cannot be
// served inside the time the run has left is refused rather than waited on, because waiting it
// out would spend the whole window to make no call at all.
func (p *pacedClient) Chat(ctx context.Context, request ChatRequest, onText func(string) error) ([]ToolCall, error) {
	reservation := p.limiter.ReserveN(p.now(), estimateTokens(request))
	if !reservation.OK() {
		// The call costs more than a minute's whole quota, so no wait would ever serve it.
		return nil, errQuotaPastDeadline
	}
	if delay := reservation.DelayFrom(p.now()); delay > 0 {
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline && p.now().Add(delay).After(deadline) {
			reservation.CancelAt(p.now())
			return nil, errQuotaPastDeadline
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			reservation.CancelAt(p.now())
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return p.inner.Chat(ctx, request, onText)
}

// estimateTokens is what one call is charged against the quota before it is made: the characters
// it sends, at charsPerToken. It counts the request alone, which is what a call can be charged
// for before its reply exists.
func estimateTokens(request ChatRequest) int {
	chars := 0
	for _, message := range request.Messages {
		chars += utf8.RuneCountInString(message.Content)
		for _, call := range message.ToolCalls {
			chars += utf8.RuneCountInString(call.Name) + utf8.RuneCountInString(call.Arguments)
		}
	}
	for _, tool := range request.Tools {
		chars += utf8.RuneCountInString(tool.Name) + utf8.RuneCountInString(tool.Description)
	}
	return chars / charsPerToken
}

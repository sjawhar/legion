package going_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logtest"
)

// testTokensPerMinute is a quota far past anything one test's prompts cost, so every test but the
// quota's own exercises the pacer without ever waiting on it.
const testTokensPerMinute = 300_000

// The two items a test's seeding search finds, and one target no citation set holds: the filter
// admits a link by its href alone, so the href is what a reply links.
var (
	routeHit = going.Hit{
		Kind:  "issue",
		ID:    "INFRA-1279",
		Title: "The answer route",
		Href:  "/issues/INFRA-1279",
	}
	planHit = going.Hit{
		Kind:  "artifact",
		ID:    "plan-md",
		Title: "The plan",
		Href:  "/issues/INFRA-1279/artifact/plan-md",
	}
)

const outsideAnswerHref = "/issues/INFRA-9999"

// errStreamWriter is the failure a test's stream writer hands back, which is the one failure Run
// reports to its caller.
var errStreamWriter = errors.New("the reader's connection is gone")

// answerStream is everything one run emitted, so a test reads the stream as a reader does. It
// fails at the event failAt names, by its number or by its name, which is how a test proves a run
// hands the writer's failure back.
type answerStream struct {
	events   []going.Event
	failAt   int
	failName string
}

func (s *answerStream) emit(event going.Event) error {
	number := len(s.events) + 1
	if (s.failAt > 0 && number == s.failAt) || (s.failName != "" && event.Name == s.failName) {
		return errStreamWriter
	}
	s.events = append(s.events, event)
	return nil
}

// names is every event's name in the order it was emitted.
func (s *answerStream) names() []string {
	out := make([]string, len(s.events))
	for i, event := range s.events {
		out[i] = event.Name
	}
	return out
}

// text is the whole answer a reader saw, which a dropped link must hold no byte of.
func (s *answerStream) text() string {
	var out strings.Builder
	for _, event := range s.events {
		if data, ok := event.Data.(going.TextData); ok {
			out.WriteString(data.Text)
		}
	}
	return out.String()
}

// count is how many events of that name the stream holds.
func (s *answerStream) count(name string) int {
	found := 0
	for _, event := range s.events {
		if event.Name == name {
			found++
		}
	}
	return found
}

// first is the first event of that name, and fails the test when the stream holds none.
func (s *answerStream) first(t *testing.T, name string) going.Event {
	t.Helper()
	for _, event := range s.events {
		if event.Name == name {
			return event
		}
	}
	t.Fatalf("stream holds no %s event, only %v", name, s.names())
	return going.Event{}
}

// terminal checks the one rule every run keeps - exactly one terminal event, and last - and
// answers it.
func (s *answerStream) terminal(t *testing.T) going.Event {
	t.Helper()
	terminals := 0
	for _, event := range s.events {
		if event.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("stream holds %d terminal events, want exactly 1: %v", terminals, s.names())
	}
	last := s.events[len(s.events)-1]
	if !last.Terminal() {
		t.Fatalf("stream ends with %s, want its terminal event last: %v", last.Name, s.names())
	}
	return last
}

// code is the error frame's own code, and fails the test when the run ended any other way.
func (s *answerStream) code(t *testing.T) string {
	t.Helper()
	event := s.terminal(t)
	data, ok := event.Data.(going.ErrorData)
	if !ok {
		t.Fatalf("stream ended with %s %+v, want an error frame", event.Name, event.Data)
	}
	return data.Code
}

// reads is the two reads a run makes through its caller: it answers every search with the same
// hits and counts the words each search was made with, which is how a test sees the run's own
// seeding search counted as the first.
type reads struct {
	hits    []going.Hit
	bodies  map[string]string
	queries []string
	err     error
	// served is how many searches are answered before err is: zero fails the run's own seeding
	// search, and one fails the first search the model asks for.
	served int
}

// request is one question over these reads.
func (r *reads) request(words string) going.Request {
	return going.Request{
		Words: words,
		Search: func(ctx context.Context, q string) ([]going.Hit, error) {
			r.queries = append(r.queries, q)
			// A real store read answers the context's own ending first (pgx does), which is
			// what makes a read taken after the run was stopped context.Canceled rather than
			// the store's own refusal.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if r.err != nil && len(r.queries) > r.served {
				return nil, r.err
			}
			return r.hits, nil
		},
		Bodies: func(_ context.Context, hits []going.Hit) (map[string]string, error) {
			found := make(map[string]string, len(hits))
			for _, hit := range hits {
				if body, ok := r.bodies[hit.Key()]; ok {
					found[hit.Key()] = body
				}
			}
			return found, nil
		},
	}
}

// testReads is the question's own hits, each with a short body.
func testReads() *reads {
	return &reads{
		hits: []going.Hit{routeHit, planHit},
		bodies: map[string]string{
			routeHit.Key(): "The route answers one question from what Dispatch holds.",
			planHit.Key():  "The plan counts the server's own search as the first of three.",
		},
	}
}

// testAnswerer builds an Answerer over the real HTTP client against model, so every test drives
// the client's own route, sign-in and reading of the stream.
func testAnswerer(t *testing.T, model *goingtest.Model, cfg going.AnswererConfig) *going.Answerer {
	t.Helper()
	cfg.Model = testClient(t, model, going.HTTPConfig{})
	if cfg.TokensPerMinute == 0 {
		cfg.TokensPerMinute = testTokensPerMinute
	}
	answerer, err := going.NewAnswerer(cfg)
	if err != nil {
		t.Fatalf("NewAnswerer: %v", err)
	}
	return answerer
}

// searchCall is a turn that calls the one tool with q.
func searchCall(id, q string) goingtest.Reply {
	return goingtest.Reply{ToolCalls: []going.ToolCall{{
		ID:        id,
		Name:      contracts.GoingSearchTool,
		Arguments: fmt.Sprintf(`{"q":%q}`, q),
	}}}
}

// answerWords is a reply of n distinct words, so a cut can be located in the answer.
func answerWords(n int) string {
	var out strings.Builder
	for i := range n {
		if i > 0 {
			out.WriteByte(' ')
		}
		fmt.Fprintf(&out, "word%d", i)
	}
	return out.String()
}

// TestNewAnswererRefusesAnswererWithoutAModel: an answerer with no chat client is a programming
// error, refused at boot rather than at the first question.
func TestNewAnswererRefusesAnswererWithoutAModel(t *testing.T) {
	if _, err := going.NewAnswerer(going.AnswererConfig{TokensPerMinute: testTokensPerMinute}); err == nil {
		t.Fatal("NewAnswerer with no model returned no error")
	}
}

// TestRunKeepsALinkToAnItemItWasGiven: the model links a hit it was given, the link reaches the
// reader as the item frame that names that hit and as the link's own visible text, which the
// text frames carry as plain text, and end is last.
func TestRunKeepsALinkToAnItemItWasGiven(t *testing.T) {
	link := fmt.Sprintf("[%s](%s)", routeHit.Title, routeHit.Href)
	model := goingtest.NewModel(t, goingtest.Reply{Text: "The order is settled on " + link + ".", Chunk: 5})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.terminal(t).Name; got != contracts.GoingEventEnd {
		t.Fatalf("stream ended with %s, want %s: %v", got, contracts.GoingEventEnd, stream.names())
	}
	if got := stream.text(); !strings.Contains(got, routeHit.Title) {
		t.Errorf("answer = %q, want it to hold the link's own text %q", got, routeHit.Title)
	}
	if got := stream.text(); strings.Contains(got, routeHit.Href) || strings.Contains(got, "](") {
		t.Errorf("answer = %q, want no link syntax in a text frame: the item frame is the link", got)
	}
	if got := stream.count(contracts.GoingEventItem); got != 1 {
		t.Fatalf("stream holds %d item events, want 1: %v", got, stream.names())
	}
	item, ok := stream.first(t, contracts.GoingEventItem).Data.(going.ItemData)
	if !ok || item.Href != routeHit.Href || item.ID != routeHit.ID {
		t.Errorf("item = %+v, want the hit the answer linked (%s %s)", item, routeHit.ID, routeHit.Href)
	}
	if got := stream.count(contracts.GoingEventChanged) + stream.count(contracts.GoingEventNone); got != 0 {
		t.Errorf("stream holds %d changed or none events, want none: %v", got, stream.names())
	}
	if len(search.queries) != 1 || search.queries[0] != "who owns the broadcast order" {
		t.Errorf("searches = %q, want the one seeding search with the question's words", search.queries)
	}
}

// TestRunCutsAReplyPastTheWordCap: a reply far past the cap is cut at
// contracts.GoingAnswerWordMax, cut says so with that count, and end still follows - a cut answer
// is an answer.
func TestRunCutsAReplyPastTheWordCap(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Text: answerWords(300), Chunk: 7})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("how long may an answer be"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.terminal(t).Name; got != contracts.GoingEventEnd {
		t.Fatalf("stream ended with %s, want %s: %v", got, contracts.GoingEventEnd, stream.names())
	}
	cut, ok := stream.first(t, contracts.GoingEventCut).Data.(going.CutData)
	if !ok || cut.Words != contracts.GoingAnswerWordMax {
		t.Errorf("cut = %+v, want the answer cut at %d words", cut, contracts.GoingAnswerWordMax)
	}
	if got := len(strings.Fields(stream.text())); got != contracts.GoingAnswerWordMax {
		t.Errorf("answer carries %d words, want %d", got, contracts.GoingAnswerWordMax)
	}
}

// TestRunNeverEmitsALinkOutsideTheCitationSet: a link to something the model was never given
// reaches a reader as its own text and no more - no target, no link syntax, no item - and the
// stream says the answer was changed.
func TestRunNeverEmitsALinkOutsideTheCitationSet(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{
		Text:  "The order is settled on [another issue](" + outsideAnswerHref + ") instead.",
		Chunk: 6,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.terminal(t).Name; got != contracts.GoingEventEnd {
		t.Fatalf("stream ended with %s, want %s: %v", got, contracts.GoingEventEnd, stream.names())
	}
	changed, ok := stream.first(t, contracts.GoingEventChanged).Data.(going.ChangedData)
	if !ok || changed.Dropped != 1 {
		t.Errorf("changed = %+v, want one dropped link", changed)
	}
	if got := stream.text(); strings.Contains(got, outsideAnswerHref) || strings.Contains(got, "](") {
		t.Errorf("answer = %q, want no target and no link syntax of the link the server dropped", got)
	}
	if got := stream.count(contracts.GoingEventItem); got != 0 {
		t.Errorf("stream holds %d item events, want none for an answer that cited nothing", got)
	}
}

// TestRunSaysNoneWhenTheModelWroteNothing: none is the model producing no answer text at all,
// and nothing else. A run whose model answered with no text says none, then ends; a run whose
// answer linked nothing is still an answer, and its end's items of 0 is what says it rests on
// no source.
func TestRunSaysNoneWhenTheModelWroteNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		none bool
	}{
		{"the model wrote nothing", "", true},
		{"the answer links nothing", "Dispatch holds no answer to that question.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := goingtest.NewModel(t, goingtest.Reply{Text: tc.text})
			answerer := testAnswerer(t, model, going.AnswererConfig{})
			stream := &answerStream{}

			if err := answerer.Run(t.Context(), testReads().request("what did nobody write down"), stream.emit); err != nil {
				t.Fatalf("Run: %v", err)
			}

			end, ok := stream.terminal(t).Data.(going.EndData)
			if !ok || end.Items != 0 {
				t.Errorf("end = %+v, want an end citing nothing", end)
			}
			want := 0
			if tc.none {
				want = 1
			}
			if got := stream.count(contracts.GoingEventNone); got != want {
				t.Errorf("stream holds %d none events, want %d: %v", got, want, stream.names())
			}
			if tc.none && stream.names()[len(stream.names())-2] != contracts.GoingEventNone {
				t.Errorf("stream ends %v, want %s before its end", stream.names(), contracts.GoingEventNone)
			}
		})
	}
}

// TestRunCountsTheSeedingSearchAsTheFirst: the server's own search is the first of
// contracts.GoingSearchMax, so a model that calls the tool twice is served twice and its third
// call comes back as a refusal it can act on, with the run still ending in an answer.
func TestRunCountsTheSeedingSearchAsTheFirst(t *testing.T) {
	model := goingtest.NewModel(t,
		searchCall("call_1", "the broadcast order"),
		searchCall("call_2", "who owns it"),
		searchCall("call_3", "once more"),
		goingtest.Reply{Text: fmt.Sprintf("It is on [%s](%s).", routeHit.Title, routeHit.Href)},
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.terminal(t).Name; got != contracts.GoingEventEnd {
		t.Fatalf("stream ended with %s, want %s: %v", got, contracts.GoingEventEnd, stream.names())
	}
	want := []string{"who owns the broadcast order", "the broadcast order", "who owns it"}
	if len(search.queries) != contracts.GoingSearchMax || !reflect.DeepEqual(search.queries, want) {
		t.Errorf("searches = %q, want the %d of %v", search.queries, contracts.GoingSearchMax, want)
	}
	if got := model.Calls(); got != 4 {
		t.Fatalf("model served %d calls, want the three tool turns and the answer", got)
	}
	refusal := toolResult(t, model.Requests()[3].Messages, "call_3")
	if !strings.Contains(refusal, "No search is left") {
		t.Errorf("the third tool call was answered %q, want a refusal the model can act on", refusal)
	}
	if got := stream.count(contracts.GoingEventItem); got != 1 {
		t.Errorf("stream holds %d item events, want the one the answer linked", got)
	}
}

// toolResult is the result one tool call was answered with, as the model received it on the turn
// after it.
func toolResult(t *testing.T, messages []going.Message, callID string) string {
	t.Helper()
	for _, message := range messages {
		if message.ToolCallID == callID {
			return message.Content
		}
	}
	t.Fatalf("the turn carried no result for tool call %s", callID)
	return ""
}

// TestRunTellsTheModelACallFailedInTheResultsOwnText: a tool result that is an error reaches
// the model as text, behind the prefix, because the chat completions wire carries no field for
// one; the model is given the refusal and answers from what it has, so such a result ends no
// run.
func TestRunTellsTheModelACallFailedInTheResultsOwnText(t *testing.T) {
	model := goingtest.NewModel(t,
		goingtest.Reply{ToolCalls: []going.ToolCall{{
			ID:        "call_1",
			Name:      contracts.GoingSearchTool,
			Arguments: `{"not-the-field":"the broadcast order"}`,
		}}},
		goingtest.Reply{Text: fmt.Sprintf("It is on [%s](%s).", routeHit.Title, routeHit.Href)},
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.terminal(t).Name; got != contracts.GoingEventEnd {
		t.Fatalf("stream ended with %s, want %s: the run goes on past a refused call: %v",
			got, contracts.GoingEventEnd, stream.names())
	}
	if got := model.Calls(); got != 2 {
		t.Fatalf("model served %d calls, want the refused tool turn and the answer", got)
	}
	refusal := toolResult(t, model.Requests()[1].Messages, "call_1")
	if !strings.HasPrefix(refusal, "Error: ") {
		t.Errorf("the failed call was answered %q, want it to open with the error prefix", refusal)
	}
	if !strings.Contains(refusal, "Call it again with q") {
		t.Errorf("the failed call was answered %q, want the words the model can act on", refusal)
	}
	if len(search.queries) != 1 {
		t.Errorf("searches = %q, want the seeding search alone: the call named no words", search.queries)
	}
}

// TestRunEndsWithTheDeadlineWhenTheModelNeverAnswers: a model that holds its response open past
// the caller's deadline ends the run with the issue's one deadline code, and the run takes the
// time the caller gave it rather than ending early.
func TestRunEndsWithTheDeadlineWhenTheModelNeverAnswers(t *testing.T) {
	const window = 250 * time.Millisecond
	model := goingtest.NewModel(t, goingtest.Reply{Silent: true})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()

	start := time.Now()
	err := answerer.Run(ctx, testReads().request("who owns the broadcast order"), stream.emit)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stream.code(t); got != contracts.GoingErrorAnswerDidNotArrive {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerDidNotArrive)
	}
	if elapsed < window {
		t.Errorf("the run ended %s in, before its %s deadline", elapsed, window)
	}
}

// TestRunEndsWithTheDeadlineWhenTheQuotaCannotPaceTheCall: a quota too small to pace the next
// call inside the time left ends the run the same way, and no call is made.
func TestRunEndsWithTheDeadlineWhenTheQuotaCannotPaceTheCall(t *testing.T) {
	frozen := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	model := goingtest.NewModel(t, goingtest.Reply{Text: "An answer nobody will see."})
	answerer := testAnswerer(t, model, going.AnswererConfig{
		TokensPerMinute: 1,
		Now:             func() time.Time { return frozen },
	})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerDidNotArrive {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerDidNotArrive)
	}
	if got := model.Calls(); got != 0 {
		t.Errorf("model served %d calls, want none: the quota could pace none of them", got)
	}
}

// TestRunFailsWhenTheModelRefusesTheCall: a model service that answers a non-2xx ends the run
// with the failure code, and the reader is told nothing the service said.
func TestRunFailsWhenTheModelRefusesTheCall(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Status: http.StatusInternalServerError})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
}

// TestRunFailsWhenTheStoreCannotBeRead: the seeding search is a store read, and a store that
// cannot answer it ends the run with the failure code.
func TestRunFailsWhenTheStoreCannotBeRead(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Text: "An answer nobody will see."})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()
	search.err = errors.New("the store refused this read")

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
	if got := model.Calls(); got != 0 {
		t.Errorf("model served %d calls, want none: there was nothing to show it", got)
	}
}

// TestRunEndsWhenAToolsSearchCannotBeRead: a search the model asked for that the store could not
// serve ends the run there, with the failure code and no further model call. The run must not do
// both: stream the answer a further call would write and then disown it with ANSWER_FAILED.
func TestRunEndsWhenAToolsSearchCannotBeRead(t *testing.T) {
	model := goingtest.NewModel(t,
		searchCall("call_1", "who owns it"),
		goingtest.Reply{Text: "An answer nobody should ever be written."},
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()
	// The run's own seeding search is served; the one the model asks for is not.
	search.served = 1
	search.err = errors.New("the store refused this read")

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
	if got := model.Calls(); got != 1 {
		t.Errorf("model served %d calls, want the one turn that called the tool: the run ended on that search", got)
	}
	if got := stream.count(contracts.GoingEventText); got != 0 {
		t.Errorf("stream holds %d text events, want none: the run ended before any answer was written", got)
	}
}

// TestRunEndsOnTheFirstFailedReadOfATurnsCalls: a turn that calls the tool twice and whose first
// search the store could not serve ends on that read - the reader is told ANSWER_FAILED, the
// second call is never run, and the store's own error is logged once. The second search would
// run under the context the first one ended, so it could only answer context.Canceled: a run
// that let it would tell the reader its answer did not arrive in time, which is not what
// happened, and would say nothing at all about the store.
func TestRunEndsOnTheFirstFailedReadOfATurnsCalls(t *testing.T) {
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	model := goingtest.NewModel(t,
		goingtest.Reply{ToolCalls: []going.ToolCall{
			{ID: "call_1", Name: contracts.GoingSearchTool, Arguments: `{"q":"who owns it"}`},
			{ID: "call_2", Name: contracts.GoingSearchTool, Arguments: `{"q":"and who answers it"}`},
		}},
		goingtest.Reply{Text: "An answer nobody should ever be written."},
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()
	// The run's own seeding search is served; both searches of the turn after it are not.
	search.served = 1
	search.err = errors.New("the store refused this read")

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s: the store failed, the run did not run out of time", got, contracts.GoingErrorAnswerFailed)
	}
	if len(search.queries) != 2 {
		t.Errorf("searches = %q, want the seeding search and the first of the turn's two calls", search.queries)
	}
	if got := model.Calls(); got != 1 {
		t.Errorf("model served %d calls, want the one turn that called the tool: the run ended on that search", got)
	}
	if got := strings.Count(logged.String(), search.err.Error()); got != 1 {
		t.Errorf("the store's error is logged %d times in %q, want once", got, logged.String())
	}
}

// TestRunGivesAHitPastTheRuneBudgetNoBody: the bodies are spent in rank order against the run's
// one budget of contracts.GoingRequestRuneMax runes, and the hit past it still reaches the model
// with its kind, its id and its title - what the model received is what the test asserts.
func TestRunGivesAHitPastTheRuneBudgetNoBody(t *testing.T) {
	const marker = "marker-of-item-"
	// One hit more than the budget has room for, each body cut to the per-hit cap as the caller
	// hands them over.
	count := contracts.GoingRequestRuneMax/contracts.GoingHitRuneMax + 1
	search := &reads{bodies: make(map[string]string, count)}
	for i := range count {
		hit := going.Hit{
			Kind:  "issue",
			ID:    fmt.Sprintf("INFRA-%d", 2000+i),
			Title: fmt.Sprintf("the item ranked %d", i),
			Href:  fmt.Sprintf("/issues/INFRA-%d", 2000+i),
		}
		search.hits = append(search.hits, hit)
		body := fmt.Sprintf("%s%d.", marker, i)
		search.bodies[hit.Key()] = body + strings.Repeat("x", contracts.GoingHitRuneMax-len(body))
	}
	model := goingtest.NewModel(t, goingtest.Reply{Text: "Dispatch holds no answer to that question."})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), search.request("what do the ranked items say"), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.terminal(t)
	requests := model.Requests()
	if len(requests) != 1 {
		t.Fatalf("model served %d calls, want 1", len(requests))
	}
	var asked string
	for _, message := range requests[0].Messages {
		if message.Role == "user" {
			asked = message.Content
		}
	}
	last := search.hits[count-1]
	if !strings.Contains(asked, last.ID) || !strings.Contains(asked, last.Title) {
		t.Errorf("the model was not given %s %q at all; it must be named with no body", last.ID, last.Title)
	}
	if got := fmt.Sprintf("%s%d.", marker, count-1); strings.Contains(asked, got) {
		t.Errorf("the model was given the body %q of the hit past the run's rune budget", got)
	}
	if got := fmt.Sprintf("%s%d.", marker, count-2); !strings.Contains(asked, got) {
		t.Errorf("the model was not given the body %q, which the budget had room for", got)
	}
}

// TestRunHandsBackTheStreamWritersFailure: the writer's failure is the one failure Run reports to
// its caller, whether it comes on the first frame or on the last, and nothing is emitted after it.
func TestRunHandsBackTheStreamWritersFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream *answerStream
	}{
		{"on the first frame", &answerStream{failAt: 1}},
		{"on the terminal frame", &answerStream{failName: contracts.GoingEventEnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := goingtest.NewModel(t, goingtest.Reply{
				Text:  fmt.Sprintf("It is on [%s](%s).", routeHit.Title, routeHit.Href),
				Chunk: 5,
			})
			answerer := testAnswerer(t, model, going.AnswererConfig{})

			err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order"), tc.stream.emit)

			if !errors.Is(err, errStreamWriter) {
				t.Fatalf("Run = %v, want the stream writer's own failure", err)
			}
			for _, event := range tc.stream.events {
				if event.Terminal() {
					t.Errorf("stream holds the terminal event %s, which the writer never took: %v", event.Name, tc.stream.names())
				}
			}
		})
	}
}

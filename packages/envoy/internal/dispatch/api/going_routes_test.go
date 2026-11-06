package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeAnswerer is the model of these tests: it answers with the frames the test named, and, when
// the test asks it to, makes the one search and the one body read a real answer makes first, so
// the closures the route hands the answerer are exercised rather than described. It calls no
// service and holds no credential.
type fakeAnswerer struct {
	// frames are emitted in order, the last of them ending the stream.
	frames []going.Event
	// searches, when set, is what the run searches for before it emits anything; the hits and
	// their bodies are kept for the test to read.
	searches string

	mu     sync.Mutex
	asked  []string
	hits   []going.Hit
	bodies map[string]string
}

func (f *fakeAnswerer) Run(ctx context.Context, request going.Request, emit func(going.Event) error) error {
	f.mu.Lock()
	f.asked = append(f.asked, request.Words)
	f.mu.Unlock()
	if f.searches != "" {
		hits, err := request.Search(ctx, f.searches)
		if err != nil {
			return emit(going.Fault(contracts.GoingErrorAnswerFailed))
		}
		bodies, err := request.Bodies(ctx, hits)
		if err != nil {
			return emit(going.Fault(contracts.GoingErrorAnswerFailed))
		}
		f.mu.Lock()
		f.hits, f.bodies = hits, bodies
		f.mu.Unlock()
	}
	for _, frame := range f.frames {
		if err := emit(frame); err != nil {
			return err
		}
	}
	return nil
}

// read answers what the run was asked, the hits it was given and their bodies.
func (f *fakeAnswerer) read() ([]string, []going.Hit, map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked, f.hits, f.bodies
}

// goingRoutesRig is the server the route tests drive: the mux, the record behind it (read
// directly, as the run's own writes reach it) and the store both run on.
type goingRoutesRig struct {
	handler  http.Handler
	server   *server
	database *store.Store
}

func newGoingRoutesRig(t *testing.T, answerer Answerer) goingRoutesRig {
	t.Helper()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer})
	return goingRoutesRig{handler: handler, server: directServer(deps), database: database}
}

// ask asks one question as login, answering the route's response.
func (rig goingRoutesRig) ask(t *testing.T, login string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests", body, login)
}

// askedRun is the run a POST answers, which is the row the record stored.
type askedRun struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Words     string    `json:"words"`
	CreatedAt time.Time `json:"created_at"`
	EndsAt    time.Time `json:"ends_at"`
}

// stream reads one run's stream as login, resuming after lastEventID when it is not empty.
func (rig goingRoutesRig) stream(t *testing.T, id, login, lastEventID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/going/requests/"+id+"/stream", nil)
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// askUnder asks one question under a bearer token, which is how every credential with no cookie
// behind it asks one: a person's own dsp_ token, and the deployment's shared token.
func (rig goingRoutesRig) askUnder(t *testing.T, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return agentRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests", body, token)
}

// streamUnder reads one run's stream under a bearer token.
func (rig goingRoutesRig) streamUnder(t *testing.T, id, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/going/requests/"+id+"/stream", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// awaitTerminal waits for the run to have stored its ending and answers every frame it holds,
// rendered "<sequence> <name> <data>" as going_test.go renders them.
func (rig goingRoutesRig) awaitTerminal(t *testing.T, id string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, _, err := rig.server.readGoingStream(context.Background(), id, 0)
		if err != nil {
			t.Fatalf("read the run's frames: %v", err)
		}
		if len(stored) > 0 && stored[len(stored)-1].Event.Terminal() {
			rendered := make([]string, 0, len(stored))
			for _, frame := range stored {
				data, err := json.Marshal(frame.Event.Data)
				if err != nil {
					t.Fatalf("marshal frame %d: %v", frame.Sequence, err)
				}
				rendered = append(rendered, fmt.Sprintf("%d %s %s", frame.Sequence, frame.Event.Name, data))
			}
			return rendered
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run stored no ending within 10s; it holds %d frames", len(stored))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedGoingProject records a project and an issue the answer's search can find, and answers the
// issue's key and title.
func seedGoingProject(t *testing.T, handler http.Handler, project string) (string, string) {
	t.Helper()
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": project, "name": project + " project",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
	}
	const title = "The pelican migration"
	issue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "spec": "# Spec\n\nThe pelicans arrive in spring.",
	}, "alice")
	if issue.Code != http.StatusCreated {
		t.Fatalf("create an issue in %s: status=%d body=%s", project, issue.Code, issue.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key, title
}

// The POST records the question and answers its run, and the answer that runs behind it reaches
// the record: the words, the hits of the project's own search and each hit's text are what the
// answerer is given, and every frame it emits is stored in order, the last of them ending the
// stream.
func TestGoingRequestRecordsTheQuestionAndItsFramesReachTheRecord(t *testing.T) {
	answerer := &fakeAnswerer{
		searches: "pelican",
		frames: []going.Event{
			going.Text("The pelicans "),
			going.Text("arrive in spring."),
			going.Item(going.Hit{Kind: "issue", ID: "SEED-1", Title: "The pelican migration", Href: "/issues/SEED-1"}),
			going.End(4, 1),
		},
	}
	rig := newGoingRoutesRig(t, answerer)
	issueKey, issueTitle := seedGoingProject(t, rig.handler, "GOI")

	response := rig.ask(t, "alice", map[string]any{"words": "when do the pelicans arrive?", "project": "GOI"})
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	run := decodeBody[askedRun](t, response)
	if run.ID == "" || run.Words != "when do the pelicans arrive?" || run.Project != "GOI" {
		t.Fatalf("the POST answered %+v, want the stored run of that question", run)
	}
	if window := run.EndsAt.Sub(run.CreatedAt); window != contracts.GoingRunWindow {
		t.Errorf("the run's window is %v, want %v", window, contracts.GoingRunWindow)
	}
	recorded, err := rig.server.readGoingRequest(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read the recorded run: %v", err)
	}
	if !recorded.Author.SameAs(goingAsker) || recorded.Words != run.Words || recorded.Project != "GOI" {
		t.Fatalf("the record holds %+v, want alice's GOI question", recorded)
	}

	frames := rig.awaitTerminal(t, run.ID)
	// The payloads read back in the key order jsonb stores them in, which is what the stream
	// writes: the frame a reader is written is the frame that was stored, byte for byte.
	want := []string{
		`1 text {"text":"The pelicans "}`,
		`2 text {"text":"arrive in spring."}`,
		`3 item {"id":"SEED-1","href":"/issues/SEED-1","kind":"issue","title":"The pelican migration"}`,
		`4 end {"items":1,"words":4}`,
	}
	if strings.Join(frames, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the record holds\n%s\nwant\n%s", strings.Join(frames, "\n"), strings.Join(want, "\n"))
	}

	asked, hits, bodies := answerer.read()
	if len(asked) != 1 || asked[0] != run.Words {
		t.Fatalf("the answerer was asked %q, want the one question %q", asked, run.Words)
	}
	var found *going.Hit
	for index, hit := range hits {
		if hit.Kind == "issue" && hit.ID == issueKey {
			found = &hits[index]
		}
	}
	if found == nil {
		t.Fatalf("the answerer's search answered %+v, want the seeded issue %s among them", hits, issueKey)
	}
	if found.Title != issueTitle || found.Href == "" {
		t.Errorf("the issue hit is %+v, want the issue's own title %q and its href", *found, issueTitle)
	}
	if body := bodies[found.Key()]; !strings.Contains(body, "pelicans arrive in spring") {
		t.Errorf("the hit's body is %q, want the issue's own text", body)
	}
}

// The question itself is held to its bounds: blank words are no question, and a question past
// contracts.GoingWordsMax is refused as search refuses a query past its own cap. Neither records
// a run.
func TestGoingRequestRefusesAQuestionItCannotAsk(t *testing.T) {
	rig := newGoingRoutesRig(t, &fakeAnswerer{frames: []going.Event{going.End(0, 0)}})

	for name, words := range map[string]any{"missing": nil, "blank": "   "} {
		t.Run("words "+name, func(t *testing.T) {
			response := rig.ask(t, "alice", map[string]any{"words": words, "project": ""})
			refusal := decodeBody[struct {
				Code string `json:"code"`
			}](t, response)
			if response.Code != http.StatusBadRequest || refusal.Code != "GOING_INPUT" {
				t.Fatalf("status=%d code=%q, want 400 GOING_INPUT", response.Code, refusal.Code)
			}
		})
	}
	for name, over := range overCapCases(contracts.GoingWordsMax) {
		t.Run("words "+name, func(t *testing.T) {
			response := rig.ask(t, "alice", map[string]any{"words": over.text})
			assertCapExceeded(t, "ask", response, "words", over.length, contracts.GoingWordsMax, "")
		})
	}

	var runs int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_requests`).Scan(&runs); err != nil {
		t.Fatalf("count the recorded runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("%d runs were recorded, want none: a question the route refuses is never asked", runs)
	}
}

// A credential scoped to one project asks only of that project: the check runs on the project
// the body names, as createIssue's does, before anything is recorded.
func TestGoingRequestRefusesAProjectScopedCallerAnotherProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	seedProject(t, rig, "OTH", "Other")

	response := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/going/requests", token, map[string]any{
		"words": "what is going on in the other project?", "project": "OTH",
	}))
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusForbidden || refusal.Code != codeProjectScope {
		t.Fatalf("status=%d refusal=%+v, want 403 %s", response.Code, refusal, codeProjectScope)
	}
	if !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, "OTH") {
		t.Fatalf("the refusal names %q, want both projects", refusal.Error)
	}

	own := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/going/requests", token, map[string]any{
		"words": "what is going on in my own project?", "project": brokerTestProject,
	}))
	if own.Code != http.StatusCreated {
		t.Fatalf("asking of its own project: status=%d body=%s", own.Code, own.Body.String())
	}
}

// The stream is the stored frames, in order: each one its sequence as the frame's id, its name
// as the event and the stored JSON as the data, and the stream ends on the terminal frame.
// Last-Event-ID answers the frames past the one a reader holds, which is what a reader that
// dropped asks for.
func TestGoingStreamReplaysStoredFramesAndResumes(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	run, err := rig.server.recordGoingRequest(context.Background(), goingAsker, "GOI", "who closed it?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	ctx := context.Background()
	for _, frame := range []going.Event{going.Text("Nobody "), going.Text("has.")} {
		if _, err := rig.server.appendGoingEvent(ctx, run.ID, frame); err != nil {
			t.Fatalf("append %s: %v", frame.Name, err)
		}
	}
	if _, err := rig.server.endGoingRun(ctx, run.ID, going.End(2, 0)); err != nil {
		t.Fatalf("end the run: %v", err)
	}

	whole := rig.stream(t, run.ID, "alice", "")
	if whole.Code != http.StatusOK {
		t.Fatalf("stream: status=%d body=%s", whole.Code, whole.Body.String())
	}
	if kind := whole.Header().Get("Content-Type"); kind != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", kind)
	}
	if cache := whole.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cache)
	}
	// The ending's payload in the key order the record stored it in (jsonb's), since the stream
	// writes the frame that was stored and not one it marshals again.
	const ending = "id: 3\nevent: end\ndata: {\"items\":0,\"words\":2}\n\n"
	want := "id: 1\nevent: text\ndata: {\"text\":\"Nobody \"}\n\n" +
		"id: 2\nevent: text\ndata: {\"text\":\"has.\"}\n\n" + ending
	if whole.Body.String() != want {
		t.Fatalf("the stream wrote\n%q\nwant\n%q", whole.Body.String(), want)
	}

	resumed := rig.stream(t, run.ID, "alice", "2")
	if resumed.Code != http.StatusOK {
		t.Fatalf("resumed stream: status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	if got := resumed.Body.String(); got != ending {
		t.Fatalf("resuming after frame 2 wrote %q, want the ending alone", got)
	}
	if done := rig.stream(t, run.ID, "alice", "3"); done.Body.String() != "" {
		t.Fatalf("resuming after the ending wrote %q, want nothing: the stream is over", done.Body.String())
	}
}

// goingStreamSink is one reader of an open stream: it keeps what the handler wrote and says when
// the handler flushed, so a test acts at a point the handler has reached rather than after a
// sleep. The body is held behind a mutex because the handler writes it on a goroutine of its own
// while the test reads it.
type goingStreamSink struct {
	header  http.Header
	flushed chan struct{}

	mu   sync.Mutex
	body strings.Builder
}

func (s *goingStreamSink) Header() http.Header { return s.header }

func (s *goingStreamSink) WriteHeader(int) {}

func (s *goingStreamSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}

// Flush says one flush happened and never blocks: the handler flushes its headers before it
// reads anything and then every frame it writes, and a test waits for the first of those.
func (s *goingStreamSink) Flush() {
	select {
	case s.flushed <- struct{}{}:
	default:
	}
}

// text is what the handler has written so far.
func (s *goingStreamSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

// A stream ends on the frame that ends it and never on the run merely holding one. One poll
// reads the frames and the run's state at two moments, so an ending committed between them is in
// the state's answer and in no frame the reader was handed: a reader that stopped there would
// close a stream that never carried its ending, which is the one way a stream may not end. The
// lock asks for that order rather than waiting for it to happen: the ending is written and held
// uncommitted, so every frames read answers nothing, and committed only once the state read is
// waiting on going_requests, so the state it answers holds an ending no frames read could have
// seen.
func TestGoingStreamCarriesAnEndingStoredBetweenItsTwoReads(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	ctx := context.Background()
	run, err := rig.server.recordGoingRequest(ctx, goingAsker, "", "who is answering this?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	for _, frame := range []going.Event{going.Text("Nobody "), going.Text("has.")} {
		if _, err := rig.server.appendGoingEvent(ctx, run.ID, frame); err != nil {
			t.Fatalf("append %s: %v", frame.Name, err)
		}
	}

	// A reader holding frame 2, polling for whatever comes after it.
	sink := &goingStreamSink{header: http.Header{}, flushed: make(chan struct{}, 4)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/going/requests/"+run.ID+"/stream", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set("Last-Event-ID", "2")
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(sink, request)
	}()
	select {
	case <-sink.flushed:
	case <-time.After(30 * time.Second):
		t.Fatal("the stream wrote no headers within 30s")
	}

	ending, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction that ends the run: %v", err)
	}
	defer ending.Rollback(ctx)
	if _, err := ending.Exec(ctx, `lock table going_requests in access exclusive mode`); err != nil {
		t.Fatalf("lock going_requests: %v", err)
	}
	if _, err := ending.Exec(ctx, `
		insert into going_events (request_id, sequence, name, data)
		values ($1::uuid, 3, $2, '{"items":0,"words":2}'::jsonb)
	`, run.ID, contracts.GoingEventEnd); err != nil {
		t.Fatalf("store the ending: %v", err)
	}
	waitUntil(t, 30*time.Second, "the stream's state read waiting on going_requests", func() bool {
		var waiting int
		if err := rig.database.Pool.QueryRow(ctx, `
			select count(*) from pg_locks where relation = 'going_requests'::regclass and not granted
		`).Scan(&waiting); err != nil {
			t.Fatalf("read what is waiting on going_requests: %v", err)
		}
		return waiting > 0
	})
	if err := ending.Commit(ctx); err != nil {
		t.Fatalf("commit the ending: %v", err)
	}

	select {
	case <-served:
	case <-time.After(60 * time.Second):
		t.Fatal("the stream did not end within 60s of its run's ending")
	}
	want := "id: 3\nevent: " + contracts.GoingEventEnd + "\ndata: {\"items\":0,\"words\":2}\n\n"
	if got := sink.text(); got != want {
		t.Fatalf("the stream wrote %q, want the ending %q: the frame a run ended on reaches the reader waiting for it", got, want)
	}
}

// A run nobody asked for, an id that could be nobody's, and a run that is somebody else's are
// one answer, word for word: an id tells a caller nothing it did not already know.
func TestGoingStreamAnswersEveryRunItWillNotServeAlike(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	mine, err := rig.server.recordGoingRequest(context.Background(), goingAsker, "", "what is going on?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}

	answers := map[string]*httptest.ResponseRecorder{
		"a run nobody asked for":   rig.stream(t, "00000000-0000-4000-8000-00000000000a", "alice", ""),
		"an id that is not a uuid": rig.stream(t, "not-a-uuid", "alice", ""),
		"another caller's run":     rig.stream(t, mine.ID, "bob", ""),
	}
	for name, response := range answers {
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s, want 404", name, response.Code, response.Body.String())
		}
		refusal := decodeBody[refusalAnswer](t, response)
		if refusal.Code != codeGoingRequestNotFound {
			t.Fatalf("%s: code=%q, want %s", name, refusal.Code, codeGoingRequestNotFound)
		}
	}
	bodies := map[string]bool{}
	for _, response := range answers {
		bodies[response.Body.String()] = true
	}
	if len(bodies) != 1 {
		t.Fatalf("the three refusals read %v, want one body for all three", bodies)
	}
}

// The case no live run can fake: the pod that took the POST is gone, so the run is past its
// ends_at with no ending stored and nothing is writing it. The reader ends it itself, with the
// deadline's error, and stores it, so the next reader of that run is answered the same ending.
func TestGoingStreamEndsARunWhosePodIsGone(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	var id string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		insert into going_requests (author, project, words, created_at, ends_at)
		values ('{"kind":"user","id":"alice"}'::jsonb, '', 'what happened to the pod?',
		        now() - interval '2 minutes', now() - interval '90 seconds')
		returning id::text
	`).Scan(&id); err != nil {
		t.Fatalf("write the run of a pod that died: %v", err)
	}

	response := rig.stream(t, id, "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("stream: status=%d body=%s", response.Code, response.Body.String())
	}
	want := fmt.Sprintf("id: 1\nevent: %s\ndata: {\"code\":\"%s\"}\n\n", contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if response.Body.String() != want {
		t.Fatalf("the stream wrote %q, want the deadline's error %q", response.Body.String(), want)
	}
	stored, err := rig.server.readGoingTerminalEvent(context.Background(), id)
	if err != nil {
		t.Fatalf("read the stored ending: %v", err)
	}
	if stored.Sequence != 1 || stored.Event.Name != contracts.GoingEventError {
		t.Fatalf("the record holds %+v, want the deadline's error stored as the run's one ending", stored)
	}
	if again := rig.stream(t, id, "alice", ""); again.Body.String() != want {
		t.Fatalf("reading it again wrote %q, want the stored ending %q", again.Body.String(), want)
	}
}

// The capability route says whether a model is configured and nothing else, to any caller
// Dispatch admits - a pod holding nothing but an agent token included - so no endpoint,
// deployment name or credential can be read from it.
func TestGoingCapabilitySaysWhetherAModelIsConfigured(t *testing.T) {
	none, _, _, _ := newRegisteredTestServer(t, testServerOptions{})
	response := sessionRequest(t, none, http.MethodGet, "/api/v1/going/capability", nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"model_configured":false}` {
		t.Fatalf("with no model: status=%d body=%s, want {\"model_configured\":false}", response.Code, response.Body.String())
	}

	configured, _, _, _ := newRegisteredTestServer(t, testServerOptions{answerer: &fakeAnswerer{}})
	answered := sessionRequest(t, configured, http.MethodGet, "/api/v1/going/capability", nil)
	if answered.Code != http.StatusOK || strings.TrimSpace(answered.Body.String()) != `{"model_configured":true}` {
		t.Fatalf("with a model: status=%d body=%s, want {\"model_configured\":true}", answered.Code, answered.Body.String())
	}
	if body := answered.Body.String(); strings.Contains(body, "fake:model") || strings.Contains(body, "identity") {
		t.Fatalf("the capability names a configured value: %s", body)
	}
	if unauthenticated := dispatchRequest(t, configured, http.MethodGet, "/api/v1/going/capability", nil, ""); unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("with no credential: status=%d body=%s, want 401", unauthenticated.Code, unauthenticated.Body.String())
	}
}

// With no model configured the question is recorded all the same and its stream carries the
// error that says so, which is how a reader learns it: the POST answers exactly as it does with
// a model, since nothing of this route's own status says what the deployment configured.
func TestGoingRequestWithNoModelConfiguredStreamsThatFault(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)

	response := rig.ask(t, "alice", map[string]any{"words": "is anybody answering?"})
	if response.Code != http.StatusCreated {
		t.Fatalf("ask with no model: status=%d body=%s", response.Code, response.Body.String())
	}
	run := decodeBody[askedRun](t, response)
	if frames := rig.awaitTerminal(t, run.ID); len(frames) != 1 ||
		frames[0] != fmt.Sprintf(`1 %s {"code":"%s"}`, contracts.GoingEventError, contracts.GoingErrorNoModelConfigured) {
		t.Fatalf("the record holds %v, want the one no-model-configured frame", frames)
	}

	stream := rig.stream(t, run.ID, "alice", "")
	want := fmt.Sprintf("id: 1\nevent: %s\ndata: {\"code\":\"%s\"}\n\n", contracts.GoingEventError, contracts.GoingErrorNoModelConfigured)
	if stream.Code != http.StatusOK || stream.Body.String() != want {
		t.Fatalf("the stream wrote %q (status %d), want %q", stream.Body.String(), stream.Code, want)
	}
}

// A run is served to the credential that asked it and to no other, whole identity against whole
// identity: two personal tokens of different owners each resolve as {Kind: "session", ID: "",
// Owner: &owner} (agent_tokens.go), so a comparison of kind and id alone - model.Actor.SameAs -
// would hand Bob every run Alice asked under her own token.
func TestGoingStreamRefusesAnotherOwnersPersonalToken(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	alice := personalTokenOwnedBy(t, rig.database, "alice", model.ScopePerson)
	bob := personalTokenOwnedBy(t, rig.database, "bob", model.ScopePerson)

	asked := rig.askUnder(t, alice, map[string]any{"words": "who owns the broadcast order?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask under alice's token: status=%d body=%s", asked.Code, asked.Body.String())
	}
	run := decodeBody[askedRun](t, asked)
	rig.awaitTerminal(t, run.ID)

	refused := rig.streamUnder(t, run.ID, bob)
	if refused.Code != http.StatusNotFound {
		t.Fatalf("bob's token reading alice's run: status=%d body=%s, want 404", refused.Code, refused.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, refused); refusal.Code != codeGoingRequestNotFound {
		t.Fatalf("bob's token reading alice's run: code=%q, want %s", refusal.Code, codeGoingRequestNotFound)
	}
	own := rig.streamUnder(t, run.ID, alice)
	if own.Code != http.StatusOK || own.Body.String() == "" {
		t.Fatalf("alice's own token reading her run: status=%d body=%q, want the run's frames", own.Code, own.Body.String())
	}
}

// The POST refuses a credential that names nobody and records nothing. The deployment's shared
// token resolves as {} (server.go), so the run it asked for would name no session, no person
// and no workload, and the stream serves such a run to nobody: the model would be called and
// the deployment's paced quota spent on frames every reader of them is answered 404.
func TestGoingRequestRefusesAnAskerThatNamesNobody(t *testing.T) {
	answerer := &fakeAnswerer{frames: []going.Event{going.End(1, 0)}}
	rig := newGoingRoutesRig(t, answerer)

	asked := rig.askUnder(t, "agent-token", map[string]any{"words": "whose run is this?"})
	if asked.Code != http.StatusForbidden {
		t.Fatalf("ask under the shared token: status=%d body=%s, want 403", asked.Code, asked.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, asked); refusal.Code != codeGoingAskerNamesNobody {
		t.Fatalf("ask under the shared token: code=%q, want %s", refusal.Code, codeGoingAskerNamesNobody)
	}
	var recorded int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_requests`).Scan(&recorded); err != nil {
		t.Fatalf("count the recorded runs: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("the refused ask recorded %d runs, want none", recorded)
	}
	if questions, _, _ := answerer.read(); len(questions) != 0 {
		t.Fatalf("the refused ask reached the model with %q, want nothing asked of it", questions)
	}
}

// A run whose recorded author names nobody is nobody's to read: model.Actor.SameCredential
// calls two credentials that name nobody the one credential, so that comparison alone would
// serve a row authored by {} to the deployment's shared token, which resolves as {} too. The
// POST refuses an asker that names nobody and records nothing, so the row is written straight
// to the record here. The pod's run that follows is what the rest of the same comparison
// rests on: {Service: &subject} carries the same empty kind and id as {}, so comparing kind
// and id alone - SameAs - would serve the pod's run to the shared token.
func TestGoingStreamRefusesARunWhoseAuthorNamesNobody(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)

	nobodys, err := rig.server.recordGoingRequest(context.Background(), model.Actor{}, "", "whose run is this?")
	if err != nil {
		t.Fatalf("record a run naming nobody: %v", err)
	}
	refused := rig.streamUnder(t, nobodys.ID, "agent-token")
	if refused.Code != http.StatusNotFound {
		t.Fatalf("the shared token reading a run naming nobody: status=%d body=%s, want 404", refused.Code, refused.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, refused); refusal.Code != codeGoingRequestNotFound {
		t.Fatalf("the shared token reading a run naming nobody: code=%q, want %s", refusal.Code, codeGoingRequestNotFound)
	}

	subject := "system:serviceaccount:dispatch:answerer"
	pod, err := rig.server.recordGoingRequest(context.Background(), model.Actor{Service: &subject}, "", "what is this pod running?")
	if err != nil {
		t.Fatalf("record a pod's run: %v", err)
	}
	crossed := rig.streamUnder(t, pod.ID, "agent-token")
	if crossed.Code != http.StatusNotFound {
		t.Fatalf("the shared token reading a pod's run: status=%d body=%s, want 404", crossed.Code, crossed.Body.String())
	}
}

// A Last-Event-ID past what going_events.sequence can hold is no resumption point, as a
// negative one is not: the whole stream is answered. Bound as a sequence it could never be, the
// read that carries the stream would fail against the integer column and the reader would be
// answered 200 with no frame at all.
func TestGoingStreamReadsAResumePointPastTheSequenceColumnAsNone(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	ctx := context.Background()
	run, err := rig.server.recordGoingRequest(ctx, goingAsker, "", "how far can a reader resume?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	if _, err := rig.server.appendGoingEvent(ctx, run.ID, going.Text("Nobody has.")); err != nil {
		t.Fatalf("append the answer: %v", err)
	}
	if _, err := rig.server.endGoingRun(ctx, run.ID, going.End(2, 0)); err != nil {
		t.Fatalf("end the run: %v", err)
	}

	want := "id: 1\nevent: text\ndata: {\"text\":\"Nobody has.\"}\n\n" +
		"id: 2\nevent: end\ndata: {\"items\":0,\"words\":2}\n\n"
	for _, resume := range []string{"2147483648", "9223372036854775808", "-1"} {
		response := rig.stream(t, run.ID, "alice", resume)
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("resuming from %s wrote %q (status %d), want the whole stream %q",
				resume, response.Body.String(), response.Code, want)
		}
	}
}

// cursorAnswerer is a run that holds one of the shared pool's connections while it answers, as
// every real run does the moment it searches: store.Pool.Query holds its connection until the
// rows close, and marks the context it was given as holding one. It opens that cursor on the
// context the route detached it with, says so, and holds it until the test lets go.
type cursorAnswerer struct {
	pool    *store.Pool
	opened  chan struct{}
	release chan struct{}
	// failed is what opening the cursor answered, read once opened is closed.
	failed error
}

func (c *cursorAnswerer) Run(ctx context.Context, _ going.Request, emit func(going.Event) error) error {
	rows, err := c.pool.Query(ctx, `select 1`)
	if err == nil {
		close(c.opened)
		<-c.release
		rows.Close()
	} else {
		c.failed = err
		close(c.opened)
	}
	return emit(going.Fault(contracts.GoingErrorAnswerFailed))
}

// The run the POST detaches reads the store on a connection mark of its own and never on the
// request's. The request's mark is still in use when the run begins: keyedWrites settles the
// Idempotency-Key with an Exec on it the moment this handler returns, and the pool refuses a
// second connection under a mark that already holds one (store.ErrNestedAcquire), so a run
// sharing it would take that settle's connection away for the length of its first search and
// the key would store no answer - a repeat would then be recorded as a second question instead
// of being answered this run's id. Here the run's cursor is held open while the settle's own
// statement runs, which is the collision itself rather than a window it might fall into.
func TestGoingRequestGivesItsRunAConnectionMarkOfItsOwn(t *testing.T) {
	answerer := &cursorAnswerer{opened: make(chan struct{}), release: make(chan struct{})}
	rig := newGoingRoutesRig(t, answerer)
	answerer.pool = rig.database.Pool
	// The run's cursor is let go however this test ends: a failure that left it open would hold
	// a connection of the pool past the test, and the store's own cleanup would wait on it.
	release := sync.OnceFunc(func() { close(answerer.release) })
	defer release()

	// The mark every route's handler runs under (server.Register), held here so the test can
	// run the settle's statement on it.
	marked := store.WithTransactionTracking(context.Background())
	request := httptest.NewRequestWithContext(marked, http.MethodPost, "/api/v1/going/requests",
		strings.NewReader(`{"words":"who holds the connection?"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	rig.server.createGoingRequest(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	run := decodeBody[askedRun](t, response)

	<-answerer.opened
	if answerer.failed != nil {
		t.Fatalf("the detached run could not open a cursor: %v", answerer.failed)
	}
	// What settleRequestKey runs on the request's mark once the handler has returned.
	if _, err := rig.database.Pool.Exec(marked, `select 1`); err != nil {
		t.Fatalf("the key's settle was refused while the run held a cursor: %v", err)
	}
	release()
	rig.awaitTerminal(t, run.ID)
}

// A POST whose run cannot be given a connection mark of its own answers that failure and
// records nothing. The mark is taken before the question is recorded, as every other caller of
// store.ForConcurrentRead answers its refusal (comment_create.go): a run nothing will answer
// must not leave a caller a 201 and a stream that only runs out its deadline. Here the
// request's own context already holds one of the pool's connections, which is the one thing
// ForConcurrentRead refuses (store.ErrNestedAcquire).
func TestGoingRequestAnswersTheFailureWhenItsRunCannotBeDetached(t *testing.T) {
	answerer := &fakeAnswerer{frames: []going.Event{going.End(1, 0)}}
	rig := newGoingRoutesRig(t, answerer)
	held, release := store.HoldsConnection(context.Background())
	defer release()

	request := httptest.NewRequestWithContext(held, http.MethodPost, "/api/v1/going/requests",
		strings.NewReader(`{"words":"who answers a run nothing can detach?"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	rig.server.createGoingRequest(response, request)

	if response.Code == http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s, want the failure and not a run nothing answers", response.Code, response.Body.String())
	}
	var recorded int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_requests`).Scan(&recorded); err != nil {
		t.Fatalf("count the recorded runs: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("the failed ask recorded %d runs, want none: nothing would ever answer them", recorded)
	}
	if questions, _, _ := answerer.read(); len(questions) != 0 {
		t.Fatalf("the failed ask reached the model with %q, want nothing asked of it", questions)
	}
}

package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// roomAllowance is what a room in use may hold beyond the bytes its writes add to the store: the
// heap a resident room's caches, replica and settlement keep whatever the history's length.
const roomAllowance = 2 << 20

// A room in use holds its stored history decoded, so what it retains grows with what each write
// adds to the store and no faster: an upload of the 25 KB spec that changes one token retains
// about what it stores, not a copy of every block's attributes.
func TestUploadsToARoomInUseKeepMemoryWithinTheBound(t *testing.T) {
	skipUnderRace(t)
	const uploads = 200
	agent, start := newLoadAgent(t)
	for range 2 {
		agent.uploadToken()
	}
	base := measureRoom(t, agent)
	began := time.Now()
	for range uploads {
		agent.uploadToken()
	}
	elapsed := time.Since(began)
	requireWithinRoomBound(t, base, measureRoom(t, agent), uploads, time.Since(start), elapsed)
}

// An upload puts every open anchor back where it was, so the anchors open on a document add to
// what an upload stores; what the room retains stays within what the store holds, at a hundred
// open anchors in table cells and prose as at none.
func TestUploadsOverOpenAnchorsKeepMemoryWithinTheBound(t *testing.T) {
	skipUnderRace(t)
	const bare, anchored, anchors = 50, 100, 100
	agent, start := newLoadAgent(t)
	for range 2 {
		agent.uploadToken()
	}
	base := measureRoom(t, agent)
	began := time.Now()
	for range bare {
		agent.uploadToken()
	}
	unanchored := measureRoom(t, agent)
	quotes := agent.anchorQuotes(anchors)
	for index, quote := range quotes {
		agent.anchoredComment(quote, fmt.Sprintf("Anchored comment %d: %s", index, docstest.SpecSentence(agent.r, 20)))
	}
	opened := measureRoom(t, agent)
	for range anchored {
		agent.uploadToken()
	}
	elapsed := time.Since(began)
	end := measureRoom(t, agent)
	t.Logf("PERUPLOAD anchors=0 retained=%.0f B/upload stored=%.0f B/upload over %d uploads",
		perWrite(unanchored.heap-base.heap, bare), perWrite(unanchored.stored-base.stored, bare), bare)
	t.Logf("PERUPLOAD anchors=%d retained=%.0f B/upload stored=%.0f B/upload over %d uploads (%d anchored comments retained %d B, stored %d B)",
		len(quotes), perWrite(end.heap-opened.heap, anchored), perWrite(end.stored-opened.stored, anchored), anchored,
		len(quotes), opened.heap-unanchored.heap, opened.stored-unanchored.stored)
	requireWithinRoomBound(t, base, end, bare+len(quotes)+anchored, time.Since(start), elapsed)
}

// A room only agents write takes the load harness's mix as the API makes it: anchored comments,
// replies on anchored threads, token edits and token uploads, the plain comments and the replies
// on plain roots writing no document. What it retains stays within what those writes store.
func TestARoomFedOnlyByAgentWritesKeepsMemoryWithinTheBound(t *testing.T) {
	skipUnderRace(t)
	const writes = 400
	agent, start := newLoadAgent(t)
	for seed := range 4 {
		agent.roots = append(agent.roots, agent.anchoredComment(agent.randomQuote(), fmt.Sprintf("Seed comment %d: %s", seed, docstest.SpecSentence(agent.r, 25))))
	}
	base := measureRoom(t, agent)
	mix := []struct {
		kind   string
		weight int
	}{{"comment", 3}, {"anchored", 33}, {"reply", 35}, {"edit", 22}, {"upload", 7}}
	counts := map[string]int{}
	began := time.Now()
	for range writes {
		pick, x := "", agent.r.IntN(100)
		for _, m := range mix {
			if x < m.weight {
				pick = m.kind
				break
			}
			x -= m.weight
		}
		switch pick {
		case "comment":
			// A plain comment writes no document.
		case "anchored":
			agent.anchoredComment(agent.randomQuote(), "lt line comment: "+docstest.SpecSentence(agent.r, 20))
		case "reply":
			if agent.r.Float64() >= 0.77 {
				pick = "reply to a plain root" // which writes no document
				break
			}
			agent.reply(agent.roots[agent.r.IntN(len(agent.roots))])
		case "edit":
			agent.editToken()
		default:
			agent.uploadToken()
		}
		counts[pick]++
	}
	elapsed := time.Since(began)
	t.Logf("MIX %v", counts)
	requireWithinRoomBound(t, base, measureRoom(t, agent), writes, time.Since(start), elapsed)
}

// loadAgent writes into one document as the load harness's agent session does through the API.
// Its document is the harness's 25 KB spec, its room is kept in use (no idle eviction) with no
// browser connected, and its choices come from a fixed seed.
type loadAgent struct {
	t          *testing.T
	service    *Service
	artifactID string
	actor      model.Actor
	r          *rand.Rand
	spec       string
	quotes     []string
	paragraphs []string
	revision   int
	written    int
	roots      []*loadThread
}

// loadThread is an anchored comment thread whose root record each reply writes again.
type loadThread struct {
	id, body, at string
	replies      []MarkReply
}

// skipUnderRace skips a measurement of the heap, or of the allocations a path makes, in a -race
// build: the race detector distorts both figures (its shadow memory, its instrumented allocations
// and the escapes its instrumentation forces are not the measured path's), and these tests
// exercise no concurrency for it to catch, so under -race they cost minutes of the package's time
// budget and prove nothing. The normal build runs them.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("the race detector distorts the heap and the allocations this test measures, and the test checks no concurrency")
	}
}

// skipAtTheBoundsUnderRace skips, in a -race build, a test whose fixture has to stand at one of
// the document's bounds: the 1,048,576 structs ygo decodes in one update (maxUpdateItems), or the
// markdown's byte and element bounds rewritten until the cap refuses a write. Such a fixture is
// built by about a million ygo writes, each of which the race detector instruments and whose
// items it shadows: measured on two processors, the tests this and skipUnderRace leave out took
// 50 of the 121 minutes a -race run of every test in the package took, and the decode-cap ones
// among them carried its resident memory from 0.8 GB to 9.5 GB. Each drives one goroutine over
// its fixture and then checks a figure or a refusal, so what the detector could catch in it - the
// room's settlement, its sweep, its sockets - the tests beside it drive over a small document in
// seconds. The normal build runs them.
func skipAtTheBoundsUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("the fixture this test stands at the document's bounds costs the race detector minutes and gigabytes, and the test drives one goroutine over it")
	}
}

func newLoadAgent(t *testing.T) (*loadAgent, time.Time) {
	t.Helper()
	start := time.Now()
	service, artifactID := newTestService(t)
	service.srv.RoomIdleTimeout = time.Hour
	r := rand.New(rand.NewPCG(715, 1))
	spec, quotes, paragraphs := docstest.LoadSpec(r, "LOAD-1", 25_000)
	seedServiceText(t, service, artifactID, spec)
	t.Logf("spec bytes=%d requirement quotes=%d prose paragraphs=%d", len(spec), len(quotes), len(paragraphs))
	return &loadAgent{
		t: t, service: service, artifactID: artifactID, actor: model.Actor{Kind: "session", ID: "ses-lg-0001"},
		r: r, spec: spec, quotes: quotes, paragraphs: paragraphs,
	}, start
}

func (a *loadAgent) newID() string {
	a.written++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", a.written)
}

func (a *loadAgent) randomQuote() string {
	return a.quotes[a.r.IntN(len(a.quotes))]
}

// anchorQuotes is n quotes the spec holds once each: its Requirement cells first, then the first
// words of its prose paragraphs.
func (a *loadAgent) anchorQuotes(n int) []string {
	a.t.Helper()
	quotes := a.quotes[:min(len(a.quotes), n*7/10)]
	for _, paragraph := range a.paragraphs {
		if len(quotes) == n {
			break
		}
		quote := strings.Join(strings.Fields(paragraph)[:8], " ")
		if strings.Count(a.spec, quote) == 1 {
			quotes = append(quotes, quote)
		}
	}
	if len(quotes) != n {
		a.t.Fatalf("the spec holds %d quotes to anchor on, want %d", len(quotes), n)
	}
	return quotes
}

// anchoredComment creates an open comment anchored on quote as the API's comment create does:
// the mark, the version the anchor names, the comment row holding the anchor, and the mark's
// record, in one transaction.
func (a *loadAgent) anchoredComment(quote, body string) *loadThread {
	a.t.Helper()
	thread := &loadThread{id: a.newID(), body: body, at: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := joinedWrite(a.service, func(ctx context.Context) error {
		anchored, err := a.service.MarkQuote(ctx, a.artifactID, MarkSpec{Kind: MarkComment, ID: thread.id, By: a.actor}, quote, nil)
		if err != nil {
			return err
		}
		snapshot, err := a.service.SnapshotVersion(ctx, a.artifactID, a.actor)
		if err != nil {
			return err
		}
		anchor := model.Anchor{ArtifactID: a.artifactID, MarkID: thread.id, Version: snapshot.Version.Number, Quote: anchored.Quote}
		if anchored.BlockID != "" {
			anchor.BlockID = &anchored.BlockID
		}
		encodedAnchor, err := json.Marshal(anchor)
		if err != nil {
			return err
		}
		author, err := json.Marshal(a.actor)
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, anchor) values ($1, 'DOC-1', $2, $3, $4)
		`, thread.id, author, body, encodedAnchor); err != nil {
			return err
		}
		return a.service.ProjectMark(ctx, a.artifactID, thread.id, MarkRecord{
			Kind: "comment", By: ActorRef(a.actor), CreatedAt: thread.at, Text: body, Replies: []MarkReply{},
		}, a.actor)
	}); err != nil {
		a.t.Fatalf("anchored comment on %q: %v", quote, err)
	}
	return thread
}

// reply answers root as the API does on an anchored thread: it writes the root's record again,
// replies included.
func (a *loadAgent) reply(root *loadThread) {
	a.t.Helper()
	root.replies = append(root.replies, MarkReply{By: ActorRef(a.actor), Text: "lt reply: " + docstest.SpecSentence(a.r, 20), At: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := joinedProjectMark(a.service, a.artifactID, root.id, MarkRecord{
		Kind: "comment", By: ActorRef(a.actor), CreatedAt: root.at, Text: root.body, Replies: root.replies,
	}, a.actor); err != nil {
		a.t.Fatalf("reply: %v", err)
	}
}

// editToken replaces the revision token with an edit operation.
func (a *loadAgent) editToken() {
	a.t.Helper()
	if _, err := joinedApplyOps(a.service, a.artifactID, []model.EditOp{{
		Op: "replace", Find: fmt.Sprintf("Revision token r%d", a.revision), With: fmt.Sprintf("Revision token r%d", a.revision+1),
	}}, a.actor, nil); err != nil {
		a.t.Fatalf("token edit r%d: %v", a.revision, err)
	}
	a.revision++
}

// uploadToken reads the document's text, changes its revision token and uploads the whole text.
func (a *loadAgent) uploadToken() {
	a.t.Helper()
	text, err := a.service.Text(context.Background(), a.artifactID)
	if err != nil {
		a.t.Fatal(err)
	}
	old := fmt.Sprintf("Revision token r%d", a.revision)
	if !strings.Contains(text, old) {
		a.t.Fatalf("the document's text holds no %q", old)
	}
	a.revision++
	if _, err := joinedReplaceText(a.service, a.artifactID, strings.Replace(text, old, fmt.Sprintf("Revision token r%d", a.revision), 1), a.actor); err != nil {
		a.t.Fatalf("token upload r%d: %v", a.revision, err)
	}
}

// roomMemory is the heap the process holds and the bytes Postgres stores for the document, read
// once the room's writes have settled.
type roomMemory struct {
	heap, stored int64
}

// measureRoom waits for every write to reach the store and the room's settlement to finish, then
// reads the heap (the least of three readings, each after two collections, since a sync.Pool's
// contents survive the first and anything else allocating meanwhile only adds) and the stored
// bytes. The room must still be resident at both ends, or the heap would not hold it.
func measureRoom(t *testing.T, agent *loadAgent) roomMemory {
	t.Helper()
	service, artifactID := agent.service, agent.artifactID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	waitForLandedAppends(t, service, artifactID)
	// The settlement runs service.settle (20ms) after the last write; give it time to start
	// before waiting for it to clear what it owes.
	time.Sleep(500 * time.Millisecond)
	waitFor(t, 2*time.Minute, "the document's settlement", func() bool {
		owed, err := settlementPending(ctx, service.store.Pool, artifactID)
		return err == nil && !owed
	})
	waitForLandedAppends(t, service, artifactID)
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the room is not resident")
	}
	memory := roomMemory{heap: math.MaxInt64, stored: storedDocumentBytes(t, service, artifactID)}
	for range 3 {
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		memory.heap = min(memory.heap, int64(stats.HeapAlloc))
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the room is not resident")
	}
	return memory
}

// storedDocumentBytes is every byte Postgres holds for the document's Yjs state: its update log,
// its checkpoint and its named snapshots.
func storedDocumentBytes(t *testing.T, service *Service, artifactID string) int64 {
	t.Helper()
	var stored int64
	if err := service.store.Pool.QueryRow(context.Background(), `
		select (select coalesce(sum(octet_length(update)), 0) from doc_updates where artifact_id = $1)
			+ (select coalesce(sum(octet_length(state)), 0) from doc_checkpoints where artifact_id = $1)
			+ (select coalesce(sum(octet_length(state)), 0) from doc_snapshots where artifact_id = $1)
	`, artifactID).Scan(&stored); err != nil {
		t.Fatalf("sum the document's stored bytes: %v", err)
	}
	return stored
}

// requireWithinRoomBound requires what the room retained between base and end to be at most what
// the store gained plus roomAllowance, and logs both with the bound and the bytes per write.
func requireWithinRoomBound(t *testing.T, base, end roomMemory, writes int, total, elapsed time.Duration) {
	t.Helper()
	retained, stored := end.heap-base.heap, end.stored-base.stored
	bound := stored + roomAllowance
	t.Logf("BOUND retained=%d stored=%d bound=%d (stored + %d) writes=%d", retained, stored, bound, roomAllowance, writes)
	t.Logf("BOUND per write: retained=%.0f B stored=%.0f B; writes took %s, test %s",
		perWrite(retained, writes), perWrite(stored, writes), elapsed.Round(time.Millisecond), total.Round(time.Millisecond))
	if retained > bound {
		t.Errorf("BOUND exceeded: the room retained %d bytes over %d writes, %d more than the %d they stored plus %d", retained, writes, retained-bound, stored, roomAllowance)
	}
}

func perWrite(bytes int64, writes int) float64 {
	return float64(bytes) / float64(writes)
}

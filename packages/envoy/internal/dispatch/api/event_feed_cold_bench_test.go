package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
	"github.com/sjawhar/envoy/internal/logtest"
)

// coldFeedComments is how many comment.created events the cold benchmarks read: the page the 2x
// bound was stated over, which is also the page BenchmarkTheActivityFeedsWarmRead measures warm.
const coldFeedComments = 200

// coldFeedDisplayCache empties the display trees a restart empties, by filling it: one value of
// exactly its bound evicts everything else and is itself evicted by the first tree the read puts
// (bytelru.Put), so the read measured after this one starts from nothing and nothing of this call
// is left to measure. The filler's bytes are the caller's one allocation, handed back here under
// a key of their own each round, since a key the cache already holds is only marked used again.
//
// What no helper here empties is the connection pool's prepared statements, which a restart
// empties too: a real first read also parses each statement it runs, this one does not, and a
// cold read measured here is the work of the server alone.
func coldFeedDisplayCache(filler json.RawMessage, round int) {
	pmdoc.PutStoredDisplayTree(pmdoc.TextSum("a cold feed benchmark's display cache filler "+strconv.Itoa(round)),
		pmdoc.DisplayLatest, filler)
}

// coldFeedAnswers empties the answers the two lists keep, which a restart empties and which any
// write to the issue invalidates: a feed read after either is an answer encoded and compressed
// again.
func coldFeedAnswers(s *server) { s.listAnswers = newListAnswerCache() }

// coldFeedUnstoredTrees empties the table of stored trees, which is the state of a deployment
// between the migration and the end of the fill pass (texttreefill): the read finds no tree,
// reads every text for display itself, and stores what it read when the answer is written.
func coldFeedUnstoredTrees(b *testing.B, database *store.Store) {
	b.Helper()
	if _, err := database.Pool.Exec(b.Context(), `delete from text_trees`); err != nil {
		b.Fatalf("empty the stored trees: %v", err)
	}
}

// coldFeedFiller is the value coldFeedDisplayCache empties the display cache with: exactly the
// cache's bound, which is the largest value it keeps at all.
func coldFeedFiller() json.RawMessage { return make(json.RawMessage, pmdoc.DisplayCacheBytes) }

// BenchmarkTheActivityFeedsColdRead measures GET /api/v1/issues/{key}/events as a server answers
// it when nothing it kept is there to answer from. Every arm goes through the mux Register
// mounts and takes gzip, exactly as BenchmarkTheActivityFeedsWarmRead's do; what differs is what
// the read finds waiting:
//
//   - with trees: the restart itself. Both caches empty, the table holding the tree of every
//     text the page carries, which the writes that made them stored. This is the arm the 2x
//     bound is stated over, against the one below it.
//   - without trees: the same read by a caller that sends no header, the baseline of the bound.
//   - with trees, the trees cached: the answer invalidated but the trees still held, which is
//     every read after the first comment of a session - a write moves both lists' validators and
//     leaves the bodies this process has already read exactly where they were.
//   - with trees, the table empty: a deployment between the migration and the end of the fill
//     pass, where the read finds no stored tree, reads all 200 texts for display and stores what
//     it read before it returns.
//
// It is the cold half of the pair the bound is stated over. The warm benchmark measures what a
// dashboard's repeat reads cost; this measures what the first one after a deploy costs.
func BenchmarkTheActivityFeedsColdRead(b *testing.B) {
	logtest.SetDefault(b, slog.NewTextHandler(io.Discard, nil))

	handler, database, _, server := newRegisteredTestServer(b, testServerOptions{})
	benchmarkFeedProject(b, database, "FEED")
	target := benchmarkCommentFeed(b, handler, coldFeedComments)
	filler := coldFeedFiller()
	for _, read := range []struct {
		name     string
		trees    bool
		cached   bool
		unstored bool
	}{
		{name: "with trees", trees: true},
		{name: "without trees"},
		{name: "with trees, the trees cached", trees: true, cached: true},
		{name: "with trees, the table empty", trees: true, unstored: true},
	} {
		b.Run(read.name, func(b *testing.B) {
			b.ReportAllocs()
			round := 0
			for b.Loop() {
				b.StopTimer()
				round++
				coldFeedAnswers(server)
				if !read.cached {
					coldFeedDisplayCache(filler, round)
				}
				if read.unstored {
					coldFeedUnstoredTrees(b, database)
				}
				b.StartTimer()
				response := benchmarkFeedRead(b, handler, target, read.trees)
				if response.Code != http.StatusOK {
					b.Fatalf("GET %s = %d, want 200", target, response.Code)
				}
			}
		})
	}
}

// BenchmarkTheActivityFeedsColdStages measures each step a cold read with trees pays over a cold
// read without them, one at a time and on the same page of 200 comment events: the priming query
// that reads the stored trees, the cache puts that take them, the shaping that keys them by path,
// the JSON encode of the answer and its compression, the last two measured both ways so the
// difference the trees make in each is what the arm names.
//
// The steps are the handler's own calls in the handler's order (listIssueEvents), so what they
// add up to is the step list of the read itself and not a model of it.
func BenchmarkTheActivityFeedsColdStages(b *testing.B) {
	logtest.SetDefault(b, slog.NewTextHandler(io.Discard, nil))

	handler, database, _, server := newRegisteredTestServer(b, testServerOptions{})
	benchmarkFeedProject(b, database, "FEED")
	key := benchmarkColdFeedIssue(b, handler)
	ctx := b.Context()
	events, err := server.readEvents(ctx, issueOwner(key), eventListOptions{descending: true, limit: coldFeedComments})
	if err != nil {
		b.Fatalf("read the feed of %s: %v", key, err)
	}
	if len(events) != coldFeedComments {
		b.Fatalf("the feed of %s holds %d events, want %d", key, len(events), coldFeedComments)
	}
	texts := eventDisplayTexts(nil, events...)
	sums := make([][32]byte, 0, len(texts))
	seen := make(map[[32]byte]struct{}, len(texts))
	for _, text := range texts {
		sum := texttrees.Sum(text)
		if _, repeated := seen[sum]; repeated {
			continue
		}
		seen[sum] = struct{}{}
		sums = append(sums, sum)
	}
	table := texttrees.New(database.Pool)
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, sums)
	if err != nil {
		b.Fatalf("read the stored trees of %s: %v", key, err)
	}
	if len(stored) != len(sums) {
		b.Fatalf("the table holds %d of the feed's %d trees, want all of them", len(stored), len(sums))
	}
	filler := coldFeedFiller()

	b.Run("the texts of the page", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if found := eventDisplayTexts(nil, events...); len(found) != len(texts) {
				b.Fatalf("the page carries %d texts, want %d", len(found), len(texts))
			}
		}
	})

	b.Run("the priming query", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := table.Read(ctx, pmdoc.DisplayLatest, sums); err != nil {
				b.Fatalf("read the stored trees: %v", err)
			}
		}
	})

	b.Run("the cache puts", func(b *testing.B) {
		b.ReportAllocs()
		round := 0
		for b.Loop() {
			b.StopTimer()
			round++
			pmdoc.PutStoredDisplayTree(pmdoc.TextSum("a cold stage benchmark's filler "+strconv.Itoa(round)),
				pmdoc.DisplayLatest, filler)
			b.StartTimer()
			for _, sum := range sums {
				pmdoc.PutStoredDisplayTree(sum, pmdoc.DisplayLatest, stored[sum])
			}
		}
	})

	b.Run("the trees of the answer", func(b *testing.B) {
		b.ReportAllocs()
		shaped := make([]model.Event, len(events))
		for _, sum := range sums {
			pmdoc.PutStoredDisplayTree(sum, pmdoc.DisplayLatest, stored[sum])
		}
		for b.Loop() {
			copy(shaped, events)
			attachEventTrees(shaped, true)
		}
	})

	withTrees := make([]model.Event, len(events))
	copy(withTrees, events)
	attachEventTrees(withTrees, true)
	plain, err := encodeListAnswer(events)
	if err != nil {
		b.Fatalf("encode the feed without trees: %v", err)
	}
	treed, err := encodeListAnswer(withTrees)
	if err != nil {
		b.Fatalf("encode the feed with trees: %v", err)
	}

	for _, answer := range []struct {
		name    string
		events  []model.Event
		encoded listAnswerBytes
	}{
		{"with trees", withTrees, treed},
		{"without trees", events, plain},
	} {
		b.Run("the JSON encode/"+answer.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := encodeListAnswer(answer.events); err != nil {
					b.Fatalf("encode the feed: %v", err)
				}
			}
			b.ReportMetric(float64(len(answer.encoded)), "B/answer")
		})
		b.Run("the gzip/"+answer.name, func(b *testing.B) {
			b.ReportAllocs()
			compressed, err := gzipListAnswer(answer.encoded)
			if err != nil {
				b.Fatalf("compress the feed: %v", err)
			}
			for b.Loop() {
				if _, err := gzipListAnswer(answer.encoded); err != nil {
					b.Fatalf("compress the feed: %v", err)
				}
			}
			b.ReportMetric(float64(len(compressed)), "B/answer")
		})
	}

	b.Run("the trees read from the markdown", func(b *testing.B) {
		b.ReportAllocs()
		round := 0
		for b.Loop() {
			// The cache is emptied between rounds: DisplayTreeToStore answers a tree the cache
			// holds, and what this arm measures is the reading of a text no cache holds.
			b.StopTimer()
			round++
			coldFeedDisplayCache(filler, round)
			b.StartTimer()
			for _, text := range texts {
				if tree := pmdoc.DisplayTreeToStore(text, pmdoc.DisplayLatest); tree == nil {
					b.Fatalf("no tree of %q", text)
				}
			}
		}
	})
}

// benchmarkColdFeedIssue opens the issue the stage benchmarks read: one issue carrying the page
// of comments the bound was stated over, checked through the feed route exactly as
// benchmarkCommentFeed's is.
func benchmarkColdFeedIssue(b *testing.B, handler http.Handler) string {
	b.Helper()
	key := benchmarkFeedIssue(b, handler, "A cold feed of comments")
	for index := range coldFeedComments {
		benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
			"body": benchmarkFeedComment + "\n\nComment " + strconv.Itoa(index) + ".",
		}, http.StatusCreated)
	}
	benchmarkFeedTarget(b, handler, key, map[string]int{"comment.created": coldFeedComments})
	return key
}

package cireconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// pageSize is the per_page githubapp asks every listing for, so exactly this many objects fill
// a page and make the pass read the page after it.
const pageSize = 100

// failingWriter is a CI record writer with its first writes of check runs made to fail, so a
// test can see what a pass does with a head whose observations did not land: NATS refusing a
// write, or a connection gone while the pass ran. Every other call goes to the writer it wraps,
// which is the listener's own bucket.
type failingWriter struct {
	Writer

	mu sync.Mutex
	// records is how many more calls of Record fail before the writes land again.
	records int
}

func (w *failingWriter) Record(ctx context.Context, observation contracts.CIObservation) error {
	w.mu.Lock()
	failing := w.records > 0
	if failing {
		w.records--
	}
	w.mu.Unlock()
	if failing {
		return errors.New("the CI record store refused the write")
	}
	return w.Writer.Record(ctx, observation)
}

// failWrites makes the pass bind a writer whose first records writes of check runs fail. The
// pass binds its writer itself, on the first pass that can, so the wrapping goes around Open.
func (h *harness) failWrites(records int) *failingWriter {
	h.t.Helper()
	failing := &failingWriter{records: records}
	bind := h.reconciler.Open
	h.reconciler.Open = func() (Writer, error) {
		bound, err := bind()
		if err != nil {
			return nil, err
		}
		failing.Writer = bound
		return failing, nil
	}
	return failing
}

// Nothing changed on GitHub between two passes, so the second reads nothing in full: every page
// of the head's two listings is read with the ETag GitHub answered the first pass with, GitHub
// answers 304 to each, and the head's record is left exactly where it was.
func TestNothingChangedSoNothingIsReadInFull(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z"),
	)
	settlements := h.subscribe()

	h.pass()

	h.settlement(settlements, "after the first pass")
	_, before := h.settledRecord()
	sentBefore := h.github.count()

	h.pass()

	for _, path := range []string{checkRunsPath, checkSuitesPath} {
		requests := h.github.sincePath(sentBefore, path)
		if len(requests) != 1 {
			t.Fatalf("the second pass read %s %d times, want once: %+v", path, len(requests), requests)
		}
		if want := h.github.etagOf(requests[0].url); requests[0].ifNoneMatch != want {
			t.Errorf("the second pass read %s with If-None-Match %q, want the ETag the first pass was answered with, %q",
				path, requests[0].ifNoneMatch, want)
		}
	}
	// Every page answered 304, which is what counts the head among those GitHub had nothing new
	// for and leaves the pass with nothing to write.
	line := h.passLine()
	if got := number(t, line, "heads_not_modified"); got != 1 {
		t.Errorf("the second pass's heads_not_modified = %d, want 1: %v", got, line)
	}
	if got := number(t, line, "records"); got != 0 {
		t.Errorf("the second pass's records = %d, want 0: %v", got, line)
	}
	h.noSettlement(settlements, "after a pass over a head nothing changed at")
	_, after, ok := h.record()
	if !ok {
		t.Fatal("the head's record is gone after the second pass")
	}
	if after != before {
		t.Fatalf("the record's revision moved from %d to %d over a pass that read nothing in full", before, after)
	}
}

// A write that did not land is not hidden behind an ETag: the pass whose write of a run failed
// keeps no ETag for that head, so the next pass reads it in full, records the run, and the head
// settles. Were the ETag kept, GitHub would answer 304 for ever and the run the record lacks
// would never reach it.
func TestAWriteThatDidNotLandIsNotHiddenBehindAnETag(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.failWrites(1)
	settlements := h.subscribe()

	h.pass()

	if _, _, ok := h.record(); ok {
		t.Fatal("the head has a record although the pass's one write failed")
	}
	if !h.logged("ci reconcile did not record a check run") {
		t.Fatalf("the failed write was not warned about:\n%s", h.logs.String())
	}
	sentBefore := h.github.count()

	h.pass()

	requests := h.github.sincePath(sentBefore, checkRunsPath)
	if len(requests) != 1 {
		t.Fatalf("the next pass read the head's check runs %d times, want once: %+v", len(requests), requests)
	}
	if requests[0].ifNoneMatch != "" {
		t.Fatalf("the next pass read the head's check runs with If-None-Match %q although the write before it did not land",
			requests[0].ifNoneMatch)
	}
	settled := h.settlement(settlements, "after the pass whose write landed")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the pass whose write landed")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %+v, want ci-ok", state.Checks)
	}
}

// News of GitHub's own is written once: a completed suite GitHub has restamped -- the same id,
// status and conclusion under a later updated_at -- is written and settles once, and a pass over
// the answers as they now stand writes nothing and settles nothing more.
func TestGitHubsOwnNewsIsWrittenOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z"),
	)
	settlements := h.subscribe()

	h.pass()

	h.settlement(settlements, "after the first pass")
	_, before := h.settledRecord()

	// GitHub stamps the suite again with nothing else changed, which changes the ETag of the
	// listing and so is read in full.
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:09:00Z"),
	)

	h.pass()

	h.settlement(settlements, "after GitHub restamped the suite")
	_, after := h.settledRecord()
	if after == before {
		t.Fatalf("the record's revision stayed at %d; GitHub's newer stamp of the suite was not written", before)
	}

	h.pass()

	h.noSettlement(settlements, "after a pass over the answers as they stand")
	_, last, ok := h.record()
	if !ok {
		t.Fatal("the head's record is gone after the third pass")
	}
	if last != after {
		t.Fatalf("the record's revision moved from %d to %d; the restamped suite was written twice", after, last)
	}
}

// A head whose two listings are both empty keeps its ETags: nothing was recorded from it, so
// there is no record for it to have lost, and dropping them would read it in full at every pass
// for ever.
func TestAHeadWithNothingToRecordKeepsItsETags(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	// GitHub holds no check run and no check suite of this head: each listing answers empty.

	h.pass()

	for _, path := range []string{checkRunsPath, checkSuitesPath} {
		requests := h.github.sincePath(0, path)
		if len(requests) != 1 {
			t.Fatalf("the first pass read %s %d times, want once: %+v", path, len(requests), requests)
		}
		if requests[0].ifNoneMatch != "" {
			t.Fatalf("the first pass read %s conditionally, with If-None-Match %q", path, requests[0].ifNoneMatch)
		}
	}
	if _, _, ok := h.record(); ok {
		t.Fatal("a head with no checks at all was given a record")
	}

	for pass := 2; pass <= 3; pass++ {
		sentBefore := h.github.count()

		h.pass()

		for _, path := range []string{checkRunsPath, checkSuitesPath} {
			requests := h.github.sincePath(sentBefore, path)
			if len(requests) != 1 {
				t.Fatalf("pass %d read %s %d times, want once: %+v", pass, path, len(requests), requests)
			}
			if want := h.github.etagOf(requests[0].url); requests[0].ifNoneMatch != want {
				t.Fatalf("pass %d read %s with If-None-Match %q, want the ETag it was answered with before, %q",
					pass, path, requests[0].ifNoneMatch, want)
			}
		}
		if got := number(t, h.passLine(), "heads_not_modified"); got != 1 {
			t.Fatalf("pass %d's heads_not_modified = %d, want 1: GitHub answered 304 for both its listings", pass, got)
		}
	}
}

// A listing is read page by page: a head whose check runs fill the first page and spill onto a
// second is read to its end and every run of both pages recorded, and a run added to the second
// page afterwards is found by the next pass, whose first page answers 304.
func TestAListingIsReadPageByPage(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	// One full page of GitHub's own size, and three runs on the page after it.
	runs := make([]object, 0, pageSize+3)
	for id := 1; id <= pageSize+3; id++ {
		runs = append(runs, checkRun(int64(id), fmt.Sprintf("check-%03d", id), "completed", "success", otherApp, "2026-11-05T03:01:00Z"))
	}
	h.github.setCheckRuns(gitHubRepository, headSHA, runs...)
	settlements := h.subscribe()

	h.pass()

	h.settlement(settlements, "after the pass over a listing of two pages")
	state, before := h.settledRecord()
	if len(state.Checks) != len(runs) {
		t.Fatalf("the record holds %d checks, want the %d runs of both pages", len(state.Checks), len(runs))
	}
	requests := h.github.sincePath(0, checkRunsPath)
	if len(requests) != 2 {
		t.Fatalf("the first pass read the head's check runs %d times, want the two pages: %+v", len(requests), requests)
	}
	if !strings.Contains(requests[0].url, "page=1") || !strings.Contains(requests[1].url, "page=2") {
		t.Fatalf("the pages were read as %s then %s, want page 1 then page 2", requests[0].url, requests[1].url)
	}
	// Each page keeps an ETag of its own, which the pages of the next pass are read with.
	pages := []string{h.github.etagOf(requests[0].url), h.github.etagOf(requests[1].url)}

	// GitHub runs one more check at the same head, which lands on the second page.
	runs = append(runs, checkRun(int64(pageSize+4), "check-added", "completed", "success", otherApp, "2026-11-05T03:05:00Z"))
	h.github.setCheckRuns(gitHubRepository, headSHA, runs...)
	sentBefore := h.github.count()

	h.pass()

	requests = h.github.sincePath(sentBefore, checkRunsPath)
	if len(requests) != 2 {
		t.Fatalf("the next pass read the head's check runs %d times, want both pages again: %+v", len(requests), requests)
	}
	for page, request := range requests {
		if request.ifNoneMatch != pages[page] {
			t.Errorf("the next pass read page %d with If-None-Match %q, want the ETag that page was answered with, %q",
				page+1, request.ifNoneMatch, pages[page])
		}
	}
	h.settlement(settlements, "after the run added to the second page")
	state, after := h.settledRecord()
	if after == before {
		t.Fatalf("the record's revision stayed at %d; the run added to the second page was not recorded", before)
	}
	if len(state.Checks) != len(runs) {
		t.Fatalf("the record holds %d checks, want %d", len(state.Checks), len(runs))
	}
	if _, held := state.Checks["check-added"]; !held {
		t.Fatalf("the record does not hold the run added to the second page: %+v", state.Checks)
	}
}

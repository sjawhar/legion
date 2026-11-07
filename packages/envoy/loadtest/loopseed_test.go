package main

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// loopTestPlan is the plan of the embedded source file at seed 1, planned once for every test that
// reads it: one plan walks all 464,602 comment rows, and no test changes the plan it gets.
var (
	loopTestOnce sync.Once
	loopTestPlan *loopPlan
	loopTestErr  error
)

func loopPlanned(t *testing.T) *loopPlan {
	t.Helper()
	loopTestOnce.Do(func() {
		rows, err := readLoopRows(loopSource, loopSourceTSV)
		if err != nil {
			loopTestErr = err
			return
		}
		loopTestPlan, loopTestErr = planLoopSeed(1, rows)
	})
	if loopTestErr != nil {
		t.Fatal(loopTestErr)
	}
	return loopTestPlan
}

// loopSourceRows parses the embedded source file.
func loopSourceRows(t *testing.T) []loopRow {
	t.Helper()
	rows, err := readLoopRows(loopSource, loopSourceTSV)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestLoopSourceTotals reads the embedded source file and counts it: the seed's identity is these
// eight totals, and every count the planner makes is one of them spread over proposals.
func TestLoopSourceTotals(t *testing.T) {
	rows := loopSourceRows(t)
	var ids, live, replies, drivers, empty int
	var thread, doc int64
	for _, r := range rows {
		ids += r.IDs
		live += r.LiveIDs
		replies += r.ReplyLines
		thread += r.ThreadBytes
		doc += r.DocBytes
		if r.LiveInDriver {
			drivers++
		}
		if r.DocBytes == 0 {
			empty++
		}
	}
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"rows", int64(len(rows)), 891},
		{"ids", int64(ids), 334_536},
		{"live_ids", int64(live), 7_653},
		{"reply_lines", int64(replies), 130_066},
		{"thread_bytes", thread, 564_113_205},
		{"doc_bytes", doc, 282_508_175},
		{"live_in_driver rows", int64(drivers), 536},
		{"empty doc_bytes rows", int64(empty), 5},
	} {
		if c.got != c.want {
			t.Errorf("%s totals %d, want %d", c.name, c.got, c.want)
		}
	}
	if got := ids + replies; got != loopCommentRows {
		t.Errorf("the seed's comment rows are %d, want %d", got, loopCommentRows)
	}
}

// loopTSVEdit is the source file with fields of its data rows replaced: the key is the row's index
// among the data rows and the column's index in it.
func loopTSVEdit(edits map[[2]int]string) string {
	lines := strings.Split(strings.TrimSuffix(loopSourceTSV, "\n"), "\n")
	for at, v := range edits {
		f := strings.Split(lines[at[0]+1], "\t")
		f[at[1]] = v
		lines[at[0]+1] = strings.Join(f, "\t")
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestLoopSourceRefusesATotalThatDiffers changes one total of the source file at a time: a file
// that is not the seed's own is refused by the name of the total that differs, never planned under
// the seed's name.
func TestLoopSourceRefusesATotalThatDiffers(t *testing.T) {
	rows := loopSourceRows(t)
	firstDoc, secondDoc, firstQuiet := -1, -1, -1
	for i, r := range rows {
		switch {
		case r.DocBytes > 0 && firstDoc < 0:
			firstDoc = i
		case r.DocBytes > 0 && secondDoc < 0:
			secondDoc = i
		}
		if !r.LiveInDriver && firstQuiet < 0 {
			firstQuiet = i
		}
	}
	if firstDoc < 0 || secondDoc < 0 || firstQuiet < 0 {
		t.Fatalf("the source file has no row to change: doc_bytes rows %d and %d, a row off the driver %d",
			firstDoc, secondDoc, firstQuiet)
	}
	cases := []struct {
		name string
		text string
	}{
		{"rows", loopSourceTSV + "extra proposal\t0\t0\t0\t0\t0\t0\t0\t0\t0\t1\tno\n"},
		{"ids", loopTSVEdit(map[[2]int]string{{0, 1}: strconv.Itoa(rows[0].IDs + 1)})},
		{"live_ids", loopTSVEdit(map[[2]int]string{{0, 2}: strconv.Itoa(rows[0].LiveIDs + 1)})},
		{"reply_lines", loopTSVEdit(map[[2]int]string{{0, 9}: strconv.Itoa(rows[0].ReplyLines + 1)})},
		{"thread_bytes", loopTSVEdit(map[[2]int]string{{0, 8}: strconv.FormatInt(rows[0].ThreadBytes+1, 10)})},
		{"doc_bytes", loopTSVEdit(map[[2]int]string{{firstDoc, 10}: strconv.FormatInt(rows[firstDoc].DocBytes+1, 10)})},
		{"live_in_driver rows", loopTSVEdit(map[[2]int]string{{firstQuiet, 11}: "yes"})},
		// The text one row loses goes to another, so doc_bytes still totals what it did and the
		// rows with none are one more than the five the seed holds.
		{"empty doc_bytes rows", loopTSVEdit(map[[2]int]string{
			{firstDoc, 10}:  "0",
			{secondDoc, 10}: strconv.FormatInt(rows[firstDoc].DocBytes+rows[secondDoc].DocBytes, 10),
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readLoopRows(loopSource, c.text)
			if err == nil {
				t.Fatalf("a source file whose %s differs was read without a word", c.name)
			}
			if !strings.Contains(err.Error(), c.name) {
				t.Errorf("the refusal is %q, which does not name %s", err, c.name)
			}
		})
	}
	t.Run("header", func(t *testing.T) {
		_, err := readLoopRows(loopSource, "proposal\tids\n")
		if err == nil || !strings.Contains(err.Error(), "header is") {
			t.Errorf("a file with other columns was refused with %v, want a refusal naming the header", err)
		}
	})
	t.Run("column count", func(t *testing.T) {
		_, err := readLoopRows(loopSource, loopHeader+"\nname\t1\t2\n")
		if err == nil || !strings.Contains(err.Error(), "columns, want 12") {
			t.Errorf("a short row was refused with %v, want a refusal naming the columns", err)
		}
	})
}

// TestLoopPlanTotals is the seed the plan holds: one issue per proposal, every comment row and
// every byte of the source file's threads, and the loop's review pages.
func TestLoopPlanTotals(t *testing.T) {
	p := loopPlanned(t)
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"issues", int64(p.Totals.Issues), 891},
		{"comment rows", int64(p.Totals.CommentRows), 464_602},
		{"thread bytes", p.Totals.ThreadBytes, 564_113_205},
		{"review pages", int64(p.Totals.ReviewPages), 6_057},
		{"review page bytes", p.Totals.ReviewPageBytes, 60_291_378},
		{"review indexes", int64(p.Totals.ReviewIndexes), 536},
	} {
		if c.got != c.want {
			t.Errorf("the plan's %s total %d, want %d", c.name, c.got, c.want)
		}
	}
	if p.Totals.Threads+p.Totals.Replies != p.Totals.CommentRows {
		t.Errorf("%d threads and %d replies are not the %d comment rows",
			p.Totals.Threads, p.Totals.Replies, p.Totals.CommentRows)
	}
	if p.Source != loopSource || p.SourceSHA256 != loopSourceSum() || p.Seed != 1 {
		t.Errorf("the plan names %s at %s under seed %d", p.Source, p.SourceSHA256, p.Seed)
	}
	// Every proposal is an issue of its own, with its row's counts and at least one part.
	rows := loopSourceRows(t)
	parts, generated := 0, 0
	for i, pr := range p.Proposals {
		parts += len(pr.Parts)
		if pr.GeneratedDoc {
			generated++
		}
		if pr.Name != rows[i].Name || pr.Index != i || pr.LiveInDriver != rows[i].LiveInDriver ||
			pr.IDs != rows[i].IDs || pr.ReplyLines != rows[i].ReplyLines ||
			pr.ThreadBytes != rows[i].ThreadBytes || len(pr.Parts) == 0 {
			t.Fatalf("proposal %d is %+v, want row %+v", i, pr, rows[i])
		}
	}
	if parts != p.Totals.Parts {
		t.Errorf("the proposals hold %d parts, and the totals say %d", parts, p.Totals.Parts)
	}
	// The five proposals whose own text the source file counts as 0 bytes get a document all the
	// same: a thread anchors on a quote in a document.
	if generated != loopEmptyDocs {
		t.Errorf("%d proposals got a generated document, want %d", generated, loopEmptyDocs)
	}
}

// TestLoopPlanPartsRespectTheCaps checks every part of the plan against the Dispatch caps the
// issue's budgets come from, and against the quote rows an anchored write needs.
func TestLoopPlanPartsRespectTheCaps(t *testing.T) {
	p := loopPlanned(t)
	t.Run("margin budgets", func(t *testing.T) {
		for _, pr := range p.Proposals {
			for i, part := range pr.Parts {
				want := int64(loopEarlierMargin)
				if i == len(pr.Parts)-1 {
					want = loopLastMargin
				}
				if part.ThreadBytes > want {
					t.Fatalf("proposal %d (%s) part %d of %d: its threads hold %d bytes, past %d",
						pr.Index, pr.Name, i, len(pr.Parts), part.ThreadBytes, want)
				}
			}
		}
	})
	t.Run("markdown and elements", func(t *testing.T) {
		for _, pr := range p.Proposals {
			for i, part := range pr.Parts {
				if part.MarkdownBytes > loopMarkdownBudget {
					t.Fatalf("proposal %d (%s) part %d: %d bytes of markdown, past %d",
						pr.Index, pr.Name, i, part.MarkdownBytes, loopMarkdownBudget)
				}
				if part.Elements > loopElementCap {
					t.Fatalf("proposal %d (%s) part %d: %d elements, past %d",
						pr.Index, pr.Name, i, part.Elements, loopElementCap)
				}
			}
		}
	})
	t.Run("spare quote rows", func(t *testing.T) {
		for _, pr := range p.Proposals {
			for i, part := range pr.Parts {
				want := 0
				if i == len(pr.Parts)-1 {
					want = loopSpareQuotes
				}
				if part.SpareQuotes != want {
					t.Fatalf("proposal %d (%s) part %d of %d carries %d spare quote rows, want %d",
						pr.Index, pr.Name, i, len(pr.Parts), part.SpareQuotes, want)
				}
			}
		}
	})
	t.Run("one distinct quote row per thread", func(t *testing.T) {
		for _, pr := range p.Proposals {
			next := 0
			for i, part := range pr.Parts {
				if part.QuoteRows != part.Threads {
					t.Fatalf("proposal %d (%s) part %d: %d quote rows for %d threads",
						pr.Index, pr.Name, i, part.QuoteRows, part.Threads)
				}
				if part.Threads == 0 {
					continue
				}
				if part.FirstThread != next {
					t.Fatalf("proposal %d (%s) part %d starts at thread %d, want %d",
						pr.Index, pr.Name, i, part.FirstThread, next)
				}
				next += part.Threads
				rows := map[string]bool{}
				for j := part.FirstThread; j < next; j++ {
					row := loopQuoteRow(pr.Index, j)
					if rows[row] {
						t.Fatalf("proposal %d (%s) part %d holds %q twice", pr.Index, pr.Name, i, row)
					}
					rows[row] = true
				}
				for s := range part.SpareQuotes {
					row := loopSpareRow(pr.Index, s)
					if rows[row] {
						t.Fatalf("proposal %d (%s) part %d holds %q twice", pr.Index, pr.Name, i, row)
					}
					rows[row] = true
				}
			}
			if next != pr.IDs {
				t.Fatalf("proposal %d (%s) spreads %d threads over its parts, want its %d ids",
					pr.Index, pr.Name, next, pr.IDs)
			}
		}
	})
	// Rendering every part would build the loop's whole 376 MB of markdown, so a sample stands
	// for it: every 97th proposal, the five whose document the planner generated, and the
	// largest, whose 19 parts are the fullest in the plan.
	t.Run("the markdown holds the rows the plan counted", func(t *testing.T) {
		largest := 0
		for _, pr := range p.Proposals {
			if pr.IDs > p.Proposals[largest].IDs {
				largest = pr.Index
			}
		}
		if p.Proposals[largest].Name != "DS-exec-context" {
			t.Fatalf("the largest proposal is %s, want DS-exec-context", p.Proposals[largest].Name)
		}
		for _, pr := range p.Proposals {
			if pr.Index%97 != 0 && pr.Index != largest && !pr.GeneratedDoc {
				continue
			}
			for i, part := range pr.Parts {
				doc := p.partDocument(pr.Index, i)
				if int64(len(doc)) != part.MarkdownBytes {
					t.Fatalf("proposal %d (%s) part %d renders %d bytes, and the plan counted %d",
						pr.Index, pr.Name, i, len(doc), part.MarkdownBytes)
				}
				blocks := strings.Split(strings.TrimSuffix(doc, "\n\n"), "\n\n")
				if len(blocks) != part.Elements {
					t.Fatalf("proposal %d (%s) part %d renders %d elements, and the plan counted %d",
						pr.Index, pr.Name, i, len(blocks), part.Elements)
				}
				held := map[string]bool{}
				for _, b := range blocks {
					held[b] = true
				}
				for _, th := range p.partThreads(pr.Index, i) {
					if !held[th.Quote] {
						t.Fatalf("proposal %d (%s) part %d has no line %q for its thread to anchor on",
							pr.Index, pr.Name, i, th.Quote)
					}
				}
				for s := range part.SpareQuotes {
					if !held[loopSpareRow(pr.Index, s)] {
						t.Fatalf("proposal %d (%s) part %d is missing spare quote row %d", pr.Index, pr.Name, i, s)
					}
				}
			}
		}
	})
}

// TestLoopPlanPartsExpandToTheirCounts expands every part of the plan and weighs what comes out
// against what the plan recorded: the writer sends what the plan counted, and no body or thread
// record it sends is one Dispatch refuses. Expanding all 334,536 threads takes about a third of a
// second, since each part draws its proposal's comment sizes again.
func TestLoopPlanPartsExpandToTheirCounts(t *testing.T) {
	p := loopPlanned(t)
	var threads, replies, rows int
	var bytes int64
	for _, pr := range p.Proposals {
		status := map[string]int{}
		open := 0
		for i, part := range pr.Parts {
			var partBytes int64
			var partReplies int
			got := p.partThreads(pr.Index, i)
			if len(got) != part.Threads {
				t.Fatalf("proposal %d (%s) part %d expands to %d threads, and the plan counted %d",
					pr.Index, pr.Name, i, len(got), part.Threads)
			}
			for _, th := range got {
				if th.Proposal != pr.Index || th.Part != i || th.Quote != loopQuoteRow(pr.Index, th.Index) {
					t.Fatalf("proposal %d (%s) part %d expanded thread %+v", pr.Index, pr.Name, i, th)
				}
				for _, b := range th.Bodies {
					// Every body is ASCII, so its bytes are its UTF-16 units against the cap.
					if b < 1 || b > loopBodyCap {
						t.Fatalf("proposal %d (%s) thread %d holds a body of %d bytes, want 1 to %d",
							pr.Index, pr.Name, th.Index, b, loopBodyCap)
					}
				}
				if th.Bytes() > loopRecordCap {
					t.Fatalf("proposal %d (%s) thread %d is a record of %d bytes, past %d",
						pr.Index, pr.Name, th.Index, th.Bytes(), loopRecordCap)
				}
				status[th.Status]++
				if th.Open {
					open++
				}
				partBytes += th.Bytes()
				partReplies += len(th.Bodies) - 1
				rows += len(th.Bodies)
			}
			if partBytes != part.ThreadBytes || partReplies != part.Replies {
				t.Fatalf("proposal %d (%s) part %d expands to %d bytes in %d replies, and the plan counted %d in %d",
					pr.Index, pr.Name, i, partBytes, partReplies, part.ThreadBytes, part.Replies)
			}
			threads += len(got)
			replies += partReplies
			bytes += partBytes
		}
		// A proposal's threads carry its own status mix, and its live ids are the open ones.
		if open != pr.LiveIDs || status["live"] != pr.LiveIDs || status["met"] != pr.Met ||
			status["rejected"] != pr.Rejected || status["parked"] != pr.Parked ||
			status["withdrawn"] != pr.Withdrawn || status["other"] != pr.Other {
			t.Fatalf("proposal %d (%s) expands to %v with %d open, want %d live, %d met, %d rejected, %d parked, %d withdrawn, %d other",
				pr.Index, pr.Name, status, open, pr.LiveIDs, pr.Met, pr.Rejected, pr.Parked,
				pr.Withdrawn, pr.Other)
		}
	}
	if threads != p.Totals.Threads || replies != p.Totals.Replies || rows != p.Totals.CommentRows {
		t.Errorf("the parts expand to %d threads, %d replies and %d comment rows; the totals say %d, %d and %d",
			threads, replies, rows, p.Totals.Threads, p.Totals.Replies, p.Totals.CommentRows)
	}
	if bytes != loopThreadBytes {
		t.Errorf("the parts expand to %d thread bytes, want %d", bytes, loopThreadBytes)
	}
}

// TestLoopPlanIsDeterministic plans the seed twice: one seed gives one plan, byte for byte, and
// another seed gives another, so a run can be repeated and two runs can differ.
func TestLoopPlanIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	plan := func(name string, args ...string) []byte {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := runLoopSeedPlan(append(args, "-out", path)); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := plan("first.json")
	second := plan("second.json")
	if !slices.Equal(first, second) {
		t.Errorf("two runs at seed 1 wrote %d and %d bytes that differ", len(first), len(second))
	}
	other := plan("other.json", "-seed", "2")
	if slices.Equal(first, other) {
		t.Error("seed 2 wrote the plan of seed 1")
	}
	if !strings.Contains(string(first), `"seed": 1`) || !strings.Contains(string(other), `"seed": 2`) {
		t.Error("the plan does not name the seed it was drawn under")
	}
	if err := runLoopSeedPlan([]string{"-out", filepath.Join(dir, "x.json"), "stray"}); err == nil ||
		!strings.Contains(err.Error(), `unexpected argument "stray"`) {
		t.Errorf("a stray argument was taken: %v", err)
	}
}

// TestLoopReviewPageDistribution weighs the review pages the plan drew: their total is the load
// the loop's own pages are, and their median and mean are the distribution it measured.
func TestLoopReviewPageDistribution(t *testing.T) {
	p := loopPlanned(t)
	sizes := make([]int64, 0, len(p.ReviewPages))
	var total int64
	for _, page := range p.ReviewPages {
		if page.Bytes < 1 {
			t.Fatalf("review page %d of proposal %d is %d bytes", page.Index, page.Proposal, page.Bytes)
		}
		sizes = append(sizes, page.Bytes)
		total += page.Bytes
	}
	slices.Sort(sizes)
	median := float64(sizes[(len(sizes)-1)/2])
	mean := float64(total) / float64(len(sizes))
	if math.Abs(median-loopPageMedian)/loopPageMedian > 0.01 {
		t.Errorf("the pages' median is %.0f bytes, want %d within 1%%", median, loopPageMedian)
	}
	if math.Abs(mean-loopPageMean)/loopPageMean > 0.01 {
		t.Errorf("the pages' mean is %.1f bytes, want %d within 1%%", mean, loopPageMean)
	}
	if int64(median) != p.Totals.ReviewPageMedianBytes || mean != p.Totals.ReviewPageMeanBytes {
		t.Errorf("the plan records a median of %d and a mean of %.1f, and its pages give %.0f and %.1f",
			p.Totals.ReviewPageMedianBytes, p.Totals.ReviewPageMeanBytes, median, mean)
	}
	// Every live proposal has a review index and at least one page, and the pages are shared out
	// in proportion to its requirement ids.
	pages := map[int]int{}
	for _, page := range p.ReviewPages {
		pages[page.Proposal]++
	}
	live := 0
	for _, pr := range p.Proposals {
		if !pr.LiveInDriver {
			if pages[pr.Index] != 0 {
				t.Errorf("proposal %d (%s) is off the driver and got %d review pages", pr.Index, pr.Name, pages[pr.Index])
			}
			continue
		}
		live++
		if pages[pr.Index] < 1 {
			t.Errorf("proposal %d (%s) is live on the driver and got no review page", pr.Index, pr.Name)
		}
	}
	if len(p.ReviewIndexes) != live {
		t.Errorf("%d review indexes for %d live proposals", len(p.ReviewIndexes), live)
	}
	for _, idx := range p.ReviewIndexes {
		if idx.Pages != pages[idx.Proposal] || idx.Bytes < 1 {
			t.Errorf("the review index of proposal %d names %d pages of %d bytes, and the plan holds %d pages",
				idx.Proposal, idx.Pages, idx.Bytes, pages[idx.Proposal])
		}
	}
	biggest, smallest := p.Proposals[0].Index, p.Proposals[0].Index
	for _, pr := range p.Proposals {
		if !pr.LiveInDriver {
			continue
		}
		if pr.IDs > p.Proposals[biggest].IDs {
			biggest = pr.Index
		}
		if p.Proposals[smallest].IDs == 0 || (pr.IDs > 0 && pr.IDs < p.Proposals[smallest].IDs) {
			smallest = pr.Index
		}
	}
	if pages[biggest] <= pages[smallest] {
		t.Errorf("the largest live proposal (%d ids) got %d pages and the smallest (%d ids) got %d",
			p.Proposals[biggest].IDs, pages[biggest], p.Proposals[smallest].IDs, pages[smallest])
	}
}

// TestLoopShareGivesEachWeightAtLeastOne is the share the review pages are spread by: in
// proportion, by largest remainder, every weight with one of its own, and the total exact however
// small the weights are.
func TestLoopShareGivesEachWeightAtLeastOne(t *testing.T) {
	cases := []struct {
		name    string
		total   int
		weights []int
		want    []int
	}{
		{"proportional", 10, []int{1, 1, 3}, []int{2, 2, 6}},
		{"largest remainder first", 7, []int{1, 1, 1}, []int{3, 2, 2}},
		// Weights so uneven that the ones rounded up take the share past the total: the smallest
		// remainders give it back, and none of them loses its one.
		{"a one for every weight", 4, []int{100, 1, 1, 1}, []int{1, 1, 1, 1}},
		{"zero weights", 5, []int{8, 0, 0}, []int{3, 1, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := loopShare(c.total, c.weights)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("loopShare(%d, %v) = %v, want %v", c.total, c.weights, got, c.want)
			}
			sum := 0
			for _, n := range got {
				sum += n
			}
			if sum != c.total {
				t.Errorf("loopShare(%d, %v) shares %d", c.total, c.weights, sum)
			}
		})
	}
	if _, err := loopShare(2, []int{1, 1, 1}); err == nil {
		t.Error("a total too small for one each was shared out all the same")
	}
}

// TestLoopRehearsalIsATrueSubsetOfTheWholePlan: a rehearsal of n proposals holds the whole plan's
// first n unchanged, the review pages and indexes of those proposals alone, and totals of its own,
// so what it writes is byte for byte what the whole seed writes for those proposals.
func TestLoopRehearsalIsATrueSubsetOfTheWholePlan(t *testing.T) {
	whole := loopPlanned(t)
	const n = 10
	cut := loopRehearsal(whole, n)

	if cut.Rehearsal != n {
		t.Errorf("rehearsal = %d, want %d", cut.Rehearsal, n)
	}
	if len(cut.Proposals) != n {
		t.Fatalf("proposals = %d, want %d", len(cut.Proposals), n)
	}
	for i := range cut.Proposals {
		if !reflect.DeepEqual(cut.Proposals[i], whole.Proposals[i]) {
			t.Fatalf("proposal %d of the rehearsal is not the whole plan's", i)
		}
	}
	for _, page := range cut.ReviewPages {
		if page.Proposal >= n {
			t.Fatalf("the rehearsal holds review page %d of proposal %d, which it does not hold", page.Index, page.Proposal)
		}
	}
	var pages, indexes int
	for _, page := range whole.ReviewPages {
		if page.Proposal < n {
			pages++
		}
	}
	for _, idx := range whole.ReviewIndexes {
		if idx.Proposal < n {
			indexes++
		}
	}
	if len(cut.ReviewPages) != pages || len(cut.ReviewIndexes) != indexes {
		t.Errorf("rehearsal has %d pages and %d indexes, want the whole plan's %d and %d for those proposals",
			len(cut.ReviewPages), len(cut.ReviewIndexes), pages, indexes)
	}
	if cut.Totals.Issues != n || cut.Totals.ReviewPages != pages {
		t.Errorf("totals = %+v, want %d issues and %d review pages", cut.Totals, n, pages)
	}
	if cut.Totals.ThreadBytes >= whole.Totals.ThreadBytes {
		t.Errorf("rehearsal thread bytes %d, want fewer than the whole plan's %d", cut.Totals.ThreadBytes, whole.Totals.ThreadBytes)
	}
	if same := loopRehearsal(whole, 0); same != whole {
		t.Error("a rehearsal of 0 proposals is not the whole plan")
	}
	if same := loopRehearsal(whole, len(whole.Proposals)+1); same != whole {
		t.Error("a rehearsal of more proposals than the plan holds is not the whole plan")
	}
}

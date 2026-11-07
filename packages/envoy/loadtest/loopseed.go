package main

import (
	"cmp"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The Verified Linux spec loop's own seed, planned without a stack (`loadgen loop-seed-plan`).
//
// The loop runs 891 proposals, each a Dispatch issue whose requirement ids are comment threads on
// the proposal's part documents, with review pages and a review index for each proposal the
// driver is live on. proposals-64796913-byid.tsv counts what those records weigh today; this
// planner turns that file into the plan of the records a deployment must hold to carry the loop,
// which a later writer sends through Dispatch's API. It reads nothing from the network, draws
// every size from -seed alone, and writes one JSON file.
//
// The plan records each part's counts and byte totals, never one record per thread: 334,536
// thread records would make a file nobody can read. partThreads expands one part into its threads
// as a pure function of the plan and the indexes, so the writer sends what the plan counted.

// loopSource is the file the seed is counted from: one row per proposal of the Verified Linux spec
// loop, by requirement id, as the loop held them on 2026-10-26.
const loopSource = "proposals-64796913-byid.tsv"

//go:embed proposals-64796913-byid.tsv
var loopSourceTSV string

// loopHeader is the source file's first line. A file whose columns are not these ones is refused
// before any total is counted.
const loopHeader = "proposal\tids\tlive_ids\tmet\trejected\tparked\twithdrawn\tother\t" +
	"thread_bytes\treply_lines\tdoc_bytes\tlive_in_driver"

// The source file's verified totals. They are the seed's identity: a file that disagrees with any
// of them would make every count below wrong, so readLoopRows refuses it by the name of the total
// that differs rather than planning a different seed under the same name.
const (
	loopRows        = 891
	loopIDs         = 334_536
	loopLiveIDs     = 7_653
	loopReplyLines  = 130_066
	loopThreadBytes = 564_113_205
	loopDocBytes    = 282_508_175
	loopDriverRows  = 536
	loopEmptyDocs   = 5
)

// loopCommentRows is what the seed writes as comments: one row per requirement id, which is one
// thread, and one per superseding line, which is a reply in its id's thread.
const loopCommentRows = loopIDs + loopReplyLines

// The caps the plan is built under, each one a refusal in the Dispatch these records go to.
const (
	// loopMiB is the unit every document cap is a multiple of.
	loopMiB = 1 << 20
	// loopMarginCap is the most text one document's margin may hold over every comment record it
	// ever held (internal/dispatch/docs/growth.go:149-153); a write past it answers 413
	// CAP_EXCEEDED.
	loopMarginCap = loopMiB
	// loopRecordCap is the most text one margin record - a comment and its replies, written whole
	// at every change - may hold (internal/dispatch/docs/growth.go:155-160).
	loopRecordCap = 256 << 10
	// loopMarkdownCap and loopElementCap are what one document's markdown may hold
	// (internal/dispatch/pmdoc/elements.go:292 and :29).
	loopMarkdownCap = loopMiB
	loopElementCap  = 65_536
	// loopBodyCap is the most UTF-16 units one comment body may hold
	// (internal/contracts/generated.go:210); a longer body answers 400 CAP_EXCEEDED. Every body
	// this seed writes is ASCII, so a body's bytes are its UTF-16 units.
	loopBodyCap = 32_000
)

// The plan's budgets under those caps (decision D2 of the issue's spec). An earlier part's threads
// fill at most 95% of the margin cap; the last part of a proposal stops at 70% and carries
// loopSpareQuotes quote rows no thread uses, so the requirement threads a run writes have a line
// to anchor on and still leave the margin inside its cap.
const (
	loopEarlierMargin  = loopMarginCap * 95 / 100
	loopLastMargin     = loopMarginCap * 70 / 100
	loopMarkdownBudget = loopMarkdownCap * 95 / 100
	loopSpareQuotes    = 64
	// loopQuoteBudget is the markdown one part's quote rows and spares may fill: half its budget,
	// so the proposal's own text always has the other half.
	loopQuoteBudget = loopMarkdownBudget / 2
	// loopMaxQuoteRows keeps a part's rows, its spares, its title and its text paragraphs inside
	// loopElementCap even where the rows are short enough that loopQuoteBudget does not bind.
	loopMaxQuoteRows = 16_384
	// loopMaxParts is what loopPartTitle's part number is wide enough to name.
	loopMaxParts = 999
)

// loopBlockBytes is the blank line that follows every markdown block, which is what makes the next
// block its own element.
const loopBlockBytes = 2

// loopTextParagraphBytes is how much of a proposal's own text one paragraph of a part carries.
const loopTextParagraphBytes = 512

// The review pages of the 536 proposals the driver is live on, and the distribution their byte
// sizes are drawn from: the loop's own pages have a median of 9,137 bytes and a mean of 9,954, and
// the plan's pages total exactly 6,057 x 9,954.
const (
	loopReviewPageCount = 6_057
	loopPageMedian      = 9_137
	loopPageMean        = 9_954
	loopReviewPageBytes = loopReviewPageCount * loopPageMean
)

// The PRNG streams one plan draws from. Each is a stream of rand.NewPCG under -seed, with the
// purpose in the high half of the stream id and the proposal in the low half, so what one proposal
// draws never moves when another proposal's counts do.
const (
	loopStreamBodies = iota + 1
	loopStreamStatuses
	loopStreamPages
)

func loopStream(purpose, index uint64) uint64 { return purpose<<32 | index }

// loopStatusNames are the thread statuses the source file counts. A live id's thread is left open;
// every other thread is resolved, under the status its proposal recorded for it.
var loopStatusNames = [...]string{"live", "met", "rejected", "parked", "withdrawn", "other"}

const loopStatusLive = 0

// loopRow is one proposal's row of the source file: its requirement ids, how they ended, the bytes
// its threads hold today, the superseding lines its threads carry as replies, its own text, and
// whether the driver is live on it. 336 of the 891 rows have no requirement id at all: a proposal
// of text and no thread, which is still an issue and still part documents.
type loopRow struct {
	Name         string
	IDs          int
	LiveIDs      int
	Met          int
	Rejected     int
	Parked       int
	Withdrawn    int
	Other        int
	ThreadBytes  int64
	ReplyLines   int
	DocBytes     int64
	LiveInDriver bool
}

// comments is the row's comment rows: a root per requirement id and a reply per superseding line.
func (r loopRow) comments() int { return r.IDs + r.ReplyLines }

// readLoopRows parses the source file and refuses one whose totals are not the seed's own.
func readLoopRows(source, text string) ([]loopRow, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) == 0 || lines[0] != loopHeader {
		head := ""
		if len(lines) > 0 {
			head = lines[0]
		}
		return nil, fmt.Errorf("%s: header is %q, want %q", source, head, loopHeader)
	}
	rows := make([]loopRow, 0, len(lines)-1)
	for n, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 12 {
			return nil, fmt.Errorf("%s line %d: %d columns, want 12", source, n+2, len(f))
		}
		if f[0] == "" {
			return nil, fmt.Errorf("%s line %d: the proposal name is empty", source, n+2)
		}
		var nums [10]int64
		for i, name := range []string{"ids", "live_ids", "met", "rejected", "parked", "withdrawn",
			"other", "thread_bytes", "reply_lines", "doc_bytes"} {
			v, err := strconv.ParseInt(f[i+1], 10, 64)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("%s line %d: %s is %q, want a number of 0 or more",
					source, n+2, name, f[i+1])
			}
			nums[i] = v
		}
		if f[11] != "yes" && f[11] != "no" {
			return nil, fmt.Errorf("%s line %d: live_in_driver is %q, want yes or no", source, n+2, f[11])
		}
		rows = append(rows, loopRow{
			Name: f[0], IDs: int(nums[0]), LiveIDs: int(nums[1]), Met: int(nums[2]),
			Rejected: int(nums[3]), Parked: int(nums[4]), Withdrawn: int(nums[5]),
			Other: int(nums[6]), ThreadBytes: nums[7], ReplyLines: int(nums[8]),
			DocBytes: nums[9], LiveInDriver: f[11] == "yes",
		})
	}
	if err := checkLoopTotals(source, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// checkLoopTotals refuses a source file whose totals are not the verified ones, naming the first
// total that differs. The constants above are the seed's identity, so a file that counts anything
// else is a different seed and is never planned under this one's name.
func checkLoopTotals(source string, rows []loopRow) error {
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
		{"rows", int64(len(rows)), loopRows},
		{"ids", int64(ids), loopIDs},
		{"live_ids", int64(live), loopLiveIDs},
		{"reply_lines", int64(replies), loopReplyLines},
		{"thread_bytes", thread, loopThreadBytes},
		{"doc_bytes", doc, loopDocBytes},
		{"live_in_driver rows", int64(drivers), loopDriverRows},
		{"empty doc_bytes rows", int64(empty), loopEmptyDocs},
	} {
		if c.got != c.want {
			return fmt.Errorf("%s: %s totals %d, want %d", source, c.name, c.got, c.want)
		}
	}
	return nil
}

// loopPlan is what loadgen loop-seed-plan writes: every record the Verified Linux spec loop's seed
// holds, by the counts and byte totals a writer needs, under one seed.
type loopPlan struct {
	Source       string `json:"source"`
	SourceSHA256 string `json:"source_sha256"`
	Seed         uint64 `json:"seed"`
	// Proposals is one issue per source row, in the file's order.
	Proposals     []loopProposal    `json:"proposals"`
	ReviewPages   []loopReviewPage  `json:"review_pages"`
	ReviewIndexes []loopReviewIndex `json:"review_indexes"`
	// Rehearsal is how many of the source's proposals this plan holds when it is a rehearsal —
	// the first n, for a run on a stack too small for the whole seed — and 0 when the plan is the
	// loop's own. A rehearsal is a true subset of the whole plan, cut from it, so what it writes
	// is byte for byte what the whole seed would write for those proposals; every count loop-seed-
	// check judges comes from the plan, so a rehearsal checks against its own totals and says so.
	Rehearsal int        `json:"rehearsal,omitempty"`
	Totals    loopTotals `json:"totals"`
}

// loopProposal is one issue of the seed: one source row, its counts, and the part documents its
// text and its threads are spread over. The issue key is the writer's to learn, so the plan names
// the proposal and its index and never a key.
type loopProposal struct {
	Index        int    `json:"index"`
	Name         string `json:"name"`
	LiveInDriver bool   `json:"live_in_driver"`
	IDs          int    `json:"ids"`
	LiveIDs      int    `json:"live_ids"`
	Met          int    `json:"met"`
	Rejected     int    `json:"rejected"`
	Parked       int    `json:"parked"`
	Withdrawn    int    `json:"withdrawn"`
	Other        int    `json:"other"`
	ReplyLines   int    `json:"reply_lines"`
	ThreadBytes  int64  `json:"thread_bytes"`
	// TextBytes is the proposal's own text the parts carry: its doc_bytes, or the one generated
	// line of a proposal whose doc_bytes is 0 (GeneratedDoc), since a thread anchors on a quote in
	// a document and a proposal with no document of its own has nowhere to anchor.
	TextBytes    int64      `json:"text_bytes"`
	GeneratedDoc bool       `json:"generated_doc"`
	Parts        []loopPart `json:"parts"`
}

// loopPart is one numbered part document of a proposal: the threads that anchor in it, the bytes
// those threads weigh in its margin, and the markdown it holds. Its markdown carries one distinct
// quote row per thread, and the last part of each proposal carries SpareQuotes rows no thread
// uses.
type loopPart struct {
	Index       int `json:"index"`
	FirstThread int `json:"first_thread"`
	Threads     int `json:"threads"`
	Replies     int `json:"replies"`
	// ThreadBytes is what the part's threads hold in its margin: every root and reply body.
	ThreadBytes   int64 `json:"thread_bytes"`
	MarkdownBytes int64 `json:"markdown_bytes"`
	Elements      int   `json:"elements"`
	QuoteRows     int   `json:"quote_rows"`
	SpareQuotes   int   `json:"spare_quotes"`
	// TextBytes is the proposal's own text this part carries, as paragraphs of at most
	// loopTextParagraphBytes.
	TextBytes int64 `json:"text_bytes"`
}

// loopReviewPage is one page of a live proposal's review: a document of its own.
type loopReviewPage struct {
	Proposal int   `json:"proposal"`
	Index    int   `json:"index"`
	Bytes    int64 `json:"bytes"`
}

// loopReviewIndex is one live proposal's review index: the document that lists its review pages.
type loopReviewIndex struct {
	Proposal int   `json:"proposal"`
	Pages    int   `json:"pages"`
	Bytes    int64 `json:"bytes"`
}

// loopTotals is the whole seed in one object.
type loopTotals struct {
	Issues      int   `json:"issues"`
	Parts       int   `json:"parts"`
	Threads     int   `json:"threads"`
	Replies     int   `json:"replies"`
	CommentRows int   `json:"comment_rows"`
	ThreadBytes int64 `json:"thread_bytes"`
	// DocumentBytes is the markdown of every document the seed writes: the parts, the review pages
	// and the review indexes.
	DocumentBytes   int64 `json:"document_bytes"`
	ReviewPages     int   `json:"review_pages"`
	ReviewIndexes   int   `json:"review_indexes"`
	ReviewPageBytes int64 `json:"review_page_bytes"`
	// The distribution the review pages came out at, so a reader sees the one it got rather than
	// the one the planner aimed for.
	ReviewPageMedianBytes int64   `json:"review_page_median_bytes"`
	ReviewPageMeanBytes   float64 `json:"review_page_mean_bytes"`
}

// loopThread is one requirement id's comment thread, as partThreads expands it: the quote row it
// anchors on in its part, how it ended, and the byte size of its root body and of each reply body.
type loopThread struct {
	Proposal int
	Index    int
	Part     int
	Quote    string
	Status   string
	Open     bool
	// Bodies is the root's byte size first, then one per reply. The bodies the writer sends are
	// ASCII, so a body's bytes are its UTF-16 units against loopBodyCap, and their sum is the
	// thread's margin record against loopRecordCap.
	Bodies []int64
}

// Bytes is what the thread holds in its part's margin: its record, root and replies.
func (t loopThread) Bytes() int64 {
	var n int64
	for _, b := range t.Bodies {
		n += b
	}
	return n
}

// loopSourceSum is the hex SHA-256 of the embedded source file: the plan names it so a reader can
// tell which file its counts came from.
func loopSourceSum() string {
	sum := sha256.Sum256([]byte(loopSourceTSV))
	return hex.EncodeToString(sum[:])
}

// loopQuoteRow is the line of a part's markdown that one requirement id's thread anchors on. An
// anchor is {artifact, quote, occurrence} (internal/dispatch/api/anchors.go:28-46), so every
// anchored write needs quote text that is in the target document: each thread gets a line of its
// own, distinct from every other line of every part.
func loopQuoteRow(proposal, thread int) string {
	return fmt.Sprintf("REQ-%04d-%06d is this requirement's line, and the quote its thread anchors on.",
		proposal, thread)
}

// loopSpareRow is one of the quote rows the last part of a proposal carries for no thread, so a
// requirement thread written during a run has a line to anchor on.
func loopSpareRow(proposal, spare int) string {
	return fmt.Sprintf("REQ-%04d-spare-%02d is a free line for a requirement written during the run.",
		proposal, spare)
}

// loopSpareBytes is the markdown the spare rows of one proposal's last part fill.
func loopSpareBytes(proposal int) int {
	n := 0
	for s := range loopSpareQuotes {
		n += len(loopSpareRow(proposal, s)) + loopBlockBytes
	}
	return n
}

// loopPartTitle is a part document's heading. Its part number is fixed width, so a part's markdown
// is the same size wherever the planner moves it.
func loopPartTitle(name string, part int) string {
	return fmt.Sprintf("# %s, part %03d", name, part+1)
}

// loopGeneratedDoc is the one-line document of a proposal whose own text the source file counts as
// 0 bytes (5 of the 891 rows): its threads need a document with a quote in it to anchor on.
func loopGeneratedDoc(proposal int, name string) string {
	return fmt.Sprintf("Proposal %04d (%s) carried no text of its own; this line stands in for it.",
		proposal, name)
}

// loopFiller is the text a part's paragraphs of the proposal's own text are filled with.
const loopFiller = "the verified linux spec loop carried this proposal's text here. "

// loopTextParagraph is one paragraph of a part's share of its proposal's text, of exactly n bytes.
func loopTextParagraph(proposal, part, index, n int) string {
	return loopFillText(fmt.Sprintf("P%04d-%03d-%06d ", proposal, part, index), n)
}

// loopParagraphs is how many paragraphs a part's share of its proposal's text is written as.
func loopParagraphs(text int64) int {
	return int((text + loopTextParagraphBytes - 1) / loopTextParagraphBytes)
}

// loopTextShare is the proposal text part i of k carries: an even split, except that a generated
// one-line document is one line on the first part rather than a few bytes on each.
func loopTextShare(text int64, i, k int, generated bool) int64 {
	if generated {
		if i == 0 {
			return text
		}
		return 0
	}
	return int64(i+1)*text/int64(k) - int64(i)*text/int64(k)
}

// loopReplies is how many superseding lines thread j of a proposal carries as replies: the row's
// reply_lines spread over its ids, which keeps every thread's record far inside loopRecordCap. A
// proposal with no id has no thread to ask it about.
func loopReplies(j, ids, replies int) int {
	return (j+1)*replies/ids - j*replies/ids
}

// loopThreadOffset is where thread j's comment rows start in its proposal's row of body sizes: the
// j roots before it and their replies. A proposal with no id has no thread to ask it about.
func loopThreadOffset(j, ids, replies int) int {
	return j + j*replies/ids
}

// The width of the lognormal the body sizes are drawn from, and the weight range one draw is
// clamped to. The clamp is what holds the two record caps without changing any total: a weight is
// at most 16 times the smallest, so the largest body of a proposal of C comment rows and B thread
// bytes is at most B*16000/(16000+(C-1)*1000), which over this source file peaks at 29,879 bytes
// on DS-calling-context, under loopBodyCap, and the largest thread record at 59,242, far under
// loopRecordCap.
const (
	loopBodySigma  = 0.55
	loopWeightUnit = 4_000
	loopWeightMin  = loopWeightUnit / 4
	loopWeightMax  = loopWeightUnit * 4
)

// loopBodySizes draws the byte size of every comment row of one proposal: a lognormal weight per
// row, scaled by cumulative floors so that the sizes total the row's thread_bytes exactly and each
// one is within a byte of its weight's share. The cumulative floors are integer arithmetic, so the
// sizes are the same on every machine.
func loopBodySizes(seed uint64, index int, r loopRow) []int64 {
	sizes := make([]int64, r.comments())
	rng := rand.New(rand.NewPCG(seed, loopStream(loopStreamBodies, uint64(index))))
	var weights int64
	for i := range sizes {
		w := int64(math.Round(loopWeightUnit * math.Exp(loopBodySigma*rng.NormFloat64())))
		sizes[i] = min(max(w, loopWeightMin), loopWeightMax)
		weights += sizes[i]
	}
	var cum, prev int64
	for i, w := range sizes {
		cum += w
		at := r.ThreadBytes * cum / weights
		sizes[i] = at - prev
		prev = at
	}
	return sizes
}

// loopStatuses draws how each thread of one proposal ended: the row's live ids, which stay open,
// and its met, rejected, parked, withdrawn and other ids, which are resolved, shuffled over the
// proposal's threads.
func loopStatuses(seed uint64, index int, r loopRow) []uint8 {
	out := make([]uint8, 0, r.IDs)
	for status, n := range [...]int{r.LiveIDs, r.Met, r.Rejected, r.Parked, r.Withdrawn, r.Other} {
		for range n {
			out = append(out, uint8(status))
		}
	}
	// loopStatuses gives one status per thread whatever the row says: a row whose statuses add up
	// to fewer than its ids leaves the rest resolved as "other", and one that adds up to more is
	// cut to its ids.
	for len(out) < r.IDs {
		out = append(out, uint8(len(loopStatusNames)-1))
	}
	out = out[:r.IDs]
	rng := rand.New(rand.NewPCG(seed, loopStream(loopStreamStatuses, uint64(index))))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// loopPacked is one part while the planner fills it: the part the plan records, and the markdown
// its quote rows fill, which the plan does not carry.
type loopPacked struct {
	part  loopPart
	quote int
}

// with is this part with thread j in it: one more thread, one more quote row, its replies, its
// bytes and its quote row's markdown.
func (pk loopPacked) with(j, quoteBytes int, row loopRow, totals []int64) loopPacked {
	pk.part.Threads++
	pk.part.QuoteRows++
	pk.part.Replies += loopReplies(j, row.IDs, row.ReplyLines)
	pk.part.ThreadBytes += totals[j]
	pk.quote += quoteBytes
	return pk
}

// fits is true for a part inside every cap the plan is built under: its threads inside the margin
// budget given, and its quote rows, the proposal's spare rows counted, inside the markdown and
// the elements one document holds.
func (pk loopPacked) fits(spare int, margin int64) bool {
	return pk.part.ThreadBytes <= margin &&
		pk.quote+spare <= loopQuoteBudget &&
		pk.part.QuoteRows+loopSpareQuotes <= loopMaxQuoteRows
}

// loopPackParts fills part documents with a proposal's threads in order, closing a part when the
// next thread would take its margin past the earlier-part budget or its quote rows past what one
// document's markdown and elements hold.
func loopPackParts(index int, row loopRow, totals []int64) []loopPacked {
	spare := loopSpareBytes(index)
	packed := []loopPacked{{}}
	for j := range row.IDs {
		q := len(loopQuoteRow(index, j)) + loopBlockBytes
		p := &packed[len(packed)-1]
		next := p.with(j, q, row, totals)
		if p.part.Threads > 0 && !next.fits(spare, loopEarlierMargin) {
			packed = append(packed, loopPacked{part: loopPart{FirstThread: j}})
			p = &packed[len(packed)-1]
			next = p.with(j, q, row, totals)
		}
		*p = next
	}
	return packed
}

// loopSplitLast moves the longest suffix of the last part's threads that fits the last-part budget
// into a part of their own, when the last part holds more than that budget or has no room for its
// spare quote rows. The prefix it leaves is an earlier part, which the wider budget covers.
func loopSplitLast(index int, row loopRow, totals []int64, packed []loopPacked) []loopPacked {
	spare := loopSpareBytes(index)
	at := len(packed) - 1
	last := packed[at]
	if last.fits(spare, loopLastMargin) {
		return packed
	}
	var tail loopPacked
	first := last.part.FirstThread + last.part.Threads
	for j := first - 1; j > last.part.FirstThread; j-- {
		next := tail.with(j, len(loopQuoteRow(index, j))+loopBlockBytes, row, totals)
		if !next.fits(spare, loopLastMargin) {
			break
		}
		tail = next
		first = j
	}
	if tail.part.Threads == 0 {
		// One thread holds more than the last-part budget on its own. No split helps, and
		// planLoopProposal refuses the plan rather than writing a part that would be refused.
		return packed
	}
	tail.part.FirstThread = first
	packed[at].part.Threads -= tail.part.Threads
	packed[at].part.Replies -= tail.part.Replies
	packed[at].part.ThreadBytes -= tail.part.ThreadBytes
	packed[at].part.QuoteRows -= tail.part.QuoteRows
	packed[at].quote -= tail.quote
	return append(packed, tail)
}

// planLoopProposal plans one issue: the sizes of its comment rows, the parts its threads and its
// text are spread over, and each part's markdown. It refuses a part that would break a cap rather
// than planning a write Dispatch answers 413 to.
func planLoopProposal(seed uint64, index int, row loopRow) (loopProposal, error) {
	sizes := loopBodySizes(seed, index, row)
	totals := make([]int64, row.IDs)
	for j := range totals {
		off := loopThreadOffset(j, row.IDs, row.ReplyLines)
		for _, b := range sizes[off : off+1+loopReplies(j, row.IDs, row.ReplyLines)] {
			totals[j] += b
		}
	}
	packed := loopSplitLast(index, row, totals, loopPackParts(index, row, totals))

	text, generated := row.DocBytes, row.DocBytes == 0
	if generated {
		text = int64(len(loopGeneratedDoc(index, row.Name)))
	}
	// A part holds its text beside its quote rows, so the text alone decides how many parts a
	// proposal with little thread and much text needs. room is the text the fullest part has space
	// for, its paragraph breaks counted; the parts the threads filled take the rest.
	over := len(loopPartTitle(row.Name, 0)) + loopBlockBytes + loopSpareBytes(index)
	quote := 0
	for _, pk := range packed {
		quote = max(quote, pk.quote)
	}
	over += quote
	room := int64(loopMarkdownBudget-over) * loopTextParagraphBytes / (loopTextParagraphBytes + loopBlockBytes)
	if !generated && room > 0 {
		for len(packed) < int((text+room-1)/room) {
			packed = append(packed, loopPacked{part: loopPart{FirstThread: row.IDs}})
		}
	}
	if len(packed) > loopMaxParts {
		return loopProposal{}, fmt.Errorf("proposal %d (%s) needs %d parts, more than the %d a part number names",
			index, row.Name, len(packed), loopMaxParts)
	}

	p := loopProposal{
		Index: index, Name: row.Name, LiveInDriver: row.LiveInDriver, IDs: row.IDs,
		LiveIDs: row.LiveIDs, Met: row.Met, Rejected: row.Rejected, Parked: row.Parked,
		Withdrawn: row.Withdrawn, Other: row.Other, ReplyLines: row.ReplyLines,
		ThreadBytes: row.ThreadBytes, TextBytes: text, GeneratedDoc: generated,
		Parts: make([]loopPart, len(packed)),
	}
	for i := range packed {
		part := packed[i].part
		part.Index = i
		part.TextBytes = loopTextShare(text, i, len(packed), generated)
		if i == len(packed)-1 {
			part.SpareQuotes = loopSpareQuotes
		}
		paragraphs := loopParagraphs(part.TextBytes)
		part.Elements = 1 + part.QuoteRows + part.SpareQuotes + paragraphs
		part.MarkdownBytes = int64(len(loopPartTitle(row.Name, i))+loopBlockBytes) +
			int64(packed[i].quote) + part.TextBytes + int64(paragraphs*loopBlockBytes)
		if part.SpareQuotes > 0 {
			part.MarkdownBytes += int64(loopSpareBytes(index))
		}
		margin := int64(loopEarlierMargin)
		if i == len(packed)-1 {
			margin = loopLastMargin
		}
		switch {
		case part.ThreadBytes > margin:
			return loopProposal{}, fmt.Errorf("proposal %d (%s) part %d: its threads hold %d bytes, past the %d of its margin budget",
				index, row.Name, i, part.ThreadBytes, margin)
		case part.MarkdownBytes > loopMarkdownBudget:
			return loopProposal{}, fmt.Errorf("proposal %d (%s) part %d: its markdown is %d bytes, past the %d of its budget",
				index, row.Name, i, part.MarkdownBytes, loopMarkdownBudget)
		case part.Elements > loopElementCap:
			return loopProposal{}, fmt.Errorf("proposal %d (%s) part %d: its markdown is %d elements, past the %d one document holds",
				index, row.Name, i, part.Elements, loopElementCap)
		}
		p.Parts[i] = part
	}
	return p, nil
}

// partThreads expands one part of the plan into the threads that anchor in it. It is a pure
// function of the plan and the two indexes - the same plan gives the same bytes every time - so
// the writer sends exactly the threads, replies and bytes the plan counted. The returned bodies
// share one array per call. It draws the proposal's whole row of body sizes again, since a size
// is its weight's share of the row's thread_bytes and that share is only known from all of them;
// the counts the draw needs are the ones the plan carries.
func (p *loopPlan) partThreads(proposal, part int) []loopThread {
	pr := p.Proposals[proposal]
	pt := pr.Parts[part]
	row := loopPlanRow(&pr)
	sizes := loopBodySizes(p.Seed, proposal, row)
	statuses := loopStatuses(p.Seed, proposal, row)
	out := make([]loopThread, 0, pt.Threads)
	for j := pt.FirstThread; j < pt.FirstThread+pt.Threads; j++ {
		off := loopThreadOffset(j, pr.IDs, pr.ReplyLines)
		end := off + 1 + loopReplies(j, pr.IDs, pr.ReplyLines)
		out = append(out, loopThread{
			Proposal: proposal,
			Index:    j,
			Part:     part,
			Quote:    loopQuoteRow(proposal, j),
			Status:   loopStatusNames[statuses[j]],
			Open:     statuses[j] == loopStatusLive,
			Bodies:   sizes[off:end:end],
		})
	}
	return out
}

// partDocument renders one part's markdown: its title, its share of the proposal's own text, the
// quote row of every thread that anchors in it, and, on the last part, the spare rows. It is the
// document the plan's MarkdownBytes and Elements count.
func (p *loopPlan) partDocument(proposal, part int) string {
	pr := p.Proposals[proposal]
	pt := pr.Parts[part]
	var b strings.Builder
	b.Grow(int(pt.MarkdownBytes))
	b.WriteString(loopPartTitle(pr.Name, part))
	b.WriteString("\n\n")
	if pr.GeneratedDoc && pt.TextBytes > 0 {
		b.WriteString(loopGeneratedDoc(proposal, pr.Name))
		b.WriteString("\n\n")
	} else {
		for i, left := 0, pt.TextBytes; left > 0; i++ {
			n := min(left, loopTextParagraphBytes)
			b.WriteString(loopTextParagraph(proposal, part, i, int(n)))
			b.WriteString("\n\n")
			left -= n
		}
	}
	for j := pt.FirstThread; j < pt.FirstThread+pt.Threads; j++ {
		b.WriteString(loopQuoteRow(proposal, j))
		b.WriteString("\n\n")
	}
	for s := range pt.SpareQuotes {
		b.WriteString(loopSpareRow(proposal, s))
		b.WriteString("\n\n")
	}
	return b.String()
}

// loopShare splits total over the weights in proportion to them, by largest remainder, giving each
// weight at least one. It refuses a total that cannot give every weight its one.
func loopShare(total int, weights []int) ([]int, error) {
	if total < len(weights) {
		return nil, fmt.Errorf("%d to share over %d weights leaves one of them none", total, len(weights))
	}
	var sum int64
	for _, w := range weights {
		sum += int64(w)
	}
	if sum <= 0 {
		return nil, fmt.Errorf("the %d weights to share %d over add up to %d", len(weights), total, sum)
	}
	out := make([]int, len(weights))
	fracs := make([]float64, len(weights))
	got := 0
	for i, w := range weights {
		q := float64(total) * float64(w) / float64(sum)
		fracs[i] = q - math.Floor(q)
		out[i] = max(int(math.Floor(q)), 1)
		got += out[i]
	}
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	// Largest remainder first, by index where two remainders are equal, so the share is the same
	// on every run.
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(fracs[b], fracs[a]) })
	for i := 0; got < total; i = (i + 1) % len(order) {
		out[order[i]]++
		got++
	}
	// A weight rounded up to its one can take the share past the total; the smallest remainders
	// give the surplus back, never below the one each keeps.
	for i := len(order) - 1; got > total; i = (i - 1 + len(order)) % len(order) {
		if j := order[i]; out[j] > 1 {
			out[j]--
			got--
		}
	}
	return out, nil
}

// loopNormalQuantiles is the standard normal's quantile at the midpoint of each of n equal
// probability strata: a sample of n draws whose own median and mean are the distribution's, rather
// than n random draws whose median wanders by a percent.
func loopNormalQuantiles(n int) []float64 {
	z := make([]float64, n)
	for i := range z {
		z[i] = math.Sqrt2 * math.Erfinv(2*(float64(i)+0.5)/float64(n)-1)
	}
	return z
}

// loopPageSigma is the lognormal width at which that sample's mean is ratio times its median,
// found by bisection: the sample's median is exp(0) = 1 and its mean grows with the width, so one
// width gives the loop's own ratio of mean to median.
func loopPageSigma(z []float64, ratio float64) float64 {
	mean := func(sigma float64) float64 {
		var sum float64
		for _, v := range z {
			sum += math.Exp(sigma * v)
		}
		return sum / float64(len(z))
	}
	lo, hi := 0.0, 4.0
	for range 64 {
		mid := (lo + hi) / 2
		if mean(mid) < ratio {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// loopPageSizes draws the byte size of every review page: a lognormal whose width gives the loop's
// own ratio of mean to median, shuffled over the pages and scaled by cumulative floors so the
// sizes total exactly loopReviewPageBytes. The scale leaves the mean at loopPageMean exactly and
// the median within a byte of loopPageMedian, whatever the page count.
func loopPageSizes(seed uint64, pages int) []int64 {
	z := loopNormalQuantiles(pages)
	sigma := loopPageSigma(z, float64(loopPageMean)/float64(loopPageMedian))
	weights := make([]float64, pages)
	var sum float64
	for i, v := range z {
		weights[i] = math.Exp(sigma * v)
		sum += weights[i]
	}
	rng := rand.New(rand.NewPCG(seed, loopStream(loopStreamPages, 0)))
	rng.Shuffle(len(weights), func(i, j int) { weights[i], weights[j] = weights[j], weights[i] })
	sizes := make([]int64, pages)
	var cum float64
	var prev int64
	for i, w := range weights {
		cum += w
		at := int64(float64(loopReviewPageBytes) * cum / sum)
		sizes[i] = at - prev
		prev = at
	}
	// Cumulative floors leave the last page the rounding of every page before it; the total is
	// exact either way.
	sizes[pages-1] += loopReviewPageBytes - prev
	return sizes
}

// loopReviewIndexLine is one line of a live proposal's review index.
func loopReviewIndexLine(proposal, page int) string {
	return fmt.Sprintf("- review page %03d of proposal %04d is in this review.", page, proposal)
}

// loopReviewIndexTitle is the heading of a live proposal's review index: the line
// loopReviewIndexBytes counts and loopIndexText writes, so both read it from here.
func loopReviewIndexTitle(proposal int) string {
	return fmt.Sprintf("# review index of proposal %04d", proposal)
}

// loopReviewIndexBytes is the markdown of one live proposal's review index: its title and one line
// per review page.
func loopReviewIndexBytes(proposal, pages int) int64 {
	n := int64(len(loopReviewIndexTitle(proposal)) + loopBlockBytes)
	for page := range pages {
		n += int64(len(loopReviewIndexLine(proposal, page)) + loopBlockBytes)
	}
	return n
}

// planLoopSeed plans the whole seed under one -seed: one issue per source row with its parts, the
// review pages of the proposals the driver is live on, their review indexes, and the totals.
func planLoopSeed(seed uint64, rows []loopRow) (*loopPlan, error) {
	if err := checkLoopTotals(loopSource, rows); err != nil {
		return nil, err
	}
	p := &loopPlan{Source: loopSource, SourceSHA256: loopSourceSum(), Seed: seed}
	p.Proposals = make([]loopProposal, len(rows))
	var live []int
	var ids []int
	for i, row := range rows {
		pr, err := planLoopProposal(seed, i, row)
		if err != nil {
			return nil, err
		}
		p.Proposals[i] = pr
		if row.LiveInDriver {
			live = append(live, i)
			ids = append(ids, row.IDs)
		}
	}
	pages, err := loopShare(loopReviewPageCount, ids)
	if err != nil {
		return nil, err
	}
	sizes := loopPageSizes(seed, loopReviewPageCount)
	p.ReviewPages = make([]loopReviewPage, 0, loopReviewPageCount)
	p.ReviewIndexes = make([]loopReviewIndex, 0, len(live))
	at := 0
	for k, proposal := range live {
		for page := range pages[k] {
			p.ReviewPages = append(p.ReviewPages, loopReviewPage{
				Proposal: proposal, Index: page, Bytes: sizes[at],
			})
			at++
		}
		p.ReviewIndexes = append(p.ReviewIndexes, loopReviewIndex{
			Proposal: proposal, Pages: pages[k], Bytes: loopReviewIndexBytes(proposal, pages[k]),
		})
	}
	p.Totals = loopPlanTotals(p)
	return p, nil
}

// loopPlanTotals counts the plan: what a reader sees without walking it.
func loopPlanTotals(p *loopPlan) loopTotals {
	t := loopTotals{
		Issues:                len(p.Proposals),
		ReviewPages:           len(p.ReviewPages),
		ReviewIndexes:         len(p.ReviewIndexes),
		ReviewPageMedianBytes: loopMedian(p.ReviewPages),
	}
	for _, pr := range p.Proposals {
		t.Threads += pr.IDs
		t.Replies += pr.ReplyLines
		t.Parts += len(pr.Parts)
		for _, part := range pr.Parts {
			t.ThreadBytes += part.ThreadBytes
			t.DocumentBytes += part.MarkdownBytes
		}
	}
	t.CommentRows = t.Threads + t.Replies
	for _, page := range p.ReviewPages {
		t.ReviewPageBytes += page.Bytes
		t.DocumentBytes += page.Bytes
	}
	for _, idx := range p.ReviewIndexes {
		t.DocumentBytes += idx.Bytes
	}
	if len(p.ReviewPages) > 0 {
		t.ReviewPageMeanBytes = float64(t.ReviewPageBytes) / float64(len(p.ReviewPages))
	}
	return t
}

// loopMedian is the middle review page's byte size, the lower of the two where the count is even.
func loopMedian(pages []loopReviewPage) int64 {
	if len(pages) == 0 {
		return 0
	}
	sizes := make([]int64, len(pages))
	for i, page := range pages {
		sizes[i] = page.Bytes
	}
	slices.Sort(sizes)
	return sizes[(len(sizes)-1)/2]
}

// loopRehearsal cuts the whole plan down to its first n proposals, with the review pages and
// indexes of those proposals alone and the totals recomputed. It is a cut rather than a plan of
// its own so that a rehearsal writes byte for byte what the whole seed writes for those
// proposals, and so that no second planner can drift from the one the loop's seed comes from.
// n at or above the plan's proposal count gives the whole plan back, unmarked.
func loopRehearsal(p *loopPlan, n int) *loopPlan {
	if n <= 0 || n >= len(p.Proposals) {
		return p
	}
	cut := &loopPlan{
		Source:       p.Source,
		SourceSHA256: p.SourceSHA256,
		Seed:         p.Seed,
		Proposals:    p.Proposals[:n],
		Rehearsal:    n,
	}
	for _, page := range p.ReviewPages {
		if page.Proposal < n {
			cut.ReviewPages = append(cut.ReviewPages, page)
		}
	}
	for _, idx := range p.ReviewIndexes {
		if idx.Proposal < n {
			cut.ReviewIndexes = append(cut.ReviewIndexes, idx)
		}
	}
	cut.Totals = loopPlanTotals(cut)
	return cut
}

// runLoopSeedPlan plans the Verified Linux spec loop's seed and writes it, without a stack: every
// record a Dispatch deployment must hold to carry the loop, from the embedded source file and the
// seed alone.
func runLoopSeedPlan(args []string) error {
	fs := flag.NewFlagSet("loop-seed-plan", flag.ExitOnError)
	seed := fs.Uint64("seed", 1, "the seed of every size the plan draws")
	out := fs.String("out", "", "the plan's path (default $LOADTEST_ROOT/loopseed.plan.json)")
	rehearsal := fs.Int("rehearsal", 0, "plan the first n proposals alone, for a rehearsal on a stack too small for the whole seed (0: the whole seed)")
	_ = fs.Parse(args)

	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *rehearsal < 0 {
		return fmt.Errorf("-rehearsal %d is not a count of proposals", *rehearsal)
	}
	if *out == "" {
		*out = filepath.Join(loadtestRoot(), "loopseed.plan.json")
	}
	rows, err := readLoopRows(loopSource, loopSourceTSV)
	if err != nil {
		return err
	}
	plan, err := planLoopSeed(*seed, rows)
	if err != nil {
		return err
	}
	plan = loopRehearsal(plan, *rehearsal)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	lg := log.New(os.Stderr, "loadgen loop-seed-plan: ", 0)
	writeJSONFile(*out, plan, 0o644, lg)
	rehearsalNote := ""
	if plan.Rehearsal > 0 {
		rehearsalNote = fmt.Sprintf(" (a rehearsal of %d of %d proposals)", plan.Rehearsal, len(rows))
	}
	lg.Printf("%d issues, %d parts, %d comment rows of %d bytes, %d review pages of %d bytes%s: %s",
		plan.Totals.Issues, plan.Totals.Parts, plan.Totals.CommentRows, plan.Totals.ThreadBytes,
		plan.Totals.ReviewPages, plan.Totals.ReviewPageBytes, rehearsalNote, *out)
	return nil
}

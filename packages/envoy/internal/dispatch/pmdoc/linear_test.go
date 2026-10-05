package pmdoc

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Parse and Render finish in time linear in the text on the shapes that cost quadratic time
// before LEGION-465. Each shape is timed at an eighth of its target size and then at the full
// size - not half: growsLinearly's check is took > allowance*growth*before, so growth (the size
// step) must exceed allowance for a quadratic regression's growth² to ever clear it at all (at a
// 2x step, slack 3 allows a 6x budget that a quadratic's 4x growth clears without tripping it -
// TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack, below, proves both directions) -
// through growsLinearly, which keeps the fastest of several same-size samples before comparing
// growth, so -race's per-access overhead and box load - whose bursts can land on only one of the
// two measurements rather than inflating both alike - cannot flake this (LEGION-569: a loaded box
// pushed `a_`'s fixed 30 s bound past the worst case a single sample each way was sized for). The
// smaller sample this step makes costs less to measure, not more, so it does not grow the test's
// wall time. A write of a mebibyte of these is refused for the elements it makes
// (MaxDocumentElements), so the parse here is a read-back's, which counts none and reads all of
// it. Render's target is smaller (512 KiB, not a mebibyte): rendering is linear even in `<a`
// (measured directly, not just through this ratio: 256 KiB-4 MiB all grew within 10% of the size
// itself), but -race's instrumentation on its many small writes costs real wall time a loaded box
// multiplies further - 4 MiB of `<a` took 2m2s under -race at a load average of 55-79 - so this
// loop keeps its slowest shape well inside a package's default test timeout even there.
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		full := (1 << 20) / len(shape)
		small := full / 8
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("parsing %q", shape), []int{small * len(shape), full * len(shape)}, linearSlack, func(size int) time.Duration {
				repeats := size / len(shape)
				started := time.Now()
				if _, err := ParseRendering(strings.Repeat(shape, repeats)); err != nil {
					t.Fatalf("parse %d bytes of %q: %v", size, shape, err)
				}
				return time.Since(started)
			})
		})
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		full := (512 << 10) / len(shape)
		small := full / 8
		doc := func(repeats int) *Node {
			return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, repeats)}}}}}
		}
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("rendering %q", shape), []int{small * len(shape), full * len(shape)}, linearSlack, func(size int) time.Duration {
				repeats := size / len(shape)
				started := time.Now()
				if _, err := Render(doc(repeats)); err != nil {
					t.Fatalf("render %d bytes of %q: %v", size, shape, err)
				}
				return time.Since(started)
			})
		})
	}
}

// growsLinearly's check is took > allowance*growth*before: true-linear time grows about as fast
// as growth itself, quadratic time about growth² - so growth must exceed allowance for growth² to
// ever clear allowance*growth. This proves both directions with a synthetic timing function at
// the exact growth (8, not the input bytes - growthVerdict only ever divides sizes) and slack
// (linearSlack) TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText uses: growth=8 exceeds
// slack=3, so the arithmetic actually discriminates. The quadratic direction goes through
// growthVerdict directly, not growsLinearly, since a real *testing.T that fails would fail this
// test too - growsLinearly is a six-line wrapper over growthVerdict's verdict, so this is the same
// check growsLinearly makes.
func TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack(t *testing.T) {
	const small, full = 1, 8 // the growth step itself (full/small); growthVerdict never reads these as bytes
	linear := func(size int) time.Duration { return time.Duration(size) * time.Second }
	quadratic := func(size int) time.Duration { return time.Duration(size*size) * time.Second }
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, quadratic); failedStep != 0 {
		t.Errorf("growthVerdict did not fail a synthetic quadratic timing function at growth=8, slack=%d (failedStep=%d), want step 0 to fail", linearSlack, failedStep)
	}
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, linear); failedStep != -1 {
		t.Errorf("growthVerdict failed a synthetic linear timing function at growth=8, slack=%d (step %d), want it to pass", linearSlack, failedStep)
	}
	// growsLinearly itself, through a real *testing.T, confirms the wrapper agrees with
	// growthVerdict on the passing direction (the failing direction cannot run through a real
	// t.Fatalf without failing this test, which the growthVerdict assertion above already proves).
	growsLinearly(t, "synthetic linear", []int{small, full}, linearSlack, linear)
}

// linearSlack is how many times faster than growth itself a linear operation's time may grow
// before growsLinearly calls it quadratic, at the 8x growth step
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText uses (full is eight times small, not
// two): true-linear time grows about 8x, comfortably inside the 3x8=24 threshold; the quadratic
// cost LEGION-465 fixed would grow about 64x (8²), clearing that threshold with 2.6x to spare.
const linearSlack = 3

// The shapes a sweep found after the ones above, each quadratic before LEGION-465 finished: a
// text of one delimiter character, whose every character was escaped by a scan of its whole run;
// many `[^` before one `]`, whose every label was case-folded to look it up among the footnote
// labels; a text of many line feeds, whose every line joined the rest of the text to ask whether
// only whitespace followed; and a run of `~` in a paragraph, which goldmark's strikethrough parser
// scanned again from every `~` in it. Before the fix 128 KiB of each text took 18-115 s to render
// and 32 KiB of `[^a` 7.7 s; each bound is at least ten times the time measured after it.
func TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds(t *testing.T) {
	paragraph := func(text string) *Node {
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}
	}
	definition := func(label string) *Node {
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": label}, Children: []*Node{paragraph("x")}}
	}
	labels := strings.Repeat("[^a", (4<<20)/3) + "]"
	for _, test := range []struct {
		name string
		doc  *Node
	}{
		{"a run of *", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("*", 4<<20))}}},
		{"a run of _", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("_", 4<<20))}}},
		{"a run of ~", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("~", 4<<20))}}},
		{"[^a before one ] with no footnote", &Node{Type: "doc", Children: []*Node{paragraph(labels)}}},
		{"[^a before one ] with a short label defined", &Node{Type: "doc", Children: []*Node{paragraph(labels), definition("a")}}},
		{"[^a before one ] with a long label defined", &Node{Type: "doc", Children: []*Node{paragraph(labels), definition(strings.Repeat("b", 1000))}}},
		{"line feeds", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("a\n", 2<<20))}}},
	} {
		started := time.Now()
		if _, err := Render(test.doc); err != nil {
			t.Fatalf("render 4 MiB of %s: %v", test.name, err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("render of 4 MiB of %s took %s, want under 30 s", test.name, elapsed)
		}
	}
	started := time.Now()
	if _, err := Parse("a" + strings.Repeat("~", 1<<20)); err != nil {
		t.Fatalf("parse a run of 1 MiB of ~: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("parse of a run of 1 MiB of ~ took %s, want under 30 s", elapsed)
	}
}

// A table whose every row holds an escaped pipe in a code span parses in time linear in its size,
// up to the 1 MiB a document may be. goldmark's table transformer checked every code span's text
// against every escaped pipe in the document: on one machine at a load of about 300, 16 KiB of such
// rows parsed in 35 ms and 256 KiB in 6.8 s, and 1 MiB took 2 min 9 s. unescapeTablePipes,
// which replaced it, parsed 16 KiB in 19 ms, 256 KiB in 0.6-0.8 s and 1 MiB in 2.0-2.4 s there. No
// wall-clock bound holds on every runner, so the parse is timed at growing sizes and its growth
// bounded (growsLinearly). A caller's write of a table that size passes the element limit and is
// refused, so it is read as a rendering is, whose parse no element limit stops.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	growsLinearly(t, "parsing a table of `\\|` cells", []int{16 << 10, 256 << 10, 1 << 20}, growthAllowance, func(size int) time.Duration {
		markdown := header + strings.Repeat(row, (size-len(header))/len(row))
		started := time.Now()
		if _, err := ParseRendering(markdown); err != nil {
			t.Fatalf("parse %d bytes of `\\|` rows: %v", size, err)
		}
		return time.Since(started)
	})
}

// growthAllowance is how many times faster than its input a parse's time may grow from one size
// to the next. Linear time grows at most as fast as the input, less while a fixed cost counts, and
// quadratic time as the input's square. Four times the input's growth lets a runner's load
// quadruple between two parses and still refuses a quadratic one at any step over four.
const growthAllowance = 4

// growthVerdict runs growsLinearly's measurement and retry algorithm without a *testing.T to
// report through: which step (0-based, among sizes[1:]) first grew past its bound, or -1 if none
// did, and one line of context for every step up to and including a failure - the fail-style line
// at that step, a log-style line at every other. growsLinearly below turns these into
// t.Fatalf/t.Logf; TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack calls this
// directly, so the bound it proves is provable without a real *testing.T failing, a real parse or
// render, or a hang past some other timeout standing in for the check actually tripping.
func growthVerdict(sizes []int, allowance int, timed func(size int) time.Duration) (failedStep int, lines []string) {
	first := func() time.Duration { return min(timed(sizes[0]), timed(sizes[0]), timed(sizes[0])) }
	before := first()
	failedStep = -1
	for i := 1; i < len(sizes); i++ {
		smaller, size := sizes[i-1], sizes[i]
		growth := size / smaller
		took := timed(size)
		if i == 1 {
			before = min(before, first())
		}
		exceeded := took > time.Duration(allowance*growth)*before
		if exceeded {
			took = min(took, timed(size))
			if i > 1 {
				before = max(before, timed(smaller))
			}
			exceeded = took > time.Duration(allowance*growth)*before
		}
		ratio := float64(took) / float64(before)
		if exceeded {
			failedStep = i - 1
			lines = append(lines, fmt.Sprintf("took %s at %s, %.0f times the %s it took at %s: the input grew %d times, and a parse linear in it may grow %d times at most",
				took.Round(time.Millisecond), sizeText(size), ratio, before.Round(time.Millisecond), sizeText(smaller), growth, allowance*growth))
			return failedStep, lines
		}
		lines = append(lines, fmt.Sprintf("%s at %s, %s at %s, %.1f times for an input %d times larger (at most %d)", before.Round(time.Millisecond), sizeText(smaller), took.Round(time.Millisecond), sizeText(size), ratio, growth, allowance*growth))
		before = took
	}
	return failedStep, lines
}

// growsLinearly times what at each of sizes in turn and fails t at the first size whose time grew
// more than allowance times faster than the size did. The first size is timed three times before
// the second and three times after it, and the fastest of the six kept, so neither a first run's
// warm-up nor a load spike shorter than the second size's run reads as growth. A step over the
// bound times the larger size again and keeps the faster time, and after the first step also
// times the smaller size again and keeps the slower, so load that rose between two runs does not
// read as growth either.
func growsLinearly(t *testing.T, what string, sizes []int, allowance int, timed func(size int) time.Duration) {
	t.Helper()
	failedStep, lines := growthVerdict(sizes, allowance, timed)
	for step, line := range lines {
		if step == failedStep {
			t.Fatalf("%s %s", what, line)
			return
		}
		t.Logf("%s: %s", what, line)
	}
}

func sizeText(size int) string {
	if size >= 1<<20 && size%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", size>>20)
	}
	return fmt.Sprintf("%d KiB", size>>10)
}

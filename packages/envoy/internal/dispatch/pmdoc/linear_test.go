package pmdoc

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Parse and Render finish in time linear in the text on the shapes that cost quadratic time
// before LEGION-465. Each shape is timed at growthStep's fraction of its target size and then at
// the full size (parseFullTarget/renderFullTarget: linear_sizes.go, and a smaller pair under
// -race, linear_sizes_race.go, so a -count=N run stays well inside go test's own default
// per-package timeout even there), through growsLinearly (below).
//
// What each shape's closure measures is this process's own CPU time (processCPUTime), not wall
// time. A loaded box's scheduler can inflate wall time on either side of a comparison without
// either side doing more work - that is LEGION-569/571's whole flake history across three rounds
// of widening the growth step and the slack without removing the root cause. CPU time is immune
// to another process's contention for the same cores; it only grows with work this process (and
// its GC) actually does.
//
// growsLinearly also samples every size the same number of times and keeps the fastest
// (growthVerdict's sampleCount), so neither a first run's warm-up nor whichever side a stray spike
// lands on biases the comparison. Round 3's asymmetric sampling - up to six draws at the smaller
// size against one or two at the larger - systematically biased the baseline low under bursty
// load, inflating the apparent ratio independent of any real growth; TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack's
// own history records the fix.
//
// Every measured call also runs under its own deadline (perCallDeadline, growsLinearly): a single
// call that itself blocks far longer than this bound allows fails fast and names its own shape,
// rather than the package's own ten-minute default eventually firing with no shape attached
// (perCallDeadline's own comment has the measured margin; leakedCallGuard's has what happens to
// such a call afterward).
//
// A write of a mebibyte of these is refused for the elements it makes (MaxDocumentElements), so
// the parse here is a read-back's, which counts none and reads all of it. Render's target is
// smaller than parse's (linear_sizes.go): this loop's own ratio check is `<a`'s live linearity
// measurement, at whatever parseFullTarget/renderFullTarget currently are, not a one-time aside
// recorded in a comment to go stale - but -race's own per-access instrumentation costs real CPU
// time a reintroduced O(n²) bug would cost far more of; the margin this loop checks is that
// difference, not a hang (which perCallDeadline catches on its own, independent of the ratio).
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		full := parseFullTarget / len(shape)
		small := full / growthStep
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("parsing %q", shape), []int{small * len(shape), full * len(shape)}, linearSlack, func(size int) time.Duration {
				repeats := size / len(shape)
				before := processCPUTime()
				if _, err := ParseRendering(strings.Repeat(shape, repeats)); err != nil {
					t.Fatalf("parse %d bytes of %q: %v", size, shape, err)
				}
				return processCPUTime() - before
			})
		})
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		full := renderFullTarget / len(shape)
		small := full / growthStep
		doc := func(repeats int) *Node {
			return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, repeats)}}}}}
		}
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("rendering %q", shape), []int{small * len(shape), full * len(shape)}, linearSlack, func(size int) time.Duration {
				repeats := size / len(shape)
				before := processCPUTime()
				if _, err := Render(doc(repeats)); err != nil {
					t.Fatalf("render %d bytes of %q: %v", size, shape, err)
				}
				return processCPUTime() - before
			})
		})
	}
}

// growthStep is the size ratio TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's two
// measurements differ by (full is growthStep times small). growsLinearly's check is
// took > allowance*growth*before, so growth must exceed allowance for growth² (a quadratic
// regression's growth) to ever clear allowance*growth (linear's own expected growth) at all: at
// growthStep=8 and linearSlack=3 the bound is 3*8=24, true-linear clears it with 3x headroom, and
// a quadratic regression's 8²=64 clears it by 2.67x. A 2x step, which rounds 1-2 of LEGION-571
// used, could never discriminate at any slack - 2²=4 always clears 2*slack once slack>2 - which
// TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack's own synthetic check, below,
// catches as a negative control.
const growthStep = 8

// growthVerdict's check is took > allowance*growth*before: true-linear time grows about as fast
// as growth itself, quadratic time about growth² - so growth must exceed allowance for growth² to
// ever clear allowance*growth. This proves both directions with a synthetic timing function at
// the exact growth (growthStep) and slack (linearSlack)
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText uses: growthStep exceeds linearSlack,
// so the arithmetic actually discriminates. The quadratic direction goes through growthVerdict
// directly, not growsLinearly, since a real *testing.T that fails would fail this test too -
// growsLinearly is a thin wrapper over growthVerdict's verdict, so this is the same check
// growsLinearly makes, symmetric sampling and the per-call deadline included (growthVerdict's
// fastest and growsLinearly's perCallDeadline are exercised identically whichever caller drives
// them; TestRunWithDeadlineReportsAHangPromptly, below growsLinearly, proves the deadline itself).
func TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack(t *testing.T) {
	const small, full = 1, growthStep // the growth step itself; growthVerdict never reads these as bytes
	linear := func(size int) time.Duration { return time.Duration(size) * time.Second }
	quadratic := func(size int) time.Duration { return time.Duration(size*size) * time.Second }
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, quadratic); failedStep != 0 {
		t.Errorf("growthVerdict did not fail a synthetic quadratic timing function at growth=%d, slack=%d (failedStep=%d), want step 0 to fail", growthStep, linearSlack, failedStep)
	}
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, linear); failedStep != -1 {
		t.Errorf("growthVerdict failed a synthetic linear timing function at growth=%d, slack=%d (step %d), want it to pass", growthStep, linearSlack, failedStep)
	}
	// Negative control: round 1-2's 2x step, the same slack, the same quadratic function. This
	// must still pass growthVerdict (wrongly) - it is the historical defect
	// growthStep's doc comment names, kept here so a future reader can run it and see the defect
	// reproduce rather than take the claim on faith.
	if failedStep, _ := growthVerdict([]int{1, 2}, linearSlack, quadratic); failedStep != -1 {
		t.Errorf("growthVerdict unexpectedly failed the 2x-step negative control (failedStep=%d) - if this starts failing, the control no longer demonstrates round 1-2's defect and this test's comment needs updating, not alarm", failedStep)
	}
	// growsLinearly itself, through a real *testing.T, confirms the wrapper agrees with
	// growthVerdict on the passing direction (the failing direction cannot run through a real
	// t.Fatalf without failing this test, which the growthVerdict assertion above already proves).
	growsLinearly(t, "synthetic linear", []int{small, full}, linearSlack, linear)
}

// linearSlack is how many times faster than growth itself a linear operation's time may grow
// before growsLinearly calls it quadratic, at the growthStep growth
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText uses: true-linear time grows about
// growthStep times, comfortably inside the linearSlack*growthStep threshold; the quadratic cost
// LEGION-465 fixed would grow about growthStep² times, clearing that threshold with room to
// spare (growthStep's own doc comment has the exact numbers).
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
// bounded (growsLinearly), by this process's own CPU time rather than wall time, for the same
// reason TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText measures CPU time. A caller's
// write of a table that size passes the element limit and is refused, so it is read as a
// rendering is, whose parse no element limit stops.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	growsLinearly(t, "parsing a table of `\\|` cells", []int{16 << 10, 256 << 10, 1 << 20}, growthAllowance, func(size int) time.Duration {
		markdown := header + strings.Repeat(row, (size-len(header))/len(row))
		before := processCPUTime()
		if _, err := ParseRendering(markdown); err != nil {
			t.Fatalf("parse %d bytes of `\\|` rows: %v", size, err)
		}
		return processCPUTime() - before
	})
}

// growthAllowance is how many times faster than its input a parse's time may grow from one size
// to the next. Linear time grows at most as fast as the input, less while a fixed cost counts, and
// quadratic time as the input's square. Four times the input's growth lets a runner's load
// quadruple between two parses and still refuses a quadratic one at any step over four.
const growthAllowance = 4

// sampleCount is how many times growthVerdict measures each size before keeping the fastest - the
// same count at every size, so neither side of a comparison draws from more samples than the
// other (an asymmetric draw biases a minimum low on the side with more samples, independent of
// any real growth). Measuring CPU time rather than wall time (processCPUTime, below) removes most
// of what ambient box load would otherwise still leave to sample away.
const sampleCount = 3

// processCPUTime is this process's total CPU time, user and system, since it started
// (syscall.Getrusage(RUSAGE_SELF)), not wall time - immune to another process's contention for
// the same cores, which is LEGION-569/571's whole flake history (the file-level comment above has
// the fuller account). No test in this package calls t.Parallel (there is no t.Parallel()
// anywhere under internal/dispatch/pmdoc), so ordinarily at most one measured call runs at a time,
// and this process's CPU delta across one call is that call's own cost plus whatever GC work its
// allocations trigger meanwhile. leakedCallGuard (below) is what keeps that true even once a call
// has missed its own deadline and gone on running in the background.
func processCPUTime() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		panic(fmt.Sprintf("getrusage(RUSAGE_SELF): %v", err))
	}
	return time.Duration(usage.Utime.Nano()+usage.Stime.Nano()) * time.Nanosecond
}

// leaking tracks the what (growsLinearly's shape-and-size label) of every runWithDeadline call
// whose own goroutine is still running past its deadline - Go cannot cancel a goroutine
// mid-computation, so this is the only way a later call can tell that measuring right now would
// share processCPUTime's process-wide counter with CPU a still-running earlier call keeps
// spending. runWithDeadline adds to it the instant a deadline misses and removes from it only once
// that call's own goroutine finally returns; leakedCallGuard reads it.
var leaking struct {
	mu   sync.Mutex
	what []string
}

// stillLeaking reports whether any call is still registered as running past its own deadline -
// leakedCallGuard's predicate without its message, for a caller that only needs to poll (see
// waitForLeakToClear).
func stillLeaking() bool {
	leaking.mu.Lock()
	defer leaking.mu.Unlock()
	return len(leaking.what) > 0
}

// leakedCallGuard reports what, if anything, is still running past its own deadline - the message
// timedWithDeadline fails a new call with before ever starting it, naming the call still leaking
// CPU into this process's RUSAGE_SELF rather than inventing a ratio out of a measurement that
// shared it. A pure function (no *testing.T) so a unit test can assert it directly: see
// TestLeakedCallBlocksTheNextMeasurement.
func leakedCallGuard(what string) string {
	leaking.mu.Lock()
	defer leaking.mu.Unlock()
	if len(leaking.what) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: refusing to measure while %s is still running past its own deadline (processCPUTime is this whole process's, so measuring now would attribute its ongoing CPU to this call instead)", what, strings.Join(leaking.what, ", "))
}

// runWithDeadline runs op, labelled what, in its own goroutine and reports whether it returned
// within deadline. Go cannot cancel a goroutine mid-computation, so an op that does not return in
// time is left running - registered under what in leaking until it eventually returns and
// deregisters itself, so leakedCallGuard can refuse a later call that would otherwise share its
// ongoing CPU - rather than stopped; the point of the deadline is only to stop waiting for it and
// report that promptly, not to free its resources.
func runWithDeadline(what string, deadline time.Duration, op func() time.Duration) (took time.Duration, onTime bool) {
	result := make(chan time.Duration, 1)
	go func() { result <- op() }()
	select {
	case took := <-result:
		return took, true
	case <-time.After(deadline):
		leaking.mu.Lock()
		leaking.what = append(leaking.what, what)
		leaking.mu.Unlock()
		go func() {
			<-result
			leaking.mu.Lock()
			for i, w := range leaking.what {
				if w == what {
					leaking.what = append(leaking.what[:i], leaking.what[i+1:]...)
					break
				}
			}
			leaking.mu.Unlock()
		}()
		return 0, false
	}
}

// timedWithDeadline refuses to run op, naming whatever is still leaking (leakedCallGuard), then
// runs it under runWithDeadline and fails t, naming what and deadline, if it did not return in
// time.
func timedWithDeadline(t *testing.T, what string, deadline time.Duration, op func() time.Duration) time.Duration {
	t.Helper()
	if msg := leakedCallGuard(what); msg != "" {
		t.Fatal(msg)
	}
	took, onTime := runWithDeadline(what, deadline, op)
	if !onTime {
		t.Fatalf("%s did not return within %s", what, deadline)
	}
	return took
}

// TestRunWithDeadlineReportsAHangPromptly proves runWithDeadline's own guarantee directly: a
// synthetic op that blocks far longer than its deadline is reported late (onTime=false) promptly -
// close to the deadline itself, not close to how long op actually blocks - the property
// growsLinearly's perCallDeadline relies on to fail fast and name a shape instead of waiting out
// go test's own default timeout with none attached. The op here returns quickly after its own
// deadline misses (short enough this test itself stays fast) so it does not leave leaking
// populated for whichever test runs after this one in the same binary.
func TestRunWithDeadlineReportsAHangPromptly(t *testing.T) {
	const deadline = 20 * time.Millisecond
	const hang = 200 * time.Millisecond
	started := time.Now()
	_, onTime := runWithDeadline("synthetic prompt hang", deadline, func() time.Duration {
		time.Sleep(hang) // longer than deadline, short enough this test and its cleanup stay fast
		return 0
	})
	if elapsed := time.Since(started); elapsed > 10*deadline {
		t.Errorf("runWithDeadline took %s to report a hang against a %s deadline, want close to immediate", elapsed, deadline)
	}
	if onTime {
		t.Error("runWithDeadline reported onTime=true for an op that slept far longer than its deadline")
	}
	waitForLeakToClear(t, hang)
}

// TestLeakedCallBlocksTheNextMeasurement proves leakedCallGuard directly: once a synthetic op has
// missed its own deadline and is still running, a second, otherwise-healthy call through
// timedWithDeadline refuses to start - naming the first call - rather than silently sharing its
// still-accruing CPU (processCPUTime is this whole process's, not per-goroutine; Simplify and Deep
// both traced this on round 4's version, which had no guard at all).
func TestLeakedCallBlocksTheNextMeasurement(t *testing.T) {
	const deadline = 20 * time.Millisecond
	const hang = 200 * time.Millisecond
	_, onTime := runWithDeadline("synthetic leaked hang", deadline, func() time.Duration {
		time.Sleep(hang)
		return 0
	})
	if onTime {
		t.Fatal("runWithDeadline reported onTime=true for an op that slept far longer than its deadline")
	}
	if msg := leakedCallGuard("a second call"); msg == "" {
		t.Error("leakedCallGuard let a second call measure while the first was still registered as leaked, want it refused")
	} else {
		t.Logf("leakedCallGuard correctly refused: %s", msg)
	}
	waitForLeakToClear(t, hang)
}

// waitForLeakToClear blocks until leaking is empty or fails t: the goroutine runWithDeadline
// spawns to deregister a leaked call needs a moment, after the leaked op itself returns, to
// acquire leaking's lock and remove its entry - this gives it up to 10x hang (itself far more than
// that handoff needs) before concluding something is wrong, so a later test in this same binary
// never starts measuring while this test's own synthetic leak is still registered.
func waitForLeakToClear(t *testing.T, hang time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * hang)
	for stillLeaking() {
		if time.Now().After(deadline) {
			t.Fatalf("leaking still held an entry %s after its op should have returned and deregistered", 10*hang)
		}
		time.Sleep(time.Millisecond)
	}
}

// growthVerdict runs growsLinearly's measurement algorithm without a *testing.T to report
// through: which step (0-based, among sizes[1:]) first grew past its bound, or -1 if none did,
// and one line of context for every step up to and including a failure - the fail-style line at
// that step, a log-style line at every other. Every size, the first included, is measured
// sampleCount times and the fastest kept, so the comparison at every step draws from the same
// number of samples on both sides. growsLinearly below turns these into t.Fatalf/t.Logf;
// TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack calls this directly, so the
// bound it proves is provable without a real *testing.T failing, a real parse or render, or a
// hang past some other timeout standing in for the check actually tripping.
func growthVerdict(sizes []int, allowance int, timed func(size int) time.Duration) (failedStep int, lines []string) {
	fastest := func(size int) time.Duration {
		best := timed(size)
		for i := 1; i < sampleCount; i++ {
			if sample := timed(size); sample < best {
				best = sample
			}
		}
		return best
	}
	before := fastest(sizes[0])
	failedStep = -1
	for i := 1; i < len(sizes); i++ {
		smaller, size := sizes[i-1], sizes[i]
		growth := size / smaller
		took := fastest(size)
		ratio := float64(took) / float64(before)
		if took > time.Duration(allowance*growth)*before {
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

// perCallDeadline bounds every one of growthVerdict's measured calls: well under go test's own
// ten-minute default per-package timeout, so a single call that itself blocks far longer than
// this file's bound allows fails fast and names its own shape and size (the file-level comment
// above has why that matters). It is a wall-clock bound guarding a CPU-time budget, so box-load
// scheduling starvation, not only a regression, could in principle trip it on a healthy call -
// measured margin against that: the slowest observed healthy call at this file's current sizes,
// under real ambient box load, was "<a" rendering at parseFullTarget/renderFullTarget's race-mode
// size in about 0.8s, and "a_b*" parsing at the non-race size in about 0.9s - both over 60x under
// this deadline. A margin under 10x would call for raising it; this one does not.
const perCallDeadline = 60 * time.Second

// growsLinearly times what at each of sizes in turn and fails t at the first size whose time grew
// more than allowance times faster than the size did (growthVerdict, which this wraps). Every
// measured call runs under perCallDeadline (timedWithDeadline), so a call that itself hangs fails
// this shape by name instead of the package's own default timeout firing with none.
func growsLinearly(t *testing.T, what string, sizes []int, allowance int, timed func(size int) time.Duration) {
	t.Helper()
	bounded := func(size int) time.Duration {
		return timedWithDeadline(t, fmt.Sprintf("%s at %s", what, sizeText(size)), perCallDeadline, func() time.Duration {
			return timed(size)
		})
	}
	failedStep, lines := growthVerdict(sizes, allowance, bounded)
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

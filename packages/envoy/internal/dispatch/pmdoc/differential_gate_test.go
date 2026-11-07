package pmdoc

// The pmdoc differential's judgment, run in the head's binary alone (gen/differential.sh, which
// copies differential_test.go into the base revision and this file nowhere): it may name what this
// revision holds, where the reading beside it may not.
//
// Among what TestDifferentialCompare counts: a document the head reads as the engine but its
// writer refuses, because of the rendering it would store, is reported ("refused by the writer")
// and gates where the base stores it ("newly refused by the writer"); any other document the base
// stores and the head does not is reported ("newly refused"). A document the engine itself threw
// on is reported ("engine cannot read"), the throw being the engine's own; one whose reading Go's
// schema refuses gates ("engine reading Go's schema refuses"), since no other gate can judge a
// document Go cannot hold.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// diffSide is what one revision does with one document.
type diffSide struct {
	dump diffDump
	// stores: the write is taken and the tree renders, with nothing panicking.
	stores bool
	// correct: it stores the document as the engine reads it, in markdown both readers read back
	// as that tree.
	correct bool
	// renders: the engine's tree renders into markdown both readers read back as that tree.
	renders bool
	// engineOnly: it stores a tree Go reads back as written and the engine reads otherwise.
	engineOnly bool
	// Why correct fails: the tree differs from the engine's reading, or the engine reads the
	// rendering back otherwise (Go's read-back is in dump).
	ReadsAsEngine, EngineReadsBack bool
}

// diffRenderings is the engine's reading of one revision's two renderings of a document.
type diffRenderings struct {
	ID           string       `json:"id"`
	Render       *diffReading `json:"render"`
	EngineRender *diffReading `json:"engine_render"`
}

func judge(dump diffDump, source *Node, reading diffRenderings) diffSide {
	side := diffSide{dump: dump}
	var tree *Node
	if dump.Tree != nil {
		tree, _ = FromJSON(dump.Tree)
	}
	side.stores = dump.Write == "" && tree != nil && dump.Render != nil && len(dump.Panics) == 0
	engineBack := diffTree(reading.Render)
	side.EngineReadsBack = tree != nil && engineBack != nil && StripAnchorMarks(tree).Equal(engineBack)
	side.ReadsAsEngine = tree != nil && source != nil && tree.Equal(source)
	side.correct = side.stores && side.ReadsAsEngine && dump.ReadsBack && side.EngineReadsBack
	side.engineOnly = side.stores && dump.ReadsBack && !side.EngineReadsBack
	engineRenderBack := diffTree(reading.EngineRender)
	side.renders = source != nil && dump.EngineRender != nil && dump.EngineReadsBack && engineRenderBack != nil && source.Equal(engineRenderBack) && len(dump.Panics) == 0
	return side
}

// differs reports whether two renderings differ, one missing counting as a difference.
func differs(base, head *string) bool {
	return base == nil || head == nil || *base != *head
}

// refusesItsRendering reports whether this revision refuses the write of markdown for the
// rendering it would store: one that reads back as another document (ErrMisreadRendering) or that
// no reader of this package reads at all (ErrRenderingRefused). The comparison runs in the head's
// binary, so it asks the error rather than reading the dump's sentence, which each revision
// writes in its own words. Parts of a write run outside recoverPanic, and the dump step ran this
// same call under diffStep, which records a panic rather than dying of it; so this one recovers
// too and answers the panic, which the comparison counts as a gate of its own instead of losing
// the whole summary to it.
func refusesItsRendering(markdown string) (refuses bool, panicked string) {
	defer func() {
		if value := recover(); value != nil {
			refuses, panicked = false, fmt.Sprint(value)
		}
	}()
	SetBlockIDGenerator(counterBlockIDs())
	_, err := gateWrite(markdown)
	return errors.Is(err, ErrMisreadRendering) || errors.Is(err, ErrRenderingRefused), ""
}

// gateWrite is the write refusesItsRendering asks about, a variable so that a test can plant a
// panic in it; nothing but a test replaces it.
var gateWrite = func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }

func TestTheGateAnswersAPanicInItsReParse(t *testing.T) {
	previous := gateWrite
	gateWrite = func(string) (*Node, error) { panic("planted: a write that panics outside recoverPanic") }
	defer func() { gateWrite = previous }()
	refuses, panicked := refusesItsRendering("- a\n")
	if refuses {
		t.Error("a write that panicked was taken for a refusal of its rendering")
	}
	if !strings.Contains(panicked, "planted") {
		t.Errorf("the panic is answered as %q, which does not name it", panicked)
	}
}

func TestDifferentialCompare(t *testing.T) {
	if os.Getenv("PMDOC_DIFF_BASE") == "" {
		t.Skip("PMDOC_DIFF_BASE unset; gen/differential.sh runs this")
	}
	engine := openDiffLines(t, "PMDOC_DIFF_ENGINE")
	corpus := openDiffLines(t, "PMDOC_DIFF_CORPUS")
	dumps := [2]*diffLines{openDiffLines(t, "PMDOC_DIFF_BASE"), openDiffLines(t, "PMDOC_DIFF_HEAD")}
	readings := [2]*diffLines{openDiffLines(t, "PMDOC_DIFF_BASE_ENGINE"), openDiffLines(t, "PMDOC_DIFF_HEAD_ENGINE")}
	report, err := os.Create(os.Getenv("PMDOC_DIFF_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	defer report.Close()
	writer := bufio.NewWriter(report)
	defer writer.Flush()

	counts := map[string]int{}
	flag := func(class string, sides [2]diffSide) {
		counts[class]++
		line, _ := json.Marshal(map[string]any{
			"class": class, "id": sides[1].dump.ID,
			"base": sides[0].dump, "head": sides[1].dump,
			"base_reads_as_engine": sides[0].ReadsAsEngine, "base_engine_reads_back": sides[0].EngineReadsBack,
			"head_reads_as_engine": sides[1].ReadsAsEngine, "head_engine_reads_back": sides[1].EngineReadsBack,
		})
		writer.Write(append(line, '\n'))
	}
	documents := 0
	for {
		var read struct {
			ID string       `json:"id"`
			MD *diffReading `json:"md"`
		}
		if !engine.next(t, &read) {
			break
		}
		documents++
		var written struct {
			ID string `json:"id"`
			MD string `json:"md"`
		}
		if !corpus.next(t, &written) || written.ID != read.ID {
			t.Fatalf("the corpus does not follow the engine's readings at %q", read.ID)
		}
		// A reading the engine threw on is the engine's own refusal, and only reported; one Go's
		// schema refuses leaves the document out of every gate below, so it gates itself.
		source, refused := diffRead(read.MD)
		switch {
		case refused:
			counts["engine reading Go's schema refuses"]++
		case source == nil:
			counts["engine cannot read"]++
		}
		var sides [2]diffSide
		for index := range sides {
			var dump diffDump
			var reading diffRenderings
			if !dumps[index].next(t, &dump) || !readings[index].next(t, &reading) || dump.ID != read.ID || reading.ID != read.ID {
				t.Fatalf("the runs do not follow the corpus at %q", read.ID)
			}
			sides[index] = judge(dump, source, reading)
		}
		base, head := sides[0], sides[1]
		for index, name := range []string{"base", "head"} {
			side := sides[index]
			if side.correct {
				counts[name+" stores correctly"]++
			}
			if side.renders {
				counts[name+" renders the engine's tree correctly"]++
			}
			if side.engineOnly {
				counts[name+" engine-only read-back"]++
			}
			if len(side.dump.Panics) > 0 {
				counts[name+" panics"]++
			}
		}
		switch {
		case base.correct && !head.correct:
			flag("regression", sides)
		case !base.stores && head.stores && !head.correct:
			flag("misread", sides)
		case !base.correct && head.correct:
			flag("fixed", sides)
		}
		// A fixed point base kept of a document it read otherwise than the editor is a misread kept
		// stable; it is counted apart, and only one of a document base read as the editor gates.
		switch lost := base.dump.FixedPoint && !head.dump.FixedPoint; {
		case lost && base.ReadsAsEngine:
			flag("lost fixed point", sides)
		case lost:
			flag("lost fixed point of a misread", sides)
		}
		if base.correct && differs(base.dump.Render, head.dump.Render) {
			flag("churn", sides)
		}
		if base.renders && !head.renders {
			flag("engine-tree regression", sides)
		}
		if base.renders && differs(base.dump.EngineRender, head.dump.EngineRender) {
			flag("engine-tree churn", sides)
		}
		if !base.renders && head.renders {
			flag("engine-tree fixed", sides)
		}
		if len(head.dump.Panics) > 0 {
			flag("head panic", sides)
		}
		if !base.engineOnly && head.engineOnly {
			flag("new engine-only read-back", sides)
		}
		// The writer refuses a document the head reads as the engine for the rendering it would
		// store - one that reads back as another document, or that no reader reads at all - which
		// is read from the error this binary raises for it (refusesItsRendering) rather than from
		// the sentence the dump recorded; one the base stores is newly refused by the writer, and
		// gates. A write that panics in that re-read answers no refusal and gates on its own, as
		// the dump's own panic does.
		writerRefuses := false
		if head.dump.Parse == "" && head.dump.Write != "" && head.ReadsAsEngine {
			refuses, panicked := refusesItsRendering(written.MD)
			writerRefuses = refuses
			if panicked != "" {
				flag("head panic in the gate's re-read", sides)
			}
		}
		if writerRefuses {
			flag("refused by the writer", sides)
		}
		switch {
		case writerRefuses && base.stores:
			flag("newly refused by the writer", sides)
		case base.stores && !head.stores:
			flag("newly refused", sides)
		}
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "documents: %d (the engine cannot read %d)\n", documents, counts["engine cannot read"])
	// gates is each count that is summarized and that fails the test, in the order both read them.
	gates := []struct{ label, key string }{
		{"regressions (base stores correctly, head does not)", "regression"},
		{"misreads (base refuses, head stores what reads back otherwise)", "misread"},
		{"churn (base stores correctly, head's bytes differ)", "churn"},
		{"engine-tree regressions (base renders the engine's tree correctly, head does not)", "engine-tree regression"},
		{"engine-tree churn (base renders the engine's tree correctly, head's bytes differ)", "engine-tree churn"},
		{"head panics (recovered or not)", "head panic"},
		{"head panics in the gate's re-read of a refused write", "head panic in the gate's re-read"},
		{"lost fixed points (base reads the markdown as the engine and writes it back as read, head does not)", "lost fixed point"},
		{"newly refused by the writer (base stores, head refuses the rendering it would store)", "newly refused by the writer"},
		{"engine readings Go's schema refuses (FromJSON cannot hold what the engine read)", "engine reading Go's schema refuses"},
	}
	for _, line := range gates {
		fmt.Fprintf(&summary, "GATE %-86s %d\n", line.label, counts[line.key])
	}
	for _, line := range []struct{ label, key string }{
		{"refused by the writer (head reads it as the engine, and refuses the rendering it would store)", "refused by the writer"},
		{"newly refused (base stores, head does not, other than newly refused by the writer)", "newly refused"},
	} {
		fmt.Fprintf(&summary, "REPORTED %-82s %d\n", line.label, counts[line.key])
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&summary, "  %s: %d\n", key, counts[key])
	}
	fmt.Print(summary.String())
	for _, gate := range gates {
		if counts[gate.key] > 0 {
			t.Errorf("%d %s; see %s", counts[gate.key], gate.key, os.Getenv("PMDOC_DIFF_REPORT"))
		}
	}
}

package pmdoc

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// A heading weighs four elements, its block and its text, so a document of 16,384 headings weighs
// the limit exactly and is read, and one heading more is refused, naming the line that passes it.
func TestAWriteAtTheElementLimitIsReadAndOneElementMoreIsRefused(t *testing.T) {
	headings := MaxDocumentElements / 4
	if _, err := ParseForWrite(strings.Repeat("# a\n", headings), nil); err != nil {
		t.Fatalf("%d headings, %d elements: %v, want them read", headings, MaxDocumentElements, err)
	}
	_, err := ParseForWrite(strings.Repeat("# a\n", headings+1), nil)
	if !errors.Is(err, ErrTooManyElements) || errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "more than 65536 elements, passing that at line 16385") {
		t.Fatalf("%d headings: %v, want ErrTooManyElements naming the limit and line %d", headings+1, err, headings+1)
	}
}

// A line ending in a hard break weighs four elements, its text and the break, written as two
// spaces or a backslash, and the paragraph's last line, which ends in no break, weighs one with the
// paragraph's three: 16,384 lines weigh the limit exactly and are read, and one line more is
// refused, naming the line that passes it.
func TestHardBreaksWeighAsTheirTextAndABlock(t *testing.T) {
	lines := MaxDocumentElements / 4
	for _, line := range []string{"a  \n", "a\\\n"} {
		if _, err := ParseForWrite(strings.Repeat(line, lines), nil); err != nil {
			t.Fatalf("%d lines of %q, %d elements: %v, want them read", lines, line, MaxDocumentElements, err)
		}
		_, err := ParseForWrite(strings.Repeat(line, lines+1), nil)
		if !errors.Is(err, ErrTooManyElements) || !strings.Contains(err.Error(), fmt.Sprintf("passing that at line %d", lines)) {
			t.Fatalf("%d lines of %q: %v, want ErrTooManyElements naming line %d", lines+1, line, err, lines)
		}
	}
}

// A caller's mebibyte of any shape that makes elements by the byte is refused before goldmark has
// built the rest of it: every reader of caller markdown refuses it, a list included, which closes at
// the guard (elementListGuard), and the refusal allocates a fraction of what reading it whole took
// (2.6 GB for `)_`, which held a gigabyte at once, LEGION-481). Hard breaks allocate the most,
// about 130 MiB; a list that stayed open past the guard made an item a line, and took 240 MiB.
func TestMarkdownPastTheElementLimitIsRefusedBeforeItIsBuilt(t *testing.T) {
	const mebibyte = 1 << 20
	fill := func(unit string) string { return strings.Repeat(unit, mebibyte/len(unit)+1)[:mebibyte] }
	table := "| a | b |\n| - | - |\n" + strings.Repeat("| x | y |\n", mebibyte/10)
	run := strings.Repeat("*", 2*262_140)
	for _, shape := range []struct {
		name, markdown string
		want           error
	}{
		{")_", fill(")_"), ErrTooManyElements},
		{"a_", fill("a_"), ErrTooManyElements},
		{"a_b*", fill("a_b*"), ErrTooManyElements},
		{"[a", fill("[a"), ErrTooManyElements},
		{"a line feed per character", fill("a\n"), ErrTooManyElements},
		{"list items", fill("- a\n"), ErrTooManyElements},
		{"empty list items", fill("-\n"), ErrTooManyElements},
		{"hard breaks", fill("a  \n"), ErrTooManyElements},
		{"headings", fill("# a\n"), ErrTooManyElements},
		{"a two-column table", table, ErrTooManyElements},
		// Escaped syntax, which weighed four elements before an escape weighed one, while the
		// parse that read it whole allocated 330 to 720 MiB (escapeWeight).
		{`\~a`, fill(`\~a`), ErrTooManyElements},
		{`)\_`, fill(`)\_`), ErrTooManyElements},
		{`\)\_`, fill(`\)\_`), ErrTooManyElements},
		{`\[a`, fill(`\[a`), ErrTooManyElements},
		{"&#95;a", fill("&#95;a"), ErrTooManyElements},
		// Nested past the inline bound too, which refuses it first (nestingRefusal).
		{"262,140 nested marks", run + "x" + run, ErrSchema},
	} {
		for _, reader := range []struct {
			name  string
			parse func(string) error
		}{
			{"document", func(markdown string) error { _, err := ParseForWrite(markdown, nil); return err }},
			{"fragment", func(markdown string) error {
				_, err := ParseFragment(markdown, false, NewWriteBudget())
				return err
			}},
		} {
			t.Run(shape.name+"/"+reader.name, func(t *testing.T) {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				err := reader.parse(shape.markdown)
				runtime.ReadMemStats(&after)
				if !errors.Is(err, shape.want) {
					t.Fatalf("a mebibyte of %s: %v, want %v", shape.name, err, shape.want)
				}
				if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 160<<20 {
					t.Errorf("refusing a mebibyte of %s allocated %d MiB, want at most 160 MiB", shape.name, allocated>>20)
				}
			})
		}
	}
	if _, err := ParseInline(fill(")_"), NewWriteBudget()); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("inline markdown of a mebibyte of )_: %v, want ErrTooManyElements", err)
	}
}

// One write's markdown spends one budget, however many operations carry it: two fragments and an
// inline replacement that each fit the limit alone are refused together once they pass it.
func TestAWritesFragmentsShareOneElementBudget(t *testing.T) {
	half := strings.Repeat("# a\n", MaxDocumentElements/4/2)
	budget := NewWriteBudget()
	if _, err := ParseFragment(half, false, budget); err != nil {
		t.Fatalf("the first half: %v", err)
	}
	if _, err := ParseInline(strings.Repeat("a ", 100), budget); err != nil {
		t.Fatalf("an inline replacement beside it: %v", err)
	}
	if _, err := ParseFragment(half, false, budget); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("the second half: %v, want ErrTooManyElements", err)
	}
}

// A read-back parses a rendering the server wrote of a document it already holds, so it counts no
// elements: a mebibyte of `a_`, which a caller's write may not make, reads back whole.
func TestAReadBackCountsNoElements(t *testing.T) {
	if _, err := ParseRendering(strings.Repeat("a_", 1<<19)); err != nil {
		t.Fatalf("read back a mebibyte of a_: %v", err)
	}
}

// MeasureDocument counts a document as an upload's parse does: for each shape, the most units an
// upload takes measure within the element limit, and one unit more measures past it. Front matter
// is counted by neither, so it and 16,384 headings fit, as the headings alone do.
func TestMeasureDocumentCountsWhatAnUploadCounts(t *testing.T) {
	repeated := func(unit string) func(int) string {
		return func(units int) string { return strings.Repeat(unit, units) }
	}
	for _, shape := range []struct {
		name  string
		build func(int) string
	}{
		{"headings", repeated("# a\n")},
		{"front matter and headings", func(units int) string { return "---\ntitle: x\n---\n\n" + strings.Repeat("# a\n", units) }},
		{")_", repeated(")_")},
		{"list items", repeated("- a\n")},
		{"hard breaks", repeated("a  \n")},
		{"table rows", func(units int) string { return "| a | b |\n| - | - |\n" + strings.Repeat("| x | y |\n", units) }},
		{"links", repeated("[a](b) ")},
		{"autolinks", repeated("<https://a.example> ")},
	} {
		t.Run(shape.name, func(t *testing.T) {
			uploads := func(units int) bool {
				_, err := ParseForWrite(shape.build(units), nil)
				return !errors.Is(err, ErrTooManyElements)
			}
			low, high := 1, 1
			for uploads(high) {
				low, high = high, high*2
			}
			for high-low > 1 {
				if middle := (low + high) / 2; uploads(middle) {
					low = middle
				} else {
					high = middle
				}
			}
			if size := MeasureDocument(shape.build(low)); size.TooHeavy() {
				t.Errorf("%d units, which an upload takes, measure %+v, past the limit", low, size)
			}
			if size := MeasureDocument(shape.build(high)); !size.TooHeavy() {
				t.Errorf("%d units, which an upload refuses, measure %+v, within the limit", high, size)
			}
		})
	}
	if size := MeasureDocument("---\ntitle: x\n---\n\n" + strings.Repeat("# a\n", MaxDocumentElements/4)); !size.Counted || size.Elements != MaxDocumentElements {
		t.Fatalf("front matter and %d headings measure %+v, want the %d elements of the headings alone", MaxDocumentElements/4, size, MaxDocumentElements)
	}
}

// A refusal at the guard names the line goldmark stopped reading at as that, never as where the
// weight passed the limit, which a tree the guard left unbuilt cannot tell. A mebibyte of lines of
// one character each passes the limit on its 65,534th line, a paragraph weighing three and each line
// one, and the guard stops reading its blocks on line 131,073; 70,000 such lines it stops reading in
// their inline syntax, on line 61,074, ahead of the 65,534th, where their weight passes the limit.
func TestARefusalAtTheGuardSaysWhereReadingStopped(t *testing.T) {
	for lines, stopped := range map[int]int{1 << 19: 131_073, 70_000: 61_074} {
		_, err := ParseForWrite(strings.Repeat("a\n", lines), nil)
		if want := fmt.Sprintf("more than 65536 elements, too many to read past line %d;", stopped); !errors.Is(err, ErrTooManyElements) || !strings.Contains(err.Error(), want) {
			t.Errorf("%d one-character lines: %v, want a refusal saying %q", lines, err, want)
		}
	}
}

// A bare-row insert is parsed under a header the server writes at the table's width, which is not
// the caller's markdown: it charges the write nothing, so a row costs its own weight - the row, its
// cells and their text - and a refusal names a line of the caller's markdown, never the header's.
func TestATableRowInsertIsChargedItsRowsAlone(t *testing.T) {
	width := 100
	table := strings.Repeat("| h ", width) + "|\n" + strings.Repeat("| - ", width) + "|\n| A10 " + strings.Repeat("| x ", width-1) + "|\n"
	doc, err := Parse(table)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "A10", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	budget := NewWriteBudget()
	if _, inserted, err := InsertTableRows(doc, anchor, "| y |\n", true, budget); err != nil || !inserted {
		t.Fatalf("insert one row: inserted=%v, %v", inserted, err)
	}
	if want := 3 + width*4 + 1; budget.elements.made != want {
		t.Fatalf("one row of %d cells charged %d elements, want %d: the row, its cells and its one text", width, budget.elements.made, want)
	}
	budget = NewWriteBudget()
	budget.elements.made = MaxDocumentElements - 10
	_, _, err = InsertTableRows(doc, anchor, "\n| y |\n", true, budget)
	if !errors.Is(err, ErrTooManyElements) || !strings.Contains(err.Error(), "passing that at line 2;") {
		t.Fatalf("a row past what the batch has left: %v, want a refusal at line 2, the row's", err)
	}
}

// An escape weighs an element as the syntax it spells does: the document and its paragraph weigh
// four, so 32,766 units of `\)\_`, two escapes each, weigh the limit exactly and are read, one unit
// more is refused, and MeasureDocument counts the same. A backslash inside a code span is no
// escape, and the text a plain-text write sends is weighed before it is made into markdown: an
// element for each character its markdown escapes or reads as syntax, two for each line feed.
func TestAnEscapeWeighsAnElement(t *testing.T) {
	units := (MaxDocumentElements - 4) / 2
	if _, err := ParseForWrite(strings.Repeat(`\)\_`, units), nil); err != nil {
		t.Fatalf("%d units of \\)\\_: %v, want them read", units, err)
	}
	if _, err := ParseForWrite(strings.Repeat(`\)\_`, units+1), nil); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("%d units of \\)\\_: %v, want ErrTooManyElements", units+1, err)
	}
	if size := MeasureDocument(strings.Repeat(`\)\_`, units)); !size.Counted || size.Elements != MaxDocumentElements {
		t.Fatalf("%d units of \\)\\_ measure %+v, want %d elements", units, size, MaxDocumentElements)
	}
	if size := MeasureDocument("`" + strings.Repeat(`\_`, 2*MaxDocumentElements) + "`"); !size.Counted || size.Elements > 10 {
		t.Fatalf("a code span of backslashes measures %+v, want a few elements", size)
	}
	if err := NewWriteBudget().RefusePlainText(strings.Repeat(")_", MaxDocumentElements)); err != nil {
		t.Fatalf("plain text of %d underscores: %v, want it taken", MaxDocumentElements, err)
	}
	if err := NewWriteBudget().RefusePlainText(strings.Repeat(")_", MaxDocumentElements/2), strings.Repeat("~a", MaxDocumentElements/2+1)); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("plain text of %d syntax characters: %v, want ErrTooManyElements", MaxDocumentElements+1, err)
	}
	if err := NewWriteBudget().RefusePlainText(strings.Repeat("a\n", MaxDocumentElements/2+1)); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("plain text of %d line feeds: %v, want ErrTooManyElements", MaxDocumentElements/2+1, err)
	}
}

// A piece of inline HTML weighs two elements: the paragraph weighs three, and each `<b>a</b>` five,
// its two tags and its text, so 13,106 spans weigh 65,533 and are read, 13,107 weigh 65,538 and are
// refused, and MeasureDocument counts the same.
func TestInlineHTMLWeighsTwoElements(t *testing.T) {
	spans := (MaxDocumentElements - 3) / 5
	if _, err := ParseForWrite(strings.Repeat("<b>a</b>", spans), nil); err != nil {
		t.Fatalf("%d spans: %v, want them read", spans, err)
	}
	if _, err := ParseForWrite(strings.Repeat("<b>a</b>", spans+1), nil); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("%d spans: %v, want ErrTooManyElements", spans+1, err)
	}
	if size := MeasureDocument(strings.Repeat("<b>a</b>", spans)); !size.Counted || size.Elements != 3+5*spans {
		t.Fatalf("%d spans measure %+v, want %d elements", spans, size, 3+5*spans)
	}
}

// Markdown an upload's parse refuses for its tables' padding measures past the limit rather than as
// the paragraph the refused table is left as: a 101-column header over 200 one-cell rows, which
// would pad 20,000 cells, measured 407 elements, where it weighs 82,111 written whole.
func TestATableThePaddingRefusesMeasuresPastTheLimit(t *testing.T) {
	table := strings.Repeat("| h ", 101) + "|\n" + strings.Repeat("| - ", 101) + "|\n" + strings.Repeat("| x |\n", 200)
	if _, err := ParseForWrite(table, nil); !errors.Is(err, ErrTablePadding) {
		t.Fatalf("the table's upload: %v, want ErrTablePadding", err)
	}
	if size := MeasureDocument(table); !size.TooHeavy() {
		t.Fatalf("the table measures %+v, want it past the limit", size)
	}
}

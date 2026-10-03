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
	headings := maxWriteElements / 4
	if _, err := ParseForWrite(strings.Repeat("# a\n", headings), nil); err != nil {
		t.Fatalf("%d headings, %d elements: %v, want them read", headings, maxWriteElements, err)
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
	lines := maxWriteElements / 4
	for _, line := range []string{"a  \n", "a\\\n"} {
		if _, err := ParseForWrite(strings.Repeat(line, lines), nil); err != nil {
			t.Fatalf("%d lines of %q, %d elements: %v, want them read", lines, line, maxWriteElements, err)
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
	for _, shape := range []struct{ name, markdown string }{
		{")_", fill(")_")},
		{"a_", fill("a_")},
		{"a_b*", fill("a_b*")},
		{"[a", fill("[a")},
		{"a line feed per character", fill("a\n")},
		{"list items", fill("- a\n")},
		{"empty list items", fill("-\n")},
		{"hard breaks", fill("a  \n")},
		{"headings", fill("# a\n")},
		{"a two-column table", table},
		{"262,140 nested marks", run + "x" + run},
	} {
		for _, reader := range []struct {
			name  string
			parse func(string) error
		}{
			{"document", func(markdown string) error { _, err := ParseForWrite(markdown, nil); return err }},
			{"fragment", func(markdown string) error {
				_, err := ParseFragment(markdown, false, NewTablePaddingBudget())
				return err
			}},
		} {
			t.Run(shape.name+"/"+reader.name, func(t *testing.T) {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				err := reader.parse(shape.markdown)
				runtime.ReadMemStats(&after)
				if !errors.Is(err, ErrTooManyElements) {
					t.Fatalf("a mebibyte of %s: %v, want ErrTooManyElements", shape.name, err)
				}
				if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 160<<20 {
					t.Errorf("refusing a mebibyte of %s allocated %d MiB, want at most 160 MiB", shape.name, allocated>>20)
				}
			})
		}
	}
	if _, err := ParseInline(fill(")_")); !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("inline markdown of a mebibyte of )_: %v, want ErrTooManyElements", err)
	}
}

// One write's markdown spends one budget, however many operations carry it: two fragments and an
// inline replacement that each fit the limit alone are refused together once they pass it.
func TestAWritesFragmentsShareOneElementBudget(t *testing.T) {
	half := strings.Repeat("# a\n", maxWriteElements/4/2)
	budget := NewTablePaddingBudget()
	if _, err := ParseFragment(half, false, budget); err != nil {
		t.Fatalf("the first half: %v", err)
	}
	if _, err := ParseInlineOn(strings.Repeat("a ", 100), budget); err != nil {
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

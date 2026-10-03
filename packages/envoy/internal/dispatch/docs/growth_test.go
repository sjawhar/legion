package docs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write may not leave a document's markdown past either of an upload's limits and bigger than it
// was by that measure: longer than 1 MiB and longer than before, or making more than 65,536
// elements and more than before. Text that makes next to no elements - prose, or a code block's
// lines - is held by its bytes; text that makes many by its elements. A document already past a
// limit can still be trimmed or rewritten as long, and front matter, which an upload's parse does
// not count, is not counted here either.
func TestAWriteMayNotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	prose := func(bytes int) string { return strings.Repeat("word ", bytes/5) + "\n" }
	code := func(bytes int) string {
		return "```\n" + strings.Repeat("a line of code, forty bytes long; more\n", bytes/40) + "```\n"
	}
	headings := func(count int) string { return strings.Repeat("# a\n", count) }
	underscores := func(units int) string { return strings.Repeat(")_", units) }
	frontMatter := "---\ntitle: a spec\n---\n\n"
	tooLong := func(before, after string) string {
		return fmt.Sprintf("a markdown document is at most 1 MiB (1048576 bytes), and this change would make the document's markdown %d bytes (it was %d)", len(after), len(before))
	}
	for _, test := range []struct {
		name, before, after string
		refusal             string
	}{
		{"900 KB of prose onto 900 KB", prose(900_000), prose(900_000) + "\n" + prose(900_000), ""},
		{"900 KB of code onto 900 KB", code(900_000), code(1_800_000), ""},
		{"a heading onto 16,384, which weigh the limit", headings(16_384), headings(16_384) + "\nMore.\n",
			"make 65540 elements, past the 65536 one document may hold (it made 65536)"},
		{"front matter and 16,384 headings, new", "", frontMatter + headings(16_384), "-"},
		{"front matter and 16,385 headings, new", "", frontMatter + headings(16_385),
			"make 65540 elements, past the 65536 one document may hold (it made 0)"},
		{"more )_ than the guard reads, onto )_ it read whole", "Before.\n", underscores(150_000),
			"make more than 65536 elements, past the 65536 one document may hold (it made 4)"},
		{"more )_ onto more than the guard reads", underscores(150_000), underscores(160_000),
			"make more than 65536 elements, past the 65536 one document may hold (it made more than 65536)"},
		{"less )_ than a document past the guard held", underscores(160_000), underscores(150_000), "-"},
		{"trimming prose past 1 MiB", prose(1_800_000), prose(1_700_000), "-"},
		{"rewriting prose past 1 MiB as long", prose(1_800_000), "w" + prose(1_800_000)[1:], "-"},
		{"trimming headings past the limit", headings(17_001), headings(17_000), "-"},
		{"a heading's text rewritten as heavy past the limit", headings(17_001), headings(17_000) + "# b\n", "-"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := test.refusal
			if want == "" {
				want = tooLong(test.before, test.after)
			}
			err := refuseGrowth(nil, test.before, test.after)
			if want == "-" {
				if err != nil {
					t.Fatalf("refused: %v, want it taken", err)
				}
				return
			}
			if !errors.Is(err, pmdoc.ErrTooManyElements) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "shorten the change, or split the document") {
				t.Fatalf("got %v, want ErrTooManyElements saying %q and what to do", err, want)
			}
		})
	}
}

package pmdoc

import (
	"strings"
	"testing"
	"time"
)

// Parse and Render finish in time linear in the text on the shapes that cost quadratic time
// before LEGION-465. Each bound is at least ten times the time measured after the fix on a
// development machine (parse: `a_` 0.3 s, `a_b*` 2 s, `a~b_` 1 s per MiB; render of 4 MiB: `[a`
// 1.4 s, `a_b&` 0.4 s, `<a` 1.3 s, `[^a` 1.0 s) and well under the time before it (parse of 1 MiB
// of `a_`: about 4 minutes; of `a_b*`: 12 minutes; render of 4 MiB of `[a`: minutes).
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		markdown := strings.Repeat(shape, (1<<20)/len(shape))
		started := time.Now()
		if _, err := Parse(markdown); err != nil {
			t.Fatalf("parse 1 MiB of %q: %v", shape, err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("parse of 1 MiB of %q took %s, want under 30 s", shape, elapsed)
		}
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, (4<<20)/len(shape))}}}}}
		started := time.Now()
		if _, err := Render(doc); err != nil {
			t.Fatalf("render 4 MiB of %q: %v", shape, err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("render of 4 MiB of %q took %s, want under 30 s", shape, elapsed)
		}
	}
}

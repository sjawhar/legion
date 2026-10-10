// Package searchtest builds text for tests of Dispatch's search vectors. It imports no Dispatch
// package, so the store package's own tests use it as well as the packages that test through the
// store.
package searchtest

import (
	"fmt"
	"strings"
)

// DistinctWords is n words no two alike, `w000001 w000002 …`, a space after each and paragraph
// after every hundredth instead. Each word is a lexeme of its own, which a search vector holds as
// its seven bytes and five more (an alignment byte, a position count and a position), so 100,000
// of them, 800 KB of text inside every write's 1 MiB bound, make 1.2 MB: past the 1,048,575 bytes
// of lexemes and positions Postgres holds in one tsvector.
func DistinctWords(n int, paragraph string) string {
	var text strings.Builder
	for word := 1; word <= n; word++ {
		fmt.Fprintf(&text, "w%06d", word)
		if word%100 == 0 {
			text.WriteString(paragraph)
		} else {
			text.WriteString(" ")
		}
	}
	return text.String()
}

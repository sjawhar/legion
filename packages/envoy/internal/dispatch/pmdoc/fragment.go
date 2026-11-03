package pmdoc

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// notSpaceOrParen is the browser's `[^()\s]`, the characters a link destination the cut left
// open may hold: Go's own `\s` is the five ASCII blanks alone, so the Unicode separators
// JavaScript's `\s` holds stand beside it.
const notSpaceOrParen = `[^()\s\v\p{Zs}\x{2028}\x{2029}\x{feff}]`

// The three shapes a cut leaves a link in, each the browser's own pattern: a link whose
// destination the cut left open, a link whose text it left open, and a link whose opening the cut
// at the fragment's start took. Each keeps the link's words and drops its syntax.
var (
	cutOpenLinkDestination = regexp.MustCompile(`\[([^\[\]]*)\]\(` + notSpaceOrParen + `*$`)
	cutOpenLinkText        = regexp.MustCompile(`\[([^\[\]]*)$`)
	cutClosedLinkTail      = regexp.MustCompile(`^([^\[\]]*)\]\(` + notSpaceOrParen + `*\)`)
)

// A fragment cut out of a Markdown body: one the server cut before anybody read it, which today
// is a search hit's ts_headline snippet (the 24 words around the match). A cut can land inside a
// mark, and the half it leaves renders as its characters. The balancer below drops exactly the
// delimiters that CommonMark would have paired had the cut not happened and nothing else: a run
// the parser would read as literal text anyway (`Use ** for bold`, `2 * 3`, `snake_case`) is the
// author's and stays, and nothing inside a code span is ever a delimiter.
//
// This is the port of the browser's own balancer, packages/dispatch/web/src/features/refs/
// markdown-fragment.ts, which the palette has run on a snippet since before the server read one:
// the two must agree character for character, so a snippet the server reads for display shows
// what the browser showed. Its markdownHead is not ported, since nothing on the server cuts a
// body's head.

// SearchSnippetWords is how many words a search hit's snippet holds when ts_headline cut the body
// to its window (headlineOptions in internal/dispatch/api/search.go: MaxWords=24). A body that
// fits the window comes back whole and shorter, and is nobody's cut.
const SearchSnippetWords = 24

// SearchSnippetMarkdown is a search hit's headline as Markdown to read: the whole body when it fit
// the headline window, balanced when the window cut it (a snippet exactly the window wide).
func SearchSnippetMarkdown(snippet string) string {
	if countWords(snippet) < SearchSnippetWords {
		return snippet
	}
	return BalanceCutMarkdown(snippet)
}

// countWords is how many whitespace-separated words text holds, which is what the browser's
// `snippet.split(/\s+/u).filter(word => word !== "")` counts.
func countWords(text string) int {
	words, inWord := 0, false
	for _, char := range text {
		if fragmentWhitespace(char) {
			inWord = false
			continue
		}
		if !inWord {
			words++
			inWord = true
		}
	}
	return words
}

// BalanceCutMarkdown is fragment with every delimiter the cut left unmatched removed. Runs are
// paired as the parser would pair them (a closer with the nearest earlier opener of the same
// character, the run lengths consumed in order); whatever run, or part of a run, is left over on
// either side could only have been matched across the cut, so it goes. A link cut inside its text
// or URL at either edge keeps its words.
func BalanceCutMarkdown(fragment string) string {
	text := cutOpenLinkDestination.ReplaceAllString(fragment, "$1")
	text = cutOpenLinkText.ReplaceAllString(text, "$1")
	text = cutClosedLinkTail.ReplaceAllString(text, "$1")

	runs := cutDelimiterRuns(text, codeSpans(text))
	// Remaining unmatched length per run, consumed as pairs are made.
	remaining := make([]int, len(runs))
	for index := range runs {
		remaining[index] = runs[index].end - runs[index].start
	}
	openers := make([]int, 0, len(runs))
	for index := range runs {
		run := runs[index]
		if run.canClose {
			for o := len(openers) - 1; o >= 0 && remaining[index] > 0; o-- {
				opener := runs[openers[o]]
				if opener.char != run.char || remaining[openers[o]] == 0 {
					continue
				}
				// GFM pairs a tilde run only with a closer of its own length (`~~a~` is plain
				// text); CommonMark's emphasis pairs one or two characters of a `*` or `_` run
				// at a time.
				take := 0
				if run.char == '~' {
					if remaining[openers[o]] == remaining[index] {
						take = remaining[index]
					}
				} else {
					take = min(remaining[openers[o]], remaining[index], 2)
				}
				if take == 0 {
					continue
				}
				remaining[openers[o]] -= take
				remaining[index] -= take
				if remaining[openers[o]] == 0 {
					openers = append(openers[:o], openers[o+1:]...)
				}
			}
		}
		if run.canOpen && remaining[index] > 0 {
			openers = append(openers, index)
		}
	}
	text = withoutLeftovers(text, runs, remaining)

	// A backtick run with no closer is a code span the cut opened when the span it opened would
	// run to the fragment's end: the cut took its closer. The text after it is the code the cut
	// left - a path, an identifier - with no space-separated prose in it (`\`dispatch-deploy.y`).
	// A lone tick followed by ordinary words (`press the \` key then`) is the author's, as
	// CommonMark reads it.
	start, stop := lastBacktickRun(text)
	if start < 0 || withinSpan(codeSpans(text), start) || proseFollows(text[stop:]) {
		return text
	}
	return text[:start] + text[stop:]
}

// withoutLeftovers is text without the characters of every run the pairing left over. The
// unmatched characters go from the side nearer the cut: an opener keeps its tail, a closer its
// head. Either way the run's leftover is gone. The runs are in text order and their leftovers
// never overlap, so one pass writes what is kept.
func withoutLeftovers(text string, runs []cutDelimiterRun, remaining []int) string {
	var kept strings.Builder
	written := 0
	for index := range runs {
		if remaining[index] == 0 {
			continue
		}
		run := runs[index]
		start, stop := run.end-remaining[index], run.end
		if run.canOpen && !run.canClose {
			start, stop = run.start, run.start+remaining[index]
		}
		if kept.Len() == 0 {
			kept.Grow(len(text))
		}
		kept.WriteString(text[written:start])
		written = stop
	}
	if written == 0 {
		return text
	}
	kept.WriteString(text[written:])
	return kept.String()
}

// cutDelimiterRun is one run of `*`, `_` or `~` the parser could pair: where it stands in the text,
// and whether CommonMark's flanking rules let it open or close emphasis.
type cutDelimiterRun struct {
	char       byte
	start, end int
	canOpen    bool
	canClose   bool
}

// cutDelimiterRuns is CommonMark's delimiter runs of `*`, `_` and `~` outside code spans, each with
// whether it can open or close emphasis (the left- and right-flanking rules, with `_`'s
// word-internal restriction). A run that can do neither is literal text and is never a candidate.
func cutDelimiterRuns(text string, code [][2]int) []cutDelimiterRun {
	var runs []cutDelimiterRun
	for index := 0; index < len(text); {
		if span := spanEnd(code, index); span > 0 {
			index = span
			continue
		}
		char := text[index]
		if char != '*' && char != '_' && char != '~' {
			index++
			continue
		}
		end := index
		for end < len(text) && text[end] == char {
			end++
		}
		// GFM ends the strikethrough sequence at a third marker, so `~~~` is literal text rather
		// than a delimiter of any length.
		if char == '~' && end-index > 2 {
			index = end
			continue
		}
		before, after := classifyBefore(text, index), classifyAt(text, end)
		leftFlanking := after != fragmentSpace &&
			(after != fragmentPunctuation || before == fragmentSpace || before == fragmentPunctuation)
		rightFlanking := before != fragmentSpace &&
			(before != fragmentPunctuation || after == fragmentSpace || after == fragmentPunctuation)
		canOpen, canClose := leftFlanking, rightFlanking
		if char == '_' {
			canOpen = leftFlanking && (!rightFlanking || before == fragmentPunctuation)
			canClose = rightFlanking && (!leftFlanking || after == fragmentPunctuation)
		}
		if canOpen || canClose {
			runs = append(runs, cutDelimiterRun{char: char, start: index, end: end, canOpen: canOpen, canClose: canClose})
		}
		index = end
	}
	return runs
}

// codeSpans is the spans of text that are code spans, as CommonMark reads them: a backtick run
// and the next run of the same length close it; a run with no closer is literal text.
func codeSpans(text string) [][2]int {
	var spans [][2]int
	for index := 0; index < len(text); {
		if text[index] != '`' {
			index++
			continue
		}
		runEnd := index
		for runEnd < len(text) && text[runEnd] == '`' {
			runEnd++
		}
		fence := text[index:runEnd]
		closer := indexFrom(text, fence, runEnd)
		for closer >= 0 && closer+len(fence) < len(text) && text[closer+len(fence)] == '`' {
			// A longer run is not this span's closer; skip past it.
			past := closer
			for past < len(text) && text[past] == '`' {
				past++
			}
			closer = indexFrom(text, fence, past)
		}
		if closer < 0 {
			index = runEnd
			continue
		}
		spans = append(spans, [2]int{index, closer + len(fence)})
		index = closer + len(fence)
	}
	return spans
}

// indexFrom is where needle next stands in text at or after from, or -1 when it does not.
func indexFrom(text, needle string, from int) int {
	if from >= len(text) {
		return -1
	}
	at := strings.Index(text[from:], needle)
	if at < 0 {
		return -1
	}
	return from + at
}

// spanEnd is where the code span holding index ends, or 0 when index stands in none.
func spanEnd(spans [][2]int, index int) int {
	for _, span := range spans {
		if index >= span[0] && index < span[1] {
			return span[1]
		}
	}
	return 0
}

// withinSpan reports whether index stands inside one of the code spans.
func withinSpan(spans [][2]int, index int) bool {
	return spanEnd(spans, index) > 0
}

// lastBacktickRun is where the last run of backticks in text starts and stops, or -1 when text
// holds none: the browser's /`+(?!.*`)/su, which matches the final run whole.
func lastBacktickRun(text string) (start, stop int) {
	stop = strings.LastIndexByte(text, '`') + 1
	if stop == 0 {
		return -1, 0
	}
	start = stop - 1
	for start > 0 && text[start-1] == '`' {
		start--
	}
	return start, stop
}

// proseFollows reports whether text holds space-separated prose: the browser's /\s\S+\s\S/u, a
// whitespace, a word, a whitespace and the start of another word. Code the cut left - a path, an
// identifier - holds none.
func proseFollows(text string) bool {
	for index, char := range text {
		if !fragmentWhitespace(char) {
			continue
		}
		rest := text[index+utf8.RuneLen(char):]
		word := 0
		for word < len(rest) {
			next, size := utf8.DecodeRuneInString(rest[word:])
			if fragmentWhitespace(next) {
				break
			}
			word += size
		}
		if word == 0 || word >= len(rest) {
			continue
		}
		_, size := utf8.DecodeRuneInString(rest[word:])
		if tail := rest[word+size:]; tail != "" {
			if next, _ := utf8.DecodeRuneInString(tail); !fragmentWhitespace(next) {
				return true
			}
		}
	}
	return false
}

// fragmentGroup is the group micromark's classifyCharacter puts a character in: whitespace,
// punctuation, or neither, which is what the flanking rules read.
type fragmentGroup int

const (
	fragmentWord fragmentGroup = iota
	fragmentSpace
	fragmentPunctuation
)

// classifyBefore is the group of the code point ending at index, or whitespace at the fragment's
// start, which is what the tokenizer classifies there. The code point is taken whole, so an
// astral character is the character CommonMark reads rather than a trailing surrogate of it: a
// `**` after an emoji follows punctuation (every emoji is `\p{S}`), which decides whether that
// run can open emphasis.
func classifyBefore(text string, index int) fragmentGroup {
	if index <= 0 {
		return fragmentSpace
	}
	char, _ := utf8.DecodeLastRuneInString(text[:index])
	return classifyFragmentCharacter(char)
}

// classifyAt is the group of the code point standing at index, or whitespace at the fragment's
// end, where the tokenizer classifies the absent character.
func classifyAt(text string, index int) fragmentGroup {
	if index >= len(text) {
		return fragmentSpace
	}
	char, _ := utf8.DecodeRuneInString(text[index:])
	return classifyFragmentCharacter(char)
}

// classifyFragmentCharacter is micromark's classifyCharacter (micromark-util-classify-character):
// whitespace first, then punctuation, which it reads as `\p{P}|\p{S}` exactly as the directive
// name reader does (directivePunctuation).
func classifyFragmentCharacter(char rune) fragmentGroup {
	switch {
	case fragmentWhitespace(char):
		return fragmentSpace
	case directivePunctuation(char):
		return fragmentPunctuation
	default:
		return fragmentWord
	}
}

// fragmentWhitespace is JavaScript's `\s`, which micromark's unicodeWhitespace tests the
// character against: the ASCII blanks, the Unicode space separators, the line and paragraph
// separators, and the byte-order mark. Go's unicode.IsSpace is not it - that one holds U+0085 and
// not U+FEFF - and this reading has to be the browser's, character for character.
func fragmentWhitespace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return char >= 0x2000 && char <= 0x200a
}

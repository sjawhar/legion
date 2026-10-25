package text

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Resolver says what a bare issue key and a `#id` name for one text. Mentions asks it at most
// once for each, and only when the body holds a candidate of that kind, so a database resolver
// answers every key of one body in one query and loads the text's question map once.
type Resolver interface {
	// Issues reports which of keys, each a distinct issue key the body writes bare, names an
	// issue that exists. A key absent from the result, or mapped to false, names nothing.
	Issues(keys []string) (map[string]bool, error)
	// Questions is the question map of the document the text belongs to: each question block id
	// to the ask reference a `#` and that id makes. A text that belongs to no document has none.
	Questions() (map[string]Ref, error)
}

// Mentions is ExtractAt with the references a text writes bare: each issue key the resolver says
// exists, as the issue reference `dispatch://<KEY>` makes, and each `#id` the resolver's question
// map names, as that ask's reference. Every mention keeps its byte offset (a `#id`'s is its `#`),
// in body order, repeats included, as ExtractAt reports them. A candidate inside a reference span
// (a `dispatch://` or `http(s)://` address, whether or not it names anything) is part of that
// address, never a mention of its own. A nil resolver reads no key and no `#id`, so Mentions is
// then ExtractAt.
//
// A candidate stands alone (mentionStart): at the start of the text, after whitespace or a
// numeric character reference to whitespace (`&#32;`, as stored markdown writes the space at a
// mark's edge), or after one of ( [ " ' * _ > ` ~ |. A key is an uppercase project key of 2 to 10
// characters, `-` and a number from 1 up, ending as keyEnds says. A `#id` is `#` and the longest
// run of the block-id characters [A-Za-z0-9_-]; when no question holds that run, the run without
// its trailing `_` and `-` is tried, so `_see #q1_` names q1. There is no markdown parse here, so
// a candidate in code is read, as a reference in code is.
func Mentions(body, serverURL string, r Resolver) ([]Located, error) {
	spans := referencePattern.FindAllStringIndex(body, -1)
	located := extractSpans(body, serverURL, spans)
	if r == nil {
		return located, nil
	}

	type candidate struct {
		offset int
		key    string // set for a key
		id     string // the run after `#`, set for a `#id`
	}
	candidates := []candidate{}
	keys := []string{}
	seenKeys := map[string]struct{}{}
	hasID := false
	scan := func(from, to int) {
		for i := from; i < to; i++ {
			c := body[i]
			if c != '#' && (c < 'A' || c > 'Z') || !mentionStart(body, i) {
				continue
			}
			if c == '#' {
				end := i + 1
				for end < len(body) && blockIDByte(body[end]) {
					end++
				}
				if end > i+1 {
					candidates = append(candidates, candidate{offset: i, id: body[i+1 : end]})
					hasID = true
				}
				i = end - 1
				continue
			}
			end, ok := keyAt(body, i)
			if !ok {
				continue
			}
			key := body[i:end]
			candidates = append(candidates, candidate{offset: i, key: key})
			if _, seen := seenKeys[key]; !seen {
				seenKeys[key] = struct{}{}
				keys = append(keys, key)
			}
			i = end - 1
		}
	}
	from := 0
	for _, span := range spans {
		scan(from, span[0])
		from = span[1]
	}
	scan(from, len(body))
	if len(candidates) == 0 {
		return located, nil
	}

	var issues map[string]bool
	if len(keys) > 0 {
		var err error
		if issues, err = r.Issues(keys); err != nil {
			return nil, err
		}
	}
	var questions map[string]Ref
	if hasID {
		var err error
		if questions, err = r.Questions(); err != nil {
			return nil, err
		}
	}

	resolved := make([]Located, 0, len(candidates))
	for _, item := range candidates {
		if item.key != "" {
			if issues[item.key] {
				resolved = append(resolved, Located{
					Ref:    Ref{Kind: "issue", IssueKey: item.key, ID: item.key},
					Offset: item.offset,
				})
			}
			continue
		}
		ref, ok := questions[item.id]
		if !ok {
			if trimmed := strings.TrimRight(item.id, "_-"); trimmed != item.id && trimmed != "" {
				ref, ok = questions[trimmed]
			}
		}
		if ok {
			resolved = append(resolved, Located{Ref: ref, Offset: item.offset})
		}
	}
	if len(resolved) == 0 {
		return located, nil
	}

	merged := make([]Located, 0, len(located)+len(resolved))
	for len(located) > 0 && len(resolved) > 0 {
		if located[0].Offset < resolved[0].Offset {
			merged, located = append(merged, located[0]), located[1:]
		} else {
			merged, resolved = append(merged, resolved[0]), resolved[1:]
		}
	}
	merged = append(merged, located...)
	return append(merged, resolved...), nil
}

// keyAt reads the issue key starting at start, `[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*` as
// issueKeyPattern spells it, and returns where it ends; ok is false when none starts there or
// what follows it does not end it (keyEnds).
func keyAt(body string, start int) (end int, ok bool) {
	end = start + 1
	for end < len(body) && (body[end] >= 'A' && body[end] <= 'Z' || isDigitByte(body[end])) {
		end++
	}
	if end-start < 2 || end-start > 10 || end+1 >= len(body) || body[end] != '-' ||
		body[end+1] < '1' || body[end+1] > '9' {
		return 0, false
	}
	end += 2
	for end < len(body) && isDigitByte(body[end]) {
		end++
	}
	return end, keyEnds(body, end)
}

// keyEnds reports whether a key that runs up to end stands alone on its right: at the end of the
// text, or before a character that is not a letter, a digit, `-`, `/`, `#` or `@`. Sentence
// punctuation and the emphasis and strikethrough delimiters (`.` `,` `:` `;` `_` `*` `~`) end it
// only when no letter or digit follows them, so `CORE-1's` and `_CORE-1_.` read CORE-1, and
// `CORE-1.5`, `CORE-1_x`, `CORE-1/spec` and `CORE-1#q1` read nothing.
func keyEnds(body string, end int) bool {
	if end == len(body) {
		return true
	}
	next, size := utf8.DecodeRuneInString(body[end:])
	switch next {
	case '-', '/', '#', '@':
		return false
	case '.', ',', ':', ';', '_', '*', '~':
		after, _ := utf8.DecodeRuneInString(body[end+size:])
		return end+size == len(body) || !isLetterOrDigit(after)
	}
	return !isLetterOrDigit(next)
}

// mentionStart reports whether a bare key or `#id` may start at offset: the start of the text,
// whitespace as referenceEnd spells it, a numeric character reference to such whitespace, or one
// of ( [ " ' * _ > ` ~ |, the characters that open a parenthesis, a link's text, a quote,
// emphasis, a quote block, a code span, strikethrough or a table cell.
func mentionStart(body string, offset int) bool {
	if offset == 0 {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(body[:offset])
	switch before {
	case '(', '[', '"', '\'', '*', '_', '>', '`', '~', '|':
		return true
	case ';':
		return endsWithWhitespaceReference(body[:offset])
	}
	return isReferenceSpace(before)
}

// endsWithWhitespaceReference reports whether prefix ends with a numeric character reference, as
// CommonMark reads one (`&#` and 1 to 7 decimal digits, or `&#x` and 1 to 6 hex digits, then
// `;`), to a character isReferenceSpace accepts.
func endsWithWhitespaceReference(prefix string) bool {
	window := prefix[max(0, len(prefix)-11) : len(prefix)-1]
	amp := strings.LastIndex(window, "&#")
	if amp < 0 {
		return false
	}
	digits, base, limit := window[amp+2:], 10, 7
	if len(digits) > 0 && (digits[0] == 'x' || digits[0] == 'X') {
		digits, base, limit = digits[1:], 16, 6
	}
	if digits == "" || len(digits) > limit {
		return false
	}
	value, err := strconv.ParseUint(digits, base, 32)
	return err == nil && isReferenceSpace(rune(value))
}

// isReferenceSpace is the whitespace that ends a reference (referenceEnd): tab, line feed, form
// feed, carriage return and the Unicode space separators, not a vertical tab or U+FEFF.
func isReferenceSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\f' || r == '\r' || unicode.Is(unicode.Z, r)
}

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isDigitByte(c byte) bool {
	return c >= '0' && c <= '9'
}

// blockIDByte is a character of a question block's id, as the `:::ask{#id}` line declares it.
func blockIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || isDigitByte(c) || c == '_' || c == '-'
}

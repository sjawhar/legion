// Package text implements Dispatch text selectors and reference extraction.
package text

import (
	"errors"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ErrTargetNotFound reports that an anchor quote does not occur in its text.
var ErrTargetNotFound = errors.New("target not found")

// Candidate is one possible UTF-16 range for an ambiguous anchor quote.
type Candidate struct {
	From    int    `json:"from"`
	To      int    `json:"to"`
	Context string `json:"context"`
}

// ErrTargetAmbiguous reports a quote with multiple matches and no occurrence.
type ErrTargetAmbiguous struct {
	Candidates []Candidate
}

func (e *ErrTargetAmbiguous) Error() string { return "target is ambiguous" }

// Resolve finds quote in text. Ranges and occurrence are UTF-16 code units.
func Resolve(text, quote string, occurrence *int) (int, int, error) {
	units := encode16(text)
	quoteUnits := encode16(quote)
	matches := findAll(units, quoteUnits)
	if len(matches) == 0 {
		return 0, 0, ErrTargetNotFound
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(matches) {
			return 0, 0, ErrTargetNotFound
		}
		from := matches[*occurrence]
		return from, from + len(quoteUnits), nil
	}
	if len(matches) > 1 {
		candidates := make([]Candidate, 0, len(matches))
		for _, from := range matches {
			to := from + len(quoteUnits)
			candidates = append(candidates, Candidate{
				From:    from,
				To:      to,
				Context: Slice16(text, max(0, from-40), min(len(units), to+40)),
			})
		}
		return 0, 0, &ErrTargetAmbiguous{Candidates: candidates}
	}
	from := matches[0]
	return from, from + len(quoteUnits), nil
}

// Reresolve maps a stored anchor onto text. Exact matches nearest the previous
// offset win. When exact matching fails, whitespace-normalized text is tried.
func Reresolve(text string, anchor model.Anchor) model.Anchor {
	units := encode16(text)
	matches := findAll(units, encode16(anchor.Quote))
	if len(matches) > 0 {
		from := nearest(matches, anchor.From)
		anchor.From = from
		anchor.To = from + Len16(anchor.Quote)
		anchor.Orphaned = false
		return anchor
	}

	normalizedText, spans := normalizedRanges(text)
	normalizedQuote, _ := normalizedRanges(anchor.Quote)
	if normalizedQuote != "" {
		textRunes := []rune(normalizedText)
		quoteRunes := []rune(normalizedQuote)
		if matches := findAllRunes(textRunes, quoteRunes); len(matches) > 0 {
			index := nearestSpan(matches, spans, anchor.From)
			anchor.From = spans[index].from
			anchor.To = spans[index+len(quoteRunes)-1].to
			anchor.Orphaned = false
			return anchor
		}
	}

	anchor.Orphaned = true
	return anchor
}

func findAll(haystack, needle []uint16) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	matches := []int{}
	for start := 0; start <= len(haystack)-len(needle); start++ {
		matched := true
		for offset, unit := range needle {
			if haystack[start+offset] != unit {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, start)
		}
	}
	return matches
}

func findAllRunes(haystack, needle []rune) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	matches := []int{}
	for start := 0; start <= len(haystack)-len(needle); start++ {
		matched := true
		for offset, value := range needle {
			if haystack[start+offset] != value {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, start)
		}
	}
	return matches
}

func nearest(matches []int, from int) int {
	best := matches[0]
	bestDistance := abs(best - from)
	for _, candidate := range matches[1:] {
		distance := abs(candidate - from)
		if distance < bestDistance || (distance == bestDistance && candidate < best) {
			best = candidate
			bestDistance = distance
		}
	}
	return best
}

func nearestSpan(matches []int, spans []range16, from int) int {
	best := matches[0]
	bestDistance := abs(spans[best].from - from)
	for _, candidate := range matches[1:] {
		distance := abs(spans[candidate].from - from)
		if distance < bestDistance || (distance == bestDistance && candidate < best) {
			best = candidate
			bestDistance = distance
		}
	}
	return best
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

type range16 struct {
	from int
	to   int
}

func normalizedRanges(value string) (string, []range16) {
	out := make([]rune, 0, len(value))
	spans := make([]range16, 0, len(value))
	unitOffset := 0
	inWhitespace := false
	whitespace := range16{}
	for _, value := range []rune(value) {
		width := Len16(string(value))
		if unicode.IsSpace(value) {
			if !inWhitespace {
				whitespace = range16{from: unitOffset, to: unitOffset + width}
				inWhitespace = true
			} else {
				whitespace.to = unitOffset + width
			}
			unitOffset += width
			continue
		}
		if inWhitespace && len(out) > 0 {
			out = append(out, ' ')
			spans = append(spans, whitespace)
		}
		inWhitespace = false
		out = append(out, value)
		spans = append(spans, range16{from: unitOffset, to: unitOffset + width})
		unitOffset += width
	}
	return string(out), spans
}

package model

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/contracts"
)

func units(t *testing.T, text string) int {
	t.Helper()
	return len(utf16.Encode([]rune(text)))
}

// A body of at most contracts.DispatchEventBodyMax UTF-16 units rides an event whole, and says so
// by answering false: nothing of it is missing, and no reader has to go back to the server.
func TestEventBodyCarriesABodyAtTheBoundWhole(t *testing.T) {
	for name, body := range map[string]string{
		"empty":             "",
		"one line":          "Ship it.",
		"at the bound":      strings.Repeat("a", contracts.DispatchEventBodyMax),
		"wide at the bound": strings.Repeat("あ", contracts.DispatchEventBodyMax),
		"a surrogate pair at the bound": strings.Repeat("a", contracts.DispatchEventBodyMax-2) +
			"\U0001F680",
	} {
		t.Run(name, func(t *testing.T) {
			if got := units(t, body); got > contracts.DispatchEventBodyMax {
				t.Fatalf("the test body is %d UTF-16 units, past the %d bound", got, contracts.DispatchEventBodyMax)
			}
			carried, truncated := EventBody(body)
			if carried != body || truncated {
				t.Fatalf("EventBody(%d units) cut to %d units, truncated=%v; want the body whole and false",
					units(t, body), units(t, carried), truncated)
			}
		})
	}
}

// A longer body is cut at the last blank line inside the bound, so the event carries whole
// paragraphs: three 15,000-unit paragraphs leave the first two.
func TestEventBodyCutsAtTheLastBlankLineInsideTheBound(t *testing.T) {
	const paragraph = 15_000
	first, second, third := strings.Repeat("a", paragraph), strings.Repeat("b", paragraph), strings.Repeat("c", paragraph)
	body := first + "\n\n" + second + "\n\n" + third
	carried, truncated := EventBody(body)
	if !truncated {
		t.Fatalf("a body of %d units answered truncated=false", units(t, body))
	}
	if want := first + "\n\n" + second; carried != want {
		t.Fatalf("the carried body is %d units, want the first two paragraphs (%d units)",
			units(t, carried), units(t, want))
	}
}

// With no blank line inside the bound the cut falls at the last line break, and with neither at
// the last character boundary. Every cut leaves at most the bound.
func TestEventBodyCutsAtALineBreakThenAtACharacter(t *testing.T) {
	line := strings.Repeat("a", 12_000) + "\n"
	lines := strings.Repeat(line, 4)
	carried, truncated := EventBody(lines)
	if want := line + strings.Repeat("a", 12_000); !truncated || carried != want {
		t.Fatalf("a body of four 12,001-unit lines was cut to %d units (truncated=%v), want the first line and the second's text (%d units)",
			units(t, carried), truncated, units(t, want))
	}

	oneBlock := strings.Repeat("a", contracts.DispatchEventBodyMax+1_000)
	carried, truncated = EventBody(oneBlock)
	if !truncated || carried != strings.Repeat("a", contracts.DispatchEventBodyMax) {
		t.Fatalf("a single line of %d units was cut to %d units (truncated=%v), want exactly the %d-unit bound",
			units(t, oneBlock), units(t, carried), truncated, contracts.DispatchEventBodyMax)
	}
}

// A character whose UTF-16 form is a surrogate pair is carried whole or not at all: a cut that
// fell between its two units would make a body no JSON string can hold. The character astride
// the bound is dropped, so the cut body is one unit short of it.
func TestEventBodyNeverCutsInsideASurrogatePair(t *testing.T) {
	body := strings.Repeat("a", contracts.DispatchEventBodyMax-1) + strings.Repeat("\U0001F680", 10)
	carried, truncated := EventBody(body)
	if !truncated {
		t.Fatalf("a body of %d units answered truncated=false", units(t, body))
	}
	if want := strings.Repeat("a", contracts.DispatchEventBodyMax-1); carried != want {
		t.Fatalf("the carried body is %q…%q of %d units, want the %d letters before the first rocket",
			carried[:4], carried[len(carried)-4:], units(t, carried), contracts.DispatchEventBodyMax-1)
	}
	for _, r := range carried {
		if utf16.RuneLen(r) != 1 {
			t.Fatalf("the carried body holds %q, a character of two UTF-16 units, at the cut", r)
		}
	}
}

// Every cut body is at most the bound, whatever the shape of the text: the one promise the
// outbox's envelope size rests on.
func TestEveryCutBodyIsWithinTheBound(t *testing.T) {
	for name, body := range map[string]string{
		"paragraphs":       strings.Repeat(strings.Repeat("word ", 100)+"\n\n", 400),
		"one long line":    strings.Repeat("x", 200_000),
		"wide characters":  strings.Repeat("あ", 50_000),
		"blank line first": "\n\n" + strings.Repeat("y", contracts.DispatchEventBodyMax+10),
		"no text at all":   strings.Repeat("\n", contracts.DispatchEventBodyMax+10),
	} {
		t.Run(name, func(t *testing.T) {
			carried, truncated := EventBody(body)
			if !truncated {
				t.Fatalf("a body of %d units answered truncated=false", units(t, body))
			}
			if got := units(t, carried); got > contracts.DispatchEventBodyMax {
				t.Fatalf("the cut body is %d units, past the %d bound", got, contracts.DispatchEventBodyMax)
			}
			if !strings.HasPrefix(body, carried) {
				t.Fatalf("the cut body is not the head of the body it was cut from")
			}
		})
	}
}

// A blank line at the very start names no text to carry, so the cut falls through to the rules
// after it rather than leaving an event with an empty body.
func TestEventBodyNeverCutsToNothing(t *testing.T) {
	body := "\n\n" + strings.Repeat("z", contracts.DispatchEventBodyMax+10)
	carried, truncated := EventBody(body)
	if !truncated || carried == "" {
		t.Fatalf("EventBody cut a body of %d units to %q (truncated=%v), want text and true",
			units(t, body), carried, truncated)
	}
}

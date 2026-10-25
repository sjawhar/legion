package embed

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
)

// longText is exactly maxInputChars (32,000) runes, mixing one-, two-, three- and four-byte
// UTF-8 runes, so a client that counted bytes, or cut a text at its own limit, would not deliver
// it whole.
func longText() string {
	text := strings.Repeat("aé漢🙂", maxInputChars/4)
	if n := len([]rune(text)); n != maxInputChars {
		panic(fmt.Sprintf("longText has %d runes, want %d", n, maxInputChars))
	}
	return text
}

func TestLiteralRendersAPgvectorTextForm(t *testing.T) {
	got := Literal([]float32{0.5, -1, 0})
	want := "[0.50000000,-1.00000000,0.00000000]"
	if got != want {
		t.Errorf("Literal = %q, want %q", got, want)
	}
}

// markedError carries the throttle marker with a chosen answer, so IsThrottled is tested on the
// marker alone.
type markedError struct{ throttled bool }

func (e markedError) Error() string   { return "simulated marked error" }
func (e markedError) Throttled() bool { return e.throttled }

func TestIsThrottledReadsTheMarkerThroughAWrappedError(t *testing.T) {
	if !IsThrottled(fmt.Errorf("embed: some call: %w", markedError{throttled: true})) {
		t.Error("IsThrottled(a wrapped marked throttle) = false, want true")
	}
	if !IsThrottled(fmt.Errorf("embed: some call: %w", embedtest.FakeThrottleError{Code: "ThrottlingException"})) {
		t.Error("IsThrottled(a wrapped embedtest.FakeThrottleError) = false, want true")
	}
	cause := errors.New("simulated cause")
	marked := &throttleError{err: cause}
	if !IsThrottled(marked) || !errors.Is(marked, cause) || marked.Error() != cause.Error() {
		t.Errorf("throttleError = %v: want it marked, unwrapping to its cause, with the cause's text", marked)
	}
}

func TestIsThrottledIgnoresAnUnmarkedOrFalseError(t *testing.T) {
	if IsThrottled(errors.New("embed: connection refused")) {
		t.Error("IsThrottled(a plain error) = true, want false")
	}
	if IsThrottled(markedError{throttled: false}) {
		t.Error("IsThrottled(a marker answering false) = true, want false")
	}
	// A Bedrock-style throttle code alone is not the marker: only the client that recognizes its
	// own codes marks them.
	if IsThrottled(codedError{code: "ThrottlingException"}) {
		t.Error("IsThrottled(an unmarked coded error) = true, want false")
	}
	if IsThrottled(nil) {
		t.Error("IsThrottled(nil) = true, want false")
	}
}

// TestEstimateTokensTruncatesAndDividesByFourCharsPerToken proves EstimateTokens matches the
// same truncation Embed itself applies (maxInputChars, by rune count) and the 4-chars/token
// ratio the original "Bedrock capacity" measurement used - the basis for embedqueue's shared
// token-rate budget (reserveTokens).
func TestEstimateTokensTruncatesAndDividesByFourCharsPerToken(t *testing.T) {
	short := strings.Repeat("x", 40)
	if got, want := EstimateTokens([]string{short}), 10; got != want {
		t.Errorf("EstimateTokens(%d chars) = %d, want %d", len(short), got, want)
	}
	long := strings.Repeat("y", maxInputChars+8000)
	if got, want := EstimateTokens([]string{long}), maxInputChars/4; got != want {
		t.Errorf("EstimateTokens(a text over maxInputChars) = %d, want %d (truncated to maxInputChars first)", got, want)
	}
	if got, want := EstimateTokens([]string{short, short}), 20; got != want {
		t.Errorf("EstimateTokens(two short texts) = %d, want %d (summed across the batch)", got, want)
	}
}

//go:build !race

package pmdoc

// parseFullTarget and renderFullTarget are TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's
// full-size targets outside -race. Under -race (linear_sizes_race.go) both are a growthStep's
// fraction of these: -race's own per-access instrumentation costs real CPU time regardless of
// ambient load, so a -count=N run needs smaller absolute targets to stay inside go test's own
// default per-package timeout, not because the ratio check (growthVerdict) needs different sizes
// to discriminate - growthStep and linearSlack, which that check actually depends on, are the
// same under both.
const (
	// parseFullTarget is a mebibyte: the write-path limit (MaxDocumentBytes) this loop's parse
	// reads back, since a caller's write of this much is refused for the elements it makes.
	parseFullTarget = 1 << 20
	// renderFullTarget is smaller: large enough for growthStep's quadratic margin, small enough
	// that even the slowest shape (`<a`) stays fast - rendering is not bound by MaxDocumentBytes.
	renderFullTarget = 512 << 10
)

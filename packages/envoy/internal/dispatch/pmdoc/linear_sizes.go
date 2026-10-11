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
	// updateSeedTarget is TestUpdateIsLinearInTheBlockCount's seed of one-word paragraphs: the
	// mebibyte a document may hold, which a rebuild from a stored rendering writes whole.
	updateSeedTarget = 1 << 20
	// updateShapeTarget is its other shapes', each 21,845 blocks or fewer, about what the 65,536
	// elements one upload may make allow of them.
	updateShapeTarget = 128 << 10
)

// alternatingBlocks are the documents TestUpdateIsLinearInTheBlockCount replaces every other block
// of: 16,384 blocks is about the most paragraphs the 65,536 elements one upload may make allow.
var alternatingBlocks = []int{4_096, 16_384}

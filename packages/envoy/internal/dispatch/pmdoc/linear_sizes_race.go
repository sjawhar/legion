//go:build race

package pmdoc

// See linear_sizes.go: the non-race build's doc comment explains both constants and why -race
// gets its own, smaller pair - an eighth of the non-race targets, the same fraction growthStep
// already separates each loop's own small size from its full size.
const (
	parseFullTarget  = 128 << 10
	renderFullTarget = 64 << 10
)

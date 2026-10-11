//go:build race

package pmdoc

// See linear_sizes.go: the non-race build's doc comment explains both constants and why -race
// gets its own, smaller pair - an eighth of the non-race targets, the same fraction growthStep
// already separates each loop's own small size from its full size.
const (
	parseFullTarget   = 128 << 10
	renderFullTarget  = 64 << 10
	updateSeedTarget  = 128 << 10
	updateShapeTarget = 16 << 10
)

// alternatingBlocks is the race build's pair: smaller, as its other targets are, but past the few
// hundred blocks where Update's fixed costs outweigh ygo's walks.
var alternatingBlocks = []int{2_048, 4_096}

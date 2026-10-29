//go:build race

package pmdoc

// See linear_sizes.go: the non-race build's comments explain each constant and why -race gets its
// own, smaller ones - an eighth of the non-race targets, the same fraction growthStep already
// separates each loop's own small size from its full size.
const (
	parseFullTarget   = 128 << 10
	renderFullTarget  = 64 << 10
	updateSeedTarget  = 128 << 10
	updateShapeTarget = 16 << 10
)

// alternatingBlocks is the race build's one size, the non-race build's smaller one.
// -race slows Update's work beside ygo's walks - building each block and comparing the trees -
// more than it slows the walks, so that linear work outweighs the walks further into the document:
// at 2,048 blocks Update took 2.1 to 4.0 times the walks, reaching alternatingAllowance, and a
// write that walks the fragment once more per replaced block 3.5 to 9.2 times, so that size could
// not tell the two apart. At 4,096 blocks the walks' quadratic growth dominates: 1.5 to 2.5 times
// for Update against 11.4 to 21.3 for that write.
var alternatingBlocks = []int{4_096}

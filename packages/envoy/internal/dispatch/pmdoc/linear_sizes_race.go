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

// tagShapeSizes stop at 256 KiB under -race, in one step of sixteen times: growthVerdict allows a
// step growthAllowance times its growth, 64 times here, which a quadratic cost (256 times) still
// exceeds, where steps of four times would allow sixteen, all a quadratic cost grows by. Rendering
// `<a b="x;a@b.cc ">` repeated grows linearly - 0.76 s at 256 KiB, 1.14 s at 512 KiB and 2.61 s at
// a mebibyte without -race, on a two-core pod - but it costs about fifteen times its parse, and on
// a slow runner -race's instrumentation took the mebibyte past the one-minute deadline per call the
// test then had (perCallDeadline). A write of that shape never reaches 512 KiB: the 65,536 elements
// one upload may make refuse it first.
var tagShapeSizes = []int{16 << 10, 256 << 10}

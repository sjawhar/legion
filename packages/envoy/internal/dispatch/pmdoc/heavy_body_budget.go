//go:build !race

package pmdoc

import "time"

// heavyBodyCPUBudget is the most CPU time the parse of one body at the body cap may spend
// (heavy_bodies_test.go), outside -race. A body a caller writes is read on a request's own
// goroutine, and the bodies that froze a reader's browser tab are the heaviest a caller may send;
// the slowest of them measured about 0.5 s here, so a second is the bound with the slack a loaded
// runner needs and still catches a cost that has grown with the square of a body's length again.
//
// Under -race (heavy_body_budget_race.go) it is zero and no time is asserted at all: the race
// detector instruments every memory access, which takes the same calls to 2-8 s whatever this
// package does, so a bound there would measure the detector. What that build asserts instead is
// that each shape's time grows in proportion to its length (growsLinearly), which the
// instrumentation cannot change, as linear_sizes.go's pair of targets says of its own loops.
const heavyBodyCPUBudget = time.Second

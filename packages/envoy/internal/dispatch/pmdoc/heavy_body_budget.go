package pmdoc

import "time"

// heavyBodyCPUBudget is the most CPU time the parse of one body at the body cap may spend
// (heavy_bodies_test.go). A body a caller writes is read on a request's own goroutine, and the
// bodies that froze a reader's browser tab are the heaviest a caller may send; the slowest of
// them measured about 0.5 s here, so a second is the bound with the slack a loaded runner needs
// and still catches a cost that has grown with the square of a body's length again. This
// package's tests run plain, never under the race detector, so the bound measures this package's
// own work in every run of them.
const heavyBodyCPUBudget = time.Second

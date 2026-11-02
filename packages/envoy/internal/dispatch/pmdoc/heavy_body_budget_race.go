//go:build race

package pmdoc

import "time"

// See heavy_body_budget.go: zero is no time bound at all, which is what the race build asserts of
// these bodies, since the race detector's own instrumentation, not this package, is what takes
// them to 2-8 s there. Their growth is asserted in both builds.
const heavyBodyCPUBudget = time.Duration(0)

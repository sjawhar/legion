//go:build race

package storetest

// raceDetector says this binary was built with the race detector, which the xid rig reads to pick
// the rule a window is held to (XidGapAllowanceFor). The build tag is the only thing that says
// so: a rig that guessed its build from a measured duration would widen its own bound on the very
// stall it is there to catch.
const raceDetector = true

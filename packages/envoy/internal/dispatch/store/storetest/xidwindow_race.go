//go:build race

package storetest

// raceDetector says this binary was built with the race detector, which the xid rig reads to pick
// the gap allowance in force (XidGapAllowance). The build tag is the only thing that says so: a
// rig that guessed its build from a measured duration would widen its own bound on the very stall
// it is there to catch.
const raceDetector = true

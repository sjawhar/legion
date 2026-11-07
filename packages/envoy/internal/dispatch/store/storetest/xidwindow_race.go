//go:build race

package storetest

// raceDetector says this binary was built with the race detector, which the xid rig reads to say
// that it judges no window's time (XidJudgesGaps), only its shape. The build tag is the only
// thing that says so: a rig that guessed its build from a measured duration would decide what to
// judge from the very stall it cannot read.
const raceDetector = true

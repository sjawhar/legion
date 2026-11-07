//go:build !race

package storetest

// raceDetector says this binary was built without the race detector, so the xid rig holds every
// window to D6's own gap as well as to its shape (XidJudgesGaps).
const raceDetector = false

//go:build race

package docs

// stormRaceEnabled makes the edit storm run fewer seeds and fewer edits under -race, whose
// instrumentation makes each live-room commit and its shadow render several times the cost.
const stormRaceEnabled = true

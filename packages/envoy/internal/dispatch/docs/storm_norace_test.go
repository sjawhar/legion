//go:build !race

package docs

// stormRaceEnabled is false outside -race (storm_race_test.go).
const stormRaceEnabled = false

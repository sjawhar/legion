//go:build !race

package pmdoc

// raceEnabled is false outside -race (layout_race_test.go).
const raceEnabled = false

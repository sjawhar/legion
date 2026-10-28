//go:build race

package pmdoc

// incrementalRaceBuild makes the incremental render differential mutate fewer blocks under -race,
// whose instrumentation multiplies each whole render's cost (linear_sizes_race.go).
const incrementalRaceBuild = true

//go:build !race

package pmdoc

// incrementalRaceBuild is false outside -race (layout_race_test.go).
const incrementalRaceBuild = false

//go:build !race

package bus_test

// raceEnabled is whether the test binary runs under the race detector: race_test.go says why an
// allocation test reads it.
const raceEnabled = false

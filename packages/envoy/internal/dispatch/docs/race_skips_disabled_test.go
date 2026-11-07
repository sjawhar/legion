//go:build !race

package docs

// raceSkipsEnabled is raceEnabled for the two helpers of race_skips_test.go, which that file says
// why they read their own constant.
const raceSkipsEnabled = false

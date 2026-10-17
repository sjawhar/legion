//go:build race

package bus_test

// raceEnabled is whether the test binary runs under the race detector; norace_test.go is its
// other half. An allocation test skips under it, as the standard library's do (internal/race,
// runtime/slice_test.go TestAppendByteInLoop): the race runtime allocates on its own, and disables
// the tiny allocator, so testing.AllocsPerRun, which counts every malloc in the process, no longer
// counts only the function under test.
const raceEnabled = true

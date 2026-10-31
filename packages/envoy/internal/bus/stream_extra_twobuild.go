//go:build envoy_twobuild

package bus

// A binary built with the envoy_twobuild tag stands for a later build of Envoy that compiles in one
// stream subject more than this one, notifications.twobuild.>, as a release that adds a subject
// does. cmd/listener's two-build test (twobuild_test.go, under the multiprocess tag) runs it beside
// the plain build for one machine id, so the two disagree on the stream's subjects the way two
// deployed builds do during a roll. Nothing is released with this tag, and without it this file is
// not compiled: the plain build's subjects and stream are unchanged.
func init() {
	streamSubjects = append(streamSubjects, "notifications.twobuild.>")
	// streamCfg took the slice as it stood when the package's variables were initialised, before
	// this init ran.
	streamCfg.Subjects = streamSubjects
}

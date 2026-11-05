package pmdoc

import "sync/atomic"

// DisplayTreeVersion numbers what this build makes of a display tree: the read a reader is shown
// (ReadForDisplay), the wire encoding that read is carried in (toDisplayJSON, display_encode.go)
// and the dashboard schema whose defaults that encoding leaves out. A stored tree is served in
// place of reading its text again only while the version that stored it stands at this number,
// so a change to any of the three raises it and every tree stored before it is read again.
//
// It is a number of its own rather than RendererVersion because neither of the two moves the
// other: a change to what Render writes voids no display tree, and a change to what a reader is
// shown voids no stored rendering. A build that shared one number between them would read every
// stored tree again for a change to the markdown writer alone.
//
// DisplayVersion (display.go) is not this: it is one of the two display *modes* a text can be
// read in, DisplayLatest being the other, and the mode a tree was read in is part of that tree's
// key beside this version. This constant numbers the parser; that one names a reading.
//
// The golden testdata/display-tree-version.json holds this number beside a hash of the display
// corpus's trees in both modes, and TestDisplayTreeVersionGoldenHoldsTheCorpus refuses a change
// to that hash that leaves the number alone.
const DisplayTreeVersion = 1

// displayTreeVersionNow is the version this process keys a stored tree by. It is a variable
// seeded from the constant rather than the constant itself because a test has to stand the
// server at a version of its own to watch the background pass store the trees again, which a
// compile-time constant cannot be made to do; no server writes it, so one runs its whole life at
// DisplayTreeVersion.
var displayTreeVersionNow = func() *atomic.Int64 {
	version := &atomic.Int64{}
	version.Store(DisplayTreeVersion)
	return version
}()

// DisplayTreeVersionNow is the version every store of a tree, and every lookup of one, keys by:
// DisplayTreeVersion, unless a test is standing this process at another
// (SetDisplayTreeVersionForTest).
func DisplayTreeVersionNow() int { return int(displayTreeVersionNow.Load()) }

// SetDisplayTreeVersionForTest stands this process at version, the way a deploy of a build with a
// higher constant does, until the function it answers puts the version it found back. It is for
// tests alone: no server changes the version it runs at, and a caller outside a test would leave
// readers served trees two builds disagree about.
func SetDisplayTreeVersionForTest(version int) (restore func()) {
	previous := displayTreeVersionNow.Swap(int64(version))
	return func() { displayTreeVersionNow.Store(previous) }
}

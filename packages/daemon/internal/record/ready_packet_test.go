package record

import "testing"

// The packet a by-hand READY is told as is its note, a blank line, then the merger's summary with
// every line that begins `Merge:` - the merger's line asking for the merge to be submitted -
// replaced by the by-hand line, so no reader of the packet arms it. A READY with no note is told as
// its summary, byte for byte.
func TestReadyPacketPutsTheNoteFirstAndReplacesEveryMergeLine(t *testing.T) {
	const (
		note       = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
		submitLine = "Merge: the daemon submits pull request #42 at head once READY is accepted"
		first      = "READY #42 at head (approved at approved) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)"
		last       = "A docs/solutions/legion/retro-LEGION-208.md"
	)
	for _, tc := range []struct {
		name    string
		summary string
		note    string
		want    string
	}{
		{
			name:    "no note leaves the summary as it is",
			summary: first + "\n" + submitLine + "\n\n" + last,
			want:    first + "\n" + submitLine + "\n\n" + last,
		},
		{
			name:    "the note comes first and the merger's Merge line is replaced",
			summary: first + "\n" + submitLine + "\n\n" + last,
			note:    note,
			want:    note + "\n\n" + first + "\n" + byHandMergeLine + "\n\n" + last,
		},
		{
			name:    "every Merge line is replaced",
			summary: first + "\n" + submitLine + "\n" + submitLine + "\n\n" + last,
			note:    note,
			want:    note + "\n\n" + first + "\n" + byHandMergeLine + "\n" + byHandMergeLine + "\n\n" + last,
		},
		{
			name:    "a summary with no Merge line keeps every line",
			summary: first + "\n\n" + last,
			note:    note,
			want:    note + "\n\n" + first + "\n\n" + last,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadyPacket(tc.summary, tc.note); got != tc.want {
				t.Fatalf("ReadyPacket(%q, %q) = %q, want %q", tc.summary, tc.note, got, tc.want)
			}
		})
	}
}

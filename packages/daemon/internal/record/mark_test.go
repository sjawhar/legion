package record

import (
	"testing"
	"time"
)

// A wait the daemon records starts at a sequenced event or read, and Dispatch has given every
// review and every event a sequence since its migration 0118, so an act it shows with no sequence
// was recorded before that start, whatever its time says: a time is the start of the writing
// transaction on one clock, and a registration's read has no time of its own to compare with. Two
// sequences compare as sequences, and two marks without one fall back to their times.
func TestAnActWithNoSequenceIsNeverAfterASequencedStart(t *testing.T) {
	base := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		act   EventMark
		start EventMark
		after bool
	}{
		{name: "a later sequence", act: EventMark{Seq: 11}, start: EventMark{Seq: 10}, after: true},
		{name: "an earlier sequence with a later time", act: EventMark{Seq: 9, At: base.Add(time.Hour)}, start: EventMark{Seq: 10, At: base}, after: false},
		{name: "no sequence and a later time", act: EventMark{At: base.Add(time.Hour)}, start: EventMark{Seq: 10, At: base}, after: false},
		{name: "no sequence against a read with no time", act: EventMark{At: base.Add(time.Hour)}, start: EventMark{Seq: 10}, after: false},
		{name: "times alone", act: EventMark{At: base.Add(time.Minute)}, start: EventMark{At: base}, after: true},
		{name: "nothing known", act: EventMark{}, start: EventMark{}, after: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.act.After(tc.start); got != tc.after {
				t.Fatalf("%#v after %#v = %v, want %v", tc.act, tc.start, got, tc.after)
			}
		})
	}
}

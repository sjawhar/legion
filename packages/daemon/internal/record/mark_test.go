package record

import (
	"testing"
	"time"
)

// Two sequences compare as sequences. A mark without one, which a read of a Dispatch that does not
// give the sequence produces, is ordered against a sequenced start by their times when both carry
// one, as before Dispatch kept the sequence; against a registration's read, which has a sequence
// and no time, it is never after. Two marks without a sequence fall back to their times.
func TestAnActWithNoSequenceIsOrderedByTimeAgainstASequencedStart(t *testing.T) {
	base := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		act   EventMark
		start EventMark
		after bool
	}{
		{name: "a later sequence", act: EventMark{Seq: 11}, start: EventMark{Seq: 10}, after: true},
		{name: "an earlier sequence with a later time", act: EventMark{Seq: 9, At: base.Add(time.Hour)}, start: EventMark{Seq: 10, At: base}, after: false},
		{name: "no sequence and a later time", act: EventMark{At: base.Add(time.Hour)}, start: EventMark{Seq: 10, At: base}, after: true},
		{name: "no sequence and an earlier time", act: EventMark{At: base.Add(-time.Hour)}, start: EventMark{Seq: 10, At: base}, after: false},
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

// Two reads of one review merge into the fuller mark, whichever gave less.
func TestMergedKeepsWhatEitherReadGave(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if got := (EventMark{At: at}).Merged(EventMark{Seq: 9, At: at}); got != (EventMark{Seq: 9, At: at}) {
		t.Fatalf("time-only read merged with the sequenced one = %#v, want both parts", got)
	}
	if got := (EventMark{Seq: 9}).Merged(EventMark{At: at}); got != (EventMark{Seq: 9, At: at}) {
		t.Fatalf("sequence-only read merged with the timed one = %#v, want both parts", got)
	}
	if got := (EventMark{Seq: 9, At: at}).Merged(EventMark{Seq: 7, At: at.Add(time.Hour)}); got != (EventMark{Seq: 9, At: at}) {
		t.Fatalf("a full mark merged = %#v, want it kept as it was", got)
	}
}

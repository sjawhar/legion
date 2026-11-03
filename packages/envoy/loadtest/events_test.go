package main

import (
	"errors"
	"maps"
	"testing"
)

func TestEventMixPresetsResolve(t *testing.T) {
	cases := map[string]map[string]float64{
		"production-2026-10-26": {"comment": 3, "anchored": 33, "reply": 35, "edit": 22, "upload": 7},
		"report-2026-10-27":     {"comment": 50, "anchored": 30, "edit": 20},
		"reply=1,upload=3":      {"reply": 1, "upload": 3},
	}
	for spec, want := range cases {
		mix, err := parseEventMix(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if !maps.Equal(mix.Weights, want) {
			t.Errorf("%s resolves to %v, want %v", spec, mix.Weights, want)
		}
	}
}

func TestEventMixRefusals(t *testing.T) {
	cases := map[string]string{
		"comment=50,bogus=10":   "bogus",
		"comment=50,edit=-5":    "edit",
		"comment=1,comment=2":   "comment",
		"anchored=lots":         "anchored",
		"production-2026-10-99": "production-2026-10-99",
	}
	for spec, kind := range cases {
		_, err := parseEventMix(spec)
		var mixErr *eventMixError
		if !errors.As(err, &mixErr) {
			t.Fatalf("%s: accepted or refused without naming an item: %v", spec, err)
		}
		if mixErr.Kind != kind {
			t.Errorf("%s: refusal names %q, want %q", spec, mixErr.Kind, kind)
		}
	}
	if _, err := parseEventMix("comment=0,edit=0"); err == nil {
		t.Error("a mix whose weights are all zero was accepted")
	}
}

func TestEventMixPicksInProportion(t *testing.T) {
	mix, err := parseEventMix("report-2026-10-27")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := range 1000 {
		counts[mix.pick(float64(i)/1000)]++
	}
	want := map[string]int{"comment": 500, "anchored": 300, "edit": 200}
	if !maps.Equal(counts, want) {
		t.Errorf("1,000 evenly spaced picks give %v, want %v", counts, want)
	}
}

func TestCurrentRevision(t *testing.T) {
	n, ok := currentRevision("# Spec\n\nRevision token r41\n\n| Revision token r9 |")
	if !ok || n != 41 {
		t.Fatalf("currentRevision = %d, %t; want the first token, 41", n, ok)
	}
	if _, ok := currentRevision("# Spec without a token"); ok {
		t.Fatal("currentRevision found a token in a spec without one")
	}
}

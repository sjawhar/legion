package core

import (
	"strings"
	"testing"
)


func TestBuildMetaMarkerHasFrontmatterDelimiters(t *testing.T) {
	got := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: "R"})
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("missing leading '---' delimiter: %q", got)
	}
	if !strings.HasSuffix(got, "---\n") {
		t.Errorf("missing trailing '---' delimiter: %q", got)
	}
}

func TestBuildMetaMarkerEmitsUrgency(t *testing.T) {
	got := BuildMetaMarker(MetaMarker{Urgency: UrgencyHigh, RequestID: "R"})
	if !strings.Contains(got, "urgency: high") {
		t.Errorf("urgency missing: %q", got)
	}
}


func TestParseMetaMarkerEmpty(t *testing.T) {
	if ParseMetaMarker("plain body") != nil {
		t.Errorf("expected nil for body without marker")
	}
}


func TestBuildMetaMarkerEmitsRequestID(t *testing.T) {
	got := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: "req-abc123"})
	if !strings.Contains(got, "requestId: req-abc123") {
		t.Errorf("requestId missing: %q", got)
	}
}

func TestBuildMetaMarkerOmitsAskWhenEmpty(t *testing.T) {
	got := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: "R"})
	if strings.Contains(got, "ask:") {
		t.Errorf("ask field leaked into output: %q", got)
	}
}

func TestBuildMetaMarkerSerializesAsk(t *testing.T) {
	ask := []QuestionInfo{
		{Question: "Canary?", Header: "Canary", Options: []QuestionOption{{Label: "yes"}}},
	}
	got := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: "R", Ask: ask})
	if !strings.Contains(got, "ask:") {
		t.Errorf("ask key missing: %q", got)
	}
	if !strings.Contains(got, "Canary?") {
		t.Errorf("question text missing: %q", got)
	}
}

func TestParseMetaMarkerReadsUrgencyFromFrontmatter(t *testing.T) {
	body := "---\nurgency: high\nrequestId: R\n---\n\nbody text"
	parsed := ParseMetaMarker(body)
	if parsed == nil {
		t.Fatalf("expected non-nil parse")
	}
	if parsed.Urgency != UrgencyHigh {
		t.Errorf("urgency: got %q want high", parsed.Urgency)
	}
}

func TestParseMetaMarkerReadsRequestID(t *testing.T) {
	body := "---\nurgency: med\nrequestId: req-7\n---\n"
	parsed := ParseMetaMarker(body)
	if parsed == nil {
		t.Fatalf("nil parse")
	}
	if parsed.RequestID != "req-7" {
		t.Errorf("RequestID: got %q want req-7", parsed.RequestID)
	}
}

func TestParseMetaMarkerReadsAsk(t *testing.T) {
	body := "---\nurgency: med\nrequestId: R\nask:\n  - question: Color?\n    header: Color\n    options:\n      - label: blue\n      - label: red\n---\n"
	parsed := ParseMetaMarker(body)
	if parsed == nil {
		t.Fatalf("nil parse")
	}
	if len(parsed.Ask) != 1 {
		t.Fatalf("len(Ask): got %d want 1", len(parsed.Ask))
	}
	if parsed.Ask[0].Question != "Color?" {
		t.Errorf("Question: got %q", parsed.Ask[0].Question)
	}
	if len(parsed.Ask[0].Options) != 2 {
		t.Errorf("Options: got %d want 2", len(parsed.Ask[0].Options))
	}
	if parsed.Ask[0].Options[0].Label != "blue" {
		t.Errorf("Options[0].Label: got %q", parsed.Ask[0].Options[0].Label)
	}
}

func TestParseMetaMarkerRejectsUnknownUrgency(t *testing.T) {
	body := "---\nurgency: nuclear\nrequestId: R\n---\n"
	if parsed := ParseMetaMarker(body); parsed != nil {
		t.Errorf("expected nil for unknown urgency, got %+v", parsed)
	}
}

func TestParseMetaMarkerRejectsMissingRequestID(t *testing.T) {
	body := "---\nurgency: med\n---\n"
	if parsed := ParseMetaMarker(body); parsed != nil {
		t.Errorf("expected nil for missing requestId, got %+v", parsed)
	}
}

func TestBuildThreadBodyComposesFrontmatterSubjectBody(t *testing.T) {
	marker := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: "R"})
	got := BuildThreadBody(marker, "Subject", "Body content.")
	// Frontmatter already ends with "---\n"; the canonical layout is:
	// "---\n…---\n\n**Subject**\n\nBody content."  — one blank line between
	// frontmatter and subject, one between subject and body.
	want := marker + "\n**Subject**\n\nBody content."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestMetaMarkerRoundTrip(t *testing.T) {
	multiple := true
	original := MetaMarker{
		Urgency:   UrgencyBlocking,
		RequestID: "req-99",
		Ask: []QuestionInfo{
			{
				Question: "Color?",
				Header:   "Color",
				Options: []QuestionOption{
					{Label: "blue", Description: "ocean"},
					{Label: "red"},
				},
				Multiple: &multiple,
			},
		},
	}
	marker := BuildMetaMarker(original)
	parsed := ParseMetaMarker(marker)
	if parsed == nil {
		t.Fatalf("nil parse for %q", marker)
	}
	if parsed.Urgency != original.Urgency || parsed.RequestID != original.RequestID {
		t.Errorf("scalar mismatch: %+v vs %+v", parsed, original)
	}
	if len(parsed.Ask) != 1 || parsed.Ask[0].Question != "Color?" {
		t.Errorf("ask mismatch: %+v", parsed.Ask)
	}
	if len(parsed.Ask[0].Options) != 2 || parsed.Ask[0].Options[0].Description != "ocean" {
		t.Errorf("options mismatch: %+v", parsed.Ask[0].Options)
	}
	if parsed.Ask[0].Multiple == nil || !*parsed.Ask[0].Multiple {
		t.Errorf("Multiple should round-trip true, got %+v", parsed.Ask[0].Multiple)
	}
}

package core

import (
	"strings"
	"testing"
)

func mustBuildMeta(t *testing.T, m MetaMarker) string {
	t.Helper()
	got, err := BuildMetaMarker(m)
	if err != nil {
		t.Fatalf("BuildMetaMarker: %v", err)
	}
	return got
}

func TestBuildMetaMarkerIsAnHTMLComment(t *testing.T) {
	got := mustBuildMeta(t, MetaMarker{RequestID: "R", Urgency: UrgencyMed})
	if !strings.HasPrefix(got, "<!-- dispatch:thread\n") {
		t.Errorf("missing opening: %q", got)
	}
	if !strings.HasSuffix(got, "\n-->") {
		t.Errorf("closing --> must sit alone on the last line: %q", got)
	}
	if strings.Count(got, "-->") != 1 {
		t.Errorf("exactly one comment terminator expected: %q", got)
	}
	if strings.Contains(got, "---") {
		t.Errorf("front matter delimiters must not appear: %q", got)
	}
}

func TestBuildMetaMarkerKeyOrder(t *testing.T) {
	got := mustBuildMeta(t, MetaMarker{
		RequestID: "R",
		Urgency:   UrgencyHigh,
		Origin:    &Origin{Host: "omp"},
		Ask:       []QuestionInfo{{AskID: "R", Question: "Q?"}},
	})
	for _, pair := range [][2]string{{"requestId: R", "urgency: high"}, {"urgency: high", "origin:"}, {"origin:", "ask:"}} {
		if strings.Index(got, pair[0]) > strings.Index(got, pair[1]) {
			t.Errorf("%q must precede %q in %q", pair[0], pair[1], got)
		}
	}
}

func TestBuildMetaMarkerOmitsOriginAndAskWhenEmpty(t *testing.T) {
	got := mustBuildMeta(t, MetaMarker{RequestID: "R", Urgency: UrgencyMed})
	if strings.Contains(got, "origin:") || strings.Contains(got, "ask:") {
		t.Errorf("empty origin/ask leaked: %q", got)
	}
}

func TestBuildMetaMarkerSerializesSessionIdentityAndOmitsEmptyOriginFields(t *testing.T) {
	got := mustBuildMeta(t, MetaMarker{
		RequestID: "R",
		Urgency:   UrgencyMed,
		Origin: &Origin{
			Host:         "omp",
			Cwd:          "/home/ubuntu/legion",
			SessionID:    "01a05ac6-3b19-7000-9d2b-1e5f0a6c2b7d",
			SessionTitle: "pm: e2e submitter identity",
		},
	})
	for _, want := range []string{"host: omp", "cwd: /home/ubuntu/legion", "sessionId: 01a05ac6-3b19-7000-9d2b-1e5f0a6c2b7d", "sessionTitle: 'pm: e2e submitter identity'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, absent := range []string{"machine:", "tmux:", "pane:"} {
		if strings.Contains(got, absent) {
			t.Errorf("empty field %q leaked: %q", absent, got)
		}
	}
}

func TestParseMetaMarkerReadsHTMLComment(t *testing.T) {
	body := "<!-- dispatch:thread\nrequestId: req-7\nurgency: high\norigin:\n    host: opencode\n    sessionId: ses_1\n    sessionTitle: fix login\nask:\n    - askId: req-7\n      question: Color?\n      options:\n        - label: blue\n-->\n\n**Subject**\n\n## Context\n\nc\n\n## Question\n\nq"
	parsed := ParseMetaMarker(body)
	if parsed == nil {
		t.Fatal("nil parse")
	}
	if parsed.RequestID != "req-7" || parsed.Urgency != UrgencyHigh {
		t.Errorf("scalars: %+v", parsed)
	}
	if parsed.Origin == nil || parsed.Origin.Host != "opencode" || parsed.Origin.SessionID != "ses_1" || parsed.Origin.SessionTitle != "fix login" {
		t.Errorf("origin: %+v", parsed.Origin)
	}
	if len(parsed.Ask) != 1 || parsed.Ask[0].AskID != "req-7" || parsed.Ask[0].Question != "Color?" {
		t.Errorf("ask: %+v", parsed.Ask)
	}
}

func TestParseMetaMarkerReadsLegacyFrontmatter(t *testing.T) {
	body := "---\nurgency: med\nrequestId: R\norigin:\n  host: omp\n  tmux: main:3.0\n  pane: '%840'\nask:\n  - question: Color?\n    options:\n      - label: blue\n---\n\n**Subject**"
	parsed := ParseMetaMarker(body)
	if parsed == nil {
		t.Fatal("legacy front matter must still parse")
	}
	if parsed.RequestID != "R" || parsed.Urgency != UrgencyMed {
		t.Errorf("scalars: %+v", parsed)
	}
	if parsed.Origin == nil || parsed.Origin.Pane != "%840" {
		t.Errorf("origin: %+v", parsed.Origin)
	}
	if len(parsed.Ask) != 1 || parsed.Ask[0].AskID != "" {
		t.Errorf("legacy ask carries no askId: %+v", parsed.Ask)
	}
}

func TestParseMetaMarkerRejectsOtherKinds(t *testing.T) {
	cases := map[string]string{
		"plain body":               "plain body",
		"legacy answer comment":    "---\nkind: answer\nforThread: 1\nanswers: [[a]]\n---\n",
		"legacy urgency comment":   "---\nkind: urgency\nurgency: high\n---\n",
		"html ask marker":          "<!-- dispatch:ask\nrequestId: R\n-->\n",
		"unknown urgency":          "<!-- dispatch:thread\nrequestId: R\nurgency: nuclear\n-->",
		"missing requestId":        "<!-- dispatch:thread\nurgency: med\n-->",
		"unterminated comment":     "<!-- dispatch:thread\nrequestId: R\nurgency: med\n",
		"marker not at body start": "\n<!-- dispatch:thread\nrequestId: R\nurgency: med\n-->",
	}
	for name, body := range cases {
		if got := ParseMetaMarker(body); got != nil {
			t.Errorf("%s: expected nil, got %+v", name, got)
		}
	}
}

func TestParseAskMarker(t *testing.T) {
	body := "<!-- dispatch:ask\nrequestId: 7b1e\norigin:\n    host: claude\n    sessionId: abc\nask:\n    - askId: 7b1e\n      question: Which?\n    - askId: 7b1e.1\n      question: How many?\n-->\n\n## Context\n\nc\n\n## Question\n\nq"
	parsed := ParseAskMarker(body)
	if parsed == nil {
		t.Fatal("nil parse")
	}
	if parsed.RequestID != "7b1e" || parsed.Origin == nil || parsed.Origin.SessionID != "abc" {
		t.Errorf("parsed: %+v", parsed)
	}
	if len(parsed.Ask) != 2 || parsed.Ask[1].AskID != "7b1e.1" {
		t.Errorf("ask: %+v", parsed.Ask)
	}
	if ParseAskMarker("<!-- dispatch:thread\nrequestId: R\nurgency: med\n-->") != nil {
		t.Error("a thread marker is not an ask marker")
	}
	if ParseAskMarker("<!-- dispatch:ask\norigin:\n    host: omp\n-->") != nil {
		t.Error("an ask marker without requestId is invalid")
	}
}

func TestAskIDFor(t *testing.T) {
	if got := AskIDFor("abcd", 0); got != "abcd" {
		t.Errorf("index 0 reuses the request id, got %q", got)
	}
	if got := AskIDFor("abcd", 2); got != "abcd.2" {
		t.Errorf("index 2: got %q", got)
	}
	withIDs := WithAskIDs([]QuestionInfo{{Question: "a"}, {Question: "b"}}, "abcd")
	if withIDs[0].AskID != "abcd" || withIDs[1].AskID != "abcd.1" {
		t.Errorf("WithAskIDs: %+v", withIDs)
	}
	if WithAskIDs(nil, "abcd") != nil || WithAskIDs([]QuestionInfo{}, "abcd") != nil {
		t.Error("empty ask stays nil so the marker omits it")
	}
}

func TestMetaMarkerRoundTripWithEveryField(t *testing.T) {
	multiple := true
	original := MetaMarker{
		RequestID: "req-99",
		Urgency:   UrgencyBlocking,
		Origin: &Origin{
			Host: "omp", Machine: "example-host", Cwd: "/home/ubuntu/legion", Tmux: "main:3.0", Pane: "%840",
			SessionID: "01a05ac6-3b19-7000-9d2b-1e5f0a6c2b7d", SessionTitle: "pm: e2e submitter identity",
		},
		Ask: []QuestionInfo{{
			AskID: "req-99", Question: "Color?", Header: "Color",
			Options: []QuestionOption{{Label: "blue", Description: "ocean"}, {Label: "red"}}, Multiple: &multiple,
		}},
	}
	parsed := ParseMetaMarker(mustBuildMeta(t, original))
	if parsed == nil {
		t.Fatal("nil parse")
	}
	if parsed.RequestID != original.RequestID || parsed.Urgency != original.Urgency {
		t.Errorf("scalars: %+v", parsed)
	}
	if parsed.Origin == nil || *parsed.Origin != *original.Origin {
		t.Errorf("origin: %+v vs %+v", parsed.Origin, original.Origin)
	}
	if len(parsed.Ask) != 1 || parsed.Ask[0].AskID != "req-99" || parsed.Ask[0].Options[0].Description != "ocean" || parsed.Ask[0].Multiple == nil || !*parsed.Ask[0].Multiple {
		t.Errorf("ask: %+v", parsed.Ask)
	}
}

// Values a session or a human produces can contain the HTML comment
// terminator. The marker must still be one comment and still parse back to
// the original text.
func TestMarkerEscapesCommentDelimitersInValues(t *testing.T) {
	cases := []struct {
		name   string
		origin Origin
	}{
		{"arrow in session title", Origin{SessionTitle: "migrate A --> B"}},
		{"comment opener in title", Origin{SessionTitle: "<!-- not a comment"}},
		{"bang close in cwd", Origin{Cwd: "/tmp/x--!>y"}},
		{"colon-space and hash in cwd", Origin{Cwd: "/home/ubuntu/notes: issue #42", Tmux: "legion-2.0:12.1"}},
		{"windows-style cwd", Origin{Cwd: "C:/Users/sami/legion"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origin := tc.origin
			marker := mustBuildMeta(t, MetaMarker{RequestID: "R", Urgency: UrgencyMed, Origin: &origin})
			if strings.Count(marker, "-->") != 1 || !strings.HasSuffix(marker, "\n-->") {
				t.Fatalf("marker is not a single HTML comment: %q", marker)
			}
			if strings.Count(marker, "<!--") != 1 {
				t.Fatalf("comment opener leaked into the body: %q", marker)
			}
			if strings.Contains(marker, "--!>") {
				t.Fatalf("--!> leaked: %q", marker)
			}
			parsed := ParseMetaMarker(marker)
			if parsed == nil || parsed.Origin == nil || *parsed.Origin != tc.origin {
				t.Errorf("round trip lost data: got %+v want %+v\nmarker: %q", parsed, tc.origin, marker)
			}
		})
	}
	ask := []QuestionInfo{{AskID: "R", Question: "A --> B?", Options: []QuestionOption{{Label: "-->", Description: "<!-- x"}}}}
	marker := mustBuildMeta(t, MetaMarker{RequestID: "R", Urgency: UrgencyMed, Ask: ask})
	parsed := ParseMetaMarker(marker)
	if parsed == nil || parsed.Ask[0].Question != "A --> B?" || parsed.Ask[0].Options[0].Label != "-->" || parsed.Ask[0].Options[0].Description != "<!-- x" {
		t.Errorf("ask round trip: %+v from %q", parsed, marker)
	}
}

func TestBuildThreadBodyLayout(t *testing.T) {
	marker := mustBuildMeta(t, MetaMarker{RequestID: "R", Urgency: UrgencyMed})
	got := BuildThreadBody(marker, "Subject", "Context text.", "Question text.")
	want := marker + "\n\n**Subject**\n\n## Context\n\nContext text.\n\n## Question\n\nQuestion text."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if ParseMetaMarker(got) == nil {
		t.Error("the thread body must start with a parsable marker")
	}
}

func TestBuildFollowUpBodyLayout(t *testing.T) {
	marker, err := BuildAskMarker(AskMarker{RequestID: "F"})
	if err != nil {
		t.Fatal(err)
	}
	got := BuildFollowUpBody(marker, "More context.", "Revised question.")
	want := marker + "\n\n## Context\n\nMore context.\n\n## Question\n\nRevised question."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if ParseAskMarker(got) == nil {
		t.Error("the follow-up body must start with a parsable ask marker")
	}
}

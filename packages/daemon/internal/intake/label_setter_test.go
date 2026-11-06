package intake

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// logEvent is one event of an issue's Dispatch event log as dispatch.Client.EventsAfter serves it,
// which is the shape the durable stream carries: type typ at seq, written by an actor of kind,
// with payload as its payload.
func logEvent(seq int, typ, kind string, payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":"LEGION-755","seq":%d,"type":%q,"actor":{"kind":%q,"id":"%s-1"},"notify":true,"created_at":"2026-10-14T17:00:%02dZ","payload":%s}`,
		seq, seq, typ, kind, kind, seq, payload))
}

// issueLogEvent is an issue.created or issue.updated event whose issue carries labels.
func issueLogEvent(seq int, typ, kind string, labels ...string) json.RawMessage {
	quoted := make([]string, len(labels))
	for i, label := range labels {
		quoted[i] = fmt.Sprintf("%q", label)
	}
	return logEvent(seq, typ, kind, fmt.Sprintf(`{"key":"LEGION-755","status":"triage","title":"Queue a person's handover","labels":[%s]}`, strings.Join(quoted, ",")))
}

// The label's setter is the actor of the event that added the legion label (spec R2): the newest
// issue.created or issue.updated event whose labels carry it while the previous such event's did
// not, read oldest to newest. Events of other kinds count for nothing, so a person's comment
// between a session's label and a later edit does not make the person the setter; a label removed
// and set again names whoever set it last; and the match on the label ignores case, as
// dispatch.CarriesLegionLabel does.
func TestLabelSetterIsTheActorOfTheEventThatAddedTheLabel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []json.RawMessage
		want   string
	}{
		{
			name:   "created with the label by a person",
			events: []json.RawMessage{issueLogEvent(1, "issue.created", "user", "legion")},
			want:   "user",
		},
		{
			name: "created unlabelled, then labelled by a session",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user"),
				issueLogEvent(2, "issue.updated", "session", "legion"),
			},
			want: "session",
		},
		{
			name: "labelled by a person, removed, labelled again by a session",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user", "legion"),
				issueLogEvent(2, "issue.updated", "user"),
				issueLogEvent(3, "issue.updated", "session", "legion"),
			},
			want: "session",
		},
		{
			name: "labelled by a person, removed, by a session, removed, by a person again",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user", "legion"),
				issueLogEvent(2, "issue.updated", "user"),
				issueLogEvent(3, "issue.updated", "session", "legion"),
				issueLogEvent(4, "issue.updated", "session"),
				issueLogEvent(5, "issue.updated", "user", "legion"),
			},
			want: "user",
		},
		{
			name: "the label spelled Legion counts",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user"),
				issueLogEvent(2, "issue.updated", "session", "bug", "Legion"),
			},
			want: "session",
		},
		{
			name: "a person's comment between a session's label and the person's unrelated label counts for nothing",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "session"),
				issueLogEvent(2, "issue.updated", "session", "legion"),
				logEvent(3, "comment.created", "user", `{"id":"c-1","body":"Looks right."}`),
				issueLogEvent(4, "issue.updated", "user", "legion", "docs"),
			},
			want: "session",
		},
		{
			name: "a session's edit that keeps a person's label leaves the person the setter",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user", "legion"),
				issueLogEvent(2, "issue.updated", "session", "legion"),
			},
			want: "user",
		},
		{
			name: "an issue.closed carrying the label moves nothing",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "session"),
				logEvent(2, "issue.closed", "user", `{"key":"LEGION-755","status":"done","labels":["legion"]}`),
			},
			want: "",
		},
		{
			name: "no event added the label",
			events: []json.RawMessage{
				issueLogEvent(1, "issue.created", "user"),
				issueLogEvent(2, "issue.updated", "user", "bug"),
			},
			want: "",
		},
		{
			name:   "a payload with no labels field is unlabelled",
			events: []json.RawMessage{logEvent(1, "issue.created", "user", `{"key":"LEGION-755","status":"triage"}`)},
			want:   "",
		},
		{name: "an empty log", events: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LabelSetter(tc.events)
			if err != nil {
				t.Fatalf("LabelSetter: %v", err)
			}
			if got != tc.want {
				t.Fatalf("LabelSetter = %q, want %q", got, tc.want)
			}
		})
	}
}

// A log the daemon cannot read names no setter it can act on: an event that does not decode is
// an error naming its position, so the consumer delivers the fact again rather than treating the
// label as nobody's.
func TestLabelSetterRefusesAnEventItCannotDecode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []json.RawMessage
		want   string
	}{
		{
			name:   "an event that is not JSON",
			events: []json.RawMessage{issueLogEvent(1, "issue.created", "user"), json.RawMessage(`{not json`)},
			want:   "decode Dispatch event 2 of 2",
		},
		{
			name:   "an issue event whose payload is not an issue",
			events: []json.RawMessage{logEvent(1, "issue.created", "user", `"a string"`)},
			want:   "decode Dispatch issue payload of event 1 of 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LabelSetter(tc.events)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LabelSetter error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

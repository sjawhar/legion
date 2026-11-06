package going_test

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
)

// TestEventsAreTheContractsFramesAndEndOnlyOnEndAndError checks each constructor against the
// generated vocabulary: the frame's name, the JSON its data is written as, and whether a reader
// stops on it.
func TestEventsAreTheContractsFramesAndEndOnlyOnEndAndError(t *testing.T) {
	hit := going.Hit{Kind: "issue", ID: "INFRA-1279", Title: "Answer a question", Href: "/issues/INFRA-1279"}
	for _, tc := range []struct {
		event    going.Event
		wantName string
		wantData string
		terminal bool
	}{
		{going.Text("half an "), contracts.GoingEventText, `{"text":"half an "}`, false},
		{
			going.Item(hit),
			contracts.GoingEventItem,
			`{"kind":"issue","id":"INFRA-1279","href":"/issues/INFRA-1279","title":"Answer a question"}`,
			false,
		},
		{going.Cut(120), contracts.GoingEventCut, `{"words":120}`, false},
		{going.Changed(3), contracts.GoingEventChanged, `{"dropped":3}`, false},
		{going.None(), contracts.GoingEventNone, `{}`, false},
		{going.End(42, 2), contracts.GoingEventEnd, `{"words":42,"items":2}`, true},
		{
			going.Fault(contracts.GoingErrorAnswerFailed),
			contracts.GoingEventError,
			`{"code":"ANSWER_FAILED"}`,
			true,
		},
	} {
		t.Run(tc.wantName, func(t *testing.T) {
			if tc.event.Name != tc.wantName {
				t.Errorf("event name = %q, want %q", tc.event.Name, tc.wantName)
			}
			data, err := json.Marshal(tc.event.Data)
			if err != nil {
				t.Fatalf("marshal %s data: %v", tc.wantName, err)
			}
			if string(data) != tc.wantData {
				t.Errorf("event data = %s, want %s", data, tc.wantData)
			}
			if tc.event.Terminal() != tc.terminal {
				t.Errorf("Terminal() = %v, want %v", tc.event.Terminal(), tc.terminal)
			}
		})
	}
}

// TestHitKeyNamesAHitAcrossKinds: an id is unique only within its kind, so two hits of different
// kinds that share an id have two keys.
func TestHitKeyNamesAHitAcrossKinds(t *testing.T) {
	issue := going.Hit{Kind: "issue", ID: "42"}
	comment := going.Hit{Kind: "comment", ID: "42"}
	if got, want := issue.Key(), "issue:42"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if issue.Key() == comment.Key() {
		t.Errorf("two kinds sharing id 42 share the key %q", issue.Key())
	}
}

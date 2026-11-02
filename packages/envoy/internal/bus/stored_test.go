package bus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A stored-message read returns the first message at or after its sequence on a subject its filter
// matches, wildcards included, with the concrete subject, body and stored time; past the last one
// it is nats.ErrMsgNotFound, and with no stream nats.ErrStreamNotFound.
func TestNextStoredMessageReadsByFilterAfterASequence(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	publish := func(subject, body string) uint64 {
		t.Helper()
		ack, err := js.Publish(subject, []byte(body))
		if err != nil {
			t.Fatalf("publish on %s: %v", subject, err)
		}
		return ack.Sequence
	}
	publish("notifications.dispatch.issue.KEY-1.ask.opened", `{"n":1}`)
	publish("notifications.agent.ses_a", `{"n":2}`)
	third := publish("notifications.dispatch.issue.KEY-1.ask.answered", `{"n":3}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stored, err := client.NextStoredMessage(ctx, "notifications.dispatch.issue.KEY-1.>", 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored.Sequence != third || stored.Subject != "notifications.dispatch.issue.KEY-1.ask.answered" || string(stored.Data) != `{"n":3}` {
		t.Fatalf("stored = %+v, want seq %d on the answered subject with its body", stored, third)
	}
	if time.Since(stored.Time) > time.Minute || stored.Time.After(time.Now()) {
		t.Fatalf("stored time %s is not when it was stored", stored.Time)
	}

	if _, err := client.NextStoredMessage(ctx, "notifications.dispatch.issue.KEY-1.>", third+1); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("read past the last message = %v, want nats.ErrMsgNotFound", err)
	}

	if err := js.DeleteStream(Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	if _, err := client.NextStoredMessage(ctx, "notifications.agent.ses_a", 1); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("read without the stream = %v, want nats.ErrStreamNotFound", err)
	}
}

func TestSubjectContainsFollowsNATSWildcards(t *testing.T) {
	for _, tc := range []struct {
		pattern, subject string
		want             bool
	}{
		{"notifications.agent.>", "notifications.agent.ses_a", true},
		{"notifications.agent.>", "notifications.agent.ses_a.more", true},
		{"notifications.agent.>", "notifications.agent.*", true},
		{"notifications.agent.>", "notifications.agent.>", true},
		{"notifications.agent.>", "notifications.agent", false},
		{"notifications.agent.>", "notifications.>", false},
		{"notifications.dispatch.>", "notifications.dispatch.issue.KEY-1.>", true},
		{"notifications.dispatch.>", "notifications.team.x.y", false},
		{"notifications.*.x", "notifications.team.x", true},
		{"notifications.*.x", "notifications.*.x", true},
		{"notifications.*.x", "notifications.>", false},
		{"notifications.*.x", "notifications.team.x.y", false},
		{"notifications.team", "notifications.*", false},
		{"notifications.team", "notifications.team", true},
		{"notifications.team.x", "notifications.team", false},
		{">", "anything.at.all", true},
	} {
		if got := SubjectContains(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("SubjectContains(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func TestCheckFilterSubjectRefusesWhatNATSCannotFilterOn(t *testing.T) {
	for subject, refused := range map[string]bool{
		"notifications.dispatch.issue.KEY-1.>": false,
		"notifications.*.ses_a":                false,
		"":                                     true,
		"notifications..agent":                 true,
		"notifications.agent.":                 true,
		"notifications.>.agent":                true,
		"notifications.agent.a b":              true,
	} {
		err := CheckFilterSubject(subject)
		if refused != (err != nil) {
			t.Errorf("CheckFilterSubject(%q) = %v, want refused %v", subject, err, refused)
		}
		if err != nil && !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("CheckFilterSubject(%q) = %v, want ErrInvalidSubject", subject, err)
		}
	}
}

// ControllerNoticeRole pairs one project's controller notice lane with the role token its holder
// claims, from the project token the lane names, so the listener never builds the two strings
// side by side. Only a literal lane of the family pairs: a wildcard, a bare prefix, an issue
// notice and a longer subject name no role.
func TestControllerNoticeRolePairsALaneWithItsRoleToken(t *testing.T) {
	for topic, want := range map[string]string{
		"notifications.legion.ACME.controller":      "legion-ACME-controller",
		"notifications.legion.omp.controller":       "legion-omp-controller",
		"notifications.legion.ACME.LEGION-208":      "",
		"notifications.legion.*.controller":         "",
		"notifications.legion.>":                    "",
		"notifications.legion..controller":          "",
		"notifications.legion.controller":           "",
		"notifications.legion.ACME.x.controller":    "",
		"notifications.agent.ses_a":                 "",
		"notifications.role.legion-ACME-controller": "",
	} {
		role, lane := ControllerNoticeRole(topic)
		if role != want || lane != (want != "") {
			t.Errorf("ControllerNoticeRole(%q) = %q, %v; want %q, %v", topic, role, lane, want, want != "")
		}
	}
}

// What a session may register as an interest (/v1/interests/subscribe): anything but a topic
// reaching a private stream subject that is not its own: another session's agent subject by name
// or any wildcard over the agent subjects, a failed-delivery lane of an agent subject (its own
// included) or of a team topic, or a wildcard over either, a controller notice lane of a project
// whose controller role it does not hold, and wildcards reaching any of those. Its own agent
// subject is its own only when it is one literal subject, and so is the notice lane of the
// project whose controller role its claims name: the holder registers that lane because the
// notices reach it through its own consumer alone (INFRA-771), while another project's
// controller lane, and every other private subject, stay out of a controller's registration too.
// The cases are testdata/private-stream-subjects.json, which @legion/envoy-client's mirror of this
// rule runs too (stream-topics.test.ts): one file, read by both, so a case added for either is run
// by both on the next test run and neither can drift from a hand copy. Each case is a session id,
// a topic, the roles that session holds and the answer.
func TestReachesPrivateStreamSubjectBoundsInterests(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "private-stream-subjects.json"))
	if err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	var cases []struct {
		SessionID string   `json:"sessionID"`
		Topic     string   `json:"topic"`
		Roles     []string `json:"roles"`
		Want      bool     `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	// The file held 37 cases when this was written: a read that answers a handful of them, or
	// none, passes while covering nothing.
	if len(cases) < 37 {
		t.Fatalf("the shared cases hold %d rows, want at least 37", len(cases))
	}
	for _, tc := range cases {
		if got := ReachesPrivateStreamSubject(tc.SessionID, tc.Topic, tc.Roles); got != tc.Want {
			t.Errorf("ReachesPrivateStreamSubject(%q, %q, %v) = %v, want %v", tc.SessionID, tc.Topic, tc.Roles, got, tc.Want)
		}
	}
}

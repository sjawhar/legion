package bus

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// SubjectsCapture matches a literal topic against stream subjects by NATS's wildcard rules: `*` is
// one token, a last `>` one or more.
func TestSubjectsCaptureMatchesByNATSWildcardRules(t *testing.T) {
	for _, tc := range []struct {
		subjects []string
		topic    string
		want     bool
	}{
		{[]string{"notifications.github.>"}, "notifications.github.pr.1.checks", true},
		{[]string{"notifications.github.>"}, "notifications.github.pr", true},
		{[]string{"notifications.github.>"}, "notifications.github", false},
		{[]string{"notifications.github.>"}, "notifications.githubx.pr", false},
		{[]string{"notifications.*.checks"}, "notifications.github.checks", true},
		{[]string{"notifications.*.checks"}, "notifications.github.pr.checks", false},
		{[]string{"notifications.*.checks"}, "notifications.github", false},
		{[]string{"notifications.*"}, "notifications.team", true},
		{[]string{"notifications.*"}, "notifications.team.deploy", false},
		{[]string{"notifications.*.>"}, "notifications.team.deploy", true},
		{[]string{"notifications.*.>"}, "notifications.team", false},
		{[]string{"notifications.team.deploy"}, "notifications.team.deploy", true},
		{[]string{"notifications.team.deploy"}, "notifications.team.deployx", false},
		{[]string{">"}, "anything.at.all", true},
		{[]string{"notifications.agent.>", "notifications.github.>"}, "notifications.github.pr.1", true},
		{[]string{"notifications.agent.>", "notifications.github.>"}, "notifications.team.deploy", false},
		{nil, "notifications.github.pr.1", false},
	} {
		if got := SubjectsCapture(tc.subjects, tc.topic); got != tc.want {
			t.Errorf("SubjectsCapture(%q, %q) = %t, want %t", tc.subjects, tc.topic, got, tc.want)
		}
	}
}

// ReadStreamSubjects answers from the server, so a subject another build added to the stream after
// this binary started is in the list, though this binary does not compile it in.
func TestReadStreamSubjectsReadsASubjectAddedAtRuntime(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	const added = "notifications.twobuild.>"
	if subjects, err := ReadStreamSubjects(client.JS()); err != nil || !slices.Equal(subjects, streamSubjects) {
		t.Fatalf("ReadStreamSubjects at start = %q, %v; want %q", subjects, err, streamSubjects)
	}

	info, err := client.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	extended := info.Config
	extended.Subjects = append(slices.Clone(extended.Subjects), added)
	if _, err := client.JS().UpdateStream(&extended); err != nil {
		t.Fatalf("add %s to the stream: %v", added, err)
	}

	subjects, err := ReadStreamSubjects(client.JS())
	if err != nil {
		t.Fatalf("ReadStreamSubjects: %v", err)
	}
	if !slices.Contains(subjects, added) {
		t.Fatalf("ReadStreamSubjects = %q, missing %q", subjects, added)
	}
	if !SubjectsCapture(subjects, "notifications.twobuild.box-a.1") {
		t.Fatalf("the read subjects %q do not capture a topic under %s", subjects, added)
	}
}

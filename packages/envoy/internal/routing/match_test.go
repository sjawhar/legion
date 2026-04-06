package routing

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		topic   string
		ok      bool
	}{
		{pattern: "notifications.github.sjawhar.envoy.pr", topic: "notifications.github.sjawhar.envoy.pr", ok: true},
		{pattern: "notifications.slack.*.*.mention", topic: "notifications.slack.T123.C123.mention", ok: true},
		{pattern: "notifications.github.>", topic: "notifications.github.sjawhar.envoy.pr", ok: true},
		{pattern: "notifications.github.*.envoy.pr", topic: "notifications.github.sjawhar.envoy.pr", ok: true},
		{pattern: "notifications.github.*.envoy.issue", topic: "notifications.github.sjawhar.envoy.pr", ok: false},
	}
	for _, item := range cases {
		got := Match(item.pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", item.pattern, item.topic, item.ok, got)
		}
	}
}

func TestPerPRFiltering(t *testing.T) {
	pattern := "notifications.github.acme.widgets.pr.7706.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: PR 7706 base topic and subtopics
		{topic: "notifications.github.acme.widgets.pr.7706", ok: true},
		{topic: "notifications.github.acme.widgets.pr.7706.comment", ok: true},
		{topic: "notifications.github.acme.widgets.pr.7706.review", ok: true},
		// Should NOT match: different PR number
		{topic: "notifications.github.acme.widgets.pr.7707", ok: false},
		{topic: "notifications.github.acme.widgets.pr.7707.comment", ok: false},
		{topic: "notifications.github.acme.widgets.pr.7707.review", ok: false},
		// Should NOT match: issue topics
		{topic: "notifications.github.acme.widgets.issue.7706", ok: false},
		{topic: "notifications.github.acme.widgets.issue.7706.comment", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}
}

func TestGhostWisprTopicFiltering(t *testing.T) {
	// Wildcard subscription for all Ghost Wispr events
	pattern := "notifications.ghostwispr.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: ghostwispr events
		{topic: "notifications.ghostwispr.rec-abc123.transcript", ok: true},
		{topic: "notifications.ghostwispr.rec-abc123.summary", ok: true},
		{topic: "notifications.ghostwispr.rec-xyz789.transcript", ok: true},
		// Should NOT match: other source topics
		{topic: "notifications.github.sjawhar.legion.pr", ok: false},
		{topic: "notifications.slack.T123.C456.message", ok: false},
		{topic: "notifications.agent.ses_123", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}
}

func TestGhostWisprPerRecordingFiltering(t *testing.T) {
	// Subscription for a specific recording's events
	pattern := "notifications.ghostwispr.rec-abc123.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: target recording events
		{topic: "notifications.ghostwispr.rec-abc123.transcript", ok: true},
		{topic: "notifications.ghostwispr.rec-abc123.summary", ok: true},
		// Should NOT match: different recording
		{topic: "notifications.ghostwispr.rec-xyz789.transcript", ok: false},
		{topic: "notifications.ghostwispr.rec-xyz789.summary", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}
}

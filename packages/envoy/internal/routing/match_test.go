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

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

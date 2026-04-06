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

func TestPerSlackThreadFiltering(t *testing.T) {
	pattern := "notifications.slack.T123.C456.thread.1234567890_123456.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: target thread and its subtopics
		{topic: "notifications.slack.T123.C456.thread.1234567890_123456.message", ok: true},
		{topic: "notifications.slack.T123.C456.thread.1234567890_123456.mention", ok: true},
		// Should NOT match: different thread
		{topic: "notifications.slack.T123.C456.thread.9999999999_000000.message", ok: false},
		{topic: "notifications.slack.T123.C456.thread.9999999999_000000.mention", ok: false},
		// Should NOT match: channel-level topic (no thread segment)
		{topic: "notifications.slack.T123.C456.message", ok: false},
		{topic: "notifications.slack.T123.C456.mention", ok: false},
		// Should NOT match: different channel
		{topic: "notifications.slack.T123.C789.thread.1234567890_123456.message", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}

	// Prove normalized thread_ts (underscore) is a single NATS segment
	// while raw thread_ts (dot) would be two segments
	starPattern := "notifications.slack.T.C.thread.*.message"
	// Normalized ts = single segment: matches *
	if !Match(starPattern, "notifications.slack.T.C.thread.1234567890_123456.message") {
		t.Fatal("normalized thread_ts should match single-segment wildcard")
	}
	// Raw ts (dot) = two segments: does NOT match *
	if Match(starPattern, "notifications.slack.T.C.thread.1234567890.123456.message") {
		t.Fatal("raw dotted thread_ts should NOT match single-segment wildcard")
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

func TestWhatsappTopicFiltering(t *testing.T) {
	// Wildcard subscription for all messages on a phone number
	pattern := "notifications.whatsapp.15551234567.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: WhatsApp messages for this phone number
		{topic: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message", ok: true},
		{topic: "notifications.whatsapp.15551234567.group-abc@g.us.message", ok: true},
		{topic: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.status", ok: true},
		// Should NOT match: different phone number
		{topic: "notifications.whatsapp.15559876543.5551234567@s.whatsapp.net.message", ok: false},
		// Should NOT match: other source topics
		{topic: "notifications.slack.T123.C456.message", ok: false},
		{topic: "notifications.github.sjawhar.legion.pr", ok: false},
		{topic: "notifications.agent.ses_123", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}
}

func TestWhatsappPerJIDFiltering(t *testing.T) {
	// Subscription for a specific phone+JID combination using > wildcard
	// Note: JID contains dots (e.g., s.whatsapp.net) which create extra NATS subject levels
	// The > wildcard is required (not *) to match across these levels
	pattern := "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: events for this specific chat
		{topic: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message", ok: true},
		{topic: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.status", ok: true},
		// Should NOT match: different JID on same phone
		{topic: "notifications.whatsapp.15551234567.group-abc@g.us.message", ok: false},
		// Should NOT match: different phone number
		{topic: "notifications.whatsapp.15559876543.5551234567@s.whatsapp.net.message", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}
}

func TestCITopicFiltering(t *testing.T) {
	pattern := "notifications.github.acme.widgets.pr.42.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		{topic: "notifications.github.acme.widgets.pr.42.ci", ok: true},
		{topic: "notifications.github.acme.widgets.pr.42.comment", ok: true},
		{topic: "notifications.github.acme.widgets.pr.42.review", ok: true},
		{topic: "notifications.github.acme.widgets.pr.43.ci", ok: false},
		{topic: "notifications.github.acme.widgets.ci", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}
}

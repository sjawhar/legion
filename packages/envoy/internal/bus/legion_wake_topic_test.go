package bus

import "testing"

// The Legion wake topic is a new leaf under notifications.legion.>, which the stream already
// carries: a report published on it is retained with no stream change and no new grant, so every
// Legion daemon can read it off a durable of its own. It must not travel over core NATS, which
// would leave a daemon that was restarting at the moment of a wake with no record of it.
func TestTheLegionWakeTopicIsRetainedByTheStream(t *testing.T) {
	if LegionWakeTopic != "notifications.legion.wake" {
		t.Fatalf("LegionWakeTopic = %q, want notifications.legion.wake", LegionWakeTopic)
	}
	if !StreamCaptures(LegionWakeTopic) {
		t.Fatalf("the stream does not capture %q, so a daemon's durable would never read a wake", LegionWakeTopic)
	}
	if UsesCoreTransport(LegionWakeTopic) {
		t.Fatalf("%q travels over core NATS; a wake must be retained", LegionWakeTopic)
	}
}

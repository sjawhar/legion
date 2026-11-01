package kvwatchtest

import (
	"github.com/nats-io/nats.go"
)

// StreamBehind wraps kv so the stream state read through it (nats.KeyValue.Status, which
// bus.ReadStreamState asks for) reports lastSeq as the stream's last sequence while every other
// field, the creation time among them, stays the bucket's own. That is the stream a JetStream
// restore of an older snapshot leaves on a nats-server below v2.15, whose restore keeps the
// snapshot's creation time: a sequence space that went backwards under a bucket that was never
// recreated. nats-server v2.15 gives a restored stream a creation time of its own, so no server
// this module links leaves that state, and a test that needs it takes it from here. Every read and
// write of the bucket is the real one's; only the state's last sequence is the stubbed one.
func StreamBehind(kv nats.KeyValue, lastSeq uint64) nats.KeyValue {
	return streamBehindKV{KeyValue: kv, lastSeq: lastSeq}
}

type streamBehindKV struct {
	nats.KeyValue

	lastSeq uint64
}

// Status answers with the bucket's own status, its stream's last sequence rewound. Each Status is
// built from a stream info request of its own (nats.go kv.go:1301-1308), so the info this changes
// is read by this caller alone.
func (k streamBehindKV) Status() (nats.KeyValueStatus, error) {
	status, err := k.KeyValue.Status()
	if err != nil {
		return nil, err
	}
	bucket, ok := status.(*nats.KeyValueBucketStatus)
	if !ok {
		return status, nil
	}
	bucket.StreamInfo().State.LastSeq = k.lastSeq
	return bucket, nil
}

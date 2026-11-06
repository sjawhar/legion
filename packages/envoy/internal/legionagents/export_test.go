package legionagents

import (
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// WrapBucket replaces the store's bucket handle with the one wrap builds over it, so a test can
// hold or fail a call NATS would answer: two writes that both read before either claims, or a
// write that ends partway.
func (s *Store) WrapBucket(wrap func(nats.KeyValue) nats.KeyValue) {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	s.kv = bus.NewKeyValue(wrap(s.kv.Legacy()), s.kv.WatchHandle(), s.kv.Streams())
}

// WrapWatch replaces the handle a listing's watch is built on with the one wrap builds over it, so
// a test can stall a scan NATS would answer: a watch that delivers no key and no
// end-of-existing-keys marker.
func (s *Store) WrapWatch(wrap func(jetstream.KeyValue) jetstream.KeyValue) {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	s.kv = bus.NewKeyValue(s.kv.Legacy(), wrap(s.kv.WatchHandle()), s.kv.Streams())
}

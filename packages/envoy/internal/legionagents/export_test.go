package legionagents

import (
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// WrapBucket replaces the store's bucket handle with the one wrap builds over it, so a test can
// hold or fail a call NATS would answer: two writes that both read before either claims, or a
// write that ends partway.
func (s *Store) WrapBucket(wrap func(nats.KeyValue) nats.KeyValue) {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	s.kv = bus.KeyValue{KeyValue: wrap(s.kv.KeyValue)}
}

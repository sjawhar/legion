package kvwatch

import (
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Op is how a record changed its key: a put, or a delete or purge marker.
type Op uint8

const (
	// OpPut sets the key's value.
	OpPut Op = iota
	// OpDelete is a delete marker.
	OpDelete
	// OpPurge is a purge marker, which also stands for a record the server removed for its TTL
	// (a "MaxAge" or "Purge" marker reason, which the jetstream watcher reads as a purge).
	OpPurge
)

// Entry is one record of a bucket as a cache applies it (Cache.Apply). The watcher delivers the
// jetstream package's entries, as a store's history read does (bus.KeyValue.ReadHistory), while a
// store's Get goes through the legacy nats.KeyValue, whose entries are another type, so both convert
// into this one and a cache keeps one apply path for both (JetStreamEntry, LegacyEntry). Value
// shares the read entry's bytes.
type Entry struct {
	Key      string
	Value    []byte
	Revision uint64
	// Created is when the record was stored, on the server's clock.
	Created time.Time
	Op      Op
}

// LegacyEntry is entry, read through a legacy nats.KeyValue, as an Entry.
func LegacyEntry(entry nats.KeyValueEntry) Entry {
	op := OpPut
	switch entry.Operation() {
	case nats.KeyValueDelete:
		op = OpDelete
	case nats.KeyValuePurge:
		op = OpPurge
	}
	return Entry{Key: entry.Key(), Value: entry.Value(), Revision: entry.Revision(), Created: entry.Created(), Op: op}
}

// JetStreamEntry is entry, delivered by a jetstream watcher or read by bus.KeyValue.ReadHistory, as
// an Entry.
func JetStreamEntry(entry jetstream.KeyValueEntry) Entry {
	op := OpPut
	switch entry.Operation() {
	case jetstream.KeyValueDelete:
		op = OpDelete
	case jetstream.KeyValuePurge:
		op = OpPurge
	}
	return Entry{Key: entry.Key(), Value: entry.Value(), Revision: entry.Revision(), Created: entry.Created(), Op: op}
}

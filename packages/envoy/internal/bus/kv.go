package bus

import (
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// KeyValue is a KV handle that checks a key before nats.go builds a subject from it, since nats.go
// checks only the characters a key may hold: a key whose subject would reach past maxSubjectBytes is
// ErrTooLarge, and one NATS would not accept in a subject (an empty token, `a..b`, which nats.go
// allows, or whitespace) is ErrInvalidSubject, each refused before anything is sent. A write (Put,
// PutString, Create, Update) is held to the longest subject any call on its key builds, a watcher's
// create request, so a key written through it stays readable, watchable and deletable. Any other
// call is held only to the subject it builds itself, so a key an earlier build stored past the write
// bound still lists and deletes, and still reads while its get fits: a store serves it or skips it,
// and a reaper removes it. EnsureKeyValue and OpenKeyValue are how the listener and Dispatch open
// every bucket; a test wraps a handle it injects faults through as KeyValue{KeyValue: handle}.
type KeyValue struct {
	nats.KeyValue
}

// EnsureKeyValue opens the KV bucket config names, creating it with config when it does not exist.
func EnsureKeyValue(js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	kv, err := js.KeyValue(config.Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(config)
	}
	if err != nil {
		return KeyValue{}, err
	}
	return KeyValue{kv}, nil
}

// OpenKeyValue opens the existing KV bucket named bucket and creates nothing, as a rebuild does.
func OpenKeyValue(js nats.JetStreamContext, bucket string) (KeyValue, error) {
	kv, err := js.KeyValue(bucket)
	if err != nil {
		return KeyValue{}, err
	}
	return KeyValue{kv}, nil
}

// The subjects nats.go builds from a key of bucket, each as the bytes it puts around the key. A put
// or a delete publishes to `$KV.<bucket>.<key>`; a get is a direct get,
// `$JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key>`; a watch or a history creates a watcher on the
// key, `$JS.API.CONSUMER.CREATE.KV_<bucket>.<name>.$KV.<bucket>.<key>`, whose consumer name nats.go
// makes eight bytes, the longest of the three. A get by revision names no key in its subject.
func putOverhead(bucket string) int {
	return len("$KV..") + len(bucket)
}

func getOverhead(bucket string) int {
	return len("$JS.API.DIRECT.GET.KV_.") + len(bucket) + putOverhead(bucket)
}

func watchOverhead(bucket string) int {
	return len("$JS.API.CONSUMER.CREATE.KV_..00000000") + len(bucket) + putOverhead(bucket)
}

// check refuses key when the subject a call puts overhead bytes around it would be refused.
func (kv KeyValue) check(key string, overhead int) error {
	if limit := maxSubjectBytes - overhead; len(key) > limit {
		return fmt.Errorf("%w: a key of %d bytes, past %d", ErrTooLarge, len(key), limit)
	}
	if !validSubject(key) {
		return fmt.Errorf("%w: key %q", ErrInvalidSubject, key)
	}
	return nil
}

func (kv KeyValue) write(key string) error {
	return kv.check(key, watchOverhead(kv.Bucket()))
}

func (kv KeyValue) Get(key string) (nats.KeyValueEntry, error) {
	if err := kv.check(key, getOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	return kv.KeyValue.Get(key)
}

func (kv KeyValue) Put(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.Put(key, value)
}

func (kv KeyValue) PutString(key string, value string) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.PutString(key, value)
}

func (kv KeyValue) Create(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.Create(key, value)
}

func (kv KeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.Update(key, value, last)
}

func (kv KeyValue) Delete(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	return kv.KeyValue.Delete(key, opts...)
}

func (kv KeyValue) Purge(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	return kv.KeyValue.Purge(key, opts...)
}

func (kv KeyValue) Watch(keys string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	if err := kv.check(keys, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	return kv.KeyValue.Watch(keys, opts...)
}

func (kv KeyValue) WatchFiltered(keys []string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	for _, key := range keys {
		if err := kv.check(key, watchOverhead(kv.Bucket())); err != nil {
			return nil, err
		}
	}
	return kv.KeyValue.WatchFiltered(keys, opts...)
}

func (kv KeyValue) History(key string, opts ...nats.WatchOpt) ([]nats.KeyValueEntry, error) {
	if err := kv.check(key, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	return kv.KeyValue.History(key, opts...)
}

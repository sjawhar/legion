package bus

import (
	"fmt"

	"github.com/nats-io/nats.go"
)

// CheckedKeyValue returns kv with each key checked before nats.go builds a subject from it, which
// nats.go checks only for the characters a key may hold. A key whose longest subject would reach
// past maxSubjectBytes is ErrTooLarge, and one NATS would not accept in a subject (an empty token,
// `a..b`, which nats.go allows, or whitespace) is ErrInvalidSubject, each refused before anything
// is sent, whichever call carries the key. Every KV handle the listener and Dispatch read and write
// through is one.
func CheckedKeyValue(kv nats.KeyValue) nats.KeyValue {
	return checkedKeyValue{kv}
}

// kvKeyOverhead is what nats.go puts around a key of bucket in the longest subject it builds from
// one, the create request of a watcher on the key,
// `$JS.API.CONSUMER.CREATE.KV_<bucket>.<name>.$KV.<bucket>.<key>`, whose consumer name nats.go makes
// eight bytes. A direct get, a put and a delete build shorter ones.
func kvKeyOverhead(bucket string) int {
	return len("$JS.API.CONSUMER.CREATE.KV_.00000000.$KV..") + 2*len(bucket)
}

type checkedKeyValue struct {
	nats.KeyValue
}

func (kv checkedKeyValue) check(key string) error {
	if limit := maxSubjectBytes - kvKeyOverhead(kv.Bucket()); len(key) > limit {
		return fmt.Errorf("%w: a key of %d bytes, past %d", ErrTooLarge, len(key), limit)
	}
	if !validSubject(key) {
		return fmt.Errorf("%w: key %q", ErrInvalidSubject, key)
	}
	return nil
}

func (kv checkedKeyValue) Get(key string) (nats.KeyValueEntry, error) {
	if err := kv.check(key); err != nil {
		return nil, err
	}
	return kv.KeyValue.Get(key)
}

func (kv checkedKeyValue) GetRevision(key string, revision uint64) (nats.KeyValueEntry, error) {
	if err := kv.check(key); err != nil {
		return nil, err
	}
	return kv.KeyValue.GetRevision(key, revision)
}

func (kv checkedKeyValue) Put(key string, value []byte) (uint64, error) {
	if err := kv.check(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.Put(key, value)
}

func (kv checkedKeyValue) PutString(key string, value string) (uint64, error) {
	if err := kv.check(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.PutString(key, value)
}

func (kv checkedKeyValue) Create(key string, value []byte) (uint64, error) {
	if err := kv.check(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.Create(key, value)
}

func (kv checkedKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	if err := kv.check(key); err != nil {
		return 0, err
	}
	return kv.KeyValue.Update(key, value, last)
}

func (kv checkedKeyValue) Delete(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key); err != nil {
		return err
	}
	return kv.KeyValue.Delete(key, opts...)
}

func (kv checkedKeyValue) Purge(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key); err != nil {
		return err
	}
	return kv.KeyValue.Purge(key, opts...)
}

func (kv checkedKeyValue) Watch(keys string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	if err := kv.check(keys); err != nil {
		return nil, err
	}
	return kv.KeyValue.Watch(keys, opts...)
}

func (kv checkedKeyValue) WatchFiltered(keys []string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	for _, key := range keys {
		if err := kv.check(key); err != nil {
			return nil, err
		}
	}
	return kv.KeyValue.WatchFiltered(keys, opts...)
}

func (kv checkedKeyValue) History(key string, opts ...nats.WatchOpt) ([]nats.KeyValueEntry, error) {
	if err := kv.check(key); err != nil {
		return nil, err
	}
	return kv.KeyValue.History(key, opts...)
}

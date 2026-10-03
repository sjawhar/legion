package bus

import (
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// KeyValue is a KV handle that checks a key before nats.go builds a subject from it, since nats.go
// checks only the characters a key may hold: a key whose subject would reach past maxSubjectBytes is
// ErrTooLarge, and one NATS would not accept in a subject (an empty token, `a..b`, which nats.go
// allows, or whitespace) is ErrInvalidSubject, each refused before anything is sent. nats.go's own
// refusal of a key outside its alphabet is named the ErrInvalidKey it is (invalidKey), so every
// key a call refuses however often it is named is an ErrRefused. A write (Put, PutString, Create,
// Update) is held to the longest subject any call on its key builds, a watcher's create request, so
// a key written through it stays readable, watchable and deletable. Any other call is held only to
// the subject it builds itself, so a key an earlier build stored past the write bound still lists
// and deletes, and still reads while its get fits: a store serves it or skips it, and a reaper
// removes it. EnsureKeyValue and OpenKeyValue are how the listener and Dispatch open every bucket; a
// test wraps a handle it injects faults through as KeyValue{KeyValue: handle}.
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

// invalidKey names nats.go's refusal of a key outside its key alphabet (nats.ErrInvalidKey) as the
// ErrInvalidKey it is, naming key. nats.go checks the alphabet before it sends anything (an exact
// key on a read, write or delete, a search key on a watch), so the handle reads its answer rather
// than repeat its check.
func invalidKey(key any, err error) error {
	if errors.Is(err, nats.ErrInvalidKey) {
		return fmt.Errorf("%w: key %q: %w", ErrInvalidKey, key, err)
	}
	return err
}

func (kv KeyValue) Get(key string) (nats.KeyValueEntry, error) {
	if err := kv.check(key, getOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	entry, err := kv.KeyValue.Get(key)
	return entry, invalidKey(key, err)
}

func (kv KeyValue) Put(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.KeyValue.Put(key, value)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) PutString(key string, value string) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.KeyValue.PutString(key, value)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Create(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.KeyValue.Create(key, value)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.KeyValue.Update(key, value, last)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Delete(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	return invalidKey(key, kv.KeyValue.Delete(key, opts...))
}

// DeleteAtRead deletes key at the revision it reads, so a write that lands in between is kept, and
// returns that revision, 0 when there was none to read. A key it may not read (ErrRefused) it may not
// write either, since a write is held to the longest bound and nats.go refuses a key outside its
// alphabet everywhere, so nothing can land in between: that key is deleted at no revision, as a key
// it finds missing is, and a key outside the alphabet is refused by the delete too.
func (kv KeyValue) DeleteAtRead(key string) (uint64, error) {
	entry, err := kv.Get(key)
	var revision uint64
	var opts []nats.DeleteOpt
	switch {
	case err == nil:
		revision = entry.Revision()
		opts = append(opts, nats.LastRevision(revision))
	case errors.Is(err, nats.ErrKeyNotFound), errors.Is(err, ErrRefused):
	default:
		return 0, err
	}
	return revision, kv.Delete(key, opts...)
}

func (kv KeyValue) Purge(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	return invalidKey(key, kv.KeyValue.Purge(key, opts...))
}

func (kv KeyValue) Watch(keys string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	if err := kv.check(keys, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	watcher, err := kv.KeyValue.Watch(keys, opts...)
	return watcher, invalidKey(keys, err)
}

func (kv KeyValue) WatchFiltered(keys []string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	for _, key := range keys {
		if err := kv.check(key, watchOverhead(kv.Bucket())); err != nil {
			return nil, err
		}
	}
	watcher, err := kv.KeyValue.WatchFiltered(keys, opts...)
	return watcher, invalidKey(keys, err)
}

func (kv KeyValue) History(key string, opts ...nats.WatchOpt) ([]nats.KeyValueEntry, error) {
	if err := kv.check(key, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	history, err := kv.KeyValue.History(key, opts...)
	return history, invalidKey(key, err)
}

// StreamState is what one STREAM.INFO read says about the stream behind a KV bucket: its name, the
// sequence space it holds, how many messages are in it, and when it was created. A KV revision is
// a stream sequence, so a caller comparing revisions against the bucket's sequence space reads
// them from here.
type StreamState struct {
	Name     string
	FirstSeq uint64
	LastSeq  uint64
	Msgs     uint64
	Created  time.Time
}

// ReadStreamState reads the stream behind kv (one KV Status round trip, which is a STREAM.INFO).
func ReadStreamState(kv nats.KeyValue) (StreamState, error) {
	status, err := kv.Status()
	if err != nil {
		return StreamState{}, err
	}
	bucket, ok := status.(*nats.KeyValueBucketStatus)
	if !ok {
		return StreamState{}, fmt.Errorf("KV status of %s is a %T, not a JetStream bucket's", kv.Bucket(), status)
	}
	info := bucket.StreamInfo()
	return StreamState{
		Name:     info.Config.Name,
		FirstSeq: info.State.FirstSeq,
		LastSeq:  info.State.LastSeq,
		Msgs:     info.State.Msgs,
		Created:  info.Created,
	}, nil
}

package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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
// removes it.
//
// Every read and write goes through the embedded legacy nats.KeyValue, except what nats.go runs as a
// watcher and the reads beside them: a watch of the bucket (WatchKeys), a key's history
// (ReadHistory) and its stream's state (ReadStreamState) go through the same bucket's
// jetstream.KeyValue instead. nats.go v1.50.0's legacy KV watcher, which its legacy Watch, History
// and Keys all run, arms an init timer while its scan is pending whose callback can send on the
// channels the subscription's end has just closed, a "send on closed channel" panic in a goroutine
// nats.go owns (kv.go:1145-1172, unfixed through v1.55.0), while the jetstream package's watcher has
// no such timer and closes its updates only on the subscription goroutine's own exit (jetstream/
// kv.go:1215-1366). So nothing calls the legacy handle's Watch, WatchAll, WatchFiltered, History,
// Keys or ListKeys. A legacy handle cannot name the connection it was opened on, so every
// constructor takes it.
//
// EnsureKeyValue and OpenKeyValue are how the listener and Dispatch open every bucket, and
// WrapKeyValue pairs a legacy handle a caller already holds with its connection. A test that
// injects faults builds its handle with NewKeyValue, from the legacy handle and the jetstream one
// (WatchHandle) it wraps. A handle built as KeyValue{KeyValue: handle} reads and writes but cannot
// watch, read a history or read its stream: each answers ErrNoWatchHandle.
type KeyValue struct {
	nats.KeyValue
	// watch is the same bucket as a jetstream handle, the one every watch, history and stream read
	// goes through.
	watch jetstream.KeyValue
	// streams is the same connection's jetstream context, which every read of the bucket's stream
	// goes through (ReadStreamState): each read fetches the stream afresh. The jetstream handle's
	// own Status stores the info it read in the handle's shared stream value with no lock (nats.go
	// v1.50.0 jetstream/stream.go:460, from kv.go:1556), so two reads of one handle at once -- a
	// watcher's stream probe beside its completion check -- race on it.
	streams jetstream.JetStream
}

// ErrNoWatchHandle is what WatchKeys, ReadHistory and ReadStreamState answer on a handle that
// carries no jetstream handle or context to go through: one built as KeyValue{KeyValue: handle}. There is no
// fallback to the legacy handle.
var ErrNoWatchHandle = errors.New("the KV handle has no jetstream handle to watch through")

// RequestTimeout bounds a read through the jetstream handle whose caller holds no deadline of its
// own: the STREAM.INFO that opens the handle, a read of the bucket's stream, a key's history. It is
// the JetStream MaxWait the listener and Dispatch open their legacy handles with, so a read moved
// off the legacy handle waits no longer than it did there, and no less, whatever MaxWait the legacy
// handle carries.
const RequestTimeout = 10 * time.Second

// EnsureKeyValue opens the KV bucket config names on conn, through js, creating it with config when
// it does not exist.
func EnsureKeyValue(conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	kv, err := js.KeyValue(config.Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(config)
	}
	if err != nil {
		return KeyValue{}, err
	}
	return WrapKeyValue(conn, kv)
}

// OpenKeyValue opens the existing KV bucket named bucket on conn, through js, and creates nothing,
// as a rebuild does.
func OpenKeyValue(conn *nats.Conn, js nats.JetStreamContext, bucket string) (KeyValue, error) {
	kv, err := js.KeyValue(bucket)
	if err != nil {
		return KeyValue{}, err
	}
	return WrapKeyValue(conn, kv)
}

// WrapKeyValue pairs kv, a legacy handle opened on conn, with the jetstream handle of the same
// bucket on conn that its watches go through. Opening that handle is one STREAM.INFO, bounded by
// RequestTimeout.
func WrapKeyValue(conn *nats.Conn, kv nats.KeyValue) (KeyValue, error) {
	js, err := jetstream.New(conn)
	if err != nil {
		return KeyValue{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	watch, err := js.KeyValue(ctx, kv.Bucket())
	if err != nil {
		return KeyValue{}, fmt.Errorf("open the jetstream handle of KV bucket %s: %w", kv.Bucket(), err)
	}
	return KeyValue{KeyValue: kv, watch: watch, streams: js}, nil
}

// NewKeyValue pairs kv with watch, a jetstream handle of the same bucket, and streams, the jetstream
// context its stream is read through, without a round trip: for a test that wraps any of them to
// inject a fault.
func NewKeyValue(kv nats.KeyValue, watch jetstream.KeyValue, streams jetstream.JetStream) KeyValue {
	return KeyValue{KeyValue: kv, watch: watch, streams: streams}
}

// WatchHandle is the jetstream handle kv's watches go through, nil when it has none.
func (kv KeyValue) WatchHandle() jetstream.KeyValue {
	return kv.watch
}

// Streams is the jetstream context kv's stream is read through, nil when it has none.
func (kv KeyValue) Streams() jetstream.JetStream {
	return kv.streams
}

// WatchKeys watches the keys matching keys (jetstream.AllKeys for every key) through kv's jetstream
// handle, checking keys against the subject a watcher's create request builds. nats.go ties the
// subscription's life to ctx, which it also uses for the consumer's create request (js.go:
// 1951-1952, 2052-2058): the watcher ends at ctx's deadline, so one that follows the bucket takes a
// ctx without one, and the caller cancels ctx once it has stopped the watcher, or the goroutine
// nats.go parks on ctx.Done() outlives the watch. A ctx without a deadline still bounds the create,
// by the JetStream request wait of the handle's legacy push context, 5 s (jsm.go:1791-1795,
// js.go:299). The watcher sends a nil entry once it has delivered every existing key, and closes
// its updates only once its subscription has ended; whoever stops it keeps reading it until then,
// since nats.go's delivery blocks on a full 256-entry buffer.
func (kv KeyValue) WatchKeys(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if kv.watch == nil {
		return nil, fmt.Errorf("watch KV bucket %s: %w", kv.Bucket(), ErrNoWatchHandle)
	}
	if err := kv.check(keys, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	watcher, err := kv.watch.Watch(ctx, keys, opts...)
	return watcher, invalidKey(keys, err)
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

// invalidKey names nats.go's refusal of a key outside its key alphabet (nats.ErrInvalidKey, or
// jetstream.ErrInvalidKey on a watch through the jetstream handle) as the ErrInvalidKey it is,
// naming key. nats.go checks the alphabet before it sends anything (an exact key on a read, write
// or delete, a search key on a watch), so the handle reads its answer rather than repeat its check.
func invalidKey(key any, err error) error {
	if errors.Is(err, nats.ErrInvalidKey) || errors.Is(err, jetstream.ErrInvalidKey) {
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

// GetRevision gets key by revision, which names the revision rather than the key in its subject, so
// only nats.go's alphabet check refuses a key here.
func (kv KeyValue) GetRevision(key string, revision uint64) (nats.KeyValueEntry, error) {
	entry, err := kv.KeyValue.GetRevision(key, revision)
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

// StreamState is what one STREAM.INFO read says about the stream behind a KV bucket: its name, the
// sequence space it holds, how many messages are in it, when it was created and how long it keeps
// a message (its MaxAge, the bucket's TTL; 0 keeps them for good). A KV revision is a stream
// sequence, so a caller comparing revisions against the bucket's sequence space reads them from
// here.
type StreamState struct {
	Name     string
	FirstSeq uint64
	LastSeq  uint64
	Msgs     uint64
	Created  time.Time
	MaxAge   time.Duration
}

// ReadStreamState reads the stream behind kv (one STREAM.INFO) through its jetstream context, within
// ctx, fetching the stream afresh so no read shares state with another (KeyValue.streams). The
// legacy handle's Status would wait out that handle's JetStream MaxWait, which its opener chose for
// its own reads, so the caller bounds this one. A stream that is gone is nats.ErrStreamNotFound,
// however the jetstream package names it.
func (kv KeyValue) ReadStreamState(ctx context.Context) (StreamState, error) {
	if kv.streams == nil {
		return StreamState{}, fmt.Errorf("read the stream of KV bucket %s: %w", kv.Bucket(), ErrNoWatchHandle)
	}
	stream, err := kv.streams.Stream(ctx, "KV_"+kv.Bucket())
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return StreamState{}, fmt.Errorf("read the stream of KV bucket %s: %w: %w", kv.Bucket(), nats.ErrStreamNotFound, err)
	}
	if err != nil {
		return StreamState{}, err
	}
	info := stream.CachedInfo()
	return StreamState{
		Name:     info.Config.Name,
		FirstSeq: info.State.FirstSeq,
		LastSeq:  info.State.LastSeq,
		Msgs:     info.State.Msgs,
		Created:  info.Created,
		MaxAge:   info.Config.MaxAge,
	}, nil
}

// ReadHistory reads every revision of key the bucket holds, oldest first, through a watch of the key
// with its history included (WatchKeys), the watch nats.go's History runs on the legacy watcher
// (KeyValue says why that one never runs). ctx bounds the whole read, the watch's create included:
// nats.go ends the watch's subscription when ctx ends. A key with no revision is nats.ErrKeyNotFound,
// as nats.go's History answers. A watch that ends before it has delivered every revision -- ctx
// ended, or its subscription did -- is an error rather than the revisions that arrived, which is
// what nats.go's History would return: a caller that takes the last one for the key's latest would
// take an older revision for it. Once complete, the watch is stopped and read until it closes, since
// nats.go's delivery blocks on a full 256-entry buffer holding the watcher's lock.
func (kv KeyValue) ReadHistory(ctx context.Context, key string) ([]jetstream.KeyValueEntry, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher, err := kv.WatchKeys(ctx, key, jetstream.IncludeHistory())
	if err != nil {
		return nil, err
	}
	var entries []jetstream.KeyValueEntry
	for entry := range watcher.Updates() {
		if entry != nil {
			entries = append(entries, entry)
			continue
		}
		_ = watcher.Stop()
		go drain(watcher)
		if len(entries) == 0 {
			return nil, fmt.Errorf("read the history of key %s: %w", key, nats.ErrKeyNotFound)
		}
		return entries, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read the history of key %s of KV bucket %s: %w", key, kv.Bucket(), err)
	}
	return nil, fmt.Errorf("read the history of key %s of KV bucket %s: the watch ended after %d revisions, before it had delivered them all", key, kv.Bucket(), len(entries))
}

// drain reads watcher's updates until nats.go closes them, which it does once the subscription a
// stop ended has delivered what it held.
func drain(watcher jetstream.KeyWatcher) {
	for range watcher.Updates() {
	}
}

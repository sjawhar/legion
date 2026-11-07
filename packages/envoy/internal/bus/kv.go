package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
// Every read and write goes through the bucket's legacy nats.KeyValue, except what nats.go runs as
// a watcher and the reads beside them: a watch of the bucket (WatchKeys), a key's history
// (ReadHistory) and its stream's state (ReadStreamState) go through the same bucket's
// jetstream.KeyValue instead. nats.go v1.50.0's legacy KV watcher, which its legacy Watch, History
// and Keys all run, arms an init timer while its scan is pending whose callback can send on the
// channels the subscription's end has just closed, a "send on closed channel" panic in a goroutine
// nats.go owns (kv.go:1145-1172, unfixed through v1.55.0), while the jetstream package's watcher has
// no such timer and closes its updates only on the subscription goroutine's own exit (jetstream/
// kv.go:1215-1366). So the legacy handle is held unexported and KeyValue forwards only the calls it
// checks: nothing can reach the legacy handle's Watch, WatchAll, WatchFiltered, History, Keys or
// ListKeys through a KeyValue, and a call to one does not compile. A legacy handle cannot name the
// connection it was opened on, so every constructor takes it.
//
// EnsureKeyValue and OpenKeyValue are how the listener and Dispatch open every bucket, and
// WrapKeyValue pairs a legacy handle a caller already holds with its connection. A test that
// injects faults builds its handle with NewKeyValue, from the legacy handle (Legacy), the jetstream
// one (WatchHandle) and the jetstream context (Streams) it wraps. A zero KeyValue has none of them
// and is a programming error: every call on it panics.
type KeyValue struct {
	// legacy is the bucket's legacy handle, which every checked read and write goes through. It is
	// not embedded, so none of its watcher calls is promoted.
	legacy nats.KeyValue
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

// RequestTimeout bounds a read through the jetstream handle whose caller holds no deadline of its
// own: the STREAM.INFO that opens the handle, a read of the bucket's stream, a key's history. It is
// the JetStream MaxWait the listener and Dispatch open their legacy handles with, so a read moved
// off the legacy handle waits no longer than it did there, and no less, whatever MaxWait the legacy
// handle carries.
const RequestTimeout = 10 * time.Second

// A create a clustered server answers is committed to the JetStream metadata group, not applied
// by every member yet: a member that has not applied the new stream assignment answers a read of
// the bucket's stream "stream not found", the member the create was sent through included. A probe
// over 25 fresh three-member clusters asked every member for a bucket's stream the instant
// CreateKeyValue answered, and 67 of the 75 member reads still said so, the slowest for 206 ms. So
// a read of a bucket this start has just created, or that a concurrent start created while this
// one looked, is taken again every bucketAppliedInterval until the member answers for it, and
// given up at bucketAppliedDeadline.
//
// bucketAppliedInterval is short against that 206 ms, so the wait ends within a small fraction of
// the catch-up it waits out, and each miss is one STREAM.INFO a member answers from memory.
// bucketAppliedDeadline is RequestTimeout, the wait every other read of a bucket's stream already
// takes (WrapKeyValue, ReadStreamState), and roughly fifty times the slowest catch-up measured, so
// a member that is not merely behind fails the start where that read would have failed it rather
// than hanging it.
const (
	bucketAppliedInterval = 25 * time.Millisecond
	bucketAppliedDeadline = RequestTimeout
)

// awaitBucketApplied is read, a read of the bucket a start has just created, taken again every
// bucketAppliedInterval while the member answering reports the bucket missing (missing) and no
// longer than bucketAppliedDeadline. It returns the first answer that is not that, and the last
// miss as it is once the deadline passes, so its caller wraps one error however the wait ended.
// Any other error, a refusal included, is returned at once: only a member that has not caught up
// is waited out. The first miss is logged, as bindListenerDurable logs its first refusal, so a
// start that waited says which bucket it waited for.
func awaitBucketApplied[T any](bucket string, missing func(error) bool, read func() (T, error)) (T, error) {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		value, err := read()
		if err == nil || !missing(err) {
			return value, err
		}
		if time.Since(start) >= bucketAppliedDeadline {
			return value, err
		}
		if attempt == 1 {
			slog.Warn("envoy nats kv bucket not applied yet, waiting",
				slog.String("bucket", bucket),
				slog.String("error", err.Error()),
				slog.String("retry_in", bucketAppliedInterval.String()),
				slog.String("deadline", bucketAppliedDeadline.String()))
		}
		time.Sleep(bucketAppliedInterval)
	}
}

// bucketMissing reports whether err is a member's answer that the bucket's stream is not there,
// however the legacy or the jetstream package names it: the answer a member that has not applied
// a create of it gives (bucketAppliedInterval).
func bucketMissing(err error) bool {
	return errors.Is(err, nats.ErrBucketNotFound) || errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, nats.ErrStreamNotFound) || errors.Is(err, jetstream.ErrStreamNotFound)
}

// EnsureKeyValue opens the KV bucket config names on conn, through js, creating it with config when
// it does not exist. config.Replicas is the deployment's replica count (ReplicasEnvVar): a new
// bucket is created at it, and an existing bucket with fewer copies is raised to it
// (raiseReplicas). A refused create or raise of more than one copy names the setting
// (refusedSettings), and is returned as soon as the server answers it: nothing a server refuses is
// waited out. A bucket a ConnectOwningStream client names (WithKeptBuckets) is raised to
// the deployment's count again by that client's keeper from then on, which also ends a lowering by
// a concurrent start with a lower count; the count holds while every program of the deployment
// carries the same one.
//
// A bucket this start created, and one a concurrent start created while this one looked (the
// server answers that create "stream name already in use"), is waited for on the member answering
// before the handle is opened on it (awaitBucketApplied), since a clustered create is answered
// before every member has applied it.
func EnsureKeyValue(conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	kv, err := js.KeyValue(config.Bucket)
	created := false
	switch {
	case errors.Is(err, nats.ErrBucketNotFound):
		kv, err = js.CreateKeyValue(config)
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			// The bucket was created between this start's lookup and its create, by another start
			// of this deployment, and the member answering may not have applied that create
			// either: the lookup is taken again until it has. The copies are not raised here, as
			// they are for a bucket this start found: the start that created it just now carries
			// the deployment's count too.
			kv, err = awaitBucketApplied(config.Bucket, bucketMissing, func() (nats.KeyValue, error) {
				return js.KeyValue(config.Bucket)
			})
		} else {
			err = refusedSettings(err, nil, &nats.StreamConfig{Replicas: config.Replicas})
		}
		created = err == nil
	case err == nil:
		var before int
		before, err = raiseReplicas(js, config.Bucket, config.Replicas)
		if err == nil && before > 0 {
			// The copies this raise added are behind until they have caught up with the bucket's
			// stream, and a KV read is a direct get, which nats-server hands a copy the queue
			// group at 90% caught up rather than when it is current (server/jetstream_cluster.go:
			// 2622-2641, v2.10.29): a read taken now can be answered "key not found" by a copy
			// that does not hold the key yet. The caller reads keys as soon as this call answers
			// (the webhook redelivery sweep reads its cursor and its index), and a stale miss
			// there asks GitHub for a delivery it has already redelivered, so the start waits for
			// the copies and fails rather than reading early.
			err = awaitReplicasCurrent(js, config.Bucket, config.Replicas)
		}
	}
	if err != nil {
		return KeyValue{}, err
	}
	return wrapKeyValue(conn, kv, created)
}

// raiseReplicas raises the stream of the existing bucket to replicas copies when it has fewer, and
// returns the copies it had then, 0 when it left the bucket alone. A bucket with as many copies or
// more is left alone, and one copy needs no read: every stream has at least one. The read, the
// comparison and the update are raiseBucketSetting's, so only the copies change, and a refused
// raise names the setting (refusedSettings): `raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3:
// ...`, so the keeper's line says which bucket it could not raise.
func raiseReplicas(js nats.JetStreamContext, bucket string, replicas int) (int, error) {
	if replicas <= 1 {
		return 0, nil
	}
	deployed, raised, err := raiseBucketSetting(js, bucket, "",
		func(config *nats.StreamConfig) bool { return config.Replicas >= replicas },
		func(config *nats.StreamConfig) { config.Replicas = replicas })
	if err != nil || !raised {
		return 0, err
	}
	return deployed.Replicas, nil
}

// RaiseMaxAge raises the stream of the existing bucket to keep a key's latest value for age when
// it keeps it for less. A bucket EnsureKeyValue creates is created with the age in its config;
// this is for one an earlier build created with a shorter one, since a create that finds the
// bucket there changes nothing. A bucket that keeps values as long or longer is left alone, and
// so is one that keeps them forever, which JetStream writes as a MaxAge of 0: an operator's
// longer age stands, unlimited included. The read, the comparison and the update are
// raiseBucketSetting's, so only the age changes and its error names the bucket's stream: `raise
// KV_envoy_listener's max age: ...`.
func RaiseMaxAge(js nats.JetStreamContext, bucket string, age time.Duration) error {
	_, _, err := raiseBucketSetting(js, bucket, "'s max age",
		func(config *nats.StreamConfig) bool { return config.MaxAge == 0 || config.MaxAge >= age },
		func(config *nats.StreamConfig) { config.MaxAge = age })
	return err
}

// raiseBucketSetting raises one setting of the stream behind the existing bucket: it reads the
// stream's deployed configuration, leaves the bucket alone when enough reports that configuration
// already holds what the caller wants, and otherwise writes that configuration back with raise's
// one change applied, so the bucket's other settings - its copies, age, history, discard policy,
// direct get, rollup, delete settings, metadata and the rest - stay as they are. It returns the
// configuration it read and whether it wrote. As for the stream (ensureStreamWithConfig), two
// starts raising at once each write the deployment's one value. A raise whose answer never came
// succeeds when the stream, read again, holds what enough asks for, and one the server refused as
// overlapping the bucket's own stream is sent again until the stream holds it or its sends run out
// (updateStream). Its error
// names the bucket's stream, and after it what of the stream the raise is (of, empty for the
// stream itself); a refused raise names the deployment setting whose value it changes
// (refusedSettings).
func raiseBucketSetting(js nats.JetStreamContext, bucket, of string, enough func(*nats.StreamConfig) bool, raise func(*nats.StreamConfig)) (nats.StreamConfig, bool, error) {
	stream := "KV_" + bucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		return nats.StreamConfig{}, false, fmt.Errorf("raise %s%s: %w", stream, of, err)
	}
	if enough(&info.Config) {
		return info.Config, false, nil
	}
	raised := info.Config
	raise(&raised)
	if err := updateStream(js, &raised, func(again *nats.StreamInfo) bool { return enough(&again.Config) }); err != nil {
		return info.Config, false, fmt.Errorf("raise %s%s: %w", stream, of, refusedSettings(err, &info.Config, &raised))
	}
	return info.Config, true, nil
}

// awaitReplicasCurrent waits until the bucket's stream reports replicas copies, each one current
// with its leader and none of them offline or lagging, read every bucketAppliedInterval and given
// up at bucketAppliedDeadline. It is what EnsureKeyValue runs after a raise that added copies, and
// nothing else needs it: a bucket nobody raised has no copy that is behind.
//
// The deadline is an error naming the bucket's stream, the copies it asked for and what each copy
// that was not current had to say (copiesBehind), the way a refused raise names its setting, and
// the caller fails its start on it. Carrying on instead would hand the caller a bucket whose
// direct gets a copy that is 90% caught up may answer "key not found" for a key that is there
// (server/jetstream_cluster.go:2622-2641, v2.10.29), and a start that reads one wrong answer is
// worse than a start that does not come up: the webhook redelivery sweep reads its records the
// moment this call answers, and a stale miss there asks GitHub for a redelivery it already made.
//
// A read that fails is returned as it is, since the stream is there: this runs only after a raise
// of it answered.
func awaitReplicasCurrent(js nats.JetStreamContext, bucket string, replicas int) error {
	stream := "KV_" + bucket
	start := time.Now()
	for attempt := 1; ; attempt++ {
		info, err := js.StreamInfo(stream)
		if err != nil {
			return fmt.Errorf("raise %s: read the raised stream: %w", stream, err)
		}
		behind := copiesBehind(info, replicas)
		if behind == "" {
			return nil
		}
		if time.Since(start) >= bucketAppliedDeadline {
			return fmt.Errorf("raise %s: %d copies were not current within %s: %s", stream, replicas, bucketAppliedDeadline, behind)
		}
		if attempt == 1 {
			slog.Warn("envoy nats kv raised copies not current yet, waiting",
				slog.String("bucket", bucket),
				slog.Int("replicas", replicas),
				slog.String("behind", behind),
				slog.String("retry_in", bucketAppliedInterval.String()),
				slog.String("deadline", bucketAppliedDeadline.String()))
		}
		time.Sleep(bucketAppliedInterval)
	}
}

// copiesBehind says what keeps the stream info describes short of replicas copies that are all
// current, and is empty when none does: the stream has a leader, as many copies as were asked for,
// and every copy of it is current, online and not lagging. Each copy that is not is named with
// what it reported, so a start that gave up says which member it waited for.
func copiesBehind(info *nats.StreamInfo, replicas int) string {
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return "the stream has no leader yet"
	}
	var behind []string
	if copies := 1 + len(info.Cluster.Replicas); copies != replicas {
		behind = append(behind, fmt.Sprintf("%d copies of %d", copies, replicas))
	}
	for _, peer := range info.Cluster.Replicas {
		switch {
		case peer.Offline:
			behind = append(behind, peer.Name+" offline")
		case !peer.Current:
			behind = append(behind, fmt.Sprintf("%s not current, lag %d", peer.Name, peer.Lag))
		case peer.Lag > 0:
			behind = append(behind, fmt.Sprintf("%s lag %d", peer.Name, peer.Lag))
		}
	}
	return strings.Join(behind, ", ")
}

// SetKeyValueMaxAge sets the maximum age of the existing bucket's stream to maxAge when it carries
// another, and returns the age the bucket carries once it has: maxAge, whether it was written now
// or already there, and 0 only when no age was asked for and the bucket was left alone - so a
// caller that must know the age it is expiring against is answered by this call and makes no
// second read of its own. As raiseReplicas does for the copies, the update writes back the
// stream's deployed configuration with only its MaxAge changed, so the bucket's replicas,
// history, discard policy, direct get, rollup, delete settings, metadata and the rest stay as
// they are. A non-positive maxAge sets none: a caller whose setting is unset leaves the bucket's
// own age standing (ParseSessionMaxAge), as it leaves one copy alone.
// An age an answer never came for succeeds when the stream, read again, carries it, and one the
// server refused as overlapping the bucket's own stream is sent again (updateStream). Its error
// names the bucket's stream: `set KV_envoy_sessions max age: ...`.
func SetKeyValueMaxAge(js nats.JetStreamContext, bucket string, maxAge time.Duration) (time.Duration, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	stream := "KV_" + bucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		return 0, fmt.Errorf("set %s max age: %w", stream, err)
	}
	if info.Config.MaxAge == maxAge {
		return maxAge, nil
	}
	aged := info.Config
	aged.MaxAge = maxAge
	if err := updateStream(js, &aged, func(again *nats.StreamInfo) bool { return again.Config.MaxAge == maxAge }); err != nil {
		return 0, fmt.Errorf("set %s max age: %w", stream, err)
	}
	return maxAge, nil
}

// OpenKeyValue opens the existing KV bucket named bucket on conn, through js, and creates nothing,
// as a rebuild does. It raises nothing either: the start that opened the bucket with EnsureKeyValue
// did that.
func OpenKeyValue(conn *nats.Conn, js nats.JetStreamContext, bucket string) (KeyValue, error) {
	kv, err := js.KeyValue(bucket)
	if err != nil {
		return KeyValue{}, err
	}
	return WrapKeyValue(conn, kv)
}

// WrapKeyValue pairs kv, a legacy handle opened on conn, with the jetstream handle of the same
// bucket on conn that its watches go through. Opening that handle is one STREAM.INFO, bounded by
// RequestTimeout. The bucket is read as it is: a caller that holds a handle on it has had an
// answer for it already. EnsureKeyValue's create path waits the member out instead
// (wrapKeyValue).
func WrapKeyValue(conn *nats.Conn, kv nats.KeyValue) (KeyValue, error) {
	return wrapKeyValue(conn, kv, false)
}

// wrapKeyValue is WrapKeyValue, with the STREAM.INFO that opens the jetstream handle taken again
// until the member answering has applied the create of the bucket, when created says this start
// has just created it
// (awaitBucketApplied): the create's own answer came from the metadata group, and this read goes
// to whichever member conn is on, which may not have applied it yet. Each read is bounded by
// RequestTimeout as a lone read is, and every one of them together by bucketAppliedDeadline.
func wrapKeyValue(conn *nats.Conn, kv nats.KeyValue, created bool) (KeyValue, error) {
	js, err := jetstream.New(conn)
	if err != nil {
		return KeyValue{}, err
	}
	open := func() (jetstream.KeyValue, error) {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		return js.KeyValue(ctx, kv.Bucket())
	}
	var watch jetstream.KeyValue
	if created {
		watch, err = awaitBucketApplied(kv.Bucket(), bucketMissing, open)
	} else {
		watch, err = open()
	}
	if err != nil {
		return KeyValue{}, fmt.Errorf("open the jetstream handle of KV bucket %s: %w", kv.Bucket(), err)
	}
	return KeyValue{legacy: kv, watch: watch, streams: js}, nil
}

// NewKeyValue pairs kv with watch, a jetstream handle of the same bucket, and streams, the jetstream
// context its stream is read through, without a round trip: for a test that wraps any of them to
// inject a fault.
func NewKeyValue(kv nats.KeyValue, watch jetstream.KeyValue, streams jetstream.JetStream) KeyValue {
	return KeyValue{legacy: kv, watch: watch, streams: streams}
}

// Legacy is the legacy handle kv's checked reads and writes go through, for a test that wraps it to
// inject a fault (NewKeyValue). Production code calls kv's own methods, which check the key first.
func (kv KeyValue) Legacy() nats.KeyValue {
	return kv.legacy
}

// Opened reports whether kv is a handle on a bucket rather than the zero value, for a caller that
// opens its bucket once and keeps it (the webhook redelivery sweep's openState).
func (kv KeyValue) Opened() bool {
	return kv.legacy != nil
}

// WatchHandle is the jetstream handle kv's watches go through.
func (kv KeyValue) WatchHandle() jetstream.KeyValue {
	return kv.watch
}

// Streams is the jetstream context kv's stream is read through.
func (kv KeyValue) Streams() jetstream.JetStream {
	return kv.streams
}

// Bucket is the name of the bucket kv is a handle on.
func (kv KeyValue) Bucket() string {
	return kv.legacy.Bucket()
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
	if err := kv.check(keys, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	watcher, err := kv.watch.Watch(ctx, keys, opts...)
	return watcher, invalidKey(keys, err)
}

// WatchFilteredKeys is WatchKeys over several key patterns at once, the watch a caller that reads
// a window of a time-keyed bucket needs (the webhook redelivery index): one watcher, one
// consumer, and the subjects of every pattern on it. Each pattern is checked as WatchKeys checks
// its own, since each becomes a filter subject of the same create request.
func (kv KeyValue) WatchFilteredKeys(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	for _, key := range keys {
		if err := kv.check(key, watchOverhead(kv.Bucket())); err != nil {
			return nil, err
		}
	}
	watcher, err := kv.watch.WatchFiltered(ctx, keys, opts...)
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
// or delete, a search key on a watch), so every call through the handle reads its answer rather
// than repeat its check. RefusesRead is the one exception: it answers without calling nats.go, so
// it repeats the check as natsKeyValid, which must follow nats.go's validKeyRe on every upgrade.
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
	entry, err := kv.legacy.Get(key)
	return entry, invalidKey(key, err)
}

// RefusesRead answers the refusal a Get of key through this handle would answer before anything is
// sent, and nil when a read of key would reach the bucket. It sends nothing itself.
//
// It is for a caller holding a key it got from a scan of the bucket rather than from a read of the
// key: a listing names keys this build may be unable to read, and such a caller holds no refusal to
// classify them by, so it asks this instead and tells a key this build cannot read from an ordinary
// one without a round trip. What it answers is exactly the refusal Get answers before sending, so
// the two cannot drift silently: Get's own check first, then nats.go's key alphabet, which this
// package copies as natsKeyValid because nats.go's keyValid is unexported.
// TestRefusesReadAnswersTheSameRefusalAGetOfTheKeyAnswers is what pins that copy, against the
// refusal a real Get answers.
func (kv KeyValue) RefusesRead(key string) error {
	if err := kv.check(key, getOverhead(kv.Bucket())); err != nil {
		return err
	}
	if !natsKeyValid(key) {
		return invalidKey(key, nats.ErrInvalidKey)
	}
	return nil
}

// natsKeyValid copies nats.go v1.50.0's keyValid (kv.go:579-584), which every KV call there runs on
// an exact key, refusing it locally without sending anything: a key is valid when it is not empty,
// neither starts nor ends in a dot, and holds only `-`, `/`, `_`, `=`, `.`, a letter or a digit
// (validKeyRe, kv.go:369). The copy exists because keyValid is unexported, so a caller that asks
// what a read would be refused for (RefusesRead) cannot ask nats.go itself.
func natsKeyValid(key string) bool {
	if key == "" || key[0] == '.' || key[len(key)-1] == '.' {
		return false
	}
	for i := range len(key) {
		switch c := key[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '/', c == '_', c == '=', c == '.':
		default:
			return false
		}
	}
	return true
}

// GetRevision gets key by revision, which names the revision rather than the key in its subject, so
// only nats.go's alphabet check refuses a key here.
func (kv KeyValue) GetRevision(key string, revision uint64) (nats.KeyValueEntry, error) {
	entry, err := kv.legacy.GetRevision(key, revision)
	return entry, invalidKey(key, err)
}

func (kv KeyValue) Put(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.legacy.Put(key, value)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) PutString(key string, value string) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.legacy.PutString(key, value)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Create(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.legacy.Create(key, value)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := kv.legacy.Update(key, value, last)
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Delete(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	return invalidKey(key, kv.legacy.Delete(key, opts...))
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
	return invalidKey(key, kv.legacy.Purge(key, opts...))
}

// Status reads the bucket's status through the legacy handle, which waits out that handle's
// JetStream MaxWait: the open-time read of a bucket's TTL and a store's health probe, whose
// openers chose that wait. A read held to RequestTimeout instead is ReadStreamState.
func (kv KeyValue) Status() (nats.KeyValueStatus, error) {
	return kv.legacy.Status()
}

// Bounded is call, a bucket call that takes no context, as a caller holding ctx waits for it:
// nats.go's KV reads, writes and deletes take none and wait out the JetStream MaxWait (10 s), so
// the wait ends at the earlier of call's answer and ctx's end. answered is false when ctx ended
// first, and err is then ctx's error; the abandoned call runs on to its own end and its answer is
// dropped, so a write or a delete it sent may still land after the caller has answered (each
// caller says why that is safe). A ctx that has already ended starts no call, and a ctx that can
// never end (context.Background, which a caller with no request behind it passes) is simply
// call, in this goroutine. A background loop is not such a caller: the role claim reaper passes
// its own loop's ctx, which the listener's shutdown ends under a pass in flight
// (store.StartRoleClaimReaper).
func Bounded[T any](ctx context.Context, call func() (T, error)) (value T, answered bool, err error) {
	if ctx.Done() == nil {
		value, err = call()
		return value, true, err
	}
	if err := ctx.Err(); err != nil {
		return value, false, err
	}
	type answer struct {
		value T
		err   error
	}
	answers := make(chan answer, 1)
	go func() {
		value, err := call()
		answers <- answer{value: value, err: err}
	}()
	select {
	case got := <-answers:
		return got.value, true, got.err
	case <-ctx.Done():
		return value, false, ctx.Err()
	}
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

// ReadStreamState reads the stream behind kv (one STREAM.INFO) through its jetstream context,
// within RequestTimeout, fetching the stream afresh so no read shares state with another
// (KeyValue.streams): the read before every watcher a kvwatch.Watcher installs, its stream probe,
// gate, completion check and Check, and each read of the interest marker collector. The legacy
// handle's Status would wait out that handle's JetStream MaxWait, which its opener chose for its
// own reads, however short, so this read bounds itself. A stream that is gone is
// nats.ErrStreamNotFound, however the jetstream package names it.
func (kv KeyValue) ReadStreamState() (StreamState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
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
		go DrainWatcher(watcher)
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

// DrainWatcher reads watcher's updates until nats.go closes them, which it does once the
// subscription a stop ended has delivered what it held: nats.go's delivery blocks holding the
// watcher's lock on a full 256-entry buffer, so a watcher stopped and left unread parks its
// subscription's goroutine for good. ReadHistory, kvwatch's scans and the watchers kvwatch stops
// all read theirs through it.
func DrainWatcher(watcher jetstream.KeyWatcher) {
	for range watcher.Updates() {
	}
}

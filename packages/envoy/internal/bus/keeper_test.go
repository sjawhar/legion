package bus

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// testKeepInterval is the keeper's period in these tests (withKeepInterval).
	testKeepInterval = 250 * time.Millisecond
	// keepWait bounds every wait for the keeper to restore what was taken: a few periods, and the
	// time a cluster takes to apply a replica change it is asked for.
	keepWait = 20 * testKeepInterval
	// keeperByteLimit is the byte limit of the program whose settings the keeper restores.
	keeperByteLimit = 2_000_000
)

// awaitKept polls the stream until kept reports it holds what the keeper restores, at the latest
// for keepWait, and returns its info.
func awaitKept(t *testing.T, js nats.JetStreamContext, stream, what string, kept func(*nats.StreamInfo) bool) *nats.StreamInfo {
	t.Helper()
	deadline := time.Now().Add(keepWait)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil && kept(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s not kept within %s: %+v (%v)", stream, what, keepWait, info, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// heldFor checks the stream for periods keeper periods and fails the test when still stops
// reporting that it is as it was left.
func heldFor(t *testing.T, js nats.JetStreamContext, stream, what string, periods int, still func(*nats.StreamInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(periods) * testKeepInterval)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		info, err := js.StreamInfo(stream)
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		if !still(info) {
			t.Fatalf("%s: %s changed: %+v", stream, what, info.Config)
		}
	}
}

// keptAtSettings reports whether the stream has the setting program's three copies and byte limit
// and an id, id itself when it is not empty.
func keptAtSettings(id string) func(*nats.StreamInfo) bool {
	return func(info *nats.StreamInfo) bool {
		got := info.Config.Metadata[StreamIDMetadataKey]
		return info.Config.Replicas == 3 && info.Config.MaxBytes == keeperByteLimit && got != "" && (id == "" || got == id)
	}
}

// interleavedJetStream runs another program's write in the middle of one program's read and write
// of the stream: afterRead once its first StreamInfo has answered, beforeUpdate just before its
// first UpdateStream is sent, each once and each only when set. It counts the stream updates sent
// through it.
type interleavedJetStream struct {
	nats.JetStreamContext
	afterRead, beforeUpdate func()
	readOnce, updateOnce    sync.Once
	updates                 atomic.Int32
	// updateErr is the server's answer to the last update sent through the wrapper: nil when the
	// update was applied, an unanswered wait when nats-server never answered it.
	updateErr atomic.Pointer[error]
}

func (js *interleavedJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := js.JetStreamContext.StreamInfo(stream, opts...)
	if js.afterRead != nil {
		js.readOnce.Do(js.afterRead)
	}
	return info, err
}

func (js *interleavedJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.beforeUpdate != nil {
		js.updateOnce.Do(js.beforeUpdate)
	}
	js.updates.Add(1)
	info, err := js.JetStreamContext.UpdateStream(cfg, opts...)
	js.updateErr.Store(&err)
	return info, err
}

// awaitRestoredLog waits, within keepWait, until the keeper's restore lines name every one of wants.
// The keeper logs a restore after the server applied it, so a test that reads the stream as kept
// can get there before the line is written.
func awaitRestoredLog(t *testing.T, logs *capturedLogs, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(keepWait)
	for {
		logged := restoredLog(t, logs)
		if !slices.ContainsFunc(wants, func(want string) bool { return !strings.Contains(logged, want) }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the keeper's lines restored %q within %s, want %q among them", logged, keepWait, wants)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// restoredLog is every field the keeper's restore lines logged so far, joined.
func restoredLog(t *testing.T, logs *capturedLogs) string {
	t.Helper()
	var fields []string
	for _, record := range logs.recordsWithMsg(t, "envoy nats keeper restored the stream") {
		if record["level"] != "WARN" || record["stream"] != Stream {
			t.Fatalf("keeper record = %v, want a WARN naming %s", record, Stream)
		}
		fields = append(fields, fmt.Sprint(record["restored"]))
	}
	return strings.Join(fields, ", ")
}

// Two programs start against an R1 stream whose subject list is old, so both update it: one with
// three copies and a byte limit, one with the defaults, one copy and no limit. JetStream's stream
// update has no compare-and-swap, and the test forces the order that loses the raise: the default
// program's start reads the stream, the setting program's start raises it, and only then does the
// default program's start write what it built from its read, one copy and no limit. Within a few
// keeper periods the setting program's keeper puts the three copies and the byte limit back and
// logs both, and the stream keeps its id, on every run.
func TestTheKeeperEndsTheMixedSettingsRaceAtThreeCopiesTheByteLimitAndTheID(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	logs := captureLogs(t)
	const id = "deployed-id"
	deployed := *streamCfg
	deployed.Subjects = slices.Clone(streamSubjects[:len(streamSubjects)-1])
	deployed.Replicas = 1
	deployed.Metadata = map[string]string{StreamIDMetadataKey: id}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("deploy the R1 stream with the old subjects: %v", err)
	}

	defaults := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	defaultStart := &interleavedJetStream{JetStreamContext: js, beforeUpdate: func() {
		startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), withKeepInterval(testKeepInterval))
		if info, err := js.StreamInfo(Stream); err != nil || !keptAtSettings(id)(info) {
			t.Fatalf("the setting program's start left the stream at %+v (%v), want three copies, the byte limit and the id", info, err)
		}
	}}
	if err := ensureStreamWithConfig(defaultStart, &defaults); err != nil {
		t.Fatalf("the default program's start: %v", err)
	}
	if n := defaultStart.updates.Load(); n != 1 {
		t.Fatalf("the default program's start sent %d stream updates, want its one stale write", n)
	}

	awaitKept(t, js, Stream, "three copies, the byte limit and the id", keptAtSettings(id))
	// Under load nats-server can leave a replica change unanswered (unanswered); the default
	// program's start then reads the stream again, finds three copies, and counts its write as
	// done. That write may never land, or land later: the keeper only has something to restore
	// once it lands. So the restore line is required only when the server answered the stale write,
	// and an unanswered one must still end at the settings a few periods later.
	if stale := *defaultStart.updateErr.Load(); stale != nil {
		if !unanswered(stale) {
			t.Fatalf("the default program's stale write: %v", stale)
		}
		t.Logf("the server left the stale write unanswered (%v): checking the end state only", stale)
		time.Sleep(4 * testKeepInterval)
		awaitKept(t, js, Stream, "three copies, the byte limit and the id", keptAtSettings(id))
		return
	}
	awaitRestoredLog(t, logs, "replicas 1 -> 3", fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit))
}

// The keeper's write is the stream as it read it with its own part put back, so a concurrent
// start's write that lands between the keeper's read and its write is lost, its subject included.
// The test forces that order: the keeper of a program with a byte limit reads the stream it must
// restore, a start of a release that compiles one more subject adds that subject, and then the
// keeper's write lands without it. The next pass of that release's keeper adds its subject back
// and logs it, and a pass of the first program's keeper, whose binary does not compile the subject,
// leaves it there.
func TestEachReleasesKeeperAddsBackItsOwnSubjectAConcurrentWriteTookAway(t *testing.T) {
	js := streamTestJetStream(t)
	const added = "notifications.newer.>"
	limited := parseConnectOptions([]ConnectOption{WithStreamMaxBytes(keeperByteLimit)}).streamConfig()
	newer := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	newer.Subjects = append(slices.Clone(streamSubjects), added)
	if err := ensureStreamWithConfig(js, &limited); err != nil {
		t.Fatalf("the limited program's start: %v", err)
	}
	lowered := deployedStream(t, js).Config
	lowered.MaxBytes = -1
	if _, err := js.UpdateStream(&lowered); err != nil {
		t.Fatalf("remove the byte limit: %v", err)
	}

	keeper := &interleavedJetStream{JetStreamContext: js, beforeUpdate: func() {
		if err := ensureStreamWithConfig(js, &newer); err != nil {
			t.Fatalf("the newer release's start: %v", err)
		}
		if subjects := deployedStream(t, js).Config.Subjects; !slices.Contains(subjects, added) {
			t.Fatalf("the newer release's start left the subjects %v, want %s among them", subjects, added)
		}
	}}
	restored, err := keepStream(keeper, &limited)
	if want := []string{fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit)}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the limited program's keeper restored %v (%v), want %v", restored, err, want)
	}
	if subjects := deployedStream(t, js).Config.Subjects; slices.Contains(subjects, added) {
		t.Fatalf("subjects %v still hold %s: the keeper's write did not land after the newer start's", subjects, added)
	}

	restored, err = keepStream(js, &newer)
	if want := []string{"subjects added " + added}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the newer release's keeper restored %v (%v), want %v", restored, err, want)
	}
	if restored, err := keepStream(js, &limited); err != nil || len(restored) != 0 {
		t.Fatalf("the limited program's next pass restored %v (%v), want nothing", restored, err)
	}
	info := deployedStream(t, js)
	if want := newer.Subjects; !slices.Equal(info.Config.Subjects, want) || info.Config.MaxBytes != keeperByteLimit {
		t.Fatalf("stream subjects %v, byte limit %d; want %v, %d", info.Config.Subjects, info.Config.MaxBytes, want, keeperByteLimit)
	}
}

// A start moves the role lanes off a stream that still carries them, and a keeper that read the
// stream before the move never puts them back: on a stream whose subjects capture the role lanes
// it sends nothing and reports why, so its stale write cannot land after the start's. The test
// forces that order through the keeper's read. Its next pass, on the stream the start moved them
// off, restores the byte limit with no role lane in the list.
func TestTheKeeperNeverPutsBackARoleLaneAStartMovedOffTheStream(t *testing.T) {
	js := streamTestJetStream(t)
	roleLane := contracts.RoleTopicPrefix + ">"
	legacy := *streamCfg
	legacy.Subjects = append(slices.Clone(streamSubjects), roleLane)
	legacy.Metadata = map[string]string{StreamIDMetadataKey: "legacy-id"}
	if _, err := js.AddStream(&legacy); err != nil {
		t.Fatalf("deploy the stream with the role lanes: %v", err)
	}
	limited := parseConnectOptions([]ConnectOption{WithStreamMaxBytes(keeperByteLimit)}).streamConfig()
	defaults := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	keeper := &interleavedJetStream{JetStreamContext: js, afterRead: func() {
		if err := ensureStreamWithConfig(js, &defaults); err != nil {
			t.Fatalf("the start that moves the role lanes off: %v", err)
		}
	}}
	restored, err := keepStream(keeper, &limited)
	if err == nil || !strings.Contains(err.Error(), roleLane) || len(restored) != 0 {
		t.Fatalf("the keeper's pass on the stream it read with the role lanes = %v (%v), want an error naming %s", restored, err, roleLane)
	}
	if n := keeper.updates.Load(); n != 0 {
		t.Fatalf("the keeper sent %d stream updates, want none", n)
	}
	if subjects := deployedStream(t, js).Config.Subjects; streamCapturesRoleLanes(subjects) {
		t.Fatalf("subjects %v capture the role lanes after the start moved them off", subjects)
	}

	restored, err = keepStream(js, &limited)
	if want := []string{fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit)}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the keeper's next pass restored %v (%v), want %v", restored, err, want)
	}
	if subjects := deployedStream(t, js).Config.Subjects; !slices.Equal(subjects, streamSubjects) {
		t.Fatalf("subjects %v after the keeper's pass, want %v", subjects, streamSubjects)
	}
}

// The keeper's subject list is the deployed one with only this binary's subjects added that it
// lacks and that overlap none deployed: it keeps every deployed subject, a foreign one, a widened
// one and a narrowed one included.
func TestTheKeepersSubjectsAddOnlyItsOwnThatOverlapNothingDeployed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		deployed, own []string
		want          []string
	}{
		{"a missing subject", []string{"notifications.agent.>"}, []string{"notifications.agent.>", "notifications.slack.>"}, []string{"notifications.agent.>", "notifications.slack.>"}},
		{"a foreign subject", []string{"notifications.agent.>", "notifications.retired.>"}, []string{"notifications.agent.>"}, []string{"notifications.agent.>", "notifications.retired.>"}},
		{"a narrowed own subject", []string{"notifications.github.>"}, []string{"notifications.github.pr.>"}, []string{"notifications.github.>"}},
		{"a widened own subject", []string{"notifications.github.pr.>"}, []string{"notifications.github.>"}, []string{"notifications.github.pr.>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keptSubjects(tc.deployed, tc.own); !slices.Equal(got, tc.want) {
				t.Fatalf("keptSubjects(%v, %v) = %v, want %v", tc.deployed, tc.own, got, tc.want)
			}
		})
	}
}

// Two releases with different subject lists, one adding a subject and one adding another with a
// subject the first compiles narrowed, never switch the stream back and forth: the second's keeper
// adds its new subject once and leaves the first's wide subject in place, and from then on neither
// keeper sends an update, with both new subjects in the list.
func TestTwoKeepersWithDifferentSubjectListsNeverSwitchTheStream(t *testing.T) {
	js := streamTestJetStream(t)
	alpha := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	alpha.Subjects = append(slices.Clone(streamSubjects), "notifications.alpha.>")
	beta := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	beta.Subjects = append(slices.Clone(streamSubjects), "notifications.beta.>")
	beta.Subjects[slices.Index(beta.Subjects, "notifications.github.>")] = "notifications.github.pr.>"
	if err := ensureStreamWithConfig(js, &alpha); err != nil {
		t.Fatalf("the alpha release's start: %v", err)
	}

	restored, err := keepStream(js, &beta)
	if want := []string{"subjects added notifications.beta.>"}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the beta release's keeper restored %v (%v), want %v", restored, err, want)
	}
	counting := &countingJetStream{JetStreamContext: js}
	for range 3 {
		for _, cfg := range []*nats.StreamConfig{&alpha, &beta} {
			if restored, err := keepStream(counting, cfg); err != nil || len(restored) != 0 {
				t.Fatalf("a keeper pass with subjects %v restored %v (%v), want nothing", cfg.Subjects, restored, err)
			}
		}
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("the keepers wrote %v, want nothing", writes)
	}
	if want, subjects := append(slices.Clone(alpha.Subjects), "notifications.beta.>"), deployedStream(t, js).Config.Subjects; !slices.Equal(subjects, want) {
		t.Fatalf("subjects %v, want %v", subjects, want)
	}
}

// An operator update that lowers the stream to one copy, removes its byte limit, deletes its id,
// lengthens its age, sets DiscardOld, removes one of this binary's subjects and adds a foreign one
// is undone within a few periods by the keeper of a program whose settings are three copies and a
// byte limit, which logs each field it restored. The id it restores is derived, and stays as it is
// over the passes after. The removed subject comes back at the end of the list, and the foreign
// subject stays: the keeper only adds subjects.
func TestTheKeeperRestoresWhatAnOperatorTookAwayAndKeepsAForeignSubject(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	logs := captureLogs(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), withKeepInterval(testKeepInterval))
	info := testnats.AwaitSettledStream(t, js, Stream)
	const removed = "notifications.legion.>"

	lowered := info.Config
	lowered.Replicas = 1
	lowered.MaxBytes = -1
	lowered.Metadata = maps.Clone(info.Config.Metadata)
	delete(lowered.Metadata, StreamIDMetadataKey)
	lowered.Subjects = append(slices.DeleteFunc(slices.Clone(info.Config.Subjects), func(subject string) bool { return subject == removed }), "operations.foreign.>")
	lowered.MaxAge = info.Config.MaxAge + time.Hour
	lowered.Discard = nats.DiscardOld
	if _, err := js.UpdateStream(&lowered); err != nil {
		t.Fatalf("operator update: %v", err)
	}

	restored := awaitKept(t, js, Stream, "three copies, the byte limit and an id", keptAtSettings(""))
	if want := append(slices.Clone(lowered.Subjects), removed); !slices.Equal(restored.Config.Subjects, want) {
		t.Fatalf("subjects after the keeper's restore = %v, want %v", restored.Config.Subjects, want)
	}
	if restored.Config.MaxAge != streamCfg.MaxAge || restored.Config.Discard != nats.DiscardNew {
		t.Fatalf("age %s, discard %s after the keeper's restore; want %s, %s", restored.Config.MaxAge, restored.Config.Discard, streamCfg.MaxAge, nats.DiscardNew)
	}
	id := restored.Config.Metadata[StreamIDMetadataKey]
	if want := deployedStreamID(restored.Created); id != want {
		t.Fatalf("restored id = %q, want the derived %q", id, want)
	}
	heldFor(t, js, Stream, "the restored configuration", 4, func(info *nats.StreamInfo) bool {
		return reflect.DeepEqual(info.Config, restored.Config)
	})

	logged := restoredLog(t, logs)
	for _, want := range []string{
		"replicas 1 -> 3",
		fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit),
		fmt.Sprintf("max_age %s -> %s", lowered.MaxAge, streamCfg.MaxAge),
		"discard DiscardOld -> DiscardNew",
		StreamIDMetadataKey + " added as " + id,
		"subjects added " + removed,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the keeper's lines restored %q, want %q among them", logged, want)
		}
	}
}

// An operator's raise of the byte limit, as of a full stream, stands: the keepers of two programs
// whose setting is the old limit leave the raised limit as it is over several periods, logging
// nothing, and a start with that setting writes nothing. Once an operator lowers the limit below
// the setting, the keeper raises it back to the setting and logs it.
func TestTheKeeperKeepsAnOperatorsRaiseOfTheByteLimitAndRaisesALowerOne(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	logs := captureLogs(t)
	settings := []ConnectOption{WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), withKeepInterval(testKeepInterval)}
	startOnCluster(t, cluster, settings...)
	info := testnats.AwaitSettledStream(t, js, Stream)

	raise := info.Config
	raise.MaxBytes = 4 * keeperByteLimit
	if _, err := js.UpdateStream(&raise); err != nil {
		t.Fatalf("operator raise: %v", err)
	}
	raised := testnats.AwaitSettledStream(t, js, Stream).Config
	if raised.MaxBytes != raise.MaxBytes {
		t.Fatalf("stream byte limit = %d after the operator's raise, want %d", raised.MaxBytes, raise.MaxBytes)
	}
	unchanged := func(info *nats.StreamInfo) bool { return reflect.DeepEqual(info.Config, raised) }
	heldFor(t, js, Stream, "the operator's raise", 6, unchanged)

	counting := &countingJetStream{JetStreamContext: js}
	cfg := parseConnectOptions(settings).streamConfig()
	if err := ensureStreamWithConfig(counting, &cfg); err != nil {
		t.Fatalf("start with the old limit: %v", err)
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("a start with the old limit wrote %v, want nothing", writes)
	}
	startOnCluster(t, cluster, settings...)
	heldFor(t, js, Stream, "the operator's raise", 6, unchanged)
	if logged := restoredLog(t, logs); logged != "" {
		t.Fatalf("the keepers restored %q over the operator's raise, want nothing", logged)
	}

	lower := raised
	lower.MaxBytes = keeperByteLimit / 2
	if _, err := js.UpdateStream(&lower); err != nil {
		t.Fatalf("operator lowering: %v", err)
	}
	awaitKept(t, js, Stream, "the byte limit", func(info *nats.StreamInfo) bool { return info.Config.MaxBytes == keeperByteLimit })
	awaitRestoredLog(t, logs, fmt.Sprintf("max_bytes %d -> %d", lower.MaxBytes, keeperByteLimit))
}

// The keeper raises a bucket its program names (WithKeptBuckets) back to the deployment's copies
// after an operator lowered it, changing nothing else about it, and logs the raise. A bucket it
// does not name stays lowered. A named bucket that does not exist, as one a program opens only
// for a feature it has not configured, is skipped: the keeper creates nothing, updates nothing and
// logs nothing about it.
func TestTheKeeperRaisesTheBucketsItsProgramNamesBackToTheirCopies(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	logs := captureLogs(t)
	const listed, unlisted, missing = "envoy_keeper_listed", "envoy_keeper_unlisted", "envoy_keeper_missing"

	watcher := testnats.Connect(t, cluster.URL())
	t.Cleanup(watcher.Close)
	var missingRequests atomic.Int32
	for _, subject := range []string{"$JS.API.STREAM.UPDATE.KV_" + missing, "$JS.API.STREAM.CREATE.KV_" + missing} {
		if _, err := watcher.Subscribe(subject, func(*nats.Msg) { missingRequests.Add(1) }); err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}

	client := startOnCluster(t, cluster, WithReplicas(3), WithKeptBuckets(listed, missing), withKeepInterval(testKeepInterval))
	before := map[string]nats.StreamConfig{}
	for _, bucket := range []string{listed, unlisted} {
		if _, err := EnsureKeyValue(client.Conn, client.JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3, History: 5, TTL: time.Hour}); err != nil {
			t.Fatalf("open %s at three copies: %v", bucket, err)
		}
		stream := "KV_" + bucket
		info := testnats.AwaitSettledStream(t, js, stream)
		before[bucket] = info.Config
		lowered := info.Config
		lowered.Replicas = 1
		if _, err := js.UpdateStream(&lowered); err != nil {
			t.Fatalf("lower %s to one copy: %v", stream, err)
		}
	}

	raised := awaitKept(t, js, "KV_"+listed, "three copies", func(info *nats.StreamInfo) bool { return info.Config.Replicas == 3 })
	if !reflect.DeepEqual(raised.Config, before[listed]) {
		t.Fatalf("raised %s = %+v, want it as it was before it was lowered, %+v", listed, raised.Config, before[listed])
	}
	heldFor(t, js, "KV_"+unlisted, "one copy", 4, func(info *nats.StreamInfo) bool { return info.Config.Replicas == 1 })

	if _, err := js.StreamInfo("KV_" + missing); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("read KV_%s = %v, want it still missing", missing, err)
	}
	if n := missingRequests.Load(); n != 0 {
		t.Fatalf("the keeper sent %d create or update requests for the missing bucket, want none", n)
	}
	if failures := logs.recordsWithMsg(t, "envoy nats keeper could not raise a bucket"); len(failures) != 0 {
		t.Fatalf("the keeper logged failed raises %v, want none", failures)
	}
	raises := map[string]any{}
	for _, record := range logs.recordsWithMsg(t, "envoy nats keeper raised a bucket") {
		raises[fmt.Sprint(record["bucket"])] = record["replicas"]
	}
	if want := map[string]any{listed: "1 -> 3"}; !reflect.DeepEqual(raises, want) {
		t.Fatalf("the keeper's raises logged %v, want %v", raises, want)
	}
}

// The keeper of a program with the default settings, one copy and no byte limit, on a stream that
// has an id, sends no stream update over several periods, whatever the stream's copies and limit:
// it never lowers either, and raises no bucket it names, at one copy as well. The update count is
// read from the API subject itself.
func TestAKeeperWithTheDefaultSettingsSendsNoUpdate(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit))
	testnats.AwaitSettledStream(t, js, Stream)

	watcher := testnats.Connect(t, cluster.URL())
	t.Cleanup(watcher.Close)
	var updates atomic.Int32
	if _, err := watcher.Subscribe("$JS.API.STREAM.UPDATE.>", func(*nats.Msg) { updates.Add(1) }); err != nil {
		t.Fatalf("watch stream updates: %v", err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}

	client := startOnCluster(t, cluster, WithKeptBuckets("envoy_keeper_default"), withKeepInterval(testKeepInterval))
	if _, err := EnsureKeyValue(client.Conn, client.JS(), &nats.KeyValueConfig{Bucket: "envoy_keeper_default", Storage: nats.FileStorage}); err != nil {
		t.Fatalf("open a bucket at the default copies: %v", err)
	}
	time.Sleep(6 * testKeepInterval)
	if n := updates.Load(); n != 0 {
		t.Fatalf("the default program sent %d stream updates, want none", n)
	}

	// The watch sees an update when one is sent, so the count above is not vacuous.
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if _, err := js.UpdateStream(&info.Config); err != nil {
		t.Fatalf("update the stream: %v", err)
	}
	deadline := time.Now().Add(keepWait)
	for updates.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watch did not see an update sent to the stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close and Drain end the keeper before they return, and a pass the stop interrupts logs nothing:
// the test holds the keeper's update of a lowered stream until the stop has closed the connection,
// so the update fails as a pass in flight at a shutdown does, and the keeper logs no failure for
// it. Once the client stops, the stream stays at one copy.
func TestStoppingTheClientStopsItsKeeper(t *testing.T) {
	for _, stop := range []struct {
		name string
		stop func(*Client)
	}{
		{name: "close", stop: (*Client).Close},
		{name: "drain", stop: func(client *Client) { _ = client.Drain(5 * time.Second) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			cluster, js := startClusterJetStream(t)
			logs := captureLogs(t)
			client := startOnCluster(t, cluster, WithReplicas(3), withKeepInterval(testKeepInterval))
			info := testnats.AwaitSettledStream(t, js, Stream)

			entered := make(chan struct{})
			client.mu.Lock()
			conn := client.Conn
			client.js = &interleavedJetStream{JetStreamContext: client.js, beforeUpdate: func() {
				close(entered)
				for deadline := time.Now().Add(keepWait); !conn.IsClosed() && time.Now().Before(deadline); {
					time.Sleep(5 * time.Millisecond)
				}
			}}
			client.mu.Unlock()
			lowered := info.Config
			lowered.Replicas = 1
			if _, err := js.UpdateStream(&lowered); err != nil {
				t.Fatalf("lower the stream to one copy: %v", err)
			}
			select {
			case <-entered:
			case <-time.After(keepWait):
				t.Fatalf("the keeper sent no update for the lowered stream within %s", keepWait)
			}

			stop.stop(client)
			select {
			case <-client.keeperDone:
			default:
				t.Fatalf("%s returned with the keeper still running", stop.name)
			}
			if failures := logs.recordsWithMsg(t, "envoy nats keeper could not restore the stream"); len(failures) != 0 {
				t.Fatalf("the keeper logged %v for the pass the stop interrupted, want nothing", failures)
			}
			heldFor(t, js, Stream, "one copy", 6, func(info *nats.StreamInfo) bool { return info.Config.Replicas == 1 })
		})
	}
}

// The keeper of an owning client that connects in the background runs no pass before that
// client's first reconcile has succeeded (ensureStreamOnce). The deployed stream holds
// notifications.github.*, which the reconcile replaces with this binary's notifications.github.>
// (reconcileSubjects), and no byte limit, which the keeper restores. A pass that read the stream
// before the reconcile's write and wrote after it would put the replaced subject back, and as the
// keeper only adds subjects, nothing would ever remove it again. The client is made while the
// server is down and its keeper's period is a millisecond, so a keeper started before the reconcile
// races it on most runs; with the gate the replaced subject never comes back over many periods, and
// no keeper pass fails on a stream not reconciled yet.
func TestTheKeeperOfABackgroundClientRunsNoPassBeforeItsFirstReconcile(t *testing.T) {
	const replaced, replacing = "notifications.github.*", "notifications.github.>"
	srv, uri := testnats.Start(t)
	deploy := testnats.Connect(t, uri)
	js, err := deploy.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	deployed := *streamCfg
	deployed.Subjects = slices.Clone(streamSubjects)
	deployed.Subjects[slices.Index(deployed.Subjects, replacing)] = replaced
	deployed.Metadata = map[string]string{StreamIDMetadataKey: "deployed-id"}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("deploy the stream holding %s: %v", replaced, err)
	}
	deploy.Close()
	srv.Stop(t)

	logs := captureLogs(t)
	client, err := ConnectOwningStream([]string{uri}, WithConnectInBackground(), WithStreamMaxBytes(keeperByteLimit), withKeepInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("connect owning the stream while the server is down: %v", err)
	}
	t.Cleanup(client.Close)
	srv.Start(t)

	reader := testnats.Connect(t, uri)
	t.Cleanup(reader.Close)
	if js, err = reader.JetStream(); err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	reconciled := func(info *nats.StreamInfo) bool {
		return slices.Contains(info.Config.Subjects, replacing) && !slices.Contains(info.Config.Subjects, replaced)
	}
	for deadline := time.Now().Add(keepWait); ; {
		info, err := js.StreamInfo(Stream)
		if err == nil && reconciled(info) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first connection's reconcile did not replace %s with %s within %s: %+v (%v)", replaced, replacing, keepWait, info, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A hundred keeper periods: a pass that read the stream before the reconcile lands in them.
	for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline); {
		info, err := js.StreamInfo(Stream)
		if err != nil {
			t.Fatalf("read %s: %v", Stream, err)
		}
		if !reconciled(info) {
			t.Fatalf("subjects after the reconcile = %v, want %s kept and %s gone", info.Config.Subjects, replacing, replaced)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if failures := logs.recordsWithMsg(t, "envoy nats keeper could not restore the stream"); len(failures) != 0 {
		t.Fatalf("the keeper logged %v, want no pass before the reconcile", failures)
	}
}

// An owning client that connects in the background and stops before its server ever answered
// started no keeper, and Close and Drain return at once rather than wait for one.
func TestStoppingABackgroundClientBeforeItsFirstConnectionStartsNoKeeper(t *testing.T) {
	for _, stop := range []struct {
		name string
		stop func(*Client)
	}{
		{name: "close", stop: (*Client).Close},
		{name: "drain", stop: func(client *Client) { _ = client.Drain(5 * time.Second) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			srv.Stop(t)
			client, err := ConnectOwningStream([]string{uri}, WithConnectInBackground(), withKeepInterval(time.Millisecond))
			if err != nil {
				t.Fatalf("connect owning the stream while the server is down: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			stopped := make(chan struct{})
			go func() {
				stop.stop(client)
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s of a client that never connected did not return within 10s", stop.name)
			}
			client.mu.Lock()
			started := client.keeperDone != nil
			client.mu.Unlock()
			if started {
				t.Fatalf("a client that never reconciled the stream started a keeper")
			}
		})
	}
}

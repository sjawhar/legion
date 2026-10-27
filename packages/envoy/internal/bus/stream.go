package bus

import (
	"cmp"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

const Stream = "ENVOY_NOTIFICATIONS"

// The JetStream KV buckets Envoy's stores keep, each named for what it holds.
const (
	// InterestBucket holds each session's interests, the topics it subscribes to, keyed by session
	// id (internal/store).
	InterestBucket = "envoy_interests"
	// RoleClaimBucket holds each role lane's claim, its holder session, keyed by role
	// (internal/store).
	RoleClaimBucket = "envoy_roles"
	// SessionBucket holds each live session's registration, keyed by session id
	// (internal/session).
	SessionBucket = "envoy_sessions"
	// CIStateBucket holds the CI state of each commit of a pull request (internal/cistore).
	CIStateBucket = "envoy_ci_state"
	// WebhookRedeliveryBucket holds the cursor and records of Dispatch's sweep of the GitHub App
	// webhook's failed deliveries (internal/dispatch/redeliver).
	WebhookRedeliveryBucket = "envoy_webhook_redelivery"
)

// NotificationTopicPrefix begins every notification topic: the subjects the stream carries, the
// role lanes and the exception lanes.
const NotificationTopicPrefix = "notifications."

// ExceptionTopicPrefix begins a delivery's exception lane: a failed or refused delivery is reported
// on ExceptionTopicPrefix followed by its original topic.
const ExceptionTopicPrefix = NotificationTopicPrefix + "envoy.exceptions."

// DispatchDeliveryDurable is the stream's durable consumer Dispatch's delivery intake binds, over
// every GitHub notification (internal/dispatch/delivery).
const DispatchDeliveryDurable = "delivery-events"

// DispatchJobDurable is the stream's durable consumer Dispatch's job intake binds, over a deploy
// run's job events (internal/dispatch/delivery).
const DispatchJobDurable = "delivery-jobs"

// ListenerDurable is the durable consumer of the stream that the listener of machine machineID
// binds. A space in the machine id is a hyphen in the name.
func ListenerDurable(machineID string) string {
	return "listener-" + strings.ReplaceAll(machineID, " ", "-")
}

// ListenerQueue is the queue group the listener of machine machineID subscribes the role lanes in,
// so NATS hands each role message to one of the machine's listeners while a rolling deploy runs two.
// A space in the machine id is a hyphen in the name.
func ListenerQueue(machineID string) string {
	return "envoy-listener-" + strings.ReplaceAll(machineID, " ", "-")
}

// ListenerLiveQueue is the queue group the listener of machine machineID subscribes its live lane in
// (every notifications topic, over core NATS), so NATS hands each message to one of the machine's
// listeners while a rolling deploy runs two. It must differ from ListenerQueue: NATS merges
// matching queue subscriptions by queue name across subjects and hands each message to one member,
// so a live lane in the role lanes' group could take a role message its role lane never sees. A
// space in the machine id is a hyphen in the name.
func ListenerLiveQueue(machineID string) string {
	return "envoy-listener-live-" + strings.ReplaceAll(machineID, " ", "-")
}

// streamDuplicateWindow covers the entire retained notification lifetime, so
// an outbox retry after a crash before published_at is recorded cannot create a
// second retained Dispatch event while the original remains observable, and a
// webhook redelivery of an event the stream already holds adds no second copy.
// GitHub redelivers only deliveries from the past three days, so its whole
// redelivery horizon falls inside this window.
//
// It is also the window of the dashboard's retry promise, which the stream holds only in part: it
// keeps one copy of a repeat and reports it duplicate, while a core-NATS subscriber is handed the
// repeat anyway and drops it itself. The promise, and the part each side plays, is stated once on
// DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts, from which contracts.DeliveryDuplicateWindow
// is generated.
const streamDuplicateWindow = contracts.DeliveryDuplicateWindow

var streamSubjects = []string{
	"notifications.agent.>",
	"notifications.dispatch.>",
	"notifications.github.>",
	"notifications.slack.>",
	// The Go Legion daemon's notices for the project's controller
	// (notifications.legion.<project>.controller); its workflow notices go to each owning
	// architect's role topic.
	"notifications.legion.>",
	"notifications.ghostwispr.>",
	"notifications.whatsapp.>",
	ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">",
}

// StreamSubjects reports the durable notification subjects, excluding role lanes.
func StreamSubjects() []string {
	return slices.Clone(streamSubjects)
}

// StreamCaptures reports whether the stream retains a publish to topic: some subject this binary
// compiles into the stream matches it. Every such subject is a literal prefix followed by `.>`
// (TestEveryStreamSubjectIsALiteralPrefixAndWildcard), which matches a literal topic exactly when
// the topic extends that prefix by at least one token, so no subject is split. A topic is read
// literally: a `*` or `>` in it is a character of the topic, not a wildcard, which is what nats.go
// publishes it as.
func StreamCaptures(topic string) bool {
	for _, subject := range streamSubjects {
		prefix := strings.TrimSuffix(subject, ">")
		if len(topic) > len(prefix) && strings.HasPrefix(topic, prefix) {
			return true
		}
	}
	return false
}

// SubjectsCapture reports whether a stream carrying subjects retains a publish to topic: some
// subject matches it, token by token, a `*` matching one token and a trailing `>` one or more. It
// takes any subject shape, since the subjects it is given are the deployed stream's, which a newer
// build may have shaped otherwise than StreamCaptures assumes. A topic is read literally, as
// StreamCaptures reads it.
func SubjectsCapture(subjects []string, topic string) bool {
	topicTokens := strings.Split(topic, ".")
	return slices.ContainsFunc(subjects, func(subject string) bool {
		subjectTokens := strings.Split(subject, ".")
		for index, token := range subjectTokens {
			if token == ">" {
				return len(topicTokens) > index
			}
			if index >= len(topicTokens) || (token != "*" && token != topicTokens[index]) {
				return false
			}
		}
		return len(subjectTokens) == len(topicTokens)
	})
}

// ReadStreamSubjects reads the subjects the deployed stream carries, which may hold subjects this
// build does not compile: a newer build adds its own, and reconciliation keeps them.
func ReadStreamSubjects(js nats.JetStreamContext) ([]string, error) {
	info, err := js.StreamInfo(Stream)
	if err != nil {
		return nil, err
	}
	return info.Config.Subjects, nil
}

// streamConfigGeneration is the generation of streamCfg's settings, stamped on the stream as its
// streamGenerationKey metadata. Every change to streamCfg's MaxAge, Duplicates or Subjects takes a
// new generation and a new row in TestTheStreamConfigEqualsItsGenerationsPinnedRow. A build that
// finds a higher generation deployed leaves the stream as it is (ensureStreamWithConfig), so while
// two builds run, the older one never reverts what the newer one set.
const streamConfigGeneration = 1

// streamGenerationKey is the stream metadata key that holds the generation of its settings. A
// stream without it was last set by a build before generations: generation 0.
const streamGenerationKey = "dispatch.stream-generation"

// streamCfg is this build's stream. Its Replicas is 0: an ensure then keeps the deployed replica
// count on update and creates the stream with one replica (ensureStreamWithConfig), unless the
// client names a count (WithReplicas).
var streamCfg = &nats.StreamConfig{
	Name:      Stream,
	Subjects:  streamSubjects,
	Retention: nats.LimitsPolicy,
	// Retention equals the duplicate window by construction: the dashboard promises a
	// same-mode retry cannot deliver twice for exactly as long as the stream can still
	// recognise the repeat, and a deployment where the two differ would break that promise.
	MaxAge:     streamDuplicateWindow,
	Duplicates: streamDuplicateWindow,
	Storage:    nats.FileStorage,
}

// roleLaneSubjects are the role lanes and their exceptions twin. They travel over core NATS and
// the stream must never retain them.
var roleLaneSubjects = []string{
	contracts.RoleTopicPrefix + ">",
	ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">",
}

// subjectCapturesRoleLanes reports whether some role-lane subject would land in a stream carrying
// subject.
func subjectCapturesRoleLanes(subject string) bool {
	return slices.ContainsFunc(roleLaneSubjects, func(roleLane string) bool { return subjectsOverlap(subject, roleLane) })
}

func streamCapturesRoleLanes(subjects []string) bool {
	return slices.ContainsFunc(subjects, subjectCapturesRoleLanes)
}

func purgeLegacyRoleMessages(js nats.JetStreamContext) error {
	for _, subject := range roleLaneSubjects {
		if err := js.PurgeStream(Stream, &nats.StreamPurgeRequest{Subject: subject}); err != nil {
			return err
		}
	}
	return nil
}

func migrateLegacyConsumerFilters(js nats.JetStreamContext, subjects []string) error {
	for name := range js.ConsumerNames(Stream) {
		info, err := js.ConsumerInfo(Stream, name)
		if err != nil {
			return err
		}
		if info.Config.FilterSubject != "notifications.>" {
			continue
		}
		config := info.Config
		config.FilterSubject = ""
		config.FilterSubjects = slices.Clone(subjects)
		if _, err := js.UpdateConsumer(Stream, &config); err != nil {
			return err
		}
	}
	return nil
}

func migrateRoleLanesOffStream(js nats.JetStreamContext, oldConfig, newConfig *nats.StreamConfig) error {
	if !streamCapturesRoleLanes(oldConfig.Subjects) || streamCapturesRoleLanes(newConfig.Subjects) {
		return nil
	}
	if err := migrateLegacyConsumerFilters(js, newConfig.Subjects); err != nil {
		return err
	}
	return purgeLegacyRoleMessages(js)
}

// subjectsOverlap reports whether some subject matches both patterns. JetStream refuses two such
// subjects in one stream.
func subjectsOverlap(a, b string) bool {
	aTokens, bTokens := strings.Split(a, "."), strings.Split(b, ".")
	for index := 0; index < len(aTokens) && index < len(bTokens); index++ {
		aToken, bToken := aTokens[index], bTokens[index]
		if aToken == ">" || bToken == ">" {
			return true
		}
		if aToken != "*" && bToken != "*" && aToken != bToken {
			return false
		}
	}
	return len(aTokens) == len(bTokens)
}

// streamReconciliation is the subject list the stream carries once this build has ensured it, and
// what that did to the deployed list: the list is the deployed one, then each of this build's
// subjects the deployed list lacks. Every process of the listener and of Dispatch's server ensures
// this one stream (bus.ConnectOwningStream), on each connect, and they deploy separately: during a
// release two builds of each run at once. So a deployed subject this build does not know may be one
// another live process still needs; the ensure keeps it and names it in foreign. Two kinds of
// deployed subject go:
//   - one that captures the role lanes, which travel over core NATS and must never be retained
//     (migrateRoleLanesOffStream);
//   - one that overlaps a subject of this build's (a widened, narrowed or split subject), because
//     JetStream refuses both in one stream and the ensure would fail. This build's shape wins
//     (replaced), and the next ensure of a build with the other shape puts that one back.
//
// Reconciliation runs only on a stream stamped with this build's generation or an older one: a
// stream a newer build stamped keeps its subjects as they are (ensureStreamWithConfig).
//
// Retiring any other subject is an operator step, taken once no deployment compiled with it can
// start again: `nats stream edit ENVOY_NOTIFICATIONS --subjects=... -f`.
type streamReconciliation struct {
	subjects []string
	replaced []subjectReplacement
	foreign  []string
}

// subjectReplacement is a deployed subject a start dropped and every one of its own subjects that
// overlapped it.
type subjectReplacement struct {
	dropped string
	kept    []string
}

func reconcileSubjects(deployed, own []string) streamReconciliation {
	result := streamReconciliation{subjects: make([]string, 0, len(deployed)+len(own))}
	for _, subject := range deployed {
		if subjectCapturesRoleLanes(subject) {
			continue
		}
		if !slices.Contains(own, subject) {
			var overlapping []string
			for _, ownSubject := range own {
				if subjectsOverlap(subject, ownSubject) {
					overlapping = append(overlapping, ownSubject)
				}
			}
			if len(overlapping) > 0 {
				result.replaced = append(result.replaced, subjectReplacement{dropped: subject, kept: overlapping})
				continue
			}
			result.foreign = append(result.foreign, subject)
		}
		result.subjects = append(result.subjects, subject)
	}
	for _, subject := range own {
		if !slices.Contains(result.subjects, subject) {
			result.subjects = append(result.subjects, subject)
		}
	}
	return result
}

// log reports the reconciliation once the stream carries it.
func (r streamReconciliation) log() {
	for _, replacement := range r.replaced {
		slog.Warn("envoy nats stream subject replaced by an overlapping one", slog.String("dropped", replacement.dropped), slog.Any("kept", replacement.kept))
	}
	if len(r.foreign) > 0 {
		slog.Info("envoy nats stream keeps subjects this binary does not compile", slog.Any("subjects", r.foreign))
	}
}

// ensureStreamWithConfig creates the stream as cfg describes it, or reconciles the deployed stream
// with cfg, and returns the subjects the stream carries afterwards.
//
// A deployed stream stamped with a generation higher than streamConfigGeneration was set by a newer
// build, which owns its settings: it is left exactly as it is. Any other is reconciled: its subjects
// as reconcileSubjects says, its MaxAge and Duplicates to cfg's, and stamped with this build's
// generation; it is updated only when one of those differs. A cfg with Replicas 0 keeps the deployed
// replica count on update and creates the stream with one replica; any other count is set on create
// and on update.
func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) ([]string, error) {
	info, err := js.StreamInfo(Stream)
	if errors.Is(err, nats.ErrStreamNotFound) {
		created := *cfg
		created.Replicas = cmp.Or(created.Replicas, 1)
		created.Metadata = stampGeneration(nil)
		if _, err := js.AddStream(&created); err != nil {
			return nil, err
		}
		return created.Subjects, nil
	}
	if err != nil {
		return nil, err
	}
	deployedGeneration := streamGeneration(info.Config.Metadata)
	if deployedGeneration > streamConfigGeneration {
		slog.Info("envoy nats stream settings belong to a newer build, so this build leaves them as they are",
			slog.Int("deployed_generation", deployedGeneration), slog.Int("own_generation", streamConfigGeneration))
		return info.Config.Subjects, nil
	}
	reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
	desired := *cfg
	desired.Subjects = reconciliation.subjects
	desired.Replicas = cmp.Or(desired.Replicas, info.Config.Replicas)
	desired.Metadata = stampGeneration(info.Config.Metadata)
	if deployedGeneration == streamConfigGeneration &&
		info.Config.MaxAge == desired.MaxAge &&
		info.Config.Duplicates == desired.Duplicates &&
		slices.Equal(info.Config.Subjects, desired.Subjects) {
		reconciliation.log()
		return info.Config.Subjects, nil
	}
	migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(desired.Subjects)
	if err := migrateRoleLanesOffStream(js, &info.Config, &desired); err != nil {
		return nil, err
	}
	if _, err = js.UpdateStream(&desired); err != nil {
		return nil, err
	}
	reconciliation.log()
	if migratingRoleLanes {
		if err := purgeLegacyRoleMessages(js); err != nil {
			return nil, err
		}
	}
	return desired.Subjects, nil
}

// streamGeneration is the generation metadata stamps on a stream: 0 when it holds none, as a stream
// a build before generations last set does, or holds one that is not a number.
func streamGeneration(metadata map[string]string) int {
	generation, err := strconv.Atoi(metadata[streamGenerationKey])
	if err != nil {
		return 0
	}
	return generation
}

// stampGeneration is the metadata an ensure sends: the deployed stream's own keys, then this build's
// generation. It leaves out the keys nats-server sets itself (the `_nats.` prefix), which the server
// sets again on every create and update.
func stampGeneration(deployed map[string]string) map[string]string {
	metadata := make(map[string]string, len(deployed)+1)
	for key, value := range deployed {
		if !strings.HasPrefix(key, "_nats.") {
			metadata[key] = value
		}
	}
	metadata[streamGenerationKey] = strconv.Itoa(streamConfigGeneration)
	return metadata
}

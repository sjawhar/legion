package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

const Stream = "ENVOY_NOTIFICATIONS"

// StreamIDMetadataKey is the stream configuration's metadata key holding the stream's identity:
// set once, when the stream is created or when a start first finds it without one, and never
// changed after. It lives in the configuration, so a backup and restore, a reconcile and a leader
// election all keep it. The stream's Created time is no identity: a restore changes it, and after
// a clustered restore the servers can report different Created times (per nats-server's clustered
// restore code), so it can change again at a later leader election. POST /v1/replay answers the
// key's value as stream.id (cmd/listener/replay.go).
const StreamIDMetadataKey = "envoy_stream_id"

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

// Every subject of streamSubjects is classified for POST /v1/replay (cmd/listener/replay.go) by
// whether a topic under it ever carries one agent session's private payload, read from the code
// that publishes it. A shared subject's topics are replayed to any session that registered them as
// interests; a private subject's never are, since a registered interest is any topic a caller
// names, and a session replays only its own agent subject by name. Each subject is in exactly one
// list (TestEveryStreamSubjectIsClassifiedForReplay), so a new stream subject must be classified.
var (
	sharedStreamSubjects = []string{
		// Dispatch's outbox: an issue's, a document's or a project's events (internal/dispatch/outbox
		// envelope), each readable on its owner in Dispatch, and Dispatch's start (startup.Subject); a
		// copy for one session goes to that session's agent subject, and an issue-less message has
		// no owner and is not published (outbox publish).
		"notifications.dispatch.>",
		// A repository's GitHub webhook events (internal/webhook github, contracts.GithubEnvelopes)
		// and the CI settlements derived from them (internal/cistore).
		"notifications.github.>",
		// A Slack channel's or thread's messages and app mentions (internal/webhook slack,
		// contracts.SlackEnvelopes).
		"notifications.slack.>",
		// A Ghost Wispr recording's events, under its recording id, not an agent session's
		// (internal/webhook ghostwispr, contracts.GhostWisprEnvelope).
		"notifications.ghostwispr.>",
		// A WhatsApp chat's messages the MCP bridge relays (internal/mcpbridge BuildEnvelope).
		"notifications.whatsapp.>",
	}
	privateStreamSubjects = []string{
		// Each session's own inbox: Dispatch's routes, follower and author copies, and every
		// message sent to one session (contracts.AgentSubject).
		contracts.AgentTopicPrefix + ">",
		// The Legion daemon's notices for one project's controller (daemon notify.ControllerTopic),
		// which its role holder hears through the role and never registers.
		"notifications.legion.>",
		// A failed delivery's report, carrying the undelivered message's payload and the session it
		// was for (cmd/listener publishDeliveryException); a session's own lane included.
		ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">",
	}
)

// SharedStreamTopic reports whether every topic subject matches lies under one shared stream
// subject (sharedStreamSubjects), under NATS wildcard rules (SubjectContains): a topic no session's
// private payload is ever published on.
func SharedStreamTopic(subject string) bool {
	return slices.ContainsFunc(sharedStreamSubjects, func(shared string) bool { return SubjectContains(shared, subject) })
}

// ReachesPrivateStreamSubject reports whether a topic sessionID registers as an interest reaches a
// private stream subject (privateStreamSubjects) other than sessionID's own agent subject: it
// overlaps one under NATS wildcard rules (subjectsOverlap), so another session's inbox, a
// failed-delivery lane (a session's own included), the Legion controller lane, and wildcards
// reaching any of them (notifications.>, notifications.*.<x>, notifications.envoy.>) count, and it
// is not sessionID's own agent subject as one literal subject. A session id holding `*` or `>`
// makes a wildcard of its "own" agent subject, which is no session's alone and counts too.
func ReachesPrivateStreamSubject(sessionID, topic string) bool {
	if !slices.ContainsFunc(privateStreamSubjects, func(private string) bool { return subjectsOverlap(topic, private) }) {
		return false
	}
	return topic != contracts.AgentSubject(sessionID) || strings.ContainsAny(sessionID, "*>")
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

// streamCfg is the stream this binary creates, before ConnectOwningStream sets its replicas
// (WithReplicas) and byte limit (WithStreamMaxBytes). Its age equals the duplicate window by
// construction: the dashboard promises a same-mode retry cannot deliver twice for exactly as long
// as the stream can still recognise the repeat, and a deployment where the two differed would break
// that promise. Once a byte limit binds, the stream discards its oldest messages first and may hold
// less than the window: a repeat of a message it has already dropped is then stored again.
var streamCfg = &nats.StreamConfig{
	Name:       Stream,
	Subjects:   streamSubjects,
	Retention:  nats.LimitsPolicy,
	MaxAge:     streamDuplicateWindow,
	Duplicates: streamDuplicateWindow,
	Discard:    nats.DiscardOld,
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

// streamReconciliation is the subject list the stream carries once this binary has started, and
// what that did to the deployed list: the list is the deployed one, then each of this binary's
// subjects the deployed list lacks. Every bus.ConnectOwningStream caller ensures this one stream
// (the deployed listener and Dispatch's server, wherever they run), and they deploy separately,
// so a deployed subject this binary does not know may be one another live deployment still
// needs; start-up keeps it and names it in foreign. Two kinds of deployed subject go:
//   - one that captures the role lanes, which travel over core NATS and must never be retained
//     (migrateRoleLanesOffStream);
//   - one that overlaps a subject of this binary's (a widened, narrowed or split subject), because
//     JetStream refuses both in one stream and the start would fail. This binary's shape wins
//     (replaced), and the next start of a binary with the other shape puts that one back.
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

// ensureStreamWithConfig creates the stream with cfg, under a fresh StreamIDMetadataKey, or
// reconciles the deployed one towards cfg. cfg's Replicas is the deployment's replica count and its
// MaxBytes the deployment's byte limit, 0 when it sets none.
//
// The update starts from the deployed configuration and sets only what this binary owns:
//   - the subjects (reconcileSubjects), the age and the duplicate window, cfg's;
//   - the replicas, the deployed count or cfg's when that is more: a start never lowers them, so a
//     program whose setting is lower (an older setting, or one restarted before the others) leaves
//     a raised stream as it is, and lowering is an operator step;
//   - the byte limit, cfg's when it sets one and the deployed limit otherwise, so a program that
//     sets none never removes another's, and removing one is an operator step;
//   - discard, DiscardOld, so a full stream drops its oldest messages and still takes a publish;
//   - the metadata, the deployed metadata kept as it is, its StreamIDMetadataKey included; a stream
//     without that key gets one (identifiedMetadata).
//
// Every other deployed setting (storage, placement, compression, the message count and size
// limits, direct get and the rest) stays as it is. A stream that already holds all of that gets no
// update, and one that lacks only the key gets its own configuration back with the key added.
// A refused create or update names the settings it carried (refusedSettings).
//
// Every bus.ConnectOwningStream caller runs this on the one stream, and JetStream's stream update
// has no compare-and-swap, so two starts can each read the stream before either writes, and the
// later write wins. A deployment sets one replica count and one byte limit for all its programs,
// so both writes carry the same values and the race is harmless. One race does show: nats-server
// can leave one of two replica changes proposed at once unanswered (seen with v2.10.29,
// TestTwoStartsRaisingOneStreamAtOnceBothSucceed), so an update whose answer never came is checked
// against the stream read again, and a stream that by then needs nothing more is the start's
// success (unanswered). A stream one start creates first is reconciled by the other, keeping the
// id it was created with (AddStream answers ErrStreamNameAlreadyInUse, as its config's id
// differs). Two that both find no id write the same one, derived from what both read.
func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) error {
	info, err := js.StreamInfo(Stream)
	if errors.Is(err, nats.ErrStreamNotFound) {
		created := *cfg
		created.Metadata = maps.Clone(cfg.Metadata)
		if created.Metadata == nil {
			created.Metadata = map[string]string{}
		}
		created.Metadata[StreamIDMetadataKey] = id.New()
		if _, err = js.AddStream(&created); !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			return refusedSettings(err, nil, &created)
		}
		info, err = js.StreamInfo(Stream)
	}
	if err != nil {
		return err
	}
	desired, reconciliation := reconciledStream(info, cfg)
	if reflect.DeepEqual(desired, info.Config) {
		reconciliation.log()
		return nil
	}
	migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(desired.Subjects)
	if err := migrateRoleLanesOffStream(js, &info.Config, &desired); err != nil {
		return err
	}
	if _, err = js.UpdateStream(&desired); err != nil && !(unanswered(err) && streamReconciled(js, cfg)) {
		return refusedSettings(err, &info.Config, &desired)
	}
	reconciliation.log()
	if migratingRoleLanes {
		return purgeLegacyRoleMessages(js)
	}
	return nil
}

// reconciledStream is the configuration ensureStreamWithConfig writes over the deployed stream info
// describes, and the subject reconciliation in it: the deployed configuration with only what this
// binary owns set from cfg.
func reconciledStream(info *nats.StreamInfo, cfg *nats.StreamConfig) (nats.StreamConfig, streamReconciliation) {
	reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
	desired := info.Config
	desired.Subjects = reconciliation.subjects
	desired.MaxAge = cfg.MaxAge
	desired.Duplicates = cfg.Duplicates
	desired.Replicas = max(info.Config.Replicas, cfg.Replicas)
	if cfg.MaxBytes > 0 {
		desired.MaxBytes = cfg.MaxBytes
	}
	desired.Discard = nats.DiscardOld
	desired.Metadata = identifiedMetadata(info)
	return desired, reconciliation
}

// streamReconciled reports whether the stream, read again, already holds everything cfg asks of it.
func streamReconciled(js nats.JetStreamContext, cfg *nats.StreamConfig) bool {
	info, err := js.StreamInfo(Stream)
	if err != nil {
		return false
	}
	desired, _ := reconciledStream(info, cfg)
	return reflect.DeepEqual(desired, info.Config)
}

// unanswered reports whether err is a JetStream request's wait running out with no answer, rather
// than the server's refusal.
func unanswered(err error) bool {
	return errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// refusedSettings is err, the server's answer to request, a create (deployed nil) or an update of
// the stream deployed, named with each deployment setting whose value request changes:
// ReplicasEnvVar when it sets more than one copy the stream does not have (a create, or a raise),
// StreamMaxBytesEnvVar when it sets a byte limit the stream does not have. The bus is handed the
// numbers and not where they came from, so the names are those of the settings every deployed
// program reads them from (WithReplicas, WithStreamMaxBytes). One copy is never named: a server
// refuses no stream for having one. nil, and a refusal of a request that changes neither, are
// returned as they are.
func refusedSettings(err error, deployed, request *nats.StreamConfig) error {
	if err == nil {
		return nil
	}
	var settings []string
	if request.Replicas > 1 && (deployed == nil || request.Replicas != deployed.Replicas) {
		settings = append(settings, fmt.Sprintf("%s=%d", ReplicasEnvVar, request.Replicas))
	}
	if request.MaxBytes > 0 && (deployed == nil || request.MaxBytes != deployed.MaxBytes) {
		settings = append(settings, fmt.Sprintf("%s=%d", StreamMaxBytesEnvVar, request.MaxBytes))
	}
	if len(settings) == 0 {
		return err
	}
	return fmt.Errorf("%s: %w", strings.Join(settings, ", "), err)
}

// identifiedMetadata is a copy of the deployed stream's metadata that holds a StreamIDMetadataKey:
// the stream's own when it has one, deployedStreamID otherwise.
func identifiedMetadata(info *nats.StreamInfo) map[string]string {
	metadata := maps.Clone(info.Config.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	if metadata[StreamIDMetadataKey] == "" {
		metadata[StreamIDMetadataKey] = deployedStreamID(info.Created)
	}
	return metadata
}

// deployedStreamID is the StreamIDMetadataKey a start gives a deployed stream that has none: 16
// bytes of the SHA-256 of the stream's name and the Created time STREAM.INFO answered, in hex. Every
// start that reads the stream from one leader before any of them writes derives the same id, so
// writers racing to add it write one value, where a random id would leave the last writer's.
func deployedStreamID(created time.Time) string {
	sum := sha256.Sum256([]byte(Stream + "\x00" + created.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:16])
}

package bus

import (
	"errors"
	"log/slog"
	"slices"
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
	Replicas:   1,
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

func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) error {
	info, err := js.StreamInfo(Stream)
	if err == nil {
		reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
		desired := *cfg
		desired.Subjects = reconciliation.subjects
		if info.Config.MaxAge == desired.MaxAge &&
			info.Config.Duplicates == desired.Duplicates &&
			slices.Equal(info.Config.Subjects, desired.Subjects) {
			reconciliation.log()
			return nil
		}
		migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(desired.Subjects)
		if err := migrateRoleLanesOffStream(js, &info.Config, &desired); err != nil {
			return err
		}
		if _, err = js.UpdateStream(&desired); err != nil {
			return err
		}
		reconciliation.log()
		if migratingRoleLanes {
			return purgeLegacyRoleMessages(js)
		}
		return nil
	}
	if !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}
	_, err = js.AddStream(cfg)
	return err
}

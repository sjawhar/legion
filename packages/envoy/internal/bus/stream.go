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

// ListenerBuckets reports the KV buckets envoy-listener opens, in the order its NATS grant lists
// them: the interest and role-claim buckets (internal/store), the session registry
// (internal/session), and the CI state bucket, which only a listener with the GitHub webhook route
// opens (internal/cistore). The listener's keeper raises each of them (WithKeptBuckets), and
// envoy-infra grants the listener and the host each one's calls
// (packages/envoy/infra/core/permissions.go), so a bucket named here is both kept and granted.
// Each call returns a fresh slice, so no caller changes another's list.
func ListenerBuckets() []string {
	return []string{InterestBucket, RoleClaimBucket, SessionBucket, CIStateBucket}
}

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

// RoleLaneQueue is the queue group every listener subscribes the role lanes in, whatever its
// machine, so NATS hands each role message to one listener and the holder's stream copy is
// forwarded once. A listener whose NATS user may not subscribe in it (a hub whose grant predates
// it) subscribes in its machine's group, ListenerQueue, instead (ConnectRoleLane).
const RoleLaneQueue = "envoy-roles"

// DeliverSubjectPrefix begins the relay subject a listener publishes a session's consumer
// messages on, DeliverSubject. No stream subject captures it (StreamCaptures), so a relay frame
// travels over core NATS and the stream stores nothing twice.
const DeliverSubjectPrefix = NotificationTopicPrefix + "envoy.deliver."

// DeliverSubject is the relay subject of session sessionID: the subject the listener that owns the
// session publishes each message of its consumer (SessionConsumerName) on, and the session
// subscribes to. Every message a session gets travels on it; a plain subscription of the session's
// own carries only a lane receipt, a live ad-hoc topic and the core fallback ("Session consumers"
// in packages/envoy/AGENTS.md).
func DeliverSubject(sessionID string) string {
	return DeliverSubjectPrefix + sessionID
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
	// A team's own topic, the one namespace an agent names itself
	// (notifications.team.deploy-squad): the listener's publish route stores it so a session
	// that was not connected reads it from its consumer later, and refuses by name every other
	// notifications. topic no subject here captures (cmd/listener/api.go).
	"notifications.team.>",
	ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">",
	ExceptionTopicPrefix + "notifications.team.>",
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
		// A team's own topic, published by the listener's route for whoever registered it
		// (cmd/listener publishHandler): a group's lane carries no one session's private
		// payload, as its sender names the team and not a session.
		"notifications.team.>",
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
		// A failed delivery on a team topic, carrying that message's payload and the session it
		// was for, as the agent lane above does; only the exception's own reader may have it.
		ExceptionTopicPrefix + "notifications.team.>",
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
// that promise. A full stream refuses a new message (DiscardNew) rather than drop a stored one, so a
// stored message leaves only by age: the stream holds every message it took for the whole window,
// byte limit or not, and the promise holds. The refused publish's sender gets ErrStreamFull, which
// is not an ErrRefused: the same publish is taken once age has made room, on a clustered stream
// too, where a retry under the refused message's MsgId is acknowledged a duplicate with no sequence
// and the bus sends it once more without the id (ErrNotStored).
var streamCfg = &nats.StreamConfig{
	Name:       Stream,
	Subjects:   streamSubjects,
	Retention:  nats.LimitsPolicy,
	MaxAge:     streamDuplicateWindow,
	Duplicates: streamDuplicateWindow,
	Discard:    nats.DiscardNew,
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
//   - the subjects (reconcileSubjects);
//   - the age and the duplicate window, cfg's, and discard, cfg's DiscardNew, so a full stream
//     refuses a new message and keeps every stored one (a deployed DiscardOld stream is switched);
//   - the replicas, the deployed count or cfg's when that is more: a start never writes fewer
//     copies than it read, so a program whose setting is lower (an older setting, or one restarted
//     before the others) leaves a raised stream it reads as it is, though not one raised after its
//     read (below); lowering is an operator step that needs every program's setting lowered first;
//   - the byte limit, the deployed limit or cfg's when that is more, cfg's on a stream with none
//     and the deployed one when cfg sets none: a start never writes a lower limit than it read, so
//     an operator's raise stands, and lowering or removing one is an operator step that needs
//     every program's setting lowered or removed first;
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
// later write wins. A start can therefore lose to a concurrent start with other settings: one that
// read the stream before this start's write and writes after it puts back the subjects, the
// copies, the byte limit and the metadata it read. The client this start returns keeps the stream
// (keepStream): within one keeper period it raises the copies and the byte limit to what its
// settings ask for where the stream holds less, puts back StreamIDMetadataKey, the age, the
// duplicate window and discard, and adds back each of its own subjects the stream lost that
// overlaps none the stream carries, so the race ends at the most copies and the largest byte limit
// any program asks for. Programs of a deployment with different ENVOY_NATS_REPLICAS or
// ENVOY_STREAM_MAX_BYTES therefore do not fight over the stream: neither is ever lowered, and the
// largest stands. Two races show even with the same settings, both of them settled where the
// update is sent (updateStream): nats-server can leave one of two replica changes proposed at once
// unanswered (seen with v2.10.29), and every nats-server before v2.12.5 can refuse the later of
// two updates of one stream as overlapping an existing stream, the stream itself, when the other
// update lands while the server checks this one (TestTwoStartsRaisingOneStreamAtOnceBothSucceed). A
// stream one start creates first is reconciled by the other, keeping the id it was created with
// (AddStream answers ErrStreamNameAlreadyInUse, as its config's id differs). Two that both find no
// id write the same one, derived from what both read.
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
	if err := updateStream(js, &desired, func(again *nats.StreamInfo) bool {
		reconciled, _ := reconciledStream(again, cfg)
		return reflect.DeepEqual(reconciled, again.Config)
	}); err != nil {
		return refusedSettings(err, &info.Config, &desired)
	}
	reconciliation.log()
	if migratingRoleLanes {
		return purgeLegacyRoleMessages(js)
	}
	return nil
}

// reconciledStream is the configuration ensureStreamWithConfig writes over the deployed stream info
// describes, and the subject reconciliation in it: the deployed configuration with what this binary
// owns set from cfg (ownedStream) and the subjects reconciled (reconcileSubjects).
func reconciledStream(info *nats.StreamInfo, cfg *nats.StreamConfig) (nats.StreamConfig, streamReconciliation) {
	reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
	desired := ownedStream(info, cfg)
	desired.Subjects = reconciliation.subjects
	return desired, reconciliation
}

// keptStream is the configuration the keeper (keepStream) writes over the deployed stream info
// describes: the deployed configuration with what this binary owns set from cfg (ownedStream) and
// the deployed subjects with this binary's own added where the keeper may (keptSubjects).
func keptStream(info *nats.StreamInfo, cfg *nats.StreamConfig) nats.StreamConfig {
	kept := ownedStream(info, cfg)
	kept.Subjects = keptSubjects(info.Config.Subjects, cfg.Subjects)
	return kept
}

// ownedStream is the deployed configuration info describes with every setting this binary owns
// but the subjects set from cfg, as both a start (reconciledStream) and the keeper (keptStream)
// write it: the replicas, the deployed count or cfg's when that is more; the byte limit, the
// deployed limit or cfg's when that is more, cfg's on a stream with none and the deployed one when
// cfg sets none; the age, the duplicate window and discard, cfg's; and the metadata with a
// StreamIDMetadataKey (identifiedMetadata). Neither the copies nor the byte limit is ever lowered,
// so an operator's raise of either stands; lowering one is an operator step once every program's
// setting is lowered. The age, the window and discard are compiled into the binary, not deployment
// settings (streamCfg), and every release that runs a keeper compiles the same three (72 hours, 72
// hours, DiscardNew), so the keeper writing them puts back only what an operator changed. A release
// that changed one would have its keeper and an older release's each put their own value back
// every period while both run.
func ownedStream(info *nats.StreamInfo, cfg *nats.StreamConfig) nats.StreamConfig {
	owned := info.Config
	owned.Replicas = max(info.Config.Replicas, cfg.Replicas)
	if cfg.MaxBytes > 0 {
		owned.MaxBytes = max(info.Config.MaxBytes, cfg.MaxBytes)
	}
	owned.MaxAge = cfg.MaxAge
	owned.Duplicates = cfg.Duplicates
	owned.Discard = cfg.Discard
	owned.Metadata = identifiedMetadata(info)
	return owned
}

// keptSubjects is the subject list the keeper writes: the deployed list, then each of own, this
// binary's subjects, that the list lacks, that captures no role lane (subjectCapturesRoleLanes) and
// that overlaps no deployed subject. It only adds: unlike a start (reconcileSubjects) it removes and
// replaces nothing, so two releases with different subject lists never switch the stream back and
// forth, and a subject of this binary's that another release's overlapping one replaced stays
// replaced until this binary's next start.
func keptSubjects(deployed, own []string) []string {
	kept := slices.Clone(deployed)
	for _, subject := range own {
		if subjectCapturesRoleLanes(subject) || slices.ContainsFunc(kept, func(other string) bool { return subjectsOverlap(subject, other) }) {
			continue
		}
		kept = append(kept, subject)
	}
	return kept
}

// updateStream sends update, a whole stream configuration, and succeeds when the server accepts
// it. Two other answers come of another update of the same stream landing at once rather than of
// update itself, and after either the stream update names is read again, the update succeeding
// when the stream holds what the caller asked for (holds):
//   - an answer that never came (unanswered): nats-server can leave one of two replica changes
//     proposed at once unanswered (seen with v2.10.29). That update may still land, so it is not
//     sent again, and a stream that does not hold it returns the update's own error;
//   - a refusal of the update's subjects as overlapping an existing stream (overlapRefused), which
//     every nats-server before v2.12.5 can give the later of two updates of one stream: the server
//     reads the stream's assignment, releases its lock to check the new configuration, and then
//     looks for an overlap among the account's assignments skipping only the assignment it read,
//     so an update that lands in between leaves the stream overlapping itself
//     (jsClusteredStreamUpdateRequest, v2.10.29 server/jetstream_cluster.go; nats-server commit
//     cb1afd68d014, first released in v2.12.5, skips by name). Nothing was written, so a stream
//     that does not hold the update is sent it again, the write a server without that race would
//     have taken after the other, up to overlapSends sends in all. A stream another stream really
//     does overlap is refused every send, and the last refusal is returned.
//
// Any other refusal is the update's own error.
func updateStream(js nats.JetStreamContext, update *nats.StreamConfig, holds func(*nats.StreamInfo) bool) error {
	settled := func() bool {
		again, err := js.StreamInfo(update.Name)
		return err == nil && holds(again)
	}
	var err error
	for range overlapSends {
		if _, err = js.UpdateStream(update); !overlapRefused(err) {
			break
		}
		if settled() {
			return nil
		}
	}
	if unanswered(err) && settled() {
		return nil
	}
	return err
}

// overlapSends bounds the sends of one update the server refuses as overlapping an existing stream
// (updateStream). Each such refusal of a stream no other stream overlaps means another update of
// that stream landed while the server checked this one, so five sends outlast four such updates in
// a row, and a stream another stream does overlap costs four sends and five stream reads more
// before its refusal is returned.
const overlapSends = 5

// jsStreamSubjectOverlap is nats-server's error code for a stream create or update whose subjects
// overlap an existing stream's (JSStreamSubjectOverlapErr). nats.go names no constant for it.
const jsStreamSubjectOverlap = 10065

// overlapRefused reports whether err is nats-server's refusal of a stream's subjects as
// overlapping an existing stream's.
func overlapRefused(err error) bool {
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jsStreamSubjectOverlap
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

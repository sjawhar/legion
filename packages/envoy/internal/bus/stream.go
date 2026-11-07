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
	"strconv"
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
	// DPoPProofBucket holds one record per RFC 9449 proof a listener has already spent, keyed by
	// the proof's 'jti' and target URI, so a captured proof presented a second time inside its
	// acceptance window is refused by whichever listener of a deployment receives it
	// (cmd/listener/brokerauth.go). Its records are the only ones Envoy keeps on a bucket TTL
	// alone: nothing reads a record back, and a key that has outlived the window its proof could
	// be presented in says nothing, so each key expires rather than being deleted.
	DPoPProofBucket = "envoy_dpop_proofs"
	// WebhookRedeliveryBucket holds the cursor and records of Dispatch's sweep of the GitHub App
	// webhook's failed deliveries (internal/dispatch/redeliver).
	WebhookRedeliveryBucket = "envoy_webhook_redelivery"
	// ListenerStateBucket holds the state every listener pod of a machine shares: the machine's
	// delivery lease (ListenerLeaseKey, cmd/listener), the clean-hand-back marks of its stopping
	// pods (ListenerHandBackPrefix), its delivery ledger (ListenerLedgerPrefix, internal/dedupe)
	// and every session's last-seen record (LastSeenPrefix, internal/session).
	ListenerStateBucket = "envoy_listener"
)

// ListenerBuckets reports the KV buckets envoy-listener opens, in the order its NATS grant lists
// them: the interest and role-claim buckets (internal/store), the session registry
// (internal/session), the CI state bucket, which only a listener with the GitHub webhook route
// opens (internal/cistore), and the spent-proof bucket, which only a listener that takes broker
// access tokens opens (cmd/listener/brokerauth.go). The listener's keeper raises each of them
// (WithKeptBuckets), and envoy-infra grants the listener and the host each one's calls
// (packages/envoy/infra/core/permissions.go), so a bucket named here is both kept and granted.
// Each call returns a fresh slice, so no caller changes another's list.
func ListenerBuckets() []string {
	return []string{InterestBucket, RoleClaimBucket, SessionBucket, CIStateBucket, DPoPProofBucket}
}

// LastSeenPrefix begins each session's last-seen record in ListenerStateBucket. The listener's
// grant writes these keys for every session, whichever machine registered it: a role claim
// (RoleClaimBucket, one bucket for the whole fleet) names its holder session and no machine, so
// GET /v1/roles/<role> on any listener answers a lapsed holder's last_seen out of that session's
// record whichever machine the holder registered through. A key scoped to the writing machine
// would leave every such read with no record to find.
const LastSeenPrefix = "seen."

// ListenerLeaseKey is the key of machine machineID's delivery lease in ListenerStateBucket. The
// listener's grant writes this key and no other machine's.
func ListenerLeaseKey(machineID string) string {
	return "lease." + ListenerDurable(machineID)
}

// ListenerLedgerPrefix begins each key of the delivery ledger in ListenerStateBucket of the
// listeners that bind durable (ListenerDurable). The listener's grant writes its own machine's
// ledger and no other.
func ListenerLedgerPrefix(durable string) string {
	return "delivery." + durable + "."
}

// ListenerHandBackPrefix begins each clean-hand-back mark in ListenerStateBucket of the listeners
// that bind durable (ListenerDurable): a pod that stopped gracefully writes
// `handback.<durable>.<revision>` to vouch for the lease record it released at that revision, and
// the pod that takes that record reads the mark before it shortens its settle hold
// (cmd/listener, lease.MarkHandBack and lease.HandBackMarked). The listener's grant writes its own
// machine's marks and no other's, and the bucket's MaxAge removes a mark nobody reads. A mark is
// keyed by the revision it vouches for, not by the machine alone, so a stopping pod writes
// nothing about the lease record once another pod has taken it.
func ListenerHandBackPrefix(durable string) string {
	return "handback." + durable + "."
}

// NotificationTopicPrefix begins every notification topic: the subjects the stream carries, the
// role lanes and the exception lanes.
const NotificationTopicPrefix = "notifications."

// ExceptionTopicPrefix begins a delivery's exception lane: a failed or refused delivery is reported
// on ExceptionTopicPrefix followed by its original topic.
const ExceptionTopicPrefix = NotificationTopicPrefix + "envoy.exceptions."

// LegionWakeTopic is the one topic the listener reports a wake of a released Legion issue pod
// on: a message published to a role topic with no live holder, a message sent to the agent
// subject of a session that is gone, and the listener's own `listener_ready`. It is a new leaf
// under notifications.legion.>, which the stream already carries, so it is retained with no
// stream change and no new grant. Only the listener sees these, and it does not know which
// daemon owns a session: every Legion daemon consumes the topic on a durable of its own and acts
// only on a released claim of its own project.
const LegionWakeTopic = NotificationTopicPrefix + "legion.wake"

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

// KeptTopicPrefix begins the listener's kept lane: the role messages it kept because it could not
// deliver them and the drain cursors over them (cmd/listener/role_keep.go). The listener alone
// publishes under it and reads it, no session may register a topic under it
// (ReachesPrivateStreamSubject), and no consumer of the notification stream delivers one: the
// stream carries the prefix (streamCarriedSubjects) where no consumer filter does
// (StreamSubjects).
const KeptTopicPrefix = NotificationTopicPrefix + "envoy.kept."

// KeptRoleSubjectPrefix begins the kept lane of every role (KeptRoleSubject): what a reader of all
// of them filters the stream's subjects on and reads each role's own token back out of
// (Client.KeptRoleLanes).
const KeptRoleSubjectPrefix = KeptTopicPrefix + "role."

// KeptRoleSubject is the kept lane of role: the role messages the listener could not deliver, each
// held until the role is claimed and then forwarded to the new holder. The role name is written
// into the segment as contracts.RoleTopicPrefix writes it, unsanitized, so a role's kept subject
// follows from its role topic.
func KeptRoleSubject(role string) string {
	return KeptRoleSubjectPrefix + role
}

// KeptRoleCursorSubject is the drain cursor of role: how far the listener has read that role's
// kept lane, so a kept message already forwarded is not forwarded again. The role name is written
// into the segment as contracts.RoleTopicPrefix writes it, unsanitized, so a role's cursor subject
// follows from its role topic.
func KeptRoleCursorSubject(role string) string {
	return KeptTopicPrefix + "cursor." + role
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

// streamSubjects are the notification subjects the stream carries, and the list every consumer
// filter over them takes (StreamSubjects). The stream carries one subject more, the listener's own
// kept lane (streamCarriedSubjects).
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

// streamCarriedSubjects is every subject the stream itself carries: the notification subjects and
// the listener's kept lane, the role messages it could not deliver and the per-role drain cursors
// over them. That prefix is the stream's alone and no consumer filter takes it: a kept copy still
// carries its role topic in the envelope, so a consumer handed one would deliver it as a role
// message and re-trigger role delivery on every listener. The listener reads the lane itself
// instead (cmd/listener/role_keep.go).
var streamCarriedSubjects = slices.Concat(streamSubjects, []string{KeptTopicPrefix + ">"})

// Every subject of streamCarriedSubjects is classified for POST /v1/replay (cmd/listener/replay.go) by
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
		// A role message the listener could not deliver, carrying that message's payload and the
		// holder it was meant for, and the per-role drain cursor over them: no session may
		// register a topic under it and no replay hands one out.
		KeptTopicPrefix + ">",
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

// StreamSubjects reports the notification subjects the stream carries, excluding the role lanes
// and the listener's kept lane: the list every consumer filter built over the notification
// subjects uses. The stream's own list, which carries the kept lane too, is StreamCarriedSubjects.
func StreamSubjects() []string {
	return slices.Clone(streamSubjects)
}

// StreamCarriedSubjects reports every subject the stream itself carries (streamCarriedSubjects):
// the notification subjects and the listener's kept lane. A caller that builds or checks the
// stream takes it, where a consumer's filter takes StreamSubjects.
func StreamCarriedSubjects() []string {
	return slices.Clone(streamCarriedSubjects)
}

// StreamCaptures reports whether the stream retains a publish to topic: some subject this binary
// compiles into the stream matches it. Every such subject is a literal prefix followed by `.>`
// (TestEveryStreamSubjectIsALiteralPrefixAndWildcard), which matches a literal topic exactly when
// the topic extends that prefix by at least one token, so no subject is split. A topic is read
// literally: a `*` or `>` in it is a character of the topic, not a wildcard, which is what nats.go
// publishes it as.
func StreamCaptures(topic string) bool {
	for _, subject := range streamCarriedSubjects {
		prefix := strings.TrimSuffix(subject, ">")
		if len(topic) > len(prefix) && strings.HasPrefix(topic, prefix) {
			return true
		}
	}
	return false
}

// ReadStreamSubjects reads the subjects ENVOY_NOTIFICATIONS carries now from the server, which may
// hold more than this binary compiles in (streamSubjects): a start of another build keeps every
// subject the deployed stream carries and adds its own (reconcileSubjects).
func ReadStreamSubjects(js nats.JetStreamContext) ([]string, error) {
	info, err := js.StreamInfo(Stream)
	if err != nil {
		return nil, err
	}
	return info.Config.Subjects, nil
}

// SubjectsCapture reports whether some subject of subjects matches topic by NATS's rules: a `*`
// token matches any one token, a last `>` token matches one or more tokens, and any other token
// matches itself. topic is read literally, as nats.go publishes it.
func SubjectsCapture(subjects []string, topic string) bool {
	return slices.ContainsFunc(subjects, func(subject string) bool { return subjectMatches(subject, topic) })
}

func subjectMatches(subject, topic string) bool {
	for {
		subjectToken, subjectRest, subjectMore := strings.Cut(subject, ".")
		topicToken, topicRest, topicMore := strings.Cut(topic, ".")
		if subjectToken == ">" {
			return true
		}
		if subjectToken != "*" && subjectToken != topicToken {
			return false
		}
		if !subjectMore || !topicMore {
			return subjectMore == topicMore
		}
		subject, topic = subjectRest, topicRest
	}
}

// streamMaxConsumers is the consumer cap this binary compiles into the stream (streamCfg), the one
// setting that bounds how many sessions can hold a session consumer of their own. What a stream
// with no cap means to the server changed in nats-server 2.15. From that release the server caps
// a stream with no cap, a deployed 0 and the -1 it answers for one alike, at a default of its
// own, and that default is the server's own rather than an account's: the server-wide
// JetStreamLimits.DefaultMaxConsumers, which a server whose configuration leaves it unset starts
// at JSDefaultMaxConsumersPerStream, 1000 (server/opts.go:6255-6256 in v2.15.1, the constant at
// server/jetstream_api.go:427). Such a stream fills at 1000 consumers, and every session after
// that is refused its own (`maximum consumers limit reached`, code 10026) and falls back to core
// delivery. Before 2.15 a stream with no cap has no limit at all: v2.10.29 normalises a deployed
// 0 to -1 (server/stream.go:1227) and checks the cap only while it is positive
// (server/consumer.go around 808). The hub runs 2.15.1, where the default of 1000 is the failure
// this cap answers.
//
// That 1000 is what the version gate stands for: serverDefaultsAnUnsetCap reads "nats-server 2.15
// or later" and this binary takes it to mean "a stream with no cap is capped at 1000". An
// operator can break that link two ways, and the binary checks neither. A server started with
// `jetstream { limits { default_max_consumers: <n> } }` runs with <n> in place of the 1000
// (server/opts.go:390 and :2555 in v2.15.1); at -1, or at any number above this cap, a stream
// with no cap allows more consumers than this binary compiles, and a start and the keeper then
// lower what the server allows to 24000 rather than raise it. An account whose own JetStream
// `max_consumers` is positive has that applied to every stream of the account
// (server/consumer.go:1162-1171): above this cap, a start and the keeper lower an uncapped
// stream's limit to 24000 the same way; below it, the server refuses a stream whose cap is above
// the account's (server/jetstream.go:2560-2562), so the create and the raise are both refused. An
// account limit of -1 counts as not set and changes nothing, since the server applies its default
// over it (server/consumer.go:1166-1168). No deployment of this repository sets either: the hub's
// rendered jetstream block carries max_file_store, max_memory_store and store_dir and nothing else
// (infra/gcp/clusteredhub_live_test.go), and it renders no accounts block, so every client runs
// in the global account with its JetStream limits unset.
//
// The population the cap must cover is not the live session count. Nothing deletes a session's
// consumer when the session ends: the only delete is inside SessionConsumers.RestartAtFloor
// (session_consumers.go), and the server removes a consumer SessionConsumerInactiveThreshold, 72
// hours, after its last pull. The population is therefore every distinct session that pulled
// inside those 72 hours. It was read twice on 2026-11-06, by sampling the listener's session list
// every 20 seconds and counting each id that appeared in no earlier read, which needs no
// assumption about how long a session lives: 238 reads from 01:53:37Z to 02:33:35Z gave 42 new
// ids, 63.1 an hour, 36 of them stamped inside the window by their UUIDv7, 54.0 an hour, against
// 663 live; 136 reads from 04:23:40Z to 05:09:16Z gave 61 new ids, 80.3 an hour, 57 of them
// stamped inside it, 75.0 an hour, against 668 live. The population is live + 72 x rate: 4,551 to
// 5,206 by the first reading and 6,068 to 6,447 by the second.
//
// dispatch://INFRA-514's capacity gate asks for at least twice the larger of the stream's consumer
// count and that population, so the gate's own number is 9,102 to 12,894 across the two readings.
// 24000 is 1.9 to 2.6 times the gate's number and 3.7 to 5.3 times the population. The second
// reading sets it: at the fleet's growth between the two, a cap only 1.2 to 1.3 times the gate's
// own number would need raising again within a release or two, and the cap costs nothing while it
// is not reached, since the server allocates per live consumer. It costs what its consumers cost
// when they do reach it, measured twice on nats-server 2.15.1, 2,000 consumers of the listener's
// own shape (session_consumers.go) on a file-backed stream: 54.7 and 52.6 KiB resident and 0.56
// KiB of store per consumer, so 24000 consumers are about 1.20 to 1.25 GiB of memory and about 13
// MiB of store, against the hub's 16 GiB machine, where nats-server held 1.10 GiB resident on
// 2026-10-30.
const streamMaxConsumers = 24000

// streamMaxConsumersFloor is the first nats-server that takes a change of MaxConsumers on a stream
// that already exists. Every server before it refuses such an update with maxConsumersRefusal,
// measured on v2.10.29, v2.11.0 and v2.12.0; v2.12.5 and later take it. A create carries the cap
// on every version, so under an older server the stream holds the cap only from the create, and a
// refused raise names the floor (refusedSettings).
const streamMaxConsumersFloor = "v2.12.5"

// streamMaxConsumersDefaultMajor and streamMaxConsumersDefaultMinor are the first nats-server that
// applies a consumer cap of its own to a stream that sets none, 2.15.0: it reads such a stream as
// asking for JSDefaultMaxConsumersPerStream, which is 1000, the limit streamMaxConsumers records.
// Every server before it leaves such a stream with no limit, so there is nothing there for this
// binary's cap to fix.
const (
	streamMaxConsumersDefaultMajor = 2
	streamMaxConsumersDefaultMinor = 15
)

// serverDefaultsAnUnsetCap reports whether serverVersion is the version of a server that applies
// its own default consumer cap to a stream that sets none: nats-server
// streamMaxConsumersDefaultMajor.Minor or later. serverVersion is what the connected server
// answers, read from (*nats.Conn).ConnectedServerVersion(), and the whole question this binary
// asks of a deployed stream whose cap is -1 or 0: on such a server that stream is capped at 1000
// and is raised to streamMaxConsumers, on every older one it is unlimited and is left as it is
// (ownedStream).
//
// Only the major and the minor are read, since the first such server is a minor's own first
// release, and each is the number its field begins with, so a pre-release (`2.15.0-RC.1`) counts
// as the release it leads to and an optional leading `v` is taken as nats.go's own version parse
// takes it (versionComponents). A string that carries no major and minor - the empty string a
// client with no live connection answers among them - is no such server.
func serverDefaultsAnUnsetCap(serverVersion string) bool {
	major, rest, split := strings.Cut(strings.TrimPrefix(serverVersion, "v"), ".")
	if !split {
		return false
	}
	minor, _, _ := strings.Cut(rest, ".")
	gotMajor, gotMinor := versionNumber(major), versionNumber(minor)
	if gotMajor < 0 || gotMinor < 0 {
		return false
	}
	return gotMajor > streamMaxConsumersDefaultMajor ||
		(gotMajor == streamMaxConsumersDefaultMajor && gotMinor >= streamMaxConsumersDefaultMinor)
}

// versionNumber is the number a version's field begins with: 15 from `15` and from a
// pre-release's `15-RC` alike, and -1 from a field that begins with no digit, the empty field
// included, and from one carrying more digits than an int holds.
func versionNumber(field string) int {
	digits := 0
	for digits < len(field) && field[digits] >= '0' && field[digits] <= '9' {
		digits++
	}
	number, err := strconv.Atoi(field[:digits])
	if err != nil {
		return -1
	}
	return number
}

// streamCfg is the stream this binary creates, before ConnectOwningStream sets its replicas
// (WithReplicas) and byte limit (WithStreamMaxBytes). Its age equals the duplicate window by
// construction: the dashboard promises a same-mode retry cannot deliver twice for exactly as long
// as the stream can still recognise the repeat, and a deployment where the two differed would break
// that promise. A full stream refuses a new message (DiscardNew) rather than drop a stored one, so a
// stored message leaves only by age: the stream holds every message it took for the whole window,
// byte limit or not, and the promise holds. The refused publish's sender gets ErrStreamFull, which
// is not an ErrRefused: the same publish is taken once age has made room.
//
// It carries the consumer cap (streamMaxConsumers) as well, which a create takes on every
// nats-server; an update that changes a deployed stream's cap needs streamMaxConsumersFloor or
// later, and the reconcile owns the cap from there (ownedStream).
var streamCfg = &nats.StreamConfig{
	Name:         Stream,
	Subjects:     streamCarriedSubjects,
	Retention:    nats.LimitsPolicy,
	MaxConsumers: streamMaxConsumers,
	MaxAge:       streamDuplicateWindow,
	Duplicates:   streamDuplicateWindow,
	Discard:      nats.DiscardNew,
	Storage:      nats.FileStorage,
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

// consumerFilterSubjects is subjects without the listener's kept lane, the list a consumer's
// filter may carry: the stream's own list holds the kept prefix (streamCarriedSubjects) and no
// consumer filter ever does, since a kept copy still carries its role topic and a consumer handed
// one would deliver it as a role message, and a drain cursor is no envelope at all.
func consumerFilterSubjects(subjects []string) []string {
	return slices.DeleteFunc(slices.Clone(subjects), func(subject string) bool {
		return subjectsOverlap(subject, KeptTopicPrefix+">")
	})
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
		config.FilterSubjects = consumerFilterSubjects(subjects)
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
// MaxBytes the deployment's byte limit, 0 when it sets none. defaultsAnUnsetCap is what the
// connected server is, read from the version it answers (serverDefaultsAnUnsetCap): whether a
// deployed stream with no consumer cap is one the server caps at a default of its own.
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
//   - the consumer cap, the deployed cap or cfg's (streamMaxConsumers) when that is more, and
//     cfg's on a stream whose cap is unset, a deployed -1 or 0 alike, only where
//     defaultsAnUnsetCap holds and that stream is therefore capped at the server's own default
//     of 1000: a start never writes a lower cap than it read, so an operator's raise stands, and
//     lowering one is an operator step that needs every program's binary lowered first;
//   - the metadata, the deployed metadata kept as it is, its StreamIDMetadataKey included; a stream
//     without that key gets one (identifiedMetadata).
//
// Every other deployed setting (storage, placement, compression, the message count and size
// limits, direct get and the rest) stays as it is. A stream that already holds all of that gets no
// update, and one that lacks only the key gets its own configuration back with the key added.
// A refused create or update names the settings it carried (refusedSettings). A create carries the
// consumer cap on every nats-server, but a server before streamMaxConsumersFloor refuses an update
// that changes the cap of a stream that already exists, so on such a server a start against a
// deployed stream whose cap is positive and below cfg's is refused. A stream with no cap is sent
// no cap change at all on a server before 2.15, where an unset cap is no limit rather than the
// default of 1000, so such a server is handed no update it refuses for a stream that has nothing
// this cap fixes.
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
func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) error {
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
	desired, reconciliation := reconciledStream(info, cfg, defaultsAnUnsetCap)
	if reflect.DeepEqual(desired, info.Config) {
		reconciliation.log()
		return nil
	}
	migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(desired.Subjects)
	if err := migrateRoleLanesOffStream(js, &info.Config, &desired); err != nil {
		return err
	}
	if err := updateStream(js, &desired, func(again *nats.StreamInfo) bool {
		reconciled, _ := reconciledStream(again, cfg, defaultsAnUnsetCap)
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
// owns set from cfg (ownedStream, which defaultsAnUnsetCap reaches) and the subjects reconciled
// (reconcileSubjects).
func reconciledStream(info *nats.StreamInfo, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) (nats.StreamConfig, streamReconciliation) {
	reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
	desired := ownedStream(info, cfg, defaultsAnUnsetCap)
	desired.Subjects = reconciliation.subjects
	return desired, reconciliation
}

// keptStream is the configuration the keeper (keepStream) writes over the deployed stream info
// describes: the deployed configuration with what this binary owns set from cfg (ownedStream,
// which defaultsAnUnsetCap reaches) and the deployed subjects with this binary's own added where
// the keeper may (keptSubjects).
func keptStream(info *nats.StreamInfo, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) nats.StreamConfig {
	kept := ownedStream(info, cfg, defaultsAnUnsetCap)
	kept.Subjects = keptSubjects(info.Config.Subjects, cfg.Subjects)
	return kept
}

// ownedStream is the deployed configuration info describes with every setting this binary owns
// but the subjects set from cfg, as both a start (reconciledStream) and the keeper (keptStream)
// write it: the replicas, the deployed count or cfg's when that is more; the byte limit, the
// deployed limit or cfg's when that is more, cfg's on a stream with none and the deployed one when
// cfg sets none; the consumer cap, the deployed cap or cfg's when that is more, and cfg's on a
// stream whose cap is unset where defaultsAnUnsetCap holds; the age, the duplicate window and
// discard, cfg's; and the metadata with a StreamIDMetadataKey (identifiedMetadata). Neither the
// copies nor the byte limit nor the cap is ever lowered, so an operator's raise of any of them
// stands; lowering one is an operator step once every program's setting is lowered. The age, the
// window and discard are compiled into the binary, not deployment settings (streamCfg), and every
// release that runs a keeper compiles the same three (72 hours, 72 hours, DiscardNew), so the
// keeper writing them puts back only what an operator changed. A release that changed one would
// have its keeper and an older release's each put their own value back every period while both
// run.
//
// A deployed cap of -1 or 0 is a stream that sets no cap. What that means is the connected
// server's, which defaultsAnUnsetCap carries here (serverDefaultsAnUnsetCap): from nats-server
// 2.15 such a stream is capped at the server's own default of 1000
// (JSDefaultMaxConsumersPerStream), so it is raised to streamMaxConsumers as a smaller cap is;
// on every server before that it has no limit at all, so it is left exactly as it is and the
// binary writes no cap where none was asked for. A stream that already carries a positive cap
// below this binary's is raised on every server, since that cap bounds its consumers wherever it
// runs. The update that carries either raise needs streamMaxConsumersFloor or later on a stream
// that already exists (refusedSettings).
func ownedStream(info *nats.StreamInfo, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) nats.StreamConfig {
	owned := info.Config
	owned.Replicas = max(info.Config.Replicas, cfg.Replicas)
	if cfg.MaxBytes > 0 {
		owned.MaxBytes = max(info.Config.MaxBytes, cfg.MaxBytes)
	}
	if cfg.MaxConsumers > 0 && (info.Config.MaxConsumers > 0 || defaultsAnUnsetCap) {
		owned.MaxConsumers = max(info.Config.MaxConsumers, cfg.MaxConsumers)
	}
	owned.MaxAge = cfg.MaxAge
	owned.Duplicates = cfg.Duplicates
	owned.Discard = cfg.Discard
	owned.Metadata = identifiedMetadata(info)
	return owned
}

// keptSubjects is the subject list the keeper writes: the deployed list, then each of own, this
// binary's subjects, that the list lacks and that overlaps no deployed subject. It does not check
// own for a subject that captures a role lane, because no subject this binary compiles captures
// one, which TestStreamConfigExcludesRoleLanes in stream_config_test.go guarantees. It only adds:
// unlike a start (reconcileSubjects) it removes and replaces nothing, so two releases with
// different subject lists never switch the stream back and forth, and a subject of this binary's
// that another release's overlapping one replaced stays replaced until this binary's next start.
func keptSubjects(deployed, own []string) []string {
	kept := slices.Clone(deployed)
	for _, subject := range own {
		if slices.ContainsFunc(kept, func(other string) bool { return subjectsOverlap(subject, other) }) {
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

// maxConsumersRefusal is what nats-server answers an update that changes a stream's MaxConsumers
// with, word for word, on every server before streamMaxConsumersFloor: the text it hands
// NewJSStreamInvalidConfigError (server/stream.go), measured on v2.10.29, v2.11.0 and v2.12.0,
// which nats.go gives the caller as the whole description of an *nats.APIError, under its own
// `nats: ` prefix.
const maxConsumersRefusal = "stream configuration update can not change MaxConsumers"

// maxConsumersRefused reports whether err is that refusal, read by the server's own words and not
// by its code: the code is JSStreamInvalidConfigF, which nats.go names nats.JSStreamInvalidConfig
// (10052) and which carries whatever the server has to say about a stream's configuration, the
// storage type and the retention policy an update may not change among them, so a code test would
// take any of those for this one. The words are read off the error's own string, so a refusal
// still counts when something has wrapped it on its way here, and when it reaches the bus as
// anything other than an *nats.APIError.
func maxConsumersRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), maxConsumersRefusal)
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
// refuses no stream for having one. nil, and a refusal of a request that changes none of them, are
// returned as they are.
//
// An update that changes the deployed stream's consumer cap names the cap too, under the name
// nats-server spells it (max_consumers), with the value this binary asks for; the cap is compiled
// in rather than read from a variable (streamMaxConsumers), so it is named as the field and not
// as a setting an operator can change. That name carries the first server which can take the
// change (streamMaxConsumersFloor) only when the server's answer is its own refusal of the change
// (maxConsumersRefused), a bare maxConsumersRefusal that says neither which field nor which
// server takes it. Every other refusal of such an update - a raise of the copies the cluster has
// no room for on a server that would have taken the cap, an overlap still refused after its
// sends - names the cap alone, as the copies and the byte limit are named, since the server the
// cap needs is nothing to an operator whose update was refused for another reason. A create is
// never named: a create carries the cap on every nats-server.
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
	if deployed != nil && request.MaxConsumers != deployed.MaxConsumers {
		named := fmt.Sprintf("max_consumers %d", request.MaxConsumers)
		if maxConsumersRefused(err) {
			named += fmt.Sprintf(" (only nats-server %s and later can change it on an existing stream)", streamMaxConsumersFloor)
		}
		settings = append(settings, named)
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

// Intake (LEGION-567): a Dispatch server consumer of the GitHub events Envoy already relays over
// NATS (`notifications.github.>`), for live updates between the five-minute reconcile's passes.
// Every write here goes through the same upsert functions reconcile uses (store.go), so a NATS
// redelivery or a reconcile re-reading the same fact is a no-op, not a duplicate -- and intake
// never relies on NATS redelivery for correctness: a GitHub call that fails here is logged and
// the message is still acked, because the reconcile that runs every five minutes is the actual
// catch-up mechanism ("a missed event appears by the next five-minute reconcile", LEGION-567).
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ompSessionTrailer matches a commit message's `Omp-Session: <id>` trailer line (the same shape
// LEGION-294's own collector reads).
var ompSessionTrailer = regexp.MustCompile(`(?m)^Omp-Session:\s*(\S+)\s*$`)

// issueKeyCandidate matches a bare Dispatch issue key mention (e.g. ACME-1072), the same shape
// this codebase's own issueKeyPattern uses elsewhere (packages/envoy/internal/dispatch/api/server.go,
// packages/envoy/internal/dispatch/text/refs.go) -- duplicated here rather than imported because
// both are unexported in their own packages and this package does not depend on either.
var issueKeyCandidate = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*\b`)

// Intake wires the NATS consumer to the database, the GitHub App client and the event broker the
// issue.deployed events go through (RecordDeployedEvents). StartIntakes builds it, and only with
// the App configured.
type Intake struct {
	pool   *store.Pool
	github *githubapp.Client
	events *events.Broker
	// current is the settings the handler routes by. Read on every settingsPollInterval tick and
	// published here rather than captured by the handler: the durable's filter subject never
	// changes, so a settings change must reach the running handler without rebinding anything.
	// Only settings whose deploy repository splitRepo accepts are published, so route can compare
	// against it as it stands.
	current atomic.Pointer[DeliverySettings]
	// heldDurable is this intake's delivery from bus.DispatchDeliveryDurable under the durable's
	// hold: subscribe, stopDelivering and the take-over.
	heldDurable
}

// newIntake builds an Intake on natsClient, a connected client StartIntakes opens for it alone,
// holding its durable and taking it over under a hold and a lease it takes on leases.
func newIntake(natsClient *bus.Client, pool *store.Pool, leases intakeLeases, github *githubapp.Client, broker *events.Broker) *Intake {
	in := &Intake{pool: pool, github: github, events: broker}
	in.heldDurable = newHeldDurable(natsClient, leases, deliveryConsumerConfig, in.deliver)
	return in
}

// Run delivers GitHub events from the durable consumer while this process holds the durable's
// hold, and stands by for the hold while another process holds it, until ctx is cancelled. Idle
// until a delivery_settings row exists.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so no
// setting rebinds it. delivery_settings is read on every settingsPollInterval tick and published
// to the running handler, so a settings change -- a different deploy repository or workflow path
// -- takes effect within one poll interval with no restart. route does all the filtering, from
// those settings.
//
// Several envoy-dispatch processes run at once during a rolling release, of this build and of the
// one before it, and exactly one of them delivers from the durable: the one holding its hold
// (heldDurable). The others stand by, trying for the hold every durableBindInterval for the life
// of the process, with no deadline, and take the durable over within seconds of the holder
// releasing its hold, or of the hold lapsing when the holder dies. A holder checks every
// intakeHoldRenew that the durable still delivers to it, and stops delivering the moment its hold
// is lost. When ctx ends it stops delivering, handing back what it holds, and releases the hold,
// so a standby takes over at once (heldDurable.stop).
//
// The PR-checks workflow path is live-reachable only for a fork-originated pull request: Envoy's
// own webhook normalizer (internal/contracts/normalize.go, untouched by this package) drops a
// workflow_run envelope whose pull_requests array is non-empty, which GitHub populates for a
// same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case those
// runs never arrive here at all; the five-minute reconcile is the only path that ever records
// them. Routed rather than ignored because it is the only live-update path for the fork case,
// which does reach it.
func (in *Intake) Run(ctx context.Context) {
	reads := time.NewTicker(settingsPollInterval)
	defer reads.Stop()
	refresh := func() {
		settings, err := GetSettings(ctx, in.pool)
		if err != nil {
			if errors.Is(err, ErrNoSettings) {
				return
			}
			slog.Error("dispatch delivery: read settings for intake", "error", err)
			return
		}
		if _, _, err := splitRepo(settings.DeployRepo); err != nil {
			slog.Error("dispatch delivery: deploy_repo setting", "error", err)
			return
		}
		in.current.Store(&settings)
	}
	refresh()
	// The durable is tried only once a settings row has been read: the handler routes by those
	// settings, and an intake with none has nothing to do with a message.
	in.run(ctx, reads.C, refresh, func() bool { return in.current.Load() != nil })
}

// handle routes one envelope payload by the settings in force (route).
func (in *Intake) handle(ctx context.Context, payload string) (bool, error) {
	settings := in.current.Load()
	if settings == nil {
		return false, nil
	}
	return in.route(ctx, *settings, payload)
}

// settingsPollInterval is how often Run re-reads delivery_settings so a settings PUT reaches the
// running handler -- which repository and which workflow paths it routes by -- without an
// operator restarting the server. A var so a test can shrink it.
var settingsPollInterval = 30 * time.Second

// route dispatches one envelope payload by its kind field: a pull request to
// handlePullRequestEnvelope, a workflow run to routeWorkflowEnvelope, any other kind nowhere. It is
// the whole filter: the durable is handed every GitHub notification on the bus
// (githubIntakeSubject), CI settlements, comments and the events Envoy builds no payload for among
// them. The payload is decoded once, into its fields. An empty payload, or one of a kind route does
// not handle, is discarded before any GitHub call or write, whatever its other fields hold (a CI
// settlement's hold arrays, cistore.Summary); only a pull request's or a workflow run's string
// fields are read, as the flat string map its handler takes (flatPayload), and every other member
// is skipped. handled reports whether the kind is one route hands to a handler, whatever that
// handler then decides, so deliver records the event's time for a pull request that was not
// merged or another repository's run as well. A payload that is not a JSON object, and a kind that
// is not a string, are errMalformedPayload.
//
// An envelope whose handler stored a fact the issue.deployed rule reads (a pull request's row, a
// run, its jobs) ends with recordDeployed, whether or not the handler then failed: a run written
// before its jobs fetch failed may still be the run that shipped a pull request stored earlier.
func (in *Intake) route(ctx context.Context, settings DeliverySettings, raw string) (handled bool, err error) {
	if raw == "" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return false, fmt.Errorf("%w: %w", errMalformedPayload, err)
	}
	var kind string
	if field, ok := fields["kind"]; ok {
		if err := json.Unmarshal(field, &kind); err != nil {
			return false, fmt.Errorf("%w: kind: %w", errMalformedPayload, err)
		}
	}
	var handle func(context.Context, DeliverySettings, map[string]string) (bool, error)
	switch kind {
	case "pr":
		handle = in.handlePullRequestEnvelope
	case "workflow":
		handle = in.routeWorkflowEnvelope
	default:
		return false, nil
	}
	wrote, err := handle(ctx, settings, flatPayload(fields))
	if wrote {
		in.recordDeployed(ctx, settings)
	}
	return true, err
}

// recordDeployed writes the issue.deployed of every pull request the envelope just handled may have
// shipped or completed (RecordDeployedEvents). A failure is logged and does not fail the envelope,
// which is acknowledged as every handled envelope is: the reconcile records at the end of every
// pass too, so a row this call could not write gets its event within five minutes.
func (in *Intake) recordDeployed(ctx context.Context, settings DeliverySettings) {
	written, err := RecordDeployedEvents(ctx, in.pool, settings, in.events)
	if err != nil {
		slog.Error("dispatch delivery: record issue.deployed after an envelope", "error", err)
	}
	if written > 0 {
		slog.Info("dispatch delivery: recorded issue.deployed", "events", written, "by", "intake")
	}
}

// routeWorkflowEnvelope hands a workflow run to handleWorkflowEnvelope as the run kind its file is
// configured as. The runs this slice records are the deploy repository's own: another repository's
// run of a file with the same path is not one of them, and routing it would fetch a run this slice
// never stores. A run of a file neither setting names is not fetched either.
func (in *Intake) routeWorkflowEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) (bool, error) {
	if payload["repo"] != settings.DeployRepo {
		return false, nil
	}
	switch payload["path"] {
	case settings.DeployWorkflowPath:
		return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindDeploy, payload)
	case settings.PRChecksWorkflowPath:
		// A pr_checks run is no part of the shipping rule (shippingRunJoin reads deploy runs
		// alone), so storing one can ship nothing: the envelope ends with no deployed pass.
		_, err := in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindPRChecks, payload)
		return false, err
	default:
		return false, nil
	}
}

// errMalformedPayload is an envelope payload route cannot read: not a JSON object, or a kind that
// is not a string.
var errMalformedPayload = errors.New("malformed envelope payload")

// flatPayload is a pull request's or workflow run's string fields as the flat string map its
// handler reads. A field that is not a string is left out: payload members are additive, so a
// newer publisher may add one, and no handler reads it. A handler reads a field left out as the
// empty string, as it reads a field the payload does not carry.
func flatPayload(fields map[string]json.RawMessage) map[string]string {
	payload := make(map[string]string, len(fields))
	for name, field := range fields {
		var value string
		if json.Unmarshal(field, &value) == nil {
			payload[name] = value
		}
	}
	return payload
}

// githubIntakeSubject is the durable's one filter subject: every GitHub event Envoy relays. The
// handler decides what to do with each (route), and fetches nothing for the ones this slice does
// not want, which costs one decode of the payload per envelope.
//
// One subject rather than the set this once carried -- the wildcard pull-request subject
// notifications.github.*.*.pr.* beside one notifications.github.<owner>.<repo>.workflow.<file>.>
// per configured workflow file, through FilterSubjects -- because nats-server 2.10.29 does not
// hand that set's workflow messages over. Measured on one stream holding five pull-request and
// twelve workflow-run messages (TestWhichFilterShapeDeliversAWorkflowSubject): a durable carrying
// the set reports all of them in NumPending, then delivers the five pull requests and none of the
// runs, by pull fetch or push subscription alike. Each filter alone delivers its own messages,
// and so do two workflow filters together, or a concrete pull-request subject beside the
// workflow filter; only the wildcard pull-request filter beside it withholds the workflow
// messages, in either order. A withheld message is silent -- the consumer reports it pending and
// never errors -- so this takes the one shape that delivers everything and filters in the
// handler. It also means the filter no longer depends on the settings row, so a settings change
// needs no rebind.
const githubIntakeSubject = "notifications.github.>"

// intakeMessageTimeout bounds how long one envelope's own handling may take -- its GitHub calls
// (a pull request plus its commit pages, or a workflow run plus its jobs pages, each with this
// package's own bounded page retries) and its Postgres writes. A handler that runs past it is
// cancelled, logged and acked: the five-minute reconcile is this package's catch-up mechanism,
// so one slow envelope must not hold the consumer. A var so a test can shrink it.
var intakeMessageTimeout = time.Minute

// intakeMaxAckPending bounds how many messages NATS may have outstanding with this consumer at
// once. The handler does its GitHub calls inline on the subscription's own delivery goroutine,
// one message at a time, so the bound has to be what that one handler can acknowledge well
// inside intakeAckWait; the server's own default of 1,000 is far past it, and messages it
// redelivers under a handler still working accumulate in the client's pending buffer until that
// buffer drops them. A var so a test can shrink it.
var intakeMaxAckPending = 4

// intakeAckWait is how long NATS waits for an acknowledgement before redelivering. Derived from
// the two values above rather than chosen: the last of intakeMaxAckPending outstanding messages
// waits for the ones before it, so the bound is their total handling time, plus a minute for the
// acknowledgement itself to land. A function rather than a value so a test that shrinks either
// input gets an ack wait derived from it.
func intakeAckWait() time.Duration {
	return time.Duration(intakeMaxAckPending)*intakeMessageTimeout + time.Minute
}

// deliveryConsumerConfig is the policy a take-over creates the durable with
// (bus.DispatchDeliveryDurable, one push consumer over githubIntakeSubject), unless the deployed
// durable carries a higher generation (takeOverConfig). The server holds a backlog undelivered
// until a subscriber attaches, so a restart with events waiting loses none of them and none of
// them count against MaxAckPending in the meantime.
//
// FilterSubject, AckWait and MaxAckPending keep the values of the build before generations: that
// build's processes, still running during a rolling release, update a durable whose three differ
// from theirs in place on every settings poll, so a change to any of them waits until no such
// process is left. A change to this config takes a new consumerConfigGeneration and a new row in
// TestTheConsumerConfigEqualsItsGenerationsPinnedRow.
func deliveryConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
		Metadata:       consumerGenerationMetadata(),
	}
}

// consumerConfigGeneration is the generation of the intakes' consumer configs
// (deliveryConsumerConfig, jobConsumerConfig), stamped on each durable as its
// consumerGenerationKey metadata. A take-over that finds a durable of a higher generation
// recreates it with the deployed config rather than this build's (takeOverConfig), so while two
// builds run, the older one never reverts what the newer one set.
const consumerConfigGeneration = 1

// consumerGenerationKey is the consumer metadata key that holds the generation of its config. A
// durable without it was last created by a build before generations: generation 0.
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerGenerationMetadata is the metadata this build creates a durable with: its generation.
func consumerGenerationMetadata() map[string]string {
	return map[string]string{consumerGenerationKey: strconv.Itoa(consumerConfigGeneration)}
}

// acknowledge acks msg. An acknowledgement that does not reach the server is never discarded: it
// leaves the message outstanding, and intakeMaxAckPending of those stop the consumer being given
// anything until the ack wait expires. The Nak gives the slot back at once; when it fails too (the
// connection is down, which is why the ack failed), the ack wait is the backstop.
func acknowledge(msg *natsgo.Msg) {
	if err := msg.Ack(); err != nil {
		slog.Error("dispatch delivery: acknowledge message", "subject", msg.Subject, "error", err)
		negativelyAcknowledge(msg)
	}
}

// negativelyAcknowledge hands msg back to the server for redelivery at once, logging a Nak that
// fails: the ack wait then redelivers it.
func negativelyAcknowledge(msg *natsgo.Msg) {
	if err := msg.Nak(); err != nil {
		slog.Error("dispatch delivery: negatively acknowledge message", "subject", msg.Subject, "error", err)
	}
}

// deliver unwraps one NATS message into its envelope payload, routes it by the settings in force
// (handle) under intakeMessageTimeout, and always acks: a malformed envelope has nothing to retry,
// and a route error (a failed GitHub call, or that deadline passing) is caught up by the next
// reconcile pass rather than by NATS redelivery -- see the package doc comment. The deadline is
// what makes intakeAckWait a bound rather than a hope: without it one stuck GitHub call could hold
// an in-flight slot past the ack wait and have the message redelivered underneath it. An envelope
// route does not handle (an empty payload, or a kind route sends to no handler) is acknowledged
// and nothing else: no log line and no freshness write. One it handles records the event's time,
// whatever its handler decided.
func (in *Intake) deliver(msg *natsgo.Msg) {
	defer acknowledge(msg)
	ctx, cancel := context.WithTimeout(context.Background(), intakeMessageTimeout)
	defer cancel()

	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		slog.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
		return
	}
	handled, err := in.handle(ctx, envelope.Payload)
	switch {
	case errors.Is(err, errMalformedPayload):
		slog.Error("dispatch delivery: decode envelope payload", "subject", msg.Subject, "error", err)
		return
	case err != nil:
		slog.Warn("dispatch delivery: process event", "subject", msg.Subject, "error", err)
		return
	case !handled:
		return
	}
	if err := RecordEventAt(ctx, in.pool); err != nil {
		slog.Error("dispatch delivery: record event freshness", "error", err)
	}
}

// handlePullRequestEnvelope processes one `kind:"pr"` envelope. Only a merge (action "closed",
// merged "true") does anything; author and excluded-repository checks run on the envelope's own
// fields first, with no GitHub call, so a merge by anyone outside the population costs nothing.
// It reports whether it stored the pull request's row: a row it deleted, a pull request outside
// the population, ships with nothing.
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "pr" || payload["action"] != "closed" || payload["merged"] != "true" {
		return false, nil
	}
	repoFull := payload["repo"]
	number, err := strconv.Atoi(payload["number"])
	if err != nil {
		return false, fmt.Errorf("merged-PR envelope: malformed number %q: %w", payload["number"], err)
	}
	author := payload["author"]

	if containsString(settings.ExcludedRepos, repoFull) {
		return false, nil
	}
	if !containsString(settings.PopulationAuthors, author) {
		return false, nil
	}

	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}

	fetched, fetchErr := FetchPullRequest(ctx, in.github, owner, repo, number)
	if fetchErr != nil {
		// The population/task-label decision needs the fetch (labels aren't in the webhook
		// payload); without it we cannot tell whether this PR belongs in the population at all,
		// so nothing is written here. The reconcile's merged-PR search will find and complete
		// this PR on its own within five minutes.
		return false, fmt.Errorf("complete merged PR %s#%d: %w", repoFull, number, fetchErr)
	}
	if fetched.MergedAt == nil {
		return false, fmt.Errorf("merged-PR envelope for %s#%d, but GitHub reports it unmerged", repoFull, number)
	}

	raw := RawPullRequest{Repo: repoFull, Number: number, Title: fetched.Title, Author: fetched.Author, Labels: fetched.Labels, MergedAt: *fetched.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, liveIntakeWindow())
	if err != nil {
		// IsTaskPR's "neither label" error: a deploy-repo PR GitHub's own classifier workflow has
		// not labelled yet. Surfaced, not guessed; the reconcile retries this PR's label on its
		// next pass.
		return false, fmt.Errorf("classify %s#%d: %w", repoFull, number, err)
	}
	if !isPopulation {
		return false, DeletePullRequest(ctx, in.pool, repoFull, number)
	}

	if err := completePullRequest(ctx, in.pool, in.github, owner, repo, repoFull, number, fetched); err != nil {
		return false, err
	}
	return true, nil
}

// completePullRequest finishes writing one merged pull request's complete row -- session
// trailers, attribution inputs, issue resolution, and the upsert -- once its GitHub facts
// (FetchPullRequest's answer) are in hand. The shared tail both intake's live
// handlePullRequestEnvelope and reconcile's completePartialPullRequest call, so "a live webhook
// completes a PR" and "reconcile completes a partial row" write through the exact same last
// steps, never two copies that can drift from each other. A commit fetch that fails, for any
// reason, returns its error without writing anything: completing the row from what was read
// would record its sessions and commit-message keys short for good. The row stays as it was
// (still partial, for reconcile's own caller; unwritten, for intake's, whose merged-PR search then
// finds it), so the next pass retries the whole completion. A rate limit comes back as the
// *githubapp.RateLimitError, which stops reconcile's batch.
func completePullRequest(ctx context.Context, pool *store.Pool, github *githubapp.Client, owner, repo, repoFull string, number int, fetched FetchedPullRequest) error {
	messages, err := fetchCommitMessages(ctx, github, owner, repo, number)
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			return limited
		}
		return fmt.Errorf("complete %s#%d: %w", repoFull, number, err)
	}
	inputs := attributionInputsFrom(attributionFacts{
		Repo: repoFull, URL: fetched.URL, Title: fetched.Title, Body: fetched.Body,
		HeadRef: fetched.HeadRef, CommitMessages: messages,
	})
	issueKey, _, err := resolveStoredIssueKey(ctx, pool, fetched.URL, inputs)
	if err != nil {
		return err
	}
	return UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repoFull, Number: number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
		CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
		MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
		Rework: IsRework(fetched.Title), IssueKey: issueKey, Sessions: sessionTrailers(messages),
		Attribution: &inputs, Partial: false,
	})
}

// liveIntakeWindow is the window IsPopulationPR is checked against for a live merge event. The
// 28-day rolling window is the reconcile backfill's concern (which historical PRs to import), not
// a live event's: a merge event is, by construction, happening now, so this window is wide enough
// to always contain it and exists only to satisfy IsPopulationPR's signature.
func liveIntakeWindow() TimeWindow {
	return TimeWindow{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
}

// handleWorkflowEnvelope processes one `kind:"workflow"` envelope for the workflow bound to kind
// (the deploy workflow or the PR-checks workflow, whichever subject this consumer was bound to).
// The envelope's repo/run_id are used only to know which run to ask GitHub about: every stored
// field (head SHA, timing, conclusion, URL, PR number) comes from FetchWorkflowRun's own answer,
// never trusted from the envelope directly -- an Envoy /v1 bearer (any agent session) can publish
// an arbitrary envelope naming any repo/run_id/conclusion/timing, and this is the one place a
// forged value could otherwise rewrite a real run's stored facts. A run row is written whatever
// its current state (so an in-progress run appears before it concludes, per CONTRACT.md); jobs
// are fetched and written only once GitHub reports the run concluded. It reports whether it stored
// the run, with or without its jobs.
func (in *Intake) handleWorkflowEnvelope(ctx context.Context, settings DeliverySettings, kind DeliveryRunKind, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "workflow" {
		return false, nil
	}
	repoFull := payload["repo"]
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}
	runID, err := strconv.ParseInt(payload["run_id"], 10, 64)
	if err != nil {
		return false, fmt.Errorf("workflow envelope: malformed run_id %q: %w", payload["run_id"], err)
	}

	fetched, err := FetchWorkflowRun(ctx, in.github, owner, repo, runID)
	if err != nil {
		return false, fmt.Errorf("verify run %d against GitHub: %w", runID, err)
	}

	if err := UpsertRun(ctx, in.pool, StoredRun(repoFull, kind, fetched)); err != nil {
		return false, fmt.Errorf("upsert run %d: %w", runID, err)
	}

	if fetched.CompletedAt == nil {
		return true, nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, in.github, owner, repo, runID)
	if err != nil {
		// The run row is already written; the reconcile's own run listing fetches this run's
		// jobs too and will complete them.
		return true, fmt.Errorf("fetch jobs of run %d: %w", runID, err)
	}
	jobs := RunJobs(repoFull, runID, fetchedJobs, settings.Production())
	return true, UpsertRunJobs(ctx, in.pool, repoFull, runID, jobs, JobsComplete(jobs))
}

// commitMessagePayload is one element of GET /repos/{owner}/{repo}/pulls/{number}/commits,
// limited to the message field fetchCommitMessages reads (distinct from github_prs.go's
// commitPayload, which reads the same endpoint's dates instead).
type commitMessagePayload struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// maxCommitPages bounds fetchCommitMessages' pagination: 500 commits is already an enormous pull
// request, and a PR beyond that is not worth the API cost of chasing its full history.
const maxCommitPages = 5

// fetchCommitMessages reads a pull request's commit messages, in commit order: what its session
// trailers (sessionTrailers) and its commit-message issue keys (AttributionInputs.CommitKeys) are
// read from. Shared by Intake and Reconcile (both complete a PR the same way; reconcile must read
// them itself rather than carrying forward whatever a stale row already had, see store.go's
// UpsertPullRequest doc comment on why a partial row is never allowed to regress a complete one's
// attribution). On an error it answers the messages of the pages it read.
func fetchCommitMessages(ctx context.Context, client *githubapp.Client, owner, repo string, number int) ([]string, error) {
	token := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	messages, _, err := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	return messages, err
}

// fetchCommitMessagesWithToken is fetchCommitMessages asking token for each page's installation
// token, by readGitHubPage's rule. It also answers how many commit pages it asked GitHub for, the
// failed one included, so a caller can count what a read cost (reconcileAttributionInputs' call
// budget).
func fetchCommitMessagesWithToken(ctx context.Context, client *githubapp.Client, token func() (string, error), owner, repo string, number int) ([]string, int, error) {
	var messages []string
	for page := 1; page <= maxCommitPages; page++ {
		current, err := token()
		if err != nil {
			return messages, page - 1, fmt.Errorf("mint installation token for %s/%s PR #%d commits page %d: %w", owner, repo, number, page, err)
		}
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, header, err := readGitHubPage(ctx, client, current, commitsPath)
		if err != nil {
			return messages, page, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return messages, page, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		var commits []commitMessagePayload
		if err := json.Unmarshal(body, &commits); err != nil {
			return messages, page, fmt.Errorf("decode commits of PR #%d page %d: %w", number, page, err)
		}
		for _, c := range commits {
			messages = append(messages, c.Commit.Message)
		}
		if len(commits) < 100 {
			return messages, page, nil
		}
	}
	return messages, maxCommitPages, nil
}

// sessionTrailers is every `Omp-Session:` commit trailer across messages (LEGION-294's rule,
// ported from the prototype's `session_ids`), first-seen order, no repeats.
func sessionTrailers(messages []string) []string {
	sessions := []string{}
	for _, message := range messages {
		for _, match := range ompSessionTrailer.FindAllStringSubmatch(message, -1) {
			sessions = appendUnique(sessions, match[1])
		}
	}
	return sessions
}

// mapRunConclusion maps GitHub's raw run conclusion to this schema's narrower check constraint
// (success, failure, cancelled only); any other value (including an empty string for a run still
// in progress) is nil, matching FetchedRun's documented policy.
func mapRunConclusion(raw string) *DeliveryRunConclusion {
	switch raw {
	case "success":
		c := DeliveryRunConclusionSuccess
		return &c
	case "failure":
		c := DeliveryRunConclusionFailure
		return &c
	case "cancelled":
		c := DeliveryRunConclusionCancelled
		return &c
	default:
		return nil
	}
}

// mapJobConclusion is mapRunConclusion's job-level counterpart: a job's schema accepts a wider
// set (skipped, timed_out in addition to the three a run accepts).
func mapJobConclusion(raw *string) *DeliveryJobConclusion {
	if raw == nil {
		return nil
	}
	switch *raw {
	case "success":
		c := DeliveryJobConclusionSuccess
		return &c
	case "failure":
		c := DeliveryJobConclusionFailure
		return &c
	case "cancelled":
		c := DeliveryJobConclusionCancelled
		return &c
	case "skipped":
		c := DeliveryJobConclusionSkipped
		return &c
	case "timed_out":
		c := DeliveryJobConclusionTimedOut
		return &c
	default:
		return nil
	}
}

// splitRepo splits "owner/repo" into its two segments.
func splitRepo(repoFull string) (owner, repo string, err error) {
	parts := strings.SplitN(repoFull, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed repository %q: want owner/repo", repoFull)
	}
	return parts[0], parts[1], nil
}

// containsString reports whether values contains s, exactly.
func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

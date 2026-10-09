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

// Intake wires the NATS consumer to the database and the GitHub App client. A nil GitHub client
// (no App credentials) leaves it idle, exactly like architecture.Importer.
type Intake struct {
	nats   *bus.Client
	pool   *store.Pool
	github *githubapp.Client
}

// NewIntake builds an Intake. natsClient must already be connected (bus.ConnectOwningStream, as
// cmd/dispatch wires its own publisher).
func NewIntake(natsClient *bus.Client, pool *store.Pool, github *githubapp.Client) *Intake {
	return &Intake{nats: natsClient, pool: pool, github: github}
}

// HasApp reports whether the GitHub App credentials needed to complete a merged-PR fetch or list
// a run's jobs are configured.
func (in *Intake) HasApp() bool {
	return in != nil && in.github != nil
}

// Run binds the durable consumer and processes GitHub events until ctx is cancelled. Idle
// (logged once) without GitHub App credentials or a configured delivery_settings row, mirroring
// architecture.Run's shape.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so it is
// bound once and never rebound. delivery_settings is read on every settingsPollInterval tick and
// published to the running handler, so a settings change -- a different deploy repository or
// workflow path -- takes effect within one poll interval with no restart. route does all the
// filtering, from those settings.
//
// The PR-checks workflow path is live-reachable only for a fork-originated pull request: Envoy's
// own webhook normalizer (internal/contracts/normalize.go, untouched by this package) drops a
// workflow_run envelope whose pull_requests array is non-empty, which GitHub populates for a
// same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case those
// runs never arrive here at all; the five-minute reconcile is the only path that ever records
// them. Routed rather than ignored because it is the only live-update path for the fork case,
// which does reach it.
func (in *Intake) Run(ctx context.Context) {
	if !in.HasApp() {
		slog.Info("dispatch delivery: no GitHub App key — intake is idle")
		return
	}

	var sub *natsgo.Subscription
	// The settings the handler routes by. Read on every poll and published here rather than
	// captured by the handler closure: the durable's filter subject never changes, so a settings
	// change must reach the running handler without rebinding anything. Only settings whose deploy
	// repository splitRepo accepts are published, so route can compare against it as it stands.
	var current atomic.Pointer[DeliverySettings]
	defer func() {
		if sub != nil {
			_ = sub.Unsubscribe()
		}
	}()

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
		current.Store(&settings)
		if sub != nil {
			return
		}
		newSub, ok := in.bind(func(ctx context.Context, payload map[string]string) error {
			settings := current.Load()
			if settings == nil {
				return nil
			}
			return in.route(ctx, *settings, payload)
		})
		if !ok {
			return
		}
		sub = newSub
	}

	refresh()
	ticker := time.NewTicker(settingsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// settingsPollInterval is how often Run re-reads delivery_settings so a settings PUT reaches the
// running handler -- which repository and which workflow paths it routes by -- without an
// operator restarting the server. A var so a test can shrink it.
var settingsPollInterval = 30 * time.Second

// route dispatches one decoded payload to the PR or workflow handler by its kind field, and to
// the right workflow kind (deploy or PR-checks) by comparing the payload's own repository and
// workflow path against the configured ones. It is the whole filter: the durable is handed every
// GitHub notification on the bus (githubIntakeSubject), so everything this slice does not want
// is discarded here, before any GitHub call or database write.
func (in *Intake) route(ctx context.Context, settings DeliverySettings, payload map[string]string) error {
	switch payload["kind"] {
	case "pr":
		// Every repository's pull requests: the population spans any repository the configured
		// authors merge into, not the deploy repository alone, and handlePullRequestEnvelope
		// decides from the pull request itself.
		return in.handlePullRequestEnvelope(ctx, settings, payload)
	case "workflow":
		// The workflow runs this slice records are the deploy repository's own. Another
		// repository's run of a file with the same path is not one of them, and routing it would
		// fetch a run this slice never stores.
		if payload["repo"] != settings.DeployRepo {
			return nil
		}
		switch payload["path"] {
		case settings.DeployWorkflowPath:
			return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindDeploy, payload)
		case settings.PRChecksWorkflowPath:
			return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindPRChecks, payload)
		default:
			return nil
		}
	default:
		return nil
	}
}

// deliveryConsumerName is the durable this package's events are read through: one push consumer
// over githubIntakeSubject, with the flow control below. The server holds a backlog undelivered
// until a subscriber attaches, so a restart with events waiting loses none of them and none of
// them count against MaxAckPending in the meantime.
const deliveryConsumerName = "delivery-events"

// githubIntakeSubject is the durable's one filter subject: every GitHub event Envoy relays. The
// handler decides what to do with each (route), and discards the ones this slice does not want,
// which is a JSON decode and two map lookups per envelope.
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

// deliveryConsumerConfig is the policy bind stamps on the durable, whether it creates it or
// finds one an earlier release left with another filter or the server's own flow control.
func deliveryConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        deliveryConsumerName,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	}
}

// bind creates (on first run) or resumes (on a restart) the durable and subscribes to it.
// Checks ConsumerInfo before AddConsumer, like cmd/listener/durable.go's
// startListenerSubscription: a durable's DeliverSubject is a random inbox chosen once at
// creation, and calling AddConsumer again with a freshly generated one on every restart would
// try to change a durable's config out from under itself rather than resuming its cursor.
// Unlike the listener's own consumer, this package skips the rolling-deploy bind-retry/backoff
// machinery (bindListenerDurable): Dispatch runs as one instance, not a rolling fleet.
func (in *Intake) bind(handle func(context.Context, map[string]string) error) (*natsgo.Subscription, bool) {
	js := in.nats.JS()
	info, err := js.ConsumerInfo(bus.Stream, deliveryConsumerName)
	wanted := deliveryConsumerConfig()
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if _, err := js.AddConsumer(bus.Stream, &wanted); err != nil {
			slog.Error("dispatch delivery: create NATS consumer", "name", deliveryConsumerName, "subject", githubIntakeSubject, "error", err)
			return nil, false
		}
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", deliveryConsumerName, "error", err)
		return nil, false
	case len(info.Config.FilterSubjects) > 0:
		// A durable an earlier release left carrying a filter-subject set, which withholds the
		// workflow runs this slice exists to record (githubIntakeSubject). It is replaced rather
		// than updated in place, and replaced at its own ack floor, so the replacement neither
		// replays what the old durable acknowledged nor skips what it had not
		// (TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor).
		recreate := wanted
		recreate.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
		recreate.OptStartSeq = info.AckFloor.Stream + 1
		if err := js.DeleteConsumer(bus.Stream, deliveryConsumerName); err != nil {
			slog.Error("dispatch delivery: replace multi-subject NATS consumer", "name", deliveryConsumerName, "error", err)
			return nil, false
		}
		if _, err := js.AddConsumer(bus.Stream, &recreate); err != nil {
			slog.Error("dispatch delivery: create replacement NATS consumer", "name", deliveryConsumerName, "start_seq", recreate.OptStartSeq, "error", err)
			return nil, false
		}
		slog.Info("dispatch delivery: multi-subject NATS consumer replaced", "name", deliveryConsumerName, "subject", githubIntakeSubject, "start_seq", recreate.OptStartSeq)
	case info.Config.FilterSubject != wanted.FilterSubject ||
		info.Config.AckWait != wanted.AckWait ||
		info.Config.MaxAckPending != wanted.MaxAckPending:
		// The durable carries an earlier release's filter or flow control: update it in place,
		// preserving its delivery cursor, so this needs no operator to notice a log line and
		// restart the server. Every field here is one NATS lets an existing consumer change;
		// nothing touches its deliver policy or ack policy, which it does not.
		config := info.Config
		config.FilterSubject = wanted.FilterSubject
		config.AckWait = wanted.AckWait
		config.MaxAckPending = wanted.MaxAckPending
		if _, err := js.UpdateConsumer(bus.Stream, &config); err != nil {
			slog.Error("dispatch delivery: update NATS consumer policy", "name", deliveryConsumerName, "configured_subject", info.Config.FilterSubject, "wanted_subject", wanted.FilterSubject, "error", err)
			return nil, false
		}
		slog.Info("dispatch delivery: NATS consumer policy updated", "name", deliveryConsumerName, "subject", wanted.FilterSubject, "ack_wait", wanted.AckWait, "max_ack_pending", wanted.MaxAckPending)
	}
	// The subject must be the consumer's own filter subject: nats.go checks a bound
	// subscription's subject against it, and refuses an empty one ("subject does not match
	// consumer").
	sub, err := in.nats.Subscribe(githubIntakeSubject, func(msg *natsgo.Msg) {
		in.deliver(msg, handle)
	}, natsgo.Bind(bus.Stream, deliveryConsumerName), natsgo.ManualAck())
	if err != nil {
		slog.Error("dispatch delivery: bind NATS consumer", "name", deliveryConsumerName, "subject", githubIntakeSubject, "error", err)
		return nil, false
	}
	return sub, true
}

// deliver unwraps one NATS message into its envelope payload, calls handle under
// intakeMessageTimeout, and always acks: a malformed envelope has nothing to retry, and a handle
// error (a failed GitHub call, or that deadline passing) is caught up by the next reconcile pass
// rather than by NATS redelivery -- see the package doc comment. The deadline is what makes
// intakeAckWait a bound rather than a hope: without it one stuck GitHub call could hold an
// in-flight slot past the ack wait and have the message redelivered underneath it.
func (in *Intake) deliver(msg *natsgo.Msg, handle func(context.Context, map[string]string) error) {
	defer func() {
		err := msg.Ack()
		if err == nil {
			return
		}
		// Never discarded: an acknowledgement that does not reach the server leaves the message
		// outstanding, and intakeMaxAckPending of those stop the consumer being given anything
		// until the ack wait expires. The Nak gives the slot back at once; when it fails too
		// (the connection is down, which is why the ack failed), the ack wait is the backstop.
		slog.Error("dispatch delivery: acknowledge message", "subject", msg.Subject, "error", err)
		if err := msg.Nak(); err != nil {
			slog.Error("dispatch delivery: negatively acknowledge message", "subject", msg.Subject, "error", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), intakeMessageTimeout)
	defer cancel()

	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		slog.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
		return
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
		slog.Error("dispatch delivery: decode envelope payload", "subject", msg.Subject, "error", err)
		return
	}
	if err := handle(ctx, payload); err != nil {
		slog.Warn("dispatch delivery: process event", "subject", msg.Subject, "error", err)
		return
	}
	if err := RecordEventAt(ctx, in.pool, time.Now()); err != nil {
		slog.Error("dispatch delivery: record event freshness", "error", err)
	}
}

// handlePullRequestEnvelope processes one `kind:"pr"` envelope. Only a merge (action "closed",
// merged "true") does anything; author and excluded-repository checks run on the envelope's own
// fields first, with no GitHub call, so a merge by anyone outside the population costs nothing.
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) error {
	if payload["kind"] != "pr" || payload["action"] != "closed" || payload["merged"] != "true" {
		return nil
	}
	repoFull := payload["repo"]
	number, err := strconv.Atoi(payload["number"])
	if err != nil {
		return fmt.Errorf("merged-PR envelope: malformed number %q: %w", payload["number"], err)
	}
	author := payload["author"]

	if containsString(settings.ExcludedRepos, repoFull) {
		return nil
	}
	if !containsString(settings.PopulationAuthors, author) {
		return nil
	}

	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return err
	}

	fetched, fetchErr := FetchPullRequest(ctx, in.github, owner, repo, number)
	if fetchErr != nil {
		// The population/task-label decision needs the fetch (labels aren't in the webhook
		// payload); without it we cannot tell whether this PR belongs in the population at all,
		// so nothing is written here. The reconcile's merged-PR search will find and complete
		// this PR on its own within five minutes.
		return fmt.Errorf("complete merged PR %s#%d: %w", repoFull, number, fetchErr)
	}
	if fetched.MergedAt == nil {
		return fmt.Errorf("merged-PR envelope for %s#%d, but GitHub reports it unmerged", repoFull, number)
	}

	raw := RawPullRequest{Repo: repoFull, Number: number, Title: fetched.Title, Author: fetched.Author, Labels: fetched.Labels, MergedAt: *fetched.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, liveIntakeWindow())
	if err != nil {
		// IsTaskPR's "neither label" error: a deploy-repo PR GitHub's own classifier workflow has
		// not labelled yet. Surfaced, not guessed; the reconcile retries this PR's label on its
		// next pass.
		return fmt.Errorf("classify %s#%d: %w", repoFull, number, err)
	}
	if !isPopulation {
		return DeletePullRequest(ctx, in.pool, repoFull, number)
	}

	return completePullRequest(ctx, in.pool, in.github, owner, repo, repoFull, number, fetched)
}

// completePullRequest finishes writing one merged pull request's complete row -- session
// trailers, attribution inputs, issue resolution, and the upsert -- once its GitHub facts
// (FetchPullRequest's answer) are in hand. The shared tail both intake's live
// handlePullRequestEnvelope and reconcile's completePartialPullRequest call, so "a live webhook
// completes a PR" and "reconcile completes a partial row" write through the exact same last
// steps, never two copies that can drift from each other. A rate-limited commit fetch returns the
// *githubapp.RateLimitError without upserting anything, leaving the row as it was (still partial,
// for reconcile's own caller; unwritten, for intake's) so whichever path calls this next retries
// the fetch. Any other failed commit fetch still writes the row complete, with its issue resolved
// from what was read, but leaves its attribution inputs unread (null): storing them would record
// a partial answer as permanently as a real "these commits name nothing", while unread inputs are
// what the reconcile's backfill reads again (reconcileAttributionInputs), sessions with them.
func completePullRequest(ctx context.Context, pool *store.Pool, github *githubapp.Client, owner, repo, repoFull string, number int, fetched FetchedPullRequest) error {
	messages, err := fetchCommitMessages(ctx, github, owner, repo, number)
	commitsRead := err == nil
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			return limited
		}
		slog.Warn("dispatch delivery: fetch commit messages; the attribution backfill reads them again", "repo", repoFull, "number", number, "error", err)
	}
	inputs := attributionInputsFrom(attributionFacts{
		Repo: repoFull, URL: fetched.URL, Title: fetched.Title, Body: fetched.Body,
		HeadRef: fetched.HeadRef, CommitMessages: messages,
	})
	issueKey, _, err := resolveStoredIssueKey(ctx, pool, fetched.URL, inputs)
	if err != nil {
		return err
	}
	var stored *AttributionInputs
	if commitsRead {
		stored = &inputs
	}
	return UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repoFull, Number: number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
		CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
		MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
		Rework: IsRework(fetched.Title), IssueKey: issueKey, Sessions: sessionTrailers(messages),
		Attribution: stored, Partial: false,
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
// are fetched and written only once GitHub reports the run concluded.
func (in *Intake) handleWorkflowEnvelope(ctx context.Context, settings DeliverySettings, kind DeliveryRunKind, payload map[string]string) error {
	if payload["kind"] != "workflow" {
		return nil
	}
	repoFull := payload["repo"]
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return err
	}
	runID, err := strconv.ParseInt(payload["run_id"], 10, 64)
	if err != nil {
		return fmt.Errorf("workflow envelope: malformed run_id %q: %w", payload["run_id"], err)
	}

	fetched, err := FetchWorkflowRun(ctx, in.github, owner, repo, runID)
	if err != nil {
		return fmt.Errorf("verify run %d against GitHub: %w", runID, err)
	}

	if err := UpsertRun(ctx, in.pool, DeliveryRun{
		Repo: repoFull, RunID: runID, Kind: kind, PRNumber: fetched.PRNumber, HeadSHA: fetched.HeadSHA,
		HeadCommitAt: fetched.HeadCommitAt, StartedAt: fetched.StartedAt, CompletedAt: fetched.CompletedAt,
		Conclusion: mapRunConclusionPtr(fetched.Conclusion), URL: fetched.URL,
		HeadBranch: fetched.HeadBranch, Event: fetched.Event,
	}); err != nil {
		return fmt.Errorf("upsert run %d: %w", runID, err)
	}

	if fetched.CompletedAt == nil {
		return nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, in.github, owner, repo, runID)
	if err != nil {
		// The run row is already written; the reconcile's own run listing fetches this run's
		// jobs too and will complete them.
		return fmt.Errorf("fetch jobs of run %d: %w", runID, err)
	}
	jobs := make([]DeliveryRunJob, len(fetchedJobs))
	for i, job := range fetchedJobs {
		jobs[i] = DeliveryRunJob{
			Repo: repoFull, RunID: runID, Name: job.Name, StartedAt: job.StartedAt,
			CompletedAt: job.CompletedAt, Conclusion: mapJobConclusion(job.Conclusion),
		}
	}
	return UpsertRunJobs(ctx, in.pool, repoFull, runID, jobs)
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
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s PR #%d: %w", owner, repo, number, err)
	}
	return fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
}

// fetchCommitMessagesWithToken is fetchCommitMessages under a token the caller already holds.
func fetchCommitMessagesWithToken(ctx context.Context, client *githubapp.Client, token, owner, repo string, number int) ([]string, error) {
	var messages []string
	for page := 1; page <= maxCommitPages; page++ {
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, header, err := readGitHubPage(ctx, client, token, commitsPath)
		if err != nil {
			return messages, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return messages, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		var commits []commitMessagePayload
		if err := json.Unmarshal(body, &commits); err != nil {
			return messages, fmt.Errorf("decode commits of PR #%d page %d: %w", number, page, err)
		}
		for _, c := range commits {
			messages = append(messages, c.Commit.Message)
		}
		if len(commits) < 100 {
			break
		}
	}
	return messages, nil
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

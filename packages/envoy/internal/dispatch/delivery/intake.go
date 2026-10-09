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
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// Run subscribes to the GitHub events this slice needs and processes them until ctx is
// cancelled. Idle (logged once) without GitHub App credentials or a configured delivery_settings
// row, mirroring architecture.Run's shape. Subject resolution (which repository, which workflow
// filenames) reads delivery_settings on every settingsPollInterval tick (not once at startup): a
// settings change rebinds the durable consumer's filter subjects in place (bind's UpdateConsumer
// path) within one poll interval, with no restart needed.
//
// The PR-checks workflow subscription is live-reachable only for a fork-originated pull request:
// Envoy's own webhook normalizer (internal/contracts/normalize.go, untouched by this package)
// drops a workflow_run envelope whose pull_requests array is non-empty, which GitHub populates
// for a same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case
// this subscription never fires at all; the five-minute reconcile is the only path that ever
// records those runs. Kept rather than removed because it is the only live-update path for the
// fork case, which does reach it.
//
// bus.Client holds only one JetStream subscription at a time (a later Subscribe call
// unsubscribes an earlier one, per its own doc comment) -- so this is one durable consumer with
// several filter subjects (NATS 2.10's FilterSubjects), not one consumer per subject, and the
// single handler dispatches on the decoded payload's own kind field.
func (in *Intake) Run(ctx context.Context) {
	if !in.HasApp() {
		slog.Info("dispatch delivery: no GitHub App key — intake is idle")
		return
	}

	var sub *natsgo.Subscription
	var boundSubjects []string
	defer func() {
		if sub != nil {
			_ = sub.Unsubscribe()
		}
	}()

	rebind := func() {
		settings, err := GetSettings(ctx, in.pool)
		if err != nil {
			if errors.Is(err, ErrNoSettings) {
				return
			}
			slog.Error("dispatch delivery: read settings for intake", "error", err)
			return
		}
		owner, repo, err := splitRepo(settings.DeployRepo)
		if err != nil {
			slog.Error("dispatch delivery: deploy_repo setting", "error", err)
			return
		}

		subjects := []string{
			"notifications.github.*.*.pr.*",
			contracts.GithubWorkflowSubject(owner, repo, path.Base(settings.DeployWorkflowPath), ">"),
		}
		if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
			subjects = append(subjects, contracts.GithubWorkflowSubject(owner, repo, path.Base(settings.PRChecksWorkflowPath), ">"))
		}
		if sub != nil && slices.Equal(subjects, boundSubjects) {
			return
		}
		if sub != nil {
			_ = sub.Unsubscribe()
			sub = nil
		}
		newSub, ok := in.bind(subjects, func(ctx context.Context, payload map[string]string) error {
			return in.route(ctx, settings, owner, repo, payload)
		})
		if !ok {
			return
		}
		sub, boundSubjects = newSub, subjects
	}

	rebind()
	ticker := time.NewTicker(settingsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rebind()
		}
	}
}

// settingsPollInterval is how often Run re-reads delivery_settings to notice a changed deploy
// repository or workflow path and rebind the NATS consumer's filter subjects -- short enough that
// a settings PUT takes effect in practice without an operator restarting the server.
const settingsPollInterval = 30 * time.Second

// route dispatches one decoded payload to the PR or workflow handler by its kind field, and to
// the right workflow kind (deploy or PR-checks) by comparing the payload's own workflow path
// against the two configured ones.
func (in *Intake) route(ctx context.Context, settings DeliverySettings, owner, repo string, payload map[string]string) error {
	switch payload["kind"] {
	case "pr":
		return in.handlePullRequestEnvelope(ctx, settings, payload)
	case "workflow":
		switch payload["path"] {
		case settings.DeployWorkflowPath:
			return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindDeploy, payload)
		case settings.PRChecksWorkflowPath:
			return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindPRChecks, payload)
		default:
			// Neither configured workflow: the filter subjects above are already scoped to
			// owner/repo and the two configured workflow filenames, so this should not happen in
			// practice; ignored defensively rather than treated as an error.
			return nil
		}
	default:
		return nil
	}
}

// bind creates (on first run) or resumes (on a restart) one durable JetStream consumer filtered
// to every subject in subjects (NATS 2.10's FilterSubjects), then binds a push subscription to
// it. Checks ConsumerInfo before calling AddConsumer, exactly like
// cmd/listener/durable.go's startListenerSubscription: a durable's DeliverSubject is a random
// inbox chosen once at creation, and calling AddConsumer again with a freshly generated one on
// every restart would try to change a durable's config out from under itself, which is refused
// (or worse, silently wrong) rather than resuming the existing cursor. Unlike the listener's own
// consumer, this package skips the rolling-deploy bind-retry/backoff machinery (bindListenerDurable):
// Dispatch runs as one instance, not a rolling fleet, so "another process still holds the old
// bind" is not a concern this package needs to handle.
const deliveryConsumerName = "delivery-events"

func (in *Intake) bind(subjects []string, handle func(context.Context, map[string]string) error) (*natsgo.Subscription, bool) {
	info, err := in.nats.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if _, err := in.nats.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
			Durable:        deliveryConsumerName,
			DeliverSubject: natsgo.NewInbox(),
			FilterSubjects: subjects,
			AckPolicy:      natsgo.AckExplicitPolicy,
		}); err != nil {
			slog.Error("dispatch delivery: create NATS consumer", "name", deliveryConsumerName, "subjects", subjects, "error", err)
			return nil, false
		}
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", deliveryConsumerName, "error", err)
		return nil, false
	case !slices.Equal(info.Config.FilterSubjects, subjects):
		// A settings change (a different deploy repository, a different workflow path) moved
		// these subjects: update the existing durable's filter in place, preserving its
		// DeliverSubject and delivery cursor, so a settings PUT takes effect on its own --
		// without an operator having to notice a log line and restart the server.
		config := info.Config
		config.FilterSubjects = subjects
		if _, err := in.nats.JS().UpdateConsumer(bus.Stream, &config); err != nil {
			slog.Error("dispatch delivery: update NATS consumer subjects after a settings change", "name", deliveryConsumerName, "configured_subjects", info.Config.FilterSubjects, "wanted_subjects", subjects, "error", err)
			return nil, false
		}
		slog.Info("dispatch delivery: NATS consumer subjects updated after a settings change", "name", deliveryConsumerName, "subjects", subjects)
	}
	sub, err := in.nats.Subscribe("", func(msg *natsgo.Msg) {
		in.deliver(msg, handle)
	}, natsgo.Bind(bus.Stream, deliveryConsumerName), natsgo.ManualAck())
	if err != nil {
		slog.Error("dispatch delivery: bind NATS consumer", "name", deliveryConsumerName, "subjects", subjects, "error", err)
		return nil, false
	}
	return sub, true
}

// deliver unwraps one NATS message into its envelope payload, calls handle, and always acks: a
// malformed envelope has nothing to retry, and a handle error (a failed GitHub call) is caught up
// by the next reconcile pass rather than by NATS redelivery -- see the package doc comment.
func (in *Intake) deliver(msg *natsgo.Msg, handle func(context.Context, map[string]string) error) {
	defer func() { _ = msg.Ack() }()
	ctx := context.Background()

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
// *githubapp.RateLimitError without upserting anything: writing the row Partial: false with no
// sessions or commit keys on a rate limit would complete it with empty attribution exactly as
// permanently as a real "these commits name nothing" answer, and the next pass would never revisit
// it to try again -- leaving the row untouched (still partial, for reconcile's own caller;
// unwritten, for intake's) means whichever path calls this next actually retries the fetch
// instead of accepting a false empty answer.
func completePullRequest(ctx context.Context, pool *store.Pool, github *githubapp.Client, owner, repo, repoFull string, number int, fetched FetchedPullRequest) error {
	messages, err := fetchCommitMessages(ctx, github, owner, repo, number)
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			return limited
		}
		slog.Warn("dispatch delivery: fetch commit messages", "repo", repoFull, "number", number, "error", err)
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
	var messages []string
	for page := 1; page <= maxCommitPages; page++ {
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, header, err := client.Read(ctx, token, commitsPath)
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

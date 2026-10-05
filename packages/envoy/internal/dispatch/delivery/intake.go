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
	"net/http"
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
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ompSessionTrailer matches a commit message's `Omp-Session: <id>` trailer line (the same shape
// LEGION-294's own collector reads).
var ompSessionTrailer = regexp.MustCompile(`(?m)^Omp-Session:\s*(\S+)\s*$`)

// issueKeyCandidate matches a bare Dispatch issue key mention (e.g. AGENTC-1072), the same shape
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
// filenames) reads delivery_settings once at startup: a later settings change takes effect on
// the next restart, not live -- the five-minute reconcile reads settings fresh on every tick, so
// facts stay correct even if intake's own subjects lag a restart behind a settings edit.
//
// This slice does not subscribe to default-branch push events (the plan's "defense against a
// dropped workflow_run delivery" is a cheap nice-to-have, not a correctness requirement): the
// reconcile's five-minute cycle is the documented backstop for a missed event, and
// delivery_settings carries no default-branch field to build that subject from.
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
	settings, err := GetSettings(ctx, in.pool)
	if err != nil {
		if errors.Is(err, ErrNoSettings) {
			slog.Info("dispatch delivery: delivery is not configured — intake is idle")
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

	sub, ok := in.bind(subjects, func(ctx context.Context, payload map[string]string) error {
		return in.route(ctx, settings, owner, repo, payload)
	})
	if !ok {
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	<-ctx.Done()
}

// route dispatches one decoded payload to the PR or workflow handler by its kind field, and to
// the right workflow kind (deploy or PR-checks) by comparing the payload's own workflow path
// against the two configured ones.
func (in *Intake) route(ctx context.Context, settings model.DeliverySettings, owner, repo string, payload map[string]string) error {
	switch payload["kind"] {
	case "pr":
		return in.handlePullRequestEnvelope(ctx, settings, payload)
	case "workflow":
		switch payload["path"] {
		case settings.DeployWorkflowPath:
			return in.handleWorkflowEnvelope(ctx, settings, model.DeliveryRunKindDeploy, payload)
		case settings.PRChecksWorkflowPath:
			return in.handleWorkflowEnvelope(ctx, settings, model.DeliveryRunKindPRChecks, payload)
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
		// A settings change moved these subjects (e.g. a different deploy repository): slice 1
		// does not reconcile a drifted filter live, matching the documented "takes effect on next
		// restart" limitation in Run's doc comment. Refusing loudly here, rather than silently
		// binding to the stale subjects, is more honest than pretending they still match.
		slog.Error("dispatch delivery: NATS consumer subjects have drifted from settings; restart the server to pick it up", "name", deliveryConsumerName, "configured_subjects", info.Config.FilterSubjects, "wanted_subjects", subjects)
		return nil, false
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
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings model.DeliverySettings, payload map[string]string) error {
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

	sessions, err := in.fetchSessionTrailers(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("dispatch delivery: fetch session trailers", "repo", repoFull, "number", number, "error", err)
	}
	issueKey := in.resolveIssueKey(ctx, fetched.Title, fetched.Body)

	return UpsertPullRequest(ctx, in.pool, model.DeliveryPullRequest{
		Repo: repoFull, Number: number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
		CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
		MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
		Rework: IsRework(fetched.Title), IssueKey: issueKey, Sessions: sessions, Partial: false,
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
// A run row is written for every action (so an in-progress run appears before it concludes, per
// CONTRACT.md); jobs are fetched and written only once the run has actually completed.
func (in *Intake) handleWorkflowEnvelope(ctx context.Context, settings model.DeliverySettings, kind model.DeliveryRunKind, payload map[string]string) error {
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
	headSHA := payload["head_sha"]
	headCommitAt, err := in.headCommitTime(ctx, owner, repo, headSHA)
	if err != nil {
		// Without the head commit's timestamp the row would violate head_commit_at's not-null
		// constraint and containment would have nothing correct to compare a PR's merge time
		// against; skip this event and let the reconcile's own run listing (which fetches the
		// commit timestamp the same way) fill it in.
		return fmt.Errorf("resolve head commit %s of run %d: %w", headSHA, runID, err)
	}
	startedAt, err := time.Parse(time.RFC3339, payload["run_started_at"])
	if err != nil {
		return fmt.Errorf("workflow envelope: malformed run_started_at %q: %w", payload["run_started_at"], err)
	}

	var prNumber *int
	if numbers := strings.Split(payload["pr_numbers"], ","); len(numbers) == 1 && numbers[0] != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(numbers[0])); err == nil {
			prNumber = &n
		}
	}

	var completedAt *time.Time
	var conclusion *model.DeliveryRunConclusion
	if payload["action"] == "completed" {
		if updatedAt, err := time.Parse(time.RFC3339, payload["updated_at"]); err == nil {
			completedAt = &updatedAt
		}
		conclusion = mapRunConclusion(payload["conclusion"])
	}

	if err := UpsertRun(ctx, in.pool, model.DeliveryRun{
		Repo: repoFull, RunID: runID, Kind: kind, PRNumber: prNumber, HeadSHA: headSHA,
		HeadCommitAt: headCommitAt, StartedAt: startedAt, CompletedAt: completedAt, Conclusion: conclusion,
		URL: payload["url"],
	}); err != nil {
		return fmt.Errorf("upsert run %d: %w", runID, err)
	}

	if payload["action"] != "completed" {
		return nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, in.github, owner, repo, runID)
	if err != nil {
		// The run row is already written; the reconcile's own run listing fetches this run's
		// jobs too and will complete them.
		return fmt.Errorf("fetch jobs of run %d: %w", runID, err)
	}
	jobs := make([]model.DeliveryRunJob, len(fetchedJobs))
	for i, job := range fetchedJobs {
		jobs[i] = model.DeliveryRunJob{
			Repo: repoFull, RunID: runID, Name: job.Name, StartedAt: job.StartedAt,
			CompletedAt: job.CompletedAt, Conclusion: mapJobConclusion(job.Conclusion),
		}
	}
	return UpsertRunJobs(ctx, in.pool, repoFull, runID, jobs)
}

// headCommitTime resolves a commit's authored (or committed) time via GET
// /repos/{owner}/{repo}/commits/{sha}, reusing github_prs.go's commitPayload/commitDate (the
// response is exactly one such object, not a list).
func (in *Intake) headCommitTime(ctx context.Context, owner, repo, sha string) (time.Time, error) {
	token, err := in.github.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return time.Time{}, fmt.Errorf("mint installation token for %s/%s: %w", owner, repo, err)
	}
	return fetchCommitDate(ctx, in.github, token, owner, repo, sha)
}

// fetchCommitDate fetches one commit via GET /repos/{owner}/{repo}/commits/{sha} and reads its
// authored (or committed) time, reusing github_prs.go's commitPayload/commitDate -- that
// endpoint's response is exactly one such object, not a list. Shared by Intake.headCommitTime and
// Reconcile.headCommitTime, both of which already hold a minted token.
func fetchCommitDate(ctx context.Context, client *githubapp.Client, token, owner, repo, sha string) (time.Time, error) {
	commitPath := fmt.Sprintf("/repos/%s/%s/commits/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	body, status, _, err := client.Read(ctx, token, commitPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("fetch commit %s: %w", sha, err)
	}
	if status != http.StatusOK {
		return time.Time{}, fmt.Errorf("fetch commit %s: status %d: %s", sha, status, body)
	}
	var payload commitPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return time.Time{}, fmt.Errorf("decode commit %s: %w", sha, err)
	}
	return commitDate(payload)
}

// commitMessagePayload is one element of GET /repos/{owner}/{repo}/pulls/{number}/commits,
// limited to the message field fetchSessionTrailers reads (distinct from github_prs.go's
// commitPayload, which reads the same endpoint's dates instead).
type commitMessagePayload struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// maxCommitPages bounds fetchSessionTrailers' pagination: 500 commits is already an enormous pull
// request, and a PR beyond that is not worth the API cost of chasing its full session history.
const maxCommitPages = 5

// fetchSessionTrailers reads every `Omp-Session:` commit trailer across a pull request's commits
// (LEGION-294's rule, ported from the prototype's `session_ids`), first-seen order, no repeats.
func (in *Intake) fetchSessionTrailers(ctx context.Context, owner, repo string, number int) ([]string, error) {
	token, err := in.github.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s PR #%d: %w", owner, repo, number, err)
	}
	var sessions []string
	seen := map[string]bool{}
	for page := 1; page <= maxCommitPages; page++ {
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, _, err := in.github.Read(ctx, token, commitsPath)
		if err != nil {
			return sessions, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		if status != http.StatusOK {
			return sessions, fmt.Errorf("fetch commits of PR #%d page %d: status %d", number, page, status)
		}
		var commits []commitMessagePayload
		if err := json.Unmarshal(body, &commits); err != nil {
			return sessions, fmt.Errorf("decode commits of PR #%d page %d: %w", number, page, err)
		}
		for _, c := range commits {
			for _, match := range ompSessionTrailer.FindAllStringSubmatch(c.Commit.Message, -1) {
				id := match[1]
				if !seen[id] {
					seen[id] = true
					sessions = append(sessions, id)
				}
			}
		}
		if len(commits) < 100 {
			break
		}
	}
	return sessions, nil
}

// resolveIssueKey is LEGION-294's title/body attribution rule, slice 1's scope (the deeper
// external-links/branch/commit fallback chain is not ported -- see the plan's Risks section): the
// first bare Dispatch issue key title or body names, title scanned before body, that Dispatch
// actually has. A key nothing stored names is never accepted ("never point at an issue that
// doesn't exist").
func (in *Intake) resolveIssueKey(ctx context.Context, title, body string) *string {
	for _, candidate := range issueKeyCandidate.FindAllString(title+"\n"+body, -1) {
		var key string
		if err := in.pool.QueryRow(ctx, "select key from issues where key = $1", candidate).Scan(&key); err == nil {
			return &key
		}
	}
	return nil
}

// mapRunConclusion maps GitHub's raw run conclusion to this schema's narrower check constraint
// (success, failure, cancelled only); any other value (including an empty string for a run still
// in progress) is nil, matching FetchedRun's documented policy.
func mapRunConclusion(raw string) *model.DeliveryRunConclusion {
	switch raw {
	case "success":
		c := model.DeliveryConclusionSuccess
		return &c
	case "failure":
		c := model.DeliveryConclusionFailure
		return &c
	case "cancelled":
		c := model.DeliveryConclusionCancelled
		return &c
	default:
		return nil
	}
}

// mapJobConclusion is mapRunConclusion's job-level counterpart: a job's schema accepts a wider
// set (skipped, timed_out in addition to the three a run accepts).
func mapJobConclusion(raw *string) *model.DeliveryRunConclusion {
	if raw == nil {
		return nil
	}
	switch *raw {
	case "success":
		c := model.DeliveryConclusionSuccess
		return &c
	case "failure":
		c := model.DeliveryConclusionFailure
		return &c
	case "cancelled":
		c := model.DeliveryConclusionCancelled
		return &c
	case "skipped":
		c := model.DeliveryConclusionSkipped
		return &c
	case "timed_out":
		c := model.DeliveryConclusionTimedOut
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

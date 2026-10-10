package delivery

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// DeliverySettings is LEGION-567's one configuration record: which repository deploys, its
// workflow paths and production job name, and the population rule's authors and excluded
// repositories (LEGION-294). At most one row ever exists. LastEventAt and LastReconcileAt are the
// Delivery page's freshness: the last GitHub event the intake consumer processed, and the last
// time the five-minute reconcile *completed successfully*. LastError is the most recent
// reconcile pass's failure (a missing permission, a rate limit, any other GitHub or store error),
// cleared the next time a pass succeeds -- LastReconcileAt does not advance on a failed pass, so
// the two together answer both "when did this last actually work" and "what's wrong right now."
type DeliverySettings struct {
	DeployRepo           string      `json:"deploy_repo"`
	DeployWorkflowPath   string      `json:"deploy_workflow_path"`
	ProductionJobName    string      `json:"production_job_name"`
	PRChecksWorkflowPath string      `json:"pr_checks_workflow_path"`
	PopulationAuthors    []string    `json:"population_authors"`
	ExcludedRepos        []string    `json:"excluded_repos"`
	LastEventAt          *time.Time  `json:"last_event_at"`
	LastReconcileAt      *time.Time  `json:"last_reconcile_at"`
	LastError            *string     `json:"last_error"`
	UpdatedBy            model.Actor `json:"updated_by"`
	UpdatedAt            time.Time   `json:"updated_at"`
}

// DeliveryPullRequest is one population pull request (LEGION-294's rule): authored by one of
// DeliverySettings.PopulationAuthors, merged, not in an excluded repository, not a task PR.
// CreatedAt, MergedAt, Additions and Deletions are nil until the completing GitHub fetch (or a
// reconcile pass) fills them in — GitHub's webhook payload alone never carries them — and
// Partial is true for exactly as long as that is so. IssueKey is resolved from five sources
// (attribution.go), Attribution holding what GitHub said for them (nil until it has been read);
// Sessions are the raw `Omp-Session` commit trailer values.
// UnfetchableAt/UnfetchableReason are set when a completing fetch answers a permanent 404/410
// (the pull request or its repository no longer exists, or no longer reaches this token) rather
// than retrying forever; both nil for every normal row, and cleared by any later successful
// fetch of the same pull request.
type DeliveryPullRequest struct {
	Repo              string             `json:"repo"`
	Number            int                `json:"number"`
	Title             string             `json:"title"`
	URL               string             `json:"url"`
	Author            string             `json:"author"`
	CreatedAt         *time.Time         `json:"created_at"`
	MergedAt          *time.Time         `json:"merged_at"`
	FirstCommitAt     *time.Time         `json:"first_commit_at"`
	MergeCommitSHA    *string            `json:"merge_commit_sha"`
	Additions         *int               `json:"additions"`
	Deletions         *int               `json:"deletions"`
	Rework            bool               `json:"rework"`
	IssueKey          *string            `json:"issue"`
	Sessions          []string           `json:"sessions"`
	Partial           bool               `json:"partial"`
	UnfetchableAt     *time.Time         `json:"unfetchable_at"`
	UnfetchableReason *string            `json:"unfetchable_reason"`
	UpdatedAt         time.Time          `json:"updated_at"`
	Attribution       *AttributionInputs `json:"-"`
}

// DeliveryRunKind distinguishes a deploy-workflow run (on pushes to the configured repository's
// main) from a PR-checks run (on a pull request); PRNumber is set only for the latter.
type DeliveryRunKind string

const (
	DeliveryRunKindDeploy   DeliveryRunKind = "deploy"
	DeliveryRunKindPRChecks DeliveryRunKind = "pr_checks"
)

// DeliveryRunConclusion is a concluded run's result: delivery_runs' check constraint accepts only
// these three. A separate, wider DeliveryJobConclusion exists for a job's result -- the two are
// genuinely different types (not the same type reused) so the Go compiler, not just the
// database's check constraint, refuses a job-only value (e.g. "skipped") on a run field.
type DeliveryRunConclusion string

const (
	DeliveryRunConclusionSuccess   DeliveryRunConclusion = "success"
	DeliveryRunConclusionFailure   DeliveryRunConclusion = "failure"
	DeliveryRunConclusionCancelled DeliveryRunConclusion = "cancelled"
)

// DeliveryJobConclusion is a concluded job's result: delivery_run_jobs' check constraint accepts
// these five (three more than a run: "skipped" and "timed_out" are real job outcomes with no run-
// level equivalent delivery_runs' constraint allows).
type DeliveryJobConclusion string

const (
	DeliveryJobConclusionSuccess   DeliveryJobConclusion = "success"
	DeliveryJobConclusionFailure   DeliveryJobConclusion = "failure"
	DeliveryJobConclusionCancelled DeliveryJobConclusion = "cancelled"
	DeliveryJobConclusionSkipped   DeliveryJobConclusion = "skipped"
	DeliveryJobConclusionTimedOut  DeliveryJobConclusion = "timed_out"
)

// DeliveryRun is one run of the deploy workflow or the PR-checks workflow on the configured
// deploy repository, identified by (Repo, RunID). CompletedAt and Conclusion are nil while the
// run is still in progress.
type DeliveryRun struct {
	Repo         string                 `json:"repo"`
	RunID        int64                  `json:"run_id"`
	Kind         DeliveryRunKind        `json:"kind"`
	PRNumber     *int                   `json:"pr_number"`
	HeadSHA      string                 `json:"head_sha"`
	HeadCommitAt time.Time              `json:"head_commit_at"`
	StartedAt    time.Time              `json:"started_at"`
	CompletedAt  *time.Time             `json:"completed_at"`
	Conclusion   *DeliveryRunConclusion `json:"conclusion"`
	URL          string                 `json:"url"`
}

// DeliveryRunJob is one job of a DeliveryRun, identified within it by Name (GitHub does not
// number jobs, and a run never repeats a job name within the attempt intake and reconcile keep).
type DeliveryRunJob struct {
	Repo        string                 `json:"repo"`
	RunID       int64                  `json:"run_id"`
	Name        string                 `json:"name"`
	StartedAt   *time.Time             `json:"started_at"`
	CompletedAt *time.Time             `json:"completed_at"`
	Conclusion  *DeliveryJobConclusion `json:"conclusion"`
}

// DeployedStatus is a population pull request's deploy state: "not_tracked" for a PR outside the
// configured deploy repository (nothing is watching it -- never "waiting", which would inflate a
// count that should only ever shrink to zero as deploys catch up); "deployed" once a qualifying
// apply exists; "waiting" otherwise. A branded type (not a bare string), matching the contract's
// own closed union (`packages/contracts/src/dispatch-api.ts`'s `DeployedStatus` literal type).
type DeployedStatus string

const (
	DeployedStatusDeployed   DeployedStatus = "deployed"
	DeployedStatusWaiting    DeployedStatus = "waiting"
	DeployedStatusNotTracked DeployedStatus = "not_tracked"
)

// DeliveryRunJobView is one job of a DeliveryRunView, the shape `GET /api/v1/delivery/timeline`
// answers in a run's failed_jobs and root_failing_job -- mirrors
// packages/contracts/src/dispatch-api.ts's DeliveryRunJob exactly.
type DeliveryRunJobView struct {
	Name        string     `json:"name"`
	CompletedAt *time.Time `json:"completed_at"`
}

// DeliveryPRView is one population pull request on `GET /api/v1/delivery/timeline` -- LEGION-294's
// facts plus what the server derives at read time from the stored facts: DeployedStatus, the
// linked issue's title, priority and effective components (the nearest ancestor's attachment, as
// the issue read resolves it), and (on DeliveryRunView) RootFailingJob. Mirrors
// packages/contracts/src/dispatch-api.ts's DeliveryPR exactly; ParentAgent and Sessions resolve to
// the same set of sessions today (LEGION-567: no grouping link exists between sessions) -- see
// agents.go's comment on the pair for why both still exist.
type DeliveryPRView struct {
	ID                string         `json:"id"`
	Repo              string         `json:"repo"`
	Number            int            `json:"number"`
	Title             string         `json:"title"`
	URL               string         `json:"url"`
	Author            string         `json:"author"`
	CreatedAt         *time.Time     `json:"created_at"`
	MergedAt          *time.Time     `json:"merged_at"`
	FirstCommitAt     *time.Time     `json:"first_commit_at"`
	Additions         *int           `json:"additions"`
	Deletions         *int           `json:"deletions"`
	Partial           bool           `json:"partial"`
	Rework            bool           `json:"rework"`
	Issue             *string        `json:"issue"`
	IssueTitle        *string        `json:"issue_title"`
	Priority          *string        `json:"priority"`
	Components        []string       `json:"components"`
	Sessions          []string       `json:"sessions"`
	ParentAgent       *string        `json:"parent_agent"`
	DeployRun         *int64         `json:"deploy_run"`
	DeployedAt        *time.Time     `json:"deployed_at"`
	DeployedStatus    DeployedStatus `json:"deployed_status"`
	UnfetchableReason *string        `json:"unfetchable_reason"`
}

// DeliveryRunProductionView is a run's production job (DeliverySettings.ProductionJobName): its
// result and when it finished, both null while it runs. A successful one is a production deploy,
// drawn at CompletedAt; a failed one is a pipeline failure even when no other job failed.
type DeliveryRunProductionView struct {
	Conclusion  *DeliveryJobConclusion `json:"conclusion"`
	CompletedAt *time.Time             `json:"completed_at"`
}

// DeliveryShippedPRView is one population pull request a deploy run shipped first.
type DeliveryShippedPRView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// DeliveryRunView is one deploy run on `GET /api/v1/delivery/timeline`: a production deploy
// (Production succeeded; sized by PRs) and/or a pipeline failure (FailedJobs, RootFailingJob, or a
// failed Production). Mirrors packages/contracts/src/dispatch-api.ts's DeliveryRun exactly. PRs
// is every population pull request in the window the run shipped first, whatever facets the
// request names, and is always a slice, never nil, so it serializes as `[]` for a run that
// shipped none.
type DeliveryRunView struct {
	ID             int64                      `json:"id"`
	URL            string                     `json:"url"`
	HeadSHA        string                     `json:"head_sha"`
	HeadAt         time.Time                  `json:"head_at"`
	StartedAt      time.Time                  `json:"started_at"`
	CompletedAt    *time.Time                 `json:"completed_at"`
	Conclusion     *DeliveryRunConclusion     `json:"conclusion"`
	Production     *DeliveryRunProductionView `json:"production"`
	FailedJobs     []DeliveryRunJobView       `json:"failed_jobs"`
	RootFailingJob *DeliveryRunJobView        `json:"root_failing_job"`
	PRs            []DeliveryShippedPRView    `json:"prs"`
}

// DeliveryFreshnessView is `GET /api/v1/delivery/timeline`'s freshness object: when intake last
// processed a GitHub event, when the five-minute reconcile last *completed successfully*, (when
// the most recent pass failed) why, and how many population pull requests can no longer be
// fetched from GitHub at all (a permanent 404/410 -- store.go's unfetchable_at/unfetchable_reason).
type DeliveryFreshnessView struct {
	LastEventAt      *time.Time `json:"last_event_at"`
	LastReconcileAt  *time.Time `json:"last_reconcile_at"`
	LastError        *string    `json:"last_error"`
	UnfetchableCount int        `json:"unfetchable_count"`
}

// DeliveryWindowView is the [from, to) window a DeliveryTimelineResponse answers for.
type DeliveryWindowView struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// DeliveryComponentView names one architecture component a pull request's issue can carry: the
// facet panel, the swimlanes and the drill-down show its title, and the component facet's
// selection of a parent includes its children through Parent.
type DeliveryComponentView struct {
	Title  string  `json:"title"`
	Parent *string `json:"parent"`
}

// DeliveryWaitingPRView is one pull request of the deploy repository that merged before the
// window and had not shipped by its start, matching the request's facets and search: the
// waiting-to-deploy line starts the window counting it, and stops when it ships.
type DeliveryWaitingPRView struct {
	MergedAt   time.Time  `json:"merged_at"`
	DeployedAt *time.Time `json:"deployed_at"`
}

// DeliveryTimelineResponse is `GET /api/v1/delivery/timeline?from&to&q&<facets>`: merges, deploys,
// pipeline failures and waiting-to-deploy PRs within [from, to) and the given facets. Runs holds
// deploy runs only. Waiting holds the pull requests that merged before the window and had not
// shipped by its start, under the same facets and search. FacetCounts counts, per facet, the
// window's pull requests by that facet's values with every other facet and the search applied and
// its own selection ignored, so picking a value never zeroes its own count. ColorCounts counts
// the window's pull requests by each colour-by facet's value with no facet applied, so a value
// keeps its colour while facets change. Components names every component of the projects the
// window's issues belong to, and IssueTitles every issue the window's pull requests name, so a
// facet value a selection filtered out of PRs still has its label.
type DeliveryTimelineResponse struct {
	Window      DeliveryWindowView               `json:"window"`
	PRs         []DeliveryPRView                 `json:"prs"`
	Waiting     []DeliveryWaitingPRView          `json:"waiting"`
	Runs        []DeliveryRunView                `json:"runs"`
	FacetCounts map[string]map[string]int        `json:"facet_counts"`
	ColorCounts map[string]map[string]int        `json:"color_counts"`
	Components  map[string]DeliveryComponentView `json:"components"`
	IssueTitles map[string]string                `json:"issue_titles"`
	Freshness   DeliveryFreshnessView            `json:"freshness"`
}

// DeliveryRunJobDetailView is one job of `GET /api/v1/delivery/runs/{id}`: the drill-down's job
// list, with each job's result and timings.
type DeliveryRunJobDetailView struct {
	Name        string                 `json:"name"`
	StartedAt   *time.Time             `json:"started_at"`
	CompletedAt *time.Time             `json:"completed_at"`
	Conclusion  *DeliveryJobConclusion `json:"conclusion"`
}

// DeliveryRunDetailView is `GET /api/v1/delivery/runs/{id}`: one run of the deploy repository with
// every job it ran, ordered by when each started (a job that never started last, then by name).
type DeliveryRunDetailView struct {
	ID   int64                      `json:"id"`
	URL  string                     `json:"url"`
	Jobs []DeliveryRunJobDetailView `json:"jobs"`
}

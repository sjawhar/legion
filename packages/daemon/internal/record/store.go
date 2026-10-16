package record

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const maxInt64 = uint64(^uint64(0) >> 1)

// Postgres implements Store exclusively through caller-owned pgx transactions. It has no pool:
// opening, committing, and retrying a transaction belong to the layer that owns its boundary.
type Postgres struct{}

var _ Store = (*Postgres)(nil)

// NewStore returns the stateless Postgres record implementation.
func NewStore() *Postgres { return &Postgres{} }

func (s *Postgres) MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error) {
	tag, err := tx.Exec(ctx, `insert into processed_events (source, event_id) values ($1, $2)
		on conflict (source, event_id) do nothing`, source, eventID)
	if err != nil {
		return false, fmt.Errorf("mark %s event %s processed: %w", source, eventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

const issueColumns = `key, tree, project, title, parent, phase, generation, status, rank, handed_over, linger_until, held_from, last_dispatch_seq, ready_pending_version, coalesce(hold_reason, ''), dispatch_status`

func (s *Postgres) Issue(ctx context.Context, tx pgx.Tx, key string) (*Issue, error) {
	issue, err := scanIssue(tx.QueryRow(ctx, "select "+issueColumns+" from issues where key = $1", key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read issue %s: %w", key, err)
	}
	return issue, nil
}

func (s *Postgres) Issues(ctx context.Context, tx pgx.Tx) ([]Issue, error) {
	rows, err := tx.Query(ctx, "select "+issueColumns+" from issues order by key")
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		issues = append(issues, *issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	return issues, nil
}

func (s *Postgres) PutIssue(ctx context.Context, tx pgx.Tx, issue Issue) error {
	if issue.Generation > maxInt64 {
		return fmt.Errorf("put issue %s: generation %d does not fit a bigint", issue.Key, issue.Generation)
	}
	var heldFrom any
	var holdReason HoldReason
	if issue.Hold != nil {
		heldFrom, holdReason = string(issue.Hold.From), issue.Hold.Reason
	}
	_, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, parent, phase, generation, status, rank, handed_over, linger_until, held_from, last_dispatch_seq, ready_pending_version, hold_reason, dispatch_status)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15::text, ''), $16)
		on conflict (key) do update set tree = excluded.tree, project = excluded.project, title = excluded.title,
		parent = excluded.parent, phase = excluded.phase, generation = excluded.generation,
		status = excluded.status, rank = excluded.rank, handed_over = excluded.handed_over, linger_until = excluded.linger_until,
		held_from = excluded.held_from, last_dispatch_seq = excluded.last_dispatch_seq,
		ready_pending_version = excluded.ready_pending_version, hold_reason = excluded.hold_reason, dispatch_status = excluded.dispatch_status`,
		issue.Key, issue.Tree, issue.Project, issue.Title, issue.Parent, string(issue.Phase), int64(issue.Generation), issue.Status,
		issue.Rank, issue.HandedOver, issue.LingerUntil, heldFrom, issue.LastDispatchSeq, issue.ReadyPendingVersion, string(holdReason), issue.DispatchStatus,
	)
	if err != nil {
		return fmt.Errorf("put issue %s: %w", issue.Key, err)
	}
	return nil
}

// OpenTreeLifecycle is the admission fact's transactional entry point into the shared lifecycle
// barrier.
func (s *Postgres) OpenTreeLifecycle(ctx context.Context, tx pgx.Tx, project, tree string, authority treelifecycle.Authority) (treelifecycle.Lifecycle, error) {
	return treelifecycle.Open(ctx, tx, project, tree, authority)
}

func scanIssue(row scanner) (*Issue, error) {
	var issue Issue
	var phaseValue string
	var generation int64
	var heldFrom *string
	var holdReason string
	if err := row.Scan(&issue.Key, &issue.Tree, &issue.Project, &issue.Title, &issue.Parent, &phaseValue, &generation, &issue.Status,
		&issue.Rank, &issue.HandedOver, &issue.LingerUntil, &heldFrom, &issue.LastDispatchSeq, &issue.ReadyPendingVersion, &holdReason, &issue.DispatchStatus); err != nil {
		return nil, err
	}
	if generation < 0 {
		return nil, fmt.Errorf("issue %s has negative generation %d", issue.Key, generation)
	}
	issue.Phase = phase.Phase(phaseValue)
	if heldFrom != nil {
		issue.Hold = &Hold{From: phase.Phase(*heldFrom), Reason: HoldReason(holdReason)}
	}
	issue.Generation = uint64(generation)
	return &issue, nil
}

func (s *Postgres) Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error) {
	rows, err := tx.Query(ctx, `select issue, role, claim, handoff_commit, rounds, verdict, summary, last_handoff, decision, completed_at, merge_note from phases
		where issue = $1 order by role`, issue)
	if err != nil {
		return nil, fmt.Errorf("list phases for %s: %w", issue, err)
	}
	defer rows.Close()
	phases := []PhaseRow{}
	for rows.Next() {
		phase, err := scanPhase(rows)
		if err != nil {
			return nil, fmt.Errorf("list phases for %s: %w", issue, err)
		}
		phases = append(phases, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list phases for %s: %w", issue, err)
	}
	return phases, nil
}

func (s *Postgres) PutPhase(ctx context.Context, tx pgx.Tx, phase PhaseRow) error {
	var decision []byte
	if phase.Decision != nil {
		var err error
		if decision, err = json.Marshal(phase.Decision); err != nil {
			return fmt.Errorf("put %s phase on %s: encode the review decision: %w", phase.Role, phase.Issue, err)
		}
	}
	_, err := tx.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, summary, last_handoff,
		decision, completed_at, merge_note)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		on conflict (issue, role) do update set claim = excluded.claim,
		handoff_commit = excluded.handoff_commit, rounds = excluded.rounds, verdict = excluded.verdict,
		summary = excluded.summary, last_handoff = excluded.last_handoff, decision = excluded.decision,
		completed_at = excluded.completed_at, merge_note = excluded.merge_note`,
		phase.Issue, string(phase.Role), string(phase.Claim), phase.HandoffCommit, phase.Rounds, phase.Verdict, phase.Summary, phase.LastHandoff,
		decision, phase.CompletedAt, phase.MergeNote,
	)
	if err != nil {
		return fmt.Errorf("put %s phase on %s: %w", phase.Role, phase.Issue, err)
	}
	return nil
}

func scanPhase(row scanner) (PhaseRow, error) {
	var phase PhaseRow
	var role, token string
	var decision []byte
	if err := row.Scan(&phase.Issue, &role, &token, &phase.HandoffCommit, &phase.Rounds, &phase.Verdict, &phase.Summary, &phase.LastHandoff,
		&decision, &phase.CompletedAt, &phase.MergeNote); err != nil {
		return PhaseRow{}, err
	}
	phase.CompletedAt = phase.CompletedAt.UTC()
	if decision != nil {
		phase.Decision = &ReviewDecision{}
		if err := json.Unmarshal(decision, phase.Decision); err != nil {
			return PhaseRow{}, fmt.Errorf("decode the review decision: %w", err)
		}
	}
	phase.Role, phase.Claim = claim.Role(role), claim.Token(token)
	return phase, nil
}

const pullRequestColumns = `issue, repo, number, branch, head_sha, head_updated_at,
	failing, cancelled, fix_attempts, blocked_attempts, check_runs,
	generation, snapshot, pushes, head_counted, head_counted_before, planned_red, review_seen, review_seen_at, state,
	checked_head, required, required_workflows, required_workflows_head, mergeability`

func (s *Postgres) PullRequest(ctx context.Context, tx pgx.Tx, issue string) (*PullRequest, error) {
	pr, err := scanPullRequest(tx.QueryRow(ctx, "select "+pullRequestColumns+" from pull_requests where issue = $1", issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pull request for %s: %w", issue, err)
	}
	return pr, nil
}

func (s *Postgres) PullRequestByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*PullRequest, error) {
	pr, err := scanPullRequest(tx.QueryRow(ctx, "select "+pullRequestColumns+" from pull_requests where repo = $1 and branch = $2", repo, branch))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pull request for %s %s: %w", repo, branch, err)
	}
	return pr, nil
}

// PullRequestByNumber reads the pull request a GitHub event names. The engine reached it by
// listing every issue and reading each one's pull request — a read per issue for every check,
// review and push event — and the row is one query away.
func (s *Postgres) PullRequestByNumber(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequest, error) {
	pr, err := scanPullRequest(tx.QueryRow(ctx, "select "+pullRequestColumns+" from pull_requests where repo = $1 and number = $2", repo, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pull request %s#%d: %w", repo, number, err)
	}
	return pr, nil
}

// OpenPullRequests is every open pull request of the Dispatch project's issues, by issue key.
func (s *Postgres) OpenPullRequests(ctx context.Context, tx pgx.Tx, project string) ([]PullRequest, error) {
	rows, err := tx.Query(ctx, "select "+pullRequestColumns+" from pull_requests where state = $1 and issue in (select key from issues where project = $2) order by issue",
		PullRequestOpen, project)
	if err != nil {
		return nil, fmt.Errorf("list the open pull requests of %s: %w", project, err)
	}
	defer rows.Close()
	open := []PullRequest{}
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("list the open pull requests of %s: %w", project, err)
		}
		open = append(open, *pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the open pull requests of %s: %w", project, err)
	}
	return open, nil
}

func (s *Postgres) PutPullRequest(ctx context.Context, tx pgx.Tx, pr PullRequest) error {
	failing, err := json.Marshal(pr.Failing)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode failing checks: %w", pr.Issue, err)
	}
	cancelled, err := json.Marshal(append([]string{}, pr.Cancelled...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode cancelled checks: %w", pr.Issue, err)
	}
	checkRuns, err := json.Marshal(pr.CheckRuns)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode check runs: %w", pr.Issue, err)
	}
	pushes, err := json.Marshal(append([]ClassifiedPush{}, pr.Pushes...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode pushes: %w", pr.Issue, err)
	}
	// A set never read is null, never an empty list, which is a base that requires no check.
	var required []byte
	if pr.Required != nil {
		if required, err = json.Marshal(pr.Required); err != nil {
			return fmt.Errorf("put pull request for %s: encode required checks: %w", pr.Issue, err)
		}
	}
	workflows, err := json.Marshal(append([]RequiredWorkflow{}, pr.Workflows...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode required workflows: %w", pr.Issue, err)
	}
	_, err = tx.Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, pushes,
		head_counted, head_counted_before, planned_red, review_seen, review_seen_at, state, checked_head, required,
		required_workflows, required_workflows_head, mergeability)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
		on conflict (issue) do update set repo = excluded.repo, number = excluded.number, branch = excluded.branch,
		head_sha = excluded.head_sha, head_updated_at = excluded.head_updated_at,
		failing = excluded.failing, cancelled = excluded.cancelled, fix_attempts = excluded.fix_attempts,
		blocked_attempts = excluded.blocked_attempts, check_runs = excluded.check_runs,
		generation = excluded.generation, snapshot = excluded.snapshot,
		pushes = excluded.pushes, head_counted = excluded.head_counted, head_counted_before = excluded.head_counted_before,
		planned_red = excluded.planned_red, review_seen = excluded.review_seen,
		review_seen_at = excluded.review_seen_at, state = excluded.state, checked_head = excluded.checked_head,
		required = excluded.required, required_workflows = excluded.required_workflows,
		required_workflows_head = excluded.required_workflows_head, mergeability = excluded.mergeability`,
		pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.HeadSHA, pr.HeadUpdatedAt,
		failing, cancelled, pr.FixAttempts, pr.BlockedAttempts, checkRuns,
		pr.Generation, pr.Snapshot, pushes, pr.HeadCounted, pr.HeadCountedBefore, pr.PlannedRed,
		pr.ReviewSeen.ID, pr.ReviewSeen.SubmittedAt, pr.State, pr.CheckedHead, required,
		workflows, pr.WorkflowsHead, pr.Mergeability,
	)
	if err != nil {
		return fmt.Errorf("put pull request for %s: %w", pr.Issue, err)
	}
	return nil
}

func (s *Postgres) SessionClaimsTree(ctx context.Context, tx pgx.Tx, tree, session string) (bool, error) {
	var claims bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from claims where tree = $1 and session = $2 and session <> '')", tree, session).Scan(&claims); err != nil {
		return false, fmt.Errorf("read the claims of session %s in %s: %w", session, tree, err)
	}
	return claims, nil
}

func (s *Postgres) ClaimSession(ctx context.Context, tx pgx.Tx, token claim.Token) (string, error) {
	var session string
	if err := tx.QueryRow(ctx, "select session from claims where token = $1", string(token)).Scan(&session); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read the session of claim %s: %w", token, err)
	}
	return session, nil
}

// ClearGeneration empties one issue's generation-scoped facts: a merged or closed pull request,
// the counters of one still open, its design gate, its recorded handoffs and a READY the gate
// refused.
func (s *Postgres) ClearGeneration(ctx context.Context, tx pgx.Tx, issue string) error {
	return clearGeneration(ctx, tx, "issue = $1", "key = $1", issue, fmt.Sprintf("clear the generation of %s", issue))
}

// ClearTreeGeneration empties them for every issue of the tree. A root set back to todo starts a
// new generation of the whole tree, and its children carried the old one's pull request, gate and
// handoffs into it — a child re-entered at the new generation read a handoff of the last — and a
// child's READY the old gate refused would wait on a packet cleared with those handoffs.
func (s *Postgres) ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string) error {
	return clearGeneration(ctx, tx, "issue in (select key from issues where tree = $1)", "tree = $1", tree,
		fmt.Sprintf("clear the generation of tree %s", tree))
}

// clearGeneration runs each statement with key as $1: where selects the cleared issues' rows by
// their issue column, and issues selects the same issues in the issues table.
func clearGeneration(ctx context.Context, tx pgx.Tx, where, issues, key, describe string) error {
	for _, statement := range []string{
		"delete from pull_requests where " + where + " and state <> 'open'",
		// An open pull request is kept across a new generation, but the last one's reading of it is
		// not: its counters and its checks settlement are of a head nobody has judged since, and the
		// review round's decision goes with the phases below. Its pushes stay, each a fact about two
		// commits, and so does its newest review, which orders every review it will have, and the
		// checks its base branch requires, which belong to the base, not the generation.
		"update pull_requests set fix_attempts = 0, blocked_attempts = 0, head_counted = '', head_counted_before = '', " +
			"planned_red = false, checked_head = '', failing = '[]'::jsonb, cancelled = '[]'::jsonb, " +
			"check_runs = '[]'::jsonb where " + where,
		"delete from design_gates where " + where,
		"update phases set handoff_commit = '', rounds = 0, verdict = '', summary = '', decision = null, merge_note = '' where " + where,
		// A READY the gate refused waits on the merger's packet, which the statement above clears.
		"update issues set ready_pending_version = null where " + issues,
	} {
		if _, err := tx.Exec(ctx, statement, key); err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
	}
	return nil
}

func scanPullRequest(row scanner) (*PullRequest, error) {
	var pr PullRequest
	var failing, cancelled, checkRuns, pushes, required, workflows []byte
	if err := row.Scan(&pr.Issue, &pr.Repo, &pr.Number, &pr.Branch, &pr.HeadSHA, &pr.HeadUpdatedAt,
		&failing, &cancelled, &pr.FixAttempts, &pr.BlockedAttempts, &checkRuns, &pr.Generation, &pr.Snapshot,
		&pushes, &pr.HeadCounted, &pr.HeadCountedBefore, &pr.PlannedRed, &pr.ReviewSeen.ID, &pr.ReviewSeen.SubmittedAt, &pr.State,
		&pr.CheckedHead, &required, &workflows, &pr.WorkflowsHead, &pr.Mergeability); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(failing, &pr.Failing); err != nil {
		return nil, fmt.Errorf("decode failing checks: %w", err)
	}
	if err := json.Unmarshal(cancelled, &pr.Cancelled); err != nil {
		return nil, fmt.Errorf("decode cancelled checks: %w", err)
	}
	if err := json.Unmarshal(checkRuns, &pr.CheckRuns); err != nil {
		return nil, fmt.Errorf("decode check runs: %w", err)
	}
	if err := json.Unmarshal(pushes, &pr.Pushes); err != nil {
		return nil, fmt.Errorf("decode pushes: %w", err)
	}
	if required != nil {
		if err := json.Unmarshal(required, &pr.Required); err != nil {
			return nil, fmt.Errorf("decode required checks: %w", err)
		}
	}
	if err := json.Unmarshal(workflows, &pr.Workflows); err != nil {
		return nil, fmt.Errorf("decode required workflows: %w", err)
	}
	return &pr, nil
}

func (s *Postgres) Gate(ctx context.Context, tx pgx.Tx, issue string) (*DesignGate, error) {
	var gate DesignGate
	err := tx.QueryRow(ctx, `select issue, artifact_id, latest_version, approved_version from design_gates where issue = $1`, issue).
		Scan(&gate.Issue, &gate.ArtifactID, &gate.LatestVersion, &gate.ApprovedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read design gate for %s: %w", issue, err)
	}
	return &gate, nil
}

func (s *Postgres) GatesForArtifact(ctx context.Context, tx pgx.Tx, artifactID string) ([]DesignGate, error) {
	rows, err := tx.Query(ctx, `select issue, artifact_id, latest_version, approved_version from design_gates
		where artifact_id = $1 order by issue`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list design gates for artifact %s: %w", artifactID, err)
	}
	defer rows.Close()
	var gates []DesignGate
	for rows.Next() {
		var gate DesignGate
		if err := rows.Scan(&gate.Issue, &gate.ArtifactID, &gate.LatestVersion, &gate.ApprovedVersion); err != nil {
			return nil, fmt.Errorf("scan design gate for artifact %s: %w", artifactID, err)
		}
		gates = append(gates, gate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list design gates for artifact %s: %w", artifactID, err)
	}
	return gates, nil
}

func (s *Postgres) PutGate(ctx context.Context, tx pgx.Tx, gate DesignGate) error {
	_, err := tx.Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
		values ($1, $2, $3, $4)
		on conflict (issue) do update set artifact_id = excluded.artifact_id,
		latest_version = excluded.latest_version, approved_version = excluded.approved_version`,
		gate.Issue, gate.ArtifactID, gate.LatestVersion, gate.ApprovedVersion)
	if err != nil {
		return fmt.Errorf("put design gate for %s: %w", gate.Issue, err)
	}
	return nil
}

func (s *Postgres) Slots(ctx context.Context, tx pgx.Tx) ([]Slot, error) {
	rows, err := tx.Query(ctx, "select issue, index, admitted_at from slots order by index, issue")
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	defer rows.Close()
	slots := []Slot{}
	for rows.Next() {
		var slot Slot
		if err := rows.Scan(&slot.Issue, &slot.Index, &slot.AdmittedAt); err != nil {
			return nil, fmt.Errorf("list slots: %w", err)
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	return slots, nil
}

func (s *Postgres) PutSlot(ctx context.Context, tx pgx.Tx, slot Slot) error {
	_, err := tx.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, $2, $3)
		on conflict (issue) do update set index = excluded.index, admitted_at = excluded.admitted_at`,
		slot.Issue, slot.Index, slot.AdmittedAt)
	if err != nil {
		return fmt.Errorf("put slot for %s: %w", slot.Issue, err)
	}
	return nil
}

func (s *Postgres) ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, "delete from slots where issue = $1", issue); err != nil {
		return fmt.Errorf("release slot for %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) ControllerRegistered(ctx context.Context, tx pgx.Tx, project string) (bool, error) {
	// The controllers row is keyed by the project token `legion controller start` mints under
	// (api/controller.go), and registered is controller.Record.Registered's rule: a session holds
	// the current capability.
	token, err := claim.ProjectToken(project)
	if err != nil {
		return false, err
	}
	var registered bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from controllers where project = $1 and session <> '')", token).Scan(&registered); err != nil {
		return false, fmt.Errorf("read whether %s has a registered controller: %w", project, err)
	}
	return registered, nil
}

func (s *Postgres) ControllerNoticePending(ctx context.Context, tx pgx.Tx, project string, kind NoticeKind) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2 and payload->>'kind' = $3)`,
		string(OutboxKindControllerNotice), project, string(kind)).Scan(&pending); err != nil {
		return false, fmt.Errorf("read whether %s has a %s controller notice pending: %w", project, kind, err)
	}
	return pending, nil
}

func (s *Postgres) DisarmPending(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from outbox where kind = $1 and issue = $2)",
		string(OutboxKindAutoMergeDisable), issue).Scan(&pending); err != nil {
		return false, fmt.Errorf("read whether %s has a disarm pending: %w", issue, err)
	}
	return pending, nil
}

const outboxColumns = `id, kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until`

// readyMergeConflict makes a ready_merge enqueue one durable submission per issue and READY head:
// the unique partial index 0044 turns the race between a reopen and a retry into one row. A
// duplicate enqueue keeps the existing row's next_at, a GitHub rate-limit deadline included,
// restarts its attempts, and takes the row from any worker holding it: that worker read the record
// before the reopen, so its finish and retry, each matched on its lease token, must touch nothing,
// and the row decides again on the new record once the lease expires. Its target is that partial
// index, which holds ready_merge rows alone, so no other kind's insert conflicts on it.
const readyMergeConflict = `
	on conflict (issue, (payload ->> 'head')) where kind = 'ready_merge' do update set attempts = 0,
		lease_token = case when outbox.lease_token = '' then '' else gen_random_uuid()::text end`

func (s *Postgres) Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error {
	if !json.Valid(row.Payload) {
		return fmt.Errorf("enqueue %s for %s: payload is not JSON", row.Kind, row.Issue)
	}
	var err error
	if row.CreatedAt.IsZero() {
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`+readyMergeConflict,
			string(row.Kind), row.Issue, string(row.Payload), row.Attempts, row.NextAt, row.LastError, row.LeaseToken, row.LeaseUntil)
	} else {
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`+readyMergeConflict,
			string(row.Kind), row.Issue, string(row.Payload), row.Attempts, row.NextAt, row.LastError, row.CreatedAt, row.LeaseToken, row.LeaseUntil)
	}
	if err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, err)
	}
	return nil
}

// The outbox's runnable predicates are one SQL text, so the lanes' list of due rows (DueRows), their
// claim (ClaimRow) and the state report's held rows (HeldRows) can never disagree about which row
// may run. Each refers to the statement's first five parameters, which outboxDueArgs supplies — $1
// is now, $2 to $4 the kinds the ordering rules name, $5 the lifecycle lane's kinds — so a statement
// embedding one numbers its own parameters from $6. The outer table is `outbox`, unaliased, which
// the rules' subqueries name.

// outboxUnleasedDue is a row due at $1 that no live lease holds.
const outboxUnleasedDue = `outbox.next_at <= $1 and (outbox.lease_until is null or outbox.lease_until <= $1)`

// outboxInOrder is the two rules that hold a due row behind an older one of its issue. An issue's
// Dispatch status writes run one at a time in the order they were made: a status row waits while an
// older one for the same issue is unfinished, because each carries the status its predecessor
// leaves, and a newer write run first would find the board short of it and finish unwritten as
// though a human had moved it. A start of an issue's role waits while an older issue_branch row of
// the issue is unfinished, so no role starts before its issue's branch exists on GitHub
// (IssueBranch), and a create GitHub keeps refusing holds the issue's roles back while the row
// retries. Supervise rows need no other such rule: a stop names the run it ends through the claim's
// own record of the newest start run against it (claims.last_start_row), so ordering them here
// would buy nothing that survives a retry the runtime delayed.
const outboxInOrder = `not (outbox.kind = $2 and exists (select 1 from outbox older
			where older.kind = $2 and older.issue = outbox.issue and older.id < outbox.id))
		and not (outbox.kind = $3 and outbox.payload->>'op' = 'start' and exists (select 1 from outbox branch
			where branch.kind = $4 and branch.issue = outbox.issue and branch.id < outbox.id))`

// outboxLaneFree is the rule that a lifecycle row ($5 is the lifecycle lane's kinds) runs only while
// no other lifecycle row of its issue is under a live lease. Each lifecycle row reads the issue and
// its claims and then acts on what it read — a start launches the role the issue's phase names, a
// suspend stops the roles a start launched, a workspace removal takes the workspace they used — and
// they were written to run one after another, each in the state its predecessor leaves (OutboxLane).
// A lifecycle lane runs its rows that way on its own, but a lane is one daemon's: with the rule in
// SQL, two daemons of a rolling restart never run one issue's lifecycle rows side by side, since the
// second lists and claims nothing of the issue's while the first's lease is live. The state's
// held-rows read (HeldRows) is this rule negated, so what it shows held is exactly what the lanes
// hold back. Board rows are not under it: their lane runs them as they come due.
const outboxLaneFree = `not (outbox.kind = any($5::text[]) and exists (select 1 from outbox running
			where running.issue = outbox.issue and running.id <> outbox.id and running.kind = any($5::text[]) and running.lease_until > $1))`

// outboxClaimable is a row a lane may run now: due, under no live lease, in order, and with no other
// lifecycle row of its issue running.
const outboxClaimable = outboxUnleasedDue + ` and ` + outboxInOrder + ` and ` + outboxLaneFree

// lifecycleKindNames is the lifecycle lane's kinds as the outbox stores them, the $5 every
// statement embedding the predicates takes and the lane UnfinishedLanes sorts rows into.
var lifecycleKindNames = kindNames(laneKinds[LaneLifecycle])

// outboxDueArgs is the parameters of a statement that embeds the predicates: now, the ordering
// rules' kinds and the lifecycle lane's kinds first, then the statement's own.
func outboxDueArgs(now time.Time, own ...any) []any {
	return append([]any{now, string(OutboxKindDispatchStatus), string(OutboxKindSupervise), string(OutboxKindIssueBranch), lifecycleKindNames}, own...)
}

// kindNames is kinds as the text the outbox stores them in, an empty list for none.
func kindNames(kinds []OutboxKind) []string {
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	return names
}

// DueRows lists the rows a lane may run at now (outboxClaimable), oldest first, leasing none: a
// lane reads what is due, then claims each by id (ClaimRow). Issue "" is every issue of project,
// kinds nil every kind. The database is shared by the daemons of several projects, and a row
// belongs to its issue's project: an issue key is `<project>-<n>`, and a project key has no hyphen
// (the configuration's projects keys, claim.IsIssueKey). The scope is in the select, and ClaimRow
// claims only an id a list gave it, because a lease taken on another project's row is what would
// take it from its own daemon.
func (s *Postgres) DueRows(ctx context.Context, tx pgx.Tx, project, issue string, kinds []OutboxKind, now time.Time) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where split_part(outbox.issue, '-', 1) = $6 and ($7::text = '' or outbox.issue = $7::text)
		and (cardinality($8::text[]) = 0 or outbox.kind = any($8::text[]))
		and `+outboxClaimable+`
		order by next_at, id`, outboxDueArgs(now, project, issue, kindNames(kinds))...)
	if err != nil {
		return nil, fmt.Errorf("list due outbox rows: %w", err)
	}
	defer rows.Close()
	due := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list due outbox rows: %w", err)
		}
		due = append(due, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due outbox rows: %w", err)
	}
	return due, nil
}

// ClaimRow leases row id for leaseFor from now, if a lane may still run it at now: it is in the
// outbox, due, under no live lease, in order, and no other lifecycle row of its issue is running
// (outboxClaimable). The claim is one conditional update, never a read and then a write, so two
// lanes that listed the same due row take it exactly once whichever commits first: the other's
// update waits on the row lock, then finds the lease taken, the row finished and gone, or an older
// row of its issue since queued ahead of it, and claims nothing. False is nothing claimed.
//
// The lane rule (outboxLaneFree) is the one predicate the row lock does not cover. Two daemons
// claiming different lifecycle rows of one issue at once update different rows, so neither waits
// on the other's row lock, and each update's subquery runs in its own READ COMMITTED snapshot,
// where the other's uncommitted lease is invisible: Postgres rechecks a concurrently updated row's
// new version against the where clause, but only the row being updated, never a subquery over
// other rows, so both claims would succeed and the issue would have two lifecycle rows running
// side by side. So a lifecycle claim is two statements: first a transaction-scoped advisory lock
// keyed by the row's issue, then the update. The second daemon blocks on the lock until the first
// commits, and its update then takes a fresh snapshot, in which the first's lease is live, and
// refuses. The lock statement is a select over the row, so a board row, or a row finished and gone,
// selects nothing and locks nothing: an issue's board lane never waits on its lifecycle claim. The
// lock is released at the claim transaction's commit, so it is held for the claim alone, never
// across an executor. intake.ApplyFact's advisory lock (hashtext('legion.apply_fact')) is a
// different key, and no transaction takes both — a lane claims in a transaction of its own, and no
// fact handler claims a row — so no lock ordering arises between them.
func (s *Postgres) ClaimRow(ctx context.Context, tx pgx.Tx, id int64, now time.Time, leaseFor time.Duration) (OutboxRow, bool, error) {
	if leaseFor <= 0 {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: lease duration must be positive", id)
	}
	token, err := leaseToken()
	if err != nil {
		return OutboxRow{}, false, err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('legion.outbox.lifecycle:' || issue)) from outbox
		where id = $1 and kind = any($2::text[])`, id, lifecycleKindNames); err != nil {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: %w", id, err)
	}
	row, err := scanOutbox(tx.QueryRow(ctx, `update outbox set lease_token = $7, lease_until = $8
		where outbox.id = $6 and `+outboxClaimable+`
		returning `+outboxColumns, outboxDueArgs(now, id, token, now.Add(leaseFor))...))
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboxRow{}, false, nil
	}
	if err != nil {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: %w", id, err)
	}
	return row, true, nil
}

// RenewOutboxLease moves the lease on row id to until while leaseToken still holds it, so a lane
// running a long row keeps it from the lapse that would let another claim it mid-run. False is a
// lease no longer the caller's: lapsed and claimed by another, or the row finished.
func (s *Postgres) RenewOutboxLease(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, until time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, "update outbox set lease_until = $3 where id = $1 and lease_token = $2", id, leaseToken, until)
	if err != nil {
		return false, fmt.Errorf("renew the lease of outbox row %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// NextDue is the earliest moment after now one of issue's rows of kinds comes due: the least
// next_at after now among the rows no live lease holds, so a lane with nothing to run sleeps until
// it rather than polls. A row due already is not it, nor one another lane is running whose lease
// outlasts its next_at. False is none; kinds nil is every kind.
func (s *Postgres) NextDue(ctx context.Context, tx pgx.Tx, issue string, kinds []OutboxKind, now time.Time) (time.Time, bool, error) {
	var next *time.Time
	if err := tx.QueryRow(ctx, `select min(next_at) from outbox
		where issue = $1 and (cardinality($2::text[]) = 0 or kind = any($2::text[]))
		and next_at > $3 and (lease_until is null or lease_until <= $3)`, issue, kindNames(kinds), now).Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("read when %s's outbox is next due: %w", issue, err)
	}
	if next == nil {
		return time.Time{}, false, nil
	}
	return *next, true, nil
}

// UnfinishedLanes is every lane of project's issues with a row still in the outbox — due, running,
// backing off or held — by issue then lane: the lanes the runner needs an executor for.
func (s *Postgres) UnfinishedLanes(ctx context.Context, tx pgx.Tx, project string) ([]LaneKey, error) {
	rows, err := tx.Query(ctx, `select distinct issue, case when kind = any($2::text[]) then $3::text else $4::text end as lane
		from outbox where split_part(issue, '-', 1) = $1 order by issue, lane`,
		project, lifecycleKindNames, string(LaneLifecycle), string(LaneBoard))
	if err != nil {
		return nil, fmt.Errorf("list the unfinished outbox lanes of %s: %w", project, err)
	}
	defer rows.Close()
	lanes := []LaneKey{}
	for rows.Next() {
		var key LaneKey
		var lane string
		if err := rows.Scan(&key.Issue, &lane); err != nil {
			return nil, fmt.Errorf("list the unfinished outbox lanes of %s: %w", project, err)
		}
		key.Lane = OutboxLane(lane)
		lanes = append(lanes, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the unfinished outbox lanes of %s: %w", project, err)
	}
	return lanes, nil
}

// HeldRows is every due, unleased lifecycle row of project's issues a lane may not run at now
// (outboxClaimable negated) that has a lifecycle row of its issue running to wait on (outboxLaneFree
// negated), by next_at then id, each with that row: the oldest of its issue's running lifecycle
// rows. Issue "" is every issue of project. It is the lanes' own predicates read back, so what the
// state shows held is exactly what the lanes hold back. A start fenced behind an older issue_branch
// row of its issue is held while the branch row runs — issue_branch is a lifecycle kind, so the
// start waits on it — and listed by nothing while the branch row waits unleased for its retry, since
// it then waits on no running row, as it did before the lanes. Board rows are never held: their lane
// runs them as they come due. A row the lane rule holds fails the claim, so the claim's negation
// adds no row to the lane rule's; it is written out because Postgres types a parameter by its uses
// and refuses a statement with one it never uses, and the ordering rules' kinds are among the
// parameters every fragment shares.
func (s *Postgres) HeldRows(ctx context.Context, tx pgx.Tx, project, issue string, now time.Time) ([]HeldRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+`, (select min(running.id) from outbox running
			where running.issue = outbox.issue and running.id <> outbox.id and running.kind = any($5::text[]) and running.lease_until > $1) as waits_on
		from outbox where split_part(outbox.issue, '-', 1) = $6 and ($7::text = '' or outbox.issue = $7::text)
		and outbox.kind = any($5::text[])
		and `+outboxUnleasedDue+` and not (`+outboxClaimable+`) and not `+outboxLaneFree+`
		order by next_at, id`,
		outboxDueArgs(now, project, issue)...)
	if err != nil {
		return nil, fmt.Errorf("list the held outbox rows of %s: %w", project, err)
	}
	defer rows.Close()
	held := []HeldRow{}
	for rows.Next() {
		var row HeldRow
		var err error
		if row.OutboxRow, err = scanOutbox(rows, &row.WaitsOn); err != nil {
			return nil, fmt.Errorf("list the held outbox rows of %s: %w", project, err)
		}
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the held outbox rows of %s: %w", project, err)
	}
	return held, nil
}

func (s *Postgres) WaitingNotices(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and next_at > $2 and split_part(issue, '-', 1) = $3 order by id`, string(OutboxKindNotice), now, project)
	if err != nil {
		return nil, fmt.Errorf("list waiting notices: %w", err)
	}
	defer rows.Close()
	waiting := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list waiting notices: %w", err)
		}
		waiting = append(waiting, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list waiting notices: %w", err)
	}
	return waiting, nil
}

func (s *Postgres) DropStatusWrites(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where kind = $1 and issue = $2", string(OutboxKindDispatchStatus), issue); err != nil {
		return fmt.Errorf("drop the queued status writes of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) DropCatchUps(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where kind = $1 and issue = $2 and payload->>'kind' = 'catch-up'", string(OutboxKindNotice), issue); err != nil {
		return fmt.Errorf("drop the queued catch-ups of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) OutboxLeased(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) (bool, error) {
	var leased bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from outbox where id = $1 and lease_token = $2)", id, leaseToken).Scan(&leased); err != nil {
		return false, fmt.Errorf("read the lease of outbox row %d: %w", id, err)
	}
	return leased, nil
}

func (s *Postgres) ExpediteOutbox(ctx context.Context, tx pgx.Tx, row OutboxRow, now time.Time) error {
	if _, err := tx.Exec(ctx, "update outbox set next_at = $2 where id = $1 and next_at > $2", row.ID, now); err != nil {
		return fmt.Errorf("expedite outbox row %d: %w", row.ID, err)
	}
	return nil
}

func (s *Postgres) TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]Issue, error) {
	rows, err := tx.Query(ctx, "select "+issueColumns+" from issues where tree = $1 order by key", tree)
	if err != nil {
		return nil, fmt.Errorf("list the issues of %s: %w", tree, err)
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("list the issues of %s: %w", tree, err)
		}
		issues = append(issues, *issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the issues of %s: %w", tree, err)
	}
	return issues, nil
}

func (s *Postgres) EarlierNotices(ctx context.Context, tx pgx.Tx, tree string, id int64) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and id < $2 and issue in (select key from issues where tree = $3)
		order by id`, string(OutboxKindNotice), id, tree)
	if err != nil {
		return nil, fmt.Errorf("read the notices of %s before row %d: %w", tree, id, err)
	}
	defer rows.Close()
	earlier := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("read the notices of %s before row %d: %w", tree, id, err)
		}
		earlier = append(earlier, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the notices of %s before row %d: %w", tree, id, err)
	}
	return earlier, nil
}

// ErrOutboxLeaseLost is FinishLeasedOutbox finding the row no longer held by the caller's lease:
// another worker reclaimed it after this lease expired, or it is already gone.
var ErrOutboxLeaseLost = errors.New("outbox row lease is no longer held")

func (s *Postgres) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where id = $1 and lease_token = $2", id, leaseToken); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", id, err)
	}
	return nil
}

func (s *Postgres) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	var finished int64
	err := tx.QueryRow(ctx, "delete from outbox where id = $1 and lease_token = $2 returning id", id, leaseToken).Scan(&finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("finish outbox row %d: %w", id, ErrOutboxLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("finish outbox row %d: %w", id, err)
	}
	return nil
}

func (s *Postgres) RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error {
	if _, err := tx.Exec(ctx, `update outbox set attempts = attempts + 1, next_at = $3, last_error = $4,
		lease_token = '', lease_until = null where id = $1 and lease_token = $2`, id, leaseToken, nextAt, lastErr); err != nil {
		return fmt.Errorf("retry outbox row %d: %w", id, err)
	}
	return nil
}

// RoleRun reads one role's run of an issue generation as the store holds it. RoleRuns owns the
// query shape, so the one-role caller and a state-report batch share it.
func (s *Postgres) RoleRun(ctx context.Context, tx pgx.Tx, token claim.Token, issue string, role claim.Role, generation uint64) (RoleRun, error) {
	runs, err := s.RoleRuns(ctx, tx, []RoleRunKey{{Token: token, Issue: issue, Role: role, Generation: generation}})
	if err != nil {
		return RoleRun{}, err
	}
	return runs[token], nil
}

// roleRunIdentity is a candidate role run as an outbox supervise row names it: its issue, and the
// payload's role and generation exactly as Postgres renders them (`payload->>'role'`,
// `payload->>'generation'`), so a row is matched before it is decoded.
type roleRunIdentity struct {
	issue      string
	role       string
	generation string
}

// RoleRuns reads the claims and queued supervise rows of keys in two bounded queries, under the
// caller's transaction snapshot. The outbox query reads the selected issues' supervise rows by the
// outbox_issue index with each payload's role and generation as text, and decodes only the rows of
// keys' roles and generations. A row of another role is never decoded, so one that does not decode
// fails only a read of its own role's run, as RoleRun's did.
func (s *Postgres) RoleRuns(ctx context.Context, tx pgx.Tx, keys []RoleRunKey) (map[claim.Token]RoleRun, error) {
	runs := make(map[claim.Token]RoleRun, len(keys))
	if len(keys) == 0 {
		return runs, nil
	}
	tokens := make([]string, 0, len(keys))
	issues := make([]string, 0, len(keys))
	seenIssues := make(map[string]struct{}, len(keys))
	candidates := make(map[roleRunIdentity]claim.Token, len(keys))
	for _, key := range keys {
		if _, exists := runs[key.Token]; exists {
			continue
		}
		runs[key.Token] = RoleRun{}
		tokens = append(tokens, string(key.Token))
		candidates[roleRunIdentity{issue: key.Issue, role: string(key.Role), generation: strconv.FormatUint(key.Generation, 10)}] = key.Token
		if _, seen := seenIssues[key.Issue]; !seen {
			seenIssues[key.Issue] = struct{}{}
			issues = append(issues, key.Issue)
		}
	}

	rows, err := tx.Query(ctx, `select c.token, c.state, c.tree, c.suspended_by, c.last_start_row, d.claim_token is not null,
		coalesce(d.generation, 0), coalesce(d.phase, ''), d.confirmed_at
		from claims c left join pending_task_deliveries d on d.claim_token = c.token where c.token = any($1)`, tokens)
	if err != nil {
		return nil, fmt.Errorf("read the claims of %d role runs: %w", len(keys), err)
	}
	for rows.Next() {
		var token string
		var held RoleClaim
		var pending bool
		var task supervise.Delivery
		var confirmed *time.Time
		if err := rows.Scan(&token, &held.State, &held.Tree, &held.SuspendedBy, &held.LastStartRow, &pending, &task.Generation, &task.Phase, &confirmed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the claims of %d role runs: %w", len(keys), err)
		}
		if pending {
			if confirmed != nil {
				task.ConfirmedAt = *confirmed
			}
			held.Pending = &task
		}
		run := runs[claim.Token(token)]
		run.Claim = &held
		runs[claim.Token(token)] = run
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the claims of %d role runs: %w", len(keys), err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `select `+outboxColumns+`, coalesce(payload->>'role', ''), coalesce(payload->>'generation', '')
		from outbox where kind = $1 and issue = any($2) order by id`, string(OutboxKindSupervise), issues)
	if err != nil {
		return nil, fmt.Errorf("read the queued role runs of %d issues: %w", len(issues), err)
	}
	defer rows.Close()
	for rows.Next() {
		var row OutboxRow
		var kind, role, generation string
		var payload []byte
		if err := rows.Scan(&row.ID, &kind, &row.Issue, &payload, &row.Attempts, &row.NextAt, &row.LastError,
			&row.CreatedAt, &row.LeaseToken, &row.LeaseUntil, &role, &generation); err != nil {
			return nil, fmt.Errorf("read the queued role runs of %d issues: %w", len(issues), err)
		}
		token, wanted := candidates[roleRunIdentity{issue: row.Issue, role: role, generation: generation}]
		if !wanted {
			continue
		}
		row.Kind = OutboxKind(kind)
		row.Payload = append(json.RawMessage(nil), payload...)
		decoded, err := DecodeOutboxPayload(row)
		if err != nil {
			return nil, err
		}
		request, ok := decoded.(SuperviseRequest)
		if !ok {
			return nil, fmt.Errorf("outbox row %d of kind %s decodes to %T", row.ID, row.Kind, decoded)
		}
		run := runs[token]
		run.Queued = append(run.Queued, QueuedSupervise{ID: row.ID, Request: request})
		runs[token] = run
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the queued role runs of %d issues: %w", len(issues), err)
	}
	return runs, nil
}

// PendingStatusWrites lists every Dispatch status write of project's issues the outbox has not
// finished: due now, in flight, backing off after a failed attempt, or waiting behind an older
// write for the same issue. Finishing deletes the row, so each one left is a write the outbox has
// not finished; a write that reached Dispatch whose finish failed stays listed until its lease
// expires and the rerun finishes it. Oldest first: the order one issue's statuses run in
// (outboxInOrder).
func (s *Postgres) PendingStatusWrites(ctx context.Context, tx pgx.Tx, project string) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2 order by id`, string(OutboxKindDispatchStatus), project)
	if err != nil {
		return nil, fmt.Errorf("list pending Dispatch status writes: %w", err)
	}
	defer rows.Close()
	pending := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending Dispatch status writes: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending Dispatch status writes: %w", err)
	}
	return pending, nil
}

// scanOutbox reads one row of outboxColumns, then extra: the destinations of any columns the
// statement selects after them.
func scanOutbox(row scanner, extra ...any) (OutboxRow, error) {
	var out OutboxRow
	var kind string
	var payload []byte
	if err := row.Scan(append([]any{&out.ID, &kind, &out.Issue, &payload, &out.Attempts, &out.NextAt, &out.LastError,
		&out.CreatedAt, &out.LeaseToken, &out.LeaseUntil}, extra...)...); err != nil {
		return OutboxRow{}, err
	}
	out.Kind = OutboxKind(kind)
	out.Payload = append(json.RawMessage(nil), payload...)
	return out, nil
}

func leaseToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint outbox lease token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

type scanner interface {
	Scan(dest ...any) error
}

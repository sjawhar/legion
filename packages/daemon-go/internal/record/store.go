package record

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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

const issueColumns = `key, tree, project, title, parent, phase, generation, status, rank, linger_until, held_from, last_dispatch_seq, ready_pending_version`

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
	if issue.HeldFrom != nil {
		heldFrom = string(*issue.HeldFrom)
	}
	_, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, parent, phase, generation, status, rank, linger_until, held_from, last_dispatch_seq, ready_pending_version)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		on conflict (key) do update set tree = excluded.tree, project = excluded.project, title = excluded.title,
		parent = excluded.parent, phase = excluded.phase, generation = excluded.generation,
		status = excluded.status, rank = excluded.rank, linger_until = excluded.linger_until,
		held_from = excluded.held_from, last_dispatch_seq = excluded.last_dispatch_seq,
		ready_pending_version = excluded.ready_pending_version`,
		issue.Key, issue.Tree, issue.Project, issue.Title, issue.Parent, string(issue.Phase), int64(issue.Generation), issue.Status,
		issue.Rank, issue.LingerUntil, heldFrom, issue.LastDispatchSeq, issue.ReadyPendingVersion,
	)
	if err != nil {
		return fmt.Errorf("put issue %s: %w", issue.Key, err)
	}
	return nil
}

func scanIssue(row scanner) (*Issue, error) {
	var issue Issue
	var phaseValue string
	var generation int64
	var heldFrom *string
	if err := row.Scan(&issue.Key, &issue.Tree, &issue.Project, &issue.Title, &issue.Parent, &phaseValue, &generation, &issue.Status,
		&issue.Rank, &issue.LingerUntil, &heldFrom, &issue.LastDispatchSeq, &issue.ReadyPendingVersion); err != nil {
		return nil, err
	}
	if generation < 0 {
		return nil, fmt.Errorf("issue %s has negative generation %d", issue.Key, generation)
	}
	issue.Phase = phase.Phase(phaseValue)
	if heldFrom != nil {
		value := phase.Phase(*heldFrom)
		issue.HeldFrom = &value
	}
	issue.Generation = uint64(generation)
	return &issue, nil
}

func (s *Postgres) Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error) {
	rows, err := tx.Query(ctx, `select issue, role, claim, handoff_commit, rounds, verdict from phases
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
	_, err := tx.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (issue, role) do update set claim = excluded.claim,
		handoff_commit = excluded.handoff_commit, rounds = excluded.rounds, verdict = excluded.verdict`,
		phase.Issue, string(phase.Role), string(phase.Claim), phase.HandoffCommit, phase.Rounds, phase.Verdict,
	)
	if err != nil {
		return fmt.Errorf("put %s phase on %s: %w", phase.Role, phase.Issue, err)
	}
	return nil
}

func scanPhase(row scanner) (PhaseRow, error) {
	var phase PhaseRow
	var role, token string
	if err := row.Scan(&phase.Issue, &role, &token, &phase.HandoffCommit, &phase.Rounds, &phase.Verdict); err != nil {
		return PhaseRow{}, err
	}
	phase.Role, phase.Claim = claim.Role(role), claim.Token(token)
	return phase, nil
}

const pullRequestColumns = `issue, repo, number, branch, head_sha, head_updated_at, head_updated_at_source,
	verdict, failing, failing_statuses, review_decision, fix_attempts, blocked_attempts, check_runs,
	generation, snapshot, reconciled, pending_push, head_counted`

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

func (s *Postgres) PutPullRequest(ctx context.Context, tx pgx.Tx, pr PullRequest) error {
	failing, err := json.Marshal(pr.Failing)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode failing checks: %w", pr.Issue, err)
	}
	failingStatuses, err := json.Marshal(pr.FailingStatuses)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode failing statuses: %w", pr.Issue, err)
	}
	checkRuns, err := json.Marshal(pr.CheckRuns)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode check runs: %w", pr.Issue, err)
	}
	var pendingPush []byte
	if pr.PendingPush != nil {
		pendingPush, err = json.Marshal(pr.PendingPush)
		if err != nil {
			return fmt.Errorf("put pull request for %s: encode pending push: %w", pr.Issue, err)
		}
	}
	_, err = tx.Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		head_updated_at_source, verdict, failing, failing_statuses, review_decision, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, reconciled, pending_push, head_counted)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		on conflict (issue) do update set repo = excluded.repo, number = excluded.number, branch = excluded.branch,
		head_sha = excluded.head_sha, head_updated_at = excluded.head_updated_at,
		head_updated_at_source = excluded.head_updated_at_source, verdict = excluded.verdict,
		failing = excluded.failing, failing_statuses = excluded.failing_statuses,
		review_decision = excluded.review_decision, fix_attempts = excluded.fix_attempts,
		blocked_attempts = excluded.blocked_attempts, check_runs = excluded.check_runs,
		generation = excluded.generation, snapshot = excluded.snapshot, reconciled = excluded.reconciled,
		pending_push = excluded.pending_push, head_counted = excluded.head_counted`,
		pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.HeadSHA, pr.HeadUpdatedAt, pr.HeadUpdatedAtSource,
		pr.Verdict, failing, failingStatuses, pr.ReviewDecision, pr.FixAttempts, pr.BlockedAttempts, checkRuns,
		pr.Generation, pr.Snapshot, pr.Reconciled, pendingPush, pr.HeadCounted,
	)
	if err != nil {
		return fmt.Errorf("put pull request for %s: %w", pr.Issue, err)
	}
	return nil
}

func (s *Postgres) DeletePullRequest(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, "delete from pull_requests where issue = $1", issue); err != nil {
		return fmt.Errorf("delete pull request for %s: %w", issue, err)
	}
	return nil
}

func scanPullRequest(row scanner) (*PullRequest, error) {
	var pr PullRequest
	var failing, failingStatuses, checkRuns, pendingPush []byte
	if err := row.Scan(&pr.Issue, &pr.Repo, &pr.Number, &pr.Branch, &pr.HeadSHA, &pr.HeadUpdatedAt,
		&pr.HeadUpdatedAtSource, &pr.Verdict, &failing, &failingStatuses, &pr.ReviewDecision,
		&pr.FixAttempts, &pr.BlockedAttempts, &checkRuns, &pr.Generation, &pr.Snapshot, &pr.Reconciled,
		&pendingPush, &pr.HeadCounted); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(failing, &pr.Failing); err != nil {
		return nil, fmt.Errorf("decode failing checks: %w", err)
	}
	if err := json.Unmarshal(failingStatuses, &pr.FailingStatuses); err != nil {
		return nil, fmt.Errorf("decode failing statuses: %w", err)
	}
	if err := json.Unmarshal(checkRuns, &pr.CheckRuns); err != nil {
		return nil, fmt.Errorf("decode check runs: %w", err)
	}
	if pendingPush != nil {
		pr.PendingPush = &PendingPush{}
		if err := json.Unmarshal(pendingPush, pr.PendingPush); err != nil {
			return nil, fmt.Errorf("decode pending push: %w", err)
		}
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

const outboxColumns = `id, kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until`

func (s *Postgres) Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error {
	if !json.Valid(row.Payload) {
		return fmt.Errorf("enqueue %s for %s: payload is not JSON", row.Kind, row.Issue)
	}
	var err error
	if row.CreatedAt.IsZero() {
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			string(row.Kind), row.Issue, string(row.Payload), row.Attempts, row.NextAt, row.LastError, row.LeaseToken, row.LeaseUntil)
	} else {
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			string(row.Kind), row.Issue, string(row.Payload), row.Attempts, row.NextAt, row.LastError, row.CreatedAt, row.LeaseToken, row.LeaseUntil)
	}
	if err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, err)
	}
	return nil
}

// ClaimDue leases due rows, oldest first. An issue's Dispatch status writes run one at a time in
// the order they were made: a status row waits while an older one for the same issue is unfinished,
// because each carries the status its predecessor leaves, and a newer write run first would find the
// board short of it and finish unwritten as though a human had moved it.
func (s *Postgres) ClaimDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int, leaseFor time.Duration) ([]OutboxRow, error) {
	if limit <= 0 {
		return []OutboxRow{}, nil
	}
	if leaseFor <= 0 {
		return nil, fmt.Errorf("claim due outbox rows: lease duration must be positive")
	}
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where next_at <= $1 and (lease_until is null or lease_until <= $1)
		and not (kind = $3 and exists (select 1 from outbox older
			where older.kind = $3 and older.issue = outbox.issue and older.id < outbox.id))
		order by next_at, id limit $2 for update skip locked`, now, limit, string(OutboxKindDispatchStatus))
	if err != nil {
		return nil, fmt.Errorf("claim due outbox rows: %w", err)
	}
	defer rows.Close()
	claimed := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("claim due outbox rows: %w", err)
		}
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim due outbox rows: %w", err)
	}
	for i := range claimed {
		token, err := leaseToken()
		if err != nil {
			return nil, err
		}
		until := now.Add(leaseFor)
		if _, err := tx.Exec(ctx, "update outbox set lease_token = $2, lease_until = $3 where id = $1", claimed[i].ID, token, until); err != nil {
			return nil, fmt.Errorf("claim outbox row %d: %w", claimed[i].ID, err)
		}
		claimed[i].LeaseToken, claimed[i].LeaseUntil = token, &until
	}
	return claimed, nil
}

func (s *Postgres) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where id = $1 and lease_token = $2", id, leaseToken); err != nil {
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

func (s *Postgres) PendingStatusWrites(ctx context.Context, tx pgx.Tx) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and next_at <= now() order by next_at, id`, string(OutboxKindDispatchStatus))
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

func scanOutbox(row scanner) (OutboxRow, error) {
	var out OutboxRow
	var kind string
	var payload []byte
	if err := row.Scan(&out.ID, &kind, &out.Issue, &payload, &out.Attempts, &out.NextAt, &out.LastError,
		&out.CreatedAt, &out.LeaseToken, &out.LeaseUntil); err != nil {
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

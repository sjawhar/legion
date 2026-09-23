package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	outboxBatchSize = 32
	outboxLease     = 30 * time.Second
	outboxPoll      = 100 * time.Millisecond
	// outboxFailedTickWait spaces the ticks while the database refuses, so an outage logs once a
	// second rather than ten times.
	outboxFailedTickWait = time.Second
	messageReadSkew      = 5 * time.Second
)

// outbox runs each effect that the workflow transaction committed. Claiming and finishing have
// separate transactions so a slow external service never holds an issue transition lock.
type outbox struct {
	pool       *pgxpool.Pool
	records    record.Store
	dispatch   dispatch.Client
	notices    notify.Publisher
	supervisor *supervisor
	tokens     appauth.Tokens
	handlers   []intake.Handler
	project    string
	stateDir   string
	repo       string
	log        *slog.Logger
	now        func() time.Time
	provision  func(context.Context, workspace.Request) (workspace.Workspace, error)
	remove     func(context.Context, workspace.Workspace) error
}

func newOutbox(pool *pgxpool.Pool, records record.Store, client dispatch.Client, publisher notify.Publisher, supervisor *supervisor, tokens appauth.Tokens, handlers []intake.Handler, project, stateDir string, configured config.Project, tools map[string]string, log *slog.Logger) *outbox {
	if log == nil {
		log = slog.Default()
	}
	return &outbox{
		pool: pool, records: records, dispatch: client, notices: publisher, supervisor: supervisor, tokens: tokens,
		handlers: handlers, project: project, stateDir: stateDir, repo: configured.Repo, log: log, now: time.Now,
		provision: func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
			return workspace.Provision(ctx, workspace.NewRunner(5*time.Minute, tools), request)
		},
		remove: func(ctx context.Context, working workspace.Workspace) error {
			return workspace.Remove(ctx, workspace.NewRunner(5*time.Minute, tools), working)
		},
	}
}

// Run keeps due effects draining until the daemon stops. It runs immediately so a restart never
// waits one poll interval before recovering a row whose prior lease expired. A tick whose claim
// fails (a Postgres restart drops the pool's connections) is logged and tried again after
// outboxFailedTickWait, so the runner outlives the failure instead of ending with it.
func (r *outbox) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		wait := outboxPoll
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("outbox tick failed; the next tick tries again", "error", err)
			wait = outboxFailedTickWait
		}
		timer.Reset(wait)
	}
}

// RunOnce claims currently due rows, executes each side effect outside the lease transaction, and
// finishes or retries only while its lease token still matches. A row whose finish or retry fails
// is logged and left leased: its lease expires and the row is due again, so one row's failed
// transaction never stops the rows after it.
func (r *outbox) RunOnce(ctx context.Context) error {
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.pool == nil || r.records == nil {
		return errors.New("outbox requires Postgres and its record store")
	}
	now := r.now().UTC()
	var rows []record.OutboxRow
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = r.records.ClaimDue(ctx, tx, now, outboxBatchSize, outboxLease)
		return err
	}); err != nil {
		return fmt.Errorf("claim due outbox rows: %w", err)
	}
	for _, row := range rows {
		if err := r.execute(ctx, row); err != nil {
			r.log.Error("outbox row failed", "row", row.ID, "kind", row.Kind, "error", err)
			if retryErr := r.retry(ctx, row, err); retryErr != nil {
				r.log.Error("outbox row retry not recorded; it runs again when its lease expires", "row", row.ID, "error", retryErr)
			}
			continue
		}
		if err := r.finish(ctx, row); err != nil {
			r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
		}
	}
	return nil
}

func (r *outbox) finish(ctx context.Context, row record.OutboxRow) error {
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", row.ID, err)
	}
	return nil
}

func (r *outbox) retry(ctx context.Context, row record.OutboxRow, cause error) error {
	nextAt := r.now().UTC().Add(retryBackoff(row.Attempts))
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, nextAt, cause.Error())
	}); err != nil {
		return fmt.Errorf("retry outbox row %d: %w", row.ID, err)
	}
	return nil
}

func retryBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 6 {
		attempts = 6
	}
	return time.Second << attempts
}

func (r *outbox) execute(ctx context.Context, row record.OutboxRow) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	switch value := payload.(type) {
	case record.StatusWrite:
		return r.status(ctx, row, value)
	case record.MessagePost:
		return r.message(ctx, row, value)
	case record.Notice:
		return r.notice(ctx, row, value)
	case record.SuperviseRequest:
		return r.supervise(ctx, row, value)
	case record.GateSeed:
		return r.seedGate(ctx, row, value)
	case record.LingerClose:
		return r.linger(ctx, row, value)
	case record.WorkspaceRemove:
		return r.removeWorkspace(ctx, row)
	default:
		return fmt.Errorf("outbox row %d: no executor for %T", row.ID, payload)
	}
}

func (r *outbox) status(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) error {
	if r.dispatch == nil {
		return errors.New("dispatch status executor has no Dispatch client")
	}
	issue, err := r.dispatch.GetIssue(ctx, row.Issue)
	if err != nil {
		return fmt.Errorf("read Dispatch issue %s before status write: %w", row.Issue, err)
	}
	if issue.Status != payload.ObservedStatus && issue.Status != payload.Status {
		return nil
	}
	if issue.Status == payload.Status {
		return nil
	}
	if err := r.dispatch.SetStatus(ctx, row.Issue, payload.Status); err != nil {
		return fmt.Errorf("set Dispatch issue %s to %s: %w", row.Issue, payload.Status, err)
	}
	return nil
}

func (r *outbox) message(ctx context.Context, row record.OutboxRow, payload record.MessagePost) error {
	if r.dispatch == nil {
		return errors.New("dispatch message executor has no Dispatch client")
	}
	marker := fmt.Sprintf("<!-- legion-outbox:%d -->", row.ID)
	bodies, err := r.dispatch.MessageBodiesSince(ctx, row.Issue, row.CreatedAt.Add(-messageReadSkew))
	if err != nil {
		return fmt.Errorf("read Dispatch messages for %s: %w", row.Issue, err)
	}
	for _, body := range bodies {
		if strings.Contains(body, marker) {
			return nil
		}
	}
	if err := r.dispatch.PostMessage(ctx, row.Issue, payload.Body+"\n\n"+marker); err != nil {
		return fmt.Errorf("post Dispatch message for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) notice(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if r.notices == nil {
		return errors.New("notice executor has no Envoy publisher")
	}
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if issue.Tree == "" {
		return fmt.Errorf("notice row %d issue %s has no tree root", row.ID, row.Issue)
	}
	message := fmt.Sprintf("%s on %s", payload.Kind, row.Issue)
	dedupeKey := fmt.Sprintf("legion-outbox:%d", row.ID)
	token, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return fmt.Errorf("the notice topic of %s: %w", row.Issue, err)
	}
	if err := r.notices.Publish(ctx, notify.Topic(token, row.Issue), message, payload, dedupeKey); err != nil {
		return fmt.Errorf("publish issue notice for %s: %w", row.Issue, err)
	}
	if issue.Tree != issue.Key {
		if err := r.notices.Publish(ctx, notify.Topic(token, issue.Tree), message, payload, dedupeKey); err != nil {
			return fmt.Errorf("publish tree notice for %s: %w", row.Issue, err)
		}
	}
	return nil
}

func (r *outbox) supervise(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) error {
	if r.supervisor == nil {
		return errors.New("supervise executor has no claim supervisor")
	}
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Generation != issue.Generation {
		r.log.Info("outbox supervise row serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
			"generation", payload.Generation, "current", issue.Generation, "op", payload.Op, "role", payload.Role)
		return nil
	}
	if payload.Tree != issue.Tree {
		return fmt.Errorf("supervise row %d tree %s does not match issue %s tree %s", row.ID, payload.Tree, issue.Key, issue.Tree)
	}
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	switch payload.Op {
	case "start":
		if !found {
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			machine, _, err = r.supervisor.Create(ctx, supervise.Claim{
				Token: token, Project: r.project, Tree: issue.Tree, Issue: issue.Key, Role: payload.Role, State: supervise.StateQueued,
			}, "")
			if err != nil {
				return fmt.Errorf("create claim %s: %w", token, err)
			}
		}
		state := machine.Claim().State
		switch state {
		case supervise.StateQueued:
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
				return fmt.Errorf("start claim %s: %w", token, err)
			}
		case supervise.StateSuspended:
			if err := machine.Handle(ctx, supervise.RequestResume{Claim: token}); err != nil {
				return fmt.Errorf("resume claim %s: %w", token, err)
			}
		case supervise.StateFailed:
			// Only the workflow starts a role whose claim failed: the architect retrying the
			// held phase, or a later transition that needs the role again.
			if err := machine.Handle(ctx, supervise.RequestRetry{Claim: token}); err != nil {
				return fmt.Errorf("retry claim %s: %w", token, err)
			}
		case supervise.StateRetired:
			// A retired claim's tree closed, and its linger removed the workspace; the tree was
			// re-admitted, so the workspace comes back before the kept session relaunches.
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			if err := machine.Handle(ctx, supervise.RequestRetry{Claim: token}); err != nil {
				return fmt.Errorf("relaunch retired claim %s: %w", token, err)
			}
		}
		if payload.Task != "" {
			if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: payload.Task, ID: fmt.Sprintf("outbox:%d", row.ID)}); err != nil {
				return fmt.Errorf("deliver to claim %s: %w", token, err)
			}
		}
		return nil
	case "suspend":
		if !found {
			return nil
		}
		switch machine.Claim().State {
		case supervise.StateFailed, supervise.StateRetired:
			// The claim runs nothing, so there is nothing to suspend.
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token}); err != nil {
			return fmt.Errorf("suspend claim %s: %w", token, err)
		}
		return nil
	case "stop":
		if !found {
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestStop{Claim: token}); err != nil {
			return fmt.Errorf("stop claim %s: %w", token, err)
		}
		return nil
	default:
		return fmt.Errorf("outbox row %d has unknown supervise operation %q", row.ID, payload.Op)
	}
}

func (r *outbox) provisionWorkspace(ctx context.Context, issue record.Issue) error {
	if r.tokens == nil {
		return errors.New("supervise executor has no GitHub App token manager")
	}
	owner, _, ok := strings.Cut(r.repo, "/")
	if !ok || owner == "" {
		return fmt.Errorf("supervise executor has invalid repository %q", r.repo)
	}
	lease, err := r.tokens.Token(ctx, appauth.Implement, owner)
	if err != nil {
		return fmt.Errorf("mint implement App token to provision %s: %w", issue.Key, err)
	}
	if _, err := r.provision(ctx, workspace.Request{
		StateDir: r.stateDir, Repo: r.repo, Issue: issue.Key, Token: lease.Token, CredentialHelper: credentialHelper(r.stateDir),
	}); err != nil {
		return fmt.Errorf("provision workspace for %s: %w", issue.Key, err)
	}
	return nil
}

func (r *outbox) seedGate(ctx context.Context, row record.OutboxRow, payload record.GateSeed) error {
	if r.dispatch == nil {
		return errors.New("gate seed executor has no Dispatch client")
	}
	approval, err := r.dispatch.Approval(ctx, payload.ArtifactID)
	if err != nil {
		return fmt.Errorf("read Dispatch approval %s: %w", payload.ArtifactID, err)
	}
	// The version Dispatch holds is the current one, even one the architect did not see: approved
	// there, the gate opens at it; otherwise a later version still moves the gate to it, closed.
	fact := intake.DispatchArtifact{Key: row.Issue, ArtifactID: payload.ArtifactID, Kind: intake.DispatchArtifactVersion, Version: approval.LatestVersion}
	switch {
	case approval.State == "approved" && approval.Version != nil && *approval.Version == approval.LatestVersion:
		fact.Kind = intake.DispatchArtifactApproved
	case approval.LatestVersion <= payload.Version:
		return nil
	}
	_, err = intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("gate-seed:%s:%d:%s:%s:%d", row.Issue, payload.Generation, payload.ArtifactID, fact.Kind, fact.Version), fact, r.handlers...)
	if err != nil {
		return fmt.Errorf("apply approved gate seed for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) linger(ctx context.Context, row record.OutboxRow, payload record.LingerClose) error {
	_, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("linger:%s:%d", row.Issue, payload.Generation), intake.LingerExpired{
		Issue: row.Issue, Generation: payload.Generation,
	}, r.handlers...)
	if err != nil {
		return fmt.Errorf("apply linger expiration for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) removeWorkspace(ctx context.Context, row record.OutboxRow) error {
	if r.repo == "" {
		return errors.New("workspace removal has no configured repository")
	}
	working, err := workspace.Location(r.stateDir, r.repo, row.Issue)
	if err != nil {
		return fmt.Errorf("locate workspace for %s: %w", row.Issue, err)
	}
	if err := r.remove(ctx, working); err != nil {
		return fmt.Errorf("remove workspace for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) issue(ctx context.Context, key string) (record.Issue, error) {
	var issue *record.Issue
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		return record.Issue{}, fmt.Errorf("read workflow issue %s: %w", key, err)
	}
	if issue == nil {
		return record.Issue{}, fmt.Errorf("workflow issue %s is not recorded", key)
	}
	return *issue, nil
}

// credentialHelper is the git credential helper every issue workspace names: this daemon's pane
// launcher by its absolute path, so a push from any directory reaches `legion credential`.
func credentialHelper(stateDir string) string {
	return "!'" + strings.ReplaceAll(filepath.Join(stateDir, "bin", "legion"), "'", `'\''`) + "' credential"
}

package architecture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SourceDir is the fixed directory an architecture source's model is read
// from, at the configured branch's head commit.
const SourceDir = ".dispatch/architecture"

// ErrNoSource is "this project has no architecture source configured".
var ErrNoSource = errors.New("no architecture source configured")

// SystemActor authors every importer-written event: the importer is not a
// human and not a session.
var SystemActor = model.Actor{Kind: "system", ID: "architecture-importer"}

// SourceColumns is the architecture_sources select list ScanSource reads, in
// scan order; every query that returns a source row selects exactly this.
const SourceColumns = `project_key, repo, branch, enabled, installation_id, created_by, created_at, last_sync_at, last_commit, last_error, last_tree_sha`

// syncTimeout bounds one whole sync — every GitHub call plus the projection —
// so a slow upstream cannot hold the per-project lock (and every queued
// Refresh behind it) indefinitely.
var syncTimeout = 2 * time.Minute

// errSourceMoved is a sync that found the source's repo or branch changed
// while its fetch was in flight: nothing was written, and the next trigger
// imports the new configuration.
var errSourceMoved = errors.New("the architecture source changed during the sync; nothing was recorded")

// Importer imports one project's architecture model from its configured
// source. Every trigger — the five-minute ticker, the Settings Refresh button,
// and the dispatch_architecture_sync tool — funnels through Sync, and syncs of
// one project are serialized by a per-project mutex (the server is one
// process), so two refreshes of the same project cannot interleave their
// projections.
type Importer struct {
	store  *store.Store
	github *githubapp.Client
	events *events.Broker

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewImporter wires the importer to the database, the GitHub App client (nil
// is the "no app credentials" state: every sync answers ErrNoAppKey), and the
// event broker.
func NewImporter(database *store.Store, github *githubapp.Client, broker *events.Broker) *Importer {
	return &Importer{store: database, github: github, events: broker, locks: map[string]*sync.Mutex{}}
}

func (i *Importer) lock(project string) *sync.Mutex {
	i.mu.Lock()
	defer i.mu.Unlock()
	lock, ok := i.locks[project]
	if !ok {
		lock = &sync.Mutex{}
		i.locks[project] = lock
	}
	return lock
}

// HasApp reports whether the App credentials needed to sync are configured.
func (i *Importer) HasApp() bool {
	return i != nil && i.github != nil
}

// ListEnabled names every project with an enabled architecture source, in key
// order: the ticker's work list.
func (i *Importer) ListEnabled(ctx context.Context) ([]string, error) {
	rows, err := i.store.Pool.Query(ctx, `
		select project_key from architecture_sources where enabled order by project_key
	`)
	if err != nil {
		return nil, fmt.Errorf("list enabled architecture sources: %w", err)
	}
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, fmt.Errorf("scan enabled architecture source: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enabled architecture sources: %w", err)
	}
	return projects, nil
}

// Sync imports project's architecture model at its source branch's head.
//
// It returns the source row as it now stands plus, when the import failed, the
// failure itself: the row carries the same text in last_error, the previous
// projection stays up (the spec's drift rule), and the caller decides how to
// answer. A missing source row is ErrNoSource with a zero row.
//
// A healthy source whose branch head is the recorded commit — or whose new
// head carries an unchanged architecture subtree — only refreshes the
// bookkeeping (last_sync_at, and the commit for the moved head): no file
// fetch, no re-projection, no event, so a caller looping the sync route is
// bounded to the head lookup.
//
// Events follow state changes, not ticks: `architecture.synced` when the
// imported model changes (or the source is recovering from a failure),
// `architecture.sync_failed` when the error text first appears or changes. A
// steady source — same commit, or the same failure over and over — emits
// nothing.
func (i *Importer) Sync(ctx context.Context, project string) (model.ArchitectureSource, error) {
	lock := i.lock(project)
	lock.Lock()
	defer lock.Unlock()
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	source, err := i.loadSource(ctx, project)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	owner, name, token, importErr := i.resolve(ctx, &source)
	if importErr != nil {
		return i.failed(ctx, parent, source, importErr)
	}
	commit, importErr := i.github.Ref(ctx, token, owner, name, source.Branch)
	if importErr != nil {
		return i.failed(ctx, parent, source, importErr)
	}
	// Unchanged head of a healthy source: the projection already is this
	// commit. Refresh the freshness stamp only.
	if source.LastError == nil && source.LastCommit != nil && *source.LastCommit == commit {
		return i.touch(ctx, source, commit)
	}
	listing, importErr := i.github.Dir(ctx, token, owner, name, commit, SourceDir)
	if importErr != nil {
		return i.failed(ctx, parent, source, importErr)
	}
	// The head moved but the architecture directory's tree object is
	// identical: record the new commit without re-fetching or re-projecting.
	if source.LastError == nil && source.LastTreeSha != nil && *source.LastTreeSha == listing.SHA {
		return i.touch(ctx, source, commit)
	}
	files, importErr := i.github.DirFiles(ctx, token, owner, name, listing)
	if importErr != nil {
		return i.failed(ctx, parent, source, importErr)
	}
	parsed, importErr := Parse(files)
	if importErr != nil {
		return i.failed(ctx, parent, source, importErr)
	}
	return i.project(ctx, source, commit, listing.SHA, files, parsed)
}

// failed records importErr on the source row and hands both back: the Sync
// contract for every import failure. When the sync's own deadline is what
// failed (the caller is still alive), the short record write runs on a
// cancel-free context so the timeout lands on the row like any other failure;
// a caller that has gone away (shutdown, client hang-up) records nothing.
func (i *Importer) failed(ctx, parent context.Context, source model.ArchitectureSource, importErr error) (model.ArchitectureSource, error) {
	recordCtx := ctx
	if ctx.Err() != nil && parent.Err() == nil {
		recordCtx = context.WithoutCancel(parent)
	}
	updated, err := i.recordFailure(recordCtx, source, importErr)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	return updated, importErr
}

// touch is the unchanged-model short-circuit's only write: last_sync_at (and
// the moved head's commit), guarded by the repo/branch/state the sync began
// from so a concurrent PUT is never overstamped. No event either way.
func (i *Importer) touch(ctx context.Context, source model.ArchitectureSource, commit string) (model.ArchitectureSource, error) {
	updated, err := ScanSource(i.store.Pool.QueryRow(ctx, `
		update architecture_sources
		set last_sync_at = now(), last_commit = $4
		where project_key = $1 and repo = $2 and branch = $3 and last_error is null
		returning `+SourceColumns, source.Project, source.Repo, source.Branch, commit))
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.ArchitectureSource{}, fmt.Errorf("record architecture sync freshness: %w", err)
	}
	// The source moved (or vanished) mid-sync: leave the row alone.
	updated, err = i.loadSource(ctx, source.Project)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	return updated, errSourceMoved
}

func (i *Importer) loadSource(ctx context.Context, project string) (model.ArchitectureSource, error) {
	source, err := ScanSource(i.store.Pool.QueryRow(ctx,
		`select `+SourceColumns+` from architecture_sources where project_key = $1`, project))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ArchitectureSource{}, fmt.Errorf("%w for %s", ErrNoSource, project)
		}
		return model.ArchitectureSource{}, fmt.Errorf("read architecture source: %w", err)
	}
	return source, nil
}

// resolve splits the stored repository and resolves the installation token
// (the stored id is a cache: PR A's contract).
func (i *Importer) resolve(ctx context.Context, source *model.ArchitectureSource) (owner, name, token string, err error) {
	owner, name, err = splitRepo(source.Repo)
	if err != nil {
		return "", "", "", err
	}
	installation, err := i.github.Installation(ctx, owner, name)
	if err != nil {
		return "", "", "", err
	}
	if installation.ID != source.InstallationID {
		if _, err := i.store.Pool.Exec(ctx,
			`update architecture_sources set installation_id = $2 where project_key = $1`,
			source.Project, installation.ID); err != nil {
			return "", "", "", fmt.Errorf("record re-resolved installation: %w", err)
		}
		source.InstallationID = installation.ID
	}
	token, err = i.github.Token(ctx, installation.ID)
	if err != nil {
		return "", "", "", err
	}
	return owner, name, token, nil
}

// lockSourceRow opens a writing transaction the cross-process way: it takes
// the source's row lock (`for update`), so two server processes projecting or
// recording on one project serialize on the row instead of deadlocking inside
// delete+reinsert, and re-reads repo/branch under the lock. moved reports
// that the source changed since this sync's fetch began — the caller must
// abort without writing, because its data describes the old configuration.
func lockSourceRow(ctx context.Context, tx pgx.Tx, source model.ArchitectureSource) (locked lockedSource, moved bool, err error) {
	var repo, branch string
	if err := tx.QueryRow(ctx,
		`select repo, branch, last_commit, last_error from architecture_sources where project_key = $1 for update`,
		source.Project).Scan(&repo, &branch, &locked.lastCommit, &locked.lastError); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return lockedSource{}, true, nil
		}
		return lockedSource{}, false, fmt.Errorf("lock architecture source: %w", err)
	}
	return locked, repo != source.Repo || branch != source.Branch, nil
}

// lockedSource is the sync state read under the row lock. The state-change
// decision (emit an event or not) is made from it, never from the row loaded
// before the fetch: another process may have recorded this very commit or
// this very failure in between, and the second writer must then stay silent.
type lockedSource struct {
	lastCommit *string
	lastError  *string
}

// abortMoved rolls the aborted write back and answers with the row as it now
// stands plus errSourceMoved. A source deleted mid-sync is ErrNoSource.
func (i *Importer) abortMoved(ctx context.Context, tx pgx.Tx, project string) (model.ArchitectureSource, error) {
	_ = tx.Rollback(ctx)
	updated, err := i.loadSource(ctx, project)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	return updated, errSourceMoved
}

// project writes the snapshot, replaces the project's components and their
// dependency edges, and updates the source row — one transaction, so a reader
// never sees a half-projected model.
func (i *Importer) project(
	ctx context.Context,
	source model.ArchitectureSource,
	commit string,
	treeSHA string,
	files map[string][]byte,
	parsed Model,
) (model.ArchitectureSource, error) {
	encoded, err := json.Marshal(snapshotFiles(files))
	if err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("encode architecture snapshot: %w", err)
	}
	tx, err := i.store.Pool.Begin(ctx)
	if err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("begin architecture import: %w", err)
	}
	defer tx.Rollback(ctx)
	locked, moved, err := lockSourceRow(ctx, tx, source)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	if moved {
		return i.abortMoved(ctx, tx, source.Project)
	}

	// The snapshot is immutable per commit: a re-import of the same commit
	// (the duplicate key) keeps the stored files and reuses the row, so the
	// only visible effect is a refreshed last_sync_at.
	var snapshotID int64
	err = tx.QueryRow(ctx, `
		insert into architecture_snapshots (project_key, commit, files)
		values ($1, $2, $3)
		on conflict (project_key, commit) do nothing
		returning id
	`, source.Project, commit, encoded).Scan(&snapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`select id from architecture_snapshots where project_key = $1 and commit = $2`,
			source.Project, commit).Scan(&snapshotID)
	}
	if err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("record architecture snapshot: %w", err)
	}

	if _, err := tx.Exec(ctx, `delete from components where project_key = $1`, source.Project); err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("retire previous components: %w", err)
	}
	// Both projections travel as one batch per table: the per-row INSERTs are unchanged, so
	// a failure still names the component or edge that caused it.
	components := &pgx.Batch{}
	for _, component := range parsed.Components {
		var parent *string
		if component.Parent != "" {
			parent = &component.Parent
		}
		paths := component.Paths
		if paths == nil {
			paths = []string{}
		}
		components.Queue(`
			insert into components (project_key, id, title, prose, parent, external, paths, snapshot_id)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
		`, source.Project, component.ID, component.Title, component.Prose, parent, component.External, paths, snapshotID)
	}
	if err := execBatch(ctx, tx, components, func(index int) string {
		return "project component " + parsed.Components[index].ID
	}); err != nil {
		return model.ArchitectureSource{}, err
	}
	dependencies := &pgx.Batch{}
	var edges [][2]string
	for _, component := range parsed.Components {
		for _, dependency := range component.DependsOn {
			dependencies.Queue(`
				insert into component_depends (project_key, from_id, to_id)
				values ($1, $2, $3)
				on conflict do nothing
			`, source.Project, component.ID, dependency)
			edges = append(edges, [2]string{component.ID, dependency})
		}
	}
	if err := execBatch(ctx, tx, dependencies, func(index int) string {
		return "project dependency " + edges[index][0] + " -> " + edges[index][1]
	}); err != nil {
		return model.ArchitectureSource{}, err
	}

	updated, err := ScanSource(tx.QueryRow(ctx, `
		update architecture_sources
		set last_sync_at = now(), last_commit = $2, last_error = null, last_tree_sha = $3
		where project_key = $1
		returning `+SourceColumns, source.Project, commit, treeSHA))
	if err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("record architecture sync: %w", err)
	}

	// State change only: a new commit, or a source recovering from a recorded
	// failure, judged from the row as it was under the lock. A same-commit
	// re-import of a healthy source is silent — including the second of two
	// processes that both fetched the same head.
	changed := locked.lastCommit == nil || *locked.lastCommit != commit || locked.lastError != nil
	var event model.Event
	if changed {
		event, err = i.events.Append(ctx, tx, model.Event{
			ProjectKey: &source.Project,
			Type:       "architecture.synced",
			Actor:      SystemActor,
			Payload:    map[string]any{"commit": commit, "components": len(parsed.Components)},
		})
		if err != nil {
			return model.ArchitectureSource{}, fmt.Errorf("append architecture.synced: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("commit architecture import: %w", err)
	}
	if changed {
		i.events.Publish(event)
	}
	return updated, nil
}

// execBatch sends the queued statements in one round-trip and reads each result in queue
// order, so the first failure is reported as "<label(index)>: <error>" for the statement
// that raised it.
func execBatch(ctx context.Context, tx pgx.Tx, batch *pgx.Batch, label func(index int) string) error {
	if batch.Len() == 0 {
		return nil
	}
	results := tx.SendBatch(ctx, batch)
	for index := range batch.Len() {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("%s: %w", label(index), err)
		}
	}
	return results.Close()
}

// recordFailure keeps the previous projection and records why this import did
// not replace it, capped so a pathological model cannot store tens of
// kilobytes per event. The event fires only when the error text first appears
// or changes, so a source that has been broken for a day emits one event, not
// one every five minutes.
func (i *Importer) recordFailure(ctx context.Context, source model.ArchitectureSource, failure error) (model.ArchitectureSource, error) {
	message := capFailureMessage(failure)
	tx, err := i.store.Pool.Begin(ctx)
	if err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("begin architecture failure record: %w", err)
	}
	defer tx.Rollback(ctx)
	locked, moved, err := lockSourceRow(ctx, tx, source)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	if moved {
		// The failure describes a configuration that no longer exists.
		return i.abortMoved(ctx, tx, source.Project)
	}

	updated, err := ScanSource(tx.QueryRow(ctx, `
		update architecture_sources
		set last_sync_at = now(), last_error = $2
		where project_key = $1
		returning `+SourceColumns, source.Project, message))
	if err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("record architecture sync failure: %w", err)
	}
	changed := locked.lastError == nil || *locked.lastError != message
	var event model.Event
	if changed {
		event, err = i.events.Append(ctx, tx, model.Event{
			ProjectKey: &source.Project,
			Type:       "architecture.sync_failed",
			Actor:      SystemActor,
			Payload:    map[string]any{"error": message},
		})
		if err != nil {
			return model.ArchitectureSource{}, fmt.Errorf("append architecture.sync_failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("commit architecture failure record: %w", err)
	}
	if changed {
		i.events.Publish(event)
	}
	return updated, nil
}

// maxFailureMessage bounds what one failure writes: last_error, the
// sync_failed payload (and its SSE frame), and thus the Settings row all
// carry the same text, so a model with thousands of problems records the
// first ones and a count of the rest.
const maxFailureMessage = 4 << 10

func capFailureMessage(failure error) string {
	message := failure.Error()
	if len(message) <= maxFailureMessage {
		return message
	}
	lines := strings.Split(message, "\n")
	// Room for the "… and K more problems" suffix.
	budget := maxFailureMessage - 64
	kept, used := 0, 0
	for _, line := range lines {
		if used+len(line)+1 > budget {
			break
		}
		used += len(line) + 1
		kept++
	}
	if kept == 0 {
		// One enormous problem: cut it at the budget, on a rune boundary so
		// the text column stays valid UTF-8.
		cut := lines[0][:budget]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		return fmt.Sprintf("%s… (truncated) and %d more problems", cut, len(lines)-1)
	}
	return fmt.Sprintf("%s\n… and %d more problems", strings.Join(lines[:kept], "\n"), len(lines)-kept)
}

// IsAccessFailure reports whether a sync failure is credential- or
// configuration-shaped: something a human fixes in GitHub or Settings, rather
// than an upstream outage or a malformed model.
func IsAccessFailure(err error) bool {
	return errors.Is(err, githubapp.ErrNoAppKey) ||
		errors.Is(err, githubapp.ErrNoInstallation) ||
		errors.Is(err, githubapp.ErrNoContentsRead) ||
		errors.Is(err, githubapp.ErrNoBranch)
}

// snapshotFiles renders the fetched files as the snapshot's jsonb value: file
// name to verbatim content.
func snapshotFiles(files map[string][]byte) map[string]string {
	stored := make(map[string]string, len(files))
	for name, content := range files {
		stored[name] = string(content)
	}
	return stored
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("stored repository %q is not owner/name", repo)
	}
	return owner, name, nil
}

// ScanSource decodes one SourceColumns row into a model.ArchitectureSource.
func ScanSource(row pgx.Row) (model.ArchitectureSource, error) {
	var source model.ArchitectureSource
	var createdBy []byte
	if err := row.Scan(
		&source.Project, &source.Repo, &source.Branch, &source.Enabled, &source.InstallationID,
		&createdBy, &source.CreatedAt, &source.LastSyncAt, &source.LastCommit, &source.LastError,
		&source.LastTreeSha,
	); err != nil {
		return model.ArchitectureSource{}, err
	}
	if err := json.Unmarshal(createdBy, &source.CreatedBy); err != nil {
		return model.ArchitectureSource{}, fmt.Errorf("decode architecture source author: %w", err)
	}
	return source, nil
}

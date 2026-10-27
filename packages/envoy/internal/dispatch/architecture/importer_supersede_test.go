package architecture

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
)

// reSaveSource runs the settings PUT's own upsert (api/architecture_source.go's
// putArchitectureSource) for CORE's legion/arch source on its unchanged branch: the person saves
// the same repository and branch again, which clears last_sync_at, last_commit, last_tree_sha and
// last_error.
func reSaveSource(t *testing.T, process *Importer) {
	t.Helper()
	if _, err := process.store.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
		values ($1, $2, $3, true, $4, $5)
		on conflict (project_key, repo) do update set
			branch = excluded.branch,
			enabled = true,
			installation_id = excluded.installation_id,
			created_by = excluded.created_by,
			last_sync_at = null,
			last_commit = null,
			last_tree_sha = null,
			last_error = null
	`, "CORE", archRepo, "main", int64(11), `{"kind":"user","id":"alice"}`); err != nil {
		t.Fatalf("re-save the source as the settings PUT does: %v", err)
	}
}

// A process loads a freshly saved source (last_sync_at null) and is held in its blob reads at the
// older head; another process imports the newer head; the person then re-saves the same repository
// and branch, which sets last_sync_at back to null. Once released, the held process must not
// record the older head it read before both of those writes: the row, the components and the
// events must stay at the newer head.
func TestASettingsReSaveDuringTwoSyncsNeverLeavesTheEarlierHead(t *testing.T) {
	branch := newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles(), commitTwo: infraFiles()})
	branch.holdBlobs(commitOne)
	a, b, database := twoProcesses(t, branch)

	held := syncInBackground(a)
	branch.waitHeld(t)
	branch.moveHead(commitTwo)
	if _, err := b.Sync(context.Background(), "CORE", archRepo); err != nil {
		t.Fatalf("the second process's sync: %v", err)
	}
	reSaveSource(t, b)
	branch.releaseHeld()

	var answer syncAnswer
	select {
	case answer = <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the held sync did not end within 30s of its release")
	}
	if answer.err != nil {
		t.Fatalf("the held sync: %v", answer.err)
	}

	var lastCommit *string
	if err := database.Pool.QueryRow(context.Background(),
		`select last_commit from architecture_sources where project_key = 'CORE'`).Scan(&lastCommit); err != nil {
		t.Fatalf("read the source row: %v", err)
	}
	if lastCommit == nil || *lastCommit != commitTwo {
		got := "<nil>"
		if lastCommit != nil {
			got = *lastCommit
		}
		t.Errorf("the row records commit %s, want %s: the held sync's compare-and-set on last_sync_at "+
			"passed null = null after the re-save (ABA) and recorded the older head it read before "+
			"another process imported %s", got, commitTwo, commitTwo)
	}
	if got := sourceComponents(t, database); !reflect.DeepEqual(got, infraComponents) {
		t.Errorf("components = %#v\nwant the newer head's %#v: the held sync projected the older head over it", got, infraComponents)
	}
	got := projectEvents(t, database)
	newer := slices.Index(got, "architecture.synced "+commitTwo)
	if newer < 0 {
		t.Fatalf("events = %#v, want an architecture.synced for %s", got, commitTwo)
	}
	if slices.Contains(got[newer+1:], "architecture.synced "+commitOne) {
		t.Errorf("events = %#v: architecture.synced for the older head %s was emitted after the newer head %s's",
			got, commitOne, commitTwo)
	}
}

// A source whose every sync is held by a GitHub rate limit recorded in github_rate_limits fails
// the same way each time; the importer's rule is that the same failure over and over emits one
// architecture.sync_failed event, not one per sync. The second sync runs after a minute has passed
// on the database's clock (the recorded limit's end moved one minute closer), as a later tick does.
func TestAHeldGitHubRateLimitEmitsOneSyncFailedEvent(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	_, database := newImporterFixture(t, fake)

	limits := ratelimit.New(database.Pool)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.test", PEM: pemText}, server.URL, githubapp.WithLimits(limits))
	if err != nil {
		t.Fatalf("new github client: %v", err)
	}
	importer := NewImporter(database, client, events.NewBroker())

	if err := limits.Record(ctx, githubapp.AppScope, time.Hour); err != nil {
		t.Fatalf("record the App's rate limit: %v", err)
	}

	first, err := importer.Sync(ctx, "CORE", archRepo)
	var limited *githubapp.RateLimitError
	if !errors.As(err, &limited) || !limited.Held {
		t.Fatalf("first sync: err = %v, want the held rate limit", err)
	}
	if _, err := database.Pool.Exec(ctx, `update github_rate_limits set until = until - interval '1 minute'`); err != nil {
		t.Fatalf("pass a minute on the recorded limit: %v", err)
	}
	second, err := importer.Sync(ctx, "CORE", archRepo)
	if !errors.As(err, &limited) || !limited.Held {
		t.Fatalf("second sync: err = %v, want the held rate limit", err)
	}

	var failed []string
	for _, event := range projectEvents(t, database) {
		if strings.HasPrefix(event, "architecture.sync_failed ") {
			failed = append(failed, event)
		}
	}
	if len(failed) != 1 {
		text := func(value *string) string {
			if value == nil {
				return "<nil>"
			}
			return *value
		}
		t.Errorf("one held rate limit over two syncs emitted %d architecture.sync_failed events, want 1: "+
			"the failure text carries the limit's remaining wait, so every sync records a \"changed\" error "+
			"(last_error %q then %q)\nevents: %#v", len(failed), text(first.LastError), text(second.LastError), failed)
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// replayedMappings is the set of repositories the project's settings.repo_project.updated events
// leave in it, oldest first; tableMappings is the set its rows hold. A reader that follows the
// events sees what the table holds only when the two agree.
func replayedMappings(t *testing.T, database *store.Store, project string) map[string]bool {
	t.Helper()
	repos := map[string]bool{}
	for _, payload := range projectEvents(t, database, project, "settings.repo_project.updated") {
		repo := payload["mapping"].(map[string]any)["repo"].(string)
		if payload["deleted"] == true {
			delete(repos, repo)
		} else {
			repos[repo] = true
		}
	}
	return repos
}

func tableMappings(t *testing.T, database *store.Store, project string) map[string]bool {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select repo from repo_projects where project = $1`, project)
	if err != nil {
		t.Fatalf("list %s's mappings: %v", project, err)
	}
	defer rows.Close()
	repos := map[string]bool{}
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			t.Fatalf("scan %s's mapping: %v", project, err)
		}
		repos[repo] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s's mappings: %v", project, err)
	}
	return repos
}

func requireEventsMatchTable(t *testing.T, database *store.Store, projects ...string) {
	t.Helper()
	for _, project := range projects {
		replayed, table := replayedMappings(t, database, project), tableMappings(t, database, project)
		if len(replayed) != len(table) {
			t.Fatalf("%s's events leave %v, its rows hold %v", project, replayed, table)
		}
		for repo := range table {
			if !replayed[repo] {
				t.Fatalf("%s's events leave %v, its rows hold %v", project, replayed, table)
			}
		}
	}
}

// A mapping the seed adds is announced on its project, as a PUT's is; a repository that already
// has a mapping, in its seeded project or another, is left alone with no event; and a later move
// tells the seeded project the mapping left.
func TestSeedRepoProjectsAnnouncesWhatItAddsAndNothingElse(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createProjects(t, handler, "CORE", "OTHER")
	ctx := context.Background()
	seed := func() {
		t.Helper()
		if err := SeedRepoProjects(ctx, database, events.NewBroker(), "acme/new=CORE"); err != nil {
			t.Fatalf("seed acme/new=CORE: %v", err)
		}
	}
	seed()
	core := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if len(core) != 1 || core[0]["deleted"] != false || core[0]["mapping"].(map[string]any)["repo"] != "acme/new" {
		t.Fatalf("CORE's mapping events after the seed = %v, want acme/new added", core)
	}
	var kind, id string
	if err := database.Pool.QueryRow(ctx, `
		select actor->>'kind', actor->>'id' from events where project_key = 'CORE' and type = 'settings.repo_project.updated'
	`).Scan(&kind, &id); err != nil || kind != "session" || id != "environment" {
		t.Fatalf("the seed's event actor = %s %s (%v), want the environment session", kind, id, err)
	}
	seed()
	if got := len(projectEvents(t, database, "CORE", "settings.repo_project.updated")); got != 1 {
		t.Fatalf("CORE saw %d mapping events after seeding acme/new again, want still 1", got)
	}
	if moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/new", map[string]any{"project": "OTHER"}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("move acme/new to OTHER: status=%d body=%s", moved.Code, moved.Body.String())
	}
	seed()
	if project := tableMappings(t, database, "OTHER"); !project["acme/new"] {
		t.Fatalf("OTHER's mappings after seeding acme/new=CORE over the dashboard's move = %v, want acme/new kept there", project)
	}
	requireEventsMatchTable(t, database, "CORE", "OTHER")
}

// The seed takes a mapping write's locks: one racing a PUT of the same repository waits for the
// PUT, then finds the mapping the PUT made and leaves it. It never adds the mapping to a project
// the PUT then moves it out of, so no project holds it without its events saying so.
func TestSeedRepoProjectsRacingAPutOfTheSameRepositoryWaitsForIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createProjects(t, handler, "CORE", "OTHER")
	ctx := context.Background()
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/other", map[string]any{"project": "OTHER", "default": true}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map acme/other as OTHER's default: status=%d body=%s", response.Code, response.Body.String())
	}
	// Holding OTHER's default row stops the PUT below at its demotion of that default: after it
	// has read that acme/new has no mapping, and before it writes one.
	release := holdLock(t, database, `select repo from repo_projects where repo = 'acme/other' for update`)
	put := putMappingAsync(t, handler, "acme/new", map[string]any{"project": "OTHER", "default": true})
	awaitLockWaiters(t, database, 1)
	seeded := make(chan error, 1)
	go func() { seeded <- SeedRepoProjects(ctx, database, events.NewBroker(), "acme/new=CORE") }()
	deadline := time.Now().Add(10 * time.Second)
	for lockWaiters(t, database) < 2 {
		select {
		case err := <-seeded:
			var project string
			_ = database.Pool.QueryRow(ctx, `select project from repo_projects where repo = 'acme/new'`).Scan(&project)
			release()
			awaitAnswer(t, put, "map acme/new to OTHER")
			t.Fatalf("the seed returned (err %v) while a PUT of acme/new was in flight, having put acme/new in %q; want it to wait for the PUT", err, project)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seed neither returned nor waited on a lock after 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	if response := awaitAnswer(t, put, "map acme/new to OTHER"); response.Code != http.StatusOK {
		t.Fatalf("map acme/new to OTHER: status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case err := <-seeded:
		if err != nil {
			t.Fatalf("seed acme/new=CORE after the PUT: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the seed never returned after the PUT committed")
	}
	if other := tableMappings(t, database, "OTHER"); !other["acme/new"] {
		t.Fatalf("OTHER's mappings = %v, want the PUT's acme/new kept", other)
	}
	requireEventsMatchTable(t, database, "CORE", "OTHER")
}

func TestSeedRepoProjectsAddsMissingRowsWithoutOverwritingSettings(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for _, project := range []string{"APP", "CORE"} {
		if _, err := database.Pool.Exec(ctx, "insert into projects (key, name) values ($1, $1)", project); err != nil {
			t.Fatalf("create project %q: %v", project, err)
		}
	}
	createdBy, err := json.Marshal(map[string]string{"kind": "user", "id": "alice"})
	if err != nil {
		t.Fatalf("encode actor: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by) values ('dashboard/repo', 'CORE', $1)
	`, createdBy); err != nil {
		t.Fatalf("create dashboard mapping: %v", err)
	}

	if err := SeedRepoProjects(ctx, database, events.NewBroker(), "Seed/Repo.git=APP,dashboard/repo=APP"); err != nil {
		t.Fatalf("seed repository projects: %v", err)
	}

	rows, err := database.Pool.Query(ctx, "select repo, project from repo_projects order by repo")
	if err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var repo, project string
		if err := rows.Scan(&repo, &project); err != nil {
			t.Fatalf("scan mapping: %v", err)
		}
		got[repo] = project
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate mappings: %v", err)
	}
	if got["seed/repo"] != "APP" || got["dashboard/repo"] != "CORE" || len(got) != 2 {
		t.Fatalf("seeded mappings: got %#v", got)
	}
}

func TestSeedRepoProjectsRejectsMissingProject(t *testing.T) {
	database := storetest.Open(t)
	err := SeedRepoProjects(context.Background(), database, events.NewBroker(), "missing/repo=MISSING")
	if err == nil || !strings.Contains(err.Error(), `seed repository project "missing/repo": project MISSING does not exist`) {
		t.Fatalf("seed missing project: got %v", err)
	}
}

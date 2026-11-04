package settings

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// testProject is the project every row these tests write is filed under.
const testProject = "DEMO"

// minimalFile is the smallest configuration that loads, as internal/config's own tests write
// one. It names neither `admission_cap` nor `max_fix_attempts`, and settles their defaults (4
// and 3): a test that gives the file its own value for one of them appends that key, since a
// file carrying one key twice is a file no operator writes.
const minimalFile = `project: DEMO
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
`

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// configFile writes body to a legion.yaml of the test's own and answers its path.
func configFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	rewrite(t, path, body)
	return path
}

func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// testStore is a migrated database of the test's own: the keeper is held to what a real store
// records, through the daemon's own schema.
func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(context.Background(), testpg.Database(t, "legion_settings_test"))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	return NewStore(st.Pool(), testProject)
}

// keeperOn is a keeper over path with its own holder, as a boot builds one.
func keeperOn(t *testing.T, path string, store *Store) *Keeper {
	t.Helper()
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return New(live, store, quiet())
}

// A route change is stored, and the next boot lays it back over the file: the daemon a restart
// brings up is running on what the person asked for, with their name and the moment of the
// change (acceptance A5, "a route change survives a restart").
func TestARouteChangeIsLaidBackOverTheFileAtTheNextBoot(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)

	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7})
	if err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "7" {
		t.Fatalf("Route applied %+v, want admission_cap 7", applied)
	}

	restarted := keeperOn(t, path, store)
	if cap := restarted.Live().Snapshot().AdmissionCap; cap != 4 {
		t.Fatalf("the file alone reads admission_cap %d, want the file's own 4 before the restore", cap)
	}
	kept, err := restarted.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if cap := restarted.Live().Snapshot().AdmissionCap; cap != 7 {
		t.Fatalf("admission_cap after the restore = %d, want the route's 7", cap)
	}
	if len(kept) != 1 || kept[0].Path != "admission_cap" || kept[0].By != "ana@example.com" ||
		kept[0].Source != config.SourceRoute || kept[0].At.IsZero() {
		t.Fatalf("Restore kept %+v, want ana's admission_cap change with its own time", kept)
	}
}

// A secret path is applied and never stored: the value — a credential, the path of a file
// holding one, or an address that may carry one — stays out of the database every project's
// daemon shares, where a plain value of the same call is kept. The row of such a path goes with
// it, so a restart reads the file's own value for it.
func TestACredentialChangedThroughTheRouteIsAppliedAndNotStored(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	keeper := keeperOn(t, configFile(t, minimalFile), store)

	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{
		"operator_token_file": "/etc/legion/operator-token",
		"admission_cap":       7,
	})
	if err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}

	paths := map[string]bool{}
	for _, change := range applied {
		paths[change.Path] = true
	}
	if !paths["operator_token_file"] || !paths["admission_cap"] {
		t.Fatalf("the route applied %+v, want both paths", applied)
	}
	if file := keeper.Live().Snapshot().OperatorTokenFile; file != "/etc/legion/operator-token" {
		t.Fatalf("the daemon reads operator_token_file %q, want the one the route set", file)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "admission_cap" {
		t.Fatalf("the store holds %+v, want the plain value alone and no credential", rows)
	}
}

// The file takes its key back: a change of that key in the file itself, while the daemon was
// down, outlives the route change above it, and the row goes with it. A route change of another
// key stands (acceptance A5, "until the file changes that key").
func TestTheFileTakesBackOnlyTheKeysItChanged(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7, "max_fix_attempts": 5}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rewrite(t, path, minimalFile+"admission_cap: 9\n")
	restarted := keeperOn(t, path, store)
	kept, err := restarted.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	cfg := restarted.Live().Snapshot()
	if cfg.AdmissionCap != 9 {
		t.Errorf("admission_cap = %d, want the file's own 9: the file changed that key", cfg.AdmissionCap)
	}
	if cfg.MaxFixAttempts != 5 {
		t.Errorf("max_fix_attempts = %d, want the route's 5: the file changed no such key", cfg.MaxFixAttempts)
	}
	if len(kept) != 1 || kept[0].Path != "max_fix_attempts" {
		t.Fatalf("Restore kept %+v, want the max_fix_attempts change alone", kept)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "max_fix_attempts" {
		t.Fatalf("the store holds %+v, want the row the file did not take back", rows)
	}
}

// The same under a running daemon: the file arm settles the file as it now reads, keeps the
// route changes of keys the file did not touch, and drops the ones it did.
func TestTheFileArmTakesBackTheKeysTheFileChangedUnderTheRunningDaemon(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7, "max_fix_attempts": 5}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rewrite(t, path, minimalFile+"admission_cap: 9\nreview_round_cap: 2\n")
	applied, err := keeper.File(ctx)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	cfg := keeper.Live().Snapshot()
	if cfg.AdmissionCap != 9 || cfg.MaxFixAttempts != 5 || cfg.ReviewRoundCap != 2 {
		t.Fatalf("the file arm settled cap %d, fixes %d, rounds %d; want the file's 9, the route's 5 and the file's 2",
			cfg.AdmissionCap, cfg.MaxFixAttempts, cfg.ReviewRoundCap)
	}
	var paths []string
	for _, change := range applied {
		paths = append(paths, change.Path)
		if change.Source != config.SourceFile {
			t.Errorf("%s was applied as %q, want the file's own source", change.Path, change.Source)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("the file arm applied %v, want admission_cap and review_round_cap", paths)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "max_fix_attempts" {
		t.Fatalf("the store holds %+v, want the row the file did not take back", rows)
	}
}

// A path the route withdraws is dropped from the overlay and from the store: the file's own
// value takes the key back at once.
func TestARouteWithdrawalGivesTheKeyBackToTheFile(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	keeper := keeperOn(t, configFile(t, minimalFile), store)
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	applied, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": nil})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(applied) != 1 || applied[0].New != "4" {
		t.Fatalf("the withdrawal applied %+v, want admission_cap back at the file's 4", applied)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the store holds %+v after the withdrawal, want nothing", rows)
	}
}

// A path fixed for the process is refused by name, with the reason its row gives, and nothing is
// stored for it (acceptance A5, "a key fixed for the process is refused by name").
func TestAFixedPathIsRefusedAndStoredNowhere(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	keeper := keeperOn(t, configFile(t, minimalFile), store)

	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"bind": "0.0.0.0", "admission_cap": 6})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(refused) != 1 || refused[0].Path != "bind" || refused[0].Reason == "" {
		t.Fatalf("Route refused %+v, want bind by name with its reason", refused)
	}
	if len(applied) != 1 || applied[0].Path != "admission_cap" {
		t.Fatalf("Route applied %+v, want the one path it did not refuse", applied)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "admission_cap" {
		t.Fatalf("the store holds %+v, want the applied path alone", rows)
	}
}

// A store with no settings table — a database this release has not migrated yet — carries no
// route change, and the boot reads it as none rather than refusing to start. The boot reads the
// rows before it migrates, which is the one moment that table can be missing.
func TestABootBeforeTheMigrationReadsNoStoredChange(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testpg.Database(t, "legion_settings_unmigrated"))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(st.Close)

	keeper := keeperOn(t, configFile(t, minimalFile), NewStore(st.Pool(), testProject))
	kept, err := keeper.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore against an unmigrated database: %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("Restore kept %+v, want nothing", kept)
	}
}

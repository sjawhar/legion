package tmux

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// staticCredentialExpiry is the lease expiry every rendered test credential carries.
var staticCredentialExpiry = time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)

// staticCredentialToken is the token staticCredential renders for role: one per App, so a review
// role's files and an implement role's differ as the two Apps' tokens do.
func staticCredentialToken(role claim.Role) string {
	return "ghs_" + string(appauth.AppRoleFor(role)) + "_token"
}

// staticCredential is the runtime's GitHubCredential in every test that needs one: the role's
// App's token (appauth.AppRoleFor) rendered as gh's files, with a fixed expiry.
func staticCredential(_ context.Context, role claim.Role) (ghconfig.Rendered, error) {
	app := appauth.AppRoleFor(role)
	return ghconfig.Render(staticCredentialToken(role), string(app), staticCredentialExpiry), nil
}

// fakeServerRuntime is a runtime built by New over a tmux that is never run: its PATH names a stub
// tmux so New resolves one, and run and readProc are replaced by a server that answers every
// launch command as a fresh private server would — no session, then one window, pane %1 with pid
// 4242 under the server's pid 1 — and a /proc in which that pane has exec'd its shell. It records
// every argv, so a test reads what a launch told tmux.
type fakeServerRuntime struct {
	*Runtime
	argvs [][]string
}

func newFakeServerRuntime(t *testing.T, credential runtime.GitHubCredential, log *slog.Logger) *fakeServerRuntime {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", OmpInvocation: "omp", StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil }, GitHubCredential: credential, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServerRuntime{Runtime: rt}
	rt.run = func(_ context.Context, argv []string) (result, error) {
		f.argvs = append(f.argvs, argv)
		switch verb(argv) {
		case "has-session":
			return result{exitCode: 1, stderr: "no server running on /tmp/tmux/legion-omp"}, nil
		case "new-window":
			return result{stdout: "@1 %1 4242\n"}, nil
		}
		return result{}, nil
	}
	rt.readProc = func(path string) ([]byte, error) {
		switch path {
		case "/proc/4242/stat":
			return []byte("4242 (sh) " + statTail), nil
		case "/proc/1/stat":
			return []byte("1 (tmux: server) " + statTail), nil
		}
		return nil, os.ErrNotExist
	}
	return f
}

// spec is testSpec with a role prompt that exists, as a launch requires.
func (f *fakeServerRuntime) spec(t *testing.T, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the stand-in.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	token, err := claim.NewToken(spec.Project, spec.Issue, role)
	if err != nil {
		t.Fatal(err)
	}
	spec.Claim, spec.Role = token, role
	spec.Prompt.RolePromptPaths = []string{prompt}
	return spec
}

// pairs are the -e pairs of the one new-window the fake server answered, NAME=value.
func (f *fakeServerRuntime) pairs(t *testing.T) []string {
	t.Helper()
	for _, argv := range f.argvs {
		if verb(argv) != "new-window" {
			continue
		}
		var pairs []string
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "-e" {
				pairs = append(pairs, argv[i+1])
			}
		}
		return pairs
	}
	t.Fatal("no new-window reached the fake server")
	return nil
}

// readHostsToken is the github.com token the hosts.yml under dir holds.
func readHostsToken(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ghconfig.HostsFile))
	if err != nil {
		t.Fatalf("hosts.yml under %s: %v", dir, err)
	}
	token, err := ghconfig.TokenFromHosts(data)
	if err != nil {
		t.Fatalf("hosts.yml under %s: %v", dir, err)
	}
	return token
}

// ghDirEntries are the names under a claim's gh directory: the two files, and never a temporary
// file a replaced hosts.yml left behind.
func ghDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// logLines are the log's lines holding what.
func logLines(logs string, what string) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, what) {
			lines = append(lines, line)
		}
	}
	return lines
}

// A daemon with no GitHub Apps (Stage 2's, which configures no workflow) hands the runtime no
// credential function, and its panes still boot: New accepts the nil, a spawn writes no gh
// directory under the claim's secrets and tells the pane none of the four gh variables, logs no
// written credential, and the refresher — started by Observe for every runtime — does nothing.
func TestARuntimeWithoutACredentialFunctionSpawnsPanesWithNoGhFiles(t *testing.T) {
	var logs strings.Builder
	f := newFakeServerRuntime(t, nil, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	spec := f.spec(t, claim.RoleTester)

	loc, err := f.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if loc.Claim != spec.Claim || loc.Incarnation != "4242:1234567" {
		t.Fatalf("Spawn returned %+v", loc)
	}
	if _, err := os.Stat(runtime.GHConfigDir(f.stateDir, spec.Claim)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the launch made a gh directory with no credential to put in it (%v)", err)
	}
	for _, pair := range f.pairs(t) {
		for _, name := range []string{"GH_CONFIG_DIR=", "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST="} {
			if strings.HasPrefix(pair, name) {
				t.Errorf("the pane was told %q with no credential function", pair)
			}
		}
	}
	if written := logLines(logs.String(), "github credential"); len(written) != 0 {
		t.Errorf("the runtime logged %q with no credential function", written)
	}
	// Spawn tracked the pane, so the refresher's walk reaches it and still writes nothing.
	f.refreshGitHubCredentials(context.Background())
	if _, err := os.Stat(runtime.GHConfigDir(f.stateDir, spec.Claim)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refresher made a gh directory with no credential to put in it (%v)", err)
	}
}

// A spawn writes the claim's gh files beside its other secret files, from the daemon's function
// for the pane's role: a 0700 directory `<state_dir>/secrets/<claim>-gh` holding a 0600 hosts.yml
// with the role's App token and a 0600 config.yml, named on the pane as GH_CONFIG_DIR with
// GH_TOKEN, GITHUB_TOKEN and GH_HOST emptied and no tool path, and logged with the claim, role, App
// and expiry. A mint that fails fails the launch before tmux is asked anything: a pane whose gh
// holds no token would start unable to reach GitHub.
func TestSpawnWritesTheClaimsGhFilesAndNamesTheDirectoryOnThePane(t *testing.T) {
	var logs strings.Builder
	f := newFakeServerRuntime(t, staticCredential, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	spec := f.spec(t, claim.RoleTester)

	loc, err := f.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if loc.Claim != spec.Claim || loc.Incarnation != "4242:1234567" {
		t.Fatalf("Spawn returned %+v", loc)
	}
	dir := runtime.GHConfigDir(f.stateDir, spec.Claim)
	if want := filepath.Join(f.stateDir, "secrets", string(spec.Claim)+"-gh"); dir != want {
		t.Fatalf("the claim's gh directory is %s, want %s", dir, want)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("gh directory stat = %v, %v; want a 0700 directory", info, err)
	}
	for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s stat = %v, %v; want mode 0600", name, info, err)
		}
	}
	if token := readHostsToken(t, dir); token != staticCredentialToken(claim.RoleTester) {
		t.Errorf("hosts.yml holds %q, want the review App's %q", token, staticCredentialToken(claim.RoleTester))
	}
	if config, err := os.ReadFile(filepath.Join(dir, ghconfig.ConfigFile)); err != nil || string(config) != ghconfig.Config {
		t.Errorf("config.yml = %q (%v), want %q", config, err, ghconfig.Config)
	}
	if entries := ghDirEntries(t, dir); !slices.Equal(entries, []string{ghconfig.ConfigFile, ghconfig.HostsFile}) {
		t.Errorf("the gh directory holds %v, want the two files alone", entries)
	}
	pairs := f.pairs(t)
	for _, want := range []string{"GH_CONFIG_DIR=" + dir, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST="} {
		if !slices.Contains(pairs, want) {
			t.Errorf("the pane's pairs %q lack %q", pairs, want)
		}
	}
	for _, pair := range pairs {
		name, value, _ := strings.Cut(pair, "=")
		if strings.HasSuffix(name, "_PATH") {
			t.Errorf("the pane is told a tool path %q", pair)
		}
		if strings.Contains(value, staticCredentialToken(claim.RoleTester)) {
			t.Errorf("the pane's %s carries the token", name)
		}
	}
	written := logLines(logs.String(), "github credential written")
	if len(written) != 1 {
		t.Fatalf("the runtime logged %d written credentials %q, want one", len(written), written)
	}
	for _, want := range []string{"claim=" + string(spec.Claim), "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00"} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the runtime logged %q, want %q in it", written[0], want)
		}
	}

	// A mint that fails fails the launch, naming the claim, and nothing reaches tmux.
	refused := newFakeServerRuntime(t, func(context.Context, claim.Role) (ghconfig.Rendered, error) {
		return ghconfig.Rendered{}, errors.New("no installation token: GitHub is down")
	}, nil)
	spec = refused.spec(t, claim.RoleImplementer)
	_, err = refused.Spawn(context.Background(), spec)
	if want := "spawn " + string(spec.Claim) + ": write the github credential: no installation token: GitHub is down"; err == nil || err.Error() != want {
		t.Fatalf("Spawn with a failing mint = %v, want %q", err, want)
	}
	if len(refused.argvs) != 0 {
		t.Errorf("the refused launch ran tmux: %q", refused.argvs)
	}
	if _, err := os.Stat(runtime.GHConfigDir(refused.stateDir, spec.Claim)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused launch left a gh directory (%v)", err)
	}
}

// The refresher brings every tracked pane's hosts.yml to the current render of its role's
// credential and touches nothing else: a stale file is replaced whole, by rename, and logged with
// the claim, role, App and expiry; a file already at the render is not rewritten; a directory
// that is gone is made again; the controller's claim, which has no App, and a token that names no
// role are skipped. A mint that fails is logged and leaves the pane's last token in place until
// the next tick.
func TestTheRefresherRewritesOnlyTheGhFilesWhoseHostsAreStale(t *testing.T) {
	ctx := context.Background()
	var logs strings.Builder
	r := &Runtime{
		stateDir:         t.TempDir(),
		gitHubCredential: staticCredential,
		log:              slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		tracked:          map[string]*trackedProcess{},
	}
	token := func(issue string, role claim.Role) claim.Token {
		t.Helper()
		token, err := claim.NewToken("omp", issue, role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	stale, current, gone := token("LEGION-43", claim.RoleTester), token("LEGION-44", claim.RoleImplementer), token("LEGION-45", claim.RoleReviewer)
	controller, nameless := claim.ControllerToken("omp"), claim.Token("not-a-claim")
	for i, token := range []claim.Token{stale, current, gone, controller, nameless} {
		r.track(runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "4242:" + string(rune('1'+i)),
			Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%" + string(rune('1'+i))},
		}, "")
	}
	dir := func(token claim.Token) string { return runtime.GHConfigDir(r.stateDir, token) }
	if _, err := ghconfig.Write(dir(stale), ghconfig.Render("ghs_stale_lease", string(appauth.Review), staticCredentialExpiry)); err != nil {
		t.Fatal(err)
	}
	rendered, _ := staticCredential(ctx, claim.RoleImplementer)
	if _, err := ghconfig.Write(dir(current), rendered); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir(current), ghconfig.HostsFile), past, past); err != nil {
		t.Fatal(err)
	}

	r.refreshGitHubCredentials(ctx)

	if got := readHostsToken(t, dir(stale)); got != staticCredentialToken(claim.RoleTester) {
		t.Errorf("the stale pane's hosts.yml holds %q, want the review App's %q", got, staticCredentialToken(claim.RoleTester))
	}
	if info, err := os.Stat(filepath.Join(dir(stale), ghconfig.HostsFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the replaced hosts.yml stat = %v, %v; want mode 0600", info, err)
	}
	if info, err := os.Stat(filepath.Join(dir(current), ghconfig.HostsFile)); err != nil || !info.ModTime().Equal(past) {
		t.Errorf("the current pane's hosts.yml was rewritten (mtime %v, %v), want it left as it was", info.ModTime(), err)
	}
	if got := readHostsToken(t, dir(gone)); got != staticCredentialToken(claim.RoleReviewer) {
		t.Errorf("the recreated directory's hosts.yml holds %q, want %q", got, staticCredentialToken(claim.RoleReviewer))
	}
	if info, err := os.Stat(dir(gone)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the recreated directory stat = %v, %v; want mode 0700", info, err)
	}
	for _, token := range []claim.Token{controller, nameless} {
		if _, err := os.Stat(dir(token)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refresher made a gh directory for %s (%v)", token, err)
		}
	}
	for _, token := range []claim.Token{stale, current, gone} {
		if entries := ghDirEntries(t, dir(token)); !slices.Equal(entries, []string{ghconfig.ConfigFile, ghconfig.HostsFile}) {
			t.Errorf("%s's gh directory holds %v, want the two files alone", token, entries)
		}
	}
	refreshed := logLines(logs.String(), "github credential refreshed")
	if len(refreshed) != 2 {
		t.Fatalf("the runtime logged %d refreshed credentials %q, want the stale and the recreated panes'", len(refreshed), refreshed)
	}
	for _, want := range []string{"claim=" + string(stale), "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00"} {
		if !strings.Contains(refreshed[0], want) {
			t.Errorf("the runtime logged %q, want %q in it", refreshed[0], want)
		}
	}
	if !strings.Contains(refreshed[1], "claim="+string(gone)) {
		t.Errorf("the runtime logged %q, want the recreated pane's claim in it", refreshed[1])
	}
	if failed := logLines(logs.String(), "refresh failed"); len(failed) != 0 {
		t.Errorf("the runtime logged failures %q while every render succeeded", failed)
	}

	// A mint that fails leaves the pane's last token, logged; its siblings are still refreshed.
	logs.Reset()
	r.gitHubCredential = func(ctx context.Context, role claim.Role) (ghconfig.Rendered, error) {
		if role == claim.RoleTester {
			return ghconfig.Rendered{}, errors.New("no installation token: GitHub is down")
		}
		return ghconfig.Render("ghs_"+string(appauth.AppRoleFor(role))+"_second", string(appauth.AppRoleFor(role)), staticCredentialExpiry), nil
	}
	r.refreshGitHubCredentials(ctx)
	if got := readHostsToken(t, dir(stale)); got != staticCredentialToken(claim.RoleTester) {
		t.Errorf("after a failed mint the pane's hosts.yml holds %q, want its last token %q", got, staticCredentialToken(claim.RoleTester))
	}
	for token, role := range map[claim.Token]claim.Role{current: claim.RoleImplementer, gone: claim.RoleReviewer} {
		if got, want := readHostsToken(t, dir(token)), "ghs_"+string(appauth.AppRoleFor(role))+"_second"; got != want {
			t.Errorf("%s's hosts.yml holds %q, want the re-minted %q", token, got, want)
		}
	}
	failed := logLines(logs.String(), "github credential refresh failed")
	if len(failed) != 1 {
		t.Fatalf("the runtime logged %d failed refreshes %q, want the tester's alone", len(failed), failed)
	}
	for _, want := range []string{"claim=" + string(stale), "role=" + string(claim.RoleTester), "GitHub is down"} {
		if !strings.Contains(failed[0], want) {
			t.Errorf("the runtime logged %q, want %q in it", failed[0], want)
		}
	}
}

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
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// staticCredentialExpiry is the lease expiry every rendered test credential carries.
var staticCredentialExpiry = time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)

// staticCredentialToken is the write token staticCredential renders for role: one per App, so a
// review role's files and an implement role's differ as the two Apps' tokens do.
func staticCredentialToken(role claim.Role) string {
	return "ghs_" + string(appauth.AppRoleFor(role)) + "_token"
}

// staticReadToken is the read-only token staticCredential renders for role on owner.
func staticReadToken(role claim.Role, owner string) string {
	return "ghs_" + string(appauth.AppRoleFor(role)) + "_read_" + owner
}

// staticCredential is the runtime's GitHubCredential in every test that needs one: a bundle of
// the two owners acme and sjawhar (and repo's own, when it is another), the role's App's write
// token (appauth.AppRoleFor) in the directory of repo's owner — acme when repo is the zero
// Repository — and its read token in every other, rendered as gh's files with a fixed expiry.
func staticCredential(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
	app := string(appauth.AppRoleFor(role))
	owner := strings.ToLower(repo.Owner())
	if owner == "" {
		owner = "acme"
	}
	bundle := ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{}}
	for _, name := range []string{"acme", "sjawhar", owner} {
		bundle.Dirs[name] = ghconfig.Render(staticReadToken(role, name), app, staticCredentialExpiry)
	}
	bundle.Dirs[owner] = ghconfig.Render(staticCredentialToken(role), app, staticCredentialExpiry)
	return bundle, nil
}

// staticControllerCredential is the runtime's ControllerCredential in every test that needs one:
// the bundle staticCredential renders for the controller, whose default is the owner it is asked
// for and acme, the first owner, when it is asked for none, as appauth.ControllerCredential
// answers.
func staticControllerCredential(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
	bundle, err := staticCredential(ctx, claim.RoleController, ghrepo.Repository{})
	if err != nil || defaultOwner == "" {
		return bundle, err
	}
	bundle.Default = strings.ToLower(defaultOwner)
	return bundle, nil
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
	return newFakeServerRuntimeWith(t, credential, log, nil)
}

// newFakeServerRuntimeWith is newFakeServerRuntime over the Options change makes, when not nil.
func newFakeServerRuntimeWith(t *testing.T, credential runtime.GitHubCredential, log *slog.Logger, change func(*Options)) *fakeServerRuntime {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil }, GitHubCredential: credential, ControllerCredential: staticControllerCredential,
		GH: fakeGH, ClaimRepository: acmeWidgets, Log: log,
	}
	if change != nil {
		change(&opts)
	}
	rt, err := New(opts)
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

// acmeWidgets is the daemon's record of every test claim's repository: acme/widgets, as each
// test spec's is.
func acmeWidgets(context.Context, claim.Token) (ghrepo.Repository, error) {
	return ghrepo.MustParse("acme/widgets"), nil
}

// fakeGH is the real gh every fake server runtime is told, which a pane's LEGION_GH_PATH names.
const fakeGH = "/usr/bin/gh"

// spec is testSpec on acme/widgets, with the issue's workspace provisioned and a role prompt that
// exists, as a launch requires.
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
	spec.Repository, spec.DefaultBranch = ghrepo.MustParse("acme/widgets"), "main"
	located, err := workspace.Location(f.stateDir, spec.Repository, spec.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(located.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
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
// directory under the claim's secrets and tells the pane none of the six gh variables, logs no
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
		for _, name := range []string{"GH_CONFIG_DIR=", "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=", "LEGION_GH_OWNERS_DIR=", "LEGION_GH_PATH="} {
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

// A spawn writes the claim's gh bundle beside its other secret files, from the daemon's function
// for the pane's role and its repository's owner: a 0700 root `<state_dir>/secrets/<claim>-gh`
// holding one 0700 directory per owner — acme's with the role's App write token, sjawhar's with its
// read token, each a 0600 hosts.yml and config.yml — and the default owner's name; the pane is told
// GH_CONFIG_DIR, acme's directory, with GH_TOKEN, GITHUB_TOKEN and GH_HOST emptied,
// LEGION_GH_OWNERS_DIR, the root, and LEGION_GH_PATH, the real gh, and no other tool path; the log
// line names the claim, role, App, expiry and owners. A mint that fails fails the launch before
// tmux is asked anything: a pane whose gh holds no token would start unable to reach GitHub.
func TestSpawnWritesTheClaimsGhBundleAndNamesItOnThePane(t *testing.T) {
	var logs strings.Builder
	var asked []string
	credential := func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		asked = append(asked, repo.String())
		return staticCredential(ctx, role, repo)
	}
	f := newFakeServerRuntime(t, credential, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	spec := f.spec(t, claim.RoleTester)

	loc, err := f.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if loc.Claim != spec.Claim || loc.Incarnation != "4242:1234567" {
		t.Fatalf("Spawn returned %+v", loc)
	}
	if !slices.Equal(asked, []string{"acme/widgets"}) {
		t.Errorf("the launch asked the credential for %q, want its own repository, acme/widgets", asked)
	}
	root := runtime.GHConfigDir(f.stateDir, spec.Claim)
	if want := filepath.Join(f.stateDir, "secrets", string(spec.Claim)+"-gh"); root != want {
		t.Fatalf("the claim's gh root is %s, want %s", root, want)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("gh root stat = %v, %v; want a 0700 directory", info, err)
	}
	if entries := ghDirEntries(t, root); !slices.Equal(entries, []string{"acme", "sjawhar"}) {
		t.Errorf("the gh root holds %v, want one directory per owner and nothing else", entries)
	}
	for owner, token := range map[string]string{"acme": staticCredentialToken(claim.RoleTester), "sjawhar": staticReadToken(claim.RoleTester, "sjawhar")} {
		dir := ghconfig.Dir(root, owner)
		for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
			if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s's %s stat = %v, %v; want mode 0600", owner, name, info, err)
			}
		}
		if got := readHostsToken(t, dir); got != token {
			t.Errorf("%s's hosts.yml holds %q, want %q", owner, got, token)
		}
	}
	pairs := f.pairs(t)
	for _, want := range []string{"GH_CONFIG_DIR=" + filepath.Join(root, "acme"), "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=", "LEGION_GH_OWNERS_DIR=" + root, "LEGION_GH_PATH=" + fakeGH} {
		if !slices.Contains(pairs, want) {
			t.Errorf("the pane's pairs %q lack %q", pairs, want)
		}
	}
	for _, pair := range pairs {
		name, value, _ := strings.Cut(pair, "=")
		if strings.HasSuffix(name, "_PATH") && name != "LEGION_GH_PATH" {
			t.Errorf("the pane is told a tool path %q", pair)
		}
		if strings.Contains(value, staticCredentialToken(claim.RoleTester)) || strings.Contains(value, "_read_") {
			t.Errorf("the pane's %s carries a token", name)
		}
	}
	written := logLines(logs.String(), "github credential written")
	if len(written) != 1 {
		t.Fatalf("the runtime logged %d written credentials %q, want one", len(written), written)
	}
	for _, want := range []string{"claim=" + string(spec.Claim), "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00", "dirs=\"[acme sjawhar]\""} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the runtime logged %q, want %q in it", written[0], want)
		}
	}

	// A mint that fails fails the launch, naming the claim, and nothing reaches tmux.
	refused := newFakeServerRuntime(t, func(context.Context, claim.Role, ghrepo.Repository) (ghconfig.Bundle, error) {
		return ghconfig.Bundle{}, errors.New("no installation token: GitHub is down")
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

// New refuses a credential function without the real gh the panes' `legion gh` runs: an absolute
// path, which the daemon resolves at boot.
func TestNewRefusesACredentialFunctionWithoutTheRealGh(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, gh := range []string{"", "gh"} {
		_, err := New(Options{
			Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
			EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
			AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
			Executable: func() (string, error) { return "/opt/legion", nil }, GitHubCredential: staticCredential, GH: gh, ClaimRepository: acmeWidgets,
		})
		if err == nil || !strings.Contains(err.Error(), "real gh") {
			t.Errorf("New with GH %q = %v, want a refusal naming the real gh", gh, err)
		}
	}
}

// The refresher brings every tracked pane's gh bundle to the current render of its role's
// credential for the pane's own repository, the one its launch recorded (none for the controller), and
// touches nothing else: a stale owner's hosts.yml is replaced whole, by rename, and logged with the
// claim, role, App, expiry and the owners it rewrote; an owner already at the render is not
// rewritten; a root that is gone is made again, including the controller's, whose render is the
// review App's (appauth.AppRoleFor); a token that names no role is skipped. A mint that fails is
// logged and leaves the pane's last tokens in place until the next tick, and so does a render whose
// default is not the owner of the pane's own repository.
func TestTheRefresherRewritesOnlyTheOwnersWhoseHostsAreStale(t *testing.T) {
	ctx := context.Background()
	var logs strings.Builder
	r := &Runtime{
		stateDir:             t.TempDir(),
		gitHubCredential:     staticCredential,
		controllerCredential: staticControllerCredential,
		log:                  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		tracked:              map[string]*trackedProcess{},
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
	repos := map[claim.Token]ghrepo.Repository{
		stale: ghrepo.MustParse("acme/widgets"), current: ghrepo.MustParse("sjawhar/widgets"),
		gone: ghrepo.MustParse("sjawhar/widgets"), controller: {}, nameless: ghrepo.MustParse("acme/widgets"),
	}
	for i, token := range []claim.Token{stale, current, gone, controller, nameless} {
		r.track(runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "4242:" + string(rune('1'+i)),
			Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%" + string(rune('1'+i))},
		}, "LEGION-45", repos[token], "acme")
	}
	root := func(token claim.Token) string { return runtime.GHConfigDir(r.stateDir, token) }
	staleBundle, _ := staticCredential(ctx, claim.RoleTester, ghrepo.MustParse("acme/widgets"))
	staleBundle.Dirs["sjawhar"] = ghconfig.Render("ghs_stale_lease", string(appauth.Review), staticCredentialExpiry)
	if _, err := ghconfig.WriteBundle(root(stale), staleBundle); err != nil {
		t.Fatal(err)
	}
	currentBundle, _ := staticCredential(ctx, claim.RoleImplementer, ghrepo.MustParse("sjawhar/widgets"))
	if _, err := ghconfig.WriteBundle(root(current), currentBundle); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	for _, owner := range []string{"acme", "sjawhar"} {
		if err := os.Chtimes(filepath.Join(root(current), owner, ghconfig.HostsFile), past, past); err != nil {
			t.Fatal(err)
		}
	}
	r.refreshGitHubCredentials(ctx)

	if got := readHostsToken(t, ghconfig.Dir(root(stale), "sjawhar")); got != staticReadToken(claim.RoleTester, "sjawhar") {
		t.Errorf("the stale owner's hosts.yml holds %q, want %q", got, staticReadToken(claim.RoleTester, "sjawhar"))
	}
	if info, err := os.Stat(filepath.Join(root(stale), "sjawhar", ghconfig.HostsFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the replaced hosts.yml stat = %v, %v; want mode 0600", info, err)
	}
	for _, owner := range []string{"acme", "sjawhar"} {
		if info, err := os.Stat(filepath.Join(root(current), owner, ghconfig.HostsFile)); err != nil || !info.ModTime().Equal(past) {
			t.Errorf("the current pane's %s hosts.yml was rewritten (mtime %v, %v), want it left as it was", owner, info.ModTime(), err)
		}
	}
	if got := readHostsToken(t, ghconfig.Dir(root(gone), "sjawhar")); got != staticCredentialToken(claim.RoleReviewer) {
		t.Errorf("the recreated root's sjawhar hosts.yml holds %q, want %q", got, staticCredentialToken(claim.RoleReviewer))
	}
	if info, err := os.Stat(root(gone)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the recreated root stat = %v, %v; want mode 0700", info, err)
	}
	if got := readHostsToken(t, ghconfig.Dir(root(controller), "acme")); got != staticCredentialToken(claim.RoleController) {
		t.Errorf("the controller's acme hosts.yml holds %q, want the review App's %q", got, staticCredentialToken(claim.RoleController))
	}
	if _, err := os.Stat(root(nameless)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refresher made a gh directory for %s (%v)", nameless, err)
	}
	for _, token := range []claim.Token{stale, current, gone, controller} {
		if entries := ghDirEntries(t, root(token)); !slices.Equal(entries, []string{"acme", "sjawhar"}) {
			t.Errorf("%s's gh root holds %v, want the two owners' directories alone", token, entries)
		}
	}
	refreshed := logLines(logs.String(), "github credential refreshed")
	if len(refreshed) != 3 {
		t.Fatalf("the runtime logged %d refreshed credentials %q, want the controller, the stale and the recreated panes'", len(refreshed), refreshed)
	}
	if !strings.Contains(refreshed[0], "claim="+string(controller)) {
		t.Errorf("the runtime logged %q, want the controller's claim in it", refreshed[0])
	}
	for _, want := range []string{"claim=" + string(stale), "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00", "dirs=[sjawhar]"} {
		if !strings.Contains(refreshed[1], want) {
			t.Errorf("the runtime logged %q, want %q in it", refreshed[1], want)
		}
	}
	if !strings.Contains(refreshed[2], "claim="+string(gone)) {
		t.Errorf("the runtime logged %q, want the recreated pane's claim in it", refreshed[2])
	}
	if failed := logLines(logs.String(), "refresh failed"); len(failed) != 0 {
		t.Errorf("the runtime logged failures %q, want none", failed)
	}

	// A mint that fails leaves the pane's last tokens, logged; a render naming another default than
	// the owner of the pane's own repository is refused the same way; their siblings are still refreshed, each only
	// for the owner that turned.
	logs.Reset()
	r.gitHubCredential = func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		switch role {
		case claim.RoleTester:
			return ghconfig.Bundle{}, errors.New("no installation token: GitHub is down")
		case claim.RoleReviewer:
			return staticCredential(ctx, role, ghrepo.MustParse("acme/widgets"))
		}
		bundle, err := staticCredential(ctx, role, repo)
		bundle.Dirs["acme"] = ghconfig.Render("ghs_acme_second", string(appauth.AppRoleFor(role)), staticCredentialExpiry)
		return bundle, err
	}
	r.controllerCredential = func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		bundle, err := staticControllerCredential(ctx, defaultOwner)
		bundle.Dirs["acme"] = ghconfig.Render("ghs_acme_second", string(appauth.AppRoleFor(claim.RoleController)), staticCredentialExpiry)
		return bundle, err
	}
	r.refreshGitHubCredentials(ctx)
	if got := readHostsToken(t, ghconfig.Dir(root(stale), "sjawhar")); got != staticReadToken(claim.RoleTester, "sjawhar") {
		t.Errorf("after a failed mint the pane's hosts.yml holds %q, want its last token %q", got, staticReadToken(claim.RoleTester, "sjawhar"))
	}
	for owner, want := range map[string]string{"sjawhar": staticCredentialToken(claim.RoleReviewer), "acme": staticReadToken(claim.RoleReviewer, "acme")} {
		if got := readHostsToken(t, ghconfig.Dir(root(gone), owner)); got != want {
			t.Errorf("after a render naming another default the reviewer's %s hosts.yml holds %q, want its last token %q", owner, got, want)
		}
	}
	for _, token := range []claim.Token{current, controller} {
		if got := readHostsToken(t, ghconfig.Dir(root(token), "acme")); got != "ghs_acme_second" {
			t.Errorf("%s's acme hosts.yml holds %q, want the re-minted ghs_acme_second", token, got)
		}
	}
	if info, err := os.Stat(filepath.Join(root(current), "sjawhar", ghconfig.HostsFile)); err != nil || !info.ModTime().Equal(past) {
		t.Errorf("the current pane's sjawhar hosts.yml was rewritten (mtime %v, %v), want only acme's", info.ModTime(), err)
	}
	failed := strings.Join(logLines(logs.String(), "github credential refresh failed"), "\n")
	if n := strings.Count(failed, "refresh failed"); n != 2 {
		t.Fatalf("the runtime logged %d failed refreshes %q, want the tester's and the reviewer's", n, failed)
	}
	for _, want := range []string{
		"claim=" + string(gone), "default owner acme is not the pane's own owner sjawhar",
		"claim=" + string(stale), "role=" + string(claim.RoleTester), "GitHub is down",
	} {
		if !strings.Contains(failed, want) {
			t.Errorf("the runtime logged %q, want %q in it", failed, want)
		}
	}
	for _, line := range logLines(logs.String(), "github credential refreshed") {
		if !strings.Contains(line, "dirs=[acme]") {
			t.Errorf("the runtime logged %q, want acme alone rewritten", line)
		}
	}
}

// The repository of a pane the runtime adopted after a restart is the daemon's own record
// (Options.ClaimRepository), never anything under the pane's gh directory, which the pane's user
// can write: a bundle root whose default and files were rewritten to name another owner is
// brought back to the recorded repository's write token and the other owner's read token. An
// adopted controller's bundle is every owner read-only, its default the first owner, and asks the
// record nothing. A record that cannot name the repository is logged and nothing is written.
func TestAnAdoptedPanesRepositoryIsTheDaemonsRecordNotItsGhDirectory(t *testing.T) {
	ctx := context.Background()
	var logs strings.Builder
	var asked []claim.Token
	tester, err := claim.NewToken("omp", "LEGION-43", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := claim.NewToken("omp", "LEGION-46", claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	controller := claim.ControllerToken("omp")
	r := &Runtime{
		stateDir:             t.TempDir(),
		gitHubCredential:     staticCredential,
		controllerCredential: staticControllerCredential,
		claimRepository: func(_ context.Context, token claim.Token) (ghrepo.Repository, error) {
			asked = append(asked, token)
			if token == planner {
				return ghrepo.Repository{}, errors.New("no record of LEGION-46")
			}
			return ghrepo.MustParse("acme/widgets"), nil
		},
		log:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		tracked: map[string]*trackedProcess{},
	}
	for i, token := range []claim.Token{tester, planner, controller} {
		r.track(runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "4242:" + string(rune('1'+i)),
			Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%" + string(rune('1'+i))},
		}, "", ghrepo.Repository{}, "")
	}
	root := func(token claim.Token) string { return runtime.GHConfigDir(r.stateDir, token) }
	// The pane's user rewrote its bundle to name sjawhar its default, with sjawhar's directory
	// holding a write token: the bundle such a default would mint.
	tampered, _ := staticCredential(ctx, claim.RoleTester, ghrepo.MustParse("sjawhar/widgets"))
	if _, err := ghconfig.WriteBundle(root(tester), tampered); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root(tester), ".default"), []byte("sjawhar"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.refreshGitHubCredentials(ctx)

	for owner, want := range map[string]string{"acme": staticCredentialToken(claim.RoleTester), "sjawhar": staticReadToken(claim.RoleTester, "sjawhar")} {
		if got := readHostsToken(t, ghconfig.Dir(root(tester), owner)); got != want {
			t.Errorf("the adopted tester's %s hosts.yml holds %q, want %q", owner, got, want)
		}
	}
	if entries := ghDirEntries(t, root(tester)); !slices.Equal(entries, []string{"acme", "sjawhar"}) {
		t.Errorf("the adopted tester's gh root holds %v, want the two owners' directories alone", entries)
	}
	for owner, want := range map[string]string{"acme": staticCredentialToken(claim.RoleController), "sjawhar": staticReadToken(claim.RoleController, "sjawhar")} {
		if got := readHostsToken(t, ghconfig.Dir(root(controller), owner)); got != want {
			t.Errorf("the adopted controller's %s hosts.yml holds %q, want %q", owner, got, want)
		}
	}
	if _, err := os.Stat(root(planner)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refresher wrote a gh directory for %s, whose repository the record could not name (%v)", planner, err)
	}
	if !slices.Equal(asked, []claim.Token{tester, planner}) {
		t.Errorf("the refresher asked the record for %v, want the adopted tree panes' %v", asked, []claim.Token{tester, planner})
	}
	failed := logLines(logs.String(), "github credential refresh failed")
	if len(failed) != 1 || !strings.Contains(failed[0], "claim="+string(planner)) || !strings.Contains(failed[0], "no record of LEGION-46") {
		t.Errorf("the runtime logged failures %q, want the planner's alone, naming the record's error", failed)
	}
}

// The controller pane's refresh asks for its bundle by the default owner its launch recorded
// (Options.ControllerCredential), so a project that gains an owner sorting before that one keeps
// the pane's GH_CONFIG_DIR the bundle's default: the controller works no repository, and a bundle
// asked for by repository takes its default from the first owner the project serves, which after
// such a change is another owner than the one the pane was launched with.
func TestAControllerPaneIsRefreshedForTheOwnerItsLaunchRecorded(t *testing.T) {
	ctx := context.Background()
	var logs strings.Builder
	var asked []string
	app := string(appauth.AppRoleFor(claim.RoleController))
	// The project served sjawhar alone when the pane was launched, and has since gained acme,
	// which sorts before it.
	r := &Runtime{
		stateDir: t.TempDir(),
		gitHubCredential: func(context.Context, claim.Role, ghrepo.Repository) (ghconfig.Bundle, error) {
			return ghconfig.Bundle{}, errors.New("the controller's bundle is not asked for by repository")
		},
		controllerCredential: func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
			asked = append(asked, defaultOwner)
			if defaultOwner == "" {
				defaultOwner = "acme"
			}
			return ghconfig.Bundle{Default: defaultOwner, Dirs: map[string]ghconfig.Rendered{
				"acme":    ghconfig.Render("read-acme", app, staticCredentialExpiry),
				"sjawhar": ghconfig.Render("read-sjawhar", app, staticCredentialExpiry),
			}}, nil
		},
		log:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		tracked: map[string]*trackedProcess{},
	}
	controller := claim.ControllerToken("omp")
	r.track(runtime.Locator{
		Runtime: runtime.RuntimeTmux, Claim: controller, Incarnation: "4242:1",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}, "", ghrepo.Repository{}, "sjawhar")
	root := runtime.GHConfigDir(r.stateDir, controller)
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "sjawhar", Dirs: map[string]ghconfig.Rendered{
		"sjawhar": ghconfig.Render("read-sjawhar-first-lease", app, staticCredentialExpiry),
	}}); err != nil {
		t.Fatal(err)
	}

	r.refreshGitHubCredentials(ctx)

	if !slices.Equal(asked, []string{"sjawhar"}) {
		t.Errorf("the refresher asked the controller's credential for %q, want the owner its launch recorded, sjawhar", asked)
	}
	for owner, want := range map[string]string{"acme": "read-acme", "sjawhar": "read-sjawhar"} {
		if got := readHostsToken(t, ghconfig.Dir(root, owner)); got != want {
			t.Errorf("the controller's %s hosts.yml holds %q, want %q", owner, got, want)
		}
	}
	if failed := logLines(logs.String(), "github credential refresh failed"); len(failed) != 0 {
		t.Errorf("the runtime logged failures %q, want the controller refreshed rather than refused", failed)
	}
}

// leaseTokens leases "write-<owner>" for a write mint and "read-<owner>" for a read-only one, so a
// test reads which mint each owner's directory holds.
type leaseTokens struct{}

func (leaseTokens) Token(_ context.Context, _ appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return appauth.Lease{Token: "write-" + repo.Owner(), ExpiresAt: staticCredentialExpiry}, nil
}

func (leaseTokens) ReadToken(_ context.Context, _ appauth.AppRole, owner string, _ []string) (appauth.Lease, error) {
	return appauth.Lease{Token: "read-" + owner, ExpiresAt: staticCredentialExpiry}, nil
}

// A pane that holds no write token keeps holding none. A tree pane launched on no repository gets
// every owner read-only, its default the first owner; a refresh renders it for that same absent
// repository, never for its default. A tree pane the runtime adopted, whose claim the daemon
// records no repository for (ClaimRepository answers the zero Repository), is refreshed read-only
// the same way, and neither is logged as a failure.
func TestARefreshNeverGivesAReadOnlyPaneAWriteToken(t *testing.T) {
	ctx := context.Background()
	var logs strings.Builder
	credential := appauth.GitHubCredential(leaseTokens{},
		func() []ghrepo.Repository {
			return []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("acme/widgets")}
		},
		ghrepo.Renames{}, nil, slog.Default())
	f := newFakeServerRuntime(t, credential, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	spec := f.spec(t, claim.RoleImplementer)
	spec.Repository, spec.DefaultBranch = ghrepo.Repository{}, ""
	if _, err := f.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	adopted, err := claim.NewToken(spec.Project, "LEGION-47", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	f.claimRepository = func(_ context.Context, token claim.Token) (ghrepo.Repository, error) {
		if token != adopted {
			t.Errorf("the refresher asked the record for %s, which the runtime launched itself", token)
		}
		return ghrepo.Repository{}, nil
	}
	f.track(runtime.Locator{
		Runtime: runtime.RuntimeTmux, Claim: adopted, Incarnation: "4242:7",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%7"},
	}, "", ghrepo.Repository{}, "")
	readOnly := func(when string, tokens ...claim.Token) {
		t.Helper()
		for _, token := range tokens {
			root := runtime.GHConfigDir(f.stateDir, token)
			for _, owner := range []string{"acme", "sjawhar"} {
				if got := readHostsToken(t, ghconfig.Dir(root, owner)); got != "read-"+owner {
					t.Errorf("%s, %s's %s hosts.yml holds %q, want the read-only read-%s", when, token, owner, got, owner)
				}
			}
		}
	}
	readOnly("after the launch", spec.Claim)

	f.refreshGitHubCredentials(ctx)

	readOnly("after a refresh", spec.Claim, adopted)
	if failed := logLines(logs.String(), "github credential refresh failed"); len(failed) != 0 {
		t.Errorf("the runtime logged failures %q, want none", failed)
	}
}

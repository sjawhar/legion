package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// gitCredentialBundle writes a gh bundle of a role whose issue is acme's: acme's write token, the
// read token of the other owner sjawhar, and, under sjawhar, the narrower read token of the one
// repository sjawhar/legion, as a role's bundle holds one directory per repository of an owner it
// writes to none of. sjawhar's other repositories — legion-smoke among them — have no directory
// of their own, as acme's have none under acme. It returns the bundle's root and acme's
// directory, which GH_CONFIG_DIR names.
func gitCredentialBundle(t *testing.T) (root, defaultDir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{
		"acme":           ghconfig.Render("ghs_acme_write", "implement", expires),
		"sjawhar":        ghconfig.Render("ghs_sjawhar_read", "implement", expires),
		"sjawhar/legion": ghconfig.Render("ghs_sjawhar_legion_read", "implement", expires),
	}}); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.Dir(root, "acme")
}

func runGitCredentialHere(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	previous := gitCredentialStdin
	gitCredentialStdin = strings.NewReader(stdin)
	t.Cleanup(func() { gitCredentialStdin = previous })
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"legion", "git-credential"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// `legion git-credential get` answers git's credential protocol from the hosts.yml of the
// repository the path git asks about names (credential.useHttpPath), lowercased: from
// `<owner>/<repo>`'s directory of LEGION_GH_OWNERS_DIR where the bundle holds one, from the
// owner's directory otherwise — a repository the bundle holds no directory for, and a second
// segment no bundle directory can be named for, which is no reason to refuse and no way out of
// the owner's directory — and from nothing else: it answers nothing, exit 1, naming why, for any
// host but github.com over https, a request with no path, a path whose owner is no GitHub login
// or one the bundle holds no directory for, an unset LEGION_GH_OWNERS_DIR, and a hosts.yml with
// no token. GH_CONFIG_DIR, the issue owner's write token, is never an answer for another owner's
// repository. store and erase write nothing and exit 0.
func TestGitCredentialAnswersFromTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty", ghconfig.HostsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	both := map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root}
	const answer = "username=x-access-token\npassword=%s\n"
	for _, tc := range []struct {
		name     string
		env      map[string]string
		args     []string
		stdin    string
		code     int
		stdout   string
		stderrIn string
	}{
		{
			name:   "the repository's own directory",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=sjawhar/legion.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_legion_read", 1),
		},
		{
			name:   "the repository's own directory, named in another case and without .git",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=/SJawhar/Legion\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_legion_read", 1),
		},
		{
			name:   "a second segment that is no repository name",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=sjawhar/..\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_read", 1),
		},
		{
			name:   "another owner's directory",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=sjawhar/legion-smoke.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_read", 1),
		},
		{
			name:   "an owner named in another case",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=SJawhar/legion-smoke\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_read", 1),
		},
		{
			name:   "the issue owner's directory",
			env:    map[string]string{"LEGION_GH_OWNERS_DIR": root},
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=/acme/widgets.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_acme_write", 1),
		},
		{
			name:     "an owner the bundle does not hold",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=Other/Thing\n\n",
			code:     1,
			stderrIn: `"other" is not an owner of this project`,
		},
		{
			name:     "a path that is no owner",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=../sjawhar/x\n\n",
			code:     1,
			stderrIn: `"..", which is no GitHub login`,
		},
		{
			name:     "no path (useHttpPath off)",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\n\n",
			code:     1,
			stderrIn: "git sent no path",
		},
		{
			name:     "no owners directory",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: "LEGION_GH_OWNERS_DIR is unset",
		},
		{
			name:     "another host",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=gitlab.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: `host "gitlab.com"`,
		},
		{
			name:     "plain http",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=http\nhost=github.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: `protocol "http"`,
		},
		{
			name:     "a hosts.yml with no token",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=empty/thing.git\n\n",
			code:     1,
			stderrIn: "names no github.com oauth_token",
		},
		{
			name:  "store",
			env:   map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root},
			args:  []string{"store"},
			stdin: "protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_x\n\n",
		},
		{
			name:  "erase",
			env:   map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root},
			args:  []string{"erase"},
			stdin: "protocol=https\nhost=github.com\n\n",
		},
		{
			name:     "an unknown operation",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			args:     []string{"approve"},
			code:     2,
			stderrIn: "usage: legion git-credential get|store|erase",
		},
		{
			name:     "no operation",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			code:     2,
			stderrIn: "usage: legion git-credential get|store|erase",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
				if value, ok := tc.env[name]; ok {
					t.Setenv(name, value)
				} else {
					fixtureenv.Unset(t, name)
				}
			}
			code, stdout, stderr := runGitCredentialHere(t, tc.stdin, tc.args...)
			if code != tc.code || stdout != tc.stdout || !strings.Contains(stderr, tc.stderrIn) {
				t.Fatalf("legion git-credential %v = exit %d, stdout %q, stderr %q; want exit %d, stdout %q, stderr containing %q",
					tc.args, code, stdout, stderr, tc.code, tc.stdout, tc.stderrIn)
			}
		})
	}
}

// Each operation the usage line names answers --help, exit 0, with its own usage, as the docs
// site's CLI generator reads every subcommand a usage line names (docs/site/generators/legion-cli.sh),
// and reads no request.
func TestGitCredentialOperationsAnswerHelp(t *testing.T) {
	for _, op := range []string{"get", "store", "erase"} {
		code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\n\n", op, "--help")
		if code != 0 || stdout != "" || !usageOf(stderr, "git-credential", op) {
			t.Errorf("legion git-credential %s --help = exit %d, stdout %q, stderr %q; want exit 0 and its usage", op, code, stdout, stderr)
		}
	}
}

// namedTokens is each lease named for the App, the owner and whether it reads or writes, as the
// daemon's own token source answers one.
type namedTokens struct{}

func (namedTokens) Token(_ context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return appauth.Lease{Token: "ghs_" + string(role) + "_write_" + repo.Owner(), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (namedTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string, _ []string) (appauth.Lease, error) {
	return appauth.Lease{Token: "ghs_" + string(role) + "_read_" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// renamedCredentialBundle writes to disk the bundle appauth.GitHubCredential builds for an
// implementer whose issue's record names sjawhar/legion — the repository as it was recorded
// before the transfer — in a project that serves served, under renames. It returns the bundle's
// root and sjawhar's directory, which the pod's GH_CONFIG_DIR names.
func renamedCredentialBundle(t *testing.T, served []ghrepo.Repository, renames ghrepo.Renames) (root, defaultDir string) {
	t.Helper()
	repos := func() []ghrepo.Repository { return served }
	credential := appauth.GitHubCredential(namedTokens{}, repos, renames, nil, slog.Default())
	bundle, err := credential(context.Background(), legionclaim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("build the bundle: %v", err)
	}
	root = filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, bundle); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.Dir(root, bundle.Default)
}

// A pod whose issue is recorded on the repository as it was still has to write the repository as
// GitHub holds it now: with the pair configured, `legion git-credential get` for the clone path
// of that record, the one a `jj git push` of the tree presents a token for, answers the write
// token minted for cybermonklabs, the owner the repository moved to, from sjawhar's own
// directory of the bundle appauth.GitHubCredential built. It answers that whether or not the
// project serves another repository of sjawhar under its own name, since the pair is resolved
// for the repository the record names and for nothing else: one more mapping entry of that
// owner is what turned the live pair off and left 47 trees unable to push. With no pair
// configured the same bundle gives sjawhar its own token, as today, and that is what the same
// request answers.
func TestGitCredentialAnswersARenamedRepositoryWithTheOwnerItMovedTo(t *testing.T) {
	pair := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	moved := []ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")}
	for _, tc := range []struct {
		name    string
		served  []ghrepo.Repository
		renames ghrepo.Renames
		token   string
	}{
		{name: "the pair configured", served: moved, renames: pair, token: "ghs_implement_write_cybermonklabs"},
		{name: "the pair configured, and the project serves another repository of sjawhar",
			served:  append(slices.Clone(moved), ghrepo.MustParse("sjawhar/oh-my-pi")),
			renames: pair, token: "ghs_implement_write_cybermonklabs"},
		{name: "no pair configured", served: moved, renames: ghrepo.Renames{}, token: "ghs_implement_write_sjawhar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, defaultDir := renamedCredentialBundle(t, tc.served, tc.renames)
			t.Setenv("GH_CONFIG_DIR", defaultDir)
			t.Setenv("LEGION_GH_OWNERS_DIR", root)
			code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath=/sjawhar/legion.git\n\n", "get")
			want := "username=x-access-token\npassword=" + tc.token + "\n"
			if code != 0 || stdout != want || stderr != "" {
				t.Fatalf("legion git-credential get for /sjawhar/legion = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, want)
			}
		})
	}
}

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

const provisioningTokenFileEnv = "LEGION_PROVISIONING_TOKEN_FILE"

// provisioningHelper is the one-shot credential: a git credential helper that answers `get` with
// the token. git asks it for https://github.com alone (the helper is
// credential.https://github.com.helper, set after a reset of every helper configured before it),
// so a remote rewritten to another scheme, host, or port gets nothing, and no askpass is asked in
// its place. In a pod the one process holding it, Fetch, reads no configuration a tree agent can
// write, so the only remote it asks for is the one its own argv names. On the tmux runtime the
// credentialed clone and fetch read the shared clone's configuration, so there the scope is
// defence, not a boundary (config.go). One example of what it leaves open: an http.proxy the tree
// wrote with http.sslVerify off still sees the token on its way to github.com. It is an example,
// not the whole list.
const provisioningHelper = `#!/bin/sh
[ "$1" = get ] || exit 0
printf 'username=x-access-token\npassword=%s\n' "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")"
`

// remote is how a clone or fetch reaches the repository: the environment it runs with, the
// directory of the one-shot credential that environment names, if any, removed once it is done,
// the bookmarks a fetch brings (every one when empty), and, for a pod's feed, the feed repository
// the shared clone keeps its objects in (ensureRepoClone).
type remote struct {
	env      []string
	dir      string
	branches []string
	shared   string
}

// newProvisioningCredential ports workspace.ts's createProvisioningCredential, the one-shot
// credential. The Go daemon keeps the token in a 0600 file under parent, so the processes it is
// handed to receive only a file pointer and never a secret environment value. GIT_ASKPASS is set
// empty, which git reads as no askpass at all — neither core.askPass nor SSH_ASKPASS — and terminal
// prompts are off.
func newProvisioningCredential(parent, token string) (remote, error) {
	if token == "" {
		return remote{}, errors.New("workspace provisioning token is required")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return remote{}, fmt.Errorf("create provisioning credential parent %s: %w", parent, err)
	}
	directory, err := os.MkdirTemp(parent, "provisioning-credential-")
	if err != nil {
		return remote{}, fmt.Errorf("create provisioning credential directory: %w", err)
	}
	cleanup := func(cause error) (remote, error) {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			return remote{}, fmt.Errorf("%w; remove provisioning credential directory: %v", cause, removeErr)
		}
		return remote{}, cause
	}
	tokenFile := filepath.Join(directory, provisioningTokenName)
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return cleanup(fmt.Errorf("write provisioning token: %w", err))
	}
	helper := filepath.Join(directory, "helper")
	if err := os.WriteFile(helper, []byte(provisioningHelper), 0o700); err != nil {
		return cleanup(fmt.Errorf("write provisioning credential helper: %w", err))
	}
	if err := os.Chmod(helper, 0o700); err != nil {
		return cleanup(fmt.Errorf("chmod provisioning credential helper: %w", err))
	}
	return remote{
		dir: directory,
		env: []string{
			"GIT_ASKPASS=",
			"GIT_TERMINAL_PROMPT=0",
			provisioningTokenFileEnv + "=" + tokenFile,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.https://github.com.helper",
			"GIT_CONFIG_VALUE_1=!" + shellprefix.Literal(helper),
		},
	}, nil
}

// provisioningTokenName is the one-shot credential's token file, in its directory.
const provisioningTokenName = "token"

// setToken writes token over the one-shot credential's, which git's helper reads at each request
// for it: Fetch's, before each transfer from GitHub, with the token as it stands then.
func (r remote) setToken(token string) error {
	if token == "" {
		return errors.New("workspace provisioning token is required")
	}
	if err := os.WriteFile(filepath.Join(r.dir, provisioningTokenName), []byte(token), 0o600); err != nil {
		return fmt.Errorf("write provisioning token: %w", err)
	}
	return nil
}

func (r remote) remove() error {
	if r.dir == "" {
		return nil
	}
	if err := os.RemoveAll(r.dir); err != nil {
		return fmt.Errorf("remove provisioning credential directory: %w", err)
	}
	return nil
}

// Source is how provisioning reaches the repository for the shared clone's clone and fetch: from a
// pod's feed with no credential (FromFeed), or from GitHub with the one-shot credential
// (FromGitHub). Only this package's two constructors make one.
type Source interface {
	// check refuses a Source that cannot reach repository, before provisioning runs anything.
	check(repository ghrepo.Repository) error
	// open is the remote the clone and the fetch of repository use, with bookmark the issue's own
	// and defaultBranch the repository's; the caller removes it.
	open(repository ghrepo.Repository, bookmark, defaultBranch string) (remote, error)
}

// FromFeed is a pod's feed directory, where Fetch cloned the repository in the pod's first init
// container: the shared clone clones and fetches from it with no credential, in the second, which
// the provisioning Secret is not mounted in.
func FromFeed(dir string) Source { return feedSource(dir) }

type feedSource string

func (dir feedSource) check(repository ghrepo.Repository) error {
	_, err := FeedRepository(string(dir), repository)
	return err
}

func (dir feedSource) open(repository ghrepo.Repository, bookmark, defaultBranch string) (remote, error) {
	feed, err := FeedRepository(string(dir), repository)
	if err != nil {
		return remote{}, err
	}
	return feedRemote(feed, repository, bookmark, defaultBranch), nil
}

// FromGitHub is GitHub itself, reached with the one-shot credential holding token, created under
// credentialDir and removed again: the tmux runtime, which names its state directory.
func FromGitHub(token, credentialDir string) Source {
	return gitHubSource{token: token, credentialDir: credentialDir}
}

type gitHubSource struct{ token, credentialDir string }

func (s gitHubSource) check(ghrepo.Repository) error {
	if s.credentialDir == "" {
		return errors.New("workspace credential directory is required")
	}
	return nil
}

func (s gitHubSource) open(ghrepo.Repository, string, string) (remote, error) {
	return newProvisioningCredential(s.credentialDir, s.token)
}

// FeedRepository is where Fetch clones repository under a feed directory, and where a feed
// Source clones and fetches it from: <feed>/<owner>/<name>.git, the layout the repository cache's
// volume shares. The zero repository is refused, as Location refuses it: it names the feed's own
// `.git`.
func FeedRepository(feed string, repository ghrepo.Repository) (string, error) {
	if repository.IsZero() {
		return "", errors.New("workspace repository is required")
	}
	if feed == "" {
		return "", errors.New("workspace feed directory is required")
	}
	return filepath.Join(feed, repository.Owner(), repository.Name()+".git"), nil
}

// FeedEntry is whether name — a path inside a bare repository, slash-separated and relative to it,
// a directory when dir is set — is one of the refs and packs the repository cache streams and a
// feed takes from it: HEAD, packed-refs, what lies under refs/, and objects/pack/pack-<id>.pack.
// Never a loose object, nor a pack's index, reverse index or bitmap, a multi-pack index or a
// commit-graph, which the feed builds itself from the packs it verifies (unpackCache); never the
// repository's own configuration, hooks, info or description, which a feed makes for itself, nor
// objects/info/alternates or http-alternates, which would point the feed at objects elsewhere; and
// never a file or directory git is still writing (tmp_*, incoming-*, *.lock).
func FeedEntry(name string, dir bool) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, "tmp_") || strings.HasPrefix(base, "incoming-") || strings.HasSuffix(base, ".lock") {
		return false
	}
	switch {
	case dir:
		return name == "refs" || strings.HasPrefix(name, "refs/") || name == "objects" || name == "objects/pack"
	case name == "HEAD" || name == "packed-refs" || strings.HasPrefix(name, "refs/"):
		return true
	case path.Dir(name) == "objects/pack":
		return packFile.MatchString(base)
	}
	return false
}

// packFile is the name git gives a pack: its SHA-1 or SHA-256 id.
var packFile = regexp.MustCompile(`^pack-([0-9a-f]{40}|[0-9a-f]{64})\.pack$`)

// GitHubURL is the repository's GitHub remote: the URL the shared clone is cloned from and Fetch
// clones into the feed, and so the URL feedRemote rewrites to the feed.
func GitHubURL(repository ghrepo.Repository) string {
	return "https://github.com/" + repository.String()
}

// feedRemote reaches GitHubURL, the remote the shared clone's origin names, at the feed repository
// instead, over git's file transport alone: no credential, and no network. The feed is GitHub as
// it stood when the pod's workspace-fetch ran, and a tree agent may have pushed since, so a fetch
// from it brings the repository's default branch and the issue's own bookmark alone, each by its
// exact name: every other bookmark keeps its target and its tracking. The shared clone itself is
// made shared with the feed (ensureRepoClone), so its history stays there.
func feedRemote(feed string, repo ghrepo.Repository, bookmark, defaultBranch string) remote {
	return remote{
		env: []string{
			"GIT_ALLOW_PROTOCOL=file",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=url." + feed + ".insteadOf",
			"GIT_CONFIG_VALUE_0=" + GitHubURL(repo),
		},
		branches: []string{defaultBranch, bookmark},
		shared:   feed,
	}
}

// ensureRepoClone ports workspace.ts's ensureRepoClone: clone into a temporary sibling, verify it
// contains .jj, then rename it into place. A killed clone can therefore never appear to be a final
// clone. From GitHub the clone is `jj git clone`; from a pod's feed (source.shared) it is a git
// clone shared with the feed (sharedClone), which copies none of the history. The steps whose
// duration follows the repository's size (the clone, or a shared clone's checkout and then jj's
// import of every commit) share one CloneTimeout, counted on clock from the first's start, each
// given what remains of it (cloneSteps), not the runner's slow-command budget.
func ensureRepoClone(ctx context.Context, run Runner, clock Clock, cloneDir, remote, defaultBranch string, source remote) error {
	jjDir := filepath.Join(cloneDir, ".jj")
	if exists, err := pathExists(cloneDir); err != nil {
		return err
	} else if exists {
		if complete, err := pathExists(jjDir); err != nil {
			return err
		} else if complete {
			return nil
		}
		if err := os.RemoveAll(cloneDir); err != nil {
			return fmt.Errorf("remove incomplete clone %s: %w", cloneDir, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cloneDir), 0o700); err != nil {
		return fmt.Errorf("create clone parent: %w", err)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(cloneDir), filepath.Base(cloneDir)+".clone-")
	if err != nil {
		return fmt.Errorf("create temporary clone directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(temporary)
	}()

	steps := cloneSteps{clock: clock, deadline: clock.Now().Add(CloneTimeout)}
	if source.shared != "" {
		if err := sharedClone(ctx, run, steps, temporary, remote, defaultBranch, source); err != nil {
			return err
		}
	} else if err := steps.run(ctx, run, []string{"jj", "git", "clone", remote, temporary}, source.env); err != nil {
		return err
	}
	complete, err := pathExists(filepath.Join(temporary, ".jj"))
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("incomplete Jujutsu clone at %s: missing .jj", temporary)
	}
	if err := os.Rename(temporary, cloneDir); err != nil {
		if cloneCollision(err) {
			complete, statErr := pathExists(jjDir)
			if statErr == nil && complete {
				return nil
			}
		}
		return fmt.Errorf("rename cloned repository into place: %w", err)
	}
	return nil
}

// sharedClone makes the colocated clone at temporary from a pod's feed without copying its
// history: `git clone --shared` writes objects/info/alternates naming the feed's objects, which
// every container of the pod mounts at the same path; origin is then set to remote, GitHub, as a
// `jj git clone` of it would leave it; `jj git init --colocate` makes it jj's; and the default
// branch's bookmark tracks origin's, as jj's own clone tracks it, so a later fetch moves it. The
// clone checks out the feed's HEAD, as `jj git clone` checks out GitHub's: on a clone with no
// working tree jj's first snapshot would record every file deleted in the clone's own workspace. A
// default branch the feed lacks is left untracked, for the base's own refusal to name
// (baseCommit).
func sharedClone(ctx context.Context, run Runner, steps cloneSteps, temporary, remote, defaultBranch string, source remote) error {
	if err := steps.run(ctx, run, []string{"git", "clone", "--shared", "--quiet", source.shared, temporary}, source.env); err != nil {
		return err
	}
	gitDir := filepath.Join(temporary, ".git")
	if _, err := RunChecked(ctx, run, []string{"git", "--git-dir=" + gitDir, "remote", "set-url", "origin", remote}, nil, ""); err != nil {
		return err
	}
	if err := steps.run(ctx, run, []string{"jj", "git", "init", "--colocate", temporary}, nil); err != nil {
		return err
	}
	present, err := runCommand(ctx, run, []string{"git", "--git-dir=" + gitDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/" + defaultBranch}, nil, "")
	if err != nil {
		return fmt.Errorf("run git rev-parse refs/remotes/origin/%s: %w", defaultBranch, err)
	}
	if present.ExitCode != 0 {
		return nil
	}
	_, err = runCheckedOnClone(ctx, run, temporary, "bookmark", "track", originRow(defaultBranch))
	return err
}

// cloneSteps is the one CloneTimeout the shared clone's size-following steps share, ending at
// deadline by clock: ProvisionBound budgets one CloneTimeout for workspace-init's clone.
type cloneSteps struct {
	clock    Clock
	deadline time.Time
}

// run runs one size-following step of the shared clone's clone under what remains of the shared
// CloneTimeout, checked as RunChecked checks its commands; a step with nothing left fails, naming
// the bound, without running.
func (s cloneSteps) run(ctx context.Context, run Runner, argv, env []string) error {
	left := s.deadline.Sub(s.clock.Now())
	if left <= 0 {
		return fmt.Errorf("the shared clone's %s bound ran out before %s", CloneTimeout, strings.Join(argv, " "))
	}
	result, err := runCommandTimeout(ctx, run, argv, env, "", left)
	_, err = checkedResult(argv, result, err)
	return err
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func cloneCollision(err error) bool {
	return errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) || strings.Contains(err.Error(), "file exists")
}

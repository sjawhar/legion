package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// isolatedGitConfig is the configuration Fetch's git reads besides its own: none. Not the system's,
// not the image user's; and git's `-c`, GIT_CONFIG_PARAMETERS, no process provisioning starts
// inherits (config.go).
var isolatedGitConfig = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// FetchRequest is a pod's first init container's: the repository, the provisioning token, the
// directory its one-shot credential goes under, the feed directory, the log each retried clone
// attempt is told to, and the clock the retries read and wait on.
type FetchRequest struct {
	Repo          ghrepo.Repository
	Token         string
	CredentialDir string
	Feed          string
	Log           func(line string)
	Clock         Clock
}

// Fetch fills the feed from GitHub with the provisioning token and no configuration but the
// one-shot credential's and the pins: the process of a pod's first init container, the only one
// that holds the token, whose feed is its own and which mounts nothing a tree agent can write, and
// the repository cache's process (repocache), whose volume is its own. It reaches GitHub as the
// tmux runtime's Source does, so the token's holders share one credential rule. A feed that does
// not hold the repository yet has it cloned bare into a temporary sibling, its garbage collection
// turned off (feedConfiguration), and renamed into place once all of that succeeded, so the feed's
// repository is either whole or absent; a feed that holds it already — the issue volume's feed on
// a relaunch, the cache's clone at each refresh — has every branch fetched into it, the ones GitHub
// deleted pruned. Provision then clones and fetches the shared clone from the feed with no
// credential. It returns the feed repository.
//
// A transfer that fails transiently (transientCloneFailure) is run again after the next of
// fetchBackoff's waits, once per wait, its failure logged and whatever a failed clone left removed
// first, all within FetchTimeout from the first attempt's start: each attempt is bounded by what
// remains of it, and no retry starts past it. A transfer still failing after the last wait, and
// every other failure, is the answer at once. Each attempt's stderr has the token replaced by ***
// as soon as the attempt returns, so nothing Fetch logs or returns carries it, wherever git echoed
// it.
func Fetch(ctx context.Context, run Runner, request FetchRequest) (string, error) {
	feed, err := FeedRepository(request.Feed, request.Repo)
	if err != nil {
		return "", err
	}
	github := gitHubSource{token: request.Token, credentialDir: request.CredentialDir}
	if err := github.check(request.Repo); err != nil {
		return "", err
	}
	if request.Log == nil {
		return "", errors.New("workspace fetch log is required")
	}
	if request.Clock == nil {
		return "", errors.New("workspace fetch clock is required")
	}
	credential, err := github.open(request.Repo, "", "")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = credential.remove()
	}()
	if err := os.MkdirAll(filepath.Dir(feed), 0o700); err != nil {
		return "", fmt.Errorf("create feed parent: %w", err)
	}
	present, err := pathExists(feed)
	if err != nil {
		return "", err
	}
	var transfer feedTransfer
	if present {
		transfer = fetchInto(feed, request.Repo)
	} else {
		if transfer, err = cloneBeside(feed, request.Repo, request.Log); err != nil {
			return "", err
		}
		defer func() {
			_ = os.RemoveAll(transfer.destination)
		}()
	}
	if err := transferWithRetries(ctx, run, request, transfer, merge(credential.env, isolatedGitConfig)); err != nil {
		return "", err
	}
	if !present {
		if err := feedConfiguration(ctx, run, transfer.destination); err != nil {
			return "", err
		}
		if err := os.Rename(transfer.destination, feed); err != nil {
			return "", fmt.Errorf("move the cloned repository into the feed at %s: %w", feed, err)
		}
	}
	return feed, credential.remove()
}

// feedTransfer is how Fetch brings the repository from GitHub: verb names each attempt in the
// log and in a failure, argv is the attempt, and destination, for a clone, is the temporary
// directory the attempt clones into, removed before a retry.
type feedTransfer struct {
	verb        string
	argv        []string
	destination string
}

// fetchInto is the transfer into a feed repository that exists: every branch of GitHub's, named by
// its URL in the argv rather than read from the feed's configuration, pruned of those GitHub
// deleted.
func fetchInto(feed string, repo ghrepo.Repository) feedTransfer {
	return feedTransfer{verb: "fetch", argv: []string{"git", "--git-dir=" + feed, "fetch", "--prune", "--quiet", GitHubURL(repo), "+refs/heads/*:refs/heads/*"}}
}

// cloneBeside is the transfer into a feed that lacks the repository: a bare clone into a temporary
// sibling of feed. A sibling an earlier Fetch left — killed before it renamed or removed it — is
// removed first and logged: only Fetch writes beside the feed's repository, and a killed clone of
// a large repository left on a volume of fixed size would fill it.
func cloneBeside(feed string, repo ghrepo.Repository, log func(string)) (feedTransfer, error) {
	stale, err := filepath.Glob(feed + cloneSuffix + "*")
	if err != nil {
		return feedTransfer{}, err
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil {
			return feedTransfer{}, fmt.Errorf("remove the clone an earlier fetch left at %s: %w", path, err)
		}
		log("removed the clone an earlier fetch left at " + path)
	}
	destination, err := os.MkdirTemp(filepath.Dir(feed), filepath.Base(feed)+cloneSuffix)
	if err != nil {
		return feedTransfer{}, fmt.Errorf("create the clone's temporary directory beside %s: %w", feed, err)
	}
	return feedTransfer{verb: "clone", argv: []string{"git", "clone", "--bare", "--quiet", GitHubURL(repo), destination}, destination: destination}, nil
}

// cloneSuffix follows the feed repository's name in the temporary directory its clone runs in.
const cloneSuffix = ".clone-"

// feedConfiguration is what the feed repository's own configuration sets: no automatic garbage
// collection or maintenance, ever. The issue's clone keeps its objects in the feed
// (objects/info/alternates), so an object a gc dropped from the feed — one reachable only from a
// branch GitHub deleted and the fetch pruned — would be gone from the clone that still names it.
func feedConfiguration(ctx context.Context, run Runner, gitDir string) error {
	for _, pair := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}} {
		if _, err := FeedGit(ctx, run, gitDir, "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// FeedGit runs git on the bare repository gitDir — a feed's, or the repository cache's — under
// the runner's slow-command budget, holding no credential and reading no configuration but that
// repository's own: what reads or tends such a repository outside Fetch's transfer.
func FeedGit(ctx context.Context, run Runner, gitDir string, args ...string) (Result, error) {
	return RunChecked(ctx, run, append([]string{"git", "--git-dir=" + gitDir}, args...), isolatedGitConfig, "")
}

// transferWithRetries runs transfer under env until it succeeds, retrying a transient failure as
// Fetch says.
func transferWithRetries(ctx context.Context, run Runner, request FetchRequest, transfer feedTransfer, env []string) error {
	scrub := strings.NewReplacer(request.Token, "***")
	now := request.Clock.Now()
	deadline := now.Add(FetchTimeout)
	for attempt := 1; ; attempt++ {
		result, runErr := runCommandTimeout(ctx, run, transfer.argv, env, "", deadline.Sub(now))
		result.Stderr = scrub.Replace(result.Stderr)
		_, failed := checkedResult(transfer.argv, result, runErr)
		if failed == nil {
			return nil
		}
		if runErr != nil || !transientCloneFailure(result) {
			return failed
		}
		if attempt > len(fetchBackoff) {
			return fmt.Errorf("%w\n%s attempt %d failed, and the fetch retries a %s at most %d times", failed, transfer.verb, attempt, transfer.verb, len(fetchBackoff))
		}
		wait := fetchBackoff[attempt-1]
		if !request.Clock.Now().Add(wait).Before(deadline) {
			return fmt.Errorf("%w\n%s attempt %d failed, and a retry in %s would start past the fetch's %s bound", failed, transfer.verb, attempt, wait, FetchTimeout)
		}
		request.Log(fmt.Sprintf("%s attempt %d failed; retrying in %s: %s", transfer.verb, attempt, wait,
			strings.ReplaceAll(strings.TrimSpace(result.Stderr), "\n", " | ")))
		if transfer.destination != "" {
			if err := os.RemoveAll(transfer.destination); err != nil {
				return fmt.Errorf("remove what %s attempt %d left at %s: %w", transfer.verb, attempt, transfer.destination, err)
			}
		}
		if err := request.Clock.Sleep(ctx, wait); err != nil {
			return fmt.Errorf("%w\nthe wait before %s attempt %d ended: %w", failed, transfer.verb, attempt+1, err)
		}
		if now = request.Clock.Now(); !now.Before(deadline) {
			return fmt.Errorf("%w\n%s attempt %d failed, and its retry after the %s wait would start past the fetch's %s bound", failed, transfer.verb, attempt, wait, FetchTimeout)
		}
	}
}

// Clock is the time Fetch's retries read and wait on: WallClock, or a test's own.
type Clock interface {
	Now() time.Time
	// Sleep waits d, or returns ctx's error once ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
}

// WallClock is the real time.
type WallClock struct{}

func (WallClock) Now() time.Time { return time.Now() }

func (WallClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// fetchBackoff is the wait before each retry of the fetch's clone, one retry per entry: a first
// retry soon after a stream GitHub cut, the last a minute later. A clone still failing after the
// last fails the fetch, so a GitHub outage holds a launch for a few attempts, not the whole
// FetchTimeout, which bounds a slow transfer instead.
var fetchBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

// gitFatalExit is the status git's die() exits with, which every failure below ends in. Any other
// is not git reporting a failed transfer: a signal, a usage error, or the attempt's own timeout.
const gitFatalExit = 128

var (
	// causeLine is git's line saying why a request of the clone failed, in either of its forms:
	// remote-curl's `RPC failed; …` for a request of the transfer, once the repository was found,
	// and `unable to access '<url>': …` for the first request. What follows is the HTTP status the
	// server answered with, or curl's own error.
	causeLine = regexp.MustCompile(`(?:RPC failed; |unable to access '[^']*': )(.*)`)
	// statusFailure is the HTTP status a cause names, in either form: `HTTP <status> curl …` after
	// `RPC failed;`, or curl's `The requested URL returned error: <status>`.
	statusFailure = regexp.MustCompile(`(?:^HTTP |The requested URL returned error: )(\d+)`)
)

// curlNetworkFailures are curl's errors for a connection that could not be made or broke: never
// an answer from GitHub, so the next attempt may not meet them. Any other curl error a cause names
// is not here — a certificate that fails to verify (curl 60), a write to the clone that failed
// (curl 23) — since it fails the same way every time.
var curlNetworkFailures = []string{
	"Failed to connect to",     // CURLE_COULDNT_CONNECT (7): refused, or the connect timed out
	"Could not resolve host",   // CURLE_COULDNT_RESOLVE_HOST (6)
	"timed out",                // CURLE_OPERATION_TIMEDOUT (28): "Connection timed out after …", "Operation timed out after …"
	"Connection reset by peer", // a reset under any of curl's errors
	"Recv failure",             // CURLE_RECV_ERROR (56)
	"Send failure",             // CURLE_SEND_ERROR (55)
	"Empty reply from server",  // CURLE_GOT_NOTHING (52)
	"transfer closed with outstanding read data remaining", // CURLE_PARTIAL_FILE (18)
	"was not closed cleanly",                               // CURLE_HTTP2_STREAM (92)
	"HTTP2 framing layer",                                  // CURLE_HTTP2 (16)
	"SSL_ERROR_SYSCALL",                                    // OpenSSL: the connection closed under TLS
	"unexpected eof while reading",                         // OpenSSL 3: the same
	"TLS connection was non-properly terminated",           // GnuTLS, Debian's git: the same
	"GnuTLS recv error",                                    // GnuTLS: a read that failed
	"Error in the pull function",                           // GnuTLS: the same, under the handshake
	"Error in the push function",                           // GnuTLS: a write that failed
}

// streamCuts are git's own lines for a pack stream that ended before the pack did, which a
// connection closed mid-transfer leaves when curl itself reported nothing.
var streamCuts = []string{
	"early EOF",
	"unexpected disconnect while reading sideband packet",
	"the remote end hung up unexpectedly",
}

// transientCloneFailure is whether a clone of GitHub that failed may succeed if run again: git's
// fatal exit, with a cause that names a status GitHub may not answer again — 408 (it timed the
// request out), 429 (it is limiting the rate) or a 5xx (it failed) — or a connection that failed
// or broke (curlNetworkFailures), or, with no cause line at all, a pack stream that ended early
// (streamCuts). The cause line decides, never the follow-on line git prints after it: a 403 on a
// request of the transfer ends in the same `expected flush after ref listing` a 502 does. A
// credential refused (`Authentication failed`), a repository not found or not granted (404, 403),
// a certificate that does not verify, a full disk, and anything this does not recognise fail at
// once, as every clone failure did before the fetch retried.
func transientCloneFailure(result Result) bool {
	if result.TimedOut || result.ExitCode != gitFatalExit {
		return false
	}
	cause := causeLine.FindStringSubmatch(result.Stderr)
	if cause == nil {
		return containsAny(result.Stderr, streamCuts)
	}
	if status := statusFailure.FindStringSubmatch(cause[1]); status != nil {
		return transientStatus(status[1])
	}
	return containsAny(cause[1], curlNetworkFailures)
}

// transientStatus is whether an HTTP status is one a server may not answer again: 408, 429, 5xx.
func transientStatus(status string) bool {
	code, err := strconv.Atoi(status)
	return err == nil && (code == 408 || code == 429 || code >= 500 && code <= 599)
}

// containsAny is whether text holds any of needles.
func containsAny(text string, needles []string) bool {
	return slices.ContainsFunc(needles, func(needle string) bool { return strings.Contains(text, needle) })
}

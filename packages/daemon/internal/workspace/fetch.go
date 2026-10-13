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

// Fetch clones the repository from GitHub, bare, into the feed, with the provisioning token and
// no configuration but the one-shot credential's and the pins: the process of a pod's first init
// container, the only one that holds the token, whose feed is its own and which mounts nothing a
// tree agent can write. It reaches GitHub as the tmux runtime's Source does, so the token's two
// holders share one credential rule. Provision then clones and fetches the shared clone from the
// feed with no credential. It returns the feed repository.
//
// A clone that fails transiently (transientCloneFailure) is cloned again after the next of
// fetchBackoff's waits, once per wait, its failure logged and whatever it left at the feed removed
// first, all within FetchTimeout from the first attempt's start: each attempt is bounded by what
// remains of it, and no retry starts past it. A clone still failing after the last wait, and every
// other failure, is the answer at once. Each attempt's stderr has the token replaced by *** as soon
// as the attempt returns, so nothing Fetch logs or returns carries it, wherever git echoed it.
func Fetch(ctx context.Context, run Runner, request FetchRequest) (string, error) {
	feed, err := feedRepository(request.Feed, request.Repo)
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
	credential, err := github.open(request.Repo, "")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = credential.remove()
	}()
	if err := os.MkdirAll(filepath.Dir(feed), 0o700); err != nil {
		return "", fmt.Errorf("create feed parent: %w", err)
	}
	clone := []string{"git", "clone", "--bare", "--quiet", GitHubURL(request.Repo), feed}
	env := merge(credential.env, isolatedGitConfig)
	scrub := strings.NewReplacer(request.Token, "***")
	now := request.Clock.Now()
	deadline := now.Add(FetchTimeout)
	for attempt := 1; ; attempt++ {
		result, runErr := runCommandTimeout(ctx, run, clone, env, "", deadline.Sub(now))
		result.Stderr = scrub.Replace(result.Stderr)
		_, failed := checkedResult(clone, result, runErr)
		if failed == nil {
			return feed, credential.remove()
		}
		if runErr != nil || !transientCloneFailure(result) {
			return "", failed
		}
		if attempt > len(fetchBackoff) {
			return "", fmt.Errorf("%w\nclone attempt %d failed, and the fetch retries a clone at most %d times", failed, attempt, len(fetchBackoff))
		}
		wait := fetchBackoff[attempt-1]
		if !request.Clock.Now().Add(wait).Before(deadline) {
			return "", fmt.Errorf("%w\nclone attempt %d failed, and a retry in %s would start past the fetch's %s bound", failed, attempt, wait, FetchTimeout)
		}
		request.Log(fmt.Sprintf("clone attempt %d failed; retrying in %s: %s", attempt, wait,
			strings.ReplaceAll(strings.TrimSpace(result.Stderr), "\n", " | ")))
		if err := os.RemoveAll(feed); err != nil {
			return "", fmt.Errorf("remove what clone attempt %d left at %s: %w", attempt, feed, err)
		}
		if err := request.Clock.Sleep(ctx, wait); err != nil {
			return "", fmt.Errorf("%w\nthe wait before clone attempt %d ended: %w", failed, attempt+1, err)
		}
		if now = request.Clock.Now(); !now.Before(deadline) {
			return "", fmt.Errorf("%w\nclone attempt %d failed, and its retry after the %s wait would start past the fetch's %s bound", failed, attempt, wait, FetchTimeout)
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

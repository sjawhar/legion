package workspace

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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

// fetchBackoff is the wait before each retry of the fetch's clone, its last repeated until
// FetchTimeout: a first retry soon after a stream GitHub cut, then no more than one attempt a
// minute through an outage.
var fetchBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

// gitFatalExit is the status git's die() exits with, which every failure below ends in. Any other
// is not git reporting a failed transfer: a signal, a usage error, or the attempt's own timeout.
const gitFatalExit = 128

var (
	// rpcFailure is remote-curl's line for a request of the transfer that failed once the
	// repository was found: `RPC failed; HTTP <status> curl <code> …` when the server answered
	// with an error status, `RPC failed; curl <code> …` when the connection failed or broke.
	rpcFailure = regexp.MustCompile(`RPC failed; (?:HTTP (\d+))?`)
	// accessFailure is git's fatal line for a first request that failed for any reason but a
	// refused credential or a missing repository (each its own line), naming curl's error.
	accessFailure = regexp.MustCompile(`unable to access '[^']*': (.*)`)
	// statusFailure is curl's error for a server's error status.
	statusFailure = regexp.MustCompile(`The requested URL returned error: (\d+)`)
)

// curlNetworkFailures are curl's errors for a connection that could not be made or broke: never
// an answer from GitHub, so the next attempt may not meet them. A certificate that fails to verify
// is not here, since it fails the same way every time.
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
// fatal exit with stderr naming a connection that failed or broke, or a status GitHub may not
// answer again — 408 (it timed the request out), 429 (it is limiting the rate) or a 5xx (it
// failed). The cause line decides, never the follow-on line git prints after it: a 403 on a
// request of the transfer ends in the same `expected flush after ref listing` a 502 does. A
// credential refused (`Authentication failed`), a repository not found or not granted (404, 403),
// a certificate that does not verify, a full disk, and anything this does not recognise fail at
// once, as every clone failure did before the fetch retried.
func transientCloneFailure(result Result) bool {
	if result.TimedOut || result.ExitCode != gitFatalExit {
		return false
	}
	if rpc := rpcFailure.FindStringSubmatch(result.Stderr); rpc != nil {
		return rpc[1] == "" || transientStatus(rpc[1])
	}
	if access := accessFailure.FindStringSubmatch(result.Stderr); access != nil {
		if status := statusFailure.FindStringSubmatch(access[1]); status != nil {
			return transientStatus(status[1])
		}
		return containsAny(access[1], curlNetworkFailures)
	}
	return containsAny(result.Stderr, streamCuts)
}

// transientStatus is whether an HTTP status is one a server may not answer again: 408, 429, 5xx.
func transientStatus(status string) bool {
	code, err := strconv.Atoi(status)
	return err == nil && (code == 408 || code == 429 || code >= 500 && code <= 599)
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// packages/envoy/cmd/agent-secrets/identity.go
//
// Where this process's session identity lives, and `agent-secrets identity`, the one local test
// of whether it has one. An agent box or pod keeps its key under AGENT_SECRETS_KEY_DIR (file
// mode); a host session asks agent-secrets-helper over AGENT_SECRETS_HELPER_SOCK (helper mode).
// A process can be inside its session with neither variable set — omp's eval kernel starts every
// run with a filtered environment that drops AGENT_SECRETS_* — so each falls back to the path its
// launcher uses: a box's key dir is $XDG_RUNTIME_DIR/agent-secrets (scripts/agentbox mounts it
// there), and the helper's socket is helper.DefaultSocket, in that same directory on the host,
// where the helper keeps no key.pem. A variable that is set is used as given; a default is used
// only when its file is there.
//
// A box's key and enrollment arrive after the box starts: its launcher runs keygen inside it and
// enrolls it once omp's session id appears, while omp is already starting MCP servers whose first
// call needs them. The launcher marks that window explicitly, writing enrollment.pending into the
// key dir before the box starts and removing it once enrollment is written, or once it has
// written enrollment.error. A call waits only while that marker is there and fresh; every other
// state fails as fast as it always did.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

const (
	// keyFile and enrollmentFile are a box's or pod's identity in its key dir: the key keygen
	// writes, and the enrollment id enroll (or the pod's launcher) writes beside it.
	keyFile        = "key.pem"
	enrollmentFile = "enrollment"
	// pendingMarker is the file a box's launcher keeps in the key dir while it sets the box up.
	pendingMarker = "enrollment.pending"
	// pendingMarkerLife bounds how long a client trusts a marker, by its mtime. It covers the
	// launcher's usual setup (120 s for omp's session id, ten enrollment attempts about 3 s apart,
	// and keygen), not its worst case: against a slow broker the launcher can keep retrying for
	// about 450 s. A setup slower than this bound, or a marker a dead launcher left, makes a call
	// fail fast with the message it would give without the marker.
	pendingMarkerLife = 160 * time.Second
	// defaultEnrollWait bounds how long a call waits on a live marker (AGENT_SECRETS_ENROLL_WAIT
	// overrides it). It stays under omp's 30 s MCP connect timeout: an MCP server's first call is
	// the one that meets the window, and omp never retries a server that failed to start.
	defaultEnrollWait  = 20 * time.Second
	enrollPollInterval = 100 * time.Millisecond
	// identityDeadline bounds identity's one question to the helper, dial included.
	identityDeadline = 2 * time.Second
	// identityProbeURL is the htu of the proof identity asks the helper for. It is not a URL, so
	// the proof matches no broker route and is worthless if it ever leaked; identity discards it
	// unread.
	identityProbeURL = "agent-secrets:identity"
)

// keyDir is AGENT_SECRETS_KEY_DIR, else the default $XDG_RUNTIME_DIR/agent-secrets — the
// directory helper.DefaultSocket names, so both defaults read XDG_RUNTIME_DIR one way.
func keyDir() string {
	if dir := os.Getenv("AGENT_SECRETS_KEY_DIR"); dir != "" {
		return dir
	}
	return filepath.Dir(helper.DefaultSocket(os.Getenv))
}

// helperSocket is AGENT_SECRETS_HELPER_SOCK, else the helper's default socket; named reports
// which, since a default counts only when the socket is there.
func helperSocket() (sock string, named bool) {
	if sock := os.Getenv("AGENT_SECRETS_HELPER_SOCK"); sock != "" {
		return sock, true
	}
	return helper.DefaultSocket(os.Getenv), false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// freshMarker reports whether dir holds a launcher's enrollment.pending younger than
// pendingMarkerLife.
func freshMarker(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, pendingMarker))
	return err == nil && time.Since(info.ModTime()) < pendingMarkerLife
}

// enrollWait is AGENT_SECRETS_ENROLL_WAIT (a Go duration), else defaultEnrollWait.
func enrollWait() (time.Duration, error) {
	v := os.Getenv("AGENT_SECRETS_ENROLL_WAIT")
	if v == "" {
		return defaultEnrollWait, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("AGENT_SECRETS_ENROLL_WAIT=%q is not a duration such as 20s", v)
	}
	return d, nil
}

// awaitEnrollment waits, for wait at most, while dir holds a fresh marker and not yet both
// key.pem and enrollment. It reports whether it gave up with the marker still there.
func awaitEnrollment(dir string, wait time.Duration) (gaveUp bool) {
	deadline := time.Now().Add(wait)
	for freshMarker(dir) && !(exists(filepath.Join(dir, keyFile)) && exists(filepath.Join(dir, enrollmentFile))) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		time.Sleep(min(enrollPollInterval, remaining))
	}
	return false
}

// helperInstalled reports whether this machine has the helper's user unit, the file Plan B's
// launchers (dotfiles shims/omp, scripts/cld, scripts/oc) also read as "the helper is installed".
// Its socket being absent then means the helper is stopped (a clean stop removes the socket),
// not that this machine has none.
func helperInstalled() bool {
	home, err := os.UserHomeDir()
	return err == nil && exists(filepath.Join(home, ".config", "systemd", "user", "agent-secrets-helper.service"))
}

// cmdIdentity implements "identity": exit 0 when this process has a broker identity, 1 when it
// does not, without the network and without changing anything. It never prints to stdout, so a
// caller can put it in front of a command whose stdout is the caller's own (dotfiles'
// secret-run, legion's envoy scripts), and it never registers anything: `register` from an
// unregistered process would create a session, and self or whoami would ask the broker.
//
// A process has an identity when its key dir holds key.pem or a fresh enrollment.pending, or when
// the helper's sign op — which signs a throwaway proof and changes nothing — answers OK (enrolled)
// or NOT_ENROLLED (registered and still enrolling) for it. Such a process uses the broker even if
// its enrollment later fails, and the failure is the broker call's. A registered session on a
// helper holding no launcher credential (NO_CREDENTIAL: after a reboot or helper restart, until
// the operator logs the machine in) has none, since that helper enrolls no one; it is exit 1 with
// the not-logged-in notice. A helper that cannot be asked — its socket refuses connections, is
// absent although AGENT_SECRETS_HELPER_SOCK names it or the helper is installed, or gives no
// answer within identityDeadline — is exit 1 with a notice on stderr, so a caller that then uses
// its other backend does not do so silently. With no key dir, no socket named or present, and no
// helper installed (a laptop), it is exit 1 and silent.
func cmdIdentity(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "agent-secrets identity: unexpected argument %q\n", args[0])
		return exitUsageError
	}
	dir := keyDir()
	if exists(filepath.Join(dir, keyFile)) || freshMarker(dir) {
		return 0
	}
	sock, named := helperSocket()
	if !exists(sock) {
		if named || helperInstalled() {
			fmt.Fprintf(stderr, "agent-secrets: helper unreachable at %s; not an agent session (the socket is absent)\n", sock)
		}
		return 1
	}
	resp, err := helper.Call(sock, helper.Request{Op: "sign", Method: http.MethodGet, URL: identityProbeURL}, identityDeadline)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: helper unreachable at %s; not an agent session (%v)\n", sock, err)
		return 1
	}
	switch {
	case resp.OK, resp.Code == helper.CodeNotEnrolled:
		return 0
	case resp.Code == helper.CodeNotASession:
		return 1
	case resp.Code == helper.CodeNoCredential:
		reportError(stderr, "agent-secrets identity", errNoCredential)
		return 1
	default:
		fmt.Fprintf(stderr, "agent-secrets: helper at %s answered %s: %s; not an agent session\n", sock, resp.Code, resp.Error)
		return 1
	}
}

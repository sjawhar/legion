// Command agent-secrets is the client for the AGENTC-393 secrets broker: an agent box or pod
// signs with the key.pem and enrollment file it keeps under AGENT_SECRETS_KEY_DIR (file mode); a
// host session has no key of its own and asks agent-secrets-helper, over
// AGENT_SECRETS_HELPER_SOCK, to sign on its behalf (helper mode). It enrolls a runtime (box
// enrollment only, through the local helper — see cmdEnrollHelper below), requests and reads
// back secret grants, and — in its most common shape — requests one or more secrets and execs a
// command with them in its environment.
//
//	agent-secrets keygen --out <dir>
//	agent-secrets enroll --helper --kind box --runtime-id <id> --thumbprint <tp> [--session-id <id>]
//	agent-secrets unenroll --helper --enrollment <id>
//	agent-secrets launcher login
//	agent-secrets launcher login-status
//	agent-secrets register [--wait SECONDS] [--exec -- COMMAND [ARGS...]]
//	agent-secrets identity
//	agent-secrets renew
//	agent-secrets request NAME... [--reason TEXT] [--json]
//	agent-secrets status <request_id> [--json]
//	agent-secrets cancel <request_id>
//	agent-secrets revoke <grant_id>
//	agent-secrets self [--json]
//	agent-secrets whoami [--json]
//	agent-secrets sign --method M --url U [--enrollment E]
//	agent-secrets NAME... [--reason TEXT] [--wait DURATION] -- <command> [args...]
//
// Environment: AGENT_SECRETS_URL (the broker's base URL) and, for every session-authenticated
// subcommand, either AGENT_SECRETS_KEY_DIR (an agent box or pod's key.pem and enrollment, the
// two files keygen and enroll write) or AGENT_SECRETS_HELPER_SOCK (a host session's
// agent-secrets-helper socket, joined by `agent-secrets register`). When unset, each falls back
// to its launcher's default path, $XDG_RUNTIME_DIR/agent-secrets and the helper socket inside it,
// and a default counts only when its file is there (identity.go). AGENT_SECRETS_ENROLL_WAIT (a
// duration, default 20s) bounds how long a call waits while a box's launcher is still enrolling
// it. AGENT_SECRETS_APPROVE_URL (Dispatch's origin) makes `launcher login` and a pending exec form
// print the Dispatch page where a person decides. The exec form's child keeps AGENT_SECRETS_URL,
// AGENT_SECRETS_KEY_DIR and AGENT_SECRETS_HELPER_SOCK, since it is the same session
// (buildChildEnv). A host session enrolls (kind host) automatically through the helper's own
// enroll loop, and a pod's own enrollment is its launcher's job — this CLI has no direct
// enrollment path for either; only a box enrolls through it, and only via --helper (the shared
// broker contract, dispatch://AGENTC-393/artifact/plan-overview-md, has no launcher bearer token:
// nothing on a devbox can enroll except through a helper or the Legion daemon, the two processes
// that hold a launcher's proof-signing key).
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/buildversion"
)

// Exit codes beyond 0 (done) and 1 (failed): 75 (EX_TEMPFAIL) and 77 (EX_NOPERM) follow BSD
// sysexits.h, chosen so a caller of the "request" and NAME... -- <command> forms can tell "still
// waiting" from "refused" without parsing stderr, and 126 and 127 are the shell's own for a command
// that cannot run or is not there. Each carries a comment, and every exit code is 0, 1 or one of
// these: the broker's generated error reference (cmd/broker-refgen) prints them and refuses a code
// without a comment or a function that returns any other.
const (
	exitUsageError = 2   // a usage error: an unknown flag or argument, or a required one or AGENT_SECRETS_URL missing
	exitPending    = 75  // the request is still waiting for a person to approve it; nothing was run
	exitDenied     = 77  // the request was denied; nothing was run
	exitCannotRun  = 126 // the command `register --exec` was given exists but could not be run
	exitNotFound   = 127 // the command `register --exec` was given was not found
)

// command is one form of agent-secrets, as usage lists it and its own -h describes it.
type command struct {
	name     string // the words after "agent-secrets"; "" for the NAME... -- <command> form
	synopsis string
	summary  string // one or more lines, each under 90 characters
}

// commands is every form, in the order usage lists them.
var commands = []command{
	{"keygen", "agent-secrets keygen --out <dir>",
		"Write a new signing key to <dir>/key.pem and print its thumbprint. A box's launcher runs it\ninside the box before enrolling it."},
	{"enroll", "agent-secrets enroll --helper --kind box --runtime-id <id> --thumbprint <tp> [--session-id <id>]",
		"Enroll a box's key through this machine's agent-secrets-helper, write the enrollment id to\nAGENT_SECRETS_KEY_DIR/enrollment, and print it."},
	{"unenroll", "agent-secrets unenroll --helper --enrollment <id>",
		"Revoke a box's enrollment through this machine's agent-secrets-helper."},
	{"launcher login", "agent-secrets launcher login",
		"Log this machine in: print the confirmation code its operator types into Dispatch, then wait\nfor the decision (exit 0 once approved, 1 when denied or expired)."},
	{"launcher login-status", "agent-secrets launcher login-status",
		"Print \"issued\" and exit 0 while this machine's helper holds a launcher credential; otherwise\nprint the last login's state and exit 1."},
	{"register", "agent-secrets register [--wait SECONDS] [--exec -- COMMAND [ARGS...]]",
		"Register this process with agent-secrets-helper as a host session. With --exec, run COMMAND\nas this same process, so it and everything it starts are that session."},
	{"identity", "agent-secrets identity",
		"Exit 0 when this process has a broker identity and 1 when it does not, asking the broker\nnothing."},
	{"--version", "agent-secrets --version",
		"Print the release this binary was built as."},
	{"renew", "agent-secrets renew",
		"Keep an agent box's enrollment lease alive, renewing it until interrupted."},
	{"request", "agent-secrets request NAME... [--reason TEXT] [--json]",
		"Request secrets and print the request's id and state; exit 75 while it waits for approval\nand 77 when it is denied."},
	{"status", "agent-secrets status <request_id> [--json]",
		"Print a request's state, its grant once granted, and who decided it."},
	{"cancel", "agent-secrets cancel <request_id>",
		"Cancel one of this session's pending requests."},
	{"revoke", "agent-secrets revoke <grant_id>",
		"End one of this session's grants."},
	{"self", "agent-secrets self [--json]",
		"Print this session's enrollment, operator, lease, and live grants."},
	{"whoami", "agent-secrets whoami [--json]",
		"Print this session's enrollment as the broker's JSON, as self --json does."},
	{"sign", "agent-secrets sign --method M --url U [--enrollment E]",
		"Print the proof this session would sign for one broker call, without making the call."},
	{"", "agent-secrets NAME... [--reason TEXT] [--wait DURATION] -- <command> [args...]",
		"Request secrets, wait up to --wait (default 30m) for a person to decide, and run <command>\nwith each granted secret in its environment under its NAME; nothing runs unless all are\ngranted."},
}

// usageEnvironment closes usage: the variables every form reads, and the exit codes.
const usageEnvironment = `
environment:
  AGENT_SECRETS_URL          the broker's base URL; every form that calls the broker needs it
  AGENT_SECRETS_KEY_DIR      an agent box's or pod's key.pem and enrollment
                             (default $XDG_RUNTIME_DIR/agent-secrets, used when key.pem is there)
  AGENT_SECRETS_HELPER_SOCK  a host session's agent-secrets-helper socket
                             (default $XDG_RUNTIME_DIR/agent-secrets/helper.sock, used when it is there)
  AGENT_SECRETS_ENROLL_WAIT  how long a call waits while a box's launcher is still enrolling it
                             (default 20s)
  AGENT_SECRETS_APPROVE_URL  Dispatch's address; launcher login names the page under it where the
                             operator types the code, and the exec form the page where a person
                             approves its waiting request
  OMP_SESSION_ID             the agent session the broker notifies if a pending request expires

exit codes: 0 done, 1 failed, 2 usage error, 75 still waiting for approval, 77 denied;
register --exec exits 127 when COMMAND is not found and 126 when it cannot run
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage())
		return 0
	case "--version":
		fmt.Fprintf(stdout, "agent-secrets %s\n", buildversion.String())
		return 0
	case "keygen":
		return cmdKeygen(args[1:], stdout, stderr)
	case "enroll":
		return cmdEnroll(args[1:], stdout, stderr)
	case "unenroll":
		return cmdUnenroll(args[1:], stdout, stderr)
	case "launcher":
		return cmdLauncher(args[1:], stdout, stderr)
	case "register":
		return cmdRegister(args[1:], stdout, stderr)
	case "renew":
		return cmdRenew(args[1:], stdout, stderr)
	case "request":
		return cmdRequest(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "cancel":
		return cmdCancel(args[1:], stdout, stderr)
	case "revoke":
		return cmdRevoke(args[1:], stdout, stderr)
	case "self":
		return cmdSelf("self", args[1:], stdout, stderr)
	case "whoami":
		// whoami is GET /v1/enrollments/self, but must always print the broker's raw JSON body
		// (scripts/agentbox's box-doctor and host-doctor both pipe bare `agent-secrets whoami`
		// straight into jq): identical to self --json, so it aliases cmdSelf with --json forced
		// on rather than duplicating it.
		return cmdSelf("whoami", append([]string{"--json"}, args[1:]...), stdout, stderr)
	case "sign":
		return cmdSign(args[1:], stdout, stderr)
	case "identity":
		return cmdIdentity(args[1:], stdout, stderr)
	default:
		return cmdExec(args, stdout, stderr)
	}
}

func usage() string {
	var b strings.Builder
	b.WriteString("usage:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %s\n", c.synopsis)
		for line := range strings.SplitSeq(c.summary, "\n") {
			fmt.Fprintf(&b, "      %s\n", line)
		}
	}
	b.WriteString(usageEnvironment)
	return b.String()
}

func lookupCommand(name string) command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	panic(fmt.Sprintf("agent-secrets: commands has no entry %q", name))
}

// writeCommandHelp prints one form's synopsis and summary.
func writeCommandHelp(w io.Writer, c command) {
	fmt.Fprintf(w, "usage: %s\n\n%s\n", c.synopsis, c.summary)
}

// newFlagSet is the flag set of the form named name: its errors and its -h go to stderr, and -h
// prints the form's synopsis and summary, then its flags when it defines any. A form with no
// flags parses its arguments with it too, so its -h answers as every other form's does and any
// flag is a usage error.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	c := lookupCommand(name)
	flags := flag.NewFlagSet(strings.TrimSpace("agent-secrets "+name), flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		writeCommandHelp(flags.Output(), c)
		hasFlags := false
		flags.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(flags.Output(), "\nflags:")
			flags.PrintDefaults()
		}
	}
	return flags
}

func exitUsage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return exitUsageError
}

// reportError prints a command's failure as "<prefix>: <err>", except that a helper holding no
// launcher credential is reported with the one line identity prints for it, whatever the command.
func reportError(stderr io.Writer, prefix string, err error) {
	if errors.Is(err, errNoCredential) {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", errNoCredential)
		return
	}
	fmt.Fprintf(stderr, "%s: %v\n", prefix, err)
}

// splitArgs partitions args into recognized "--flag"/"--flag value" tokens (as declared by
// takesValue, keyed by flag name without its leading dashes) and every other bare token, so a
// subcommand can accept secret names or ids before, after, or between its optional flags — the
// stdlib flag package alone stops parsing at the first non-flag token, which would misparse
// "agent-secrets request NAME --reason TEXT". A token absent from takesValue is treated as a
// boolean flag consuming no value (e.g. --json).
func splitArgs(args []string, takesValue map[string]bool) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := strings.TrimLeft(a, "-")
		if !strings.HasPrefix(a, "-") || name == "" || name == a {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		if takesValue[name] && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// writeFileAtomic writes data to path by renaming a finished temporary file into place, so no
// reader ever sees path half-written: a call waiting out a box's enrollment (awaitEnrollment)
// reads key.pem and enrollment the moment each appears.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has taken it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA private key", path)
	}
	return key, nil
}

// buildSigner selects and builds this process's Signer: an agent box or pod signs with the key
// and enrollment id it keeps in its key dir (file mode); a host session has no key of its own and
// asks agent-secrets-helper over its socket, which signs only for processes descending from a
// session `agent-secrets register` registered (helper mode). The key dir and socket are
// AGENT_SECRETS_KEY_DIR and AGENT_SECRETS_HELPER_SOCK, or their defaults where those are unset
// (identity.go). While a box's launcher is still enrolling it (a fresh enrollment.pending in the
// key dir), this first waits for key.pem and enrollment, for AGENT_SECRETS_ENROLL_WAIT at most.
// The enrollment id it returns is file mode's own; helper mode returns "" because nothing needs
// it as a literal except RenewEnrollment's URL path, and a host session's lease is renewed by the
// helper itself — cmdRenew refuses outright once it sees an empty id from a successful build.
func buildSigner() (enrollmentID string, signer Signer, err error) {
	wait, err := enrollWait()
	if err != nil {
		return "", nil, err
	}
	dir := keyDir()
	gaveUp := awaitEnrollment(dir, wait)
	enrollmentID, signer, err = selectSigner(dir)
	if err != nil && gaveUp {
		err = fmt.Errorf("%w (waited %s while %s was there)", err, wait, filepath.Join(dir, pendingMarker))
	}
	return enrollmentID, signer, err
}

func selectSigner(dir string) (string, Signer, error) {
	if keyPath := filepath.Join(dir, keyFile); exists(keyPath) {
		key, keyErr := loadKey(keyPath)
		if keyErr != nil {
			return "", nil, keyErr
		}
		id, idErr := readTrimmed(filepath.Join(dir, enrollmentFile))
		if idErr != nil {
			if reason, rerr := readTrimmed(filepath.Join(dir, "enrollment.error")); rerr == nil {
				return "", nil, fmt.Errorf("%w (enrollment.error: %s)", idErr, reason)
			}
			return "", nil, idErr
		}
		if id == "" {
			return "", nil, fmt.Errorf("%s: empty", filepath.Join(dir, enrollmentFile))
		}
		return id, &fileSigner{key: key, enrollmentID: id}, nil
	}
	sock, named := helperSocket()
	if named || exists(sock) {
		return "", &helperSigner{sock: sock}, nil
	}
	return "", nil, fmt.Errorf("no session identity: the key dir %s has no key.pem (AGENT_SECRETS_KEY_DIR; an agent box or pod is enrolled by its launcher) and no helper socket is set or at %s (AGENT_SECRETS_HELPER_SOCK; a host session is registered by `agent-secrets register`)", dir, sock)
}

// sessionContext resolves the broker URL and this process's Signer every session-authenticated
// subcommand (renew, request, status, cancel, revoke, self, and the exec form) needs, plus the
// enrollment id buildSigner read from file mode (empty in helper mode).
func sessionContext() (base, enrollmentID string, signer Signer, err error) {
	base = strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		return "", "", nil, errors.New("AGENT_SECRETS_URL is required")
	}
	enrollmentID, signer, err = buildSigner()
	if err != nil {
		return "", "", nil, err
	}
	return base, enrollmentID, signer, nil
}

// writeVerbatim writes exactly raw, nothing more and nothing less: --json's contract is "the
// response body verbatim ... one JSON object on stdout and nothing else", so this must never
// append a newline the broker's own response didn't already end with.
func writeVerbatim(w io.Writer, raw []byte) {
	w.Write(raw)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// nextBackoff advances the 2s -> 10s backoff every poll loop in this package uses.
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > 10*time.Second {
		next = 10 * time.Second
	}
	return next
}

// ---------------------------------------------------------------------------
// keygen
// ---------------------------------------------------------------------------

func cmdKeygen(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"out": true})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets keygen: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := newFlagSet("keygen", stderr)
	out := flags.String("out", os.Getenv("AGENT_SECRETS_KEY_DIR"), "directory to write key.pem into")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if *out == "" {
		fmt.Fprintln(stderr, "agent-secrets keygen: --out (or AGENT_SECRETS_KEY_DIR) is required")
		return exitUsageError
	}
	key, err := proof.NewKey()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: generate key: %v\n", err)
		return 1
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: marshal key: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: %v\n", err)
		return 1
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writeFileAtomic(filepath.Join(*out, keyFile), pemBytes, 0o600); err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: %v\n", err)
		return 1
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: thumbprint: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, thumb)
	return 0
}

// ---------------------------------------------------------------------------
// enroll / unenroll (box only, through the local agent-secrets-helper socket)
// ---------------------------------------------------------------------------

// cmdEnroll implements "enroll --helper --kind box --runtime-id <id> --thumbprint <tp>
// [--session-id <id>]". --helper is required: the shared broker contract has no launcher bearer
// token, so this CLI has no other way to enroll anything — the host's one launcher credential
// lives only in agent-secrets-helper's memory (a human installs it with `agent-secrets launcher
// login`), and only a box enrolls through this command at all (a host session enrolls itself
// automatically via `agent-secrets register`; a pod's enrollment is its own launcher's job).
func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{
		"kind": true, "runtime-id": true, "thumbprint": true, "session-id": true,
	})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets enroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := newFlagSet("enroll", stderr)
	kind := flags.String("kind", "", `"box" (the only kind --helper supports)`)
	runtimeID := flags.String("runtime-id", "", "this runtime's stable id")
	thumbprint := flags.String("thumbprint", "", "the signing key's RFC 7638 thumbprint")
	sessionID := flags.String("session-id", "", "the agent session the broker notifies if one of this box's pending requests expires")
	helperFlag := flags.Bool("helper", false, "enroll through this machine's agent-secrets-helper, which holds its launcher credential (required: a launcher credential is a key held only by a helper or a launcher, never a token this CLI could read)")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper is required (this CLI enrolls only through the local agent-secrets-helper socket)")
		return exitUsageError
	}
	return cmdEnrollHelper(*kind, *runtimeID, *thumbprint, *sessionID, stdout, stderr)
}

// cmdEnrollHelper asks the local agent-secrets-helper daemon, over its unix socket, to enroll
// this box's key using the machine credential `agent-secrets launcher login` installed.
// --helper supports only kind "box" today: a host session enrolls itself through
// `agent-secrets register`, not this flag, and a pod has no local helper daemon to ask. On
// success it writes the enrollment id into AGENT_SECRETS_KEY_DIR/enrollment (buildSigner reads
// it back for every later session-authenticated subcommand) and prints the enrollment id alone
// on stdout — the contract scripts/agentbox parses.
func cmdEnrollHelper(kind, runtimeID, thumbprint, sessionID string, stdout, stderr io.Writer) int {
	if kind != "box" {
		fmt.Fprintln(stderr, `agent-secrets enroll: --helper supports only --kind box today`)
		return exitUsageError
	}
	if runtimeID == "" || thumbprint == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper needs --runtime-id and --thumbprint")
		return exitUsageError
	}
	dir := os.Getenv("AGENT_SECRETS_KEY_DIR")
	if dir == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: AGENT_SECRETS_KEY_DIR is required")
		return exitUsageError
	}
	sock, _ := helperSocket()
	var sessionIDPtr *string
	if sessionID != "" {
		sessionIDPtr = &sessionID
	}
	resp, err := helper.Call(sock, helper.Request{Op: "enroll-box", RuntimeID: runtimeID, Thumbprint: thumbprint, SessionID: sessionIDPtr}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets enroll: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(filepath.Join(dir, enrollmentFile), []byte(resp.EnrollmentID+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, resp.EnrollmentID)
	return 0
}

// cmdUnenroll implements "unenroll --helper --enrollment <id>". --helper is required for the
// same reason cmdEnroll requires it: the shared broker contract has no launcher bearer token.
func cmdUnenroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"enrollment": true})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets unenroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := newFlagSet("unenroll", stderr)
	enrollmentID := flags.String("enrollment", "", "the enrollment id to revoke")
	helperFlag := flags.Bool("helper", false, "unenroll through the local agent-secrets-helper socket")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --helper is required (this CLI unenrolls only through the local agent-secrets-helper socket)")
		return exitUsageError
	}
	if *enrollmentID == "" {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --enrollment is required")
		return exitUsageError
	}
	return cmdUnenrollHelper(*enrollmentID, stderr)
}

// cmdUnenrollHelper asks the local agent-secrets-helper daemon to revoke a box enrollment over
// its unix socket, using the machine credential `agent-secrets launcher login` installed.
// Idempotent: the helper's UnenrollBox treats 204 and 404 as done. Prints nothing on success,
// matching the exit-code-only unenroll contract.
func cmdUnenrollHelper(enrollmentID string, stderr io.Writer) int {
	sock, _ := helperSocket()
	resp, err := helper.Call(sock, helper.Request{Op: "unenroll-box", EnrollmentID: enrollmentID}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// renew
// ---------------------------------------------------------------------------

func cmdRenew(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("renew", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintln(stderr, "agent-secrets renew: no arguments are accepted")
		return exitUsageError
	}
	base, enrollmentID, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets renew: %v\n", err)
		return exitUsageError
	}
	if enrollmentID == "" {
		fmt.Fprintln(stderr, "agent-secrets renew: a host session's lease is renewed by agent-secrets-helper; agent-secrets renew is for agent boxes")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := newClient(base)
	backoff := 2 * time.Second
	for {
		expires, err := c.RenewEnrollment(ctx, signer, enrollmentID)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			// The broker answering with a refusal (401 PROOF_INVALID: the enrollment is gone) is
			// final; anything else — a restart, a network blip, a 5xx — is retried until the lease
			// would be lost anyway.
			var refused *apiError
			if errors.As(err, &refused) && refused.Status/100 == 4 {
				fmt.Fprintf(stderr, "agent-secrets renew: %v\n", err)
				return 1
			}
			fmt.Fprintf(stderr, "agent-secrets renew: %v; retrying in %s\n", err, backoff)
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = 2 * time.Second
		lease := time.Until(expires)
		if lease <= 0 {
			lease = time.Second
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(lease / 3):
		}
	}
}

// ---------------------------------------------------------------------------
// request
// ---------------------------------------------------------------------------

func cmdRequest(args []string, stdout, stderr io.Writer) int {
	flagArgs, names := splitArgs(args, map[string]bool{"reason": true})
	flags := newFlagSet("request", stderr)
	reason := flags.String("reason", "", "why these secrets are needed")
	asJSON := flags.Bool("json", false, "print the response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(names) == 0 {
		fmt.Fprintln(stderr, "agent-secrets request: at least one secret NAME is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets request: %v\n", err)
		return exitUsageError
	}
	result, raw, err := newClient(base).CreateRequest(context.Background(), signer, names, *reason, os.Getenv("OMP_SESSION_ID"))
	if err != nil {
		reportError(stderr, "agent-secrets request", err)
		return 1
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
	} else {
		fmt.Fprintf(stdout, "request_id: %s\nstate: %s\n", result.RequestID, result.State)
	}
	switch result.State {
	case "pending":
		return exitPending
	case "denied":
		return exitDenied
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	flags := newFlagSet("status", stderr)
	asJSON := flags.Bool("json", false, "print the response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets status: exactly one request_id is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets status: %v\n", err)
		return exitUsageError
	}
	result, raw, err := newClient(base).GetRequest(context.Background(), signer, positional[0])
	if err != nil {
		reportError(stderr, "agent-secrets status", err)
		return 1
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	fmt.Fprintf(stdout, "state: %s\n", result.State)
	if result.GrantID != nil {
		fmt.Fprintf(stdout, "grant_id: %s\n", *result.GrantID)
	}
	if result.Decision != nil {
		fmt.Fprintf(stdout, "decided_by: %s\n", result.Decision.By)
	}
	return 0
}

// ---------------------------------------------------------------------------
// cancel
// ---------------------------------------------------------------------------

func cmdCancel(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("cancel", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets cancel: exactly one request_id is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets cancel: %v\n", err)
		return exitUsageError
	}
	if err := newClient(base).CancelRequest(context.Background(), signer, positional[0]); err != nil {
		reportError(stderr, "agent-secrets cancel", err)
		return 1
	}
	fmt.Fprintln(stdout, "cancelled")
	return 0
}

// ---------------------------------------------------------------------------
// revoke
// ---------------------------------------------------------------------------

func cmdRevoke(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("revoke", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets revoke: exactly one grant_id is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets revoke: %v\n", err)
		return exitUsageError
	}
	if err := newClient(base).RevokeGrant(context.Background(), signer, positional[0]); err != nil {
		reportError(stderr, "agent-secrets revoke", err)
		return 1
	}
	fmt.Fprintln(stdout, "revoked")
	return 0
}

// ---------------------------------------------------------------------------
// self
// ---------------------------------------------------------------------------

// cmdSelf is self, and whoami as self --json; name is which, for its help and its messages.
func cmdSelf(name string, args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets %s: unexpected argument %q\n", name, positional[0])
		return exitUsageError
	}
	flags := newFlagSet(name, stderr)
	asJSON := flags.Bool("json", false, "print the response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	base, _, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", name, err)
		return exitUsageError
	}
	result, raw, err := newClient(base).Self(context.Background(), signer)
	if err != nil {
		reportError(stderr, "agent-secrets "+name, err)
		return 1
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	operator := ""
	if result.Operator != nil {
		operator = *result.Operator
	}
	fmt.Fprintf(stdout, "enrollment_id: %s\nkind: %s\noperator: %s\nlease_expires_at: %s\n",
		result.EnrollmentID, result.Kind, operator, result.LeaseExpiresAt.Format(time.RFC3339))
	for _, g := range result.Grants {
		fmt.Fprintf(stdout, "grant: %s (request %s, expires %s)\n", g.GrantID, g.RequestID, g.ExpiresAt.Format(time.RFC3339))
	}
	return 0
}

// ---------------------------------------------------------------------------
// NAME... -- <command> [args...]
// ---------------------------------------------------------------------------

// cmdExec implements the default "agent-secrets NAME... [--reason TEXT]
// [--wait DURATION] -- <command> [args...]" form: it requests the named secrets, waits out a
// pending decision, and syscall.Execs the command with the granted values in its environment.
// Nothing runs when no NAME is given, and nothing runs while any requested secret is still
// pending or was denied — a denial of any single name denies the whole request (requests.Machine
// aggregates the decision), so this never partially execs with some secrets missing.
func cmdExec(args []string, stdout, stderr io.Writer) int {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
	before, command := args[:sep], args[sep+1:]
	if len(command) == 0 {
		fmt.Fprintln(stderr, "agent-secrets: a command is required after --")
		return exitUsageError
	}

	flagArgs, names := splitArgs(before, map[string]bool{"reason": true, "wait": true})
	flags := newFlagSet("", stderr)
	reason := flags.String("reason", "", "why these secrets are needed")
	wait := flags.Duration("wait", 30*time.Minute, "how long to wait for a pending request")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(names) == 0 {
		fmt.Fprintln(stderr, "agent-secrets: at least one secret NAME is required")
		return exitUsageError
	}

	base, _, signer, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return exitUsageError
	}
	c := newClient(base)
	ctx := context.Background()
	result, _, err := c.CreateRequest(ctx, signer, names, *reason, os.Getenv("OMP_SESSION_ID"))
	if err != nil {
		reportError(stderr, "agent-secrets", err)
		return 1
	}

	state, grantID, requestID := result.State, result.GrantID, result.RequestID
	if state == "pending" {
		reportPending(stderr, requestID, *result.RecordID, *wait)
		deadline := time.Now().Add(*wait)
		backoff := 2 * time.Second
		for state == "pending" {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			time.Sleep(minDuration(backoff, remaining))
			status, _, err := c.GetRequest(ctx, signer, requestID)
			if err != nil {
				reportError(stderr, "agent-secrets", err)
				return 1
			}
			state, grantID = status.State, status.GrantID
			backoff = nextBackoff(backoff)
		}
	}
	switch state {
	case "granted":
	case "pending":
		fmt.Fprintf(stderr, "agent-secrets: request %s is still waiting for approval; nothing was run. Check it with: agent-secrets status %s\n", requestID, requestID)
		return exitPending
	case "denied":
		fmt.Fprintf(stderr, "agent-secrets: request %s was denied\n", requestID)
		return exitDenied
	case "cancelled", "expired":
		fmt.Fprintf(stderr, "agent-secrets: request %s was %s\n", requestID, state)
		return 1
	default:
		fmt.Fprintf(stderr, "agent-secrets: request %s is in unexpected state %q\n", requestID, state)
		return 1
	}
	if grantID == nil {
		fmt.Fprintf(stderr, "agent-secrets: request %s was granted but its grant is no longer live; request it again\n", requestID)
		return 1
	}

	values, err := c.GrantValues(ctx, signer, *grantID)
	if err != nil {
		reportError(stderr, "agent-secrets", err)
		return 1
	}
	var missing []string
	for _, name := range names {
		if _, ok := values.Values[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "agent-secrets: %s not released (proxy-delivery or otherwise unavailable)\n", strings.Join(missing, ", "))
		return 1
	}

	path, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return 1
	}
	if err := syscall.Exec(path, command, buildChildEnv(os.Environ(), values.Values)); err != nil {
		fmt.Fprintf(stderr, "agent-secrets: exec %s: %v\n", command[0], err)
		return 1
	}
	return 0 // unreachable: syscall.Exec replaces this process on success
}

// reportPending says, once, before the exec form's wait, that a person must decide the request
// and where: the Dispatch page of its credential record (the broker names one for every pending
// request) under approveURL when that is set, else the Inbox's Credential requests section.
func reportPending(stderr io.Writer, requestID, recordID string, wait time.Duration) {
	fmt.Fprintf(stderr, "agent-secrets: request %s is waiting for approval; waiting up to %s\n", requestID, wait)
	if base := approveURL(); base != "" {
		fmt.Fprintf(stderr, "agent-secrets: approve or deny it at %s/credentials/%s\n", base, recordID)
		return
	}
	fmt.Fprintln(stderr, "agent-secrets: approve or deny it under Credential requests in the Dispatch Inbox")
}

// sessionIdentityVars are the AGENT_SECRETS_* variables the exec form's child keeps: the broker's
// URL and the helper socket or key dir that say which session this is. The child is the same
// session — the helper signs for every descendant of the registered root, and a box's key never
// leaves the box — so an `agent-secrets` call it makes (a skill that nests a request, gh's token
// helper under `agent-secrets NAME -- omp`) must reach the broker as that session instead of
// failing with "AGENT_SECRETS_URL is required". Every other AGENT_SECRETS_* variable (a wait
// bound, an approve URL) configures this one invocation and is dropped.
var sessionIdentityVars = map[string]bool{
	"AGENT_SECRETS_URL":         true,
	"AGENT_SECRETS_HELPER_SOCK": true,
	"AGENT_SECRETS_KEY_DIR":     true,
}

// buildChildEnv is the exec form's child environment: the inherited environment minus every
// AGENT_SECRETS_* variable but sessionIdentityVars and, for every released secret name, minus its
// inherited entry too, before that name's granted value is appended. Without that second strip,
// a duplicate key from the inherited environment could shadow the broker-released value under an
// execve implementation that keeps the first occurrence of a repeated key rather than the last.
func buildChildEnv(environ []string, values map[string]string) []string {
	envp := make([]string, 0, len(environ)+len(values))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "AGENT_SECRETS_") && !sessionIdentityVars[key] {
			continue
		}
		if _, released := values[key]; released {
			continue
		}
		envp = append(envp, e)
	}
	for name, value := range values {
		envp = append(envp, name+"="+value)
	}
	return envp
}

// Command agent-secrets is the box/pod-side client for the AGENTC-833 secrets broker: it
// enrolls a runtime, requests and reads back secret grants, and — in its most common shape —
// requests one or more secrets and execs a command with them in its environment.
//
//	agent-secrets keygen --out <dir>
//	agent-secrets enroll --launcher-token-file <path> --kind box|host --runtime-id <id> --operator <login> --thumbprint <tp> [--approver-issue <KEY>] [--session-id <id>]
//	agent-secrets enroll --launcher-token-file <path> --kind pod --runtime-id <pod uid> --pod-token-file <path> --approver-issue <KEY> --thumbprint <tp> [--session-id <id>]
//	agent-secrets unenroll --launcher-token-file <path> --enrollment <id>
//	agent-secrets launcher login --operator <login> --host <name> [--service <name>] --out <path>
//	agent-secrets renew
//	agent-secrets request NAME... [--reason TEXT] [--issue KEY] [--json]
//	agent-secrets status <request_id> [--json]
//	agent-secrets cancel <request_id>
//	agent-secrets revoke <grant_id>
//	agent-secrets self [--json]
//	agent-secrets NAME... [--reason TEXT] [--issue KEY] [--wait DURATION] -- <command> [args...]
//
// Environment: AGENT_SECRETS_URL (the broker's base URL) and AGENT_SECRETS_KEY_DIR (holds
// key.pem and enrollment, the two files keygen and enroll write and every session-authenticated
// subcommand reads).
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

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// Exit codes for the "request" and NAME...-- <command> forms: 75 (EX_TEMPFAIL) and 77
// (EX_NOPERM) follow BSD sysexits.h, chosen so a caller can tell "still waiting" from "refused"
// without parsing stderr.
const (
	exitUsageError = 2
	exitPending    = 75
	exitDenied     = 77
)

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
	case "keygen":
		return cmdKeygen(args[1:], stdout, stderr)
	case "enroll":
		return cmdEnroll(args[1:], stdout, stderr)
	case "unenroll":
		return cmdUnenroll(args[1:], stdout, stderr)
	case "launcher":
		return cmdLauncher(args[1:], stdout, stderr)
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
		return cmdSelf(args[1:], stdout, stderr)
	default:
		return cmdExec(args, stdout, stderr)
	}
}

func usage() string {
	return `usage:
  agent-secrets keygen --out <dir>
  agent-secrets enroll --launcher-token-file <path> --kind box|host --runtime-id <id> --operator <login> --thumbprint <tp> [--approver-issue <KEY>] [--session-id <id>]
  agent-secrets enroll --launcher-token-file <path> --kind pod --runtime-id <pod uid> --pod-token-file <path> --approver-issue <KEY> --thumbprint <tp> [--session-id <id>]
  agent-secrets unenroll --launcher-token-file <path> --enrollment <id>
  agent-secrets launcher login --operator <login> --host <name> [--service <name>] --out <path>
  agent-secrets renew
  agent-secrets request NAME... [--reason TEXT] [--issue KEY] [--json]
  agent-secrets status <request_id> [--json]
  agent-secrets cancel <request_id>
  agent-secrets revoke <grant_id>
  agent-secrets self [--json]
  agent-secrets NAME... [--reason TEXT] [--issue KEY] [--wait DURATION] -- <command> [args...]
`
}

func exitUsage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return exitUsageError
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

// sessionContext resolves the broker URL, enrollment id, and signing key every session-
// authenticated subcommand (renew, request, status, cancel, revoke, self, and the exec form)
// needs, from AGENT_SECRETS_URL and the key.pem/enrollment files under AGENT_SECRETS_KEY_DIR.
func sessionContext() (base, enrollmentID string, key *ecdsa.PrivateKey, err error) {
	base = strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		return "", "", nil, errors.New("AGENT_SECRETS_URL is required")
	}
	dir := os.Getenv("AGENT_SECRETS_KEY_DIR")
	if dir == "" {
		return "", "", nil, errors.New("AGENT_SECRETS_KEY_DIR is required")
	}
	if key, err = loadKey(filepath.Join(dir, "key.pem")); err != nil {
		return "", "", nil, err
	}
	if enrollmentID, err = readTrimmed(filepath.Join(dir, "enrollment")); err != nil {
		return "", "", nil, err
	}
	if enrollmentID == "" {
		return "", "", nil, fmt.Errorf("%s: empty", filepath.Join(dir, "enrollment"))
	}
	return base, enrollmentID, key, nil
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
	flags := flag.NewFlagSet("agent-secrets keygen", flag.ContinueOnError)
	flags.SetOutput(stderr)
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
	if err := os.WriteFile(filepath.Join(*out, "key.pem"), pemBytes, 0o600); err != nil {
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
// enroll / unenroll (launcher side, bearer launcher token)
// ---------------------------------------------------------------------------

func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{
		"launcher-token-file": true, "kind": true, "runtime-id": true, "operator": true,
		"thumbprint": true, "approver-issue": true, "session-id": true, "pod-token-file": true,
	})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets enroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := flag.NewFlagSet("agent-secrets enroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	launcherTokenFile := flags.String("launcher-token-file", "", "path to the launcher bearer token")
	kind := flags.String("kind", "", `"box", "host" or "pod"`)
	runtimeID := flags.String("runtime-id", "", "this runtime's stable id")
	operator := flags.String("operator", "", "the operator's GitHub login")
	thumbprint := flags.String("thumbprint", "", "the signing key's RFC 7638 thumbprint")
	approverIssue := flags.String("approver-issue", "", "approve requests through this issue's assignee instead of the operator")
	sessionID := flags.String("session-id", "", "the Envoy session id to wake on a decision")
	podTokenFile := flags.String("pod-token-file", "", "path to the pod's projected service-account token")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if *launcherTokenFile == "" || *kind == "" || *runtimeID == "" || *thumbprint == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: --launcher-token-file, --kind, --runtime-id and --thumbprint are required")
		return exitUsageError
	}
	switch *kind {
	case "box", "host":
		if *operator == "" || *podTokenFile != "" {
			fmt.Fprintf(stderr, "agent-secrets enroll: --kind %s needs --operator and takes no --pod-token-file\n", *kind)
			return exitUsageError
		}
	case "pod":
		// A pod has no operator: its service launcher enrolls it with its projected token, and an
		// issue's assignee approves its requests.
		if *operator != "" || *podTokenFile == "" || *approverIssue == "" {
			fmt.Fprintln(stderr, "agent-secrets enroll: --kind pod needs --pod-token-file and --approver-issue and takes no --operator")
			return exitUsageError
		}
	default:
		fmt.Fprintln(stderr, `agent-secrets enroll: --kind must be "box", "host" or "pod"`)
		return exitUsageError
	}
	base := strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: AGENT_SECRETS_URL is required")
		return exitUsageError
	}
	dir := os.Getenv("AGENT_SECRETS_KEY_DIR")
	if dir == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: AGENT_SECRETS_KEY_DIR is required")
		return exitUsageError
	}
	token, err := readTrimmed(*launcherTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	body := EnrollBody{Kind: *kind, RuntimeID: *runtimeID, Thumbprint: *thumbprint, Approver: approverBody{Kind: "operator"}}
	if *operator != "" {
		body.Operator = operator
	}
	if *approverIssue != "" {
		body.Approver = approverBody{Kind: "issue_assignee", Issue: approverIssue}
	}
	if *sessionID != "" {
		body.SessionID = sessionID
	}
	if *podTokenFile != "" {
		podToken, err := readTrimmed(*podTokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
			return 1
		}
		body.PodToken = &podToken
	}
	result, err := newClient(base).CreateEnrollment(context.Background(), token, body)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(dir, "enrollment"), []byte(result.EnrollmentID+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, result.EnrollmentID)
	return 0
}

func cmdUnenroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"launcher-token-file": true, "enrollment": true})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets unenroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := flag.NewFlagSet("agent-secrets unenroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	launcherTokenFile := flags.String("launcher-token-file", "", "path to the launcher bearer token")
	enrollmentID := flags.String("enrollment", "", "the enrollment id to revoke")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if *launcherTokenFile == "" || *enrollmentID == "" {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --launcher-token-file and --enrollment are required")
		return exitUsageError
	}
	base := strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		fmt.Fprintln(stderr, "agent-secrets unenroll: AGENT_SECRETS_URL is required")
		return exitUsageError
	}
	token, err := readTrimmed(*launcherTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %v\n", err)
		return 1
	}
	if err := newClient(base).DeleteEnrollment(context.Background(), token, *enrollmentID); err != nil {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %v\n", err)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// launcher login
// ---------------------------------------------------------------------------

func cmdLauncher(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "login" {
		fmt.Fprintln(stderr, `agent-secrets launcher: only "login" is supported`)
		return exitUsageError
	}
	flagArgs, positional := splitArgs(args[1:], map[string]bool{"operator": true, "host": true, "service": true, "out": true})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets launcher login: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := flag.NewFlagSet("agent-secrets launcher login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	operator := flags.String("operator", "", "the operator who approves this launcher's issuance")
	host := flags.String("host", "", "this launcher's host name")
	service := flags.String("service", "", `mint a service credential (e.g. "legion-daemon") instead of a personal one`)
	out := flags.String("out", "", "path to write the launcher token")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if *operator == "" || *host == "" || *out == "" {
		fmt.Fprintln(stderr, "agent-secrets launcher login: --operator, --host and --out are required")
		return exitUsageError
	}
	base := strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		fmt.Fprintln(stderr, "agent-secrets launcher login: AGENT_SECRETS_URL is required")
		return exitUsageError
	}
	var servicePtr *string
	if *service != "" {
		servicePtr = service
	}

	c := newClient(base)
	ctx := context.Background()
	pendingID, code, err := c.RequestLauncherCredential(ctx, *operator, *host, servicePtr)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets launcher login: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "confirmation code: %s\napprove the Dispatch ask on %s's standing secrets issue only if it shows this code\n", code, *operator)
	backoff := 2 * time.Second
	for {
		state, token, credentialID, err := c.ReadLauncherCredential(ctx, pendingID)
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets launcher login: %v\n", err)
			return 1
		}
		switch state {
		case "issued":
			if token == "" {
				// The one-time token was already handed to another reader of this pending id:
				// whoever holds it, it is not this process, so this login did not succeed. There
				// is no self-service way to revoke a launcher credential today; naming it is the
				// most this CLI can do, and an operator has to act on it by hand.
				if credentialID != "" {
					fmt.Fprintf(stderr, "agent-secrets launcher login: a launcher credential was already issued for this request by someone else (credential id %s); this login did NOT succeed and nothing was written. Contact an operator to investigate and revoke launcher credential %s before retrying.\n", credentialID, credentialID)
				} else {
					fmt.Fprintln(stderr, "agent-secrets launcher login: a launcher credential was already issued for this request by someone else; this login did NOT succeed and nothing was written. Contact an operator to investigate before retrying.")
				}
				return 1
			}
			if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
				fmt.Fprintf(stderr, "agent-secrets launcher login: %v\n", err)
				return 1
			}
			if err := os.WriteFile(*out, []byte(token+"\n"), 0o600); err != nil {
				fmt.Fprintf(stderr, "agent-secrets launcher login: %v\n", err)
				return 1
			}
			return 0
		case "denied":
			fmt.Fprintln(stderr, "agent-secrets launcher login: request was denied")
			return 1
		case "expired":
			fmt.Fprintln(stderr, "agent-secrets launcher login: request expired before it was answered")
			return 1
		}
		time.Sleep(backoff)
		backoff = nextBackoff(backoff)
	}
}

// ---------------------------------------------------------------------------
// renew
// ---------------------------------------------------------------------------

func cmdRenew(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if len(positional) > 0 || len(flagArgs) > 0 {
		fmt.Fprintln(stderr, "agent-secrets renew: no arguments are accepted")
		return exitUsageError
	}
	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets renew: %v\n", err)
		return exitUsageError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := newClient(base)
	backoff := 2 * time.Second
	for {
		expires, err := c.RenewEnrollment(ctx, key, enrollmentID)
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
	flagArgs, names := splitArgs(args, map[string]bool{"reason": true, "issue": true})
	flags := flag.NewFlagSet("agent-secrets request", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reason := flags.String("reason", "", "why these secrets are needed")
	issue := flags.String("issue", "", "the Dispatch issue to open the ask on")
	asJSON := flags.Bool("json", false, "print the response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(names) == 0 {
		fmt.Fprintln(stderr, "agent-secrets request: at least one secret NAME is required")
		return exitUsageError
	}
	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets request: %v\n", err)
		return exitUsageError
	}
	result, raw, err := newClient(base).CreateRequest(context.Background(), key, enrollmentID, names, *reason, *issue, "")
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets request: %v\n", err)
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
	flags := flag.NewFlagSet("agent-secrets status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print the response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets status: exactly one request_id is required")
		return exitUsageError
	}
	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets status: %v\n", err)
		return exitUsageError
	}
	result, raw, err := newClient(base).GetRequest(context.Background(), key, enrollmentID, positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets status: %v\n", err)
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
	if len(flagArgs) > 0 || len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets cancel: exactly one request_id is required")
		return exitUsageError
	}
	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets cancel: %v\n", err)
		return exitUsageError
	}
	if err := newClient(base).CancelRequest(context.Background(), key, enrollmentID, positional[0]); err != nil {
		fmt.Fprintf(stderr, "agent-secrets cancel: %v\n", err)
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
	if len(flagArgs) > 0 || len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets revoke: exactly one grant_id is required")
		return exitUsageError
	}
	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets revoke: %v\n", err)
		return exitUsageError
	}
	if err := newClient(base).RevokeGrant(context.Background(), key, enrollmentID, positional[0]); err != nil {
		fmt.Fprintf(stderr, "agent-secrets revoke: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "revoked")
	return 0
}

// ---------------------------------------------------------------------------
// self
// ---------------------------------------------------------------------------

func cmdSelf(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets self: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := flag.NewFlagSet("agent-secrets self", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print the response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets self: %v\n", err)
		return exitUsageError
	}
	result, raw, err := newClient(base).Self(context.Background(), key, enrollmentID)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets self: %v\n", err)
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

// cmdExec implements the default "agent-secrets NAME... [--reason TEXT] [--issue KEY]
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

	flagArgs, names := splitArgs(before, map[string]bool{"reason": true, "issue": true, "wait": true})
	flags := flag.NewFlagSet("agent-secrets", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reason := flags.String("reason", "", "why these secrets are needed")
	issue := flags.String("issue", "", "the Dispatch issue to open the ask on")
	wait := flags.Duration("wait", 30*time.Minute, "how long to wait for a pending request")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(names) == 0 {
		fmt.Fprintln(stderr, "agent-secrets: at least one secret NAME is required")
		return exitUsageError
	}

	base, enrollmentID, key, err := sessionContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return exitUsageError
	}
	c := newClient(base)
	ctx := context.Background()
	result, _, err := c.CreateRequest(ctx, key, enrollmentID, names, *reason, *issue, "")
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return 1
	}

	state, grantID, requestID := result.State, result.GrantID, result.RequestID
	var detail *string
	if state == "pending" {
		deadline := time.Now().Add(*wait)
		backoff := 2 * time.Second
		for state == "pending" {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			time.Sleep(minDuration(backoff, remaining))
			status, _, err := c.GetRequest(ctx, key, enrollmentID, requestID)
			if err != nil {
				fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
				return 1
			}
			state, grantID, detail = status.State, status.GrantID, status.Detail
			backoff = nextBackoff(backoff)
		}
	}
	if state != "pending" && state != "granted" && detail == nil {
		if status, _, err := c.GetRequest(ctx, key, enrollmentID, requestID); err == nil {
			detail = status.Detail
		}
	}
	why := "no reason recorded"
	if detail != nil {
		why = *detail
	}
	switch state {
	case "granted":
	case "pending":
		fmt.Fprintf(stderr, "agent-secrets: request %s is still waiting for approval; nothing was run. Check it with: agent-secrets status %s\n", requestID, requestID)
		return exitPending
	case "denied":
		fmt.Fprintf(stderr, "agent-secrets: request %s was denied: %s\n", requestID, why)
		return exitDenied
	case "cancelled", "expired":
		fmt.Fprintf(stderr, "agent-secrets: request %s was %s: %s\n", requestID, state, why)
		return 1
	default:
		fmt.Fprintf(stderr, "agent-secrets: request %s is in unexpected state %q\n", requestID, state)
		return 1
	}
	if grantID == nil {
		fmt.Fprintf(stderr, "agent-secrets: request %s was granted but its grant is no longer live; request it again\n", requestID)
		return 1
	}

	values, err := c.GrantValues(ctx, key, enrollmentID, *grantID)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
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

// buildChildEnv is the exec form's child environment: the inherited environment with every
// AGENT_SECRETS_* variable stripped (this CLI's own configuration is never the child's business)
// and, for every released secret name, its inherited entry ALSO stripped before that name's
// granted value is appended. Without that second strip, a duplicate key from the inherited
// environment could shadow the broker-released value under an execve implementation that keeps
// the first occurrence of a repeated key rather than the last.
func buildChildEnv(environ []string, values map[string]string) []string {
	envp := make([]string, 0, len(environ)+len(values))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "AGENT_SECRETS_") {
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

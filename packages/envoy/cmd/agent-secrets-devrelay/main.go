// Command agent-secrets-devrelay is the local dev stack's stand-in for Dispatch's
// credential-request relay, so the credential-request broker
// (packages/envoy/cmd/broker) can be decided by hand without a Dispatch deployment: it calls the
// broker's UI routes with the UI bearer token and the approving human's login, the same request
// Dispatch's server sends when a signed-in human clicks Approve or Deny.
//
//	agent-secrets-devrelay approve --record <id> --login <login> --broker <url> --ui-token <token>
//	agent-secrets-devrelay deny    --record <id> --login <login> --broker <url> --ui-token <token>
//	agent-secrets-devrelay machine-approve --code XXXX-XXXX --login <login> --broker <url> --ui-token <token> [--deny]
//
// Environment defaults: AGENT_SECRETS_URL, AGENT_SECRETS_UI_TOKEN, AGENT_SECRETS_APPROVER. The
// login is whatever the broker should record as the decider: Dispatch sends its session's own
// login, and the broker refuses any login that is not the record's approver.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const exitUsageError = 2

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
	case "approve":
		return cmdDecide(true, args[1:], stdout, stderr)
	case "deny":
		return cmdDecide(false, args[1:], stdout, stderr)
	case "machine-approve":
		return cmdMachineApprove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-secrets-devrelay: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
}

func usage() string {
	return `usage:
  agent-secrets-devrelay approve --record <id> --login <login> --broker <url> --ui-token <token>
  agent-secrets-devrelay deny    --record <id> --login <login> --broker <url> --ui-token <token>
  agent-secrets-devrelay machine-approve --code XXXX-XXXX --login <login> --broker <url> --ui-token <token> [--deny]

Environment defaults: AGENT_SECRETS_URL, AGENT_SECRETS_UI_TOKEN, AGENT_SECRETS_APPROVER.
`
}

func exitUsage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return exitUsageError
}

// commonFlags is every subcommand's shared connection and approver configuration.
type commonFlags struct {
	broker  string
	uiToken string
	login   string
}

func registerCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.broker, "broker", os.Getenv("AGENT_SECRETS_URL"), "broker base URL (default $AGENT_SECRETS_URL)")
	fs.StringVar(&c.uiToken, "ui-token", os.Getenv("AGENT_SECRETS_UI_TOKEN"), "UI bearer token (default $AGENT_SECRETS_UI_TOKEN)")
	fs.StringVar(&c.login, "login", os.Getenv("AGENT_SECRETS_APPROVER"), "the deciding human's login (default $AGENT_SECRETS_APPROVER)")
	return c
}

func (c *commonFlags) validate() error {
	if c.broker == "" {
		return errors.New("--broker (or AGENT_SECRETS_URL) is required")
	}
	if c.uiToken == "" {
		return errors.New("--ui-token (or AGENT_SECRETS_UI_TOKEN) is required")
	}
	if c.login == "" {
		return errors.New("--login (or AGENT_SECRETS_APPROVER) is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// UI-bearer HTTP client
// ---------------------------------------------------------------------------

type uiClient struct {
	base  string
	token string
	http  *http.Client
}

func newUIClient(base, token string) *uiClient {
	return &uiClient{base: base, http: &http.Client{Timeout: 30 * time.Second}, token: token}
}

// apiError is the broker's {"code":...,"error":...} error envelope, plus the HTTP status it came
// with.
type apiError struct {
	Status int
	Code   string
	Reason string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Reason, e.Status)
}

// do sends a UI-bearer-authenticated request and, on a 2xx response, returns its raw body; any
// other status decodes the broker's error envelope into an *apiError.
func (c *uiClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var envelope struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		return nil, &apiError{Status: resp.StatusCode, Code: envelope.Code, Reason: envelope.Error}
	}
	return raw, nil
}

// writeVerbatim prints raw (a broker response body) followed by a newline, for terminal
// readability.
func writeVerbatim(w io.Writer, raw []byte) {
	w.Write(raw)
	fmt.Fprintln(w)
}

// ---------------------------------------------------------------------------
// approve / deny
// ---------------------------------------------------------------------------

func cmdDecide(approve bool, args []string, stdout, stderr io.Writer) int {
	name := "deny"
	if approve {
		name = "approve"
	}
	fs := flag.NewFlagSet("agent-secrets-devrelay "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	recordID := fs.String("record", "", "credential-request record id (required)")
	c := registerCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage(err)
	}
	if *recordID == "" {
		fmt.Fprintf(stderr, "agent-secrets-devrelay %s: --record is required\n", name)
		return 1
	}
	if err := c.validate(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devrelay %s: %v\n", name, err)
		return 1
	}
	decideRaw, err := newUIClient(c.broker, c.uiToken).do(context.Background(), http.MethodPost,
		"/v1/credential-requests/"+url.PathEscape(*recordID)+"/"+name, map[string]any{"approver": c.login})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devrelay %s: %v\n", name, err)
		return 1
	}
	writeVerbatim(stdout, decideRaw)
	return 0
}

// ---------------------------------------------------------------------------
// machine-approve
// ---------------------------------------------------------------------------

// cmdMachineApprove resolves a pending machine login by its typed code, as Dispatch's
// machine-login page does, then decides that record with the same code: the code is the only
// thing that selects a machine login (ruling 13).
func cmdMachineApprove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-secrets-devrelay machine-approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	code := fs.String("code", "", "the machine login's confirmation code, XXXX-XXXX (required)")
	deny := fs.Bool("deny", false, "deny the machine login instead of approving it")
	c := registerCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage(err)
	}
	if *code == "" {
		fmt.Fprintln(stderr, "agent-secrets-devrelay machine-approve: --code is required")
		return 1
	}
	if err := c.validate(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devrelay machine-approve: %v\n", err)
		return 1
	}
	client := newUIClient(c.broker, c.uiToken)
	ctx := context.Background()
	lookupRaw, err := client.do(ctx, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": *code})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devrelay machine-approve: lookup: %v\n", err)
		return 1
	}
	var rec struct {
		RecordID string `json:"record_id"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(lookupRaw, &rec); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devrelay machine-approve: decode lookup: %v\n", err)
		return 1
	}
	if rec.State != "pending" {
		fmt.Fprintf(stderr, "agent-secrets-devrelay machine-approve: code %s names a machine login that is %s, not pending\n", *code, rec.State)
		return 1
	}
	action := "approve"
	if *deny {
		action = "deny"
	}
	decideRaw, err := client.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(rec.RecordID)+"/"+action,
		map[string]any{"approver": c.login, "code": *code})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devrelay machine-approve: %v\n", err)
		return 1
	}
	writeVerbatim(stdout, decideRaw)
	return 0
}

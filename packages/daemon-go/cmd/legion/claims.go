package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/config"
)

// claimsUsage names every subcommand of `legion claims`.
const claimsUsage = "usage: legion claims spawn|deliver|suspend|resume|stop|list [flags]"

// claimsCommands is the operator's hand on the daemon's claims, one subcommand per operator route
// (internal/api/operator.go).
var claimsCommands = map[string]command{
	"spawn":   runClaimsSpawn,
	"deliver": runClaimsDeliver,
	"suspend": claimRequest("suspend"),
	"resume":  claimRequest("resume"),
	"stop":    claimRequest("stop"),
	"list":    runClaimsList,
}

func runClaims(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, claimsUsage)
		return 2
	}
	sub, ok := claimsCommands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "legion claims: unknown subcommand %q\n%s\n", args[0], claimsUsage)
		return 2
	}
	return sub(ctx, args[1:], stdout, stderr)
}

// claimsCall is one `legion claims` subcommand: the flags every one of them takes — where the
// daemon is, the file holding the operator bearer, and whether to print the daemon's answer as it
// came — and the streams it reports on.
//
// The bearer is only ever a file's contents: no flag takes it as a value, so it is in no argv, and
// once read it goes into the request's Authorization header and nowhere else — no line this
// command prints carries it.
type claimsCall struct {
	name           string
	flags          *flag.FlagSet
	tokenFile      *string
	configPath     *string
	port           *int
	asJSON         *bool
	stdout, stderr io.Writer
}

func newClaimsCall(sub string, stdout, stderr io.Writer) *claimsCall {
	flags := newFlags("claims "+sub, stderr)
	return &claimsCall{
		name:       "legion claims " + sub,
		flags:      flags,
		tokenFile:  flags.String("operator-token-file", "", "file holding the operator bearer the daemon's operator_token_file names (required)"),
		configPath: flags.String("config", "", "path to legion.yaml (default "+defaultConfigPath+")"),
		port:       flags.Int("port", 0, "port to reach, overriding the configured one"),
		asJSON:     flags.Bool("json", false, "print the daemon's answer as it served it"),
		stdout:     stdout,
		stderr:     stderr,
	}
}

// parse reads args, then refuses a word that is not a flag and any required flag left out or
// empty, by name — each a usage error, before anything is read or sent.
func (c *claimsCall) parse(args []string, required ...string) bool {
	if err := c.flags.Parse(args); err != nil {
		return false
	}
	if c.flags.NArg() > 0 {
		fmt.Fprintf(c.stderr, "%s: unexpected argument %q\n", c.name, c.flags.Arg(0))
		return false
	}
	for _, name := range append([]string{"operator-token-file"}, required...) {
		if c.flags.Lookup(name).Value.String() == "" {
			fmt.Fprintf(c.stderr, "%s: --%s is required\n", c.name, name)
			return false
		}
	}
	return true
}

// operator is a daemon's operator routes, and the bearer that opens them.
type operator struct {
	base   string
	bearer string
}

// connect reads the bearer by the rules every secret pointer is read by — an unreadable or blank
// file is refused naming its path, never its contents — and finds the daemon the way `legion
// state` does.
func (c *claimsCall) connect() (operator, bool) {
	bearer, err := config.ReadSecretPointer("--operator-token-file", *c.tokenFile)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return operator{}, false
	}
	address, err := stateAddress(*c.configPath, *c.port)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return operator{}, false
	}
	return operator{base: "http://" + address + "/legion/v1/operator/claims", bearer: bearer}, true
}

// send makes one operator request and reports its answer: a 2xx is printed by print — or, under
// --json, written as the daemon served it — and anything else is the daemon's refusal, printed
// with its status and sentence, and the command fails.
//
// The request carries no deadline of its own. What it asks for waits on the runtime — a suspend
// or stop waits out the worker's stop grace, a resume that and a launch — and every step is
// bounded by the daemon's configuration, which this command may not have read; a signal to this
// process ends the wait.
func (c *claimsCall) send(ctx context.Context, op operator, method, path string, body any, print func(io.Writer, []byte) error) int {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			fmt.Fprintf(c.stderr, "%s: encode the request: %v\n", c.name, err)
			return 1
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, op.base+path, reader)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return 1
	}
	request.Header.Set("Authorization", "Bearer "+op.bearer)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return 1
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: read the answer to %s %s: %v\n", c.name, method, request.URL, err)
		return 1
	}

	if response.StatusCode/100 != 2 {
		fmt.Fprintf(c.stderr, "%s: %s\n", c.name, refusal(response.StatusCode, answer))
		return 1
	}
	if *c.asJSON {
		if _, err := c.stdout.Write(answer); err != nil {
			fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
			return 1
		}
		return 0
	}
	if err := print(c.stdout, answer); err != nil {
		fmt.Fprintf(c.stderr, "%s: read the answer %s served: %v\n", c.name, request.URL, err)
		return 1
	}
	return 0
}

// refusal is an answer outside 2xx as the operator reads it: the status, and the sentence the
// daemon's `{"error"}` body carries — or the body itself when it carries none, so nothing the
// daemon said is lost.
func refusal(status int, body []byte) string {
	refused := legionclaim.Refusal{Status: status}
	if err := json.Unmarshal(body, &refused); err != nil || refused.Message == "" {
		refused.Message = strings.TrimSpace(string(body))
	}
	answered := fmt.Sprintf("the daemon answered %d %s", refused.Status, http.StatusText(refused.Status))
	if refused.Message == "" {
		return answered
	}
	return answered + ": " + refused.Message
}

// decodeAnswer reads a 2xx answer into into, refusing a member the struct does not have: a daemon
// whose wire this binary does not speak says so rather than printing half of it.
func decodeAnswer(answer []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(answer))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// printClaim is the one line a claim prints as: token, role, issue, state, generation.
func printClaim(w io.Writer, answer []byte) error {
	var c api.OperatorClaim
	if err := decodeAnswer(answer, &c); err != nil {
		return err
	}
	writeClaimLine(w, c)
	return nil
}

func printClaims(w io.Writer, answer []byte) error {
	var list api.OperatorClaims
	if err := decodeAnswer(answer, &list); err != nil {
		return err
	}
	for _, c := range list.Claims {
		writeClaimLine(w, c)
	}
	return nil
}

func writeClaimLine(w io.Writer, c api.OperatorClaim) {
	fmt.Fprintf(w, "%s %s %s %s %d\n", c.Token, c.Role, c.Issue, c.State, c.Generation)
}

// runClaimsSpawn creates the claim on --role of --issue, in the tree --tree roots, and launches
// it; --prompt-file is the role prompt its agent's system prompt starts with, sent as the file
// holds it, and --task its first delivery, sent once the agent is ready.
func runClaimsSpawn(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newClaimsCall("spawn", stdout, stderr)
	tree := c.flags.String("tree", "", "issue key of the tree's root (required)")
	issue := c.flags.String("issue", "", "issue key the claim is on (required)")
	role := c.flags.String("role", "", "role the claim holds on the issue (required)")
	promptFile := c.flags.String("prompt-file", "", "file holding the role prompt (required)")
	task := c.flags.String("task", "", "the claim's first task, sent once its agent is ready")
	if !c.parse(args, "tree", "issue", "role", "prompt-file") {
		return 2
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	prompt, err := os.ReadFile(*promptFile)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: read the role prompt: %v\n", c.name, err)
		return 1
	}
	return c.send(ctx, op, http.MethodPost, "", api.SpawnRequest{
		Tree: *tree, Issue: *issue, Role: legionclaim.Role(*role), Prompt: string(prompt), Task: *task,
	}, printClaim)
}

// runClaimsDeliver gives --claim the task --task.
func runClaimsDeliver(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newClaimsCall("deliver", stdout, stderr)
	token := c.flags.String("claim", "", "the claim's token (required)")
	task := c.flags.String("task", "", "the task (required)")
	if !c.parse(args, "claim", "task") {
		return 2
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/deliver", api.DeliverRequest{Task: *task}, printClaim)
}

// claimRequest is a subcommand that posts request, with no body, to --claim's route.
func claimRequest(request string) command {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		c := newClaimsCall(request, stdout, stderr)
		token := c.flags.String("claim", "", "the claim's token (required)")
		if !c.parse(args, "claim") {
			return 2
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/"+request, nil, printClaim)
	}
}

// runClaimsList prints every claim the daemon supervises, one line each, in token order.
func runClaimsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newClaimsCall("list", stdout, stderr)
	if !c.parse(args) {
		return 2
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	return c.send(ctx, op, http.MethodGet, "", nil, printClaims)
}

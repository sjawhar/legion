package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
)

// claimsUsage names every subcommand of `legion claims`.
const claimsUsage = "usage: legion claims spawn|deliver|suspend|resume|stop|close|list [flags]"

// claimsCommands is the operator's hand on the daemon's claims, one subcommand per operator route
// (internal/api/operator.go). close is the tree's close, through its root claim, for a tree no
// workflow issue backs.
var claimsCommands = map[string]command{
	"spawn":   runClaimsSpawn,
	"deliver": runClaimsDeliver,
	"suspend": claimRequest("suspend"),
	"resume":  claimRequest("resume"),
	"stop":    claimRequest("stop"),
	"close":   claimRequest("close"),
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

// claimsRoute is the root of the daemon's operator claim routes.
const claimsRoute = "/legion/v1/operator/claims"

// claimsCall is one `legion claims` subcommand: an operator command whose bearer is required, and
// which can print the daemon's answer as it came.
type claimsCall struct {
	*operatorCall
	asJSON *bool
}

func newClaimsCall(sub string, stdout, stderr io.Writer) *claimsCall {
	c := newOperatorCall("claims "+sub, "file holding the operator bearer the daemon's operator_token_file names (required)", stdout, stderr)
	return &claimsCall{operatorCall: c, asJSON: c.flags.Bool("json", false, "print the daemon's answer as it served it")}
}

// parse is operatorCall.parse with --operator-token-file among the required flags.
func (c *claimsCall) parse(args []string, required ...string) bool {
	return c.operatorCall.parse(args, append([]string{"operator-token-file"}, required...)...)
}

// send is operatorCall.send on an operator claim route: a 2xx is printed by print, or under --json
// written as the daemon served it.
func (c *claimsCall) send(ctx context.Context, op operator, method, path string, body any, print func(io.Writer, []byte) error) int {
	if *c.asJSON {
		print = writeAnswer
	}
	return c.operatorCall.send(ctx, op, method, claimsRoute+path, body, print)
}

// writeAnswer writes a 2xx answer as the daemon served it.
func writeAnswer(w io.Writer, answer []byte) error {
	_, err := w.Write(answer)
	return err
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
// it. --prompt-file is an optional operator override; omitted, the daemon composes the role's
// shared and Go-specific parts. --task is its first delivery, sent once the agent is ready.
func runClaimsSpawn(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newClaimsCall("spawn", stdout, stderr)
	tree := c.flags.String("tree", "", "issue key of the tree's root (required)")
	issue := c.flags.String("issue", "", "issue key the claim is on (required)")
	role := c.flags.String("role", "", "role the claim holds on the issue (required)")
	promptFile := c.flags.String("prompt-file", "", "file holding an operator role-prompt override")
	task := c.flags.String("task", "", "the claim's first task, sent once its agent is ready")
	if !c.parse(args, "tree", "issue", "role") {
		return 2
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	prompt := ""
	if *promptFile != "" {
		body, err := os.ReadFile(*promptFile)
		if err != nil {
			fmt.Fprintf(c.stderr, "%s: read the role prompt: %v\n", c.name, err)
			return 1
		}
		prompt = string(body)
	}
	return c.send(ctx, op, http.MethodPost, "", api.SpawnRequest{
		Tree: *tree, Issue: *issue, Role: legionclaim.Role(*role), Prompt: prompt, Task: *task,
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

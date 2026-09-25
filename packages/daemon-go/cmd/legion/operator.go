package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/config"
)

// operatorCall is one command that reaches the daemon over the operator's bearer — each `legion
// claims` subcommand, and `legion status <issue> <status>` from an operator shell: the flags every
// one of them takes — where the daemon is and the file holding the bearer — and the streams it
// reports on.
//
// The bearer is only ever a file's contents: no flag takes it as a value, so it is in no argv, and
// once read it goes into the request's Authorization header and nowhere else — no line these
// commands print carries it.
type operatorCall struct {
	name           string
	flags          *flag.FlagSet
	tokenFile      *string
	configPath     *string
	port           *int
	stdout, stderr io.Writer
}

// newOperatorCall is the `legion <sub>` command's flag set with the three flags every operator
// command takes; tokenUsage is how --operator-token-file reads in its help.
func newOperatorCall(sub, tokenUsage string, stdout, stderr io.Writer) *operatorCall {
	flags := newFlags(sub, stderr)
	return &operatorCall{
		name:       "legion " + sub,
		flags:      flags,
		tokenFile:  flags.String("operator-token-file", "", tokenUsage),
		configPath: flags.String("config", "", "path to legion.yaml (default "+defaultConfigPath+")"),
		port:       flags.Int("port", 0, "port to reach, overriding the configured one"),
		stdout:     stdout,
		stderr:     stderr,
	}
}

// parse reads args, then refuses a word that is not a flag and any required flag left out or
// empty, by name — each a usage error, before anything is read or sent.
func (c *operatorCall) parse(args []string, required ...string) bool {
	if err := c.flags.Parse(args); err != nil {
		return false
	}
	if c.flags.NArg() > 0 {
		fmt.Fprintf(c.stderr, "%s: unexpected argument %q\n", c.name, c.flags.Arg(0))
		return false
	}
	for _, name := range required {
		if c.flags.Lookup(name).Value.String() == "" {
			fmt.Fprintf(c.stderr, "%s: --%s is required\n", c.name, name)
			return false
		}
	}
	return true
}

// connect reads the bearer --operator-token-file names, when the command was given one, by
// config.ReadOperatorTokenFile's rules — a file that cannot be read, holds nothing, or that its
// group or others can read is refused naming its path, never its contents — and finds the daemon
// the way `legion state` does (daemonAddress).
func (c *operatorCall) connect() (operator, bool) {
	var bearer string
	if *c.tokenFile != "" {
		read, err := config.ReadOperatorTokenFile("--operator-token-file", *c.tokenFile)
		if err != nil {
			fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
			return operator{}, false
		}
		bearer = read
	}
	address, err := daemonAddress(*c.configPath, *c.port)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return operator{}, false
	}
	return operator{base: address, bearer: bearer}, true
}

// send makes one request of the daemon and reports its answer: a 2xx is handed to print, and
// anything else is the daemon's refusal, printed with its status and sentence, and the command
// fails.
//
// The request carries no deadline of its own. What it asks for may wait on the runtime — a suspend
// or stop waits out the worker's stop grace, a resume that and a launch — and every step is
// bounded by the daemon's configuration, which this command may not have read; a signal to this
// process ends the wait.
func (c *operatorCall) send(ctx context.Context, op operator, method, path string, body any, print func(io.Writer, []byte) error) int {
	status, answer, err := op.do(ctx, method, path, body)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return 1
	}
	if status/100 != 2 {
		fmt.Fprintf(c.stderr, "%s: %s\n", c.name, refusal(status, answer))
		return 1
	}
	if err := print(c.stdout, answer); err != nil {
		fmt.Fprintf(c.stderr, "%s: read the answer %s%s served: %v\n", c.name, op.base, path, err)
		return 1
	}
	return 0
}

// operator is a daemon's API root, and the operator bearer that opens its operator routes — ""
// for a request that authenticates otherwise (a grant in its body).
type operator struct {
	base   string
	bearer string
}

// do is one request to the daemon: body as JSON when it is not nil, the bearer as its
// Authorization when op carries one. It answers the status and the body the daemon served; only a
// request that got no answer is an error.
func (op operator) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, op.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if op.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+op.bearer)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read the answer to %s %s: %w", method, request.URL, err)
	}
	return response.StatusCode, answer, nil
}

// daemonAddress is the API root of the daemon a command reaches: the one LEGION_DAEMON_URL names —
// the daemon names it on every pane, and a pane has no legion.yaml — unless --config or --port
// says otherwise, and otherwise the configured bind and port (stateAddress).
func daemonAddress(configPath string, port int) (string, error) {
	if _, pane := os.LookupEnv("LEGION_DAEMON_URL"); pane && configPath == "" && port == 0 {
		return daemonURL(), nil
	}
	address, err := stateAddress(configPath, port)
	if err != nil {
		return "", err
	}
	return "http://" + address, nil
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

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
	"os/exec"
	"strings"
)

const (
	slackBotTokenName = "LEGION_SLACK_BOT_TOKEN"
	defaultSlackAPI   = "https://slack.com/api"
)

// slackCommands is `legion slack`'s public bot surface. It obtains the bot token only through
// agent-secrets, so its caller never reads or carries the token.
var slackCommands = map[string]command{
	"post":  runSlackPost,
	"read":  runSlackRead,
	"reply": runSlackReply,
}

func runSlack(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "slack", slackCommands, args, stdout, stderr)
}

func runSlackPost(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, channel, text := slackMessageFlags("slack post", "usage: legion slack post --channel <channel> --text <text>", stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if !validSlackMessageFlags(flags, *channel, *text) {
		return 2
	}
	return runWithSlackToken(ctx, "post", args, "Post a message as the Legion Slack app", stdout, stderr, func(token string) error {
		return postSlackMessage(ctx, token, *channel, *text, "", stdout)
	})
}

func runSlackReply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, channel, text := slackMessageFlags("slack reply", "usage: legion slack reply --channel <channel> --thread-ts <timestamp> --text <text>", stderr)
	threadTS := flags.String("thread-ts", "", "parent message timestamp (required)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if !validSlackMessageFlags(flags, *channel, *text) || *threadTS == "" {
		if *threadTS == "" {
			fmt.Fprintln(stderr, "legion slack reply: --thread-ts is required")
		}
		return 2
	}
	return runWithSlackToken(ctx, "reply", args, "Reply in a Legion Slack thread", stdout, stderr, func(token string) error {
		return postSlackMessage(ctx, token, *channel, *text, *threadTS, stdout)
	})
}

func runSlackRead(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("slack read", "usage: legion slack read --channel <channel> --thread-ts <timestamp>", stderr)
	channel := flags.String("channel", "", "Slack channel ID (required)")
	threadTS := flags.String("thread-ts", "", "parent message timestamp (required)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 || *channel == "" || *threadTS == "" {
		if *channel == "" {
			fmt.Fprintln(stderr, "legion slack read: --channel is required")
		}
		if *threadTS == "" {
			fmt.Fprintln(stderr, "legion slack read: --thread-ts is required")
		}
		if flags.NArg() > 0 {
			flags.Usage()
		}
		return 2
	}
	return runWithSlackToken(ctx, "read", args, "Read a Legion Slack thread", stdout, stderr, func(token string) error {
		return readSlackThread(ctx, token, *channel, *threadTS, stdout)
	})
}

func slackMessageFlags(name, usage string, stderr io.Writer) (*flag.FlagSet, *string, *string) {
	flags := newFlags(name, usage, stderr)
	channel := flags.String("channel", "", "Slack channel ID (required)")
	text := flags.String("text", "", "message text (required)")
	return flags, channel, text
}

func validSlackMessageFlags(flags *flag.FlagSet, channel, text string) bool {
	if flags.NArg() > 0 || channel == "" || text == "" {
		if channel == "" {
			fmt.Fprintln(flags.Output(), "--channel is required")
		}
		if text == "" {
			fmt.Fprintln(flags.Output(), "--text is required")
		}
		if flags.NArg() > 0 {
			flags.Usage()
		}
		return false
	}
	return true
}

// runWithSlackToken restarts this command beneath agent-secrets when it is not already the broker's
// child. agent-secrets grants the token to that child process alone; this parent cannot inspect it.
func runWithSlackToken(ctx context.Context, subcommand string, args []string, reason string, stdout, stderr io.Writer, operation func(string) error) int {
	token, child := os.LookupEnv(slackBotTokenName)
	if !child {
		return runSlackThroughAgentSecrets(ctx, subcommand, args, reason, stdout, stderr)
	}
	if token == "" {
		fmt.Fprintf(stderr, "legion slack: agent-secrets started this command without %s\n", slackBotTokenName)
		return 1
	}
	if err := operation(token); err != nil {
		fmt.Fprintf(stderr, "legion slack: %v\n", err)
		return 1
	}
	return 0
}

func runSlackThroughAgentSecrets(ctx context.Context, subcommand string, args []string, reason string, stdout, stderr io.Writer) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "legion slack: resolve the legion executable for agent-secrets: %v\n", err)
		return 1
	}
	childArgs := make([]string, 0, len(args)+7)
	childArgs = append(childArgs, slackBotTokenName, "--reason", reason, "--", executable, "slack", subcommand)
	childArgs = append(childArgs, args...)
	command := exec.CommandContext(ctx, "agent-secrets", childArgs...)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return exited.ExitCode()
		}
		fmt.Fprintf(stderr, "legion slack: run agent-secrets: %v\n", err)
		return 1
	}
	return 0
}

type slackPostResult struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

type slackThreadResult struct {
	OK       bool              `json:"ok"`
	Messages []json.RawMessage `json:"messages"`
	HasMore  bool              `json:"has_more,omitempty"`
}

func postSlackMessage(ctx context.Context, token, channel, text, threadTS string, stdout io.Writer) error {
	payload := map[string]string{"channel": channel, "text": text}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode chat.postMessage request: %w", err)
	}
	response, err := slackRequest(ctx, token, "chat.postMessage", http.MethodPost, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result slackPostResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode chat.postMessage response: %w", err)
	}
	if !result.OK || result.Channel == "" || result.TS == "" {
		return errors.New("Slack chat.postMessage reported failure")
	}
	return json.NewEncoder(stdout).Encode(struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}{Channel: result.Channel, TS: result.TS})
}

func readSlackThread(ctx context.Context, token, channel, threadTS string, stdout io.Writer) error {
	response, err := slackRequest(ctx, token, "conversations.replies", http.MethodGet, nil, func(endpoint *url.URL) {
		query := endpoint.Query()
		query.Set("channel", channel)
		query.Set("ts", threadTS)
		endpoint.RawQuery = query.Encode()
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result slackThreadResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode conversations.replies response: %w", err)
	}
	if !result.OK {
		return errors.New("Slack conversations.replies reported failure")
	}
	return json.NewEncoder(stdout).Encode(result)
}

// slackRequest adds the token only to Slack's Authorization header. It never includes an API
// response in an error, because a hostile or proxying endpoint could reflect that header.
func slackRequest(ctx context.Context, token, method, requestMethod string, body io.Reader, mutate ...func(*url.URL)) (*http.Response, error) {
	endpoint, err := slackEndpoint(method)
	if err != nil {
		return nil, err
	}
	for _, change := range mutate {
		change(endpoint)
	}
	request, err := http.NewRequestWithContext(ctx, requestMethod, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("make Slack %s request: %w", method, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Slack %s: %w", method, err)
	}
	if response.StatusCode/100 != 2 {
		response.Body.Close()
		return nil, fmt.Errorf("Slack %s returned HTTP %d", method, response.StatusCode)
	}
	return response, nil
}

func slackEndpoint(method string) (*url.URL, error) {
	base := os.Getenv("LEGION_SLACK_API_URL")
	if base == "" {
		base = defaultSlackAPI
	}
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("LEGION_SLACK_API_URL must be an absolute URL")
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, fmt.Errorf("LEGION_SLACK_API_URL must use http or https")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + method
	endpoint.RawQuery = ""
	return endpoint, nil
}

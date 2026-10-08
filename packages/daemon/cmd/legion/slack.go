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
	// slackReportingChannelsEnv is LEGION_SLACK_REPORTING_CHANNELS, which specs.SpawnSpec sets
	// (packages/daemon/internal/daemon/specs.go) from this deployment's configured
	// `slack.reporting_channels`: the comma-separated channel IDs legion slack post and reply may
	// address. A report thread's text reaches a session as untrusted data; enforcing the allowlist
	// here, in code, means a hostile report cannot talk a session out of it the way it could a
	// prompt rule.
	slackReportingChannelsEnv = "LEGION_SLACK_REPORTING_CHANNELS"
)

// slackAPIBaseURL and slackHTTPClient are the only seams a test may redirect: unexported package
// variables a test file sets directly, in-process. Neither is an environment variable or a flag —
// in release code or otherwise — so an environment- or prompt-directed run can never retarget
// where the brokered bot token is sent; production always pins requests to defaultSlackAPI.
var (
	slackAPIBaseURL = defaultSlackAPI
	slackHTTPClient = http.DefaultClient
)

// slackCommands is `legion slack`'s public bot surface. This process obtains the bot token only
// through agent-secrets, restarting itself as the broker's child to redeem it, so this specific
// invocation never reads or carries it. That is not a security boundary: LEGION_SLACK_BOT_TOKEN is
// a shared agent-tier broker secret every enrolled session can redeem the same way, so any of them
// can read or post in every channel the bot is in. The channel allowlist below (allowedSlackChannel)
// guards against this process's own mistakes, not against a session that chooses to misuse the
// token directly; the human design-gate review before work starts is where that trust is placed.
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
	if !allowedSlackChannel(*channel) {
		fmt.Fprintf(stderr, "legion slack post: %s is not a configured Slack reporting channel\n", *channel)
		return 1
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
	if !allowedSlackChannel(*channel) {
		fmt.Fprintf(stderr, "legion slack reply: %s is not a configured Slack reporting channel\n", *channel)
		return 1
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

// allowedSlackChannel reports whether channel is one of this deployment's configured Slack
// reporting channels (LEGION_SLACK_REPORTING_CHANNELS, which specs.SpawnSpec sets). legion slack
// post and reply are the only posters in this slice — the allowlist does not yet cover a deferred
// worker round-trip — so this is their one enforcement point: a report thread's text can ask a
// session to post wherever it likes, but the session cannot comply with anything this refuses.
func allowedSlackChannel(channel string) bool {
	for _, configured := range strings.Split(os.Getenv(slackReportingChannelsEnv), ",") {
		if configured != "" && configured == channel {
			return true
		}
	}
	return false
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

// slackRepliesPage is one page of conversations.replies: readSlackThread keeps paging while
// HasMore is true, following ResponseMetadata.NextCursor, and returns every message across every
// page so a long thread never loses context to the first page alone.
type slackRepliesPage struct {
	OK               bool              `json:"ok"`
	Messages         []json.RawMessage `json:"messages"`
	HasMore          bool              `json:"has_more,omitempty"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata,omitempty"`
}

// slackThreadResult is readSlackThread's complete answer: every message of the thread, gathered
// across every page conversations.replies paginated, up to slackMaxReplyPages pages or
// slackMaxReplyMessages messages, whichever comes first. Truncated is true when a cap stopped the
// read before Slack ran out of pages, so a caller knows the thread may hold more than it was shown.
type slackThreadResult struct {
	OK        bool              `json:"ok"`
	Messages  []json.RawMessage `json:"messages"`
	Truncated bool              `json:"truncated,omitempty"`
}

// slackMaxReplyPages and slackMaxReplyMessages bound how much of a thread readSlackThread will
// fetch: an unusually long thread, or one whose pagination loops a cursor back on itself, must
// not read forever. Reaching either cap ends the read with Truncated set; a cursor Slack repeats,
// which conversations.replies should never do, is refused outright rather than paged again.
const (
	slackMaxReplyPages    = 20
	slackMaxReplyMessages = 1000
)

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
	var messages []json.RawMessage
	cursor := ""
	seenCursors := map[string]bool{}
	truncated := false
	for pages := 0; ; pages++ {
		if pages >= slackMaxReplyPages {
			truncated = true
			break
		}
		response, err := slackRequest(ctx, token, "conversations.replies", http.MethodGet, nil, func(endpoint *url.URL) {
			query := endpoint.Query()
			query.Set("channel", channel)
			query.Set("ts", threadTS)
			if cursor != "" {
				query.Set("cursor", cursor)
			}
			endpoint.RawQuery = query.Encode()
		})
		if err != nil {
			return err
		}
		var page slackRepliesPage
		decodeErr := json.NewDecoder(response.Body).Decode(&page)
		response.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode conversations.replies response: %w", decodeErr)
		}
		if !page.OK {
			return errors.New("Slack conversations.replies reported failure")
		}
		messages = append(messages, page.Messages...)
		if len(messages) >= slackMaxReplyMessages {
			messages = messages[:slackMaxReplyMessages]
			truncated = true
			break
		}
		next := page.ResponseMetadata.NextCursor
		if !page.HasMore || next == "" {
			break
		}
		if next == cursor || seenCursors[next] {
			return fmt.Errorf("Slack conversations.replies repeated cursor %q", next)
		}
		seenCursors[next] = true
		cursor = next
	}
	return json.NewEncoder(stdout).Encode(slackThreadResult{OK: true, Messages: messages, Truncated: truncated})
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
	response, err := slackHTTPClient.Do(request)
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
	endpoint, err := url.Parse(slackAPIBaseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("slack API base URL %q is invalid", slackAPIBaseURL)
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, fmt.Errorf("slack API base URL %q must use http or https", slackAPIBaseURL)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + method
	endpoint.RawQuery = ""
	return endpoint, nil
}

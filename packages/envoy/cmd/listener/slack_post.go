package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/slackapi"
)

const (
	// slackBotTokenVariable is the Slack bot token (xoxb-…) POST /v1/slack/post posts with. It
	// stays in the listener: an agent reaches Slack only through the route.
	slackBotTokenVariable = "ENVOY_SLACK_BOT_TOKEN"
	// slackAPIURLVariable moves Slack's Web API (slackapi.DefaultBaseURL), for another Slack
	// deployment or a stand-in.
	slackAPIURLVariable = "ENVOY_SLACK_API_URL"
	// slackPostTimeout bounds the route's call to Slack inside the Envoy client's 5 s request
	// timeout, so the caller hears the listener's answer, never its own timeout, and does not post
	// the reply a second time while Slack is still taking the first.
	slackPostTimeout = 4 * time.Second
)

// slackPoster posts a reply into a Slack thread: a slackapi.Client, or a stand-in in a test.
type slackPoster interface {
	PostMessage(ctx context.Context, m slackapi.Message) (slackapi.Posted, error)
}

// slackPosterFromEnv is the listener's Slack client, from ENVOY_SLACK_BOT_TOKEN and
// ENVOY_SLACK_API_URL: nil, and so a route that answers 503, with no token; a refusal for an API
// URL that is not an absolute http(s) URL, or one set without a token, which would otherwise turn
// into a silent no-post.
func slackPosterFromEnv(getenv func(string) string) (slackPoster, error) {
	token := strings.TrimSpace(getenv(slackBotTokenVariable))
	base := strings.TrimSpace(getenv(slackAPIURLVariable))
	if token == "" {
		if base != "" {
			return nil, fmt.Errorf("%s is set without %s: the listener posts to Slack only with its bot token", slackAPIURLVariable, slackBotTokenVariable)
		}
		return nil, nil
	}
	if base == "" {
		base = slackapi.DefaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute http or https URL, such as %s (got %q)", slackAPIURLVariable, slackapi.DefaultBaseURL, base)
	}
	return slackapi.New(base, token, &http.Client{}), nil
}

// slackPostRequest is POST /v1/slack/post's body: the channel's id, the root ts of the thread the
// reply goes in, and its text.
type slackPostRequest struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
}

// slackPostResponse is the route's answer: where the reply was posted, and its ts.
type slackPostResponse struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	TS       string `json:"ts"`
}

// slackPostHandler is POST /v1/slack/post: one reply posted into a Slack thread with chat.postMessage
// under the listener's bot token, behind the same apiAuth as the rest of /v1. With no token it
// answers 503 naming the variable; a missing field, an unknown one (a misspelled thread_ts would
// otherwise post at the channel's top level) or text past contracts.SlackPostTextMax UTF-16 units is
// a 400, before Slack is called. Slack's refusal is a 502 carrying Slack's code verbatim, and a
// Slack that does not answer within slackPostTimeout a 504 saying the reply may have been posted.
// Each post is logged with its channel, thread and ts, never its text.
func slackPostHandler(poster slackPoster, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if poster == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "Slack posting is off on this listener: set "+slackBotTokenVariable+" to the Slack app's bot token")
			return
		}
		var request slackPostRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		request.Channel = strings.TrimSpace(request.Channel)
		request.ThreadTS = strings.TrimSpace(request.ThreadTS)
		for _, field := range []struct{ name, value string }{
			{"channel", request.Channel}, {"thread_ts", request.ThreadTS}, {"text", strings.TrimSpace(request.Text)},
		} {
			if field.value == "" {
				writeJSONError(w, http.StatusBadRequest, field.name+" is required", field.name)
				return
			}
		}
		if length := len16(request.Text); length > contracts.SlackPostTextMax {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("text is %d characters over the %d-character limit (%d/%d)",
				length-contracts.SlackPostTextMax, contracts.SlackPostTextMax, length, contracts.SlackPostTextMax), "text")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), slackPostTimeout)
		defer cancel()
		posted, err := poster.PostMessage(ctx, slackapi.Message{Channel: request.Channel, ThreadTS: request.ThreadTS, Text: request.Text})
		attrs := []slog.Attr{slog.String("channel", request.Channel), slog.String("thread_ts", request.ThreadTS)}
		var refused *slackapi.Error
		switch {
		case errors.As(err, &refused):
			logger.Warn("slack post refused", append(attrs, slog.String("slack_error", refused.Code))...)
			writeJSONError(w, http.StatusBadGateway, err.Error())
		case errors.Is(err, slackapi.ErrUnanswered):
			logger.Warn("slack post unanswered", append(attrs, slog.String("error", err.Error()))...)
			writeJSONError(w, http.StatusGatewayTimeout, err.Error())
		case err != nil:
			logger.Warn("slack post failed", append(attrs, slog.String("error", err.Error()))...)
			writeJSONError(w, http.StatusBadGateway, err.Error())
		default:
			logger.Info("slack post", append(attrs, slog.String("ts", posted.TS))...)
			writeJSON(w, http.StatusOK, slackPostResponse{Channel: request.Channel, ThreadTS: request.ThreadTS, TS: posted.TS})
		}
	}
}

// len16 is text's length in UTF-16 units, the unit contracts' caps count in, as the tool's own
// maximum counts a JavaScript string.
func len16(text string) int {
	length := 0
	for _, r := range text {
		length += utf16.RuneLen(r)
	}
	return length
}

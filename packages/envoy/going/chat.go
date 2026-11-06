package going

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/redirects"
)

// The route HTTPClient posts to, held here alone: it is appended to the configured endpoint,
// which is the base URL of an OpenAI-shaped chat completions service (for an Azure AI Foundry
// resource, https://<resource>.services.ai.azure.com/openai/v1).
const chatCompletionsRoute = "/chat/completions"

const (
	// chatRequestTimeout bounds one Chat call, sign-in included. A whole run is bounded by
	// contracts.GoingRunWindow from the POST that started it, so a call still running past that
	// window could no longer reach the reader; the caller's own deadline cuts earlier whenever it
	// has one.
	chatRequestTimeout = contracts.GoingRunWindow
	// chatErrorBodyLimit bounds how much of a failed response is read for its error code.
	chatErrorBodyLimit = 64 << 10
	// chatStreamLimit bounds how many bytes of a streamed completion are read. An answer is
	// bounded at contracts.GoingAnswerWordMax words and a tool call carries one short search
	// query, so 4 MiB is far past any completion this package means, and a stream running past it
	// is a model service that will not stop rather than one the reader is waiting for.
	chatStreamLimit = 4 << 20
	// chatFrameLimit bounds one data: frame, since a stream is read frame by frame and a frame
	// without end would otherwise be buffered without end.
	chatFrameLimit = 1 << 20
)

// Client generates text for an answer. The answerer takes this interface rather than HTTPClient
// so a test may drive it directly, and so the one implementation's transport stays its own
// business.
type Client interface {
	// Chat streams one completion: onText is called with each content delta as it arrives, so a
	// caller may pass the answer on before the model has finished, and the tool calls the turn
	// ended with are returned (none when it ended with an answer).
	Chat(ctx context.Context, request ChatRequest, onText func(string) error) ([]ToolCall, error)
}

// ChatRequest is one turn's input: the conversation so far, and the tools the model may call
// instead of answering.
type ChatRequest struct {
	Messages []Message
	Tools    []Tool
}

// Message is one message of the conversation. A tool call's result is a message whose Role is
// tool and whose ToolCallID names the call it answers.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is one call the model made, with its arguments as the JSON text the model wrote:
// this package neither parses nor trusts them, the tool's own caller does.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Tool is one tool the model may call, with its arguments' JSON Schema as Parameters.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// TokenSource hands HTTPClient a bearer token for each call. It is declared here, with the one
// method, so this package forces no dependency on embed: *embed.EntraTokenSource satisfies it,
// and caches, so calling Token once per Chat costs nothing between exchanges.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// FileTokenSource is the bearer a file holds: the file's whole contents, with the whitespace at
// either end trimmed. It reads the file on every call rather than keeping what it read, because
// the token rotates under a running process - as the federated token embed.EntraTokenSource
// presents does, which the kubelet rewrites in place - and a source that read it once at boot
// would go on presenting a bearer the service has stopped taking.
type FileTokenSource struct {
	// Path is the file holding the bearer. It is never read at boot, so a file written after
	// the process starts refuses no boot.
	Path string
}

var _ TokenSource = FileTokenSource{}

// Token is what the file holds now. A file that cannot be read, and one holding nothing but
// whitespace, are both failures naming the path and never its contents.
func (s FileTokenSource) Token(context.Context) (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read the token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the token file %s is empty", s.Path)
	}
	return token, nil
}

// HTTPConfig configures an HTTPClient.
type HTTPConfig struct {
	// Place names what generates the text, as Identity opens with it: "azure" for a chat
	// deployment of an Azure AI Foundry resource, "endpoint" for any other OpenAI-shaped chat
	// completions service.
	Place string
	// Endpoint is the absolute base URL the route is appended to.
	Endpoint string
	// APIVersion is the api-version query value; empty sends none.
	APIVersion string
	// Model is the model each request names, the deployment's name on Azure.
	Model string
	// Tokens is the credential, each token sent as a bearer; required. It is a FileTokenSource
	// for a service whose bearer a file holds, and an *embed.EntraTokenSource for the keyless
	// Azure sign-in.
	Tokens TokenSource
	// HTTPClient replaces http.DefaultClient; a test injects one. It is never mutated: the
	// client this one calls on is a copy of it that refuses redirects (redirects.Refused).
	HTTPClient *http.Client
}

// HTTPClient calls an OpenAI-shaped chat completions service over plain net/http: one streaming
// POST per turn does not need a vendor SDK, and reading the stream frame by frame is what lets
// the answer reach the reader while the model is still writing it.
type HTTPClient struct {
	url      string
	identity string
	model    string
	tokens   TokenSource
	http     *http.Client
}

var _ Client = (*HTTPClient)(nil)

// NewHTTPClient checks cfg's shape and returns an HTTPClient; it makes no call and reads no token
// file, so a model service that is down at boot, or a token file not written yet, never refuses
// boot. It does not require https: the boot settings refuse a plain-http endpoint, and a test
// points this client at a local http server.
func NewHTTPClient(cfg HTTPConfig) (*HTTPClient, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("going: chat endpoint: %w", err)
	}
	if !endpoint.IsAbs() || endpoint.Host == "" {
		return nil, errors.New("going: chat endpoint must be an absolute URL with a host")
	}
	if endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("going: chat endpoint must carry no query or fragment: the route is appended to it")
	}
	if cfg.Model == "" {
		return nil, errors.New("going: chat needs a model")
	}
	if cfg.Tokens == nil {
		return nil, errors.New("going: chat needs a credential: a token source")
	}
	target := *endpoint
	target.Path = strings.TrimRight(endpoint.Path, "/") + chatCompletionsRoute
	target.RawPath = ""
	if cfg.APIVersion != "" {
		target.RawQuery = url.Values{"api-version": {cfg.APIVersion}}.Encode()
	}
	return &HTTPClient{
		url:      target.String(),
		identity: cfg.Place + ":" + strings.ToLower(endpoint.Host) + "/" + cfg.Model,
		model:    cfg.Model,
		tokens:   cfg.Tokens,
		http:     redirects.Refused(cmp.Or(cfg.HTTPClient, http.DefaultClient)),
	}, nil
}

// Identity is "<place>:<endpoint host>/<model>", as embed.AzureClient's is, for the one line the
// boot logs: the place belongs in it, so that line says which service of which resource answers
// questions here. It carries no credential.
func (c *HTTPClient) Identity() string { return c.identity }

// chatWireRequest is the body posted: the OpenAI chat completions shape, streamed. Tools and
// ToolChoice are sent only for a request that carries tools, since a service that takes no tools
// refuses an empty list.
type chatWireRequest struct {
	Model      string            `json:"model"`
	Messages   []chatWireMessage `json:"messages"`
	Stream     bool              `json:"stream"`
	Tools      []chatWireTool    `json:"tools,omitempty"`
	ToolChoice string            `json:"tool_choice,omitempty"`
}

type chatWireMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content,omitempty"`
	ToolCalls  []chatWireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

type chatWireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatWireFunction `json:"function"`
}

type chatWireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatWireTool struct {
	Type     string           `json:"type"`
	Function chatWireToolSpec `json:"function"`
}

type chatWireToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// chatWireFrame is one data: frame of the stream. A service sends choices empty on a frame that
// carries only usage, which is why every field is read defensively.
type chatWireFrame struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type chatWireError struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// chatBody renders request as the service's body.
func (c *HTTPClient) chatBody(request ChatRequest) chatWireRequest {
	body := chatWireRequest{Model: c.model, Stream: true, Messages: make([]chatWireMessage, len(request.Messages))}
	for i, message := range request.Messages {
		wire := chatWireMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		if len(message.ToolCalls) > 0 {
			wire.ToolCalls = make([]chatWireToolCall, len(message.ToolCalls))
			for j, call := range message.ToolCalls {
				wire.ToolCalls[j] = chatWireToolCall{
					ID:       call.ID,
					Type:     "function",
					Function: chatWireFunction{Name: call.Name, Arguments: call.Arguments},
				}
			}
		}
		body.Messages[i] = wire
	}
	if len(request.Tools) > 0 {
		body.Tools = make([]chatWireTool, len(request.Tools))
		for i, tool := range request.Tools {
			body.Tools[i] = chatWireTool{
				Type: "function",
				Function: chatWireToolSpec{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.Parameters,
				},
			}
		}
		body.ToolChoice = "auto"
	}
	return body
}

// Chat posts one streaming completion and reads its frames as they arrive: each content delta
// goes to onText before the next frame is read, so the answer reaches the reader while the model
// is still writing, and the tool calls a turn ended with are assembled from the frames that
// carried them. A failure names the HTTP status, the error code the service answered with, or the
// token file's path, and never the credential, the messages or the body.
func (c *HTTPClient) Chat(ctx context.Context, request ChatRequest, onText func(string) error) ([]ToolCall, error) {
	body, err := json.Marshal(c.chatBody(request))
	if err != nil {
		return nil, fmt.Errorf("going: encode chat request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, chatRequestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("going: build chat request: %w", err)
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("going: chat completions: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, chatStatusError(response)
	}
	return readChatStream(response, onText)
}

// token is the bearer one call presents, asked of the source on every call: a source hands back
// what its credential is now, which is what keeps a bearer that rotates under this process -
// the Azure sign-in's, a token file's - from going stale here.
func (c *HTTPClient) token(ctx context.Context) (string, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("going: chat sign-in: %w", err)
	}
	return token, nil
}

// readChatStream reads the data: frames of one completion, handing each content delta to onText
// as it arrives and assembling the turn's tool calls. [DONE] and a finish_reason both end the
// turn; the frames after either say nothing about this turn.
//
// Each frame is read from the scanner's own bytes rather than from a string of them: the sentinel
// is compared without a copy, and json.Unmarshal copies every string it decodes, so nothing the
// turn keeps aliases a buffer the next line overwrites.
func readChatStream(response *http.Response, onText func(string) error) ([]ToolCall, error) {
	// One byte past the cap is read, so a stream that fills the cap exactly is told apart from
	// one that runs past it, which is refused rather than read without end.
	limited := &io.LimitedReader{R: response.Body, N: chatStreamLimit + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 0, 4096), chatFrameLimit)
	var calls []ToolCall
	var indexes []int
	for scanner.Scan() {
		data, found := bytes.CutPrefix(bytes.TrimSpace(scanner.Bytes()), []byte("data:"))
		if !found {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if string(data) == "[DONE]" {
			return calls, nil
		}
		var frame chatWireFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return nil, fmt.Errorf("going: chat completions: HTTP %s, a frame that is not JSON", response.Status)
		}
		if len(frame.Choices) == 0 {
			continue
		}
		choice := frame.Choices[0]
		if choice.Delta.Content != "" && onText != nil {
			if err := onText(choice.Delta.Content); err != nil {
				return nil, err
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			at := slices.Index(indexes, delta.Index)
			if at < 0 {
				indexes = append(indexes, delta.Index)
				calls = append(calls, ToolCall{})
				at = len(calls) - 1
			}
			// A service repeats a call's id on the frames that continue it, so the first id is
			// the call's; its name and its arguments arrive in pieces and are joined.
			if calls[at].ID == "" {
				calls[at].ID = delta.ID
			}
			calls[at].Name += delta.Function.Name
			calls[at].Arguments += delta.Function.Arguments
		}
		if choice.FinishReason != "" {
			return calls, nil
		}
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("going: chat completions: stream exceeds %d bytes", chatStreamLimit)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("going: chat completions: read stream: %w", err)
	}
	return calls, nil
}

// chatStatusError describes a non-2xx answer by its status and the service's error code, reading
// at most chatErrorBodyLimit of the body for that code: neither says anything about the question.
func chatStatusError(response *http.Response) error {
	var decoded chatWireError
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatErrorBodyLimit))
	_ = json.Unmarshal(body, &decoded)
	if decoded.Error.Code != "" {
		return fmt.Errorf("going: chat completions: HTTP %s, error code %q", response.Status, decoded.Error.Code)
	}
	return fmt.Errorf("going: chat completions: HTTP %s", response.Status)
}

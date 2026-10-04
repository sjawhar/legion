// Package embed calls Cohere's embeddings API for Dispatch's meaning search: one vector per
// (kind, id) searchable unit, embedded after commit through embedqueue's durable retry queue, and
// one query vector per search request that asks for it.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Dimension is the embedding width every embeddings.embedding column and HNSW index
	// (0054_embeddings.up.sql) is sized for. Changing it means a new migration and a full
	// re-embed; this package does not detect a Dispatch already carrying vectors at another width.
	Dimension = 1536
	// Model is the Cohere embedding model Dispatch calls through Cohere's own API, on the
	// company's existing key - LEGION-386's "Which models" names Cohere, and embed-v4.0 is the
	// model that section's benchmark exercised through Cohere's direct API (not a Bedrock
	// permission) at this Dimension.
	Model = "embed-v4.0"
	// MaxBatchTexts is the most texts one call sends: Cohere's v2 embed endpoint accepts up to 96.
	MaxBatchTexts = 96

	defaultBaseURL = "https://api.cohere.com"
	defaultTimeout = 30 * time.Second
	// maxInputChars bounds one text's length before it is sent. Cohere's context is generous
	// (128k tokens) but an unbounded document would pay to embed far more than a search result
	// ever shows; this is a generous multiple of the 4,000-character snippet window search.go
	// already uses, not a tight limit.
	maxInputChars = 32000
)

// InputType is Cohere's asymmetric embedding mode: a stored document is embedded differently
// from the query that will later search for it.
type InputType string

const (
	InputDocument InputType = "search_document"
	InputQuery    InputType = "search_query"
)

// Embedder embeds text for meaning search. A nil Embedder (Dispatch configured without a Cohere
// key) means meaning search is off everywhere: search.go answers keyword-only and says so, and
// embedqueue.Run never starts.
type Embedder interface {
	// Embed returns one vector per text, in the same order, embedded as inputType. It returns one
	// error for the whole batch: Cohere's embed endpoint either embeds every text in a call or
	// answers an error, so there is no partial result to salvage from a failed call.
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error)
}

// Client calls Cohere's v2 embed endpoint directly.
type Client struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
}

// Option configures a Client beyond its API key.
type Option func(*Client)

// WithBaseURL overrides Cohere's API origin; a test points it at a fake server.
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(url, "/") }
}

// WithModel overrides the embedding model.
func WithModel(model string) Option {
	return func(c *Client) { c.model = model }
}

// WithHTTPClient overrides the HTTP client, for a test's timeout or transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New returns a Client. apiKey must be non-empty: a caller decides whether Dispatch runs meaning
// search at all (an empty COHERE_API_KEY) before ever constructing one, so an empty key reaching
// here is a programmer error, not a runtime condition - New panics rather than returning a Client
// that would fail every call.
func New(apiKey string, opts ...Option) *Client {
	if strings.TrimSpace(apiKey) == "" {
		panic("embed: apiKey is required")
	}
	c := &Client{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		model:   Model,
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

type embedRequest struct {
	Model           string    `json:"model"`
	Texts           []string  `json:"texts"`
	InputType       InputType `json:"input_type"`
	EmbeddingTypes  []string  `json:"embedding_types"`
	OutputDimension int       `json:"output_dimension"`
}

type embedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

type embedErrorResponse struct {
	Message string `json:"message"`
}

// Embed calls Cohere's v2 embed endpoint once for up to MaxBatchTexts texts; a caller with more
// batches it itself (embedqueue does, one batch at a time).
func (c *Client) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > MaxBatchTexts {
		return nil, fmt.Errorf("embed: %d texts exceeds the %d-text batch limit", len(texts), MaxBatchTexts)
	}
	bounded := make([]string, len(texts))
	for i, text := range texts {
		bounded[i] = truncateRunes(text, maxInputChars)
	}
	body, err := json.Marshal(embedRequest{
		Model:           c.model,
		Texts:           bounded,
		InputType:       inputType,
		EmbeddingTypes:  []string{"float"},
		OutputDimension: Dimension,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errBody embedErrorResponse
		_ = json.Unmarshal(payload, &errBody)
		message := errBody.Message
		if message == "" {
			message = string(payload)
		}
		return nil, fmt.Errorf("embed: cohere answered %d: %s", resp.StatusCode, message)
	}
	var decoded embedResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(decoded.Embeddings.Float) != len(texts) {
		return nil, fmt.Errorf("embed: cohere returned %d embeddings for %d texts", len(decoded.Embeddings.Float), len(texts))
	}
	return decoded.Embeddings.Float, nil
}

func truncateRunes(text string, maxRunes int) string {
	count := 0
	for i := range text {
		count++
		if count > maxRunes {
			return text[:i]
		}
	}
	return text
}

// Literal renders a vector as pgvector's text input form ("[0.1,0.2,...]"), the form a query
// parameter cast with ::vector accepts. Dispatch never scans a vector value back out of
// Postgres (search ranks by distance, never returns the vector itself), so this package carries
// no inverse of Literal.
func Literal(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', 8, 32))
	}
	b.WriteByte(']')
	return b.String()
}

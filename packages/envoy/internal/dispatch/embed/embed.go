// Package embed calls Cohere Embed v4 through AWS Bedrock for Dispatch's meaning search: one
// vector per (kind, id) searchable unit, embedded after commit through embedqueue's durable retry
// queue, and one query vector per search request that asks for it.
package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	// Dimension is the embedding width every embeddings.embedding column and HNSW index
	// (0071_embeddings_core.up.sql) is sized for. Changing it means a new migration and a full
	// re-embed; this package does not detect a Dispatch already carrying vectors at another width.
	Dimension = 1536
	// Model is the Bedrock model this package calls: Cohere Embed v4 through the US cross-region
	// inference profile - the exact model and region LEGION-386's benchmark measured
	// (searchbench-results.md, linear-parity-plan.md "Who we call") - reached through the AWS
	// SDK's default credential chain (Dispatch's task role in production, the devbox's instance
	// role locally) and bedrock:InvokeModel, never Cohere's own API or a shared API key
	// (LEGION-549 ask 649cdaf2: Sami ruled out a hand-seeded secret and any one-off admin apply).
	Model = "us.cohere.embed-v4:0"
	// MaxBatchTexts is the most texts one call sends: Cohere Embed v4's texts input accepts up to
	// 96, on Bedrock as through Cohere's own API.
	MaxBatchTexts = 96
	// maxInputChars bounds one text's length before it is sent. The model's context is generous
	// (128k tokens) but an unbounded document would pay to embed far more than a search result
	// ever shows; this is a generous multiple of the 4,000-character snippet window search.go
	// already uses, not a tight limit.
	maxInputChars = 32000
)

// EstimateTokens estimates how many tokens texts will cost one Bedrock InvokeModel call, at the
// same truncation this package applies before sending (maxInputChars, by rune count exactly as
// text.HeadRunes truncates) and the same rough characters-per-token ratio this PR's own "Bedrock
// capacity" measurement used (4 characters/token, a standard order-of-magnitude estimate for
// English prose - not Cohere's own tokenizer, which only Bedrock itself has). Good enough to
// pace a shared token-rate budget (embedqueue's reserveTokens); never exact.
func EstimateTokens(texts []string) int {
	const charsPerToken = 4
	total := 0
	for _, t := range texts {
		n := utf8.RuneCountInString(t)
		if n > maxInputChars {
			n = maxInputChars
		}
		total += n
	}
	return total / charsPerToken
}

// InputType is Cohere's asymmetric embedding mode: a stored document is embedded differently
// from the query that will later search for it.
type InputType string

const (
	InputDocument InputType = "search_document"
	InputQuery    InputType = "search_query"
)

// Embedder embeds text for meaning search. A nil Embedder (no AWS credentials or region reached
// Dispatch at boot) means meaning search is off everywhere: search.go answers keyword-only and
// says so, and embedqueue.Run never starts.
type Embedder interface {
	// Embed returns one vector per text, in the same order, embedded as inputType. It returns one
	// error for the whole batch: Bedrock either embeds every text in a call or answers an error,
	// so there is no partial result to salvage from a failed call.
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error)
}

// Client calls Cohere Embed v4 on AWS Bedrock (InvokeModel).
type Client struct {
	bedrock  *bedrockruntime.Client
	model    string
	endpoint string
}

// Option configures a Client beyond its AWS configuration.
type Option func(*Client)

// WithModel overrides the Bedrock model id; a test points it at a fake endpoint's model id.
func WithModel(model string) Option {
	return func(c *Client) { c.model = model }
}

// WithBaseEndpoint overrides Bedrock's regional endpoint; a test points it at a local server
// that stands in for Bedrock's InvokeModel API (the AWS SDK still signs every request against
// whatever credentials the Client's config carries, which a test's static, fake credentials
// satisfy - nothing before Bedrock's own side verifies the signature).
func WithBaseEndpoint(url string) Option {
	return func(c *Client) { c.endpoint = url }
}

// New loads the AWS SDK's default configuration (the environment, the shared config files, and
// the instance/task credential chain - nothing this package reads directly) and returns a Client.
// It returns an error if no region is configured - the one failure LoadDefaultConfig would
// otherwise accept silently and only fail on the first call - or if loading the configuration
// itself fails. Either is meaning search unavailable (LEGION-549's degraded mode): a caller logs
// it and runs with a nil Embedder, not a reason to refuse to boot - a CI job or a devbox with no
// AWS_REGION set must still start Dispatch and answer search, keyword-only.
func New(ctx context.Context, opts ...Option) (*Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(logging.LoggerFunc(files.LogSDK)))
	if err != nil {
		return nil, fmt.Errorf("embed: load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("embed: needs an AWS region: set AWS_REGION, or a region in the shared AWS config")
	}
	return NewWithConfig(cfg, opts...), nil
}

// NewWithConfig builds a Client directly from an already-loaded AWS configuration, skipping
// New's region check and credential-chain loading - a test supplies its own static, fake
// credentials and WithBaseEndpoint rather than touching the real AWS SDK default chain.
func NewWithConfig(cfg aws.Config, opts ...Option) *Client {
	c := &Client{model: Model}
	for _, opt := range opts {
		opt(c)
	}
	c.bedrock = bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		if c.endpoint != "" {
			o.BaseEndpoint = aws.String(c.endpoint)
		}
	})
	return c
}

type embedRequest struct {
	InputType       InputType `json:"input_type"`
	Texts           []string  `json:"texts"`
	EmbeddingTypes  []string  `json:"embedding_types"`
	OutputDimension int       `json:"output_dimension"`
}

// embedResponse is Bedrock's actual response shape, confirmed by a live call (2026-10-05): the
// vectors nest under embeddings.float regardless of how many embedding_types were requested -
// the AWS documentation's claim of a flat array for a single requested type does not match what
// Bedrock answers.
type embedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// Embed calls Bedrock once for up to MaxBatchTexts texts; a caller with more batches it itself
// (embedqueue does, one batch at a time).
func (c *Client) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > MaxBatchTexts {
		return nil, fmt.Errorf("embed: %d texts exceeds the %d-text batch limit", len(texts), MaxBatchTexts)
	}
	bounded := make([]string, len(texts))
	for i, t := range texts {
		bounded[i] = text.HeadRunes(t, maxInputChars)
	}
	body, err := json.Marshal(embedRequest{
		InputType:       inputType,
		Texts:           bounded,
		EmbeddingTypes:  []string{"float"},
		OutputDimension: Dimension,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	contentType := aws.String("application/json")
	out, err := c.bedrock.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(c.model),
		Body:        body,
		ContentType: contentType,
		Accept:      contentType,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: bedrock invoke model: %w", err)
	}
	var decoded embedResponse
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(decoded.Embeddings.Float) != len(texts) {
		return nil, fmt.Errorf("embed: bedrock returned %d embeddings for %d texts", len(decoded.Embeddings.Float), len(texts))
	}
	return decoded.Embeddings.Float, nil
}

// bedrockThrottleErrorCodes extends the AWS SDK's own retry.DefaultThrottleErrorCodes with two
// Bedrock-specific codes the SDK's generic set does not classify as throttles (confirmed against
// the pinned aws-sdk-go-v2's retry/standard.go: neither is in DefaultThrottleErrorCodes), on
// Main's decision (round 5 of LEGION-549's review): ServiceUnavailableException (Bedrock's own
// capacity temporarily unavailable, nothing to do with the text being embedded) and
// ModelNotReadyException (an on-demand model still scaling up - the AWS SDK's own retry-behavior
// guide recommends backing off on it) are both transient conditions about Bedrock's capacity,
// never evidence about a specific row's content, exactly like ThrottlingException already is.
var bedrockThrottleErrorCodes = func() map[string]struct{} {
	codes := make(map[string]struct{}, len(retry.DefaultThrottleErrorCodes)+2)
	for code := range retry.DefaultThrottleErrorCodes {
		codes[code] = struct{}{}
	}
	codes["ServiceUnavailableException"] = struct{}{}
	codes["ModelNotReadyException"] = struct{}{}
	return codes
}()

var bedrockThrottles = retry.IsErrorThrottles{retry.ThrottleErrorCode{Codes: bedrockThrottleErrorCodes}}

// IsThrottled reports whether err is Bedrock signaling "too many requests, back off" or one of
// the two closely related capacity conditions bedrockThrottleErrorCodes adds, rather than
// something wrong with the specific text that was being embedded. It uses the AWS SDK's own
// throttle-classification machinery (retry.IsErrorThrottles/ThrottleErrorCode - the same checker
// retry.DefaultThrottles uses, over an extended code set) instead of matching one named exception
// type by hand, so a related condition this package has not written a case for is still
// recognized correctly.
//
// embedqueue relies on this to decide what a failure means: a throttled batch is retried
// indefinitely (the provider will recover; the content was never the problem), while a non-
// throttled failure from this call is still infrastructure, not evidence about the row - only
// commitEmbedding's own failure (today, a non-finite vector) is specific enough to a row's
// content to ever count toward embedqueue's dead-letter threshold.
func IsThrottled(err error) bool {
	return bedrockThrottles.IsErrorThrottle(err) == aws.TrueTernary
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

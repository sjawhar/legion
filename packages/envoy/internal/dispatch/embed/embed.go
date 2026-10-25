// Package embed calls Cohere Embed v4 for Dispatch's meaning search, on AWS Bedrock
// (BedrockClient) or on Azure AI Foundry (AzureClient), one place per deployment: one vector per
// (kind, id) searchable unit, embedded after commit through embedqueue's durable retry queue, and
// one query vector per search request that asks for it.
package embed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	// Dimension is the embedding width every embeddings.embedding column and HNSW index
	// (0072_embeddings_core.up.sql) is sized for. Changing it means a new migration and a full
	// re-embed; this package does not detect a Dispatch already carrying vectors at another width.
	// Both places are asked for this width, so a vector from either fits the same column.
	Dimension = 1536
	// maxInputChars bounds one text's length before it is sent, by either client. The model's
	// context is generous (128k tokens) but an unbounded document would pay to embed far more
	// than a search result ever shows; this is a generous multiple of the 4,000-character snippet
	// window search.go already uses, not a tight limit. It bounds one text in runes
	// (text.HeadRunes), while DISPATCH_BODY_MAX in packages/contracts/src/dispatch-tools.ts caps a
	// comment, comment-edit and message body in UTF-16 units, which are never fewer than a text's
	// runes; so a body at that cap reaches the embedder whole only while DISPATCH_BODY_MAX <=
	// maxInputChars, and .github/scripts/check-body-embed-limit.sh fails the build when it does
	// not.
	maxInputChars = 32000
)

// EstimateTokens estimates how many tokens texts will cost one call to the model service, at the
// same truncation this package applies before sending (maxInputChars, by rune count exactly as
// text.HeadRunes truncates) and the same rough characters-per-token ratio the original "Bedrock
// capacity" measurement used (4 characters/token, a standard order-of-magnitude estimate for
// English prose - not Cohere's own tokenizer, which only the model service itself has). Good
// enough to pace a shared token-rate budget (embedqueue's reserveTokens); never exact.
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

// boundBatch checks one Embed call's texts against a place's batch limit and returns them each
// truncated to maxInputChars runes (text.HeadRunes), the form both clients send. No texts comes
// back as nil, nil: a caller answers that call with no vectors and no error, before any other
// check.
func boundBatch(texts []string, limit int) ([]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > limit {
		return nil, fmt.Errorf("embed: %d texts exceeds the %d-text batch limit", len(texts), limit)
	}
	bounded := make([]string, len(texts))
	for i, t := range texts {
		bounded[i] = text.HeadRunes(t, maxInputChars)
	}
	return bounded, nil
}

// InputType is Cohere's asymmetric embedding mode: a stored document is embedded differently
// from the query that will later search for it. Its values are Bedrock's names for the two
// modes; AzureClient maps them to Azure's own.
type InputType string

const (
	InputDocument InputType = "search_document"
	InputQuery    InputType = "search_query"
)

// Limits are one place's own numbers: no place borrows another's, because each has its own
// batch limit and quota.
type Limits struct {
	// MaxBatchTexts is the most texts one Embed call accepts.
	MaxBatchTexts int
	// BackgroundTokensPerMinute is the most estimated tokens (EstimateTokens) a minute
	// background work may spend, leaving the rest of the place's quota to live search queries.
	BackgroundTokensPerMinute int
}

// Embedder embeds text for meaning search. A nil Embedder (no place configured, or the configured
// one could not be built at boot) means meaning search is off everywhere: search.go answers
// keyword-only and says so, and embedqueue.Run never starts.
type Embedder interface {
	// Embed returns one vector per text, in the same order, embedded as inputType. It returns one
	// error for the whole batch: the model service either embeds every text in a call or answers
	// an error, so there is no partial result to salvage from a failed call.
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error)
	// Identity names the model and the place that embedded a vector: the value embeddings.model
	// records, and the one search compares a stored vector against, so a vector from another
	// model or place is never ranked against this embedder's query vector.
	Identity() string
	// Limits reports this embedder's own batch limit and background token budget.
	Limits() Limits
}

// The places meaning search reaches Cohere Embed v4, as an embedder identity names them
// (IdentityPlace) and as DISPATCH_EMBEDDER selects them.
const (
	PlaceBedrock = "bedrock"
	PlaceAzure   = "azure"
)

// azureIdentityPrefix begins every AzureClient identity, "azure:<endpoint host>/<deployment>".
const azureIdentityPrefix = PlaceAzure + ":"

// IdentityPlace names the place and the model an embedder identity records: AzureClient's is
// "azure:<endpoint host>/<deployment>", whose model is the deployment; any other is a
// BedrockClient's, whose identity is the Bedrock model id itself.
func IdentityPlace(identity string) (place, model string) {
	if rest, ok := strings.CutPrefix(identity, azureIdentityPrefix); ok {
		return PlaceAzure, rest[strings.LastIndex(rest, "/")+1:]
	}
	return PlaceBedrock, identity
}

// IsThrottled reports whether err is the model service signaling "too many requests, back off"
// (or a closely related capacity condition), rather than something wrong with the specific text
// that was being embedded. It reads only the marker each client puts on its own throttles - an
// error in err's chain with a Throttled() bool method answering true - because only the client
// knows what its place's throttles look like (bedrockThrottled, azureThrottled).
//
// embedqueue relies on this to decide what a failure means: a throttled batch is retried
// indefinitely (the provider will recover; the content was never the problem), while a non-
// throttled failure from this call is still infrastructure, not evidence about the row - only
// commitEmbedding's own failure (today, a non-finite vector) is specific enough to a row's
// content to ever count toward embedqueue's dead-letter threshold.
func IsThrottled(err error) bool {
	var marked interface{ Throttled() bool }
	return errors.As(err, &marked) && marked.Throttled()
}

// throttleError marks a client's own throttle for IsThrottled, keeping the cause's text and
// letting errors.Is/As reach the cause.
type throttleError struct{ err error }

func (e *throttleError) Error() string   { return e.err.Error() }
func (e *throttleError) Unwrap() error   { return e.err }
func (e *throttleError) Throttled() bool { return true }

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

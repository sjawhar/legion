package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The route and api-version AzureClient posts to, held here alone because INFRA-399's live probe
// may move them: Microsoft's Get Embeddings reference documents this route as the one that
// carries input_type and dimensions, appended to the configured endpoint (for an AI Foundry
// resource, https://<resource>.services.ai.azure.com/models).
const (
	azureEmbeddingsRoute = "/embeddings"
	azureAPIVersion      = "2024-05-01-preview"
)

const (
	// azureMaxBatchTexts is Cohere Embed v4's own batch limit, the same 96 texts Bedrock
	// accepts; INFRA-399's probe confirms or corrects it for the Azure deployment.
	azureMaxBatchTexts = 96
	// azureRequestTimeout bounds one Embed call, sign-in included, below embedqueue's claimWindow
	// (2 minutes), so a hung call never outlives the claim on the rows it carries.
	azureRequestTimeout = 60 * time.Second
	// azureErrorBodyLimit bounds how much of a failed response is read for its error code.
	azureErrorBodyLimit = 64 << 10
)

// TokenSource hands AzureClient a bearer token for each call. EntraTokenSource is the one
// Dispatch uses; it caches, so calling Token once per Embed costs nothing between exchanges.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// AzureConfig configures an AzureClient. Exactly one of APIKey and Tokens is set.
type AzureConfig struct {
	// Endpoint is the absolute base URL the route is appended to.
	Endpoint string
	// Deployment is the Azure deployment's name, sent as the request's model.
	Deployment string
	// TokensPerMinute is the deployment's quota; background work gets two thirds of it.
	TokensPerMinute int
	// APIKey is the key form of credential, sent as the api-key header.
	APIKey string
	// Tokens is the keyless form of credential, each token sent as a bearer.
	Tokens TokenSource
	// HTTPClient replaces http.DefaultClient; a test injects one.
	HTTPClient *http.Client
}

// AzureClient calls Cohere Embed v4 deployed on Azure AI Foundry, over plain net/http: one POST
// per batch does not need the Azure SDK.
type AzureClient struct {
	url        string
	identity   string
	deployment string
	limits     Limits
	apiKey     string
	tokens     TokenSource
	http       *http.Client
}

// NewAzure checks cfg's shape and returns an AzureClient; it makes no call, so a model service
// that is down at boot never refuses boot. It does not require https: the boot settings refuse a
// plain-http endpoint, and a test points this client at a local http server.
func NewAzure(cfg AzureConfig) (*AzureClient, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("embed: azure endpoint: %w", err)
	}
	if !endpoint.IsAbs() || endpoint.Host == "" {
		return nil, errors.New("embed: azure endpoint must be an absolute URL with a host")
	}
	if endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("embed: azure endpoint must carry no query or fragment: the route is appended to it")
	}
	if cfg.Deployment == "" {
		return nil, errors.New("embed: azure needs a deployment name")
	}
	if cfg.TokensPerMinute <= 0 {
		return nil, errors.New("embed: azure needs a positive tokens-per-minute quota")
	}
	if (cfg.APIKey == "") == (cfg.Tokens == nil) {
		return nil, errors.New("embed: azure needs exactly one credential: an API key or a token source")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	target := *endpoint
	target.Path = strings.TrimRight(endpoint.Path, "/") + azureEmbeddingsRoute
	target.RawPath = ""
	target.RawQuery = url.Values{"api-version": {azureAPIVersion}}.Encode()
	return &AzureClient{
		url:        target.String(),
		identity:   "azure:" + strings.ToLower(endpoint.Host) + "/" + cfg.Deployment,
		deployment: cfg.Deployment,
		limits: Limits{
			MaxBatchTexts: azureMaxBatchTexts,
			// Two thirds of the quota, the ratio Bedrock's budget keeps, floored at one token so
			// a tiny quota still paces background work instead of stopping it.
			BackgroundTokensPerMinute: max(cfg.TokensPerMinute*2/3, 1),
		},
		apiKey: cfg.APIKey,
		tokens: cfg.Tokens,
		http:   client,
	}, nil
}

// Identity is "azure:<endpoint host>/<deployment>": the place belongs in the identity, so a
// vector embedded by another resource or deployment is never compared with this one's queries.
func (c *AzureClient) Identity() string { return c.identity }

// Limits reports Azure's batch limit and two thirds of the configured quota.
func (c *AzureClient) Limits() Limits { return c.limits }

type azureEmbedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	InputType      string   `json:"input_type"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

type azureEmbedResponse struct {
	Data []struct {
		// Index is a pointer so a response that leaves it out is told apart from index 0.
		Index     *int      `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

type azureErrorResponse struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// azureInputType maps this package's InputType (Bedrock's names) to Azure's.
func azureInputType(inputType InputType) (string, error) {
	switch inputType {
	case InputDocument:
		return "document", nil
	case InputQuery:
		return "query", nil
	}
	return "", fmt.Errorf("embed: azure has no input type for %q", inputType)
}

// Embed calls Azure once for up to azureMaxBatchTexts texts and places each vector by the index
// Azure answers with, not by its position in the response. A 429 or 503 comes back marked for
// IsThrottled; any other failure names the HTTP status and Azure's error code, never the texts
// or the credential.
func (c *AzureClient) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > azureMaxBatchTexts {
		return nil, fmt.Errorf("embed: %d texts exceeds the %d-text batch limit", len(texts), azureMaxBatchTexts)
	}
	azureType, err := azureInputType(inputType)
	if err != nil {
		return nil, err
	}
	bounded := make([]string, len(texts))
	for i, t := range texts {
		bounded[i] = text.HeadRunes(t, maxInputChars)
	}
	body, err := json.Marshal(azureEmbedRequest{
		Model:          c.deployment,
		Input:          bounded,
		InputType:      azureType,
		Dimensions:     Dimension,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, azureRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: azure build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.tokens != nil {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("embed: azure sign-in: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	} else {
		request.Header.Set("api-key", c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("embed: azure embeddings: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, azureStatusError(response)
	}
	var decoded azureEmbedResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("embed: azure decode response: %w", err)
	}
	return placeAzureVectors(decoded, len(texts))
}

// azureStatusError describes a non-2xx answer by its status and Azure's error code, and marks 429
// (the deployment's rate limit) and 503 (the service's capacity) as throttles: neither says
// anything about the texts.
func azureStatusError(response *http.Response) error {
	var decoded azureErrorResponse
	body, _ := io.ReadAll(io.LimitReader(response.Body, azureErrorBodyLimit))
	_ = json.Unmarshal(body, &decoded)
	err := fmt.Errorf("embed: azure embeddings: HTTP %s", response.Status)
	if decoded.Error.Code != "" {
		err = fmt.Errorf("embed: azure embeddings: HTTP %s, error code %q", response.Status, decoded.Error.Code)
	}
	if azureThrottled(response.StatusCode) {
		return &throttleError{err: err}
	}
	return err
}

// azureThrottled reports whether an HTTP status is Azure telling the caller to back off.
func azureThrottled(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// placeAzureVectors puts each vector at the index Azure gave it, refusing an answer that does not
// carry exactly one vector of Dimension floats for each of the n texts.
func placeAzureVectors(decoded azureEmbedResponse, n int) ([][]float32, error) {
	if len(decoded.Data) != n {
		return nil, fmt.Errorf("embed: azure returned %d embeddings for %d texts", len(decoded.Data), n)
	}
	vectors := make([][]float32, n)
	for _, item := range decoded.Data {
		if item.Index == nil {
			return nil, errors.New("embed: azure returned an embedding with no index")
		}
		i := *item.Index
		if i < 0 || i >= n {
			return nil, fmt.Errorf("embed: azure returned index %d for %d texts", i, n)
		}
		if vectors[i] != nil {
			return nil, fmt.Errorf("embed: azure returned index %d twice", i)
		}
		if len(item.Embedding) != Dimension {
			return nil, fmt.Errorf("embed: azure returned a %d-wide embedding at index %d, want %d", len(item.Embedding), i, Dimension)
		}
		vectors[i] = item.Embedding
	}
	return vectors, nil
}

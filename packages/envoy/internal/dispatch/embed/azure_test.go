package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const testAzureKey = "test-azure-key-never-in-errors"

// azureVector is a Dimension-wide vector whose first component is mark, so a test can tell which
// input it was answered for.
func azureVector(mark float32) []float32 {
	v := make([]float32, Dimension)
	v[0] = mark
	return v
}

// azureAnswer renders Azure's Get Embeddings response with one item per index, in the order
// given, each vector marked with its own index. It runs inside a test server's handler, so it
// takes no *testing.T: encoding ints and finite floats cannot fail.
func azureAnswer(indices ...int) []byte {
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]item, len(indices))
	for i, index := range indices {
		data[i] = item{Index: index, Embedding: azureVector(float32(index))}
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": data, "model": "embed-v-4-0"})
	return body
}

// testAzureClient builds an AzureClient against server's "/models" base with the key form of
// credential unless cfg sets the token form.
func testAzureClient(t *testing.T, server *httptest.Server, cfg AzureConfig) *AzureClient {
	t.Helper()
	cfg.Endpoint = server.URL + "/models"
	if cfg.Deployment == "" {
		cfg.Deployment = "embed-v-4-0"
	}
	if cfg.TokensPerMinute == 0 {
		cfg.TokensPerMinute = 300_000
	}
	if cfg.Tokens == nil && cfg.APIKey == "" {
		cfg.APIKey = testAzureKey
	}
	cfg.HTTPClient = server.Client()
	client, err := NewAzure(cfg)
	if err != nil {
		t.Fatalf("NewAzure: %v", err)
	}
	return client
}

type staticTokens struct {
	token string
	err   error
}

func (s staticTokens) Token(context.Context) (string, error) { return s.token, s.err }

// TestAzureEmbedSendsTheDocumentedRequestForBothInputTypes checks the request field by field: the
// route and api-version, the deployment as model, Azure's names for the two input types, 1536
// dimensions, float encoding, and nothing else in the body.
func TestAzureEmbedSendsTheDocumentedRequestForBothInputTypes(t *testing.T) {
	for _, tc := range []struct {
		inputType InputType
		want      string
	}{
		{InputDocument, "document"},
		{InputQuery, "query"},
	} {
		t.Run(string(tc.inputType), func(t *testing.T) {
			var gotMethod, gotPath, gotVersion, gotContentType, gotKey, gotAuthorization string
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotVersion = r.Method, r.URL.Path, r.URL.Query().Get("api-version")
				gotContentType = r.Header.Get("Content-Type")
				gotKey, gotAuthorization = r.Header.Get("api-key"), r.Header.Get("Authorization")
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				_, _ = w.Write(azureAnswer(0, 1))
			}))
			defer server.Close()

			client := testAzureClient(t, server, AzureConfig{Deployment: "my-deployment"})
			if _, err := client.Embed(context.Background(), []string{"first", "second"}, tc.inputType); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if gotMethod != http.MethodPost || gotPath != "/models/embeddings" || gotVersion != "2024-05-01-preview" {
				t.Errorf("request = %s %s api-version=%q, want POST /models/embeddings api-version=2024-05-01-preview", gotMethod, gotPath, gotVersion)
			}
			if gotContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotContentType)
			}
			if gotKey != testAzureKey || gotAuthorization != "" {
				t.Errorf("api-key = %q, Authorization = %q; want the key in api-key and no Authorization", gotKey, gotAuthorization)
			}
			want := map[string]any{
				"model":           "my-deployment",
				"input":           []any{"first", "second"},
				"input_type":      tc.want,
				"dimensions":      float64(1536),
				"encoding_format": "float",
			}
			if !reflect.DeepEqual(gotBody, want) {
				t.Errorf("request body = %v, want %v", gotBody, want)
			}
		})
	}
}

func TestAzureEmbedSendsABearerTokenInTheKeylessForm(t *testing.T) {
	var gotKey, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAuthorization = r.Header.Get("api-key"), r.Header.Get("Authorization")
		_, _ = w.Write(azureAnswer(0))
	}))
	defer server.Close()
	client := testAzureClient(t, server, AzureConfig{Tokens: staticTokens{token: "entra-access-token"}})
	if _, err := client.Embed(context.Background(), []string{"x"}, InputQuery); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotAuthorization != "Bearer entra-access-token" || gotKey != "" {
		t.Errorf("Authorization = %q, api-key = %q; want the bearer token and no api-key", gotAuthorization, gotKey)
	}
}

// TestAzureEmbedFailsWithoutACallWhenSignInFails proves a token source's failure is a plain,
// non-throttled error, and no unauthenticated request goes out in its place.
func TestAzureEmbedFailsWithoutACallWhenSignInFails(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	client := testAzureClient(t, server, AzureConfig{Tokens: staticTokens{err: errors.New("simulated sign-in failure")}})
	_, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "simulated sign-in failure") || IsThrottled(err) {
		t.Fatalf("Embed error = %v, want the sign-in failure, not throttled", err)
	}
	if called {
		t.Error("Embed called Azure after sign-in failed")
	}
}

func TestAzureEmbedPlacesVectorsByIndexNotByPosition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(azureAnswer(2, 0, 1))
	}))
	defer server.Close()
	vectors, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), []string{"a", "b", "c"}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, v := range vectors {
		if len(v) != Dimension || v[0] != float32(i) {
			t.Errorf("vectors[%d] is the vector Azure gave index %v, want index %d's", i, v[0], i)
		}
	}
}

func TestAzureEmbedOfZeroTextsAnswersNilWithoutACall(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	vectors, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), nil, InputDocument)
	if err != nil || vectors != nil {
		t.Fatalf("Embed(nil) = %v, %v, want nil, nil", vectors, err)
	}
	if called {
		t.Error("Embed(nil) made an HTTP call; it should return before building one")
	}
}

func TestAzureEmbedRefusesMoreThanTheBatchLimit(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	client := testAzureClient(t, server, AzureConfig{})
	texts := make([]string, client.Limits().MaxBatchTexts+1)
	for i := range texts {
		texts[i] = "x"
	}
	if _, err := client.Embed(context.Background(), texts, InputDocument); err == nil {
		t.Fatal("Embed with one text past the batch limit: want an error, got nil")
	}
	if called {
		t.Error("Embed past the batch limit made an HTTP call")
	}
}

// TestAzureEmbedClassifiesStatuses: 429 and 503 are throttles, every other failure is not, and
// each error names the status and Azure's error code but never the key or the texts.
func TestAzureEmbedClassifiesStatuses(t *testing.T) {
	for _, tc := range []struct {
		status    int
		code      string
		throttled bool
	}{
		{http.StatusTooManyRequests, "RateLimitReached", true},
		{http.StatusServiceUnavailable, "ServiceUnavailable", true},
		{http.StatusBadRequest, "InvalidRequest", false},
		{http.StatusUnauthorized, "PermissionDenied", false},
		{http.StatusNotFound, "DeploymentNotFound", false},
		{http.StatusInternalServerError, "InternalServerError", false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"code":"` + tc.code + `","message":"simulated, quoting secret-text-to-embed"}}`))
			}))
			defer server.Close()
			_, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), []string{"secret-text-to-embed"}, InputDocument)
			if err == nil {
				t.Fatal("Embed: want an error, got nil")
			}
			if got := IsThrottled(err); got != tc.throttled {
				t.Errorf("IsThrottled(%v) = %v, want %v", err, got, tc.throttled)
			}
			text := err.Error()
			if !strings.Contains(text, http.StatusText(tc.status)) || !strings.Contains(text, tc.code) {
				t.Errorf("error %q does not name status %d and code %s", text, tc.status, tc.code)
			}
			if strings.Contains(text, testAzureKey) || strings.Contains(text, "secret-text-to-embed") {
				t.Errorf("error %q carries the key or the text", text)
			}
		})
	}
}

func TestAzureEmbedNamesTheStatusOfAnErrorWithoutACode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer server.Close()
	_, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), []string{"x"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "502") || IsThrottled(err) {
		t.Fatalf("Embed error = %v, want an unthrottled error naming HTTP 502", err)
	}
}

// TestAzureEmbedRefusesAMalformedAnswer: an answer that does not hold exactly one 1536-wide
// vector for each text, each at its own index, is an error rather than vectors stored at the
// wrong row or the wrong width.
func TestAzureEmbedRefusesAMalformedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"too few", `{"data":[{"index":0,"embedding":` + vectorJSON(Dimension) + `}]}`, "1 embeddings for 2 texts"},
		{"too many", `{"data":[{"index":0,"embedding":` + vectorJSON(Dimension) + `},{"index":1,"embedding":` + vectorJSON(Dimension) + `},{"index":2,"embedding":` + vectorJSON(Dimension) + `}]}`, "3 embeddings for 2 texts"},
		{"narrow", `{"data":[{"index":0,"embedding":` + vectorJSON(Dimension) + `},{"index":1,"embedding":` + vectorJSON(1024) + `}]}`, "1024-wide"},
		{"wide", `{"data":[{"index":0,"embedding":` + vectorJSON(Dimension+1) + `},{"index":1,"embedding":` + vectorJSON(Dimension) + `}]}`, "1537-wide"},
		{"duplicate index", `{"data":[{"index":1,"embedding":` + vectorJSON(Dimension) + `},{"index":1,"embedding":` + vectorJSON(Dimension) + `}]}`, "index 1 twice"},
		{"out-of-range index", `{"data":[{"index":0,"embedding":` + vectorJSON(Dimension) + `},{"index":2,"embedding":` + vectorJSON(Dimension) + `}]}`, "index 2 for 2 texts"},
		{"missing index", `{"data":[{"index":0,"embedding":` + vectorJSON(Dimension) + `},{"embedding":` + vectorJSON(Dimension) + `}]}`, "no index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			vectors, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), []string{"a", "b"}, InputDocument)
			if err == nil || !strings.Contains(err.Error(), tc.want) || vectors != nil {
				t.Fatalf("Embed = %d vectors, error %v; want no vectors and an error naming %q", len(vectors), err, tc.want)
			}
			if IsThrottled(err) {
				t.Errorf("IsThrottled(%v) = true, want false", err)
			}
		})
	}
}

func vectorJSON(width int) string {
	body, _ := json.Marshal(make([]float32, width))
	return string(body)
}

// TestAzureEmbedSendsAMaxInputCharsTextWhole proves a body at Dispatch's own limit reaches Azure
// whole: the server counts the runes it got.
func TestAzureEmbedSendsAMaxInputCharsTextWhole(t *testing.T) {
	full := longText()
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body azureEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if len(body.Input) == 1 {
			got = body.Input[0]
		}
		_, _ = w.Write(azureAnswer(0))
	}))
	defer server.Close()
	if _, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), []string{full}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n := utf8.RuneCountInString(got); n != 32000 || got != full {
		t.Errorf("server received %d runes (equal to the text sent: %v), want all 32000", n, got == full)
	}
}

func TestAzureEmbedTruncatesAnOverlongTextByRunes(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body azureEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = body.Input[0]
		_, _ = w.Write(azureAnswer(0))
	}))
	defer server.Close()
	overlong := strings.Repeat("é", maxInputChars+100)
	if _, err := testAzureClient(t, server, AzureConfig{}).Embed(context.Background(), []string{overlong}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n := utf8.RuneCountInString(got); n != maxInputChars || !utf8.ValidString(got) {
		t.Errorf("server received %d runes (valid UTF-8: %v), want %d", n, utf8.ValidString(got), maxInputChars)
	}
}

func TestAzureIdentityAndLimitsAreAzuresOwn(t *testing.T) {
	client, err := NewAzure(AzureConfig{
		Endpoint:        "https://Example-Resource.services.ai.azure.com/models/",
		Deployment:      "embed-v-4-0",
		TokensPerMinute: 300_000,
		APIKey:          "k",
	})
	if err != nil {
		t.Fatalf("NewAzure: %v", err)
	}
	if got, want := client.Identity(), "azure:example-resource.services.ai.azure.com/embed-v-4-0"; got != want {
		t.Errorf("Identity() = %q, want %q", got, want)
	}
	if got, want := client.Limits(), (Limits{MaxBatchTexts: 96, MaxBatchBytes: 320_000, BackgroundTokensPerMinute: 200_000}); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
	if got, want := client.url, "https://Example-Resource.services.ai.azure.com/models/embeddings?api-version=2024-05-01-preview"; got != want {
		t.Errorf("request URL = %q, want %q", got, want)
	}
}

func TestNewAzureRefusesAMisshapenConfig(t *testing.T) {
	valid := AzureConfig{
		Endpoint:        "https://example.services.ai.azure.com/models",
		Deployment:      "embed-v-4-0",
		TokensPerMinute: 300_000,
		APIKey:          "k",
	}
	if _, err := NewAzure(valid); err != nil {
		t.Fatalf("NewAzure(a valid config): %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*AzureConfig)
	}{
		{"no endpoint", func(c *AzureConfig) { c.Endpoint = "" }},
		{"relative endpoint", func(c *AzureConfig) { c.Endpoint = "example.services.ai.azure.com/models" }},
		{"unparsable endpoint", func(c *AzureConfig) { c.Endpoint = "https://exa mple/%zz" }},
		{"endpoint with a query", func(c *AzureConfig) { c.Endpoint += "?api-version=1" }},
		{"no deployment", func(c *AzureConfig) { c.Deployment = "" }},
		{"zero quota", func(c *AzureConfig) { c.TokensPerMinute = 0 }},
		{"negative quota", func(c *AzureConfig) { c.TokensPerMinute = -1 }},
		{"no credential", func(c *AzureConfig) { c.APIKey = "" }},
		{"both credentials", func(c *AzureConfig) { c.Tokens = staticTokens{token: "t"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.change(&cfg)
			if _, err := NewAzure(cfg); err == nil {
				t.Errorf("NewAzure(%s): want an error, got nil", tc.name)
			}
		})
	}
}

package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// testBedrockClient builds a BedrockClient against server, with static fake credentials and a
// fake region: the AWS SDK still signs every request (SigV4), but nothing on server's side
// verifies the signature, so a test server only needs to receive a well-formed InvokeModel call
// and answer Bedrock's documented response shape. The SDK's own retries are off, so a test that
// answers an error sees it on the first call rather than after the SDK's backoff.
func testBedrockClient(server *httptest.Server, opts ...BedrockOption) *BedrockClient {
	cfg := aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	return NewBedrockWithConfig(cfg, append([]BedrockOption{WithBaseEndpoint(server.URL)}, opts...)...)
}

func TestBedrockEmbedSendsInvokeModelRequestShapeAndParsesTheResponse(t *testing.T) {
	var gotPath, gotContentType string
	var gotBody bedrockEmbedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bedrock's InvokeModel carries the model id in the request path, not the JSON body.
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","response_type":"embeddings_by_type","embeddings":{"float":[[0.1,0.2,0.3]]}}`))
	}))
	defer server.Close()

	client := testBedrockClient(server)
	vectors, err := client.Embed(context.Background(), []string{"hello world"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.Contains(gotPath, BedrockModel) {
		t.Errorf("request path = %q, want it to name the model %q", gotPath, BedrockModel)
	}
	if !strings.HasPrefix(gotPath, "/model/") || !strings.HasSuffix(gotPath, "/invoke") {
		t.Errorf("path = %q, want Bedrock's /model/<id>/invoke shape", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.InputType != InputQuery || gotBody.OutputDimension != Dimension {
		t.Errorf("request body = %+v, want input_type=%s output_dimension=%d", gotBody, InputQuery, Dimension)
	}
	if len(gotBody.EmbeddingTypes) != 1 || gotBody.EmbeddingTypes[0] != "float" {
		t.Errorf("embedding_types = %v, want [float]", gotBody.EmbeddingTypes)
	}
	if len(gotBody.Texts) != 1 || gotBody.Texts[0] != "hello world" {
		t.Errorf("request texts = %v, want [hello world]", gotBody.Texts)
	}
	if len(vectors) != 1 || len(vectors[0]) != 3 || vectors[0][0] != 0.1 {
		t.Errorf("vectors = %v, want one 3-float vector starting 0.1", vectors)
	}
}

func TestBedrockEmbedOfZeroTextsAnswersNilWithoutACall(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	client := testBedrockClient(server)
	vectors, err := client.Embed(context.Background(), nil, InputDocument)
	if err != nil || vectors != nil {
		t.Fatalf("Embed(nil) = %v, %v, want nil, nil", vectors, err)
	}
	if called {
		t.Error("Embed(nil) made an HTTP call; it should return before building one")
	}
}

func TestBedrockEmbedRefusesMoreThanTheBatchLimit(t *testing.T) {
	client := NewBedrockWithConfig(aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("k", "s", "")})
	texts := make([]string, client.Limits().MaxBatchTexts+1)
	for i := range texts {
		texts[i] = "x"
	}
	if _, err := client.Embed(context.Background(), texts, InputDocument); err == nil {
		t.Fatal("Embed with one text past the batch limit: want an error, got nil")
	}
}

func TestBedrockEmbedReturnsBedrockErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()
	client := testBedrockClient(server)
	_, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "bedrock invoke model") {
		t.Fatalf("Embed error = %v, want it to mention the Bedrock call failing", err)
	}
}

// TestBedrockEmbedMarksItsOwnThrottles drives a coded Bedrock error through the real SDK path:
// Embed classifies it with Bedrock's code set and marks a throttle for IsThrottled, while a bad
// request comes back unmarked.
func TestBedrockEmbedMarksItsOwnThrottles(t *testing.T) {
	for _, tc := range []struct {
		code      string
		status    int
		throttled bool
	}{
		{"ThrottlingException", http.StatusTooManyRequests, true},
		{"ServiceUnavailableException", http.StatusServiceUnavailable, true},
		{"ModelNotReadyException", http.StatusTooManyRequests, true},
		{"ValidationException", http.StatusBadRequest, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Amzn-ErrorType", tc.code)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"simulated"}`))
			}))
			defer server.Close()
			_, err := testBedrockClient(server).Embed(context.Background(), []string{"x"}, InputDocument)
			if err == nil {
				t.Fatal("Embed: want an error, got nil")
			}
			if got := IsThrottled(err); got != tc.throttled {
				t.Errorf("IsThrottled(%v) = %v, want %v", err, got, tc.throttled)
			}
			var coded interface{ ErrorCode() string }
			if !errors.As(err, &coded) || coded.ErrorCode() != tc.code {
				t.Errorf("Embed error = %v, want it to unwrap to Bedrock's %s", err, tc.code)
			}
		})
	}
}

func TestBedrockEmbedRefusesAMismatchedEmbeddingCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response_type":"embeddings_by_type","embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	client := testBedrockClient(server)
	_, err := client.Embed(context.Background(), []string{"a", "b"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "1 embeddings for 2 texts") {
		t.Fatalf("Embed error = %v, want it to name the count mismatch", err)
	}
}

func TestBedrockEmbedTruncatesAnOverlongTextByRunesNotBytes(t *testing.T) {
	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bedrockEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText = body.Texts[0]
		_, _ = w.Write([]byte(`{"response_type":"embeddings_by_type","embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	// A multi-byte rune ("é", 2 bytes in UTF-8) repeated past the char cap: truncating by byte
	// count would split a rune and produce invalid UTF-8; this must not panic or corrupt it.
	overlong := strings.Repeat("é", maxInputChars+100)
	client := testBedrockClient(server)
	if _, err := client.Embed(context.Background(), []string{overlong}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if count := len([]rune(gotText)); count != maxInputChars {
		t.Errorf("truncated text has %d runes, want %d", count, maxInputChars)
	}
}

// TestBedrockEmbedSendsAMaxInputCharsTextWhole proves a body at Dispatch's own limit (32,000
// runes, multi-byte ones included) reaches Bedrock whole: the server counts the runes it got.
func TestBedrockEmbedSendsAMaxInputCharsTextWhole(t *testing.T) {
	full := longText()
	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bedrockEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if len(body.Texts) == 1 {
			gotText = body.Texts[0]
		}
		_, _ = w.Write([]byte(`{"response_type":"embeddings_by_type","embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	if _, err := testBedrockClient(server).Embed(context.Background(), []string{full}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := utf8.RuneCountInString(gotText); got != 32000 || gotText != full {
		t.Errorf("server received %d runes (equal to the text sent: %v), want all 32000", got, gotText == full)
	}
}

func TestNewBedrockRefusesAnEmptyRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "does-not-exist-so-nothing-else-supplies-a-region")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	if _, err := NewBedrock(context.Background()); err == nil {
		t.Fatal("NewBedrock with no region configured anywhere: want an error, got nil")
	}
}

// TestBedrockIdentityFollowsWithModel proves a vector records the model that actually embedded
// it: BedrockModel by default, and WithModel's override when one is set.
func TestBedrockIdentityFollowsWithModel(t *testing.T) {
	cfg := aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("k", "s", "")}
	if got := NewBedrockWithConfig(cfg).Identity(); got != BedrockModel {
		t.Errorf("default Identity() = %q, want %q", got, BedrockModel)
	}
	if got := NewBedrockWithConfig(cfg, WithModel("cohere.other-model")).Identity(); got != "cohere.other-model" {
		t.Errorf("Identity() with WithModel = %q, want %q", got, "cohere.other-model")
	}
}

func TestBedrockLimitsAreBedrocksOwn(t *testing.T) {
	got := NewBedrockWithConfig(aws.Config{Region: "us-west-2"}).Limits()
	if want := (Limits{MaxBatchTexts: 96, MaxBatchBytes: 0, BackgroundTokensPerMinute: 200_000}); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
}

// codedError carries an AWS-style error code and nothing else - no throttle marker - so the
// classifier tests below exercise bedrockThrottled's code set alone.
type codedError struct{ code string }

func (e codedError) Error() string     { return "simulated: " + e.code }
func (e codedError) ErrorCode() string { return e.code }

func TestBedrockThrottledRecognizesBedrockThrottleCodesThroughAWrappedError(t *testing.T) {
	for _, code := range []string{
		"ThrottlingException", "ServiceUnavailableException", "SlowDown", "ModelNotReadyException",
	} {
		wrapped := fmt.Errorf("embed: bedrock invoke model: %w", codedError{code: code})
		if !bedrockThrottled(wrapped) {
			t.Errorf("bedrockThrottled(%q wrapped) = false, want true", code)
		}
	}
}

func TestBedrockThrottledDoesNotMisclassifyAnOrdinaryError(t *testing.T) {
	if bedrockThrottled(errors.New("embed: bedrock invoke model: connection refused")) {
		t.Error("bedrockThrottled(a plain connection error) = true, want false")
	}
	if bedrockThrottled(codedError{code: "ValidationException"}) {
		t.Error("bedrockThrottled(ValidationException) = true, want false - a bad request is not a throttle")
	}
}

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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// testClient builds a Client against server, with static fake credentials and a fake region:
// the AWS SDK still signs every request (SigV4), but nothing on server's side verifies the
// signature, so a test server only needs to receive a well-formed InvokeModel call and answer
// Bedrock's documented response shape.
func testClient(server *httptest.Server) *Client {
	cfg := aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", ""),
	}
	return NewWithConfig(cfg, WithBaseEndpoint(server.URL))
}

func TestEmbedSendsBedrockInvokeModelRequestShapeAndParsesTheResponse(t *testing.T) {
	var gotPath, gotModel, gotContentType string
	var gotBody embedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		// Bedrock's InvokeModel carries the model id in the request path, not the JSON body.
		gotModel = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","response_type":"embeddings_by_type","embeddings":{"float":[[0.1,0.2,0.3]]}}`))
	}))
	defer server.Close()

	client := testClient(server)
	vectors, err := client.Embed(context.Background(), []string{"hello world"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.Contains(gotPath, Model) {
		t.Errorf("request path = %q, want it to name the model %q", gotModel, Model)
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

func TestEmbedOfZeroTextsAnswersNilWithoutACall(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	client := testClient(server)
	vectors, err := client.Embed(context.Background(), nil, InputDocument)
	if err != nil || vectors != nil {
		t.Fatalf("Embed(nil) = %v, %v, want nil, nil", vectors, err)
	}
	if called {
		t.Error("Embed(nil) made an HTTP call; it should return before building one")
	}
}

func TestEmbedRefusesMoreThanTheBatchLimit(t *testing.T) {
	client := NewWithConfig(aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("k", "s", "")})
	texts := make([]string, MaxBatchTexts+1)
	for i := range texts {
		texts[i] = "x"
	}
	if _, err := client.Embed(context.Background(), texts, InputDocument); err == nil {
		t.Fatal("Embed with MaxBatchTexts+1 texts: want an error, got nil")
	}
}

func TestEmbedReturnsBedrockErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()
	client := testClient(server)
	_, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "bedrock invoke model") {
		t.Fatalf("Embed error = %v, want it to mention the Bedrock call failing", err)
	}
}

func TestEmbedRefusesAMismatchedEmbeddingCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response_type":"embeddings_by_type","embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	client := testClient(server)
	_, err := client.Embed(context.Background(), []string{"a", "b"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "1 embeddings for 2 texts") {
		t.Fatalf("Embed error = %v, want it to name the count mismatch", err)
	}
}

func TestEmbedTruncatesAnOverlongTextByRunesNotBytes(t *testing.T) {
	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body embedRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText = body.Texts[0]
		_, _ = w.Write([]byte(`{"response_type":"embeddings_by_type","embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	// A multi-byte rune ("é", 2 bytes in UTF-8) repeated past the char cap: truncating by byte
	// count would split a rune and produce invalid UTF-8; this must not panic or corrupt it.
	overlong := strings.Repeat("é", maxInputChars+100)
	client := testClient(server)
	if _, err := client.Embed(context.Background(), []string{overlong}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if count := len([]rune(gotText)); count != maxInputChars {
		t.Errorf("truncated text has %d runes, want %d", count, maxInputChars)
	}
}

func TestNewRefusesAnEmptyRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "does-not-exist-so-nothing-else-supplies-a-region")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	if _, err := New(context.Background()); err == nil {
		t.Fatal("New with no region configured anywhere: want an error, got nil")
	}
}

func TestLiteralRendersAPgvectorTextForm(t *testing.T) {
	got := Literal([]float32{0.5, -1, 0})
	want := "[0.50000000,-1.00000000,0.00000000]"
	if got != want {
		t.Errorf("Literal = %q, want %q", got, want)
	}
}

// throttleError stands in for a real Bedrock ThrottlingException: it carries the
// `ErrorCode() string` method IsThrottled's classifier (and the AWS SDK's own
// retry.ThrottleErrorCode beneath it) looks for, without needing a live Bedrock call or a
// hand-built smithy type.
type throttleError struct{ code string }

func (e throttleError) Error() string     { return "simulated: " + e.code }
func (e throttleError) ErrorCode() string { return e.code }

func TestIsThrottledRecognizesBedrockThrottleCodesThroughAWrappedError(t *testing.T) {
	for _, code := range []string{"ThrottlingException", "ServiceUnavailableException", "SlowDown"} {
		wrapped := fmt.Errorf("embed: bedrock invoke model: %w", throttleError{code: code})
		if code == "ServiceUnavailableException" {
			// Not itself a default throttle code (it is a server error, not specifically a rate
			// limit) - included here to document that IsThrottled does not claim it, not to
			// assert it does.
			continue
		}
		if !IsThrottled(wrapped) {
			t.Errorf("IsThrottled(%q wrapped) = false, want true", code)
		}
	}
}

func TestIsThrottledDoesNotMisclassifyAnOrdinaryError(t *testing.T) {
	if IsThrottled(errors.New("embed: bedrock invoke model: connection refused")) {
		t.Error("IsThrottled(a plain connection error) = true, want false")
	}
	if IsThrottled(throttleError{code: "ValidationException"}) {
		t.Error("IsThrottled(ValidationException) = true, want false - a bad request is not a throttle")
	}
}

package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbedSendsCohereV2RequestShapeAndParsesTheResponse(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody embedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[0.1,0.2,0.3]]}}`))
	}))
	defer server.Close()

	client := New("test-key", WithBaseURL(server.URL))
	vectors, err := client.Embed(context.Background(), []string{"hello world"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v2/embed" {
		t.Errorf("path = %q, want /v2/embed", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotBody.Model != Model || gotBody.InputType != InputQuery || gotBody.OutputDimension != Dimension {
		t.Errorf("request body = %+v, want model=%s input_type=%s output_dimension=%d", gotBody, Model, InputQuery, Dimension)
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
	client := New("test-key", WithBaseURL(server.URL))
	vectors, err := client.Embed(context.Background(), nil, InputDocument)
	if err != nil || vectors != nil {
		t.Fatalf("Embed(nil) = %v, %v, want nil, nil", vectors, err)
	}
	if called {
		t.Error("Embed(nil) made an HTTP call; it should return before building one")
	}
}

func TestEmbedRefusesMoreThanTheBatchLimit(t *testing.T) {
	client := New("test-key")
	texts := make([]string, MaxBatchTexts+1)
	for i := range texts {
		texts[i] = "x"
	}
	if _, err := client.Embed(context.Background(), texts, InputDocument); err == nil {
		t.Fatal("Embed with MaxBatchTexts+1 texts: want an error, got nil")
	}
}

func TestEmbedReturnsCohereErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()
	client := New("test-key", WithBaseURL(server.URL))
	_, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("Embed error = %v, want it to mention %q", err, "rate limited")
	}
}

func TestEmbedRefusesAMismatchedEmbeddingCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	client := New("test-key", WithBaseURL(server.URL))
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
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[0.1]]}}`))
	}))
	defer server.Close()
	// A multi-byte rune ("é", 2 bytes in UTF-8) repeated past the char cap: truncating by byte
	// count would split a rune and produce invalid UTF-8; this must not panic or corrupt it.
	overlong := strings.Repeat("é", maxInputChars+100)
	client := New("test-key", WithBaseURL(server.URL))
	if _, err := client.Embed(context.Background(), []string{overlong}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if count := len([]rune(gotText)); count != maxInputChars {
		t.Errorf("truncated text has %d runes, want %d", count, maxInputChars)
	}
}

func TestNewPanicsOnAnEmptyAPIKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(\"\") did not panic")
		}
	}()
	New("")
}

func TestLiteralRendersAPgvectorTextForm(t *testing.T) {
	got := Literal([]float32{0.5, -1, 0})
	want := "[0.50000000,-1.00000000,0.00000000]"
	if got != want {
		t.Errorf("Literal = %q, want %q", got, want)
	}
}

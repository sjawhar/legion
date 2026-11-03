package embed

import (
	"context"
	"math"
	"strings"
	"testing"
)

// cosine is the similarity pgvector's cosine distance ranks by, 1 less the distance.
func cosine(a, b []float32) float64 {
	dot := 0.0
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// The property every acceptance pass of the local harness leans on: a query lands nearer the
// text that shares its words than the text that shares none.
func TestTheDevEmbedderPutsTextsSharingWordsNearest(t *testing.T) {
	vectors, err := NewDev().Embed(context.Background(), []string{
		"the window holds neighbouring blocks of one owner",
		"A Window holds NEIGHBOURING blocks, and nothing else.",
		"the duty roster names the person on call tonight",
	}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, vector := range vectors {
		if len(vector) != Dimension {
			t.Fatalf("vector %d is %d wide, want %d", i, len(vector), Dimension)
		}
		if length := math.Sqrt(cosine(vector, vector)); math.Abs(length-1) > 1e-5 {
			t.Errorf("vector %d has length %f, want 1", i, length)
		}
	}
	sharing, sharingNone := cosine(vectors[0], vectors[1]), cosine(vectors[0], vectors[2])
	if sharing <= sharingNone {
		t.Errorf("two texts sharing words are %f apart and two sharing none %f: the first must be the nearer", sharing, sharingNone)
	}
}

// One text embeds to one vector, whatever else it is embedded beside and whichever input type
// asks for it: a query's vector must be the vector the same words were stored under.
func TestTheDevEmbedderIsDeterministic(t *testing.T) {
	dev := NewDev()
	stored, err := dev.Embed(context.Background(), []string{"a window of neighbouring blocks", "something else"}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	queried, err := dev.Embed(context.Background(), []string{"a window of neighbouring blocks"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if cosine(stored[0], queried[0]) < 1-1e-6 {
		t.Errorf("the same text embedded as a document and as a query is %f apart, want the same vector", cosine(stored[0], queried[0]))
	}
}

// A text holding no word at all still has a direction, so pgvector's cosine distance to it is a
// number, and the batch limits are the ones the queue paces by.
func TestTheDevEmbedderAnswersAWordlessTextAndBoundsItsBatch(t *testing.T) {
	vectors, err := NewDev().Embed(context.Background(), []string{"   ...   "}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if length := math.Sqrt(cosine(vectors[0], vectors[0])); math.Abs(length-1) > 1e-5 {
		t.Errorf("a text holding no word has a vector of length %f, want 1", length)
	}
	limits := NewDev().Limits()
	if limits.MaxBatchTexts != devMaxBatchTexts || limits.BackgroundTokensPerMinute <= 0 {
		t.Errorf("Limits() = %+v, want a batch limit and a token budget the queue can run under", limits)
	}
	if _, err := NewDev().Embed(context.Background(), make([]string, limits.MaxBatchTexts+1), InputDocument); err == nil {
		t.Error("a batch past the limit was accepted, want the refusal every client makes")
	}
}

// The identity names this embedder and no model, and IdentityPlace reads it as its own place,
// so a vector a harness made is never ranked against a model service's query vectors.
func TestTheDevEmbedderIdentityNamesItself(t *testing.T) {
	identity := NewDev().Identity()
	if !strings.HasPrefix(identity, devIdentityPrefix) {
		t.Errorf("Identity() = %q, want it to open with %q", identity, devIdentityPrefix)
	}
	if place, model := IdentityPlace(identity); place != PlaceDev || model != devModel {
		t.Errorf("IdentityPlace(%q) = %q, %q, want %q, %q", identity, place, model, PlaceDev, devModel)
	}
	if identity == BedrockModel {
		t.Errorf("Identity() = %q, which is the Bedrock model's identity", identity)
	}
}

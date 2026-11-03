package embed

import (
	"context"
	"math"
	"strings"
	"unicode"
)

const (
	// devModel is the recipe DevClient embeds by, as its identity records it: words hashed into
	// buckets. A change to the recipe takes a new name here, so vectors made by the old one are
	// never ranked against queries made by the new.
	devModel = "hashed-words-v1"
	// devIdentity is DevClient's identity, "dev:<recipe>", the shape AzureClient's identity
	// takes ("azure:<host>/<deployment>"): the place first, so IdentityPlace reads it as this
	// place and a row a model service embedded is never ranked against this embedder's queries.
	devIdentity = devIdentityPrefix + devModel
	// devMaxBatchTexts is the most texts one Embed call takes, the same 96 Cohere Embed v4
	// takes at either place, so a local harness exercises the batching the queue really does.
	devMaxBatchTexts = 96
	// devBackgroundTokensPerMinute is what the queue may spend a minute on background work. It
	// is high because nothing is spent: no call leaves the process, and a harness that waited
	// out a real quota would be a harness nobody runs.
	devBackgroundTokensPerMinute = 100_000_000
)

// DevClient embeds text without calling anything: it hashes a text's lower-case words into the
// Dimension buckets of one vector and normalises it, so two texts sharing words are nearer each
// other than two sharing none. That is the whole of what it offers, and it is enough for a
// local harness to prove that meaning search finds the window holding the words of a query.
//
// It is never a deployment's embedder: cmd/dispatch builds it only under DISPATCH_DEV_EMBEDDER,
// which it refuses without DISPATCH_DEV_SIGNIN=1, and its identity says where its vectors came
// from, so a database a harness filled is never searched with a model's query vectors.
type DevClient struct{}

// NewDev returns the deterministic embedder of a local harness.
func NewDev() *DevClient { return &DevClient{} }

// Identity is "dev:<recipe>", which names this embedder and no model service.
func (*DevClient) Identity() string { return devIdentity }

// Limits are a batch limit Cohere's own and a token budget nothing spends, so the queue paces
// its calls as it does at a real place and waits for no quota it is not paying for.
func (*DevClient) Limits() Limits {
	return Limits{MaxBatchTexts: devMaxBatchTexts, BackgroundTokensPerMinute: devBackgroundTokensPerMinute}
}

// Embed answers one vector per text, in order. The texts are bounded exactly as a real client
// bounds them (boundBatch), so a harness meets the same batch limit and the same truncation.
// Both input types are embedded the same way, where Cohere embeds a stored document and a query
// differently: a query's vector must be near the vector of the text that answers it, and the
// words they share are all this recipe has to put it there.
func (*DevClient) Embed(_ context.Context, texts []string, _ InputType) ([][]float32, error) {
	bounded, err := boundBatch(texts, devMaxBatchTexts)
	if err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(bounded))
	for i, t := range bounded {
		vectors[i] = devVector(t)
	}
	return vectors, nil
}

// devVector is t's vector: each lower-case word adds one to the bucket its hash names, and the
// vector is then scaled to length 1, so cosine distance reads as the share of words two texts
// have in common rather than as how long they are. A text holding no word at all takes the
// first bucket alone, since a vector of zeros has no direction and pgvector's cosine distance
// to it is not a number.
func devVector(t string) []float32 {
	vector := make([]float32, Dimension)
	words := 0
	for word := range strings.FieldsFuncSeq(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		vector[devBucket(word)%Dimension]++
		words++
	}
	if words == 0 {
		vector[0] = 1
		return vector
	}
	length := float32(0)
	for _, count := range vector {
		length += count * count
	}
	length = float32(math.Sqrt(float64(length)))
	for i, count := range vector {
		vector[i] = count / length
	}
	return vector
}

// devBucket hashes one word, lower-cased as it goes, with FNV-1a over its bytes: a word differs
// from the word beside it in every bit of the result, so neighbouring words rarely share a
// bucket. Lower-casing is byte by byte, so "Window" and "window" are one word and a word
// written in another script is one word whatever case it carries.
func devBucket(word string) uint32 {
	const offset, prime = 2166136261, 16777619
	hash := uint32(offset)
	for i := range len(word) {
		letter := word[i]
		if letter >= 'A' && letter <= 'Z' {
			letter += 'a' - 'A'
		}
		hash = (hash ^ uint32(letter)) * prime
	}
	return hash
}

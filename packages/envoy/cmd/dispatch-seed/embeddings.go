package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// embeddingModel is the one `model` value every seeded vector carries, which the command prints
// so a meaning measurement binds the same value the rows hold ($11 of searchQueryMeaning).
const embeddingModel = "seed-embed-v1"

// embeddingDimensions is the width of `embeddings.embedding`, vector(1536) since migration 0072.
const embeddingDimensions = 1536

// embeddingCentres is how many clusters the vectors are drawn around, and embeddingJitter how far
// a row is drawn from its centre. Two random vectors of 1,536 dimensions are near orthogonal, so
// vectors drawn uniformly score about zero against a random query and every meaning arm of search
// would return nothing: below api/search.go's searchMeaningFloor of 0.25 a row never reaches the
// legs at all. A row drawn as normalise(centre + jitter * unit) scores about 1/sqrt(1+jitter^2)
// against its centre - 0.89 at this jitter - and about zero against any other centre, so a query
// taken from a centre finds that centre's rows and only those.
const (
	embeddingCentres = 50
	embeddingJitter  = 0.5
)

// embeddingBatch is how many rows one update statement carries. Each vector is about 11 KB of
// text, so a larger batch would make one statement several hundred megabytes.
const embeddingBatch = 200

// writeEmbeddings fills half the embedding rows the triggers queued. It drops the HNSW index
// first and builds it again after: about 285,000 per-row insertions into an HNSW index at scale 1
// would dominate the run, where one build at the cluster's maintenance memory is a fraction of
// it (the plan's Decision "The seed drops and rebuilds the HNSW index around its vector writes").
func (r *seedRun) writeEmbeddings(ctx context.Context) error {
	if _, err := r.database.Pool.Exec(ctx, `drop index if exists embeddings_cosine`); err != nil {
		return fmt.Errorf("drop the HNSW index: %w", err)
	}
	centres := newEmbeddingCentres()
	kinds, ids := r.embeddedRows()
	random := rand.New(rand.NewPCG(randomSeed+2, randomSeed^0xd1b54a32d192ed03))
	literal := make([]string, 0, embeddingBatch)
	at := baseTime.Add(seedSpan)
	if err := r.inBatches(ctx, "embeddings", len(ids), embeddingBatch,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			literal = literal[:0]
			for index := from; index < to; index++ {
				centre := centres[index%embeddingCentres]
				// The first row of each cluster is the centre itself, so the command can name a
				// row whose vector is exactly the query a measurement binds.
				if index < embeddingCentres {
					literal = append(literal, vectorLiteral(centre))
					continue
				}
				literal = append(literal, vectorLiteral(aroundCentre(centre, random)))
			}
			_, err := tx.Exec(ctx, `
				update embeddings e
				   set embedding = v.embedding::vector, model = $1, embedded_hash = e.content_hash,
				       embedded_at = $2, pending_since = $2, next_attempt_at = $2, attempt_count = 0
				  from unnest($3::text[], $4::text[], $5::text[]) as v (kind, id, embedding)
				 where e.kind = v.kind and e.id = v.id
			`, embeddingModel, at, kinds[from:to], ids[from:to], literal)
			return err
		}); err != nil {
		return err
	}
	if _, err := r.database.Pool.Exec(ctx, `
		create index embeddings_cosine on embeddings using hnsw (embedding vector_cosine_ops) where (embedding is not null)
	`); err != nil {
		return fmt.Errorf("build the HNSW index: %w", err)
	}
	r.subject.Model = embeddingModel
	r.subject.QueryVectorRow = fmt.Sprintf("%s %s", kinds[0], ids[0])
	return nil
}

// embeddedRows is every (kind, id) that gets a vector: every other row of each kind, in plan
// order, which is the half of the issues, documents, asks, comments and messages the spec asks
// for. The triggers queued a row for every one of them when the seed wrote it, so these are
// updates, not inserts.
func (r *seedRun) embeddedRows() (kinds, ids []string) {
	add := func(kind string, count int, id func(int) string) {
		for index := 0; index < count; index += 2 {
			kinds = append(kinds, kind)
			ids = append(ids, id(index))
		}
	}
	add("issue", len(r.plan.issues), func(index int) string { return r.plan.issues[index].Key })
	add("document", len(r.plan.documents), func(index int) string { return r.plan.documents[index].ID })
	add("ask", len(r.plan.asks), func(index int) string { return r.plan.asks[index].ID })
	add("comment", len(r.plan.comments), func(index int) string { return r.plan.comments[index].ID })
	add("message", len(r.plan.messages), func(index int) string { return r.plan.messages[index].ID })
	return kinds, ids
}

// newEmbeddingCentres is the cluster centres, each a unit vector from the run's fixed seed.
func newEmbeddingCentres() [][]float64 {
	random := rand.New(rand.NewPCG(randomSeed+3, randomSeed^0x2545f4914f6cdd1d))
	centres := make([][]float64, embeddingCentres)
	for index := range centres {
		centres[index] = unitVector(random)
	}
	return centres
}

// unitVector is a vector of embeddingDimensions normal components, normalised.
func unitVector(random *rand.Rand) []float64 {
	vector := make([]float64, embeddingDimensions)
	for index := range vector {
		vector[index] = random.NormFloat64()
	}
	return normalise(vector)
}

// aroundCentre is one row's vector: its centre pushed in a random direction by embeddingJitter
// and normalised again.
func aroundCentre(centre []float64, random *rand.Rand) []float64 {
	offset := unitVector(random)
	vector := make([]float64, embeddingDimensions)
	for index := range vector {
		vector[index] = centre[index] + embeddingJitter*offset[index]
	}
	return normalise(vector)
}

func normalise(vector []float64) []float64 {
	length := 0.0
	for _, value := range vector {
		length += value * value
	}
	length = math.Sqrt(length)
	if length == 0 {
		return vector
	}
	for index := range vector {
		vector[index] /= length
	}
	return vector
}

// vectorLiteral is a vector as pgvector reads one. Six decimals is far finer than the differences
// cosine distance turns on here and keeps a row's text to about 11 KB.
func vectorLiteral(vector []float64) string {
	out := make([]byte, 0, embeddingDimensions*10)
	out = append(out, '[')
	for index, value := range vector {
		if index > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendFloat(out, value, 'f', 6, 64)
	}
	return string(append(out, ']'))
}

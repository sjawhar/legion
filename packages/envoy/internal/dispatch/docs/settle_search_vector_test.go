package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/searchtest"
)

// A browser edit that grows a document past Postgres's limit on one search vector settles into a
// version, and the version is found by the words that open it (LEGION-505). 100,000 distinct words
// a hundred to a paragraph (searchtest.DistinctWords) are 1,000 paragraphs, inside every bound a
// document has, and pass that limit. Before 0071 the settlement's version write failed with
// `string is too long for tsvector`, and so did every settlement of the document.
func TestSettlementVersionsADocumentPastTheSearchVectorLimit(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	before := latestVersionNumber(t, service, artifactID)

	editLiveTree(t, service, artifactID, appendBlocks(t, searchtest.DistinctWords(100_000, "\n\n")))
	settleCurrentGeneration(t, service, artifactID)
	waitForDocumentVersion(t, service.store, artifactID, before+1)

	var opening bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select search @@ to_tsquery('english', 'w000001') from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, before+1).Scan(&opening); err != nil {
		t.Fatalf("read the settled version's search vector: %v", err)
	}
	if !opening {
		t.Error("the settled version's search vector does not hold w000001, the document's first word")
	}
}

package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A browser edit that grows a document past Postgres's limit on one search vector settles into a
// version, and the version is found by the words that open it (LEGION-505). 100,000 words no two
// alike, `w000001 w000002 …` a hundred to a paragraph, are 800 KB of markdown and 1,000 paragraphs,
// inside every bound a document has, and their whole vector is 1.2 MB of lexemes and positions,
// past the 1,048,575 bytes Postgres holds in one tsvector. Before 0068 the settlement's version
// write failed with `string is too long for tsvector`, and so did every settlement of the document.
func TestSettlementVersionsADocumentPastTheSearchVectorLimit(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	before := latestVersionNumber(t, service, artifactID)

	var words strings.Builder
	for word := 1; word <= 100_000; word++ {
		fmt.Fprintf(&words, "w%06d", word)
		if word%100 == 0 {
			words.WriteString("\n\n")
		} else {
			words.WriteString(" ")
		}
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, words.String()))
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

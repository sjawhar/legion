package api

import (
	"strings"
	"testing"
)

// An anchor artifact_id that spells a real document id in upper case, or inside braces, casts to
// that document's uuid. The readers that resolve an ask's anchor document (askRowFrom, behind the
// issue's ask list and the single-ask read, and the event log's ask hydration) answer what base
// answered for every anchor that casts: the document it names. Only a value that cannot cast
// resolves to none. Both rows here carry block_artifact_id too, the shape asks_anchor_artifact
// (0032) never cast, which is how an earlier release could leave such a spelling stored.
func TestAskReadersResolveAnUpperCaseOrBracedAnchorToItsDocument(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchor spellings", "The quick brown fox jumps over the lazy dog")
	blockDocument, _ := secondDocument(t, handler, issue.Key)
	spellings := []struct{ name, quote, question, artifactID, askID string }{
		{name: "upper case", quote: "brown", question: "Why brown?", artifactID: strings.ToUpper(issue.PrimaryArtifactID)},
		{name: "braces", quote: "lazy", question: "Why lazy?", artifactID: "{" + issue.PrimaryArtifactID + "}"},
	}
	for i, spelling := range spellings {
		spellings[i].askID = openAnchoredAsk(t, handler, issue.Key, "spec", spelling.quote, spelling.question)
		storeAnchorArtifactID(t, database, spellings[i].askID, blockDocument, `"`+spelling.artifactID+`"`)
	}
	want := anchorDocument(t, database, issue.PrimaryArtifactID)

	asks := issueAsks(t, handler, issue.Key)
	opened := openedAskAnchors(t, handler, issue.Key)
	for _, spelling := range spellings {
		if got := asks[spelling.askID].AnchorArtifact; got == nil || *got != want {
			t.Errorf("issue ask list: the %s anchor's document = %#v, want %#v", spelling.name, got, want)
		}
		if got := singleAsk(t, handler, spelling.askID).AnchorArtifact; got == nil || *got != want {
			t.Errorf("single ask read: the %s anchor's document = %#v, want %#v", spelling.name, got, want)
		}
		if got := opened[spelling.askID]; got == nil || *got != want {
			t.Errorf("issue events: the %s anchor's ask.opened document = %#v, want %#v", spelling.name, got, want)
		}
	}
}

package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The version the human approved holds one block id twice: the agent named it while the live tree
// held, beside the block the human later answered, a second ask node a document-socket peer wrote
// under that block's id. The peer's node then left, the human answered the block, and the agent
// named a version that is row-true and holds the id once. Nothing is carried, on either side of
// the repeat: the stored version is read back through the renderer's parser
// (pmdoc.ParseRendering), whose block-id repair keeps the id on the first node and mints a fresh
// one for the second, so the version he approved reads as holding a block the new version does
// not, and a block of the approved version that no longer stands refuses the carry (carryVerdict);
// askNodes, which both versions are read through, refuses a repeat as well, but the read-back
// leaves it none to refuse. TestAVersionHoldingARepeatedBlockIDCarriesNothing's run without the
// repeat is the control that carries. The service settles a second after a live write, wide
// enough that settlement does not run between the steps and repair or version the repeat on its
// own.
func TestAnApprovedVersionHoldingARepeatedBlockIDCarriesNothing(t *testing.T) {
	database, service, artifactID := seededRegionDocument(t, time.Second)
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

	editLiveTree(t, service, artifactID, appendSecondRegionAsk(t))
	joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, err := service.NamedVersion(ctx, artifactID, "Named by the agent with the repeat", agent)
		return err
	})
	if markdown := storedVersionMarkdown(t, database, artifactID, 2); strings.Count(markdown, "#region") != 2 {
		t.Fatalf("version 2 = %q, want it to hold the block id twice", markdown)
	}
	approveStoredVersion(t, database, artifactID, 2, "alice")

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		kept := tree.Children[:0]
		seen := false
		for _, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); child.Type == "ask" && id == "region" {
				if seen {
					continue
				}
				seen = true
			}
			kept = append(kept, child)
		}
		tree.Children = kept
		return tree
	})
	answerBlockAs(t, service, artifactID, "region", "alice", []string{"East"})
	joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, err := service.NamedVersion(ctx, artifactID, "Named by the agent without the repeat", agent)
		return err
	})
	if markdown := storedVersionMarkdown(t, database, artifactID, 3); strings.Count(markdown, "#region") != 1 || !strings.Contains(markdown, `answered_by="alice"`) {
		t.Fatalf("version 3 = %q, want it to hold the block once, answered by alice", markdown)
	}
	if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 1 || approvals != 0 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version 2 alone and no event", reviews, approvals)
	}
}

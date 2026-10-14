package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
	const spec = "Context\n\n:::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	database := storetest.Open(t)
	artifactID := createDocument(t, database, spec)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Second})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, spec)
	// Settlement indexes the block as an ask of this document, the row the carry reads.
	service.ScheduleSettlement(artifactID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1 and block_id = 'region'`, artifactID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("settlement did not index the block")
		}
		time.Sleep(5 * time.Millisecond)
	}
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		second, err := pmdoc.Parse(":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes last?\n\n- North: The cold one.\n:::\n")
		if err != nil {
			t.Fatal(err)
		}
		tree.Children = append(tree.Children, second.Children...)
		return tree
	})
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

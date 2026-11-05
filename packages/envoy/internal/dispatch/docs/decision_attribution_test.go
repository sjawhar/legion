package docs

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestDecisionPersonalMutationCanClaimOnlyItsWriter(t *testing.T) {
	for _, method := range []string{"upload", "editor", "rebuild"} {
		t.Run(method, func(t *testing.T) {
			database, service := blockIndexService(t)
			source := writeIndexedDocument(t, database, service, "source", ":::decision{#d1}\nA choice.\n:::\n")
			var unloadable *rebuildCaptureStore
			if method == "rebuild" {
				closeDocumentRoom(t, service, source)
				unloadable = &rebuildCaptureStore{VersionedStore: service.persistence}
				service.persistence = unloadable
			}
			write := func(login string) error {
				if unloadable != nil {
					unloadable.invalid = true
				}
				return joinedWrite(service, func(ctx context.Context) error {
					actor := model.Actor{Kind: "user", ID: login}
					markdown := ":::decision{#d1 decided_by=\"alice\"}\nA choice.\n:::\n"
					var err error
					switch method {
					case "upload":
						_, err = service.ReplaceText(ctx, source, markdown, actor)
					case "editor":
						_, err = service.ApplyOps(ctx, source, []model.EditOp{{Op: "retype", Block: "d1", Type: "decision", Attributes: map[string]any{"decided_by": "alice"}}}, actor, nil)
					case "rebuild":
						_, _, err = service.RebuildDocument(ctx, source, &markdown, actor)
					}
					if err != nil || method == "rebuild" {
						return err
					}
					_, err = service.SnapshotVersion(ctx, source, actor)
					return err
				})
			}
			var refused *ErrDecisionAttribution
			if err := write("bob"); !errors.As(err, &refused) || refused.BlockID != "d1" || refused.Decider != "alice" {
				t.Fatalf("%s allowed Bob to claim Alice: %v", method, err)
			}
			if rows := pointerRowsOf(t, database, source); len(rows) != 0 {
				t.Fatalf("refused attribution wrote graph rows: %#v", rows)
			}
			if err := write("alice"); err != nil {
				t.Fatalf("%s refused Alice's own attribution: %v", method, err)
			}
			rows := pointerRowsOf(t, database, source)
			if len(rows) != 1 || rows[0].Kind != "decided_by" || rows[0].ToKind != "person" || rows[0].ToID != "alice" {
				t.Fatalf("self-attribution lost its person relation: %#v", rows)
			}
		})
	}
}

func TestDecisionUncreditedSettlementCannotAuthorizePersonAttribution(t *testing.T) {
	tree, err := pmdoc.Parse(":::decision{#d1 decided_by=\"alice\"}\nA choice.\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	var refused *ErrDecisionAttribution
	if err := refuseDecisionPersonClaim(tree, SettlementActor); !errors.As(err, &refused) {
		t.Fatalf("uncredited settlement authorized a person claim: %v", err)
	}
	if err := refuseDecisionPersonClaim(tree, conversionActor); err != nil {
		t.Fatalf("trusted system conversion lost existing attribution: %v", err)
	}
}

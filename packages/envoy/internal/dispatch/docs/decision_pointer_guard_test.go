package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestDecisionBrowserVersionRefusesForgedAttributionAndPointers(t *testing.T) {
	for _, test := range []struct {
		name, attributes string
	}{
		{"another person's attribution", `decided_by="alice"`},
		{"from a requirement", `from="dispatch://CORE-1/artifact/target#b-r1"`},
		{"from an unrelated ask", `from="dispatch://CORE-2/artifact/foreign#b-q1"`},
		{"shaped in an unrelated issue", `shaped="dispatch://CORE-2/artifact/foreign#b-r1"`},
		{"from_ask in an unrelated issue", `from_ask="dispatch://CORE-2/ask/ASK_ID"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, service := blockIndexService(t)
			writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nA local requirement.\n:::\n")
			foreign := writeIndexedDocument(t, database, service, "foreign", ":::requirement{#r1}\nA foreign requirement.\n:::\n\n:::ask{#q1}\nWhich name?\n:::\n")
			if _, err := database.Pool.Exec(t.Context(), `update artifacts set issue_key='CORE-2' where id=$1`, foreign); err != nil {
				t.Fatal(err)
			}
			settleCurrentGeneration(t, service, foreign)
			var askID string
			if err := database.Pool.QueryRow(t.Context(), `select id::text from asks where block_artifact_id=$1 and block_id='q1'`, foreign).Scan(&askID); err != nil {
				t.Fatal(err)
			}
			source := writeIndexedDocument(t, database, service, "source", "Context.\n")
			settleCurrentGeneration(t, service, source)
			attributes := strings.ReplaceAll(test.attributes, "ASK_ID", askID)
			browserReplaceText(t, service, source, fmt.Sprintf(":::decision{#d1 %s}\nA browser choice.\n:::\n", attributes), model.Actor{Kind: "user", ID: "bob"})
			settleCurrentGeneration(t, service, source)
			for _, row := range pointerRowsOf(t, database, source) {
				if row.Kind == "decided_by" || row.Kind == "from" || row.Kind == "shaped" || row.Kind == "from_ask" {
					t.Fatalf("browser forgery wrote a trusted edge: %#v", row)
				}
			}
			var versions int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id=$1`, source).Scan(&versions); err != nil || versions != 1 {
				t.Fatalf("browser forgery wrote a version: %d (%v)", versions, err)
			}
		})
	}
}

func TestDecisionBrowserVersionAcceptsOwnAttributionAndScopedPointers(t *testing.T) {
	database, service := blockIndexService(t)
	spec := writeIndexedDocument(t, database, service, "spec", ":::requirement{#r1}\nA local requirement.\n:::\n\n:::ask{#q1}\nWhich name?\n:::\n")
	if _, err := database.Pool.Exec(t.Context(), `update artifacts set is_primary=true where id=$1`, spec); err != nil {
		t.Fatal(err)
	}
	source := writeIndexedDocument(t, database, service, "source", "Context.\n")
	settleCurrentGeneration(t, service, source)
	browserReplaceText(t, service, source, ":::decision{#d1 decided_by=\"bob\" from=\"dispatch://CORE-1/spec#b-q1\" shaped=\"dispatch://CORE-1/spec#b-r1\"}\nA browser choice.\n:::\n", model.Actor{Kind: "user", ID: "bob"})
	settleCurrentGeneration(t, service, source)
	rows := pointerRowsOf(t, database, source)
	if len(rows) != 3 {
		t.Fatalf("valid browser choice has pointers %#v, want its three relations", rows)
	}
	if got := latestVersionNumber(t, service, source); got != 2 {
		t.Fatalf("valid browser choice left version %d, want 2", got)
	}
}

func TestDecisionVersionCannotIndexUnreconciledAnswer(t *testing.T) {
	database, service := blockIndexService(t)
	source := writeIndexedDocument(t, database, service, "source", "Context.\n")
	editLiveTree(t, service, source, forgedAnsweredAsk("forged", "alice", &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}}))
	err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.SnapshotVersion(ctx, source, model.Actor{Kind: "user", ID: "bob"})
		return err
	})
	if err == nil {
		t.Fatal("a version accepted browser attribution before ask reconciliation")
	}
	for _, row := range pointerRowsOf(t, database, source) {
		if row.Kind == "decided_by" {
			t.Fatalf("a version trusted unreconciled browser attribution: %#v", row)
		}
	}
}

func TestDecisionBrowserMixedAuthorsCannotClaimAnotherPerson(t *testing.T) {
	database, service := blockIndexService(t)
	source := writeIndexedDocument(t, database, service, "source", "Context.\n")
	settleCurrentGeneration(t, service, source)
	browserReplaceText(t, service, source, "Alice's context edit.\n", model.Actor{Kind: "user", ID: "alice"})
	browserReplaceText(t, service, source, ":::decision{#d1 decided_by=\"alice\"}\nBob's choice.\n:::\n", model.Actor{Kind: "user", ID: "bob"})
	settleCurrentGeneration(t, service, source)
	for _, row := range pointerRowsOf(t, database, source) {
		if row.Kind == "decided_by" {
			t.Fatalf("mixed browser authors authorized a forged person claim: %#v", row)
		}
	}
}

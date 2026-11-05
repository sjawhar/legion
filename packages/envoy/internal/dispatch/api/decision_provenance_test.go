package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestDecisionFromAskResolvesItsActualIssueScope(t *testing.T) {
	for _, scope := range []string{"own issue", "ancestor", "unrelated issue", "forged owner", "missing ask"} {
		t.Run(scope, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{})
			createReferenceAPIProject(t, handler, "CORE")
			owner := createSpecIssue(t, handler, "CORE", ":::ask{#q1}\nWhich choice?\n:::\n")
			questions, _ := waitForOpenQuestions(t, handler, owner.Key, 1)
			ask := questions.OpenQuestions[0].Ask.ID
			writer := owner
			if scope != "own issue" {
				writer = createConversionIssue(t, handler, "The writing issue")
			}
			if scope == "ancestor" {
				if _, err := database.Pool.Exec(t.Context(), `update issues set parent_key=$1 where key=$2`, owner.Key, writer.Key); err != nil {
					t.Fatal(err)
				}
			}
			address := owner.Key
			if scope == "forged owner" {
				address = writer.Key
			}
			if scope == "missing ask" {
				ask = "00000000-0000-4000-8000-000000000001"
				address = writer.Key
			}
			target := "dispatch://" + address + "/ask/" + ask
			response := uploadChecks(t, handler, writer.Key, ":::decision{#d1 from_ask=\""+target+"\"}\nA derived choice.\n:::\n")
			if scope == "own issue" || scope == "ancestor" {
				if response.Code != http.StatusCreated {
					t.Fatalf("valid %s provenance: %d %s", scope, response.Code, response.Body.String())
				}
				checks := readChecks(t, database, writer.Key)
				var count int
				if err := database.Pool.QueryRow(t.Context(), `select count(*) from block_refs where artifact_id=$1 and kind='from_ask' and to_kind='ask' and to_id=$2`, checks.ID, ask).Scan(&count); err != nil || count != 1 {
					t.Fatalf("valid provenance edge count: %d (%v)", count, err)
				}
				return
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "from_ask") {
				t.Fatalf("invalid %s provenance: %d %s, want 400 naming from_ask", scope, response.Code, response.Body.String())
			}
			if checks := readChecks(t, database, writer.Key); checks.ID != "" {
				t.Fatalf("refused provenance created a document: %#v", checks)
			}
		})
	}
}

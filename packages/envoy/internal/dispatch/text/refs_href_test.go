package text

import (
	"encoding/json"
	"os"
	"testing"
)

// The Go reader's half of the shared golden table: every dashboard href Dispatch emits has to
// name the same reference here as its dispatch:// form does, so a pasted search link for an
// anchored comment backlinks the comment and not the document it lives beside. The table is
// generated from `DISPATCH_HREF_REFERENCES` in `@legion/contracts`, which
// `packages/contracts/src/dispatch-href.test.ts` holds this file to; the SPA and the agent
// client walk the same rows.
func TestExtractMatchesTheSharedHrefTable(t *testing.T) {
	const server = "https://dispatch.test"

	raw, err := os.ReadFile("testdata/dispatch-href-references.json")
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	var rows []struct {
		Href string `json:"href"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the shared href table is empty")
	}

	for _, row := range rows {
		t.Run(row.Href, func(t *testing.T) {
			fromHref := Extract(server+row.Href, server)
			fromRef := Extract(row.Ref, server)
			if len(fromRef) != 1 {
				t.Fatalf("%s is not a reference: %#v", row.Ref, fromRef)
			}
			if len(fromHref) != 1 {
				t.Fatalf("%s is not a reference: %#v", row.Href, fromHref)
			}
			if fromHref[0] != fromRef[0] {
				t.Errorf("%s extracted as %#v, %s as %#v", row.Href, fromHref[0], row.Ref, fromRef[0])
			}
		})
	}
}

func TestExtractRefusesAnIssueDocumentHrefNamingTwoItems(t *testing.T) {
	const server = "https://dispatch.test"
	refs := Extract(server+"/issues/CORE-1/spec?comment=c1&ask=a1", server)
	if len(refs) != 1 || refs[0].Kind != "url" {
		t.Fatalf("expected a plain URL reference, got %#v", refs)
	}
}

package pmdoc

import (
	"encoding/json"
	"testing"
)

func TestFromAdmitsABlockOfACommentAndServesDoesNot(t *testing.T) {
	const ref = "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b1"
	for _, name := range []string{"callout", "ask", "decision", "requirement", "plan_task", "check"} {
		t.Run(name, func(t *testing.T) {
			attrs := ""
			if name == "decision" {
				attrs = ` title="Choose"`
			}
			if _, err := ParseRendering(":::" + name + "{#r9" + attrs + " from=\"" + ref + "\"}\nSource.\n:::\n"); err != nil {
				t.Fatalf("from refused a comment block: %v", err)
			}
		})
	}
	if _, err := ParseRendering(":::check{#c1 serves=\"" + ref + "\"}\nCheck.\n:::\n"); err == nil {
		t.Fatal("serves accepted a comment block")
	}
}

func TestAnItemsListIsRefusedOnANonPointerAttribute(t *testing.T) {
	var attr BlockAttributeSchema
	if err := json.Unmarshal([]byte(`{"kind":"string","items":["comment"]}`), &attr); err != nil {
		t.Fatal(err)
	}
	if err := validateAttributeSchema("claim", "note", attr); err == nil {
		t.Fatal("items on a string attribute was accepted")
	}
}

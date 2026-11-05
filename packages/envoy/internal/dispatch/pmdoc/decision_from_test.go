package pmdoc

import (
	"slices"
	"testing"
)

func TestDecisionFromSchemaNamesDocumentAsksOnly(t *testing.T) {
	for _, name := range []string{"callout", "ask", "decision", "requirement", "plan_task", "check"} {
		t.Run(name, func(t *testing.T) {
			typ, ok := typedBlock(name)
			if !ok {
				t.Fatalf("no schema type %q", name)
			}
			attribute, ok := typ.Attributes.Lookup("from")
			if !ok || attribute.Kind != BlockAttributeBlockList || !slices.Equal(attribute.Targets, []string{"ask"}) {
				t.Fatalf("%s.from = %+v (declared %t), want block[] targeting only ask", name, attribute, ok)
			}
		})
	}
}

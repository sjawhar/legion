package decisionmigration

import (
	"strings"
	"testing"
)

func TestConversionPreservesAgentReasoningAndOnlyDropsAskBoilerplate(t *testing.T) {
	const legacy = "# Decisions\n\n## Keep records the decision's text, not its version\n\nKeep records the whole text. Shapes dispatch://INFRA-140/spec#b-035a41cb.\n\n## Use the name the person gave\n\nDecided in dispatch://CORE-1/ask/123. Shapes dispatch://CORE-1/spec#b-name.\n\n## Replaced\n\n### The decisions live in the spec\n\nThe previous design.\n"
	entries, err := Plan(legacy, "source-uuid")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries: %#v", entries)
	}
	if entries[0].Title != "Keep records the decision's text, not its version" || len(entries[0].Shaped) != 1 || !strings.Contains(entries[0].Body, "Keep records the whole text.") || entries[0].Drop {
		t.Fatalf("agent reasoning: %#v", entries[0])
	}
	if !entries[1].Drop || len(entries[1].Asks) != 1 || entries[1].Asks[0] != "123" {
		t.Fatalf("ask restatement: %#v", entries[1])
	}
	if !entries[2].Replaced || entries[2].Drop {
		t.Fatalf("replaced choice: %#v", entries[2])
	}
	again, err := Plan(legacy, "source-uuid")
	if err != nil || again[0].ID != entries[0].ID {
		t.Fatalf("unstable conversion identity: %#v %v", again, err)
	}
}

func TestConversionNeverTreatsAHeadingInCodeAsADecision(t *testing.T) {
	entries, err := Plan("## A choice\n\n```md\n## Not a choice\n```\n\nReason.\n", "source")
	if err != nil || len(entries) != 1 || !strings.Contains(entries[0].Body, "## Not a choice") {
		t.Fatalf("code heading: %#v %v", entries, err)
	}
}

func TestConversionKeepsAmbiguousHumanAttributionForReview(t *testing.T) {
	entries, err := Plan("## About 2 months\n\nSami chose this in dispatch://GTM-8/ask/f0c4c572. The estimate assumes internal models.\n\nUnsure: model access can change.\n", "gtm-source")
	if err != nil || len(entries) != 1 || entries[0].Drop || !entries[0].Review {
		t.Fatalf("ambiguous human wording must not be discarded: %#v %v", entries, err)
	}
}

func TestConversionPreservesProseOutsideHeadingSections(t *testing.T) {
	entries, err := Plan("# Decisions\n\nKeep this context too.\n\n## A choice\n\nIts reasoning.\n", "source")
	if err != nil || len(entries) != 2 || !entries[0].Review || entries[0].Body != "Keep this context too." {
		t.Fatalf("unsectioned content: %#v %v", entries, err)
	}
}

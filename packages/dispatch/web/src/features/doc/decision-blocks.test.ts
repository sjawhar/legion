import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { cutDecisionBlocks } from "./decision-blocks";

/** One of the decision-block fixtures this cut and the Go server's share. */
function fixture(name: string): string {
  return readFileSync(
    join(import.meta.dir, "../../../../../contracts/fixtures/decision-blocks", name),
    "utf8"
  );
}

const approved = fixture("approved.md");
const answered = fixture("answered.md");
const paragraphChanged = fixture("paragraph-changed.md");

test("two versions that differ only in their decision blocks cut to the same text", () => {
  expect(cutDecisionBlocks(approved)).toBe(cutDecisionBlocks(answered));
});

test("a changed paragraph survives the cut", () => {
  expect(cutDecisionBlocks(approved)).not.toBe(cutDecisionBlocks(paragraphChanged));
});

test("every block goes, wherever it stands, and what held it stays", () => {
  const cut = cutDecisionBlocks(answered);
  expect(cut).not.toContain("ask{");
  expect(cut).toBe(
    [
      "# Rollout plan",
      "",
      "The service ships behind a flag.",
      "",
      "Two regions take the first wave, and the rest follow a week later.",
      "",
      "## Decisions",
      "",
      "> The platform team asked for a rollback path.",
      ">",
      "- Announce the dates.",
      "- Pick the regions.",
      "",
      '::::callout{#budget-note kind="note" title="Open question"}',
      "The budget is not settled.",
      "",
      "::::",
      "",
      "The last paragraph closes the plan.",
      "",
    ].join("\n")
  );
});

test("a document with no block is returned unchanged", () => {
  const plain = "# Title\n\nA paragraph.\n\n> A quote.\n\n- One.\n- Two.\n";
  expect(cutDecisionBlocks(plain)).toBe(plain);
  expect(cutDecisionBlocks("")).toBe("");
});

test("a document that is one block cuts to nothing", () => {
  expect(cutDecisionBlocks(':::ask{#only urgency="med"}\nAlone?\n:::\n')).toBe("");
  expect(cutDecisionBlocks(':::ask{#only urgency="med"}\nAlone?\n:::')).toBe("");
});

test("a block opening on a list marker's own line closes at the marker's width", () => {
  expect(cutDecisionBlocks("- :::ask{#first}\n  Which?\n  :::\n- Next item.\n")).toBe(
    "- Next item.\n"
  );
});

test("a typed block nested inside a decision block goes with it", () => {
  expect(
    cutDecisionBlocks(
      '::::ask{#outer}\nWhich?\n:::callout{kind="note"}\nA note.\n:::\n::::\n\nAfter.\n'
    )
  ).toBe("After.\n");
});

test("a block no line closes is left as it is", () => {
  const open = "Before.\n\n:::ask{#open}\nWhich?\n";
  expect(cutDecisionBlocks(open)).toBe(open);
});

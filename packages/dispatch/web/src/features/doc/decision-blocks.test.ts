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

test("a decision block written as an example inside a code fence is text and stays", () => {
  const fenced = fixture("fenced-example.md");
  const cut = cutDecisionBlocks(fenced);
  expect(cut).toContain(
    '```markdown\n:::ask{urgency="high" multiple="false"}\nWhich region goes first?'
  );
  expect(cut).toContain("- West: The busy one.\n:::\n```");
  expect(cut).not.toContain("How many regions take the first wave?");
  expect(cut).not.toContain("#rollout");
  const editedInsideTheFence = fenced.replace(
    "Which region goes first?",
    "Which region goes last?"
  );
  expect(cutDecisionBlocks(editedInsideTheFence)).not.toBe(cut);
  expect(cutDecisionBlocks(editedInsideTheFence)).toContain("Which region goes last?");
});

test("a tilde fence, a fence inside a quote and a fence nothing closes are kept whole too", () => {
  const tilde = "Before.\n\n~~~\n:::ask{#x}\nQ?\n:::\n~~~\n\nAfter.\n";
  expect(cutDecisionBlocks(tilde)).toBe(tilde);
  const quoted = "> ```\n> :::ask{#x}\n> Q?\n> :::\n> ```\n";
  expect(cutDecisionBlocks(quoted)).toBe(quoted);
  const unclosed = "Before.\n\n```\n:::ask{#x}\nQ?\n:::\n";
  expect(cutDecisionBlocks(unclosed)).toBe(unclosed);
  // A fence whose info string holds a backtick is not one to CommonMark, but the renderer writes
  // a code block's language that way and nothing else opens a line with such a run, so the lines
  // it encloses are code and a block written inside them stays.
  const backtickInfo = "``` `lang`\n:::ask{#x}\nQ?\n:::\n```\n\nAfter.\n";
  expect(cutDecisionBlocks(backtickInfo)).toBe(backtickInfo);
});

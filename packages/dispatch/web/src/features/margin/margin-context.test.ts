import { expect, test } from "bun:test";

import type { ComposerKind } from "../conversation/composer-model";
import type { RemarkRefusal } from "../doc/editor";
import { DOCUMENT_CLOSED_REFUSAL, REANCHOR_REFUSALS } from "./margin-context";

const KINDS: readonly ComposerKind[] = ["ask", "comment", "suggestion"];
const REASONS: readonly RemarkRefusal["refused"][] = [
  "ambiguous",
  "gone",
  "unmarkable",
  "unplaceable",
  "unsurrounded",
];

/** The sentence a comment whose words are gone has always read, which the document rows measure
 *  (`packages/dispatch/e2e/compose-wait.e2e.ts`): every other refusal is built around it. */
test("a comment whose words are gone reads exactly as it always has", () => {
  expect(REANCHOR_REFUSALS.gone("comment")).toBe(
    "The text you selected was changed or deleted, so this comment has nothing to attach to. " +
      "Your draft is kept — select the text again to attach it."
  );
});

test("every re-anchor refusal names what the reader is writing, in its own word", () => {
  expect(REASONS.map((reason) => REANCHOR_REFUSALS[reason]("ask"))).toEqual([
    "The words you selected read in more than one place with the same text around them, so " +
      "nothing says which one this ask is about. Your draft is kept — select the text again to " +
      "attach it.",
    "The text you selected was changed or deleted, so this ask has nothing to attach to. " +
      "Your draft is kept — select the text again to attach it.",
    "A suggestion needs whole words inside one table cell, so this ask cannot be marked over " +
      "the text you selected. Your draft is kept — select the text again to attach it.",
    "The text you selected is still in the document, but no longer where you selected it, so " +
      "this ask cannot be attached on its own. Your draft is kept — select the text again to " +
      "attach it.",
    "The text around the words you selected changed, so nothing is left to say which reading " +
      "of them this ask is about. Your draft is kept — select the text again to attach it.",
  ]);
  expect(REASONS.map((reason) => REANCHOR_REFUSALS[reason]("suggestion"))).toEqual([
    "The words you selected read in more than one place with the same text around them, so " +
      "nothing says which one this suggestion is about. Your draft is kept — select the text " +
      "again to attach it.",
    "The text you selected was changed or deleted, so this suggestion has nothing to attach " +
      "to. Your draft is kept — select the text again to attach it.",
    "A suggestion needs whole words inside one table cell, so this suggestion cannot be marked " +
      "over the text you selected. Your draft is kept — select the text again to attach it.",
    "The text you selected is still in the document, but no longer where you selected it, so " +
      "this suggestion cannot be attached on its own. Your draft is kept — select the text " +
      "again to attach it.",
    "The text around the words you selected changed, so nothing is left to say which reading " +
      "of them this suggestion is about. Your draft is kept — select the text again to attach " +
      "it.",
  ]);
  expect(REASONS.map((reason) => REANCHOR_REFUSALS[reason]("comment"))).toEqual([
    "The words you selected read in more than one place with the same text around them, so " +
      "nothing says which one this comment is about. Your draft is kept — select the text " +
      "again to attach it.",
    "The text you selected was changed or deleted, so this comment has nothing to attach to. " +
      "Your draft is kept — select the text again to attach it.",
    "A suggestion needs whole words inside one table cell, so this comment cannot be marked " +
      "over the text you selected. Your draft is kept — select the text again to attach it.",
    "The text you selected is still in the document, but no longer where you selected it, so " +
      "this comment cannot be attached on its own. Your draft is kept — select the text again " +
      "to attach it.",
    "The text around the words you selected changed, so nothing is left to say which reading " +
      "of them this comment is about. Your draft is kept — select the text again to attach it.",
  ]);
});

test("no re-anchor refusal calls an ask or a suggestion a comment, and each keeps the draft", () => {
  for (const kind of KINDS) {
    for (const reason of REASONS) {
      const text = REANCHOR_REFUSALS[reason](kind);
      expect({ kind, reason, says: text.includes(`this ${kind}`) }).toEqual({
        kind,
        reason,
        says: true,
      });
      if (kind !== "comment") {
        expect({ kind, reason, miscalled: text.includes("this comment") }).toEqual({
          kind,
          reason,
          miscalled: false,
        });
      }
      // Whichever of the three the reader meets, the draft is where they left it and a fresh
      // selection is the way out.
      expect({ kind, reason, ends: text.endsWith("select the text again to attach it.") }).toEqual({
        kind,
        reason,
        ends: true,
      });
      expect({
        kind,
        reason,
        kept: text.includes("Your draft is kept"),
      }).toEqual({ kind, reason, kept: true });
    }
  }
});

/** Words still on the page were not changed or deleted, and saying so sends the reader looking
 *  for an edit nobody made: that sentence belongs to `"gone"` alone. */
test("only the refusal for words that read nowhere says they were changed or deleted", () => {
  for (const kind of KINDS) {
    expect({ kind, says: REANCHOR_REFUSALS.gone(kind).includes("was changed or deleted") }).toEqual(
      { kind, says: true }
    );
    for (const reason of ["ambiguous", "unmarkable", "unplaceable", "unsurrounded"] as const) {
      expect({
        kind,
        reason,
        says: REANCHOR_REFUSALS[reason](kind).includes("changed or deleted"),
      }).toEqual({ kind, reason, says: false });
    }
  }
});

/** The refusal for a block that still reads the words while nothing around them says which
 *  reading was the reader's: the word in front of their own reading was edited, or their own
 *  sentence was deleted and the reading left is one they never selected
 *  (`packages/dispatch/e2e/compose-wait.e2e.ts`). Both are the text around their words changing,
 *  and neither is the words being taken out of the document, so it must say neither that nor
 *  that the words moved. */
test("the refusal for changed surroundings tells the reader what did change, and what did not", () => {
  for (const kind of KINDS) {
    const text = REANCHOR_REFUSALS.unsurrounded(kind);
    expect({
      kind,
      moved: text.includes("no longer where you selected it"),
      says: text.includes("The text around the words you selected changed"),
      which: text.includes("which reading of them"),
    }).toEqual({ kind, moved: false, says: true, which: true });
  }
});

/** The refusal for two readings of the reader's own block that the same text surrounds: their
 *  words are exactly where they selected them and the text around them is the text they read
 *  there, so the sentence may say neither that the words moved nor that their surroundings
 *  changed. What it has to say is the one true thing left: the words read in more than one
 *  place alike, so nothing says which of them the draft is about. */
test("the refusal for readings nothing tells apart says why, and claims no edit", () => {
  for (const kind of KINDS) {
    const text = REANCHOR_REFUSALS.ambiguous(kind);
    expect({
      kind,
      changed: text.includes("changed"),
      moved: text.includes("no longer where you selected it"),
      says: text.includes("read in more than one place with the same text around them"),
      which: text.includes(`which one this ${kind} is about`),
    }).toEqual({ kind, changed: false, moved: false, says: true, which: true });
  }
});

/** The compose whose document is not on screen: the margin has no editor to read the words
 *  with, so it knows nothing about them, and the way out is opening that document rather than
 *  selecting text in the one the reader is looking at. The draft-kept promise stays; the tail
 *  every other refusal ends on would be the wrong instruction here. */
test("the refusal for a document that is not open says so, and names opening it as the way out", () => {
  expect(DOCUMENT_CLOSED_REFUSAL("comment")).toBe(
    "The document you selected the text in is not open, so there is nothing here to attach " +
      "this comment to. Your draft is kept — open that document and send it again to attach it."
  );
  for (const kind of KINDS) {
    const text = DOCUMENT_CLOSED_REFUSAL(kind);
    expect({
      kind,
      changed: text.includes("changed or deleted"),
      kept: text.includes("Your draft is kept"),
      names: text.includes(`this ${kind}`),
      selectAgain: text.includes("select the text again"),
    }).toEqual({ kind, changed: false, kept: true, names: true, selectAgain: false });
  }
});

import { expect, test } from "bun:test";

import type { ComposerKind } from "../conversation/composer-model";
import type { RemarkRefusal } from "../doc/editor";
import { REANCHOR_REFUSALS } from "./margin-context";

const KINDS: readonly ComposerKind[] = ["ask", "comment", "suggestion"];
const REASONS: readonly RemarkRefusal["refused"][] = ["gone", "unmarkable", "unplaceable"];

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
    "The text you selected was changed or deleted, so this ask has nothing to attach to. " +
      "Your draft is kept — select the text again to attach it.",
    "A suggestion needs whole words inside one table cell, so this ask cannot be marked over " +
      "the text you selected. Your draft is kept — select the text again to attach it.",
    "The text you selected is still in the document, but no longer where you selected it, so " +
      "this ask cannot be attached on its own. Your draft is kept — select the text again to " +
      "attach it.",
  ]);
  expect(REASONS.map((reason) => REANCHOR_REFUSALS[reason]("suggestion"))).toEqual([
    "The text you selected was changed or deleted, so this suggestion has nothing to attach " +
      "to. Your draft is kept — select the text again to attach it.",
    "A suggestion needs whole words inside one table cell, so this suggestion cannot be marked " +
      "over the text you selected. Your draft is kept — select the text again to attach it.",
    "The text you selected is still in the document, but no longer where you selected it, so " +
      "this suggestion cannot be attached on its own. Your draft is kept — select the text " +
      "again to attach it.",
  ]);
  expect(REASONS.map((reason) => REANCHOR_REFUSALS[reason]("comment"))).toEqual([
    "The text you selected was changed or deleted, so this comment has nothing to attach to. " +
      "Your draft is kept — select the text again to attach it.",
    "A suggestion needs whole words inside one table cell, so this comment cannot be marked " +
      "over the text you selected. Your draft is kept — select the text again to attach it.",
    "The text you selected is still in the document, but no longer where you selected it, so " +
      "this comment cannot be attached on its own. Your draft is kept — select the text again " +
      "to attach it.",
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
    for (const reason of ["unmarkable", "unplaceable"] as const) {
      expect({
        kind,
        reason,
        says: REANCHOR_REFUSALS[reason](kind).includes("changed or deleted"),
      }).toEqual({ kind, reason, says: false });
    }
  }
});

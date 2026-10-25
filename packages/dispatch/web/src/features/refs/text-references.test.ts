import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";

import {
  buildDispatchReference,
  questionReference,
  referenceRouteFromHref,
  textReferences,
} from "./routes";

const origin = "https://dispatch.test";

/** What a text cites by `textReferences`, read as the server's `text.Mentions` reads it with the
 *  row's resolver: each address that names a Dispatch item, each key whose issue exists and each
 *  `#id` the question map names, in order of first appearance. */
function cited(
  body: string,
  server: string,
  issues: readonly string[],
  questions: ReadonlyMap<string, string>
): string[] {
  const references = new Set<string>();
  for (const reference of textReferences(body)) {
    if (reference.kind === "address") {
      const route = referenceRouteFromHref(reference.value, server);
      if (route !== undefined) {
        references.add(buildDispatchReference(route));
      }
    } else if (reference.kind === "key") {
      if (issues.includes(reference.key)) {
        references.add(`dispatch://${reference.key}`);
      }
    } else {
      const question = questionReference(reference.id, questions);
      if (question !== undefined) {
        references.add(question.ask);
      }
    }
  }
  return [...references];
}

// The dashboard's reader of what a text writes is the server's, character for character: every
// row of the shared table, read with the row's existing issues and question map, cites what
// `text.Mentions` indexes for it (`refs`), code and link destinations included, where the reader
// has no markdown to tell them apart.
test.each(
  DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row] as const)
)("%s reads as the server reads it", (_body, row) => {
  expect(
    cited(
      row.body,
      row.origin ?? origin,
      row.issues ?? [],
      new Map(Object.entries(row.questions ?? {}))
    )
  ).toEqual([...row.refs]);
});

test("a key or #id is read only where it stands alone, and its span ends where the server's does", () => {
  expect(
    textReferences("CORE-1's spec, _see #q2_, legion/CORE-2 and dispatch://CORE-3/spec#q4.")
  ).toEqual([
    { end: 6, key: "CORE-1", kind: "key", start: 0 },
    { end: 24, id: "q2_", kind: "question", start: 20 },
    {
      end: 69,
      kind: "address",
      start: 44,
      value: "dispatch://CORE-3/spec#q4",
    },
  ]);
  // The run without its trailing `_` is what names the question, and what a link covers.
  expect(questionReference("q2_", new Map([["q2", "dispatch://CORE-1/ask/a2"]]))).toEqual({
    ask: "dispatch://CORE-1/ask/a2",
    id: "q2",
  });
  expect(questionReference("q2_", new Map([["q2_", "dispatch://CORE-1/ask/a3"]]))).toEqual({
    ask: "dispatch://CORE-1/ask/a3",
    id: "q2_",
  });
  expect(questionReference("q9", new Map([["q2", "dispatch://CORE-1/ask/a2"]]))).toBeUndefined();
  // A key of more than ten characters before its hyphen, a zero, or a lowercase letter is none.
  expect(textReferences("ABCDEFGHIJK-1 CORE-0 Core-1 AB-1")).toEqual([
    { end: 32, key: "AB-1", kind: "key", start: 28 },
  ]);
  // A numeric reference to whitespace stands for the space stored markdown writes at a mark's
  // edge; one to any other character does not.
  expect(textReferences("on&#x20;CORE-1 and&#65;CORE-2")).toEqual([
    { end: 14, key: "CORE-1", kind: "key", start: 8 },
  ]);
});

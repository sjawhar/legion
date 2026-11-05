import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES, isIdPrefix } from "@legion/contracts";

import { readTextReferences } from "./text-references";

const origin = "https://dispatch.test";

/** The references a reading names, in order of first appearance, as a row's `refs` lists them. */
function cited(body: string, facts: Parameters<typeof readTextReferences>[1]): string[] {
  const seen = new Set<string>();
  for (const found of readTextReferences(body, facts).references) {
    if (found.reference !== undefined) {
      seen.add(found.reference);
    }
  }
  return [...seen];
}

// The reader's half of the shared text table: every row, the ones a bare key, a `#id` or an id
// prefix writes included, read against what the row says exists. The Go reader `text.Mentions`
// walks the same rows, so neither where a reference ends nor what it names can change on one
// side alone.
test.each(
  DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row.origin ?? origin, row] as const)
)("%s cites, read against %s, its row's references", (body, server, row) => {
  expect(
    cited(body, {
      issues: row.issues,
      items: row.items,
      origin: server,
      questions: row.questions,
    })
  ).toEqual([...row.refs]);
});

// What a caller that has asked the server nothing would have to ask: the distinct keys, whether
// a `#id` is there, and each reference whose id is written short. A hook starts exactly these.
test("a reading answers what it would have to ask before it has asked anything", () => {
  const { needs, references } = readTextReferences(
    "Both CORE-1 and ACME-12 are blocked; CORE-1 again, see #q2 and dispatch://CORE-1/ask/2a7f1c90.",
    { origin }
  );

  expect(needs).toEqual({
    keys: ["CORE-1", "ACME-12"],
    prefixes: ["dispatch://CORE-1/ask/2a7f1c90"],
    questions: true,
  });
  // Nothing is resolved yet, and every candidate says so.
  expect(references.map(({ form, text, unresolved }) => ({ form, text, unresolved }))).toEqual([
    { form: "key", text: "CORE-1", unresolved: true },
    { form: "key", text: "ACME-12", unresolved: true },
    { form: "key", text: "CORE-1", unresolved: true },
    { form: "question", text: "#q2", unresolved: true },
    { form: "address", text: "dispatch://CORE-1/ask/2a7f1c90", unresolved: true },
  ]);
});

test("a text with nothing to resolve asks nothing", () => {
  const { needs, references } = readTextReferences("Tied to dispatch://CORE-1 here.", { origin });

  expect(needs).toEqual({ keys: [], prefixes: [], questions: false });
  expect(references.map((found) => found.reference)).toEqual(["dispatch://CORE-1"]);
  expect(references[0].unresolved).toBe(false);
});

// The id prefix pattern, which the server spells `text.IsIDPrefix`: a hex digit and seven or
// more hex digits and hyphens, read without case, and never a whole uuid.
test.each([
  ["2a7f1c90", true],
  ["2A7F1C90", true],
  ["2a7f1c90-1f4e", true],
  ["2a7f1c9", false],
  ["xa7f1c90", false],
  ["2a7f1c9g", false],
  ["2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b", false],
  ["2a7f1c901f4e4a2b9c3d5e6f70819a2b", false],
] as const)("%s is an id prefix: %p", (value, expected) => {
  expect(isIdPrefix(value)).toBe(expected);
});

import { expect, test } from "bun:test";

import type { BlockSchema } from "../../api/types";
import { compiledBlockSchema, schemaCreatable } from "./schema";

/** The compiled manifest with one more typed block, "future", which builds read from schema
 *  version 2 and create from schema version `creatableSince`. */
function withFutureBlock(creatableSince: number): BlockSchema {
  return {
    ...compiledBlockSchema,
    types: [
      ...compiledBlockSchema.types,
      {
        attributes: {},
        content: "block+",
        creatable_since: creatableSince,
        name: "future",
        render: "host",
        since: 2,
      },
    ],
  };
}

// The page presents the manifest it was built with: its version V, and R, the highest `since`
// among the elements it may create (`creatable_since` V or less), as Go's `pmdoc.SchemaCreatable`
// computes it. Every element of today's manifest is read and created from version 1.
test("the compiled manifest is the page's own, and everything it creates is read from version 1", () => {
  expect(compiledBlockSchema.version).toBe(2);
  expect(compiledBlockSchema.nodes?.map(({ name }) => name)).toContain("paragraph");
  expect(compiledBlockSchema.types.map(({ name }) => name)).toEqual(["callout", "ask"]);
  expect(schemaCreatable(compiledBlockSchema)).toBe(1);
});

test("an element the page creates counts toward its creatable version, one it only reads does not", () => {
  expect(schemaCreatable(withFutureBlock(2))).toBe(2);
  expect(schemaCreatable(withFutureBlock(3))).toBe(1);
});

test("nodes, marks, attributes and enum choices each count toward the creatable version", () => {
  const ask = compiledBlockSchema.types.find(({ name }) => name === "ask");
  const urgency = ask?.attributes.urgency;
  if (ask === undefined || urgency === undefined) {
    throw new Error("the compiled manifest declares the ask block's urgency");
  }
  const withAsk = (attributes: BlockSchema["types"][number]["attributes"]): BlockSchema => ({
    ...compiledBlockSchema,
    types: compiledBlockSchema.types.map((type) =>
      type.name === "ask" ? { ...ask, attributes: { ...ask.attributes, ...attributes } } : type
    ),
  });

  expect(
    schemaCreatable({
      ...compiledBlockSchema,
      nodes: [
        ...(compiledBlockSchema.nodes ?? []),
        { creatable_since: 2, name: "aside", since: 2 },
      ],
    })
  ).toBe(2);
  expect(
    schemaCreatable({
      ...compiledBlockSchema,
      marks: [...(compiledBlockSchema.marks ?? []), { creatable_since: 3, name: "glow", since: 2 }],
    })
  ).toBe(1);
  expect(
    schemaCreatable(withAsk({ due: { creatable_since: 2, kind: "timestamp", since: 2 } }))
  ).toBe(2);
  expect(
    schemaCreatable(
      withAsk({
        urgency: {
          ...urgency,
          choice_versions: { critical: { creatable_since: 2, since: 2 } },
          choices: [...(urgency.choices ?? []), "critical"],
        },
      })
    )
  ).toBe(2);
});

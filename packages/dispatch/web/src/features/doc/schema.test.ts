import { expect, test } from "bun:test";

import type { BlockSchema } from "../../api/types";
import { BlockSchemaCache } from "./schema";

const schema: BlockSchema = {
  types: [
    {
      attributes: {
        kind: { choices: ["note", "warning"], default: "note", kind: "enum" },
        title: { default: "", kind: "string" },
      },
      content: "paragraph+",
      name: "callout",
      render: "host",
    },
  ],
  version: 1,
};

test("BlockSchemaCache fetches once and retains schemas by version", async () => {
  const cache = new BlockSchemaCache();
  let requests = 0;
  const fetchSchema = async () => {
    requests += 1;
    return schema;
  };

  const [first, second] = await Promise.all([cache.load(fetchSchema), cache.load(fetchSchema)]);

  expect(first).toBe(schema);
  expect(second).toBe(schema);
  expect(requests).toBe(1);
  expect(cache.forVersion(1)).toBe(schema);
  expect(cache.forVersion(2)).toBeUndefined();
});

test("BlockSchemaCache does not retain a failed fetch: the next load fetches again", async () => {
  const cache = new BlockSchemaCache();
  let requests = 0;
  const fetchSchema = async () => {
    requests += 1;
    if (requests === 1) {
      throw new Error("schema unavailable");
    }
    return schema;
  };

  await expect(cache.load(fetchSchema)).rejects.toThrow("schema unavailable");
  expect(cache.forVersion(1)).toBeUndefined();

  expect(await cache.load(fetchSchema)).toBe(schema);
  expect(requests).toBe(2);
  expect(cache.forVersion(1)).toBe(schema);
});

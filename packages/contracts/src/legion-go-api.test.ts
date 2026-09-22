import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import * as barrel from "./index";
import { LegionGoStateResponse } from "./legion-go-api";

const fixtureDir = path.join(import.meta.dir, "..", "fixtures", "daemon-api");

/** Every fixture the Go golden test writes, with the schema that must accept it. A new fixture
 * with no entry here fails the first test rather than going unchecked. */
const schemas: Record<string, typeof LegionGoStateResponse> = {
  "state.json": LegionGoStateResponse,
};

function fixture(name: string): unknown {
  return JSON.parse(readFileSync(path.join(fixtureDir, name), "utf8"));
}

test("every Go-written fixture parses through the strict schema", () => {
  const names = readdirSync(fixtureDir).filter((name) => name.endsWith(".json"));
  expect(names.length).toBeGreaterThan(0);

  for (const name of names) {
    const schema = schemas[name];
    expect(schema, `${name} has no schema in legion-go-api.test.ts`).toBeDefined();
    const parsed = schema?.safeParse(fixture(name));
    expect(parsed?.error?.issues ?? [], `${name} failed the schema`).toEqual([]);
  }
});

test("a field the Go shape does not carry is refused", () => {
  const mutated = fixture("state.json") as Record<string, unknown>;
  mutated.workerAdmission = { queue: [] };

  expect(LegionGoStateResponse.safeParse(mutated).success).toBeFalse();
});

test("a field the Go shape requires cannot be dropped", () => {
  const mutated = fixture("state.json") as { admission: Record<string, unknown> };
  delete mutated.admission.waiting;

  expect(LegionGoStateResponse.safeParse(mutated).success).toBeFalse();
});

test("the Go daemon's schema is reachable from the package barrel", () => {
  expect(barrel.LegionGoStateResponse).toBe(LegionGoStateResponse);
});

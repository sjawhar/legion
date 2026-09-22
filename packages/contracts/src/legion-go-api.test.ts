import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import type { z } from "zod";
import {
  LegionGoErrorResponse,
  LegionGoOperatorClaimResponse,
  LegionGoOperatorClaimsResponse,
  LegionGoRegisterResponse,
  LegionGoStateResponse,
} from "./legion-go-api";

const fixtureDir = path.join(import.meta.dir, "..", "fixtures", "daemon-api");

/** Every fixture the Go golden test writes, with the schema that must accept it. A new fixture
 * with no entry here fails the first test rather than going unchecked. */
const schemas: Record<string, z.ZodType> = {
  "state.json": LegionGoStateResponse,
  "register.json": LegionGoRegisterResponse,
  "error.json": LegionGoErrorResponse,
  "operator-claim.json": LegionGoOperatorClaimResponse,
  "operator-claims.json": LegionGoOperatorClaimsResponse,
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

test("a locator is the runtime's nested shape, never the flat one", () => {
  const mutated = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  mutated.locator = {
    runtime: "tmux",
    claim: "legion-legion-legion-209-implementer",
    incarnation: "40217:9551230",
    window: "@7",
    pane: "%23",
  };

  expect(LegionGoOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a locator's backend member is the one its runtime names", () => {
  const mutated = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  mutated.locator = { ...mutated.locator, runtime: "sandbox" };

  expect(LegionGoOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a claim state is one the supervisor has", () => {
  const mutated = fixture("operator-claim.json") as Record<string, unknown>;
  mutated.state = "running";

  expect(LegionGoOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a register response without its secret is refused", () => {
  const mutated = fixture("register.json") as Record<string, unknown>;
  delete mutated.secret;

  expect(LegionGoRegisterResponse.safeParse(mutated).success).toBeFalse();
});

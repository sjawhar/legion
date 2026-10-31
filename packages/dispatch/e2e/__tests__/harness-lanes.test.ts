import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

import {
  asLane,
  harnessPorts,
  laneConflicts,
  laneCount,
  laneValue,
  laneVariable,
  refuseLaneConflicts,
  workerCount,
} from "../harness-ports";

// The preload (web/src/__tests__/setup.ts) puts happy-dom's Response on globalThis, which cannot
// read a subprocess's pipe; the cases below read one with Bun's own.
const NativeResponse = (await Bun.fetch("data:,")).constructor as typeof Response;
const runScript = fileURLToPath(new URL("../run.ts", import.meta.url));
const psqlModule = fileURLToPath(new URL("../psql.ts", import.meta.url));
const config = fileURLToPath(new URL("../playwright.config.ts", import.meta.url));
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

async function run(argv: string[], env: Record<string, string>) {
  const child = Bun.spawn(argv, {
    cwd: dispatchPackage,
    env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", ...env },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new NativeResponse(child.stderr).text(),
    new NativeResponse(child.stdout).text(),
  ]);
  return { code, stderr, stdout };
}

test("a worker reads its own lane's variable, and the plain name when its lane sets none", () => {
  const env = {
    DISPATCH_E2E_LANE_1_FAKE_ENVOY_PORT: "9101",
    DISPATCH_E2E_LANES: "2",
    FAKE_ENVOY_PORT: "9100",
    TEST_PARALLEL_INDEX: "1",
  };
  expect(laneVariable(1, "FAKE_ENVOY_PORT")).toBe("DISPATCH_E2E_LANE_1_FAKE_ENVOY_PORT");
  expect(laneValue(env, "FAKE_ENVOY_PORT")).toBe("9101");
  expect(laneValue(env, "DATABASE_URL")).toBeUndefined();
  expect(laneValue({ ...env, DATABASE_URL: "postgres://plain" }, "DATABASE_URL")).toBe(
    "postgres://plain"
  );
  // Lane 0's own names win over the plain ones too, and a process with no index is lane 0.
  const lane0 = { ...env, DISPATCH_E2E_LANE_0_FAKE_ENVOY_PORT: "9102", TEST_PARALLEL_INDEX: "" };
  expect(laneValue(lane0, "FAKE_ENVOY_PORT")).toBe("9102");
  expect(laneValue(asLane(1, lane0), "FAKE_ENVOY_PORT")).toBe("9101");
});

test("harnessPorts resolves the lane's ports and keeps both of its refusals", () => {
  const env = {
    DISPATCH_E2E_LANE_0_FAKE_ENVOY_PORT: "9100",
    DISPATCH_E2E_LANE_1_DISPATCH_E2E_PORT: "9201",
    DISPATCH_E2E_LANE_1_FAKE_ENVOY_PORT: "9101",
    DISPATCH_E2E_PORT: "9200",
  };
  expect(harnessPorts(asLane(1, env)).fakeEnvoy.port).toBe(9101);
  expect(harnessPorts(asLane(1, env)).dispatch.port).toBe(9201);
  // Lane 1 names no plain-HTTP port, so it falls back to the plain name, unset here.
  expect(harnessPorts(asLane(1, env)).plainHttp.port).toBe(0);
  // Lane 0 reads the one port DISPATCH_E2E_PORT names.
  expect(harnessPorts(asLane(0, env)).dispatch.port).toBe(9200);
  expect(() =>
    harnessPorts(asLane(1, { ...env, DISPATCH_E2E_LANE_1_FAKE_ENVOY_PORT: "0x2249" }))
  ).toThrow("FAKE_ENVOY_PORT must be a port number");
  expect(() =>
    harnessPorts(asLane(1, { ...env, DISPATCH_E2E_LANE_1_DISPATCH_E2E_PORT: "9101" }))
  ).toThrow("DISPATCH_E2E_PORT and FAKE_ENVOY_PORT name port 9101");
});

test("the worker count is a number from 1 up, and the lane count is what run.ts exported", () => {
  expect(workerCount({})).toBe(1);
  expect(workerCount({ DISPATCH_E2E_WORKERS: "" })).toBe(1);
  expect(workerCount({ DISPATCH_E2E_WORKERS: "4" })).toBe(4);
  for (const value of ["0", "-1", "two", " 2", "2.5"]) {
    expect(() => workerCount({ DISPATCH_E2E_WORKERS: value })).toThrow(
      `DISPATCH_E2E_WORKERS must be how many lanes the run has, a number from 1 up, not ${JSON.stringify(value)}`
    );
  }
  expect(laneCount({ DISPATCH_E2E_LANES: "3", DISPATCH_E2E_WORKERS: "3" })).toBe(3);
  // A run against servers a person started has no DISPATCH_E2E_LANES of run.ts's.
  expect(laneCount({ DISPATCH_E2E_WORKERS: "2" })).toBe(2);
  expect(laneCount({})).toBe(1);
});

test("a run of more than one lane refuses every variable one lane owns", () => {
  expect(laneConflicts({})).toEqual([]);
  expect(laneConflicts({ DISPATCH_E2E_PROCESSES: "1" })).toEqual([]);
  expect(
    laneConflicts({
      DATABASE_URL: "postgres://x",
      DISPATCH_E2E_PROCESSES: "2",
      DISPATCH_E2E_REUSE_SERVERS: "1",
      FAKE_ENVOY_PORT: "9021",
      PLAYWRIGHT_BASE_URL: "http://127.0.0.1:1",
    })
  ).toEqual([
    "DATABASE_URL",
    "DISPATCH_E2E_PROCESSES",
    "DISPATCH_E2E_REUSE_SERVERS",
    "FAKE_ENVOY_PORT",
    "PLAYWRIGHT_BASE_URL",
  ]);
  expect(() => refuseLaneConflicts(2, { DATABASE_URL: "postgres://x" })).toThrow(
    "DISPATCH_E2E_WORKERS=2 runs 2 lanes, each a harness of its own, so DATABASE_URL must not be set"
  );
  expect(() => refuseLaneConflicts(2, { DISPATCH_E2E_PORT: "9020" })).toThrow(
    "DISPATCH_E2E_PORT must not be set: a port is one listener's"
  );
  expect(() => refuseLaneConflicts(2, {})).not.toThrow();
});

test("e2e/run.ts refuses each of them before it starts anything", async () => {
  const conflicts: Record<string, string> = {
    DATABASE_URL: "postgres://x",
    DISPATCH_E2E_PROCESSES: "2",
    DISPATCH_E2E_REUSE_SERVERS: "1",
    FAKE_ENVOY_PORT: "9021",
    PLAYWRIGHT_BASE_URL: "http://127.0.0.1:1",
  };
  for (const [variable, value] of Object.entries(conflicts)) {
    const refused = await run(["bun", runScript, "--", "true"], {
      DISPATCH_E2E_WORKERS: "2",
      [variable]: value,
    });
    expect(refused.code).not.toBe(0);
    expect(refused.stderr).toContain(`e2e/run.ts: DISPATCH_E2E_WORKERS=2 runs 2 lanes`);
    expect(refused.stderr).toContain(`${variable} must not be set`);
  }
  // With no conflict and no provider, the refusal is the missing Postgres, and still before a
  // server starts.
  const noProvider = await run(["bun", runScript, "--", "true"], { DISPATCH_E2E_WORKERS: "2" });
  expect(noProvider.code).not.toBe(0);
  expect(noProvider.stderr).toContain("DISPATCH_E2E_PG_IMAGE");
  expect(noProvider.stderr).toContain("DISPATCH_E2E_PG_BIN");
});

// The database a lane reads is the psql it starts, so the cases that prove which one it picked
// need a live database to answer; where none is named they would prove nothing and are skipped,
// as e2e/__tests__/psql-session.test.ts's are.
const live = (process.env.DATABASE_URL ?? "").trim();

test.skipIf(live === "")("psql reads its own lane's database", async () => {
  // A process of its own per case, which imports e2e/psql.ts in the environment under test: it
  // resolves the database once, when it starts its psql.
  const resolve = [
    "bun",
    "-e",
    `await (await import(${JSON.stringify(psqlModule)})).sql("select 1")`,
  ];
  const dead = "postgres://postgres@127.0.0.1:1/never-opened?sslmode=disable";
  // Lane 1's database answers; the plain name, which lane 1 must not read, is a dead port.
  const lane = await run(resolve, {
    DATABASE_URL: dead,
    DISPATCH_E2E_LANE_1_DATABASE_URL: live,
    DISPATCH_E2E_LANES: "2",
    TEST_PARALLEL_INDEX: "1",
  });
  expect(`${lane.stderr}${lane.stdout}`).toBe("");
  expect(lane.code).toBe(0);
  // The same environment without the lane's index is lane 0, which reads the plain name.
  const plain = await run(resolve, { DATABASE_URL: dead, DISPATCH_E2E_LANE_1_DATABASE_URL: live });
  expect(plain.code).not.toBe(0);
});

test("psql names both variables when neither its lane nor the plain name has a database", async () => {
  const refused = await run(
    ["bun", "-e", `await (await import(${JSON.stringify(psqlModule)})).sql("select 1")`],
    { DISPATCH_E2E_LANES: "2", TEST_PARALLEL_INDEX: "1" }
  );
  expect(refused.code).not.toBe(0);
  expect(`${refused.stderr}${refused.stdout}`).toContain(
    "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
  );
});

test("the Playwright config refuses a --workers that is not the lane count", async () => {
  const listed = ["bunx", "playwright", "test", "--config", config, "--list", "--reporter=line"];
  const mismatched = await run([...listed, "--workers", "3"], {});
  expect(mismatched.code).not.toBe(0);
  expect(`${mismatched.stderr}${mismatched.stdout}`).toContain(
    "This run has 1 lane, each a harness of its own, so --workers 3 would not match"
  );
  const shorthand = await run([...listed, "-j2"], { DISPATCH_E2E_LANES: "3" });
  expect(shorthand.code).not.toBe(0);
  expect(`${shorthand.stderr}${shorthand.stdout}`).toContain(
    "This run has 3 lanes, each a harness of its own, so --workers 2 would not match"
  );
});

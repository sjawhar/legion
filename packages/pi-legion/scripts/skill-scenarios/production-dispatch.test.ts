import { afterAll, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  checkoutAbove,
  type Fetch,
  inWindow,
  mapLimit,
  ProductionDispatch,
  parseInstant,
  parseWindow,
  resolveDispatch,
} from "./production-dispatch";

const scratch = mkdtempSync(path.join(tmpdir(), "production-dispatch-"));
afterAll(() => rmSync(scratch, { recursive: true, force: true }));

function dir(name: string): string {
  const made = path.join(scratch, name);
  mkdirSync(made, { recursive: true });
  return made;
}

interface Sent {
  readonly url: URL;
  readonly init: RequestInit;
}

/** A fake Dispatch: `answer` gives the response for each request, and every request is kept. */
function fakeFetch(answer: (url: URL) => Response): { fetch: Fetch; sent: Sent[] } {
  const sent: Sent[] = [];
  return {
    sent,
    fetch: async (input, init) => {
      const url = new URL(input);
      sent.push({ url, init });
      return answer(url);
    },
  };
}

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const credentials = { url: "http://dispatch.test", token: "dispatch-token" };
const noSleep = async () => {};

test("DISPATCH_URL with DISPATCH_TOKEN gives the server and the token", () => {
  const home = dir("home-variables");
  expect(
    resolveDispatch(
      { HOME: home, DISPATCH_URL: "http://dispatch.test/", DISPATCH_TOKEN: "dispatch-token" },
      home
    )
  ).toEqual(credentials);
});

test("the envoy.json ENVOY_CONFIG_FILE names gives the server and the token", () => {
  const home = dir("home-file");
  const file = path.join(home, "envoy.json");
  writeFileSync(
    file,
    JSON.stringify({ dispatch: { serverUrl: "http://dispatch.test", token: "dispatch-token" } })
  );
  expect(resolveDispatch({ HOME: home, ENVOY_CONFIG_FILE: file }, home)).toEqual(credentials);
});

test("no Dispatch configuration stops the run and names what is missing", () => {
  const home = dir("home-empty");
  const file = path.join(home, ".config", "legion", "envoy.json");
  expect(() => resolveDispatch({ HOME: home }, home)).toThrow(
    `Dispatch is not configured: no envoy.json at ${file}: Dispatch is off`
  );
  expect(() => resolveDispatch({ HOME: home, DISPATCH_URL: "http://dispatch.test" }, home)).toThrow(
    "DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE"
  );
});

test("a read is one GET with the bearer and nothing else", async () => {
  const { fetch, sent } = fakeFetch(() => json([{ id: "c1" }]));
  const dispatch = new ProductionDispatch(credentials, fetch, noSleep);
  expect(await dispatch.comments("ACME-1")).toEqual([{ id: "c1" }] as never);
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url.toString()).toBe("http://dispatch.test/api/v1/issues/ACME-1/comments");
  expect(sent[0]?.init.method).toBe("GET");
  expect(sent[0]?.init.body).toBeUndefined();
  expect(sent[0]?.init.headers).toEqual({
    Accept: "application/json",
    Authorization: "Bearer dispatch-token",
  });
});

test("the issue list is one GET with no query, and a paged answer is refused", async () => {
  const { fetch, sent } = fakeFetch(() => json([{ key: "ACME-1" }]));
  const dispatch = new ProductionDispatch(credentials, fetch, noSleep);
  expect((await dispatch.issues()).map((issue) => issue.key)).toEqual(["ACME-1"]);
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url.pathname).toBe("/api/v1/issues");
  expect(sent[0]?.url.search).toBe("");

  const paged = new ProductionDispatch(
    credentials,
    fakeFetch(() => json({ issues: [], total: 0, limit: 50, offset: 0 })).fetch,
    noSleep
  );
  await expect(paged.issues()).rejects.toThrow(
    "GET /api/v1/issues answered something other than a JSON array of issues"
  );
});

test("a 429 or a 5xx is read again, and another status fails naming the read", async () => {
  const statuses = [503, 429, 200];
  const { fetch, sent } = fakeFetch(() => {
    const status = statuses.shift() ?? 200;
    return status === 200 ? json({ markdown: "x", version: 1 }) : new Response("busy", { status });
  });
  const waits: number[] = [];
  const dispatch = new ProductionDispatch(credentials, fetch, async (ms) => {
    waits.push(ms);
  });
  expect((await dispatch.artifactText("art-1")).markdown).toBe("x");
  expect(sent).toHaveLength(3);
  expect(waits).toHaveLength(2);

  const missing = new ProductionDispatch(
    credentials,
    fakeFetch(() => json({ error: "issue not found", code: "NOT_FOUND" }, 404)).fetch,
    noSleep
  );
  await expect(missing.issue("ACME-9")).rejects.toThrow(
    "GET /api/v1/issues/ACME-9 answered 404 NOT_FOUND: issue not found"
  );
});

test("a path below a git or jj checkout names the checkout; a path outside names none", () => {
  const git = dir("git-checkout");
  mkdirSync(path.join(git, ".git"));
  const jj = dir("jj-checkout");
  mkdirSync(path.join(jj, ".jj"));
  const worktree = dir("git-worktree");
  writeFileSync(path.join(worktree, ".git"), "gitdir: /elsewhere\n");
  expect(checkoutAbove(path.join(git, "a", "b"))).toBe(git);
  expect(checkoutAbove(jj)).toBe(jj);
  expect(checkoutAbove(path.join(worktree, "out"))).toBe(worktree);
  expect(checkoutAbove(path.join(dir("plain"), "out"))).toBeUndefined();
});

test("a window holds created_at from --since up to, and not at, --until, to the microsecond", () => {
  const window = parseWindow("2026-10-16T00:00:00Z", "2026-10-23T00:59:52Z");
  if (typeof window === "string") throw new Error(window);
  expect(inWindow(window, "2026-10-16T00:00:00Z")).toBe(true);
  expect(inWindow(window, "2026-10-15T23:59:59.999999Z")).toBe(false);
  expect(inWindow(window, "2026-10-23T00:59:51.999999Z")).toBe(true);
  expect(inWindow(window, "2026-10-23T00:59:52Z")).toBe(false);
  expect(inWindow(window, "2026-10-23T00:59:52.000001Z")).toBe(false);
  expect(inWindow(window, "2026-10-23T02:59:51+02:00")).toBe(true);
  expect(parseInstant("2026-10-16T17:53:32.037069Z")).toBeLessThan(
    parseInstant("2026-10-16T17:53:32.03707Z") ?? 0n
  );
  expect(parseWindow("2026-10-23", "2026-10-24T00:00:00Z")).toBe(
    "--since 2026-10-23 is not an RFC 3339 time, for example 2026-10-16T00:00:00Z"
  );
  expect(parseWindow("2026-10-24T00:00:00Z", "2026-10-23T00:00:00Z")).toBe(
    "--since 2026-10-24T00:00:00Z is not before --until 2026-10-23T00:00:00Z"
  );
});

test("mapLimit keeps the order and runs at most the limit at once", async () => {
  // The first three calls wait on gates that the test opens. The other calls do not wait.
  const gates = [1, 2, 3].map(() => Promise.withResolvers<void>());
  const started: number[] = [];
  let running = 0;
  let most = 0;
  const pending = mapLimit([1, 2, 3, 4, 5, 6, 7], 3, async (item) => {
    started.push(item);
    running++;
    most = Math.max(most, running);
    await gates[item - 1]?.promise;
    running--;
    return item * 10;
  });
  expect(started).toEqual([1, 2, 3]);
  for (const gate of [...gates].reverse()) gate.resolve();
  expect(await pending).toEqual([10, 20, 30, 40, 50, 60, 70]);
  expect(started.toSorted((a, b) => a - b)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  expect(most).toBe(3);
});

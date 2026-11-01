import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  AGENT_ARTIFACT_PATH,
  AGENT_LIVE_PATH,
  ANSWERS_PATH,
  REPLIES_PATH,
} from "../../web/src/features/refs/routes";
import { walkRoutes } from "../two-builds-routes";

// The two-builds walk (e2e/two-builds.multiprocess.e2e.ts) asks the other build's process for
// exactly the chunks the routes it visits load, so a page app.tsx adds without a row in
// e2e/two-builds-routes.ts is a page whose chunk a rolling release can fail to serve unseen.
const app = readFileSync(fileURLToPath(new URL("../../web/src/app.tsx", import.meta.url)), "utf8");

/** The path constants app.tsx's router names instead of a literal. */
const pathConstants: Record<string, string> = {
  AGENT_ARTIFACT_PATH,
  AGENT_LIVE_PATH,
  ANSWERS_PATH,
  REPLIES_PATH,
};

const lazyPages = [...app.matchAll(/^const (\w+) = lazy\(/gm)].map((match) => match[1]);

const routerRoutes = [
  ...app.matchAll(/<Route element=\{<(\w+) \/>\} path=(?:"([^"]+)"|\{(\w+)\}) \/>/g),
].map(([row, page, literal, constant]) => {
  const path = literal ?? (constant === undefined ? undefined : pathConstants[constant]);
  if (path === undefined) {
    throw new Error(`app.tsx's ${row} names a path constant this test does not resolve`);
  }
  return { page, path };
});

const walked = (route: { page?: string; path: string }) => `${route.page} at ${route.path}`;

describe("the two-builds walk's route list", () => {
  test("app.tsx declares lazy pages and routes this test can read", () => {
    expect(lazyPages.length).toBeGreaterThan(0);
    expect(routerRoutes.length).toBeGreaterThan(0);
  });

  test("covers every page app.tsx loads with lazy()", () => {
    const walkedPages = new Set<string>(walkRoutes.map((route) => route.page));
    expect(lazyPages.filter((page) => page !== undefined && !walkedPages.has(page))).toEqual([]);
  });

  test("is exactly app.tsx's router: every route, each with the page it renders", () => {
    expect(walkRoutes.map(walked).sort()).toEqual(routerRoutes.map(walked).sort());
  });
});

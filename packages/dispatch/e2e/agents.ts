import { expect, type Locator, type Page } from "@playwright/test";

import type { CreateBroadcastInput } from "../web/src/api/types";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { fakeEnvoyPort } from "./harness-ports";

export interface FakeSession {
  session_id: string;
  title: string;
  dir?: string;
  machine_id?: string;
  roles?: string[];
  capabilities?: string[];
  last_seen?: number;
  port?: number;
  topics?: string[];
}

async function fixtureRequest(
  path: string,
  method: "GET" | "PATCH" | "PUT",
  body?: object
): Promise<Response> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }
  const response = await fetch(`http://127.0.0.1:${fakeEnvoyPort}${path}`, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    method,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} failed: ${response.status} ${await response.text()}`);
  }
  return response;
}

export async function setLiveSessions(rows: FakeSession[]): Promise<void> {
  await fixtureRequest("/__fixture/sessions", "PUT", rows);
}

export async function setSessionSendStatus(sessionID: string, status: 200 | 404): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    send_status: status,
  });
}

/** Takes a seeded session out of the live registry, or puts it back, without reseeding: a
 *  reseed clears every fixture send status with it. */
export async function setSessionLive(sessionID: string, live: boolean): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { live });
}

export async function getSentMessages(): Promise<Record<string, unknown>[]> {
  return (await fixtureRequest("/__fixture/sends", "GET")).json() as Promise<
    Record<string, unknown>[]
  >;
}

/** Every broadcast request the page posts from here on, in order, as the body it sent - a route
 *  that fulfils a request itself still lists it. */
export function postedBroadcasts(page: Page): CreateBroadcastInput[] {
  const posted: CreateBroadcastInput[] = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/api/v1/broadcasts")) {
      posted.push(request.postDataJSON() as CreateBroadcastInput);
    }
  });
  return posted;
}

/** Refuses every broadcast POST the way an unreachable Envoy listener does, until `allow()`. The
 *  selected sessions stay live, so the page's 15 s agent poll cannot empty the list mid-row. */
export async function refuseBroadcasts(page: Page): Promise<{ allow: () => void }> {
  let refusing = true;
  await page.route("**/api/v1/broadcasts", (route) =>
    refusing && route.request().method() === "POST"
      ? route.fulfill({
          body: JSON.stringify({ code: "ENVOY_UNAVAILABLE", error: "Envoy listener unreachable" }),
          contentType: "application/json",
          status: 503,
        })
      : route.fallback()
  );
  return {
    allow: () => {
      refusing = false;
    },
  };
}

export interface FakeInterest {
  session_id: string;
  topics: string[];
  updated_at?: number;
}

export async function setInterests(rows: FakeInterest[]): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }

  const response = await fetch(`http://127.0.0.1:${fakeEnvoyPort}/__fixture/interests`, {
    body: JSON.stringify(rows),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`setting interests failed: ${response.status} ${await response.text()}`);
  }
}

export async function getUnsubscribeCalls(): Promise<{ session_id: string; topics: string[] }[]> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }

  const response = await fetch(`http://127.0.0.1:${fakeEnvoyPort}/__fixture/unsubscribe-calls`);
  if (!response.ok) {
    throw new Error(
      `reading unsubscribe calls failed: ${response.status} ${await response.text()}`
    );
  }
  return (await response.json()) as { session_id: string; topics: string[] }[];
}

// The Agents page's keyboard rows, in `keyboard-agents.e2e.ts`, `keyboard-palette.e2e.ts` and the
// picker spec the WebKit and Firefox projects also run, share one page: two live sessions, each
// with Dispatch activity of its own, so both are listed rows rather than folded into `Inactive` or
// `No Dispatch activity`.
// The Planner's open ask waits on the viewer, which puts it above the Reviewer. Neither carries a
// `last_seen`: the fake Envoy answers every read of a session seeded without one with the current
// time, so a shared seed never ages into Inactive. A time computed here would: this module is
// evaluated once per Playwright worker, at the first spec that imports it, and the time would age
// with every spec the worker runs after that, until the sessions fold under `Inactive`.
export const plannerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/planner",
  machine_id: "box-1",
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};
export const reviewerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/reviewer",
  machine_id: "box-1",
  roles: ["reviewer"],
  session_id: "reviewer-session",
  title: "Reviewer",
};

/** Both sessions live, both with Dispatch activity, and two open issues for the picker to offer:
 *  a keyboard reader has to be able to pass the first to reach the second. Returns the first. */
export async function seedAgents(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
  await createIssue({ project: "CORE", title: "Rollout plan" });
  await createAsk(
    issue.key,
    { question: "Which release?" },
    { actor: { id: plannerSession.session_id, kind: "session" }, as: "agent" }
  );
  await createComment(
    issue.key,
    { body: "Reviewing the diff." },
    { actor: { id: reviewerSession.session_id, kind: "session" }, as: "agent" }
  );
  await setLiveSessions([plannerSession, reviewerSession]);
  return issue.key;
}

/** The keymap binds only once sign-in resolves (`AuthGate` renders a skeleton until
 *  `/auth/whoami` answers), so a key pressed before the page renders reaches no handler. */
export async function openAgents(page: Page): Promise<void> {
  await page.goto("/agents");
  await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
  await page.locator("body").focus();
}

/** The rows a reader sees. A closed fold's rows, and the rows the filters exclude, stay mounted
 *  and hidden in the page's one keyed list, so a bare `[data-agent-row]` counts them too;
 *  counting, indexing or ordering the page's rows goes through this. */
export function shownAgentRows(page: Page): Locator {
  return page.locator("[data-agent-row]:not([hidden])");
}

/** One session's row, shown or hidden - a folded or filtered row is in the page either way - so a
 *  row that asserts where the row is says which it expects (`toBeVisible`, `toBeHidden`). */
export function agentRow(page: Page, sessionID: string): Locator {
  return page.locator(`[data-agent-row="${sessionID}"]`);
}

/** Holds every `POST` to `pattern` until `release`, as a slow server would, and counts them. */
export async function holdPosts(
  page: Page,
  pattern: string
): Promise<{ posts: () => number; release: () => void }> {
  const { promise: held, resolve: release } = Promise.withResolvers<void>();
  let posts = 0;
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    posts += 1;
    await held;
    return route.fallback();
  });
  return { posts: () => posts, release };
}

/** Holds every `POST` to `pattern` until the returned call, then refuses it, as a server that is
 *  down: 503 with a reason the composer shows. */
export async function refusePosts(page: Page, pattern: string): Promise<() => void> {
  const { promise: held, resolve: refuse } = Promise.withResolvers<void>();
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    await held;
    return route.fulfill({
      body: JSON.stringify({ code: "UNAVAILABLE", error: "the server is down" }),
      contentType: "application/json",
      status: 503,
    });
  });
  return refuse;
}

/** Pastes a text file into `field`, as the clipboard hands one to it. Firefox ignores a
 *  programmatic `ClipboardEvent`'s initializer, so the test event carries the `DataTransfer`
 *  explicitly, which every browser exposes to React's paste handler. */
export async function pasteFile(field: Locator, name: string, text: string): Promise<void> {
  await field.evaluate(
    (node, file) => {
      const data = new DataTransfer();
      data.items.add(new File([file.text], file.name, { type: "text/markdown" }));
      const paste = new Event("paste", { bubbles: true, cancelable: true });
      Object.defineProperty(paste, "clipboardData", { value: data });
      node.dispatchEvent(paste);
    },
    { name, text }
  );
}

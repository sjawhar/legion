import { expect, type Page } from "@playwright/test";

import type { CreateBroadcastInput } from "../web/src/api/types";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { harnessPorts } from "./harness-ports";

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

/** A request to the harness's fake Envoy. Playwright starts this listener for local and deployed
 *  runs, so fixture state reaches whichever Dispatch server the suite targets. */
async function fixtureRequest(
  path: string,
  method: "GET" | "PATCH" | "PUT",
  body?: object
): Promise<Response> {
  const response = await fetch(`http://127.0.0.1:${harnessPorts.fakeEnvoy.port}${path}`, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    method,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} failed: ${response.status} ${await response.text()}`);
  }
  return response;
}

/** Clears all mutable fake Envoy state between e2e rows. */
export async function resetFakeEnvoy(): Promise<void> {
  await fixtureRequest("/__fixture/reset", "PUT");
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
  await fixtureRequest("/__fixture/interests", "PUT", rows);
}

export async function getUnsubscribeCalls(): Promise<{ session_id: string; topics: string[] }[]> {
  return (await fixtureRequest("/__fixture/unsubscribe-calls", "GET")).json() as Promise<
    { session_id: string; topics: string[] }[]
  >;
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

import { expect, type Page } from "@playwright/test";

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

// The Agents page's keyboard rows, in `keyboard-agents.e2e.ts` and in the picker spec the WebKit
// and Firefox projects also run, share one page: two sessions seen recently, each with Dispatch
// activity of its own, so both are listed rows rather than folded into `Inactive` or `No
// Dispatch activity`. The Planner's open ask waits on the viewer, which puts it above the
// Reviewer.
export const plannerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/planner",
  last_seen: Date.now() - 30_000,
  machine_id: "box-1",
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};
export const reviewerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/reviewer",
  last_seen: Date.now() - 30_000,
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

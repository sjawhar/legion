import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import type { BroadcastCreated, CreateBroadcastInput } from "../web/src/api/types";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { harnessPorts } from "./harness-ports";
import { type KeyedWrite, keyedWrite, watchWrites } from "./keyed-writes";
import { sessionIds } from "./session-ids";

export interface FakeSession {
  session_id: string;
  title: string;
  dir?: string;
  machine_id?: string;
  // The session's real machine; the real listener falls back to machine_id when the session's
  // plugin predates this field, which the fake Envoy's asSeen mirrors (fake-envoy.ts).
  host?: string;
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

/** Holds every send to the session `ms` before the fake Envoy answers it, as a listener slow to
 *  take it would; 0 answers at once again. Dispatch's listener client gives up on an answer at
 *  5 s, so a hold that is to end in a delivery stays under that. */
export async function setSessionSendDelay(sessionID: string, ms: number): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    send_delay_ms: ms,
  });
}

/** What the fake listener's session store holds for an id, the question its per-session route
 *  answers. Only `absent` is a session that has ended. */
export type SessionRecord = "present" | "absent" | "unreadable";

/** Takes a seeded session out of the listener's live list, or puts it back, without reseeding: a
 *  reseed clears every fixture send status with it. This is the refilling-cache state and
 *  nothing more - the list is the listener's in-memory cache, while its per-session route reads
 *  the session store through - so the session still resolves 200 there and is still delivered
 *  to. A row that needs a session to have genuinely ended uses `setSessionRecord(id, "absent")`.
 */
export async function setSessionLive(sessionID: string, live: boolean): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { live });
}

/** Says what the listener's session store holds for a seeded session: `present` is a record the
 *  per-session route answers 200 from, and answering it puts the id back in the live list as the
 *  real listener's read-through warms its cache; `absent` ends the session, which drops it from
 *  the live list and makes every answer about it - the per-session route, the list and a send -
 *  report no live session; `unreadable` is the store read that did not answer at all, a 503 the
 *  caller must retry rather than read as an ended session. */
export async function setSessionRecord(sessionID: string, record: SessionRecord): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { record });
}

export async function getSentMessages(): Promise<Record<string, unknown>[]> {
  return (await fixtureRequest("/__fixture/sends", "GET")).json() as Promise<
    Record<string, unknown>[]
  >;
}

/** Whether a body the page posted is a broadcast's input. */
export function isBroadcastInput(body: unknown): body is CreateBroadcastInput {
  return (
    typeof body === "object" &&
    body !== null &&
    "body" in body &&
    typeof body.body === "string" &&
    "delivery" in body &&
    DELIVERY_CAPABILITIES.some((mode) => mode === body.delivery) &&
    "session_ids" in body &&
    Array.isArray(body.session_ids) &&
    body.session_ids.every((id) => typeof id === "string")
  );
}

/** Every broadcast request the page posts from here on, in order, as the body it sent and the
 *  `Idempotency-Key` it sent it under - a route that fulfils a request itself still lists it. */
export function postedBroadcasts(page: Page): KeyedWrite<CreateBroadcastInput>[] {
  return watchWrites(page, "POST", "/api/v1/broadcasts", isBroadcastInput);
}

/** Every broadcast the page sends from here on, in order: each press, Retry and Restore draft, as
 *  the body and key it sent. A repeat is not a send of its own: Dispatch answers a broadcast
 *  before it delivers, and while that answer still shows a recipient whose one attempt is pending,
 *  the page asks again under the same key (`broadcast-queue.ts`), so the POSTs of one send
 *  outnumber it. A POST under the key of an answer that left a recipient pending is that repeat,
 *  and is not listed; `postedBroadcasts` lists every POST. */
export function broadcastSends(page: Page): KeyedWrite<CreateBroadcastInput>[] {
  const sends: KeyedWrite<CreateBroadcastInput>[] = [];
  const owed = new Set<string | undefined>();
  const isBroadcastPost = (request: { method(): string; url(): string }) =>
    request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/broadcasts";
  page.on("request", (request) => {
    if (!isBroadcastPost(request)) return;
    const send = keyedWrite(request, isBroadcastInput);
    if (!owed.delete(send.key)) sends.push(send);
  });
  page.on("response", async (response) => {
    if (!isBroadcastPost(response.request()) || !response.ok()) return;
    const created = (await response.json()) as BroadcastCreated;
    const pending = created.recipients.some(
      ({ message: { deliveries } }) => deliveries.length === 1 && deliveries[0]?.state === "pending"
    );
    if (pending) owed.add(response.request().headers()["idempotency-key"]);
  });
  return sends;
}

/** The reason `refuseBroadcasts` gives, which a refused row shows after `Could not send to N
 *  agents: `. */
export const BROADCAST_REFUSED = "no selected session can receive this message";

/** Refuses every broadcast POST as Dispatch does one no selected session can receive (400
 *  `BROADCAST_EMPTY`), until `allow()`. A broadcast tries a failed fetch, a 5xx or a 429 again
 *  under its key until Dispatch answers, so a refusal is a 4xx of Dispatch's own. The selected
 *  sessions stay live, so the page's 15 s agent poll cannot empty the list mid-row. */
export async function refuseBroadcasts(page: Page): Promise<{ allow: () => void }> {
  let refusing = true;
  await page.route("**/api/v1/broadcasts", (route) =>
    refusing && route.request().method() === "POST"
      ? route.fulfill({
          body: JSON.stringify({ code: "BROADCAST_EMPTY", error: BROADCAST_REFUSED }),
          contentType: "application/json",
          status: 400,
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
  session_id: sessionIds.planner,
  title: "Planner",
};
export const reviewerSession: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/srv/reviewer",
  machine_id: "box-1",
  roles: ["reviewer"],
  session_id: sessionIds.reviewer,
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

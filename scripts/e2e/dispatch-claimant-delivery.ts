// The driver of dispatch-claimant-delivery.sh: it subscribes to the run's own NATS, drives a real
// envoy-dispatch over HTTP as three sessions (the shared agent token, each write naming its
// session as `actor`) and one person (the dev sign-in cookie), and for each case prints the
// `notifications.agent.*` topics that carried the case's event, its envelope `dispatch-<event id>`.
//
//   CLAIMANT_E2E_AGENT_TOKEN=<token> bun scripts/e2e/dispatch-claimant-delivery.ts \
//     <dispatch-url> <nats-url> <fake-envoy-url>
//
// Each case prints `== <case>`, what it read, then `ok <case>` or `FAIL <case>: <why>`; every case
// runs, a summary line follows, and the exit status is 1 when any case failed. A step outside the
// cases that fails (the sign-in, the project, the fake listener's seed) exits 2 naming it.
import { connect, StringCodec } from "nats";

const [dispatchUrl, natsUrl, envoyUrl] = process.argv.slice(2);
const agentToken = process.env.CLAIMANT_E2E_AGENT_TOKEN ?? "";
if (dispatchUrl === undefined || natsUrl === undefined || envoyUrl === undefined || !agentToken) {
  process.stderr.write(
    "usage: CLAIMANT_E2E_AGENT_TOKEN=<token> bun dispatch-claimant-delivery.ts <dispatch-url> <nats-url> <fake-envoy-url>\n"
  );
  process.exit(2);
}
const origin = new URL(dispatchUrl).origin;

/** How long an event may take to reach its issue's topic once written, and how long after that
 *  the run keeps listening for the rest of its destinations. The outbox publishes an event's
 *  destinations one after another on one connection, its issue's topic first, so a destination
 *  missing from the settle window is one this binary did not publish. A new comment's copy for a
 *  session its mention resolved to is the exception: while the mention's send is still pending the
 *  outbox holds that copy back, defers the event two seconds and re-checks it on its next
 *  five-second tick, so the mention cases listen `mentionWaitMs` instead. */
const publishWaitMs = 10_000;
const settleMs = 2_000;
const mentionWaitMs = 15_000;

const person = "alice";
const sessionP = "claimant-e2e-p";
const sessionC = "claimant-e2e-c";
const sessionQ = "claimant-e2e-q";
const agentPrefix = "notifications.agent.";

type Json = Record<string, unknown>;
interface DispatchEvent {
  id: number;
  seq: number;
  type: string;
  payload: Json | null;
}

// Every envelope the run's NATS carries, by its envelope id: the subjects it was published on.
const seen = new Map<string, Set<string>>();
const codec = StringCodec();
const nats = await connect({ name: "dispatch-claimant-delivery", servers: natsUrl });
const subscription = nats.subscribe("notifications.>");
void (async () => {
  for await (const message of subscription) {
    let eventId: unknown;
    try {
      eventId = (JSON.parse(codec.decode(message.data)) as Json).event_id;
    } catch {
      continue;
    }
    if (typeof eventId !== "string") continue;
    const subjects = seen.get(eventId) ?? new Set<string>();
    subjects.add(message.subject);
    seen.set(eventId, subjects);
  }
})();
await nats.flush();

async function call(
  method: string,
  path: string,
  headers: Record<string, string>,
  body?: Json
): Promise<unknown> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? headers : { ...headers, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`);
  return text === "" ? undefined : JSON.parse(text);
}

/** A session's request: the shared agent token, and on a write the session as its `actor`. */
function asSession(session: string, method: string, path: string, body?: Json): Promise<unknown> {
  const headers = { Authorization: `Bearer ${agentToken}` };
  const payload =
    body === undefined ? undefined : { ...body, actor: { id: session, kind: "session" } };
  return call(method, path, headers, payload);
}

/** A read with the agent token that names no session. */
function asAgent(path: string): Promise<unknown> {
  return call("GET", path, { Authorization: `Bearer ${agentToken}` });
}

async function signIn(login: string): Promise<string> {
  const path = `/auth/_dev/signin?login=${encodeURIComponent(login)}`;
  const response = await fetch(new URL(path, dispatchUrl), { redirect: "manual" });
  const cookie = response.headers
    .getSetCookie()
    .find((header) => header.startsWith("dsession="))
    ?.split(";")[0];
  if (response.status !== 302 || cookie === undefined) {
    throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  }
  return cookie;
}

const personCookie = await signIn(person).catch((error: unknown) => setupFailed("sign-in", error));

/** The person's request: the session cookie, and the dashboard origin every cookie write needs. */
function asPerson(method: string, path: string, body?: Json): Promise<unknown> {
  return call(method, path, { Cookie: personCookie, Origin: origin }, body);
}

function setupFailed(step: string, error: unknown): never {
  process.stderr.write(`setup ${step} failed: ${error instanceof Error ? error.message : error}\n`);
  process.exit(2);
}

function issuePath(key: string, rest = ""): string {
  return `/api/v1/issues/${encodeURIComponent(key)}${rest}`;
}

/** The issue's newest event's `seq`, its position in the issue's own log, which `?after=` pages by. */
async function newestSeq(key: string): Promise<number> {
  const events = (await asAgent(issuePath(key, "/events?order=desc&limit=1"))) as DispatchEvent[];
  return events[0]?.seq ?? 0;
}

/** The issue's event of `type` after seq `after` whose payload is the row `rowId`. */
async function eventOf(
  key: string,
  after: number,
  type: string,
  rowId: string
): Promise<DispatchEvent> {
  const events = (await asAgent(
    issuePath(key, `/events?after=${after}&limit=200`)
  )) as DispatchEvent[];
  const event = events.find((item) => item.type === type && item.payload?.id === rowId);
  if (event === undefined) {
    const types = events.map((item) => `${item.id}:${item.type}`).join(" ");
    throw new Error(`no ${type} event of ${rowId} after seq ${after} (found: ${types || "none"})`);
  }
  return event;
}

/** The agent topics `dispatch-<id>` reached, once it reached its issue's topic and `windowMs` more
 *  passed, or sooner once `enough` holds of them. Throws when the outbox never published it. */
async function agentTopicsOf(
  event: DispatchEvent,
  windowMs = settleMs,
  enough?: (topics: string[]) => boolean
): Promise<string[]> {
  const envelopeId = `dispatch-${event.id}`;
  const deadline = Date.now() + publishWaitMs;
  const subjects = () => [...(seen.get(envelopeId) ?? [])];
  const published = () =>
    subjects().some((subject) => subject.startsWith("notifications.dispatch."));
  while (!published()) {
    if (Date.now() > deadline) {
      throw new Error(
        `${envelopeId} (${event.type}) reached no notifications.dispatch topic within ${publishWaitMs / 1000}s`
      );
    }
    await Bun.sleep(100);
  }
  const agentTopics = () =>
    subjects()
      .filter((subject) => subject.startsWith(agentPrefix))
      .sort();
  if (enough === undefined) {
    await Bun.sleep(windowMs);
    return agentTopics();
  }
  const windowEnd = Date.now() + windowMs;
  while (!enough(agentTopics()) && Date.now() < windowEnd) await Bun.sleep(100);
  return agentTopics();
}

function note(line: string): void {
  console.log(`   ${line}`);
}

function topicsLine(event: DispatchEvent, topics: string[]): string {
  return `dispatch-${event.id} (${event.type}) on agent topics: ${topics.length === 0 ? "(none)" : topics.join(" ")}`;
}

function topic(session: string): string {
  return agentPrefix + session;
}

/** `claimant`, `route` and `route_holder` of a read, or `absent` when the read carries no such
 *  field (a server that predates them). */
function reach(row: Json | undefined): string {
  if (row === undefined) return "no row";
  const field = (name: string) => (name in row ? JSON.stringify(row[name]) : "absent");
  return `claimant=${field("claimant")} route=${field("route")} route_holder=${field("route_holder")}`;
}

/** Has the fake listener answer every send to `session` with `status`: 404 refuses it as a
 *  session it does not know, while the registry read a mention resolves through still finds it. */
async function setSendStatus(session: string, status: 200 | 404): Promise<void> {
  const response = await fetch(
    new URL(`/__fixture/sessions/${encodeURIComponent(session)}`, envoyUrl),
    {
      body: JSON.stringify({ send_status: status }),
      headers: { "Content-Type": "application/json" },
      method: "PATCH",
    }
  );
  if (!response.ok) {
    throw new Error(
      `PATCH fake listener ${session} send_status=${status}: ${response.status} ${await response.text()}`
    );
  }
}

/** The person's comment on `key` mentioning `session` in the default mode (`steer`), with what
 *  `extra` adds (an ask reply's `ask_id`). It prints the sessions the comment.created payload's
 *  mentions resolved to, how many sends for the comment the fake listener took, and each delivery
 *  row `GET /api/v1/comments/{id}` reads, and returns the event and the newest row's state. It
 *  throws when the mention did not resolve to `session`: every mention case is about a session the
 *  mention named, whatever its send did. */
async function personMentions(
  key: string,
  session: string,
  body: string,
  extra: Json = {}
): Promise<{ event: DispatchEvent; state: unknown }> {
  const after = await newestSeq(key);
  const comment = (await asPerson("POST", issuePath(key, "/comments"), {
    ...extra,
    body,
    mentions: [{ target: `session:${session}` }],
  })) as Json;
  const commentId = String(comment.id);
  const event = await eventOf(key, after, "comment.created", commentId);
  const mentions = (event.payload?.mentions ?? []) as Json[];
  const resolved = mentions.map(
    (item) => `${item.target}→${JSON.stringify(item.session_id ?? null)}`
  );
  const sends = (await (await fetch(new URL("/__fixture/sends", envoyUrl))).json()) as {
    idempotency_key: string;
  }[];
  // A mention's send is keyed `<comment id>:<target>:<mode>`.
  const taken = sends.filter((send) => send.idempotency_key.startsWith(`${commentId}:`)).length;
  note(
    `the person commented ${commentId} on ${key} mentioning session:${session}; comment.created mentions ${resolved.join(" ") || "(none)"}; the fake listener took ${taken} send(s) for it`
  );
  const rows = (((await asAgent(`/api/v1/comments/${commentId}`)) as Json).comment as Json)
    .deliveries as Json[];
  for (const row of rows) {
    const error =
      row.error === null || row.error === undefined ? "" : ` error=${JSON.stringify(row.error)}`;
    note(
      `GET /api/v1/comments/${commentId} delivery: ${row.target} attempt ${row.attempt} (${row.delivery}) session_id=${JSON.stringify(row.session_id ?? null)} state=${row.state}${error}`
    );
  }
  if (!mentions.some((item) => item.session_id === session)) {
    throw new Error(`the mention did not resolve to ${session}`);
  }
  return { event, state: rows.at(-1)?.state };
}

const results: { name: string; ok: boolean }[] = [];

async function runCase(name: string, body: () => Promise<string | undefined>): Promise<void> {
  console.log(`== ${name}`);
  let failure: string | undefined;
  try {
    failure = await body();
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error);
  }
  results.push({ name, ok: failure === undefined });
  console.log(failure === undefined ? `ok ${name}` : `FAIL ${name}: ${failure}`);
}

// --- setup -------------------------------------------------------------------------------------

// The three sessions are live on the fake Envoy listener, so a message targeted at one of them is
// delivered rather than refused for an unknown session. P and C advertise `steer`, the default
// mode of a mention, so a mention of either resolves to that session with nothing refused before
// the send; the mention cases then decide whether the send itself lands.
const seeded = await fetch(new URL("/__fixture/sessions", envoyUrl), {
  body: JSON.stringify(
    [sessionP, sessionC, sessionQ].map((id) => ({
      ...(id === sessionQ ? {} : { capabilities: ["steer"] }),
      machine_id: "claimant-e2e",
      session_id: id,
      title: id,
    }))
  ),
  headers: { "Content-Type": "application/json" },
  method: "PUT",
}).catch((error: unknown) => setupFailed("fake Envoy seed", error));
if (!seeded.ok) setupFailed("fake Envoy seed", `${seeded.status} ${await seeded.text()}`);

await asPerson("POST", "/api/v1/projects", { key: "CLM", name: "Claimant delivery" }).catch(
  (error: unknown) => setupFailed("project", error)
);

const spec = "# Retry order\n\nThe publisher retries the denied destination after the others.\n";
const claimed = (await asPerson("POST", "/api/v1/issues", {
  project: "CLM",
  spec,
  title: "Claimed by session P",
}).catch((error: unknown) => setupFailed("claimed issue", error))) as Json;
const claimedKey = String(claimed.key);
await asSession(sessionP, "POST", issuePath(claimedKey, "/claim"), {}).catch((error: unknown) =>
  setupFailed(`claim of ${claimedKey} by ${sessionP}`, error)
);
const claimedIssue = (await asAgent(issuePath(claimedKey)).catch((error: unknown) =>
  setupFailed(`read of ${claimedKey}`, error)
)) as Json;
const holder = ((claimedIssue.claim as Json | null)?.actor ?? null) as Json | null;
console.log(
  `setup: person ${person}; sessions P=${sessionP} C=${sessionC} Q=${sessionQ}; issue ${claimedKey} claim=${JSON.stringify(holder)} route=${JSON.stringify(claimedIssue.route ?? null)}`
);
if (holder?.kind !== "session" || holder.id !== sessionP) {
  setupFailed("claim", `${claimedKey} is not held by ${sessionP}`);
}
if (claimedIssue.route !== null && claimedIssue.route !== undefined) {
  setupFailed("route", `${claimedKey} has a route; the cases need none`);
}

// --- cases -------------------------------------------------------------------------------------

await runCase("case3-ask-answer", async () => {
  const ask = (await asSession(sessionC, "POST", issuePath(claimedKey, "/asks"), {
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Ship the retry order as written?",
  })) as Json;
  const askId = String(ask.id);
  note(`session C opened ask ${askId} on ${claimedKey}`);
  note(`GET /api/v1/asks/${askId}: ${reach((await asAgent(`/api/v1/asks/${askId}`)) as Json)}`);
  const inbox = (await asPerson("GET", "/api/v1/inbox")) as Json[];
  // The ask's reach is on the row's thread, as on every ask thread the server answers.
  const row = inbox.find((item) => item.id === askId);
  note(
    `GET /api/v1/inbox row's thread: ${row === undefined ? "no row" : reach(row.thread as Json | undefined)}`
  );
  const after = await newestSeq(claimedKey);
  await asPerson("POST", `/api/v1/asks/${askId}/answer`, {
    expected_edited_at: ask.edited_at ?? null,
    selected: ["Ship"],
  });
  const event = await eventOf(claimedKey, after, "ask.answered", askId);
  const topics = await agentTopicsOf(event);
  note(topicsLine(event, topics));
  // Both lists are sorted: the topics by agentTopicsOf, these as written.
  const expected = [topic(sessionC), topic(sessionP)];
  return topics.join(" ") === expected.join(" ")
    ? undefined
    : `expected exactly ${expected.join(" ")}`;
});

await runCase("case2-thread-reply", async () => {
  const root = (await asPerson("POST", issuePath(claimedKey, "/comments"), {
    anchor: { artifact: "spec", quote: "retries the denied destination" },
    body: "Is this the order we want?",
  })) as Json;
  note(`the person commented on ${claimedKey}'s spec: thread ${root.id}`);
  const after = await newestSeq(claimedKey);
  const reply = (await asPerson("POST", issuePath(claimedKey, "/comments"), {
    body: "And what does the retry publish again?",
    reply_to: root.id,
  })) as Json;
  note(`the person replied in it, mentioning nobody: comment ${reply.id}`);
  const event = await eventOf(claimedKey, after, "comment.created", String(reply.id));
  const topics = await agentTopicsOf(event);
  note(topicsLine(event, topics));
  return topics.includes(topic(sessionP)) ? undefined : `expected ${topic(sessionP)} among them`;
});

await runCase("untargeted-message", async () => {
  const after = await newestSeq(claimedKey);
  const message = (await asPerson("POST", issuePath(claimedKey, "/messages"), {
    body: "Ship it once the build is green.",
  })) as Json;
  note(`the person posted untargeted message ${message.id} on ${claimedKey}`);
  const event = await eventOf(claimedKey, after, "message.created", String(message.id));
  const topics = await agentTopicsOf(event);
  note(topicsLine(event, topics));
  return topics.join(" ") === topic(sessionP) ? undefined : `expected exactly ${topic(sessionP)}`;
});

await runCase("targeted-message", async () => {
  const after = await newestSeq(claimedKey);
  const message = (await asPerson("POST", issuePath(claimedKey, "/messages"), {
    body: "Q, can you check the retry?",
    delivery: "aside",
    target: `session:${sessionQ}`,
  })) as Json;
  note(`the person sent message ${message.id} targeted at session:${sessionQ} on ${claimedKey}`);
  const event = await eventOf(claimedKey, after, "message.created", String(message.id));
  const topics = await agentTopicsOf(event);
  note(topicsLine(event, topics));
  return topics.includes(topic(sessionP)) ? `${topic(sessionP)} carried it` : undefined;
});

await runCase("agent-comment", async () => {
  const after = await newestSeq(claimedKey);
  const comment = (await asSession(sessionC, "POST", issuePath(claimedKey, "/comments"), {
    body: "I checked the retry order.",
  })) as Json;
  note(`session C commented ${comment.id} on ${claimedKey}`);
  const event = await eventOf(claimedKey, after, "comment.created", String(comment.id));
  const topics = await agentTopicsOf(event);
  note(topicsLine(event, topics));
  return topics.includes(topic(sessionP)) ? `${topic(sessionP)} carried it` : undefined;
});

await runCase("unclaimed-issue", async () => {
  const issue = (await asPerson("POST", "/api/v1/issues", {
    project: "CLM",
    title: "Claimed by nobody",
  })) as Json;
  const key = String(issue.key);
  const read = (await asAgent(issuePath(key))) as Json;
  note(
    `issue ${key} claim=${JSON.stringify(read.claim ?? null)} route=${JSON.stringify(read.route ?? null)}`
  );
  const after = await newestSeq(key);
  const comment = (await asPerson("POST", issuePath(key, "/comments"), {
    body: "Does anyone have this?",
  })) as Json;
  note(`the person commented ${comment.id} on ${key}`);
  const event = await eventOf(key, after, "comment.created", String(comment.id));
  const topics = await agentTopicsOf(event);
  note(topicsLine(event, topics));
  // Nobody holds the issue, it has no route and the comment mentions nobody: today's rules send
  // it to no session's topic.
  return topics.length === 0 ? undefined : "expected no agent topic";
});

// A mention resolved to a session hands it the comment only when its send lands: the outbox skips
// that session's own copy for a `sent` row, holds it back while the send is pending, and publishes
// it as if the session had not been mentioned once the send failed.
await runCase("mention-delivered-claimant", async () => {
  const { event, state } = await personMentions(
    claimedKey,
    sessionP,
    "P, the retry order is settled."
  );
  const topics = await agentTopicsOf(event, mentionWaitMs);
  note(topicsLine(event, topics));
  if (state !== "sent") return `expected the mention's delivery row sent, read ${state}`;
  return topics.includes(topic(sessionP))
    ? `${topic(sessionP)} carried it beside the delivered mention`
    : undefined;
});

await runCase("mention-failed-claimant", async () => {
  await setSendStatus(sessionP, 404);
  note(`the fake listener answers every send to ${sessionP} 404; its registry read still finds it`);
  try {
    const { event, state } = await personMentions(
      claimedKey,
      sessionP,
      "P, did the retry order change?"
    );
    const topics = await agentTopicsOf(event, mentionWaitMs, (seen) =>
      seen.includes(topic(sessionP))
    );
    note(topicsLine(event, topics));
    if (state !== "failed") return `expected the mention's delivery row failed, read ${state}`;
    return topics.includes(topic(sessionP))
      ? undefined
      : `expected ${topic(sessionP)} within ${mentionWaitMs / 1000}s`;
  } finally {
    await setSendStatus(sessionP, 200);
  }
});

await runCase("mention-failed-follower", async () => {
  const issue = (await asPerson("POST", "/api/v1/issues", {
    project: "CLM",
    title: "Asked about by session C",
  })) as Json;
  const key = String(issue.key);
  const read = (await asAgent(issuePath(key))) as Json;
  note(
    `issue ${key} claim=${JSON.stringify(read.claim ?? null)} route=${JSON.stringify(read.route ?? null)}`
  );
  const ask = (await asSession(sessionC, "POST", issuePath(key, "/asks"), {
    question: "Which retry order should the publisher keep?",
  })) as Json;
  const askId = String(ask.id);
  note(`session C opened ask ${askId} on ${key}, so C follows it`);
  note(`GET /api/v1/asks/${askId}: ${reach((await asAgent(`/api/v1/asks/${askId}`)) as Json)}`);
  await setSendStatus(sessionC, 404);
  note(`the fake listener answers every send to ${sessionC} 404; its registry read still finds it`);
  try {
    const { event, state } = await personMentions(key, sessionC, "C, keep the current order.", {
      ask_id: askId,
    });
    const topics = await agentTopicsOf(event, mentionWaitMs, (seen) =>
      seen.includes(topic(sessionC))
    );
    note(topicsLine(event, topics));
    if (state !== "failed") return `expected the mention's delivery row failed, read ${state}`;
    return topics.includes(topic(sessionC))
      ? undefined
      : `expected ${topic(sessionC)} within ${mentionWaitMs / 1000}s`;
  } finally {
    await setSendStatus(sessionC, 200);
  }
});

const failed = results.filter((result) => !result.ok).map((result) => result.name);
console.log(
  `summary: ${results.length - failed.length} ok, ${failed.length} FAIL${failed.length === 0 ? "" : ` (${failed.join(", ")})`}`
);
subscription.unsubscribe();
await nats.close();
process.exit(failed.length === 0 ? 0 : 1);

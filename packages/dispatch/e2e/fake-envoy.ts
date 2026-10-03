import { harnessPorts } from "./harness-ports";

interface FakeSession {
  readonly session_id: string;
  readonly title: string;
  readonly dir?: string;
  readonly machine_id?: string;
  readonly roles?: readonly string[];
  readonly capabilities?: readonly string[];
  readonly last_seen?: number;
}

interface SentMessage {
  readonly target_session: string;
  readonly source: string;
  readonly message: string;
  readonly payload: string;
  readonly idempotency_key: string;
  readonly urgency: string;
  readonly expects_reply: string;
}

interface FakeInterest {
  session_id: string;
  topics: string[];
  updated_at?: number;
}

/** Every piece of fixture state this listener holds. `PUT /__fixture/reset` replaces it whole, so
 *  a field added here is reset with the rest without being listed anywhere else. A session reseed
 *  replaces it whole too, so a handler reads its request body before it touches `state`: a write
 *  through a `state` read before that await lands in an object a reseed may already have
 *  dropped. */
interface FakeEnvoyState {
  readonly sessions: FakeSession[];
  readonly liveSessions: Set<string>;
  readonly sendStatuses: Map<string, 200 | 404>;
  interests: FakeInterest[];
  readonly sentMessages: SentMessage[];
  /** (idempotency key, recipient) pairs this stand-in has already published, the stream's own
   *  duplicate window for the life of the fixture. Cleared with the session seed. */
  readonly publishedKeys: Set<string>;
  readonly unsubscribeCalls: unknown[];
}

function emptyState(): FakeEnvoyState {
  return {
    interests: [],
    liveSessions: new Set(),
    publishedKeys: new Set(),
    sendStatuses: new Map(),
    sentMessages: [],
    sessions: [],
    unsubscribeCalls: [],
  };
}

let state = emptyState();

/** A row in the real registry always carries a recent `last_seen` (the heartbeat). A seed that
 *  omits it means "live now", so it is stamped on every read rather than once at seeding: a
 *  session seeded into a long-lived harness (one started by hand and reused with
 *  `DISPATCH_E2E_REUSE_SERVERS=1`) would otherwise fold under Inactive ten minutes later. Only a
 *  seed that sets `last_seen` can be stale. One read stamps every row with the same `now`, so
 *  sessions seeded together tie, as the server's sorts by `last_seen` expect of one read. */
function asSeen(session: FakeSession, now: number): FakeSession {
  return { last_seen: now, ...session };
}

Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts.fakeEnvoy.port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/v1/sessions") {
      const now = Date.now();
      return Response.json(
        state.sessions
          .filter((session) => state.liveSessions.has(session.session_id))
          .map((session) => asSeen(session, now))
      );
    }
    if (request.method === "GET" && url.pathname.startsWith("/v1/roles/")) {
      const role = decodeURIComponent(url.pathname.slice("/v1/roles/".length));
      const holder = state.sessions.find(
        (session) => state.liveSessions.has(session.session_id) && session.roles?.includes(role)
      );
      if (holder === undefined)
        return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
      return Response.json({
        ...asSeen(holder, Date.now()),
        capabilities: holder.capabilities ?? [],
        holder: holder.session_id,
        role,
        roles: holder.roles ?? [],
      });
    }
    if (request.method === "POST" && url.pathname === "/v1/messages/send") {
      const message = (await request.json()) as SentMessage;
      state.sentMessages.push(message);
      const sendStatus = state.sendStatuses.get(message.target_session);
      if (
        sendStatus === 404 ||
        !state.liveSessions.has(message.target_session) ||
        !state.sessions.some((session) => session.session_id === message.target_session)
      ) {
        return Response.json(
          { error: `no live session ${message.target_session}` },
          { status: 404 }
        );
      }
      // The real listener publishes under a JetStream MsgId built from the idempotency key and
      // the recipient, so a repeat of a key the stream already holds is suppressed and answered
      // `duplicate`. Modelling that here is what lets an e2e see the duplicate wording.
      const streamKey = `${message.idempotency_key}@${message.target_session}`;
      const duplicate = state.publishedKeys.has(streamKey);
      state.publishedKeys.add(streamKey);
      return Response.json({
        event_id: `envelope-${state.sentMessages.length}`,
        recipient: message.target_session,
        ...(duplicate ? { duplicate: true } : {}),
      });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    // A reseed starts the sessions over (their live set, send statuses, sends and duplicate
    // window) and keeps the persisted subscriptions, which a test seeds on their own.
    if (request.method === "PUT" && url.pathname === "/__fixture/sessions") {
      const sessions = (await request.json()) as FakeSession[];
      const ids = sessions.map((session) => session.session_id);
      state = {
        ...emptyState(),
        interests: state.interests,
        liveSessions: new Set(ids),
        sendStatuses: new Map(ids.map((id) => [id, 200] as const)),
        sessions,
        unsubscribeCalls: state.unsubscribeCalls,
      };
      return Response.json({ ok: true });
    }
    if (request.method === "PATCH" && url.pathname.startsWith("/__fixture/sessions/")) {
      const sessionID = decodeURIComponent(url.pathname.slice("/__fixture/sessions/".length));
      const body = (await request.json()) as { live?: boolean; send_status?: 200 | 404 };
      if (!state.sessions.some((session) => session.session_id === sessionID)) {
        return Response.json({ error: `unknown fixture session ${sessionID}` }, { status: 404 });
      }
      if (body.live !== undefined) {
        if (body.live) state.liveSessions.add(sessionID);
        else state.liveSessions.delete(sessionID);
      }
      if (body.send_status !== undefined) {
        if (body.send_status !== 200 && body.send_status !== 404) {
          return Response.json({ error: "send_status must be 200 or 404" }, { status: 400 });
        }
        state.sendStatuses.set(sessionID, body.send_status);
      }
      return Response.json({
        live: state.liveSessions.has(sessionID),
        send_status: state.sendStatuses.get(sessionID) ?? 200,
        session_id: sessionID,
      });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/sends") {
      return Response.json(state.sentMessages);
    }
    if (request.method === "GET" && url.pathname === "/v1/interests/") {
      return Response.json(state.interests);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/interests") {
      const interests = (await request.json()) as FakeInterest[];
      state.interests = interests;
      return Response.json({ ok: true });
    }
    // The real Dispatch server (packages/envoy/internal/dispatch/envoy/client.go's
    // Interest method) looks up one session's persisted topics before unsubscribing
    // it, so this must resolve even though only the bare list is seeded directly.
    if (request.method === "GET" && url.pathname.startsWith("/v1/interests/")) {
      const sessionId = decodeURIComponent(url.pathname.slice("/v1/interests/".length));
      const interest = state.interests.find((item) => item.session_id === sessionId);
      if (interest === undefined) {
        return new Response("not found", { status: 404 });
      }
      return Response.json(interest);
    }
    if (request.method === "POST" && url.pathname === "/v1/interests/unsubscribe") {
      const body = (await request.json()) as { session_id: string; topics: string[] };
      state.unsubscribeCalls.push(body);
      const interest = state.interests.find((item) => item.session_id === body.session_id);
      if (interest !== undefined) {
        interest.topics = interest.topics.filter((topic) => !body.topics.includes(topic));
      }
      return Response.json({ removed: body.topics });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/unsubscribe-calls") {
      return Response.json(state.unsubscribeCalls);
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return new Response("not found", { status: 404 });
  },
});

console.log(`fake envoy listener on 127.0.0.1:${harnessPorts.fakeEnvoy.port}`);

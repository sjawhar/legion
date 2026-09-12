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

const port = Number(process.env.FAKE_ENVOY_PORT ?? "9021");
let sessions: FakeSession[] = [];
let liveSessions = new Set<string>();
const sendStatuses = new Map<string, 200 | 404>();
let interests: { session_id: string; topics: string[]; updated_at?: number }[] = [];
const sentMessages: SentMessage[] = [];
const unsubscribeCalls: unknown[] = [];

Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/v1/sessions") {
      return Response.json(sessions.filter((session) => liveSessions.has(session.session_id)));
    }
    if (request.method === "GET" && url.pathname.startsWith("/v1/roles/")) {
      const role = decodeURIComponent(url.pathname.slice("/v1/roles/".length));
      const holder = sessions.find(
        (session) => liveSessions.has(session.session_id) && session.roles?.includes(role)
      );
      if (holder === undefined)
        return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
      return Response.json({
        ...holder,
        capabilities: holder.capabilities ?? [],
        holder: holder.session_id,
        role,
        roles: holder.roles ?? [],
      });
    }
    if (request.method === "POST" && url.pathname === "/v1/messages/send") {
      const message = (await request.json()) as SentMessage;
      sentMessages.push(message);
      const sendStatus = sendStatuses.get(message.target_session);
      if (
        sendStatus === 404 ||
        !liveSessions.has(message.target_session) ||
        !sessions.some((session) => session.session_id === message.target_session)
      ) {
        return Response.json(
          { error: `no live session ${message.target_session}` },
          { status: 404 }
        );
      }
      return Response.json({
        event_id: `envelope-${sentMessages.length}`,
        recipient: message.target_session,
      });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/sessions") {
      sessions = (await request.json()) as FakeSession[];
      sendStatuses.clear();
      for (const session of sessions) sendStatuses.set(session.session_id, 200);
      liveSessions = new Set(sessions.map((session) => session.session_id));
      sentMessages.length = 0;
      return Response.json({ ok: true });
    }
    if (request.method === "PATCH" && url.pathname.startsWith("/__fixture/sessions/")) {
      const sessionID = decodeURIComponent(url.pathname.slice("/__fixture/sessions/".length));
      if (!sessions.some((session) => session.session_id === sessionID)) {
        return Response.json({ error: `unknown fixture session ${sessionID}` }, { status: 404 });
      }
      const body = (await request.json()) as { live?: boolean; send_status?: 200 | 404 };
      if (body.live !== undefined) {
        if (body.live) liveSessions.add(sessionID);
        else liveSessions.delete(sessionID);
      }
      if (body.send_status !== undefined) {
        if (body.send_status !== 200 && body.send_status !== 404) {
          return Response.json({ error: "send_status must be 200 or 404" }, { status: 400 });
        }
        sendStatuses.set(sessionID, body.send_status);
      }
      return Response.json({
        live: liveSessions.has(sessionID),
        send_status: sendStatuses.get(sessionID) ?? 200,
        session_id: sessionID,
      });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/sends") {
      return Response.json(sentMessages);
    }
    if (request.method === "GET" && url.pathname === "/v1/interests/") {
      return Response.json(interests);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/interests") {
      interests = (await request.json()) as typeof interests;
      return Response.json({ ok: true });
    }
    // The real Dispatch server (packages/envoy/internal/dispatch/envoy/client.go's
    // Interest method) looks up one session's persisted topics before unsubscribing
    // it, so this must resolve even though only the bare list is seeded directly.
    if (request.method === "GET" && url.pathname.startsWith("/v1/interests/")) {
      const sessionId = decodeURIComponent(url.pathname.slice("/v1/interests/".length));
      const interest = interests.find((item) => item.session_id === sessionId);
      if (interest === undefined) {
        return new Response("not found", { status: 404 });
      }
      return Response.json(interest);
    }
    if (request.method === "POST" && url.pathname === "/v1/interests/unsubscribe") {
      const body = (await request.json()) as { session_id: string; topics: string[] };
      unsubscribeCalls.push(body);
      const interest = interests.find((item) => item.session_id === body.session_id);
      if (interest !== undefined) {
        interest.topics = interest.topics.filter((topic) => !body.topics.includes(topic));
      }
      return Response.json({ removed: body.topics });
    }
    if (request.method === "GET" && url.pathname === "/__fixture/unsubscribe-calls") {
      return Response.json(unsubscribeCalls);
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return new Response("not found", { status: 404 });
  },
});

console.log(`fake envoy listener on 127.0.0.1:${port}`);

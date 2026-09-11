const port = Number(process.env.FAKE_ENVOY_PORT ?? "9021");
let sessions: unknown[] = [];
let interests: { session_id: string; topics: string[]; updated_at?: number }[] = [];
const unsubscribeCalls: unknown[] = [];

Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/v1/sessions") {
      return Response.json(sessions);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/sessions") {
      sessions = (await request.json()) as unknown[];
      return Response.json({ ok: true });
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

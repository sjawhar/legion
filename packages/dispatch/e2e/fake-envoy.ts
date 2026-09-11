const port = Number(process.env.FAKE_ENVOY_PORT ?? "9021");
let sessions: unknown[] = [];

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
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return new Response("not found", { status: 404 });
  },
});

console.log(`fake envoy listener on 127.0.0.1:${port}`);

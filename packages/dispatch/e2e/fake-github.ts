// Minimal GitHub App API for the architecture-source access check: the
// installation lookup, the installation-token mint, and the repository read
// that proves the minted token works. Tests seed per-repository state through
// /__fixture/repos; the server reaches this via DISPATCH_GITHUB_API_BASE.

interface FakeRepo {
  /** Contents permission the installation reports: "read", "write", "none". */
  readonly contents: string;
  readonly installation_id: number;
}

const githubPort = Number(process.env.FAKE_GITHUB_PORT ?? "9022");
// "owner/repo" → its installation; a missing repository answers 404 (App not
// installed).
let repos = new Map<string, FakeRepo>();

Bun.serve({
  hostname: "127.0.0.1",
  port: githubPort,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "PUT" && url.pathname === "/__fixture/repos") {
      const seeded = (await request.json()) as Record<string, FakeRepo>;
      repos = new Map(Object.entries(seeded));
      return Response.json({ ok: true });
    }

    const installation = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/installation$/);
    if (request.method === "GET" && installation !== null) {
      const repo = repos.get(`${installation[1]}/${installation[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({
        app_slug: "dispatch-e2e",
        id: repo.installation_id,
        permissions: { contents: repo.contents },
      });
    }

    if (
      request.method === "POST" &&
      /^\/app\/installations\/\d+\/access_tokens$/.test(url.pathname)
    ) {
      return Response.json(
        {
          expires_at: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
          token: `ghs_fake_${url.pathname.split("/")[3]}`,
        },
        { status: 201 }
      );
    }

    const repository = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)$/);
    if (request.method === "GET" && repository !== null) {
      if (!request.headers.get("Authorization")?.startsWith("Bearer ghs_fake_")) {
        return Response.json({ message: "Requires authentication" }, { status: 401 });
      }
      if (!repos.has(`${repository[1]}/${repository[2]}`)) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({ full_name: `${repository[1]}/${repository[2]}` });
    }

    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return new Response("not found", { status: 404 });
  },
});

console.log(`fake github listener on 127.0.0.1:${githubPort}`);

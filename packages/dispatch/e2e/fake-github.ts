// Minimal GitHub App API for the architecture-source access check and the
// architecture importer: the installation lookup, the installation-token mint,
// the repository read that proves the minted token works, and the
// commit/tree/blob reads a sync walks. Tests seed per-repository state through
// /__fixture/repos; the server reaches this via DISPATCH_GITHUB_API_BASE.

const ARCHITECTURE_DIR = ".dispatch/architecture";

interface FakeRepo {
  /** Contents permission the installation reports: "read", "write", "none". */
  readonly contents: string;
  readonly installation_id: number;
  /** Markdown files inside .dispatch/architecture/, keyed by file name. The
   *  branch commit is derived from their content, so a re-seed with different
   *  files moves the commit exactly like a push would. */
  readonly files?: Record<string, string>;
}

/** Deterministic commit sha for a repository's current files. */
function commitFor(repo: FakeRepo): string {
  const hasher = new Bun.CryptoHasher("sha1");
  hasher.update(JSON.stringify(repo.files ?? {}));
  return hasher.digest("hex");
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

    const commit = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)$/);
    if (request.method === "GET" && commit !== null) {
      const repo = repos.get(`${commit[1]}/${commit[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({ sha: commitFor(repo) });
    }

    // The architecture directory's subtree at a commit
    // (`git/trees/{sha}:.dispatch/architecture`, the ":" and "/" percent-encoded
    // by the Go client); a repository without the directory is GitHub's 404.
    const tree = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/git\/trees\/([^/]+)$/);
    if (request.method === "GET" && tree !== null) {
      const repo = repos.get(`${tree[1]}/${tree[2]}`);
      const dir = decodeURIComponent(tree[3] ?? "").split(":", 2)[1];
      if (repo === undefined || dir !== ARCHITECTURE_DIR) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const files = repo.files ?? {};
      return Response.json({
        sha: `tree-${commitFor(repo)}`,
        truncated: false,
        tree: Object.keys(files).map((name) => ({
          path: name,
          mode: "100644",
          type: "blob",
          sha: `blob-${name}`,
          size: Buffer.byteLength(files[name] ?? ""),
        })),
      });
    }

    const blob = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/git\/blobs\/blob-(.+)$/);
    if (request.method === "GET" && blob !== null) {
      const content = repos.get(`${blob[1]}/${blob[2]}`)?.files?.[blob[3] ?? ""];
      if (content === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({
        content: Buffer.from(content).toString("base64"),
        encoding: "base64",
      });
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

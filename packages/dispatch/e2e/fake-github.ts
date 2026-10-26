import { harnessPorts } from "./harness-ports";

// Minimal GitHub App API for the architecture-source access check, the
// architecture importer, the delivery reconcile and the issue header's GitHub link:
// the installation lookup, the installation-token mint, the repository read that
// proves the minted token works, the commit/tree/blob reads a sync walks, empty
// delivery listings, and a seeded pull request with its head's check runs and
// workflow runs. Tests seed per-repository state through /__fixture/repos; the
// server reaches this via DISPATCH_GITHUB_API_BASE.

const ARCHITECTURE_DIR = ".dispatch/architecture";

/** A pull request the GitHub link reads: its title, state and head commit. */
interface FakePull {
  readonly head_sha: string;
  readonly merged?: boolean;
  readonly state: string;
  readonly title: string;
}

/** One check run of a commit, in the check suite `check_suite_id` names. */
interface FakeCheckRun {
  readonly app_id?: number;
  readonly check_suite_id: number;
  readonly conclusion: string | null;
  readonly id: number;
  readonly name: string;
  readonly status: string;
}

/** One workflow run of a commit: its workflow's name, its triggering event and its check suite. */
interface FakeWorkflowRun {
  readonly check_suite_id: number;
  readonly event: string;
  readonly id: number;
  readonly name: string;
}

/** GitHub's page of a list: `per_page` (default 30, at most 100) items from page `page`. */
function pageOf<T>(items: readonly T[], url: URL): T[] {
  const perPage = Math.min(Number(url.searchParams.get("per_page") ?? 30), 100);
  const page = Number(url.searchParams.get("page") ?? 1);
  return items.slice((page - 1) * perPage, page * perPage);
}

interface FakeRepo {
  /** Contents permission the installation reports: "read", "write", "none". */
  readonly contents: string;
  /** Actions and Pull-requests permissions a delivery reconcile needs. */
  readonly actions?: string;
  readonly pull_requests?: string;
  readonly installation_id: number;
  /** Markdown files inside .dispatch/architecture/, keyed by file name. The
   *  branch commit is derived from their content, so a re-seed with different
   *  files moves the commit exactly like a push would. */
  readonly files?: Record<string, string>;
  /** Pull requests keyed by number; a number not here answers a server error, as before. */
  readonly pulls?: Record<string, FakePull>;
  /** Check runs keyed by commit sha, in any order. */
  readonly check_runs?: Record<string, readonly FakeCheckRun[]>;
  /** Workflow runs keyed by head sha. */
  readonly workflow_runs?: Record<string, readonly FakeWorkflowRun[]>;
}

/** Deterministic commit sha for a repository's current files. */
function commitFor(repo: FakeRepo): string {
  const hasher = new Bun.CryptoHasher("sha1");
  hasher.update(JSON.stringify(repo.files ?? {}));
  return hasher.digest("hex");
}

/** The permissions an installation reports for a repository, in both installation answers. */
function permissionsOf(repo: FakeRepo): Record<string, string | undefined> {
  return { actions: repo.actions, contents: repo.contents, pull_requests: repo.pull_requests };
}

// "owner/repo" → its installation; a missing repository answers 404 (App not
// installed).
let repos = new Map<string, FakeRepo>();

Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts.fakeGithub.port,
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
        permissions: permissionsOf(repo),
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

    if (request.method === "GET" && url.pathname === "/app/installations") {
      const installations = new Map<number, FakeRepo>();
      for (const repo of repos.values()) installations.set(repo.installation_id, repo);
      return Response.json(
        [...installations.values()].map((repo) => ({
          account: { login: "acme" },
          app_slug: "dispatch-e2e",
          id: repo.installation_id,
          permissions: permissionsOf(repo),
        }))
      );
    }

    if (request.method === "GET" && url.pathname === "/installation/repositories") {
      const installationId = Number(
        request.headers.get("Authorization")?.replace("Bearer ghs_fake_", "")
      );
      return Response.json({
        repositories: [...repos]
          .filter(([, repo]) => repo.installation_id === installationId)
          .map(([fullName]) => ({ full_name: fullName })),
      });
    }

    if (request.method === "POST" && url.pathname === "/graphql") {
      return Response.json({
        data: {
          search: { issueCount: 0, nodes: [], pageInfo: { endCursor: "", hasNextPage: false } },
        },
      });
    }

    const workflowRuns = url.pathname.match(
      /^\/repos\/([^/]+)\/([^/]+)\/actions\/workflows\/[^/]+\/runs$/
    );
    if (request.method === "GET" && workflowRuns !== null) {
      if (!repos.has(`${workflowRuns[1]}/${workflowRuns[2]}`)) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({ total_count: 0, workflow_runs: [] });
    }

    // A commit's workflow runs (`actions/runs?head_sha=`), which the GitHub link joins to the
    // check runs by check suite for each run's workflow name and event.
    const commitWorkflowRuns = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/actions\/runs$/);
    if (request.method === "GET" && commitWorkflowRuns !== null) {
      const repo = repos.get(`${commitWorkflowRuns[1]}/${commitWorkflowRuns[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const headSHA = url.searchParams.get("head_sha") ?? "";
      const runs = repo.workflow_runs?.[headSHA] ?? [];
      return Response.json({
        total_count: runs.length,
        workflow_runs: pageOf(runs, url).map((run) => ({ ...run, head_sha: headSHA })),
      });
    }

    // A seeded pull request answers as GitHub does. A stored pull request's attribution read is
    // skipped on a server error, leaving the row as seeded, so an unseeded one answers 500: a 404
    // would mark it unfetchable and change the delivery data under the test.
    const pull = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/pulls\/(\d+)$/);
    if (request.method === "GET" && pull !== null) {
      const seeded = repos.get(`${pull[1]}/${pull[2]}`)?.pulls?.[pull[3] ?? ""];
      if (seeded === undefined) {
        return Response.json({ message: "fixture has no pull request" }, { status: 500 });
      }
      return Response.json({
        head: { sha: seeded.head_sha },
        merged: seeded.merged ?? false,
        number: Number(pull[3]),
        state: seeded.state,
        title: seeded.title,
      });
    }

    const checkRuns = url.pathname.match(
      /^\/repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)\/check-runs$/
    );
    if (request.method === "GET" && checkRuns !== null) {
      const repo = repos.get(`${checkRuns[1]}/${checkRuns[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const runs = repo.check_runs?.[checkRuns[3] ?? ""] ?? [];
      return Response.json({
        check_runs: pageOf(runs, url).map(({ app_id, check_suite_id, ...run }) => ({
          ...run,
          app: { id: app_id ?? 15368, slug: "github-actions" },
          check_suite: { id: check_suite_id },
        })),
        total_count: runs.length,
      });
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

console.log(`fake github listener on 127.0.0.1:${harnessPorts.fakeGithub.port}`);

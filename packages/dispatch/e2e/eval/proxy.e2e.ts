import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import type {
  ArtifactUploadResponse,
  Ask,
  Comment,
  CommentRead,
  Event,
  GraphReferences,
  Issue,
  IssueDetails,
  IssueSummary,
  SearchResponse,
} from "@legion/contracts";
import { expect, test } from "@playwright/test";

import { createIssue, createProject, e2eAgentToken } from "../api";
import { dispatchPort } from "../harness-ports";
import { resetDatabase } from "../seed";

// The evaluation proxy (proxy.ts beside this file) against the real Dispatch server this harness
// runs: the proxy is the only barrier between an agent under evaluation and a production write,
// so the server here stands in for production and every claim is checked by reading the server
// directly, never by trusting the proxy's own answer.

const proxyScript = fileURLToPath(new URL("./proxy.ts", import.meta.url));
const server = `http://127.0.0.1:${dispatchPort}`;
const agent = { as: "agent" as const, actor: { kind: "session" as const, id: "e2e-eval-proxy" } };
const actor = agent.actor;

interface Proxy {
  readonly url: string;
  readonly writes: string;
  stop(): Promise<void>;
}

interface Recorded {
  readonly kind: "read" | "write" | "refused";
  readonly method: string;
  readonly path: string;
  readonly body?: unknown;
}

async function startProxy(excludeKey: string): Promise<Proxy> {
  const dir = await mkdtemp(path.join(tmpdir(), "dispatch-eval-proxy-"));
  const tokenFile = path.join(dir, "token");
  const writes = path.join(dir, "writes.jsonl");
  await writeFile(tokenFile, `${e2eAgentToken}\n`, { mode: 0o600 });
  const child = spawn(
    "bun",
    [
      proxyScript,
      "--upstream",
      server,
      "--token-file",
      tokenFile,
      "--writes",
      writes,
      "--exclude-key",
      excludeKey,
    ],
    { stdio: ["ignore", "pipe", "pipe"] }
  );
  const listening = new Promise<string>((resolve, reject) => {
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk: Buffer) => {
      stdout += chunk.toString();
      const found = /listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(stdout);
      if (found?.[1]) resolve(found[1]);
    });
    child.stderr.on("data", (chunk: Buffer) => {
      stderr += chunk.toString();
    });
    child.on("exit", (code) => reject(new Error(`proxy exited ${code}: ${stderr}`)));
  });
  const url = await listening;
  return {
    url,
    writes,
    stop: async () => {
      child.kill("SIGTERM");
      await rm(dir, { recursive: true, force: true });
    },
  };
}

/** One Dispatch API call with the agent bearer; `T` is the route's response type, as `api.ts`
 * types its own reads. */
async function call<T = unknown>(
  base: string,
  method: string,
  route: string,
  body?: object
): Promise<{ status: number; body: T }> {
  const response = await fetch(`${base}/api/v1${route}`, {
    method,
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${e2eAgentToken}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  return { status: response.status, body: (text === "" ? undefined : JSON.parse(text)) as T };
}

type Read = <T = unknown>(
  method: string,
  route: string,
  body?: object
) => Promise<{ status: number; body: T }>;

const direct: Read = (method, route, body) => call(server, method, route, body);

function fieldNames(value: object): string[] {
  return Object.keys(value).sort();
}

test("the evaluation proxy forwards reads, keeps every write off the server, serves the writes back, and hides an excluded issue", async () => {
  test.skip(
    test.info().project.name !== "chromium",
    "an API-only spec: one browser project proves it"
  );
  test.skip(
    process.env.PLAYWRIGHT_BASE_URL !== undefined,
    "seeds and truncates the local harness database"
  );
  await resetDatabase();
  await createProject({ key: "EVP", name: "Eval proxy" });
  const kept = await createIssue(
    { project: "EVP", title: "Proxy keeps writes off the server", spec: "The kept issue." },
    agent
  );
  const hidden = await createIssue(
    {
      project: "EVP",
      title: "Proxy hides this duplicate",
      spec: "The hidden issue.",
      parent: kept.key,
    },
    agent
  );

  const proxy = await startProxy(hidden.key);
  try {
    const viaProxy: Read = (method, route, body) => call(proxy.url, method, route, body);
    const listKeys = async (read: Read) =>
      (await read<IssueSummary[]>("GET", "/issues?project=EVP")).body.map((row) => row.key);
    const searchOwners = async (read: Read) =>
      (await read<SearchResponse>("GET", "/search?q=proxy")).body.results.map((hit) =>
        hit.owner.kind === "issue" ? hit.owner.key : undefined
      );
    const edgeIssues = async (read: Read) =>
      (
        await read<GraphReferences>(
          "GET",
          `/references?to=${encodeURIComponent(`dispatch://${kept.key}`)}`
        )
      ).body.edges.map((edge) => edge.node.issue_key);

    // Reads: an allow-listed GET answers what the server answers, less the excluded child.
    const keptDirect = await direct<IssueDetails>("GET", `/issues/${kept.key}`);
    expect(keptDirect.body.children.map((child) => child.key)).toEqual([hidden.key]);
    expect(await viaProxy("GET", `/issues/${kept.key}`)).toEqual({
      ...keptDirect,
      body: { ...keptDirect.body, children: [] },
    });

    // The excluded issue: every route it addresses or owns answers 404, and it leaves every list.
    const hiddenDetails = (await direct<IssueDetails>("GET", `/issues/${hidden.key}`)).body;
    for (const route of [
      `/issues/${hidden.key}`,
      `/issues/${hidden.key}/events`,
      `/issues/${hidden.key}/comments`,
      `/artifacts/${hiddenDetails.primary_artifact_id}/text`,
      `/references?to=${encodeURIComponent(`dispatch://${hidden.key}`)}`,
    ]) {
      expect((await direct("GET", route)).status, `${route} directly`).toBe(200);
      expect(await viaProxy("GET", route), `${route} through the proxy`).toEqual({
        status: 404,
        body: { code: "NOT_FOUND", error: "not found" },
      });
    }
    expect((await listKeys(direct)).sort()).toEqual([kept.key, hidden.key].sort());
    expect(await listKeys(viaProxy)).toEqual([kept.key]);
    expect(await searchOwners(direct)).toContain(hidden.key);
    const proxiedOwners = await searchOwners(viaProxy);
    expect(proxiedOwners).toContain(kept.key);
    expect(proxiedOwners).not.toContain(hidden.key);
    expect(await edgeIssues(direct)).toContain(hidden.key);
    expect(await edgeIssues(viaProxy)).not.toContain(hidden.key);

    // A GET off the allow-list is refused.
    expect((await viaProxy("GET", "/inbox")).status).toBe(403);

    // Writes: none reaches the server.
    const before = {
      comments: (await direct("GET", `/issues/${kept.key}/comments`)).body,
      events: (await direct("GET", `/issues/${kept.key}/events`)).body,
      issues: await listKeys(direct),
      issue: (await direct("GET", `/issues/${kept.key}`)).body,
    };
    const input = {
      comment: { body: "A finding the proxy must keep off the server.", actor },
      message: { body: "READY-shaped packet the proxy must keep off the server.", actor },
      ask: { question: "Which way?", options: [{ label: "A" }, { label: "B" }], actor },
      issue: { project: "EVP", title: "A new issue the proxy must not file", actor },
      artifact: { name: "notes.md", content: "# Notes\n\nKept off the server.", actor },
      patch: { status: "in_progress", actor },
    };
    const writeAll = async (read: Read) => ({
      comment: await read<Comment>("POST", `/issues/${kept.key}/comments`, input.comment),
      message: await read("POST", `/issues/${kept.key}/messages`, input.message),
      ask: await read<Ask>("POST", `/issues/${kept.key}/asks`, input.ask),
      issue: await read<Issue>("POST", "/issues", input.issue),
      artifact: await read<ArtifactUploadResponse>(
        "POST",
        `/issues/${kept.key}/artifacts`,
        input.artifact
      ),
      patch: await read("PATCH", `/issues/${kept.key}`, input.patch),
    });
    const faked = await writeAll(viaProxy);
    expect((await direct("GET", `/issues/${kept.key}/comments`)).body).toEqual(before.comments);
    expect((await direct("GET", `/issues/${kept.key}/events`)).body).toEqual(before.events);
    expect(await listKeys(direct)).toEqual(before.issues);
    expect((await direct("GET", `/issues/${kept.key}`)).body).toEqual(before.issue);
    // A write the proxy does not model is recorded and refused, never answered with a guess.
    expect((await viaProxy("POST", `/issues/${kept.key}/pins`, { actor })).status).toBe(501);

    // Every write was recorded, with the body the agent sent.
    const recorded = (await readFile(proxy.writes, "utf8"))
      .trim()
      .split("\n")
      .map((line): Recorded => JSON.parse(line));
    const writes = recorded.filter((line) => line.kind === "write");
    expect(writes.map((line) => `${line.method} ${line.path}`)).toEqual([
      `POST /api/v1/issues/${kept.key}/comments`,
      `POST /api/v1/issues/${kept.key}/messages`,
      `POST /api/v1/issues/${kept.key}/asks`,
      "POST /api/v1/issues",
      `POST /api/v1/issues/${kept.key}/artifacts`,
      `PATCH /api/v1/issues/${kept.key}`,
      `POST /api/v1/issues/${kept.key}/pins`,
    ]);
    expect(writes[0]?.body).toEqual(input.comment);
    expect(recorded.filter((line) => line.kind === "refused").map((line) => line.path)).toEqual([
      "/api/v1/inbox",
    ]);

    // The agent reads its own writes back.
    const comment = faked.comment.body;
    const commentsViaProxy = await viaProxy<Comment[]>("GET", `/issues/${kept.key}/comments`);
    expect(commentsViaProxy.body.map((row) => row.id)).toContain(comment.id);
    const commentRead = await viaProxy<CommentRead>("GET", `/comments/${comment.id}`);
    expect(commentRead.body.comment).toEqual(comment);
    const createdRead = await viaProxy<IssueDetails>("GET", `/issues/${faked.issue.body.key}`);
    expect(createdRead.body.title).toBe(input.issue.title);
    const keptViaProxy = (await viaProxy<IssueDetails>("GET", `/issues/${kept.key}`)).body;
    expect(keptViaProxy.status).toBe("in_progress");
    expect(keptViaProxy.open_asks.map((ask) => ask.id)).toContain(faked.ask.body.id);
    const events = await viaProxy<Event[]>(
      "GET",
      `/issues/${kept.key}/events?after=${Math.max(0, keptViaProxy.last_seq - 10)}&limit=10`
    );
    expect(events.body.map((event) => event.type)).toEqual(
      expect.arrayContaining(["comment.created", "message.created", "ask.opened"])
    );

    // Each faked answer has the fields and status the server gives the same write.
    const real = await writeAll(direct);
    for (const kind of Object.keys(real) as (keyof typeof real)[]) {
      const realBody = real[kind].body as object;
      const fakedBody = faked[kind].body as object;
      expect(faked[kind].status, `${kind} status`).toBe(real[kind].status);
      expect(fieldNames(fakedBody), `${kind} fields`).toEqual(
        fieldNames(realBody).filter((field) => field !== "advice")
      );
    }
    expect(fieldNames(faked.artifact.body.artifact)).toEqual(
      fieldNames(real.artifact.body.artifact)
    );
  } finally {
    await proxy.stop();
  }
});

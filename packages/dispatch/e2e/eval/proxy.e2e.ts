import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import type {
  ArchitectureTree,
  ArtifactUploadResponse,
  Ask,
  Comment,
  Event,
  GraphReferences,
  Issue,
  IssueDetails,
  IssueSummary,
  Message,
  MessageDelivery,
  OpenAsksResponse,
  SearchResponse,
} from "@legion/contracts";
import { expect, test } from "@playwright/test";

import { setLiveSessions } from "../agents";
import {
  type ApiOptions,
  apiGet,
  apiPost,
  claimIssue,
  createAgentMessage,
  createArtifactAsk,
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createNamedVersion,
  createProject,
  createProjectDocument,
  e2eAgentToken,
  editArtifact,
  editAsk,
  followAsk,
  getIssue,
  patchIssue,
  putArchitectureSource,
  releaseIssue,
  replyToMessageDelivery,
  requestApproval,
  resolveAsk,
  resolveComment,
  syncArchitectureSource,
} from "../api";
import { seedFakeGithub } from "../fake-github-helpers";
import { dispatchPort } from "../harness-ports";
import { resetDatabase } from "../seed";

// The evaluation proxy (proxy.ts beside this file) against the real Dispatch server this harness
// runs. The proxy is the only barrier between an agent under evaluation and a production write,
// so the server stands in for production and every claim is checked against the server itself:
// a write through the proxy leaves the server unchanged, each answer the proxy models is the
// answer the server gives the same write, and each read after those writes is the read the
// server gives once the same writes really land.

const proxyScript = fileURLToPath(new URL("./proxy.ts", import.meta.url));
const server = `http://127.0.0.1:${dispatchPort}`;
const SESSION = "e2e-eval-proxy";
const actor = { kind: "session", id: SESSION } as const;
const agent = { as: "agent", actor } as const satisfies ApiOptions;

interface Recorded {
  readonly kind: "read" | "write" | "refused";
  readonly method: string;
  readonly path: string;
  readonly status: number;
  readonly body?: unknown;
  readonly response?: unknown;
}

interface Proxy {
  readonly url: string;
  records(): Promise<Recorded[]>;
  stop(): Promise<void>;
}

async function startProxy(options: { exclude?: string; upstream?: string } = {}): Promise<Proxy> {
  const dir = await mkdtemp(path.join(tmpdir(), "dispatch-eval-proxy-"));
  const tokenFile = path.join(dir, "token");
  const writes = path.join(dir, "writes.jsonl");
  await writeFile(tokenFile, `${e2eAgentToken}\n`, { mode: 0o600 });
  const exclude = options.exclude === undefined ? [] : ["--exclude-key", options.exclude];
  const child = spawn(
    "bun",
    [
      proxyScript,
      "--upstream",
      options.upstream ?? server,
      "--token-file",
      tokenFile,
      "--writes",
      writes,
      ...exclude,
    ],
    { stdio: ["ignore", "pipe", "pipe"] }
  );
  const listening = Promise.withResolvers<string>();
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk: Buffer) => {
    stdout += chunk.toString();
    const found = /listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(stdout);
    if (found?.[1]) listening.resolve(found[1]);
  });
  child.stderr.on("data", (chunk: Buffer) => {
    stderr += chunk.toString();
  });
  child.on("exit", (code) => listening.reject(new Error(`proxy exited ${code}: ${stderr}`)));
  const url = await listening.promise;
  return {
    url,
    records: async () =>
      (await readFile(writes, "utf8"))
        .trim()
        .split("\n")
        .map((line): Recorded => JSON.parse(line)),
    stop: async () => {
      child.kill("SIGTERM");
      await rm(dir, { recursive: true, force: true });
    },
  };
}

function skipUnlessLocalChromium(): void {
  test.skip(
    test.info().project.name !== "chromium",
    "an API-only spec: one browser project proves it"
  );
  test.skip(
    process.env.PLAYWRIGHT_BASE_URL !== undefined,
    "seeds and truncates the local harness database"
  );
}

/** The project every scenario starts from: EVP with its first issue, and the session the agent
 *  under evaluation runs as live on the fake Envoy, so a human can message it. */
async function seedProject(): Promise<Issue> {
  await resetDatabase();
  await setLiveSessions([
    {
      session_id: SESSION,
      title: "Agent under evaluation",
      capabilities: ["btw", "aside", "steer"],
    },
  ]);
  await createProject({ key: "EVP", name: "Eval proxy" });
  return createIssue(
    { project: "EVP", title: "Proxy keeps writes off the server", spec: "The kept issue." },
    agent
  );
}

/** The error a failed `api.ts` call throws, for an answer that is not a 2xx. */
async function failure(call: Promise<unknown>): Promise<string> {
  try {
    await call;
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  throw new Error("the call succeeded");
}

const UUID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g;
const TIMESTAMP = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

/**
 * An answer with what two runs of the same writes cannot share made comparable: every uuid
 * becomes `<uuid N>` and every event id `<event N>` in order of first appearance, so two
 * answers that point at the same things the same way compare equal; timestamps and ages become
 * placeholders; the proxy's key for an issue it created becomes the server's. `advice` (which
 * the proxy does not model) and a document's Proof `token` (which it cannot compute) are
 * dropped.
 */
function comparable(value: unknown, rename: ReadonlyMap<string, string>): unknown {
  const uuids = new Map<string, string>();
  const events = new Map<number, string>();
  const walk = (node: unknown, field: string | undefined): unknown => {
    if (typeof node === "string") {
      let text = node;
      for (const [from, to] of rename) text = text.replaceAll(from, to);
      if (TIMESTAMP.test(text)) return "<time>";
      return text.replace(UUID, (uuid) => {
        const known = uuids.get(uuid) ?? `<uuid ${uuids.size + 1}>`;
        uuids.set(uuid, known);
        return known;
      });
    }
    if (typeof node === "number") {
      if (field === "id" || field === "opened_event_id") {
        const known = events.get(node) ?? `<event ${events.size + 1}>`;
        events.set(node, known);
        return known;
      }
      if (field === "age_seconds") return "<age>";
      const renamed = rename.get(String(node));
      return field === "number" && renamed !== undefined ? Number(renamed) : node;
    }
    if (Array.isArray(node)) return node.map((element) => walk(element, undefined));
    if (typeof node === "object" && node !== null) {
      return Object.fromEntries(
        Object.entries(node)
          .filter(([key]) => key !== "advice" && key !== "token")
          .map(([key, inner]) => [key, walk(inner, key)])
      );
    }
    return node;
  };
  return walk(value, undefined);
}

test("the proxy forwards its allow-list, refuses every other route, and hides an excluded issue wherever it appears", async () => {
  skipUnlessLocalChromium();
  const kept = await seedProject();
  await seedFakeGithub({
    "example/arch": {
      contents: "read",
      files: { "core.md": "---\ntitle: Core\n---\nThe core.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("EVP", { branch: "main", repo: "example/arch" });
  expect((await syncArchitectureSource("EVP")).last_error).toBeNull();
  const hidden = await createIssue(
    { project: "EVP", title: "Proxy hides this duplicate", spec: "The hidden issue.", force: true },
    agent
  );
  // Each write leaves the hidden key somewhere a read of the kept issue carries it: a
  // child.added and a child.status on the kept issue's log, an inherited component attachment
  // on the grandchild, an open ask in the project, a component row and an edge.
  await patchIssue(hidden.key, { parent: kept.key }, agent);
  await patchIssue(hidden.key, { status: "in_progress" }, agent);
  await patchIssue(hidden.key, { components: { mode: "none", reason: "Not code." } }, agent);
  await patchIssue(kept.key, { components: { mode: "explicit", ids: ["core"] } }, agent);
  const grand = await createIssue(
    { project: "EVP", title: "Grandchild of the kept issue", parent: hidden.key, force: true },
    agent
  );
  await createAsk(hidden.key, { question: "A question on the hidden issue?" }, agent);
  await createAsk(kept.key, { question: "A question on the kept issue?" }, agent);
  await patchIssue(
    kept.key,
    { external_links: [{ url: "https://github.com/example/widgets/issues/5" }] },
    agent
  );
  await patchIssue(
    hidden.key,
    { external_links: [{ url: "https://github.com/example/widgets/pull/5" }] },
    agent
  );
  const hiddenSpec = (await getIssue(hidden.key, agent)).primary_artifact_id;

  const proxy = await startProxy({ exclude: hidden.key });
  try {
    const via = { ...agent, base: proxy.url } satisfies ApiOptions;
    const both = async <T>(route: string) => ({
      direct: await apiGet<T>(`/api/v1${route}`, agent),
      proxied: await apiGet<T>(`/api/v1${route}`, via),
    });

    // Every route addressed by the hidden issue, or by something it owns, answers 404.
    for (const route of [
      `/issues/${hidden.key}`,
      `/issues/${hidden.key}/events`,
      `/issues/${hidden.key}/comments`,
      `/artifacts/${hiddenSpec}/text`,
      `/references?to=${encodeURIComponent(`dispatch://${hidden.key}`)}`,
    ]) {
      await apiGet(`/api/v1${route}`, agent);
      expect(await failure(apiGet(`/api/v1${route}`, via)), route).toContain(
        'failed: 404 {"code":"NOT_FOUND","error":"not found"}'
      );
    }

    // Each shape that names it answers what the server answers without it.
    const issue = await both<IssueDetails>(`/issues/${kept.key}`);
    expect(issue.direct.children.map((child) => child.key)).toEqual([hidden.key]);
    expect(issue.proxied).toEqual({ ...issue.direct, children: [] });

    const list = await both<IssueSummary[]>("/issues?project=EVP");
    expect(list.direct.map((row) => row.key)).toContain(hidden.key);
    expect(list.proxied).toEqual(
      list.direct
        .filter((row) => row.key !== hidden.key)
        .map((row) =>
          row.key === grand.key
            ? { ...row, parent: null, components: { ...row.components, inherited_from: null } }
            : row
        )
    );

    const events = await both<Event[]>(`/issues/${kept.key}/events`);
    const childEvents = events.direct.filter((event) => event.type.startsWith("child."));
    expect(childEvents.map((event) => event.type)).toEqual(["child.added", "child.status"]);
    expect(events.proxied).toEqual(events.direct.filter((event) => !childEvents.includes(event)));

    const grandRead = await both<IssueDetails>(`/issues/${grand.key}`);
    expect(grandRead.direct.components.inherited_from).toBe(hidden.key);
    expect(grandRead.proxied).toEqual({
      ...grandRead.direct,
      parent: null,
      components: { ...grandRead.direct.components, inherited_from: null },
    });

    const open = await both<OpenAsksResponse>("/asks/open?project=EVP");
    const keptAsks = open.direct.asks.filter(
      (row) => !("issue" in row.owner) || row.owner.issue.key !== hidden.key
    );
    expect(keptAsks).toHaveLength(open.direct.asks.length - 1);
    expect(open.proxied).toEqual({
      ...open.direct,
      as_of: open.proxied.as_of,
      asks: keptAsks.map((row) => ({ ...row, age_seconds: expect.any(Number) })),
      count: keptAsks.length,
      waiting_on_human: keptAsks.length,
    });

    const search = await both<SearchResponse>("/search?q=proxy");
    const owners = (response: SearchResponse) =>
      response.results.map((hit) => (hit.owner.kind === "issue" ? hit.owner.key : undefined));
    expect(owners(search.direct)).toContain(hidden.key);
    expect(owners(search.proxied)).toContain(kept.key);
    expect(owners(search.proxied)).not.toContain(hidden.key);

    const edges = await both<GraphReferences>(
      `/references?to=${encodeURIComponent(`dispatch://${kept.key}`)}`
    );
    expect(edges.direct.edges.map((edge) => edge.node.issue_key)).toContain(hidden.key);
    expect(edges.proxied).toEqual({
      ...edges.direct,
      edges: edges.direct.edges.filter((edge) => edge.node.issue_key !== hidden.key),
    });

    const tree = await both<ArchitectureTree>("/projects/EVP/architecture");
    expect(tree.direct.not_architectural.map((row) => row.key).sort()).toEqual(
      [hidden.key, grand.key].sort()
    );
    expect(tree.proxied.not_architectural).toEqual(
      tree.direct.not_architectural
        .filter((row) => row.key !== hidden.key)
        .map((row) => ({ ...row, inherited_from: null }))
    );
    expect(JSON.stringify(tree.proxied)).not.toContain(hidden.key);

    // A shape the filter does not handle fails closed: the server's refusal names the linked
    // keys, and the proxy withholds it rather than pass the hidden key on.
    const ambiguous = `/issues/resolve?ref=${encodeURIComponent("example/widgets#5")}`;
    expect(await failure(apiGet(`/api/v1${ambiguous}`, agent))).toContain(hidden.key);
    const withheld = await failure(apiGet(`/api/v1${ambiguous}`, via));
    expect(withheld).toContain('failed: 502 {"code":"EVAL_PROXY_WITHHELD"');
    expect(withheld).not.toContain(hidden.key);

    // A GET off the allow-list is refused, and every request is recorded.
    expect(await failure(apiGet("/api/v1/inbox", via))).toContain(
      'failed: 403 {"code":"EVAL_PROXY_REFUSED"'
    );
    const records = await proxy.records();
    expect(records.filter((line) => line.kind === "refused").map((line) => line.path)).toEqual([
      "/api/v1/inbox",
    ]);
    expect(records.filter((line) => line.status === 502).map((line) => line.path)).toEqual([
      "/api/v1/issues/resolve",
    ]);
  } finally {
    await proxy.stop();
  }
});

test("the routes the Dispatch skill tells an agent to read reach the server", async () => {
  skipUnlessLocalChromium();
  const kept = await seedProject();
  const proxy = await startProxy();
  try {
    const via = { ...agent, base: proxy.url } satisfies ApiOptions;
    for (const route of ["", "/schema/blocks", `/issues/${kept.key}/artifacts/spec/blocks`]) {
      expect(await apiGet(`/api/v1${route}`, via), route).toEqual(
        await apiGet(`/api/v1${route}`, agent)
      );
    }
    // Without a source the architecture read is the server's own 404, not a refusal.
    const architecture = "/api/v1/projects/EVP/architecture";
    expect(await failure(apiGet(architecture, via))).toEqual(
      await failure(apiGet(architecture, agent))
    );
  } finally {
    await proxy.stop();
  }
});

interface Seeded {
  readonly kept: Issue;
  readonly keptSpec: string;
  readonly keptSeq: number;
  readonly idle: Issue;
  readonly upstreamAsk: Ask;
  readonly upstreamComment: Comment;
  readonly direct: Message;
  readonly seededAt: string;
}

/** What exists before the agent writes: the kept issue with an ask, a comment and its spec, an
 *  idle issue no write touches, and a human's direct message to the agent's session. */
async function seedWorld(): Promise<Seeded> {
  const kept = await seedProject();
  const idle = await createIssue(
    { project: "EVP", title: "An idle issue nobody touches", force: true },
    agent
  );
  const upstreamAsk = await createAsk(
    kept.key,
    { question: "Seeded: which release?", options: [{ label: "This one" }, { label: "The next" }] },
    agent
  );
  const upstreamComment = await createComment(
    kept.key,
    { body: "Seeded: a finding to resolve." },
    agent
  );
  const direct = await createAgentMessage(SESSION, {
    body: "Seeded: a human asks the agent directly.",
    delivery: "btw",
  });
  const details = await getIssue(kept.key, agent);
  await delay(50);
  const seededAt = new Date().toISOString();
  await delay(50);
  return {
    kept,
    keptSpec: details.primary_artifact_id,
    keptSeq: details.last_seq,
    idle,
    upstreamAsk,
    upstreamComment,
    direct,
    seededAt,
  };
}

/** Every write the proxy models, in one order, against one origin. */
async function writeAll(via: ApiOptions, seeded: Seeded): Promise<Written> {
  const { kept, keptSpec, upstreamAsk, upstreamComment, direct } = seeded;
  const created = await createIssue(
    {
      project: "EVP",
      title: "An issue the agent files",
      spec: "Its spec.",
      parent: kept.key,
      labels: ["eval"],
      priority: 2,
      force: true,
    },
    via
  );
  // Named without a spec, the issue gets the server's default one.
  const bare = await createIssue(
    { project: "EVP", title: "An issue filed without a spec", force: true },
    via
  );
  const claimed = await claimIssue(created.key, via);
  const keptPatched = await patchIssue(
    kept.key,
    { status: "in_progress", priority: 1, labels: ["eval", "Proxy"] },
    via
  );
  const ask = await createAsk(
    kept.key,
    { question: "Which way?", options: [{ label: "A" }, { label: "B" }] },
    via
  );
  const comment = await createComment(kept.key, { body: "A finding." }, via);
  const reply = await createComment(
    kept.key,
    { body: "A reply to the finding.", reply_to: comment.id },
    via
  );
  const askReply = await createComment(
    kept.key,
    { body: "Still looking into it.", ask_id: upstreamAsk.id, turn: "agent" },
    via
  );
  const message = await createMessage(kept.key, { body: "READY-shaped packet." }, via);
  const messageReply = await createMessage(
    kept.key,
    { body: "A follow-up to the packet.", in_reply_to: message.id },
    via
  );
  const doc = await createIssueArtifact(
    kept.key,
    { name: "notes.md", content: "# Notes\n\nFirst." },
    via
  );
  const docAgain = await createIssueArtifact(
    kept.key,
    { name: "notes.md", content: "# Notes\n\nSecond." },
    via
  );
  const projectDoc = await createProjectDocument(
    "EVP",
    { name: "Eval notes", content: "# Eval notes\n\nA project document." },
    via
  );
  const docAsk = await createArtifactAsk(
    projectDoc.artifact.id,
    { question: "Is this document right?" },
    via
  );
  const docComment = await createArtifactComment(
    projectDoc.artifact.id,
    { body: "A note on the document." },
    via
  );
  const approval = await requestApproval(keptSpec, via);
  const edited = await editAsk(ask.id, { question: "Which way now?" }, via);
  const upstreamEdited = await editAsk(upstreamAsk.id, { urgency: "high" }, via);
  const resolvedAsk = await resolveAsk(
    upstreamAsk.id,
    { kind: "resolved", reason: "Settled in the thread." },
    via
  );
  const resolvedComment = await resolveComment(upstreamComment.id, via);
  const directReply = await replyToMessageDelivery(
    direct.id,
    { attempt: 1, body: "On it." },
    actor,
    {},
    via
  );
  const directRetry = await replyToMessageDelivery(
    direct.id,
    { attempt: 1, body: "On it." },
    actor,
    {},
    via
  );
  const released = await releaseIssue(created.key, via);
  const closed = await patchIssue(created.key, { status: "done" }, via);
  return {
    created,
    bare,
    claimed,
    keptPatched,
    ask,
    comment,
    reply,
    askReply,
    message,
    messageReply,
    doc,
    docAgain,
    projectDoc,
    docAsk,
    docComment,
    approval,
    edited,
    upstreamEdited,
    resolvedAsk,
    resolvedComment,
    directReply,
    directRetry,
    released,
    closed,
  };
}

interface Written {
  readonly created: Issue;
  readonly bare: Issue;
  readonly claimed: Issue;
  readonly keptPatched: Issue;
  readonly ask: Ask;
  readonly comment: Comment;
  readonly reply: Comment;
  readonly askReply: Comment;
  readonly message: { id: string };
  readonly messageReply: { id: string };
  readonly doc: ArtifactUploadResponse;
  readonly docAgain: ArtifactUploadResponse;
  readonly projectDoc: ArtifactUploadResponse;
  readonly docAsk: Ask;
  readonly docComment: Comment;
  readonly approval: { ask: Ask; artifact_id: string; version: number };
  readonly edited: Ask;
  readonly upstreamEdited: Ask;
  readonly resolvedAsk: Ask;
  readonly resolvedComment: Comment;
  readonly directReply: Message | MessageDelivery;
  readonly directRetry: Message | MessageDelivery;
  readonly released: Issue;
  readonly closed: Issue;
}

/** Every read whose answer the writes change, keyed by what it reads. */
async function readAll(via: ApiOptions, seeded: Seeded, written: Written) {
  const { kept, keptSpec, keptSeq, upstreamAsk, upstreamComment, direct, seededAt } = seeded;
  const { created, ask, comment, message, messageReply, doc, projectDoc, docAsk, docComment } =
    written;
  const routes: Record<string, string> = {
    "the kept issue": `/issues/${kept.key}`,
    "the created issue": `/issues/${created.key}`,
    "the default spec": `/artifacts/${written.bare.primary_artifact_id}/text`,
    "the kept issue's events": `/issues/${kept.key}/events`,
    "the kept issue's events after the seed": `/issues/${kept.key}/events?after=${keptSeq}&limit=3`,
    "the kept issue's newest events": `/issues/${kept.key}/events?order=desc&limit=4`,
    "the kept issue's events before one": `/issues/${kept.key}/events?before=${keptSeq + 4}&limit=2`,
    "the created issue's second event": `/issues/${created.key}/events?after=1&limit=1`,
    "the kept issue's asks": `/issues/${kept.key}/asks`,
    "the kept issue's open asks": `/issues/${kept.key}/asks?state=open`,
    "the kept issue's answered asks": `/issues/${kept.key}/asks?state=answered`,
    "the kept issue's comments": `/issues/${kept.key}/comments`,
    "the resolved upstream ask": `/asks/${upstreamAsk.id}`,
    "the created ask": `/asks/${ask.id}`,
    "the approval ask": `/asks/${written.approval.ask.id}`,
    "the document ask": `/asks/${docAsk.id}`,
    "the resolved upstream comment": `/comments/${upstreamComment.id}`,
    "the created comment's thread": `/comments/${comment.id}`,
    "the document comment": `/comments/${docComment.id}`,
    "the direct message's thread": `/messages/${direct.id}?session=${SESSION}`,
    "the issue message": `/issues/${kept.key}/messages/${message.id}`,
    "the issue message's thread by its reply": `/messages/${messageReply.id}`,
    "the agent's open asks": `/asks/open?author_session=${SESSION}`,
    "the project's open asks": "/asks/open?project=EVP",
    "the project's issues": "/issues?project=EVP",
    "issues in progress": "/issues?project=EVP&status=in_progress",
    "issues labelled eval": "/issues?project=EVP&label=EVAL",
    "issues at priority 1 or 2": "/issues?project=EVP&priority=1&priority=2",
    "issues with no priority": "/issues?project=EVP&priority=none",
    "the kept issue's children": `/issues?parent=${kept.key}`,
    "open issues": "/issues?project=EVP&open=true",
    "issues updated since the seed": `/issues?project=EVP&updated_since=${encodeURIComponent(seededAt)}`,
    "the uploaded document": `/artifacts/${doc.artifact.id}`,
    "the uploaded document's text": `/artifacts/${doc.artifact.id}/text`,
    "the spec awaiting approval": `/artifacts/${keptSpec}`,
    "the project's documents": "/projects/EVP/artifacts",
    "the project document by slug": `/projects/EVP/artifacts/${projectDoc.artifact.slug}`,
    "the project document's asks": `/artifacts/${projectDoc.artifact.id}/asks`,
    "the project document's comments": `/artifacts/${projectDoc.artifact.id}/comments`,
    "the project document's events": `/artifacts/${projectDoc.artifact.id}/events`,
  };
  const reads: Record<string, unknown> = {};
  for (const [name, route] of Object.entries(routes)) {
    // A refused read is an answer to compare like any other, not the end of the comparison.
    reads[name] = await apiGet(`/api/v1${route}`, via).catch((error: Error) => error.message);
  }
  return reads;
}

/** Reads of everything the writes touch, straight from the server. */
async function serverState(seeded: Seeded) {
  const { kept, keptSpec, upstreamAsk, upstreamComment, direct } = seeded;
  const routes = [
    `/issues/${kept.key}`,
    `/issues/${kept.key}/events`,
    `/issues/${kept.key}/comments`,
    `/issues/${kept.key}/asks`,
    "/issues?project=EVP",
    "/projects/EVP/artifacts",
    `/artifacts/${keptSpec}`,
    `/asks/${upstreamAsk.id}`,
    `/comments/${upstreamComment.id}`,
    `/messages/${direct.id}?session=${SESSION}`,
    `/asks/open?author_session=${SESSION}`,
  ];
  const state: Record<string, unknown> = {};
  for (const route of routes) {
    // The open-ask list reports when it was read and how old each ask is; nothing else moves
    // unless something wrote.
    const read = JSON.stringify(await apiGet(`/api/v1${route}`, agent), (key, value) =>
      key === "as_of" || key === "age_seconds" ? undefined : value
    );
    state[route] = JSON.parse(read);
  }
  return state;
}

test("each write the proxy models stays off the server, answers as the server does, and reads back as the server would", async () => {
  skipUnlessLocalChromium();
  const seeded = await seedWorld();
  const before = await serverState(seeded);
  const proxy = await startProxy();
  const proxiedStatuses: number[] = [];
  const directStatuses: number[] = [];
  let proxied: Written;
  let proxiedReads: Record<string, unknown>;
  try {
    const via = { ...agent, base: proxy.url } satisfies ApiOptions;
    proxied = await writeAll({ ...via, statuses: proxiedStatuses }, seeded);
    // None of it reached the server.
    expect(await serverState(seeded)).toEqual(before);
    proxiedReads = await readAll(via, seeded, proxied);
    const records = await proxy.records();
    const writes = records.filter((line) => line.kind === "write");
    expect(writes).toHaveLength(proxiedStatuses.length);
    expect(writes.map((line) => line.status)).toEqual(proxiedStatuses);
    expect(writes[0]?.body).toMatchObject({ title: "An issue the agent files", actor });
    expect(writes[0]?.response).toEqual(proxied.created);
  } finally {
    await proxy.stop();
  }

  // The same writes against the server itself, from the same starting state.
  const direct = await writeAll({ ...agent, statuses: directStatuses }, seeded);
  const directReads = await readAll(agent, seeded, direct);
  const rename = new Map([
    [proxied.created.key, direct.created.key],
    [proxied.bare.key, direct.bare.key],
    [String(proxied.created.number), String(direct.created.number)],
    [String(proxied.bare.number), String(direct.bare.number)],
  ]);
  expect.soft(proxiedStatuses, "each write's status").toEqual(directStatuses);
  for (const name of Object.keys(direct) as (keyof Written)[]) {
    expect
      .soft(comparable(proxied[name], rename), `the answer to ${name}`)
      .toEqual(comparable(direct[name], rename));
  }
  for (const name of Object.keys(directReads)) {
    expect
      .soft(comparable(proxiedReads[name], rename), `the read of ${name}`)
      .toEqual(comparable(directReads[name], rename));
  }
});

test("a write the proxy does not model is recorded and answered 501, never a guessed success", async () => {
  skipUnlessLocalChromium();
  const kept = await seedProject();
  const spec = (await getIssue(kept.key, agent)).primary_artifact_id;
  const ask = await createAsk(kept.key, { question: "Which way?" }, agent);
  const proxy = await startProxy();
  try {
    const via = { ...agent, base: proxy.url } satisfies ApiOptions;
    const unmodelled = 'failed: 501 {"code":"EVAL_PROXY_UNMODELLED_WRITE"';
    const edits = "the evaluation proxy does not apply document edits";
    const edit = { ops: [{ op: "replace" as const, find: "kept", with: "edited" }] };
    expect(await failure(editArtifact(spec, edit, via))).toContain(edits);
    expect(
      await failure(apiPost(`/api/v1/issues/${kept.key}/artifacts/spec/edits`, edit, via))
    ).toContain(edits);
    for (const [name, call] of [
      [
        "an issue create that would need the duplicate check",
        () => createIssue({ project: "EVP", title: "Not forced" }, via),
      ],
      [
        "a targeted message",
        () =>
          createMessage(
            kept.key,
            { body: "Hello", target: "role:architect", delivery: "btw" },
            via
          ),
      ],
      [
        "a comment with mentions",
        () =>
          createComment(kept.key, { body: "Hey", mentions: [{ target: "role:architect" }] }, via),
      ],
      [
        "an anchored comment",
        () =>
          createComment(kept.key, { body: "Here", anchor: { artifact: spec, quote: "kept" } }, via),
      ],
      ["a rank move", () => patchIssue(kept.key, { rank: { after: kept.key } }, via)],
      ["a follow", () => followAsk(ask.id, SESSION, actor, via)],
      ["a named version", () => createNamedVersion(spec, "v1", via)],
      ["an architecture sync", () => syncArchitectureSource("EVP", via)],
      [
        "a route Dispatch has no write for",
        () => apiPost(`/api/v1/issues/${kept.key}/pins`, {}, via),
      ],
    ] as const) {
      expect(await failure(call()), name).toContain(unmodelled);
    }
    const records = await proxy.records();
    expect(records.filter((line) => line.status === 501)).toHaveLength(11);
    expect((await apiGet<IssueSummary[]>("/api/v1/issues?project=EVP", agent)).length).toBe(1);
  } finally {
    await proxy.stop();
  }
});

test("an upstream that fails is answered in JSON naming the failure, and recorded", async () => {
  skipUnlessLocalChromium();
  // A port that was free a moment ago and has nothing listening on it now.
  const closed = Promise.withResolvers<number>();
  const probe = createServer().listen(0, "127.0.0.1", () => {
    const address = probe.address();
    probe.close(() => closed.resolve(typeof address === "object" && address ? address.port : 0));
  });
  const port = await closed.promise;
  const proxy = await startProxy({ upstream: `http://127.0.0.1:${port}` });
  try {
    const via = { ...agent, base: proxy.url } satisfies ApiOptions;
    const read = await failure(apiGet("/api/v1/issues/EVP-1", via));
    expect(read).toContain('failed: 502 {"code":"EVAL_PROXY_UPSTREAM_FAILED"');
    expect(read).toContain(`127.0.0.1:${port}`);
    const write = await failure(createComment("EVP-1", { body: "A finding." }, via));
    expect(write).toContain('failed: 502 {"code":"EVAL_PROXY_UPSTREAM_FAILED"');
    const malformed = await fetch(`${proxy.url}/api/v1/issues/%E0%A4%A`);
    expect(malformed.status).toBe(400);
    expect(await malformed.json()).toMatchObject({ code: "EVAL_PROXY_BAD_PATH" });
    expect(
      (await proxy.records()).map((line) => [line.kind, line.method, line.path, line.status])
    ).toEqual([
      ["read", "GET", "/api/v1/issues/EVP-1", 502],
      ["write", "POST", "/api/v1/issues/EVP-1/comments", 502],
      ["refused", "GET", "/api/v1/issues/%E0%A4%A", 400],
    ]);
  } finally {
    await proxy.stop();
  }
});

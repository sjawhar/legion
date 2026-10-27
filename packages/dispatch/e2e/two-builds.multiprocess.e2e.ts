import { readdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  type Browser,
  expect,
  type Locator,
  type Page,
  type Request,
  test,
} from "@playwright/test";

import type { ArtifactUploadResponse, CredentialRecord } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  baseUrl,
  createBroadcast,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
  dashboardOrigin,
  publishAgentStreamFrame,
  putArchitectureSource,
  syncArchitectureSource,
  userHeaders,
} from "./api";
import { seedDeliveryFixture } from "./delivery-seed";
import { connectedDot, documentEditor } from "./editor";
import { setCredentialRecords } from "./fake-broker-helpers";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  type DispatchProcess,
  pinnedTo,
  processPinCookie,
  processPinHeader,
} from "./harness-processes";
import { resetDatabase } from "./seed";
import { type LazyPiece, lazyPieces, type WalkRoutePath, walkRoutes } from "./two-builds-routes";
import { signIn } from "./users";

/**
 * A rolling release of Dispatch runs two builds at once behind one load balancer: a page one
 * build's process served names chunks only that build emitted, and the other process may be the
 * one asked for them. Here Dispatch A serves `web/dist` and B serves `web/dist-b`, a second build
 * of the same source with another build id (`bun run e2e:multiprocess`), and every route of the
 * app is opened with its page from one process and every `/assets/` request pinned to the other.
 * Each must render, ask for no asset that fails, and between the routes ask for every script and
 * stylesheet its build emitted. Then every file of both builds is fetched from both processes.
 */

const envoyDirectory = fileURLToPath(new URL("../../envoy/", import.meta.url));

/** Each process's web dist, read as e2e/run-server.sh hands it to the server: a path from
 *  packages/envoy, A's always `../dispatch/web/dist`, B's `DISPATCH_E2E_WEB_DIST_B` when set
 *  (e2e/harness-listeners.ts). */
const webDists: Record<DispatchProcess, string> = {
  a: resolve(envoyDirectory, "../dispatch/web/dist"),
  b: resolve(envoyDirectory, process.env.DISPATCH_E2E_WEB_DIST_B || "../dispatch/web/dist"),
};

interface Build {
  readonly dist: string;
  /** The entry chunk the build's `index.html` names, `/assets/index-*.js`. */
  readonly entry: string;
  /** Every file of the build's `assets` directory, by name. */
  readonly assets: readonly string[];
}

function readBuild(dist: string): Build {
  const page = readFileSync(join(dist, "index.html"), "utf8");
  const entry = /<script type="module" crossorigin src="(\/assets\/[^"]+\.js)"><\/script>/.exec(
    page
  )?.[1];
  if (entry === undefined) throw new Error(`${dist}/index.html names no entry chunk`);
  const assets = readdirSync(join(dist, "assets"), { withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => entry.name)
    .sort();
  return { assets, dist, entry };
}

let builds: Record<DispatchProcess, Build>;

/** Scripts and stylesheets of a build that no route can load, each with the reason and the
 *  source that makes it so. None today: every chunk either build emits is loaded by a route or
 *  a lazy piece the walk opens. */
const unreachable: readonly { readonly file: RegExp; readonly reason: string }[] = [];

// Two identical builds would pass every row below on a server that cannot serve the other
// build's assets at all, since each process would hold every file asked for.
test.beforeAll(() => {
  builds = { a: readBuild(webDists.a), b: readBuild(webDists.b) };
  expect(
    builds.b.entry,
    `${webDists.a}/index.html and ${webDists.b}/index.html name one entry chunk, so the two ` +
      "builds cannot tell a cross-build asset request from a same-build one. Build B with another " +
      "DISPATCH_BUILD, as `bun run e2e:multiprocess` does."
  ).not.toBe(builds.a.entry);
  expect(
    builds.b.assets,
    `${webDists.a}/assets and ${webDists.b}/assets hold the same files`
  ).not.toEqual(builds.a.assets);
});

test.beforeEach(async () => {
  await resetDatabase();
});

const session: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/two-builds",
  machine_id: "two-builds-host",
  roles: ["planner"],
  // The live view's stream hook takes a session uuid only, as a real session id is.
  session_id: "019a2b3c-0559-7000-8000-000000000559",
  title: "Two builds planner",
};

/** The text each seeded thing shows, which its route's landmark finds. */
const shown = {
  agentReply: "The release notes are read.",
  child: "Split the rollout",
  comment: "The rollout waits on the canary.",
  credentialReason: "need it for the rollout",
  document: "The handbook explains the rollout.",
  issue: "Roll out two builds",
  issueArtifact: "These notes track the rollout.",
  spec: "Both builds serve every page.",
};

const credentialRecord: CredentialRecord = {
  approver: "alice",
  decided: null,
  enrollment: { kind: "session", operator: "alice", runtime_id: "host-alice:559:0", slot: null },
  expires_at: new Date(Date.now() + 3_600_000).toISOString(),
  identifiers: ["ROLLOUT_API_KEY"],
  kind: "agent_secret",
  lifetime_seconds: 3600,
  record_id: "record-two-builds",
  requested_at: new Date().toISOString(),
  reason: shown.credentialReason,
  rules_version: "rules-v1",
  service: null,
  session: { enrollment: session.session_id, request: session.session_id },
  state: "pending",
};

/** The 1x1 PNG e2e/artifacts.e2e.ts uploads. */
const tinyPng =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

/** Uploads a picture to the session's conversation as alice, as a paste on the Agents page
 *  does, and answers its slug. */
async function uploadAgentPicture(sessionID: string): Promise<string> {
  const form = new FormData();
  form.append("name", "shot.png");
  form.append("file", new Blob([Buffer.from(tinyPng, "base64")], { type: "image/png" }));
  const response = await fetch(
    new URL(`/api/v1/agents/${encodeURIComponent(sessionID)}/artifacts`, baseUrl),
    { body: form, headers: await userHeaders("alice"), method: "POST" }
  );
  if (!response.ok) {
    throw new Error(`agent picture upload: ${response.status} ${await response.text()}`);
  }
  // The server's own upload answer, as e2e/api.ts reads every JSON answer.
  const uploaded = (await response.json()) as ArtifactUploadResponse;
  return uploaded.artifact.slug;
}

interface Seeded {
  readonly agentArtifact: string;
  readonly broadcast: string;
  readonly document: string;
  readonly issue: string;
  readonly issueArtifact: string;
}

/** What every route needs to show its own content rather than an empty or missing state: a
 *  project with an architecture source, an issue with a spec, a child, a comment and a document
 *  artifact, a project document, a live session with a broadcast, a replayed conversation and a
 *  picture, a credential record, and the delivery timeline's runs. */
async function seedEveryRoute(): Promise<Seeded> {
  await setLiveSessions([session]);
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "acme/architecture": {
      contents: "read",
      files: { "web.md": "---\ntitle: Web\n---\nThe dashboard.\n" },
      installation_id: 559,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "acme/architecture" });
  expect((await syncArchitectureSource("CORE", "acme/architecture")).last_error).toBeNull();
  const issue = await createIssue({
    project: "CORE",
    spec: `# Rollout\n\n${shown.spec}`,
    title: shown.issue,
  });
  await createIssue({ parent: issue.key, project: "CORE", title: shown.child });
  await createComment(issue.key, { body: shown.comment });
  const issueArtifact = await createIssueArtifact(issue.key, {
    content: `# Rollout notes\n\n${shown.issueArtifact}\n`,
    name: "rollout-notes.md",
  });
  const document = await createProjectDocument("CORE", {
    content: `# Handbook\n\n${shown.document}\n`,
    name: "handbook.md",
  });
  const broadcast = await createBroadcast({
    body: "Report **status**.",
    delivery: "steer",
    session_ids: [session.session_id],
  });
  await publishAgentStreamFrame(
    session.session_id,
    {
      frames: [
        {
          kind: "message",
          message: {
            at: 1_000,
            id: "u1000",
            parts: [{ text: "Read the release notes", type: "text" }],
            role: "user",
            streaming: false,
          },
          seq: 1,
          v: 1,
        },
        {
          kind: "message",
          message: {
            at: 2_000,
            id: "a2000",
            parts: [{ text: shown.agentReply, type: "text" }],
            role: "assistant",
            streaming: false,
          },
          seq: 2,
          v: 1,
        },
      ],
      session_id: session.session_id,
      v: 1,
    },
    "replay"
  );
  const agentArtifact = await uploadAgentPicture(session.session_id);
  await setCredentialRecords([credentialRecord]);
  await seedDeliveryFixture();
  return {
    agentArtifact,
    broadcast: broadcast.id,
    document: document.artifact.slug,
    issue: issue.key,
    issueArtifact: issueArtifact.artifact.slug,
  };
}

/** One page load of the walk: the route of `walkRoutes` it opens, the address, what on the page
 *  shows the route rendered its own content, and the lazy pieces that content needed. */
interface Visit {
  readonly route: WalkRoutePath;
  readonly url: string;
  readonly shows: (page: Page) => readonly Locator[];
  readonly loads?: readonly LazyPiece[];
}

function heading(page: Page, name: string): Locator {
  return page.getByRole("heading", { exact: true, level: 1, name });
}

/** A document open in the editor and connected: the editor chunk ran and the Yjs stack synced. */
function openDocument(page: Page, text: string): readonly Locator[] {
  return [documentEditor(page).filter({ hasText: text }), connectedDot(page)];
}

/** The timeline fixture's day (`seedDeliveryFixture`). */
const deliveryWindow = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

function visits(seed: Seeded): readonly Visit[] {
  const issue = `/issues/${seed.issue}`;
  const agent = `/agents/${session.session_id}`;
  return [
    { route: "/", shows: (page) => [heading(page, "Inbox")], url: "/" },
    {
      route: "/answers",
      shows: (page) => [
        heading(page, "Answered by you"),
        page.getByRole("region", { name: "Answers empty state" }),
      ],
      url: "/answers",
    },
    {
      route: "/replies",
      shows: (page) => [
        heading(page, "Replies"),
        page.getByRole("region", { name: "Replies empty state" }),
      ],
      url: "/replies",
    },
    { route: "/agents", shows: (page) => [heading(page, "Agents")], url: "/agents" },
    {
      // The preview is the headless renderer's: without it the bold is literal asterisks.
      loads: ["headless-markdown"],
      route: "/agents/broadcasts",
      shows: (page) => [
        heading(page, "Broadcasts"),
        page
          .getByRole("region", { exact: true, name: "Broadcasts" })
          .locator("[data-markdown-preview] strong", { hasText: "status" }),
      ],
      url: "/agents/broadcasts",
    },
    {
      route: "/agents/broadcasts/:id",
      shows: (page) => [
        page.getByRole("region", { exact: true, name: "Broadcast" }),
        heading(page, "Broadcast to 1 agent"),
      ],
      url: `/agents/broadcasts/${seed.broadcast}`,
    },
    {
      route: "/agents/:sessionId/live",
      shows: (page) => [
        page.getByTestId("agent-message-assistant").filter({ hasText: shown.agentReply }),
      ],
      url: `${agent}/live`,
    },
    {
      route: "/agents/:sessionId/artifacts/:slug",
      shows: (page) => [
        page.getByRole("heading", { name: "shot.png" }),
        page.getByRole("img", { name: "shot.png version 1" }),
      ],
      url: `${agent}/artifacts/${seed.agentArtifact}`,
    },
    { route: "/issues/:key/*", shows: (page) => [heading(page, shown.issue)], url: issue },
    {
      loads: ["editor"],
      route: "/issues/:key/*",
      shows: (page) => openDocument(page, shown.spec),
      url: `${issue}/spec`,
    },
    {
      route: "/issues/:key/*",
      shows: (page) => [page.locator("#issue-conversation-panel").getByText(shown.comment)],
      url: `${issue}/conversation`,
    },
    {
      route: "/issues/:key/*",
      shows: (page) => [page.locator("#issue-children-panel").getByText(shown.child)],
      url: `${issue}/children`,
    },
    {
      route: "/issues/:key/*",
      shows: (page) => [page.getByTestId(`artifact-${seed.issueArtifact}`)],
      url: `${issue}/artifacts`,
    },
    {
      loads: ["editor"],
      route: "/issues/:key/*",
      shows: (page) => openDocument(page, shown.issueArtifact),
      url: `${issue}/artifacts/${seed.issueArtifact}`,
    },
    {
      route: "/issues/:key/*",
      shows: (page) => [page.getByRole("list", { name: "Issue activity" })],
      url: `${issue}/activity`,
    },
    {
      // A project with an architecture source opens on its Architecture tab.
      route: "/projects/:key",
      shows: (page) => [
        heading(page, "Core"),
        page.getByRole("tab", { name: "Architecture", selected: true }),
      ],
      url: "/projects/CORE",
    },
    {
      route: "/projects/:key/architecture",
      shows: (page) => [
        page.getByTestId("architecture-source").filter({ hasText: "acme/architecture/main" }),
      ],
      url: "/projects/CORE/architecture",
    },
    {
      route: "/projects/:key/issues",
      shows: (page) => [
        page.locator("#project-issues-panel").getByText(shown.issue, { exact: true }).first(),
      ],
      url: "/projects/CORE/issues",
    },
    {
      route: "/projects/:key/documents",
      shows: (page) => [
        page.locator("#project-documents-panel").getByRole("listitem", { name: "handbook.md" }),
      ],
      url: "/projects/CORE/documents",
    },
    {
      loads: ["editor"],
      route: "/projects/:key/documents/:slug",
      shows: (page) => openDocument(page, shown.document),
      url: `/projects/CORE/documents/${seed.document}`,
    },
    {
      route: "/credentials/:recordId",
      shows: (page) => [
        heading(page, "Credential request"),
        page.getByText(shown.credentialReason, { exact: true }),
      ],
      url: `/credentials/${credentialRecord.record_id}`,
    },
    {
      route: "/credentials/machine",
      shows: (page) => [heading(page, "Enter machine login code")],
      url: "/credentials/machine",
    },
    { route: "/settings", shows: (page) => [heading(page, "Settings")], url: "/settings" },
    {
      // ECharts draws the timeline on a canvas it creates inside the chart's host.
      loads: ["echarts"],
      route: "/delivery",
      shows: (page) => [
        heading(page, "Delivery timeline"),
        page.getByTestId("delivery-chart").locator("canvas").first(),
      ],
      url: `/delivery?${deliveryWindow}`,
    },
    { route: "/pipeline", shows: (page) => [heading(page, "Pipeline")], url: "/pipeline" },
    { route: "*", shows: (page) => [heading(page, "Page not found")], url: "/no-such-page" },
  ];
}

function isAsset(url: URL): boolean {
  return url.origin === dashboardOrigin && url.pathname.startsWith("/assets/");
}

async function allVisible(locators: readonly Locator[]): Promise<boolean> {
  for (const locator of locators) if (!(await locator.isVisible())) return false;
  return true;
}

/** Opens every visit with the page document, API calls and sockets on `pageProcess` (the pin
 *  cookie) and every `/assets/` request on `assetProcess` (the pin header, which wins over the
 *  cookie), and then asks that every script and stylesheet of `pageProcess`'s build was answered
 *  200: the page names only its own build's chunks. */
async function walk(
  browser: Browser,
  pageProcess: DispatchProcess,
  assetProcess: DispatchProcess
): Promise<void> {
  const seed = await seedEveryRoute();
  const planned = visits(seed);
  expect(
    walkRoutes.filter((route) => !planned.some((visit) => visit.route === route.path)),
    "every route of e2e/two-builds-routes.ts has a visit"
  ).toEqual([]);
  expect(
    Object.keys(lazyPieces).filter((piece) =>
      planned.every((visit) => !visit.loads?.includes(piece as LazyPiece))
    ),
    "every lazy piece of e2e/two-builds-routes.ts has a visit that loads it"
  ).toEqual([]);

  const assetsFrom = `Dispatch ${assetProcess.toUpperCase()}`;
  const context = await browser.newContext();
  await context.addCookies([{ name: processPinCookie, url: baseUrl, value: pageProcess }]);
  await signIn(context, "alice");
  await context.route(isAsset, (route) =>
    route.continue({ headers: { ...route.request().headers(), [processPinHeader]: assetProcess } })
  );
  const page = await context.newPage();

  /** Every status each asset path was answered with, across the walk. */
  const answers = new Map<string, number[]>();
  /** Asset requests not yet finished: a visit ends only once none is, so the next navigation
   *  never cancels one and reads as a failed request. */
  const pending = new Set<Request>();
  /** What went wrong on the current visit. */
  let problems: string[] = [];
  page.on("request", (request) => {
    if (isAsset(new URL(request.url()))) pending.add(request);
  });
  page.on("requestfinished", (request) => pending.delete(request));
  page.on("requestfailed", (request) => {
    pending.delete(request);
    const url = new URL(request.url());
    if (isAsset(url)) {
      problems.push(`${url.pathname} from ${assetsFrom} failed: ${request.failure()?.errorText}`);
    }
  });
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (!isAsset(url)) return;
    answers.set(url.pathname, [...(answers.get(url.pathname) ?? []), response.status()]);
    if (response.status() >= 400) {
      problems.push(`${url.pathname} from ${assetsFrom} answered ${response.status()}`);
    }
  });
  page.on("pageerror", (error) => problems.push(`uncaught ${error.name}: ${error.message}`));

  const failures: { readonly url: string; readonly problems: readonly string[] }[] = [];
  try {
    for (const visit of planned) {
      problems = [];
      const shows = visit.shows(page);
      try {
        await page.goto(visit.url);
        // A failed asset ends the visit at once: the landmark it keeps from rendering would
        // only add a timeout.
        await expect.poll(async () => problems.length > 0 || (await allVisible(shows))).toBe(true);
        if (problems.length === 0) {
          await expect(page.locator("#root")).not.toBeEmpty();
          await expect.poll(() => pending.size).toBe(0);
        }
      } catch (error) {
        for (const locator of shows) {
          if (!(await locator.isVisible())) problems.push(`${locator} is not visible`);
        }
        if (problems.length === 0) problems.push(String(error));
      }
      if (problems.length > 0) failures.push({ problems, url: visit.url });
    }
  } finally {
    await context.close();
  }

  const build = builds[pageProcess];
  expect
    .soft(
      failures,
      `every route renders with its page from Dispatch ${pageProcess.toUpperCase()} and its ` +
        `assets from ${assetsFrom}`
    )
    .toEqual([]);
  const missed = build.assets.filter(
    (file) =>
      /\.(js|css)$/.test(file) &&
      !unreachable.some(({ file: pattern }) => pattern.test(file)) &&
      !answers.get(`/assets/${file}`)?.includes(200)
  );
  expect(
    missed,
    `every script and stylesheet of ${build.dist}/assets was answered 200 by ${assetsFrom}`
  ).toEqual([]);
}

// A walk opens about twenty-five pages, each waiting on its own content, so it outlasts the
// suite's 30 s test timeout by design.
const walkTimeout = 300_000;

test("every route renders with its page from Dispatch A and every asset from Dispatch B", async ({
  browser,
}) => {
  test.setTimeout(walkTimeout);
  await walk(browser, "a", "b");
});

test("every route renders with its page from Dispatch B and every asset from Dispatch A", async ({
  browser,
}) => {
  test.setTimeout(walkTimeout);
  await walk(browser, "b", "a");
});

test("every file of both builds' assets answers 200 with its bytes from Dispatch A and from Dispatch B", async () => {
  const wrong: string[] = [];
  for (const target of ["a", "b"] as const) {
    for (const build of [builds.a, builds.b]) {
      await Promise.all(
        build.assets.map(async (file) => {
          const response = await fetch(new URL(`/assets/${file}`, baseUrl), {
            headers: pinnedTo(target),
          });
          const where = `${build.dist}/assets/${file} from Dispatch ${target.toUpperCase()}`;
          if (response.status !== 200) {
            wrong.push(`${where} answered ${response.status}`);
            await response.body?.cancel();
            return;
          }
          const bytes = Buffer.from(await response.arrayBuffer());
          if (!bytes.equals(readFileSync(join(build.dist, "assets", file)))) {
            wrong.push(`${where} answered other bytes than the file's`);
          }
        })
      );
    }
  }
  expect(wrong.sort()).toEqual([]);
});

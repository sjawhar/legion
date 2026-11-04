import { readdirSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  type Browser,
  type BrowserContext,
  expect,
  type Page,
  test,
  type WebSocket,
} from "@playwright/test";

import type { LiveClientFrame, LiveServerFrame } from "../web/src/api/types";
import {
  baseUrl,
  createAsk,
  createIssue,
  createProject,
  e2eAgentToken,
  getAsk,
  getIssueArtifactText,
  listComments,
} from "./api";
import { seedDeliveryFixture } from "./delivery-seed";
import { connectedDot, documentEditor, typeAtEnd } from "./editor";
import {
  apiPinCookie,
  binaryB,
  type DispatchProcess,
  dispatchProcesses,
  pagePinCookie,
  pinnedTo,
} from "./harness-processes";
import type { ProxiedRequest, ProxiedSocket } from "./round-robin-proxy";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

/**
 * A rolling release runs two builds of Dispatch at once behind one load balancer, so a page one
 * build served talks to the other build's server. Here a browser context's page requests (its
 * navigations, `index.html`, `/assets/*`) are pinned to one process and its API, event stream,
 * websockets and `/auth` to the other (the proxy's `dispatch-e2e-page` and `dispatch-e2e-api`
 * cookies, e2e/round-robin-proxy.ts). With `DISPATCH_E2E_BINARY_B` naming a previous release's
 * envoy-dispatch and `DISPATCH_E2E_WEB_DIST_B` its web dist, Dispatch B is that release, so each
 * direction runs one build's page against the other build's server; without it both processes run
 * this tree. In each direction the page signs in, opens a project's issue list and an issue, posts
 * a comment, answers an ask, opens the issue's document - where that server admits the page
 * writable it types and sees the edit saved and shown in a context of the other build's page, and
 * where it admits the page read-only the page says so and the stored document is untouched
 * (`pageWritable`) - and opens the Delivery page, whose live socket subscribes to the timeline and
 * is answered a snapshot. Every selector is one both builds' own browser suites use.
 *
 * Each seed call names the process it runs on (`process` in e2e/api.ts, which pins its sign-in
 * there too), so the build that wrote a row and the build that reads it back are both known.
 */

/** What a message calls each process's build. */
const builds: Record<DispatchProcess, string> = {
  a: "this tree's build (Dispatch A)",
  b: binaryB === undefined ? "this tree's build (Dispatch B)" : "the previous build (Dispatch B)",
};

const seedAgent = { kind: "session", id: "e2e-cross-build" } as const;

/** packages/envoy, which `DISPATCH_E2E_BINARY_B` is a path from, and the directory
 *  build-dispatch-build.sh wrote that build into: the binary's own, which also holds the build's
 *  `pmdoc/schema/blocks.json` and its `web/` dist. */
const envoyDir = fileURLToPath(new URL("../../envoy", import.meta.url));
const buildB = binaryB === undefined ? undefined : dirname(resolve(envoyDir, binaryB));

/** The schema manifest version a build's server reads and admits a page by. */
function manifestVersion(process: DispatchProcess): number {
  const path =
    process === "b" && buildB !== undefined
      ? resolve(buildB, "pmdoc", "schema", "blocks.json")
      : resolve(envoyDir, "internal", "dispatch", "pmdoc", "schema", "blocks.json");
  const manifest: unknown = JSON.parse(readFileSync(path, "utf8"));
  if (
    manifest === null ||
    typeof manifest !== "object" ||
    !("version" in manifest) ||
    typeof manifest.version !== "number"
  ) {
    throw new Error(`${path} declares no schema version`);
  }
  return manifest.version;
}

/** Whether a build's page carries the manifest it was built with, which it presents as
 *  `schema_version` and `schema_creatable` (web/src/features/doc/schema.ts). A page of a build
 *  before that reads the schema from the server it talks to and sends `schema_version` alone. */
function pageCarriesManifest(process: DispatchProcess): boolean {
  if (process !== "b" || buildB === undefined) {
    return true;
  }
  const assets = resolve(buildB, "web", "assets");
  return readdirSync(assets).some(
    (file) =>
      file.endsWith(".js") &&
      readFileSync(resolve(assets, file), "utf8").includes("schema_creatable")
  );
}

/**
 * Whether a page of `pageProcess` may write a document on the server of `server`.
 *
 * A page that carries no manifest of its own takes its `schema_version` from the schema route of
 * the server it talks to and sends no `schema_creatable`, so it presents that server's own
 * version and creates up to 1: every server admits it, its own rule (a version equal to the
 * server's) and this tree's (R 1 at most what the server reads, and a version at least what the
 * server creates and the document holds) alike.
 *
 * A page that carries its manifest presents that manifest's version, so a server reading another
 * version admits it read-only: a build before the fleet columns compares the token with its own
 * version exactly (`schemaReadOnly`), and this tree's rule refuses a page whose version is below
 * the highest `since` the fleet creates (`docs.schemaFleet.admits`).
 */
function pageWritable(pageProcess: DispatchProcess, server: DispatchProcess): boolean {
  return (
    !pageCarriesManifest(pageProcess) || manifestVersion(pageProcess) === manifestVersion(server)
  );
}

/** The day `seedDeliveryFixture` writes its timeline on. */
const deliveryWindow = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

test.beforeEach(async () => {
  await resetDatabase();
  expect((await fetch(new URL("/_proxy/reset", baseUrl), { method: "POST" })).status).toBe(204);
});

/** A signed-in context whose page comes from `page` and whose every other request goes to `api`. */
async function splitContext(
  browser: Browser,
  page: DispatchProcess,
  api: DispatchProcess
): Promise<BrowserContext> {
  const context = await browser.newContext();
  await context.addCookies([
    { name: pagePinCookie, url: baseUrl, value: page },
    { name: apiPinCookie, url: baseUrl, value: api },
  ]);
  await signIn(context, "alice");
  return context;
}

/** The frames the page's live socket sends and receives, from the moment it opens. Both builds
 *  run every live subscription of a page over one `/ws/live` socket, with the same frames
 *  (web/src/api/live-socket.ts). */
function watchLiveSocket(page: Page): { sent: LiveClientFrame[]; received: LiveServerFrame[] } {
  const frames = { received: [] as LiveServerFrame[], sent: [] as LiveClientFrame[] };
  page.on("websocket", (socket: WebSocket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    socket.on("framesent", ({ payload }) => {
      frames.sent.push(JSON.parse(String(payload)) as LiveClientFrame);
    });
    socket.on("framereceived", ({ payload }) => {
      frames.received.push(JSON.parse(String(payload)) as LiveServerFrame);
    });
  });
  return frames;
}

for (const pageProcess of dispatchProcesses) {
  const server: DispatchProcess = pageProcess === "a" ? "b" : "a";
  test(`a page of ${builds[pageProcess]} works against the server of ${builds[server]}`, async ({
    browser,
  }) => {
    test.setTimeout(120_000);
    const on = { process: server };
    await createProject({ key: "XB", name: "Cross build" }, on);
    const issue = await createIssue(
      { project: "XB", spec: "# Cross build\n\nWritten before the edit.\n", title: "Two builds" },
      on
    );
    const ask = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
      { ...on, actor: seedAgent, as: "agent" }
    );
    // The Delivery page's timeline, written in Postgres: the live socket answers its timeline
    // subscription with a snapshot only once delivery is configured.
    await seedDeliveryFixture();

    const context = await splitContext(browser, pageProcess, server);
    try {
      const page = await context.newPage();
      const live = watchLiveSocket(page);

      // The project's issue list, and the issue from it.
      await page.goto("/projects/XB/issues");
      await page.getByRole("link", { name: new RegExp(issue.key) }).click();
      await expect(documentEditor(page)).toContainText("Written before the edit.");

      // A comment, which the other build's server lists.
      await page.goto(`/issues/${issue.key}/conversation`);
      const comment = `A comment from ${builds[pageProcess]}'s page`;
      const message = page
        .getByRole("form", { name: "Comment composer" })
        .getByRole("textbox", { name: "Comment" });
      await message.fill(comment);
      await message.press("Control+Enter");
      await expect(
        page.getByRole("list", { name: "Conversation turns" }).getByText(comment)
      ).toBeVisible();
      await expect
        .poll(async () =>
          (await listComments(issue.key, undefined, { process: pageProcess })).map(
            (written) => written.body
          )
        )
        .toContain(comment);

      // The ask, answered in place, which the other build's server reads as answered.
      const card = page.getByRole("region", { name: "Conversation" }).getByTestId(`ask-${ask.id}`);
      await card.getByRole("radio", { name: "Ship" }).check();
      await card.getByRole("button", { exact: true, name: "Answer" }).click();
      await expect(
        card.getByRole("list", { name: "Options" }).locator('li[data-selected="true"]')
      ).toContainText("Ship");
      await expect
        .poll(async () => (await getAsk(ask.id, { process: pageProcess })).answers.length)
        .toBe(1);

      // The issue's document. Where the other build's server admits this page writable
      // (pageWritable), an edit typed here is saved by that server, then read from the store by
      // this page's own server, and shown below in a context of the other build's page. Where it
      // admits the page read-only - a page carrying a manifest of another version than the
      // server reads - the page says so, takes no edit, and the stored document is untouched.
      await page.goto(`/issues/${issue.key}/spec`);
      await expect(documentEditor(page)).toContainText("Written before the edit.");
      await expect(connectedDot(page)).toHaveText("connected");
      const edit = `Typed on ${builds[pageProcess]}'s page.`;
      const writable = pageWritable(pageProcess, server);
      if (writable) {
        await typeAtEnd(page, edit);
      } else {
        await expect(page.locator('.dispatch-doc[data-read-only="true"]')).toBeVisible();
      }
      for (const reader of [server, pageProcess]) {
        await expect
          .poll(
            async () =>
              (await getIssueArtifactText(issue.key, "spec", { process: reader })).markdown,
            { timeout: 30_000 }
          )
          .toContain(writable ? edit : "Written before the edit.");
        if (!writable) {
          const { markdown } = await getIssueArtifactText(issue.key, "spec", { process: reader });
          expect(markdown).not.toContain(edit);
        }
      }

      // The Delivery page on the fixture's day: the page's live socket subscribes to the timeline
      // view, and the other build's server answers that subscription with a snapshot.
      await page.goto(`/delivery?${deliveryWindow}`);
      await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
      await expect
        .poll(() =>
          live.sent.flatMap((frame) => (frame.type === "subscribe" ? [frame.view.kind] : []))
        )
        .toContain("timeline");
      await expect
        .poll(() => {
          const timeline = live.sent.flatMap((frame) =>
            frame.type === "subscribe" && frame.view.kind === "timeline" ? [frame.sub] : []
          );
          return live.received.some(
            (frame) => frame.type === "snapshot" && timeline.includes(frame.sub)
          );
        })
        .toBe(true);

      // The split held: every asset came from the page's process, and the document and live
      // sockets went to the other. Read before the other build's context opens a page of its own.
      const assets = (
        (await (await fetch(new URL("/_proxy/requests", baseUrl))).json()) as ProxiedRequest[]
      ).filter((request) => request.path.startsWith("/assets/"));
      expect(assets.length).toBeGreaterThan(0);
      expect(assets.filter((request) => request.upstream !== pageProcess)).toEqual([]);
      const sockets = (await (
        await fetch(new URL("/_proxy/sockets", baseUrl))
      ).json()) as ProxiedSocket[];
      for (const path of ["/ws/doc/", "/ws/live"]) {
        const opened = sockets.filter((socket) => socket.path.startsWith(path));
        expect(opened.length, `a ${path} socket opened`).toBeGreaterThan(0);
        expect(opened.filter((socket) => socket.upstream !== server)).toEqual([]);
      }

      // The document, in a context of the other build's page against this page's own server: the
      // edit where this page could make one, and the text it opened with where it could not.
      const otherBuild = await splitContext(browser, server, pageProcess);
      try {
        const reading = await otherBuild.newPage();
        await reading.goto(`/issues/${issue.key}/spec`);
        await expect(documentEditor(reading)).toContainText(
          writable ? edit : "Written before the edit."
        );
      } finally {
        await otherBuild.close();
      }
    } finally {
      await context.close();
    }
  });
}

// A newer build writes an element this one does not know into a document both serve. Each page
// opening it says so, and offers no upload to replace it: a replacement deletes that element.
test("a document holding newer content shows that state and no replace drop zone", async ({
  browser,
}) => {
  test.skip(
    binaryB !== undefined,
    "the newer-content hook is this tree's; Dispatch B runs the previous build (DISPATCH_E2E_BINARY_B)"
  );
  await createProject({ key: "NEW", name: "Newer content" }, { process: "a" });
  const issue = await createIssue(
    { project: "NEW", spec: "Written before the newer element.\n", title: "Newer content" },
    { process: "a" }
  );
  // A /_test/ route goes to both processes (e2e/round-robin-proxy.ts).
  const injected = await fetch(
    new URL(`/api/v1/artifacts/${issue.primary_artifact_id}/_test/newer-content`, baseUrl),
    { headers: { Authorization: `Bearer ${e2eAgentToken}` }, method: "POST" }
  );
  expect(injected.status, await injected.text()).toBe(204);
  for (const target of dispatchProcesses) {
    const text = await fetch(
      new URL(`/api/v1/artifacts/${issue.primary_artifact_id}/text`, baseUrl),
      { headers: { Authorization: `Bearer ${e2eAgentToken}`, ...pinnedTo(target) } }
    );
    expect(text.status).toBe(409);
    expect(await text.text()).toContain('"code":"DOC_NEWER_CONTENT"');
  }

  for (const target of dispatchProcesses) {
    const context = await splitContext(browser, target, target);
    try {
      const page = await context.newPage();
      await page.goto(`/issues/${issue.key}/spec`);
      const editor = page.getByRole("region", { name: "Document editor" });
      await expect(
        editor.getByRole("status").filter({
          hasText: "This document holds content this version of Dispatch cannot show yet.",
        })
      ).toBeVisible();
      await expect(
        editor.getByText("Upload markdown to replace and repair this document.")
      ).toHaveCount(0);
      await expect(editor.getByLabel("Upload artifact")).toHaveCount(0);
      await expect(documentEditor(page)).toBeHidden();
    } finally {
      await context.close();
    }
  }
});

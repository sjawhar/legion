import { expect, type Page, type TestInfo, test, type WebSocketRoute } from "@playwright/test";
import { PIPELINE_AT, seedDeliveryFixture, seedPipelineFixture } from "./delivery-seed";
import { seedDeliveryReconcileGithub } from "./fake-github-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

interface LiveRecord {
  readonly [key: string]: unknown;
}

interface LiveFrame extends LiveRecord {
  readonly type: string;
}

interface LiveSocket {
  readonly frames: LiveFrame[];
  readonly opened: Promise<void>;
}

const deliveryGet = /\/api\/v1\/delivery\/(timeline|measures|pipeline)(?:\?|$)/;

/** Stores review screenshots outside Playwright's success-cleaned output when a caller requests it. */
function screenshotPath(testInfo: TestInfo, name: string): string {
  const directory = process.env.DISPATCH_DELIVERY_LIVE_SCREENSHOT_DIR;
  return directory === undefined ? testInfo.outputPath(name) : `${directory}/${name}`;
}

function isLiveRecord(value: unknown): value is LiveRecord {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function isLiveFrame(value: unknown): value is LiveFrame {
  return isLiveRecord(value) && typeof value.type === "string";
}

/** Decodes one server frame at the test boundary, so the assertions below never mistake malformed
 * transport data for an application update. */
function parseLiveFrame(payload: string): LiveFrame {
  const frame: unknown = JSON.parse(payload);
  if (!isLiveFrame(frame)) {
    throw new Error(`Delivery socket sent a frame without a string type: ${payload}`);
  }
  return frame;
}

/** Whether one observed delta contains the named pull-request upsert. */
function deltaIncludesPR(frame: LiveFrame, id: string): boolean {
  if (frame.type !== "delta" || !isLiveRecord(frame.items) || !isLiveRecord(frame.items.prs)) {
    return false;
  }
  const upsert = frame.items.prs.upsert;
  return Array.isArray(upsert) && upsert.some((pr) => isLiveRecord(pr) && pr.id === id);
}

/** The count entries a delta changed, preserving the family and facet that own every value. */
function deltaCounts(
  frame: LiveFrame
): { family: string; facet: string; value: string; count: unknown }[] {
  if (!isLiveRecord(frame.counts)) return [];
  const entries: { family: string; facet: string; value: string; count: unknown }[] = [];
  for (const [family, facets] of Object.entries(frame.counts)) {
    if (!isLiveRecord(facets)) continue;
    for (const [facet, values] of Object.entries(facets)) {
      if (!isLiveRecord(values)) continue;
      for (const [value, count] of Object.entries(values)) {
        entries.push({ family, facet, value, count });
      }
    }
  }
  return entries;
}

/** A snapshot count by its family, facet and value; undefined means the earlier body held none. */
function snapshotCount(frame: LiveFrame, family: string, facet: string, value: string): unknown {
  if (!isLiveRecord(frame.body) || !isLiveRecord(frame.body[family])) return undefined;
  const facets = frame.body[family];
  if (!isLiveRecord(facets[facet])) return undefined;
  return facets[facet][value];
}

/** The complete Delivery GET set, in its server order, for comparison with the resumed page. */
function deliveryPRIDs(body: unknown): string[] {
  if (!isLiveRecord(body) || !Array.isArray(body.prs)) {
    throw new Error("Delivery GET answered without a pull-request array");
  }
  return body.prs.map((pr) => {
    if (!isLiveRecord(pr) || typeof pr.id !== "string") {
      throw new Error("Delivery GET answered a pull request without an id");
    }
    return pr.id;
  });
}

/** Watches the page's real Delivery socket before navigation: no test injects a frame or replaces
 * the transport, so every assertion observes the server route the page actually uses. */
function watchDeliverySocket(page: Page): LiveSocket {
  const opened = Promise.withResolvers<void>();
  const frames: LiveFrame[] = [];
  let connected = false;
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname !== "/ws/delivery") return;
    if (!connected) {
      connected = true;
      opened.resolve();
    }
    socket.on("framereceived", (event) => {
      const payload = typeof event.payload === "string" ? event.payload : event.payload.toString();
      frames.push(parseLiveFrame(payload));
    });
  });
  return { frames, opened: opened.promise };
}

/** Proxies the real live route until a test cuts its connection, then rejects every reconnect so
 * the page remains observably reconnecting instead of immediately reopening after a 1012 restart. */
async function deliveryTransport(page: Page): Promise<{ disconnect: () => Promise<void> }> {
  const routes: WebSocketRoute[] = [];
  let disconnected = false;
  await page.routeWebSocket(/\/ws\/delivery$/u, async (route) => {
    if (disconnected) {
      await route.close({ code: 1011, reason: "test network drop" });
      return;
    }
    const server = route.connectToServer();
    routes.push(route);
    server.onMessage((message) => route.send(message));
  });
  return {
    disconnect: async () => {
      disconnected = true;
      await Promise.all(
        routes.splice(0).map((route) => route.close({ code: 1011, reason: "test network drop" }))
      );
    },
  };
}

async function waitForFrame(
  frames: readonly LiveFrame[],
  matches: (frame: LiveFrame) => boolean
): Promise<LiveFrame> {
  await expect.poll(() => frames.some(matches)).toBe(true);
  const frame = frames.find(matches);
  if (frame === undefined) throw new Error("a frame that Playwright just observed disappeared");
  return frame;
}

async function openLiveDelivery(page: Page): Promise<LiveSocket> {
  const live = watchDeliverySocket(page);
  await page.goto("/delivery?mode=list");
  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
  await live.opened;
  await waitForFrame(live.frames, (frame) => frame.type === "snapshot");
  return live;
}

/** Opens the page without relying on Playwright's Chromium-only socket-inspection events. */
async function openDeliveryPage(page: Page): Promise<void> {
  await page.goto("/delivery?mode=list");
  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
}

/** Whether one observed delta changes a named map key to the expected value. */
function deltaMapValue(frame: LiveFrame, map: string, key: string, value: string): boolean {
  return (
    frame.type === "delta" &&
    isLiveRecord(frame.maps) &&
    isLiveRecord(frame.maps[map]) &&
    frame.maps[map][key] === value
  );
}

/** The subscription a snapshot answered, so a test can tell its data frames from a freshness
 * frame, which every page receives without a subscription id. */
function snapshotSubscription(frame: LiveFrame): string {
  if (frame.type !== "snapshot" || typeof frame.sub !== "string") {
    throw new Error("Delivery socket sent a snapshot without its subscription id");
  }
  return frame.sub;
}

function insertMergedPR(
  number: number,
  title: string,
  issue: string | null = null
): Promise<string> {
  const issueValue = issue === null ? "null" : `'${issue}'`;
  return sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES (
       'acme/widgets', ${number}, '${title}', 'https://github.com/acme/widgets/pull/${number}', 'octocat',
       now() - interval '1 minute', now(), 3, 1, false, ${issueValue}, ARRAY[]::text[], false
     )`
  );
}

async function deliveryRequest(page: Page, path: string): Promise<unknown> {
  const response = await page.request.get(path, { headers: { Authorization: "Bearer e2e-token" } });
  expect(response.status()).toBe(200);
  return response.json();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a merge appears without a reload, and the page issues no delivery GET", async ({
  browser,
}, testInfo) => {
  await seedDeliveryFixture();
  await sql(
    `UPDATE delivery_settings SET last_event_at = now(), last_reconcile_at = now() WHERE singleton`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const deliveryRequests: string[] = [];
  page.on("request", (request) => {
    if (request.method() === "GET" && deliveryGet.test(new URL(request.url()).pathname)) {
      deliveryRequests.push(request.url());
    }
  });

  try {
    const live = await openLiveDelivery(page);
    await insertMergedPR(690, "feat: a merge that arrived live");

    await expect(page.getByText("feat: a merge that arrived live", { exact: true })).toBeVisible();
    await waitForFrame(live.frames, (frame) => deltaIncludesPR(frame, "acme/widgets#690"));
    expect(deliveryRequests).toEqual([]);
    await page.screenshot({
      path: screenshotPath(testInfo, "delivery-live-desktop.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

test("a production job's success shows the deploy before its run ends", async ({ browser }) => {
  await seedDeliveryFixture();
  await insertMergedPR(691, "feat: production job completes first");
  await sql(
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, completed_at, conclusion, url, head_branch, event
     ) VALUES (
       'acme/widgets', 691, 'deploy', 'live691', now(), now(), null, null,
       'https://github.com/acme/widgets/actions/runs/691', 'main', 'push'
     )`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
     VALUES ('acme/widgets', 691, 'widgets-release / widgets-release', now(), null, null)`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await openLiveDelivery(page);
    await expect(page.getByText("waiting", { exact: true })).toBeVisible();
    await sql(
      `UPDATE delivery_run_jobs
       SET completed_at = now(), conclusion = 'success'
       WHERE repo = 'acme/widgets' AND run_id = 691 AND name = 'widgets-release / widgets-release'`
    );

    await expect(page.getByRole("button", { name: "Open deploy run #691" })).toBeVisible();
    expect(
      await sql(
        "SELECT completed_at IS NULL FROM delivery_runs WHERE repo = 'acme/widgets' AND run_id = 691"
      )
    ).toBe("t");
  } finally {
    await context.close();
  }
});

test("a reconcile-style re-write sends no frame", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const live = await openLiveDelivery(page);
    const snapshot = await waitForFrame(live.frames, (frame) => frame.type === "snapshot");
    const sub = snapshotSubscription(snapshot);
    await page.waitForTimeout(100);
    const before = live.frames.length;
    await sql(
      `UPDATE delivery_pull_requests SET updated_at = now() WHERE repo = 'acme/widgets' AND number = 1`
    );
    await page.waitForTimeout(5_000);
    const data = live.frames
      .slice(before)
      .filter(
        (frame) => (frame.type === "snapshot" || frame.type === "delta") && frame.sub === sub
      );
    expect(data).toEqual([]);
  } finally {
    await context.close();
  }
});

test("each delta carries only what changed", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const live = await openLiveDelivery(page);
    const snapshot = await waitForFrame(live.frames, (frame) => frame.type === "snapshot");
    await insertMergedPR(692, "feat: one narrow delta");
    const delta = await waitForFrame(live.frames, (frame) =>
      deltaIncludesPR(frame, "acme/widgets#692")
    );
    if (!isLiveRecord(delta.items) || !isLiveRecord(delta.items.prs) || !isLiveRecord(delta.maps)) {
      throw new Error("Delivery delta omitted its item or map sections");
    }
    const upsert = delta.items.prs.upsert;
    const remove = delta.items.prs.remove;

    expect(upsert).toEqual([expect.objectContaining({ id: "acme/widgets#692" })]);
    expect(remove ?? []).toEqual([]);
    expect(Object.keys(delta.maps)).toEqual([]);
    for (const { family, facet, value, count } of deltaCounts(delta)) {
      expect(snapshotCount(snapshot, family, facet, value)).not.toBe(count);
    }
  } finally {
    await context.close();
  }
});

test("after every socket is dropped, the page resumes and equals the GET answer", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await openLiveDelivery(page);
    await insertMergedPR(693, "feat: before the reconnect");
    await expect(page.getByText("feat: before the reconnect", { exact: true })).toBeVisible();

    const disconnected = await context.request.post("/api/v1/delivery/_test/disconnect", {
      headers: { Authorization: "Bearer e2e-token" },
    });
    expect(disconnected.status()).toBe(200);
    await insertMergedPR(694, "feat: while the socket reconnects");
    await expect(
      page.getByText("feat: while the socket reconnects", { exact: true })
    ).toBeVisible();

    const body = await deliveryRequest(
      page,
      "/api/v1/delivery/timeline?from=" +
        encodeURIComponent(new Date(Date.now() - 28 * 24 * 60 * 60 * 1000).toISOString()) +
        "&to=" +
        encodeURIComponent(new Date().toISOString())
    );
    const rendered = await page
      .locator("tbody tr a[title]")
      .evaluateAll((links) => links.map((link) => link.getAttribute("title")));
    expect([...rendered].sort()).toEqual(deliveryPRIDs(body).sort());
  } finally {
    await context.close();
  }
});

test("an issue title change reaches the page", async ({ browser }) => {
  await seedDeliveryFixture();
  await sql(
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('ACME-690', 'ACME', 690, 'Initial live issue title', 'todo', 1, '{"kind":"system","id":"e2e"}', 'U690')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES (
       'acme/widgets', 695, 'feat: an issue title follows live', 'https://github.com/acme/widgets/pull/695', 'octocat',
       now() - interval '1 minute', now(), 3, 1, false, 'ACME-690', ARRAY[]::text[], false
     )`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const live = await openLiveDelivery(page);
    await expect(page.getByText("ACME-690 · P1", { exact: true })).toBeVisible();
    const response = await context.request.patch("/api/v1/issues/ACME-690", {
      data: { title: "Renamed live issue title" },
      headers: { Origin: new URL(page.url()).origin },
    });
    expect(response.status()).toBe(200);
    await waitForFrame(live.frames, (frame) =>
      deltaMapValue(frame, "issue_titles", "ACME-690", "Renamed live issue title")
    );
  } finally {
    await context.close();
  }
});

test("the Pipeline page updates live", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const live = watchDeliverySocket(page);
    await page.goto(`/pipeline?at=${PIPELINE_AT}`);
    await expect(page.getByRole("heading", { name: "Pipeline" })).toBeVisible();
    await live.opened;
    await waitForFrame(live.frames, (frame) => frame.type === "snapshot");
    const production = page.locator('[data-metric="production_apply"]');
    await expect(production).toContainText("1 run");
    await sql(
      `INSERT INTO delivery_runs (
         repo, run_id, kind, head_sha, head_commit_at, started_at, completed_at, conclusion, url, head_branch, event
       ) VALUES (
         'acme/widgets', 6907, 'deploy', 'pipeline6907', '2026-10-02T08:00:00Z', '2026-10-02T08:00:00Z', null, null,
         'https://github.com/acme/widgets/actions/runs/6907', 'main', 'push'
       )`,
      `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
       VALUES ('acme/widgets', 6907, 'widgets-release / widgets-release', '2026-10-02T08:00:00Z', '2026-10-02T09:00:00Z', 'success')`
    );
    await expect(production).toContainText("2 runs");
  } finally {
    await context.close();
  }
});

test("the freshness row says Live with the last change, and Reconnecting while dropped", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  await seedDeliveryReconcileGithub();
  await sql(
    `UPDATE delivery_settings SET last_event_at = now(), last_reconcile_at = now() WHERE singleton`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const transport = await deliveryTransport(page);
    await openLiveDelivery(page);
    const freshness = page.getByRole("region", { name: "Source freshness" });
    await expect(freshness.locator('[data-source="prs"]')).toContainText(/^PRs Live/);

    await transport.disconnect();
    await expect(freshness.locator('[data-source="prs"]')).toContainText("Reconnecting");
  } finally {
    await context.close();
  }
});

test("on a phone the delivery page shows a merge without a reload", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "webkit-iphone",
    "the WebKit iPhone project owns the phone path"
  );
  await seedDeliveryFixture();
  await seedDeliveryReconcileGithub();
  await sql(
    `UPDATE delivery_settings SET last_event_at = now(), last_reconcile_at = now() WHERE singleton`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
    await openDeliveryPage(page);
    const freshness = page
      .getByRole("region", { name: "Source freshness" })
      .locator('[data-source="prs"]');
    await expect(freshness).toContainText(/^PRs Live/);
    await insertMergedPR(696, "feat: a phone merge that arrived live");
    await expect(
      page.getByText("feat: a phone merge that arrived live", { exact: true })
    ).toBeVisible();
    await expect(freshness).toContainText(/^PRs Live/);
    await page.screenshot({
      path: screenshotPath(testInfo, "delivery-live-phone.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

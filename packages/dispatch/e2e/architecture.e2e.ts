import { createHash } from "node:crypto";
import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createIssue,
  createProject,
  getArchitecture,
  getIssue,
  patchIssue,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

async function expectTouchTarget(locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
}

/** A 90-character component title: on a phone it must truncate, never widen the row or the page. */
const longTitle =
  "Observability pipeline for ingesting, enriching, and retaining structured service telemetry";

/** The component model the fake GitHub serves: Platform contains Web and API, GitHub is
 *  external, Docs and the long-titled Observability have no tracked work. */
const model = {
  "api.md": "---\ntitle: API\nparent: platform\n---\nThe HTTP surface.\n",
  "docs.md": "---\ntitle: Docs\n---\nThe handbook.\n",
  "github.md": "---\ntitle: GitHub\nexternal: true\n---\nThe forge.\n",
  "observability.md": `---\ntitle: ${longTitle}\n---\nTraces and metrics.\n`,
  "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n",
  "web.md":
    "---\ntitle: Web\nparent: platform\ndepends_on: [api]\npaths: [packages/web]\n---\nThe **SPA**.\n",
};
const modelCommit = createHash("sha1").update(JSON.stringify(model)).digest("hex");

/** Seeds CORE with the model imported and work attached in every mode: two issues on Web (one
 *  in progress with a child inheriting Web, one done), one on API, one unassigned, one not
 *  architectural. */
async function seedArchitecture() {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({ "legion/arch": { contents: "read", files: model, installation_id: 101 } });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  const source = await syncArchitectureSource("CORE");
  expect(source.last_error).toBeNull();

  const login = await createIssue({ project: "CORE", title: "Web login" });
  const shipped = await createIssue({ project: "CORE", title: "Web shipped" });
  const child = await createIssue({ parent: login.key, project: "CORE", title: "Login child" });
  const contract = await createIssue({ project: "CORE", title: "API contract" });
  // A long title, so the phone assertion on its Unassigned row link is not vacuous.
  const loose = await createIssue({
    project: "CORE",
    title: "Loose end: consolidate every remaining flag into the shared runtime configuration",
  });
  const hire = await createIssue({ project: "CORE", title: "Hire someone" });
  await patchIssue(login.key, { components: { ids: ["web"], mode: "explicit" } });
  await patchIssue(login.key, { status: "in_progress" });
  await patchIssue(shipped.key, { components: { ids: ["web"], mode: "explicit" } });
  await patchIssue(shipped.key, { status: "done" });
  await patchIssue(contract.key, { components: { ids: ["api"], mode: "explicit" } });
  await patchIssue(hire.key, { components: { mode: "none", reason: "hiring, not code" } });
  return { child, contract, hire, login, loose, shipped };
}

function row(page: Page, title: string): Locator {
  return page.getByRole("article", { name: new RegExp(`^${title},`) });
}

async function screenshot(page: Page, projectName: string, name: string): Promise<void> {
  const dir = process.env.DISPATCH_ARCHITECTURE_SCREENSHOT_DIR;
  if (dir === undefined) {
    return;
  }
  const width = projectName === "iphone" ? "390" : "1280";
  await page.screenshot({ fullPage: true, path: `${dir}/${name}-${width}.png` });
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a project with a source opens on its Architecture: bars, drill-down, details, a bar that moves on a status change, and the Unassigned picker", async ({
  browser,
}, testInfo) => {
  const issues = await seedArchitecture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // Spec item 1: the project opens at /architecture (the URL changes; Back stays sane).
    await page.goto("/projects/CORE");
    await expect(page).toHaveURL(/\/projects\/CORE\/architecture$/);
    await expect(page.getByRole("tab", { name: "Architecture" })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    const source = page.getByTestId("architecture-source");
    await expect(source).toContainText("legion/arch/main");
    await expect(source).toContainText(`at ${modelCommit.slice(0, 8)}`);
    await expect(source).toContainText("checked");
    const scope = page.getByTestId("architecture-scope");
    await expect(scope).toContainText("1/6 issues done");
    await expect(scope).toContainText("2 components with no tracked work");
    await expect(scope).toContainText("1 unassigned");
    await expect(scope).toContainText("1 not architectural");

    // Root rows: Platform carries the fill bar and the count, the external GitHub row has no
    // bar, Docs has a dashed track and no count.
    const platform = row(page, "Platform");
    await expect(platform).toContainText("1/4 issues done");
    await expect(platform.getByTestId("progress-fill")).toHaveCSS("width", /^\d+(\.\d+)?px$/);
    const github = row(page, "GitHub");
    await expect(github).toContainText("external");
    await expect(github).not.toContainText("issues done");
    await expect(github.locator("[data-tone]")).toHaveCount(0);
    const docs = row(page, "Docs");
    await expect(docs).toContainText("No tracked work");
    await expect(docs.locator('[data-tone="no-work"]')).toHaveClass(/border-dashed/);
    const chevron = page.getByRole("link", { name: "Open the components inside Platform" });
    if (testInfo.project.name === "iphone") {
      await expectTouchTarget(chevron);
      // A long title truncates inside its row: neither the link nor the page scrolls sideways
      // (phone styles.css makes every `a` inline-flex, which would shrink-wrap a `block` link).
      const longLink = row(page, longTitle).getByRole("link", { exact: true, name: longTitle });
      await expect
        .poll(() => longLink.evaluate((node) => node.scrollWidth <= node.clientWidth))
        .toBe(true);
      expect(
        await page.evaluate(
          () => (document.scrollingElement?.scrollWidth ?? 0) === window.innerWidth
        )
      ).toBe(true);
    }

    // Item 8: the chevron descends; the breadcrumb leads back.
    await chevron.click();
    await expect(page).toHaveURL(/\?component=platform$/);
    await expect(page.getByRole("navigation", { name: "Component level" })).toHaveText(
      /All components\s*\/\s*Platform/
    );
    await expect(row(page, "Web")).toContainText("1/3 issues done");
    await expect(row(page, "API")).toContainText("0/1 issues done");
    await expect(row(page, "Platform")).toHaveCount(0);

    // Item 7: a row opens its details below the tree — Work, then Code & definition.
    await row(page, "Web").getByRole("link", { exact: true, name: "Web" }).click();
    await expect(page).toHaveURL(/\?component=web$/);
    const details = page.getByTestId("component-details");
    await expect(details.getByRole("heading", { level: 2, name: "Web" })).toBeVisible();
    const openWork = details.getByRole("list", { name: "Open work on Web" });
    // Unfinished work by lifecycle status (the triage child before its in-progress parent).
    await expect(openWork.getByRole("listitem")).toHaveText([/Login child/, /Web login/]);
    await expect(page.getByTestId(`work-${issues.login.key}`)).toContainText("In progress");
    await expect(page.getByTestId(`work-${issues.child.key}`)).toContainText("inherited");
    if (testInfo.project.name === "iphone") {
      // A work row's title is readable on a phone: it takes its own line under the chevron,
      // so the status pill and timestamp wrap beneath it instead of squeezing it to "CORE-1 · We".
      const workLink = page
        .getByTestId(`work-${issues.login.key}`)
        .getByRole("link", { name: `${issues.login.key} · Web login` });
      const [linkBox, rowBox] = await Promise.all([
        workLink.boundingBox(),
        page.getByTestId(`work-${issues.login.key}`).boundingBox(),
      ]);
      expect(linkBox).not.toBeNull();
      expect(rowBox).not.toBeNull();
      expect((linkBox?.width ?? 0) / (rowBox?.width ?? 1)).toBeGreaterThan(0.6);
      await expect
        .poll(() => workLink.evaluate((node) => node.scrollWidth <= node.clientWidth + 1))
        .toBe(true);
    }
    await expect(page.getByTestId(`work-${issues.shipped.key}`)).toHaveCount(0);
    await details.getByRole("button", { name: "Show 1 done" }).click();
    await expect(page.getByTestId(`work-${issues.shipped.key}`)).toContainText("Web shipped");
    await expect(details.locator("strong", { hasText: "SPA" })).toBeVisible();
    await expect(details.getByText("packages/web")).toBeVisible();
    await expect(details.getByRole("link", { exact: true, name: "API" })).toHaveAttribute(
      "href",
      "/projects/CORE/architecture?component=api"
    );
    await screenshot(page, testInfo.project.name, "architecture-tree");

    // Item 9: a work row's chevron expands the issue's children in place.
    await page.getByRole("button", { name: `Expand children of ${issues.login.key}` }).click();
    await expect(
      page.getByRole("list", { name: `Children of ${issues.login.key}` }).getByRole("listitem")
    ).toHaveText([/Login child/]);

    // Item 11: closing an issue through the API moves the bar without a reload (SSE).
    await patchIssue(issues.contract.key, { status: "done" });
    await expect(row(page, "API")).toContainText("1/1 issues done");
    await expect(scope).toContainText("2/6 issues done");

    // Item 12: the Unassigned list; the picker attaches the issue and the count moves.
    await scope.getByRole("button", { name: "1 unassigned" }).click();
    await expect(page).toHaveURL(/\?component=web&view=unassigned$/);
    const unassigned = page.getByTestId("unassigned-list");
    const looseLink = unassigned.getByRole("link", {
      name: `${issues.loose.key} · Loose end: consolidate every remaining flag into the shared runtime configuration`,
    });
    await expect(looseLink).toBeVisible();
    if (testInfo.project.name === "iphone") {
      // The head takes its own line on a phone and the long title ellipsizes inside its link
      // (the controls stack under it): the link itself never overflows.
      await expect
        .poll(() => looseLink.evaluate((node) => node.scrollWidth <= node.clientWidth))
        .toBe(true);
    }
    const attach = unassigned.getByRole("button", {
      name: `Attach ${issues.loose.key} to components`,
    });
    await attach.click();
    const search = page.getByRole("combobox", { name: "Search components" });
    await expect(page.getByRole("option", { name: "web · Web" })).toBeVisible();
    // External components are never offered: the server would refuse them.
    await expect(page.getByRole("option", { name: "github · GitHub" })).toHaveCount(0);
    if (testInfo.project.name === "iphone") {
      await expectTouchTarget(attach);
      await expectTouchTarget(search);
    }
    await screenshot(page, testInfo.project.name, "architecture-unassigned-picker");
    await page.getByRole("option", { name: "web · Web" }).click();
    await search.press("Escape");
    await expect
      .poll(() => getIssue(issues.loose.key).then((issue) => issue.components))
      .toMatchObject({ ids: ["web"], mode: "explicit" });
    await expect(scope).toContainText("0 unassigned");
    await expect(row(page, "Web")).toContainText("1/4 issues done");
    await expect(
      unassigned.getByText("Every issue is attached to a component or declared not architectural.")
    ).toBeVisible();

    // The Not architectural list names the reason; Reconsider puts the issue back in play.
    await scope.getByRole("button", { name: "1 not architectural" }).click();
    const none = page.getByTestId("not-architectural-list");
    await expect(none).toContainText("hiring, not code");
    await none.getByRole("button", { name: "Reconsider" }).click();
    await expect
      .poll(() => getIssue(issues.hire.key).then((issue) => issue.components.mode))
      .toBe("inherit");
    await expect(scope).toContainText("1 unassigned");
    await expect(scope).toContainText("0 not architectural");
  } finally {
    await context.close();
  }
});

test("Refresh with a broken model keeps the last model up behind an error banner; /issues keeps its filter; a project without a source opens on Issues", async ({
  browser,
}) => {
  await seedArchitecture();
  await createProject({ key: "OPS", name: "Ops" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/projects/CORE/architecture");
    await expect(row(page, "Platform")).toBeVisible();

    // Api.md and api.md are both component "api": the push is rejected whole and the tree
    // keeps the model it had, its commit named in the banner.
    await seedFakeGithub({
      "legion/arch": {
        contents: "read",
        files: { ...model, "Api.md": "Duplicate casing.\n" },
        installation_id: 101,
      },
    });
    const synced = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-source/sync") &&
        response.ok()
    );
    await page.getByTestId("architecture-source").getByRole("button", { name: "Refresh" }).click();
    await synced;
    const banner = page.getByRole("alert");
    await expect(banner).toContainText("Last import failed:");
    await expect(banner).toContainText("duplicate component id");
    await expect(banner).toContainText(`showing the model from ${modelCommit.slice(0, 8)}`);
    await expect(row(page, "Platform")).toContainText("1/4 issues done");
    await expect(row(page, "Platform")).toHaveClass(/border-dashed/);
    await expect(page.getByTestId("architecture-source")).toContainText(
      `at ${modelCommit.slice(0, 8)}`
    );
    const tree = await getArchitecture("CORE");
    expect(tree.source.last_commit).toBe(modelCommit);
    expect(tree.components.map((component) => component.id).sort()).toEqual([
      "api",
      "docs",
      "github",
      "observability",
      "platform",
      "web",
    ]);

    // The Issues tab lives at /issues now and keeps its filter parameters.
    await page.goto("/projects/CORE/issues?label=frontend");
    await expect(page).toHaveURL(/\/projects\/CORE\/issues\?label=frontend$/);
    await expect(page.getByRole("tab", { name: "Issues" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("button", { name: "Filters · 1 active" })).toBeVisible();

    // A project without a source has no Architecture tab and opens on Issues, the filter
    // parameters forwarded.
    await page.goto("/projects/OPS?label=frontend");
    await expect(page).toHaveURL(/\/projects\/OPS\/issues\?label=frontend$/);
    await expect(page.getByRole("tab", { name: "Architecture" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Filters · 1 active" })).toBeVisible();
  } finally {
    await context.close();
  }
});

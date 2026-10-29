import { createHash } from "node:crypto";
import { expect, type Locator, type Page, test } from "@playwright/test";

import { createIssue, createProject, getIssue } from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A project imports its component model from two repositories, each its own source, and the
// model is both sources' components together. This file uses only helpers, markup and
// fake-GitHub routes that exist before multi-source support, so the same file run against a
// build with one source per project fails at the two-row Settings assertion (the second save
// replaces the first), not at an import, seed or selector.

/** legion/arch: Platform, holding Web. */
const legionFiles = {
  "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n",
  "web.md": "---\ntitle: Web\nparent: platform\n---\nThe SPA.\n",
};
/** acme/infra: Cluster, holding Ingress, and DNS. No id repeats legion/arch's. */
const infraFiles = {
  "infra-cluster.md": "---\ntitle: Cluster\n---\nWhere the services run.\n",
  "infra-dns.md": "---\ntitle: DNS\n---\nThe zones.\n",
  "infra-ingress.md": "---\ntitle: Ingress\nparent: infra-cluster\n---\nThe edge.\n",
};

/** The fake GitHub's head commit for a repository is sha1 over its seeded files' JSON. */
function commitOf(files: Record<string, string>): string {
  return createHash("sha1").update(JSON.stringify(files)).digest("hex");
}

const legionCommit = commitOf(legionFiles);
const infraCommit = commitOf(infraFiles);

/** A component's row on the architecture page, by its title. */
function componentRow(page: Page, title: string): Locator {
  return page.getByRole("article", { name: new RegExp(`^${title},`) });
}

/** Saves `repo` on `main` as a CORE source through the Settings form. The response predicate
 *  matches the one-source route and the per-repository one alike. */
async function addSource(page: Page, repo: string): Promise<void> {
  await page.getByLabel("Source project").selectOption("CORE");
  await page.getByLabel("Source repository").fill(repo);
  await page.locator("#architecture-source-branch").fill("main");
  const saved = page.waitForResponse(
    (response) =>
      response.request().method() === "PUT" &&
      new URL(response.url()).pathname.startsWith("/api/v1/projects/CORE/architecture-source") &&
      response.ok()
  );
  await page.getByRole("button", { name: "Add source" }).click();
  await saved;
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a project with two architecture sources shows both in Settings, on its Architecture page and in an issue's Components picker", async ({
  browser,
}) => {
  // Step 1: project CORE and two repositories, each with its own component files.
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Route the SPA through the edge" });
  await seedFakeGithub({
    "acme/infra": { contents: "read", files: infraFiles, installation_id: 102 },
    "legion/arch": { contents: "read", files: legionFiles, installation_id: 101 },
  });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // Step 2: Settings adds legion/arch, then acme/infra, to CORE; both stay.
    await page.goto("/settings");
    const section = page.getByRole("region", { name: "Architecture sources" });
    await expect(section).toBeVisible();
    const coreRows = section
      .getByRole("row")
      .filter({ has: page.getByRole("cell", { exact: true, name: "CORE" }) });
    const sourceRow = (repo: string) =>
      coreRows.filter({ has: page.getByRole("cell", { exact: true, name: repo }) });

    await addSource(page, "legion/arch");
    await expect(sourceRow("legion/arch")).toBeVisible();
    await addSource(page, "acme/infra");
    await expect(sourceRow("acme/infra")).toBeVisible();
    // A second repository is a second source, never a replacement of the first.
    await expect(sourceRow("legion/arch"), "legion/arch is still a CORE source").toBeVisible();
    await expect(coreRows, "CORE has one row per repository").toHaveCount(2);

    for (const repo of ["legion/arch", "acme/infra"]) {
      const synced = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          response.url().endsWith(`/api/v1/projects/CORE/architecture-sources/${repo}/sync`) &&
          response.ok()
      );
      await sourceRow(repo)
        .getByRole("button", { name: `Refresh architecture source ${repo} for CORE` })
        .click();
      await synced;
    }
    // Each row names its own repository's head and how many components it supplies.
    await expect(sourceRow("legion/arch")).toContainText(
      `Synced ${legionCommit.slice(0, 12)} · 2 components`
    );
    await expect(sourceRow("acme/infra")).toContainText(
      `Synced ${infraCommit.slice(0, 12)} · 3 components`
    );

    // Step 3: the project opens on its Architecture, whose top level holds each source's roots.
    await page.goto("/projects/CORE");
    await expect(page).toHaveURL(/\/projects\/CORE\/architecture$/);
    await expect(componentRow(page, "Platform")).toBeVisible();
    await expect(componentRow(page, "Cluster")).toBeVisible();
    await expect(componentRow(page, "DNS")).toBeVisible();
    await expect(componentRow(page, "Web")).toHaveCount(0);
    await expect(componentRow(page, "Ingress")).toHaveCount(0);

    // Step 4: one source line per repository, each at its own commit.
    const sources = page.getByTestId("architecture-source");
    await expect(sources).toHaveCount(2);
    await expect(sources.nth(0)).toContainText("legion/arch/main");
    await expect(sources.nth(0)).toContainText(`at ${legionCommit.slice(0, 8)}`);
    await expect(sources.nth(1)).toContainText("acme/infra/main");
    await expect(sources.nth(1)).toContainText(`at ${infraCommit.slice(0, 8)}`);

    // A component's details name the repository whose files define it. DNS has no children, so
    // selecting it keeps the top level, where Platform's row still is.
    const details = page.getByTestId("component-details");
    const repository = details.locator('dt:text-is("Repository") + dd');
    await componentRow(page, "DNS").getByRole("link", { exact: true, name: "DNS" }).click();
    await expect(page).toHaveURL(/\?component=infra-dns$/);
    await expect(details.getByRole("heading", { level: 2, name: "DNS" })).toBeVisible();
    await expect(repository).toHaveText("acme/infra");
    await componentRow(page, "Platform")
      .getByRole("link", { exact: true, name: "Platform" })
      .click();
    await expect(page).toHaveURL(/\?component=platform$/);
    await expect(details.getByRole("heading", { level: 2, name: "Platform" })).toBeVisible();
    await expect(repository).toHaveText("legion/arch");

    // Step 5: an issue's Components picker offers both sources' components; one of each saves.
    await page.goto(`/issues/${issue.key}`);
    const line = page.getByTestId("issue-components");
    await expect(line).toContainText("Not attached");
    await line.getByRole("button", { name: `Edit components of ${issue.key}` }).click();
    const search = page.getByRole("combobox", { name: "Search components" });
    await expect(page.getByRole("option", { name: "infra-ingress · Ingress" })).toBeVisible();
    await page.getByRole("option", { name: "web · Web" }).click();
    await page.getByRole("option", { name: "infra-ingress · Ingress" }).click();
    await search.press("Escape");
    await expect
      .poll(() =>
        getIssue(issue.key).then(({ components }) => ({
          ids: [...components.ids].sort(),
          mode: components.mode,
        }))
      )
      .toEqual({ ids: ["infra-ingress", "web"], mode: "explicit" });
    const chips = [
      line.getByText("web", { exact: true }),
      line.getByText("infra-ingress", { exact: true }),
    ];
    for (const chip of chips) {
      await expect(chip).toBeVisible();
    }

    // Both stay attached across a reload.
    await page.reload();
    await expect(line).not.toContainText("Not attached");
    for (const chip of chips) {
      await expect(chip).toBeVisible();
    }
  } finally {
    await context.close();
  }
});

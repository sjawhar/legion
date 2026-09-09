import { expect, test } from "@playwright/test";

import {
  createAsk,
  createIssue,
  createProject,
  getAsk,
  getIssue,
  getIssueEvents,
  patchIssue,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-session", origin: { tmux: "dispatch:1.2" } },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("inbox answers asks inline and keeps issue state per user", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const firstIssue = await createIssue({ project: "CORE", title: "First decision" });
  const secondIssue = await createIssue({ project: "CORE", title: "Second decision" });
  await createAsk(
    firstIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "First ask" },
    session
  );
  const childIssue = await createIssue({
    parent: firstIssue.key,
    project: "CORE",
    title: "Child decision",
  });
  await patchIssue(firstIssue.key, {
    external_links: [{ kind: "github_issue", url: "https://github.com/sjawhar/legion/issues/815" }],
  });
  await createAsk(
    secondIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Second ask" },
    session
  );
  const newestAsk = await createAsk(
    firstIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Newest ask" },
    session
  );
  await expect
    .poll(async () =>
      (await getIssueEvents(firstIssue.key)).some((event) => event.actor.id === "e2e-session")
    )
    .toBe(true);

  const alice = await asUser(browser, "alice");
  const alicePage = await alice.newPage();
  await alicePage.goto("/");
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }

  const inboxCards = alicePage.locator("[data-testid^=ask-]");
  await expect(inboxCards).toHaveCount(3);
  await expect(inboxCards.nth(0)).toContainText("Newest ask");
  await alicePage.screenshot({ path: testInfo.outputPath("inbox-three-asks.png"), fullPage: true });
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }

  await newestAsk;
  await alicePage.getByTestId(`ask-${newestAsk.id}`).getByRole("radio", { name: "Ship" }).check();
  await alicePage
    .getByTestId(`ask-${newestAsk.id}`)
    .getByRole("button", { name: "Submit answer" })
    .click();
  await expect(inboxCards).toHaveCount(2);
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(alicePage.getByRole("heading", { name: "Needs you (2)" })).toBeVisible();
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }
  await expect
    .poll(() => getAsk(newestAsk.id))
    .toMatchObject({
      answer: { selected: ["Ship"], user: "alice" },
      state: "answered",
    });

  await alicePage.goto(`/issues/${firstIssue.key}`);
  await expect(alicePage.getByRole("region", { name: "Active sessions" })).toContainText(
    "e2e-session"
  );
  const issueTitle = alicePage.getByLabel("Issue title");
  await issueTitle.fill("First decision revised");
  await issueTitle.press("Enter");
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({ title: "First decision revised" });
  await alicePage.getByLabel("Status").selectOption("testing");
  await expect.poll(() => getIssue(firstIssue.key)).toMatchObject({ status: "testing" });
  await alicePage.getByLabel("Route").fill("role:legion-controller-core");
  await alicePage.getByRole("button", { name: "Save route" }).click();
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({
      route: "role:legion-controller-core",
    });
  await alicePage.getByLabel("Route").fill("");
  await alicePage.getByRole("button", { name: "Save route" }).click();
  await expect.poll(() => getIssue(firstIssue.key)).toMatchObject({ route: null });
  await expect(
    alicePage.getByRole("link", { name: "https://github.com/sjawhar/legion/issues/815" })
  ).toHaveAttribute("title", "GitHub details are unavailable for this sign-in.");
  await alicePage.getByRole("tab", { name: "Children" }).click();
  await expect(
    alicePage
      .getByRole("tabpanel")
      .getByRole("link", { name: `${childIssue.key} · Child decision` })
  ).toBeVisible();
  await alicePage.getByRole("tab", { name: "Log" }).click();
  await expect(alicePage.getByText("Ask answered: Newest ask")).toBeVisible();
  await alicePage.screenshot({ path: testInfo.outputPath("issue-page.png"), fullPage: true });
  await alicePage.getByRole("button", { name: "Pin issue" }).click();
  await alicePage.goto("/");
  await alicePage.goto(`/issues/${firstIssue.key}`);
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(alicePage.getByRole("heading", { name: "Pinned" })).toBeVisible();
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }
  await expect(alicePage.getByRole("button", { name: "Unpin issue" })).toBeVisible();
  const bob = await asUser(browser, "bob");
  const bobPage = await bob.newPage();
  await bobPage.goto("/");
  await expect(bobPage.locator("[data-testid^=ask-]")).toHaveCount(2);
  await expect(bobPage.getByRole("heading", { name: "Pinned" })).toHaveCount(0);

  await bob.close();
  await alice.close();
});

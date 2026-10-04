import { expect, test } from "@playwright/test";
import { type FakeSession, openAgents, setLiveSessions, shownAgentRows } from "./agents";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Three sessions with no field in common, so a match on one field never comes from another: the
// title, directory and machine each carry a word the other two sessions lack.
const scrum: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/scrum-bot",
  machine_id: "build-host",
  roles: ["planner"],
  session_id: "scrum-session",
  title: "Scrum planning",
};
const release: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/release-writer",
  machine_id: "release-host",
  roles: ["writer"],
  session_id: "release-session",
  title: "Release notes",
};
const archivist: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/archive",
  machine_id: "archive-host",
  roles: [],
  session_id: "archivist-session",
  title: "Archivist",
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** All three sessions live, each with Dispatch activity of its own so none folds under `No
 *  Dispatch activity`: the Scrum session's open ask waits on the viewer, which lists it first, and
 *  the Release session comments last, which lists it above the Archivist. Returns the issue key the
 *  Scrum session's ask names - the only issue key any session's ask names. */
async function seed(): Promise<string> {
  await setLiveSessions([scrum, release, archivist]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Sprint planning" });
  await createAsk(
    issue.key,
    { question: "Which sprint?" },
    { actor: { id: scrum.session_id, kind: "session" }, as: "agent" }
  );
  for (const session of [archivist, release]) {
    await createComment(
      issue.key,
      { body: `${session.title} checking in.` },
      { actor: { id: session.session_id, kind: "session" }, as: "agent" }
    );
  }
  return issue.key;
}

test("the search box narrows by a word in the title, directory, machine, session id or issue key, every typed word required wherever it appears, and clears back to every session", async ({
  browser,
}, testInfo) => {
  const issueKey = await seed();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openAgents(page);
    const width = testInfo.project.name === "iphone" ? "390" : "1280";
    const search = page.getByRole("searchbox", { name: "Search agents" });
    const titles = () => shownAgentRows(page).locator("h2");
    await expect(titles()).toHaveText(["Scrum planning", "Release notes", "Archivist"]);

    // A title fragment, case-insensitive.
    await search.fill("SCRUM");
    await expect(titles()).toHaveText(["Scrum planning"]);

    // A directory fragment none of the titles share.
    await search.fill("archive");
    await expect(titles()).toHaveText(["Archivist"]);

    // A machine name.
    await search.fill("release-host");
    await expect(titles()).toHaveText(["Release notes"]);

    // The session id itself.
    await search.fill("archivist-session");
    await expect(titles()).toHaveText(["Archivist"]);

    // An issue key: the one session with an open ask on it.
    await search.fill(issueKey);
    await expect(titles()).toHaveText(["Scrum planning"]);

    // Two words found in two different fields of the one session - its title and the issue key
    // one of its asks names - still match, wherever each word appears; the same two words
    // exclude a session missing either.
    await search.fill(`scrum ${issueKey}`);
    await expect(titles()).toHaveText(["Scrum planning"]);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-search-filtered-${width}.png`),
    });
    await search.fill(`release ${issueKey}`);
    await expect(titles()).toHaveText([]);
    await expect(page.getByText("No agent matches these filters.")).toBeVisible();

    // Clearing shows every session again.
    await search.fill("");
    await expect(titles()).toHaveText(["Scrum planning", "Release notes", "Archivist"]);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-search-cleared-${width}.png`),
    });
  } finally {
    await alice.close();
  }
});

test("the query survives a reload through the page's address, and Control+K focuses the box instead of opening the Dispatch-wide search dialog", async ({
  browser,
}) => {
  await seed();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openAgents(page);
    const search = page.getByRole("searchbox", { name: "Search agents" });
    await search.fill("scrum");
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Scrum planning"]);
    expect(new URL(page.url()).searchParams.get("q")).toBe("scrum");

    await page.reload();
    await expect(page.getByRole("searchbox", { name: "Search agents" })).toHaveValue("scrum");
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Scrum planning"]);

    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await expect(page.getByRole("searchbox", { name: "Search agents" })).toBeFocused();
    await expect(page.getByRole("dialog", { name: "Search" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

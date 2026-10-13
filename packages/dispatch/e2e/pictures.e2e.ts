import { expect, type Locator, type Page, type Response, test } from "@playwright/test";

import { agentRow, setLiveSessions } from "./agents";
import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  getIssueArtifactText,
} from "./api";
import { documentEditor, openSpec, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { pastePicture } from "./sends";
import { asUser } from "./users";

// A picture pasted into a message, an ask's answer or a direct message is uploaded and written as
// Markdown's image syntax at its version (`![shot.png](dispatch://…/artifact/shot-png@v1)`), and
// every place that renders the text shows it: an `<img>` loaded from the version's same-origin
// bytes route, linked to the artifact's page. A document holding that line - a spec an agent wrote
// the picture into - draws it the same way in its editor and in a past version.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-pictures" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** Waits for `picture` to have loaded real pixels: scrolled to first, since it loads lazily. */
async function expectDrawn(picture: Locator, src: string): Promise<void> {
  await expect(picture).toBeVisible();
  await expect(picture).toHaveAttribute("src", src);
  await expect(picture).toHaveAttribute("loading", "lazy");
  await picture.scrollIntoViewIfNeeded();
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
    .toBeGreaterThan(0);
}

/** The server's answer to the next `POST` to `path`; a row checks it is a success with
 *  `expectSucceeded`, so a refusal fails naming its status rather than timing out. */
function posted(page: Page, path: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "POST" && new URL(response.url()).pathname === path
  );
}

async function expectSucceeded(answer: Promise<Response>): Promise<Response> {
  const response = await answer;
  expect(response.status(), `POST ${new URL(response.url()).pathname}`).toBeLessThan(300);
  return response;
}

test("a picture pasted into an issue message shows inline in the conversation", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pictures in messages" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    const field = composer.getByRole("textbox", { name: "Comment" });
    await field.fill("The broken layout:");

    const uploaded = posted(page, `/api/v1/issues/${issue.key}/artifacts`);
    await pastePicture(field, "shot.png");
    await expectSucceeded(uploaded);
    const syntax = `![shot.png](dispatch://${issue.key}/artifact/shot-png@v1)`;
    await expect(field).toHaveValue(`The broken layout: ${syntax}`);

    const sent = posted(page, `/api/v1/issues/${issue.key}/comments`);
    await field.press("Control+Enter");
    expect((await expectSucceeded(sent)).request().postDataJSON()).toMatchObject({
      body: `The broken layout: ${syntax}`,
    });

    // A comment turn opens collapsed to its one-line preview, where the picture is a thumbnail
    // beside its caption, inside the link to its page.
    const link = page
      .locator("#issue-conversation-panel")
      .getByRole("link", { name: "shot.png" })
      .filter({ has: page.locator("img") });
    await expect(link).toHaveAttribute("href", `/issues/${issue.key}/artifacts/shot-png?v=1`);
    await expectDrawn(
      link.locator("img"),
      `/api/v1/issues/${issue.key}/artifacts/shot-png/versions/1`
    );

    // The picture opens its own page, which shows it and its versions.
    await link.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/shot-png?v=1`);
    await expect(page.getByRole("heading", { name: "shot.png" })).toBeVisible();
    await expect(page.getByRole("img", { name: "shot.png version 1" })).toBeVisible();
    await expect(page.getByRole("region", { name: "Versions for shot.png" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Download version 1" }).first()).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a picture pasted into an ask's answer shows inline in the answer", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pictures in answers" });
  const ask = await createAsk(
    issue.key,
    { question: "Which screen is wrong?" },
    { actor: { id: "planner-session", kind: "session" }, as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const answer = card.getByLabel("Your answer");

    const uploaded = posted(page, `/api/v1/issues/${issue.key}/artifacts`);
    await pastePicture(answer, "screen.png");
    await expectSucceeded(uploaded);
    const syntax = `![screen.png](dispatch://${issue.key}/artifact/screen-png@v1)`;
    await expect(answer).toHaveValue(syntax);

    const answered = posted(page, `/api/v1/asks/${ask.id}/answer`);
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    expect((await expectSucceeded(answered)).request().postDataJSON()).toMatchObject({
      text: syntax,
    });

    // The answer is the ask's record: the issue's conversation shows it with the picture.
    await page.goto(`/issues/${issue.key}/conversation`);
    const picture = page
      .locator("#issue-conversation-panel")
      .getByRole("img", { name: "screen.png" });
    await expectDrawn(picture, `/api/v1/issues/${issue.key}/artifacts/screen-png/versions/1`);
  } finally {
    await alice.close();
  }
});

test("a picture pasted into a direct message on the Agents page shows inline and opens its page", async ({
  browser,
}) => {
  const planner = {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    machine_id: "planner-host",
    roles: ["planner"],
    session_id: "planner-session",
    title: "Planner",
  };
  await setLiveSessions([planner]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    // A session with no Dispatch activity yet is listed under its fold.
    await page
      .getByRole("region", { name: "Agents" })
      .getByRole("button", { name: "No Dispatch activity (1)" })
      .click();
    const row = agentRow(page, planner.session_id);
    await row.getByRole("button", { exact: true, name: "Planner" }).click();
    const field = row.getByRole("textbox", { name: "Comment" });

    const uploaded = posted(page, `/api/v1/agents/${planner.session_id}/artifacts`);
    await pastePicture(field, "shot.png");
    await expectSucceeded(uploaded);
    const syntax = `![shot.png](dispatch://agent/${planner.session_id}/artifact/shot-png@v1)`;
    await expect(field).toHaveValue(syntax);

    const sent = posted(page, `/api/v1/agents/${planner.session_id}/messages`);
    await field.press("Control+Enter");
    expect((await expectSucceeded(sent)).request().postDataJSON()).toMatchObject({ body: syntax });

    const conversation = row.getByRole("list", { name: "Conversation with Planner" });
    const picture = conversation.getByRole("img", { name: "shot.png" });
    await expectDrawn(
      picture,
      `/api/v1/agents/${planner.session_id}/artifacts/shot-png/versions/1`
    );

    // The picture opens its own page, which shows it and its versions.
    await picture.click();
    await expect(page).toHaveURL(`/agents/${planner.session_id}/artifacts/shot-png?v=1`);
    await expect(page.getByRole("heading", { name: "shot.png" })).toBeVisible();
    await expect(page.getByRole("img", { name: "shot.png version 1" })).toBeVisible();
    await expect(page.getByRole("region", { name: "Versions for shot.png" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Download version 1" }).first()).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a picture written into a spec draws in the editor and in a past version, and its markdown is kept", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "Pictures in specs",
  });
  const syntax = `![shot.png](dispatch://${issue.key}/artifact/shot-png@v1)`;
  const src = `/api/v1/issues/${issue.key}/artifacts/shot-png/versions/1`;
  const alice = await asUser(browser, "alice");
  try {
    // The picture reaches the issue the way a person's does: pasted into a comment and sent, so
    // the page moves on with no unsent draft behind it.
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const field = page
      .getByRole("form", { name: "Comment composer" })
      .getByRole("textbox", { name: "Comment" });
    const uploaded = posted(page, `/api/v1/issues/${issue.key}/artifacts`);
    await pastePicture(field, "shot.png");
    await expectSucceeded(uploaded);
    await expect(field).toHaveValue(syntax);
    const sent = posted(page, `/api/v1/issues/${issue.key}/comments`);
    await field.press("Control+Enter");
    await expectSucceeded(sent);

    // An agent writes the line into the spec, as `dispatch doc-edit` does: a new version, whose
    // number the server assigns. The paragraph after it is where the caret lands in a moment.
    await editArtifact(
      issue.primary_artifact_id,
      { ops: [{ after: "end", markdown: `${syntax}\n\nBelow the picture.`, op: "insert" }] },
      agentSession
    );
    const newestVersion = async () =>
      Math.max(
        ...(await getArtifact(issue.primary_artifact_id)).versions.map(({ number }) => number)
      );
    await expect.poll(newestVersion).toBeGreaterThan(1);
    const pictured = await newestVersion();

    await openSpec(page, issue.key, "Below the picture.");
    await expectDrawn(documentEditor(page).getByRole("img", { name: "shot.png" }), src);

    // Editing around the picture keeps its line as it was written.
    await typeAtEnd(page, "Typed in the editor.");
    await expect
      .poll(async () => (await getIssueArtifactText(issue.key, "spec")).markdown)
      .toContain("Typed in the editor.");
    expect((await getIssueArtifactText(issue.key, "spec")).markdown).toContain(syntax);

    // The past version draws it through the read-only view.
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(pictured));
    const versionView = page.getByTestId("version-view");
    await expectDrawn(versionView.getByRole("img", { name: "shot.png" }), src);
    await expect(versionView.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
      "contenteditable",
      "false"
    );
  } finally {
    await alice.close();
  }
});

test("a picture line the browser cannot draw, or that pins no version, shows a link; a web picture stays an <img>", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "Pictures that are links",
  });
  // A markdown document: its version's route answers JSON, which no `<img>` can draw.
  await createIssueArtifact(issue.key, { content: "# Notes\n", name: "notes.md" }, agentSession);
  const versioned = `dispatch://${issue.key}/artifact/notes-md@v1`;
  const unversioned = `dispatch://${issue.key}/artifact/notes-md`;
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown: `![notes.md](${versioned})\n\n![notes.md](${unversioned})\n\n![logo](https://example.invalid/logo.png)\n\nBelow the pictures.`,
          op: "insert",
        },
      ],
    },
    agentSession
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the pictures.");
    const editor = documentEditor(page);
    const linkName = `${issue.key} notes-md`;

    // The picture loads lazily, so it is scrolled to before its load can fail and give way to
    // the link.
    const versionedPicture = editor.locator(`[data-dispatch-doc-picture="${versioned}"]`);
    await versionedPicture.scrollIntoViewIfNeeded();
    const versionedLink = versionedPicture.getByRole("link", { name: linkName });
    await expect(versionedLink).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/notes-md?v=1`
    );
    await expect(versionedPicture.locator("img")).toHaveCount(0);

    const unversionedPicture = editor.locator(`[data-dispatch-doc-picture="${unversioned}"]`);
    await expect(unversionedPicture.getByRole("link", { name: linkName })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/notes-md`
    );
    await expect(unversionedPicture.locator("img")).toHaveCount(0);

    // A picture on another website is the schema's own `<img>`; its host does not exist, so only
    // the source is asserted.
    await expect(editor.getByRole("img", { name: "logo" })).toHaveAttribute(
      "src",
      "https://example.invalid/logo.png"
    );

    // A drag that starts on the link moves the picture node, as one on an `<img>` does, and the
    // link's own words never reach the document: dropped onto the last paragraph, the versioned
    // picture now follows the unversioned one, still written as it was, and once. The paragraph
    // is brought into view first, with the link still in it: `dragTo` scrolls its target into
    // view only as it moves there, after the press, and Chromium begins no drag when the page
    // scrolled between the press and the move (the desktop viewport puts the paragraph below the
    // fold; the phone's does not).
    const below = editor.getByText("Below the pictures.");
    await below.evaluate((paragraph) => paragraph.scrollIntoView({ block: "nearest" }));
    await expect(versionedLink).toBeInViewport();
    await versionedLink.dragTo(below);
    const versionedLine = `![notes.md](${versioned})`;
    const unversionedLine = `![notes.md](${unversioned})`;
    const markdown = async () => (await getIssueArtifactText(issue.key, "spec")).markdown;
    await expect
      .poll(async () => {
        const text = await markdown();
        return text.indexOf(versionedLine) > text.indexOf(unversionedLine);
      })
      .toBe(true);
    const moved = await markdown();
    // Split around the line: two parts is exactly one occurrence.
    expect(moved.split(versionedLine)).toHaveLength(2);
    expect(moved).not.toContain(`[${linkName}](`);

    // The link is a link: a click on it in the live editor - the moved node's, which the locator
    // resolves afresh - opens what it names.
    await versionedLink.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md?v=1`);
  } finally {
    await alice.close();
  }
});

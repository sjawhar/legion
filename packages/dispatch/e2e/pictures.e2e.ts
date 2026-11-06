import { expect, type Locator, type Page, type Response, test } from "@playwright/test";

import type { ArtifactUploadResponse } from "../web/src/api/types";
import { REF_PREVIEW_OPEN_DELAY_MS } from "../web/src/features/refs/ref-preview-timing";
import { agentRow, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifact,
  getIssueArtifactText,
} from "./api";
import { documentEditor, openSpec, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { ONE_PIXEL_PNG, pastePicture } from "./sends";
import { sessionIds } from "./session-ids";
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

/** Waits for the full-size view a click on a picture opened to draw that picture's real pixels,
 *  and answers the picture it drew. The view is PhotoSwipe's (`features/refs/picture-viewer.ts`)
 *  and its root carries no accessible name of its own, so the slide is found by the alt text the
 *  viewer gives it - the picture's caption - and what the reader sees is read from that element:
 *  the size it is drawn at, and the bytes behind it. A picture that fits the window is drawn at
 *  its own pixels, which is what a reader opened it for. */
async function expectFullSize(page: Page, caption: string, src: string): Promise<Locator> {
  const picture = page.getByRole("dialog").getByRole("img", { name: caption });
  await expect(picture).toBeVisible();
  // The viewer gives its slide the address the page drew from, as the browser resolved it, so
  // the route is read off that address rather than compared to the page's own relative one.
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => new URL(image.src).pathname))
    .toBe(src);
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
    .toBeGreaterThan(0);
  const drawn = await picture.evaluate((image: HTMLImageElement) => ({
    natural: image.naturalWidth,
    width: image.getBoundingClientRect().width,
  }));
  expect(Math.round(drawn.width), "the view draws the picture at its own pixels").toBe(
    drawn.natural
  );
  return picture;
}

/** Closes the full-size view the way a reader dismisses it: a click on the backdrop beside the
 *  picture. A click on the picture itself zooms it now (`tapAction: "zoom-or-close"`), so the
 *  point is taken from the picture's own box, clear of it and clear of the controls above it. */
async function clickBackdrop(page: Page, picture: Locator): Promise<void> {
  const box = await picture.boundingBox();
  expect(box, "the full-size picture has no box to aim beside").not.toBeNull();
  if (box === null) return;
  await page.mouse.click(box.x / 2, box.y + box.height / 2);
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
    { actor: { id: sessionIds.planner, kind: "session" }, as: "agent" }
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
    session_id: sessionIds.planner,
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

    // A click opens the picture at full size over the page, and a click on the backdrop beside
    // it closes it: a click on the picture itself zooms it now.
    await picture.click();
    const shown = await expectFullSize(
      page,
      "shot.png",
      `/api/v1/agents/${planner.session_id}/artifacts/shot-png/versions/1`
    );
    await clickBackdrop(page, shown);
    await expect(shown).toHaveCount(0);
    await expect(page).not.toHaveURL(/\/artifacts\//);

    // The picture's link still names its own page, which shows it and its versions.
    const pagePath = await picture.locator("xpath=ancestor::a[1]").getAttribute("href");
    expect(pagePath).toBe(`/agents/${planner.session_id}/artifacts/shot-png?v=1`);
    await page.goto(pagePath ?? "");
    await expect(page.getByRole("heading", { name: "shot.png" })).toBeVisible();
    await expect(page.getByRole("img", { name: "shot.png version 1" })).toBeVisible();
    await expect(page.getByRole("region", { name: "Versions for shot.png" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Download version 1" }).first()).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a picture written into a spec draws in the editor and in a past version, with or without a version, and its markdown is kept", async ({
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
    // number the server assigns. Beside it, the same picture written without `@vN`, which draws
    // its latest version. The paragraph after them is where the caret lands in a moment.
    const latestSyntax = `![latest shot](dispatch://${issue.key}/artifact/shot-png)`;
    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          {
            after: "end",
            markdown: `${syntax}\n\n${latestSyntax}\n\nBelow the picture.`,
            op: "insert",
          },
        ],
      },
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
    await expectDrawn(documentEditor(page).getByRole("img", { name: "latest shot" }), src);

    // A click on a picture in the editor opens it at full size; a click on the backdrop beside
    // it closes it.
    await documentEditor(page).getByRole("img", { name: "shot.png" }).click();
    const shown = await expectFullSize(page, "shot.png", src);
    await clickBackdrop(page, shown);
    await expect(shown).toHaveCount(0);

    // Editing around the picture keeps its line as it was written.
    await typeAtEnd(page, "Typed in the editor.");
    await expect
      .poll(async () => (await getIssueArtifactText(issue.key, "spec")).markdown)
      .toContain("Typed in the editor.");
    expect((await getIssueArtifactText(issue.key, "spec")).markdown).toContain(syntax);
    expect((await getIssueArtifactText(issue.key, "spec")).markdown).toContain(latestSyntax);

    // The past version draws it through the read-only view.
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(pictured));
    const versionView = page.getByTestId("version-view");
    await expectDrawn(versionView.getByRole("img", { name: "shot.png" }), src);
    await expectDrawn(versionView.getByRole("img", { name: "latest shot" }), src);
    await expect(versionView.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
      "contenteditable",
      "false"
    );
  } finally {
    await alice.close();
  }
});

test("a picture line the browser cannot draw, at a version or not, shows a link; a web picture stays an <img>", async ({
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
    // fold).
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

// A picture's source and a link's target are nodes their author named, so each is drawn and
// opened whatever the length of its name: a reference key past the 2048 bytes text cites
// (MAX_REFERENCE_TARGET_BYTES), yet within the 2692 a new upload's key holds, still loads from its
// version's bytes and opens its document's dashboard page. The same document's page URL or
// `dispatch://` reference left as text (bare, in angle brackets, or a link whose words are its
// target) is text, which the server cites only within those bytes, so it stays the plain link it is.
test("a long-named picture in a comment draws, its link to a long-named document opens it, and the bare page URL stays a plain link @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long names" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pictureName = `${"a".repeat(2600)}.png`;
    const upload = await page.request.post(`/api/v1/issues/${issue.key}/artifacts`, {
      headers: { Origin: dashboardOrigin },
      multipart: {
        file: {
          buffer: Buffer.from(ONE_PIXEL_PNG, "base64"),
          mimeType: "image/png",
          name: pictureName,
        },
        name: pictureName,
      },
    });
    expect(upload.status(), await upload.text()).toBeLessThan(300);
    const pictureSlug = ((await upload.json()) as ArtifactUploadResponse).artifact.slug;
    // Words, so the link's title - the document's name - wraps in the column.
    const design = await createIssueArtifact(issue.key, {
      content: "# Design\n\nThe long-named design.\n",
      name: `${"design ".repeat(372)}.md`,
    });
    const designSlug = design.artifact.slug;
    for (const slug of [pictureSlug, designSlug]) {
      const keyBytes = Buffer.byteLength(`${issue.key}/${slug}`);
      expect(keyBytes).toBeGreaterThan(2048);
      expect(keyBytes).toBeLessThanOrEqual(2692);
    }
    const designTarget = `dispatch://${issue.key}/artifact/${designSlug}`;
    const path = `/issues/${issue.key}/artifacts/${designSlug}`;
    const bareUrl = `${dashboardOrigin}${path}`;
    // A block of a short-named document whose id is past the 256 bytes a block reference names.
    const blockTarget = `dispatch://${issue.key}/spec#b-${"b".repeat(257)}`;
    const comment = await createComment(issue.key, {
      body: `![shot](dispatch://${issue.key}/artifact/${pictureSlug}@v1)\n\nSee [design](${designTarget}).\n\nBare: ${bareUrl}\n\nAngle: <${designTarget}>\n\nSelf: [${designTarget}](${designTarget})\n\nBlock: <${blockTarget}>`,
    });

    // The hover card's open delay below is a page timer (`REF_PREVIEW_OPEN_DELAY_MS`), so the
    // page's own clock is what spends it.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const thread =
      testInfo.project.name === "iphone" ? page.getByRole("dialog", { name: "Thread" }) : turn;

    // The source is asserted before the picture is waited on: one the browser cannot fetch
    // (`dispatch://…`) would fail as an image that never loads rather than naming its source.
    const src = `/api/v1/issues/${issue.key}/artifacts/${pictureSlug}/versions/1`;
    const shot = thread.getByRole("img", { name: "shot" });
    await expect(shot).toHaveAttribute("src", src);
    await expectDrawn(shot, src);

    // The link is found by the target its author wrote, which it keeps whatever it shows.
    const link = thread
      .locator("p", { hasText: "See" })
      .locator(`a[data-dispatch-href="${designTarget}"]`);
    await expect(link).toHaveAttribute("href", path);
    // Asserted once the author's link is drawn, in the same pass: the bare URL's words are the URL
    // and it keeps its href, never the document's name and route a reference link would show.
    const bare = thread.locator("p", { hasText: "Bare:" }).getByRole("link");
    await expect(bare).toHaveAttribute("href", bareUrl);
    await expect(bare).toHaveText(bareUrl);
    await expect(bare).not.toHaveAttribute("data-dispatch-ref");
    // The `dispatch://` forms of text past the bounds are their words alone: no link to click,
    // and nothing the hover card opens on (`[data-dispatch-ref]`, `[data-dispatch-href]`).
    for (const [label, words] of [
      ["Angle:", designTarget],
      ["Self:", designTarget],
      ["Block:", blockTarget],
    ] as const) {
      const paragraph = thread.locator("p", { hasText: label });
      await expect(paragraph).toHaveText(`${label} ${words}`);
      await expect(paragraph.getByRole("link")).toHaveCount(0);
      await expect(paragraph.locator("[data-dispatch-href],[data-dispatch-ref]")).toHaveCount(0);
      if (testInfo.project.name !== "iphone") {
        await paragraph.hover();
        await page.clock.runFor(2 * REF_PREVIEW_OPEN_DELAY_MS);
        await expect(page.getByRole("tooltip")).toHaveCount(0);
      }
    }
    await page.mouse.move(0, 0);
    // The link reads as the document's name, so it wraps past the window's height; the click lands
    // on its last line, clear of the composer pinned above the conversation.
    const lastLine = await link.evaluate((anchor) => {
      const box = anchor.getBoundingClientRect();
      const lines = [...anchor.getClientRects()].filter((line) => line.width > 0);
      const line = lines[lines.length - 1] ?? box;
      return { x: line.left + line.width / 2 - box.left, y: line.top + line.height / 2 - box.top };
    });
    await link.click({ position: lastLine });
    await expect(page).toHaveURL(path);
    await expect(documentEditor(page)).toContainText("The long-named design.");
  } finally {
    await alice.close();
  }
});

// INFRA-760 k8: ProseMirror ends a textblock whose last child is an inline leaf - a paragraph
// that ends with a picture - with an empty `<img class="ProseMirror-separator">`. Tailwind's
// preflight made that element a block and the typography plugin gave it 2em of top and bottom
// margin, so such a paragraph stood about 115 px taller than the picture in it and left a band
// of empty space under the line. `web/src/styles.css` gives that one class back the inline
// display and the zero margin ProseMirror writes it for.

/** A 400x300 greyscale PNG. The one-pixel PNG a paste carries has no height to speak of, so the
 *  space under a paragraph is measured against a picture with a real one. */
const MEASURED_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAZAAAAEsCAAAAADI3LoeAAABq0lEQVR42u3RAQ0AAAzCMKQjHRt/0klYU50qFgARECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAERECACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiACAkRAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABERAgAgIEAEBIiBABASIBUAEBIiAABEQIAICREAEBIiAABEQIAICREAEBIiAABEQIAICREAEBIiAABEQIAICREAEBIiA/G2Fp2238uSu6gAAAABJRU5ErkJggg==";

/** How much taller than the picture in it a paragraph may stand: the line's own leading, and
 *  nothing like the band an unstyled separator left. */
const PARAGRAPH_SLACK_PX = 30;

/** Uploads `base64` to the issue under `name`, through the route a pasted picture takes, and
 *  answers the line that draws it and the address the browser draws it from. */
async function uploadPicture(
  page: Page,
  issueKey: string,
  name: string,
  base64: string
): Promise<{ reference: string; src: string }> {
  const answer = await page.request.post(`/api/v1/issues/${issueKey}/artifacts`, {
    headers: { Origin: dashboardOrigin },
    multipart: {
      file: { buffer: Buffer.from(base64, "base64"), mimeType: "image/png", name },
      name,
    },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  const { artifact } = (await answer.json()) as ArtifactUploadResponse;
  return {
    reference: `dispatch://${issueKey}/artifact/${artifact.slug}@v1`,
    src: `/api/v1/issues/${issueKey}/artifacts/${artifact.slug}/versions/1`,
  };
}

/** Writes `markdown` at the end of the issue's spec as an agent's `doc-edit` does, and answers
 *  the version number the server gave that write. */
async function writeIntoSpec(artifactId: string, markdown: string): Promise<number> {
  await editArtifact(artifactId, { ops: [{ after: "end", markdown, op: "insert" }] }, agentSession);
  const newest = async () =>
    Math.max(...(await getArtifact(artifactId)).versions.map(({ number }) => number));
  await expect.poll(newest).toBeGreaterThan(1);
  return newest();
}

/** Fails unless the paragraph holding `picture` is at most `PARAGRAPH_SLACK_PX` taller than the
 *  picture itself: the two boxes are read in one pass, so a scroll between them cannot skew the
 *  comparison. */
async function expectParagraphHugsPicture(picture: Locator): Promise<void> {
  const measured = await picture.evaluate((image: HTMLImageElement) => {
    const paragraph = image.closest("p");
    if (paragraph === null) throw new Error("the picture stands in no paragraph");
    return {
      paragraph: paragraph.getBoundingClientRect().height,
      picture: image.getBoundingClientRect().height,
    };
  });
  expect(measured.picture, "the picture reserves no height of its own").toBeGreaterThan(100);
  expect(
    measured.paragraph - measured.picture,
    `the paragraph is ${measured.paragraph}px tall around a ${measured.picture}px picture`
  ).toBeLessThanOrEqual(PARAGRAPH_SLACK_PX);
}

test("a paragraph that ends with a picture is no taller than the picture, in the editor and in a past version @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "The space under a picture",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "panel.png", MEASURED_PNG);
    const pictured = await writeIntoSpec(
      issue.primary_artifact_id,
      `![panel](${picture.reference})\n\nBelow the picture.`
    );

    await openSpec(page, issue.key, "Below the picture.");
    const inEditor = documentEditor(page).getByRole("img", { name: "panel" });
    await expectDrawn(inEditor, picture.src);
    await expectParagraphHugsPicture(inEditor);

    await page.getByRole("combobox", { name: "Version" }).selectOption(String(pictured));
    const inVersion = page.getByTestId("version-view").getByRole("img", { name: "panel" });
    await expectDrawn(inVersion, picture.src);
    await expectParagraphHugsPicture(inVersion);
  } finally {
    await alice.close();
  }
});

// INFRA-760 k10: a picture line the browser cannot draw shows a link to the artifact's page, and
// one element carries one hover card. The editor library's own `Open link` card shows while the
// view takes edits, and Dispatch's card triggers on `data-dispatch-href`, so the attribute is
// written in a read-only view alone (`features/doc/picture-view.ts`): the live editor shows the
// library's card, a past version Dispatch's, and neither shows two. The element stays a link
// either way, with the page's own address in it.

/** The editor library's card: the `Open link` button it puts beside a link while the view takes
 *  edits. It is built once and kept on the page, shown and hidden by `data-visible`, so what a
 *  reader sees is the shown one. */
function libraryCard(page: Page): Locator {
  return page
    .getByRole("dialog", { name: "Link actions" })
    .and(page.locator('[data-visible="true"]'));
}

for (const view of [
  { card: "the editor library's", editable: true, name: "the live editor" },
  { card: "Dispatch's own", editable: false, name: "a past version" },
] as const) {
  test(`a picture's fallback link shows ${view.card} card alone in ${view.name}, and stays a link`, async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: "# Pictures\n\nIntro.",
      title: `A fallback link in ${view.name}`,
    });
    // A markdown document: its version's route answers JSON, which no `<img>` can draw.
    await createIssueArtifact(issue.key, { content: "# Notes\n", name: "notes.md" }, agentSession);
    const source = `dispatch://${issue.key}/artifact/notes-md@v1`;
    const pictured = await writeIntoSpec(
      issue.primary_artifact_id,
      `![notes.md](${source})\n\nBelow the link.`
    );
    const path = `/issues/${issue.key}/artifacts/notes-md?v=1`;
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      // The cards below open and close on page timers, so the page's own clock spends them and
      // a card that must not open is waited for rather than raced.
      await page.clock.install();
      await openSpec(page, issue.key, "Below the link.");
      if (!view.editable) {
        await page.getByRole("combobox", { name: "Version" }).selectOption(String(pictured));
      }
      const scope = view.editable ? documentEditor(page) : page.getByTestId("version-view");
      const wrapper = scope.locator(`[data-dispatch-doc-picture="${source}"]`);
      await wrapper.scrollIntoViewIfNeeded();
      const link = wrapper.getByRole("link", { name: `${issue.key} notes-md` });
      await expect(link).toHaveAttribute("href", path);

      // One card, the one this view owns.
      await link.hover();
      await page.clock.runFor(2 * REF_PREVIEW_OPEN_DELAY_MS);
      await expect(libraryCard(page)).toHaveCount(view.editable ? 1 : 0);
      await expect(page.getByRole("tooltip")).toHaveCount(view.editable ? 0 : 1);
      if (view.editable) {
        await expect(libraryCard(page).getByRole("button")).toHaveText("Open link");
      } else {
        await expect(page.getByRole("tooltip")).toContainText("notes.md");
      }
      // The pointer leaves, and both cards are gone before anything is clicked.
      await page.mouse.move(0, 0);
      await page.clock.runFor(1_000);
      await expect(libraryCard(page)).toHaveCount(0);
      await expect(page.getByRole("tooltip")).toHaveCount(0);

      // A middle click is the browser's own: a new tab on the link's address.
      const opened = page.context().waitForEvent("page");
      await link.click({ button: "middle" });
      const tab = await opened;
      await expect(tab).toHaveURL(path);
      await tab.close();

      // A plain click opens the artifact inside the app: a full reload would clear the marker.
      await page.evaluate(() => {
        (window as unknown as { __e2eNavMarker?: boolean }).__e2eNavMarker = true;
      });
      await link.click();
      await expect(page).toHaveURL(path);
      expect(
        await page.evaluate(
          () => (window as unknown as { __e2eNavMarker?: boolean }).__e2eNavMarker
        )
      ).toBe(true);

      // It takes the keyboard's focus, and Enter opens the same page.
      await page.goBack();
      await expect(link).toHaveAttribute("href", path);
      await link.focus();
      await expect(link).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(page).toHaveURL(path);
    } finally {
      await alice.close();
    }
  });
}

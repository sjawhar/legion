import { deflateSync } from "node:zlib";
import { expect, type Locator, type Page, test } from "@playwright/test";

import type { ArtifactUploadResponse } from "../web/src/api/types";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifact,
} from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { touchDrag } from "./touch";
import { asUser } from "./users";

/**
 * The full-size view a click on a picture opens, and the set a reader steps through in it
 * (INFRA-760 k1 and k2). A reader opens a picture to see its own pixels, so every row here
 * reads what the reader sees - a control by its accessible name, the counter's words, the box
 * the browser draws the picture in, the picture's own `naturalWidth` - rather than the markup
 * underneath it.
 *
 * This file is its own negative control: it keeps every helper of its own, so that it can be
 * dropped onto a build of the change's merge base, where each row fails on an assertion. A row
 * that imported a helper this change adds would fail to load there and prove nothing.
 *
 * The fixture is written here rather than stored (`writePng`), because a picture smaller than
 * the window is not zoomable and a click on it closes the view instead of zooming: the view's
 * own pixels, its pan and its zoom need a picture larger than the window at both project
 * widths.
 */

const agentSession = {
  actor: { id: "e2e-picture-view", kind: "session" as const },
  as: "agent" as const,
};

/** The fixture's own size: wider and taller than the `chromium` project's 1280x720 window and
 *  the `iphone` project's 390x844 one, so the viewer always has a zoom to make and room to pan
 *  at the picture's own pixels. */
const PICTURE = { height: 1200, width: 1600 };

const CRC_TABLE = Array.from({ length: 256 }, (_, byte) => {
  let value = byte;
  for (let bit = 0; bit < 8; bit += 1) {
    value = (value & 1) === 1 ? 0xedb88320 ^ (value >>> 1) : value >>> 1;
  }
  return value >>> 0;
});

function crc32(bytes: Buffer): number {
  let value = 0xffffffff;
  for (const byte of bytes) {
    value = (CRC_TABLE[(value ^ byte) & 0xff] ?? 0) ^ (value >>> 8);
  }
  return (value ^ 0xffffffff) >>> 0;
}

/** One PNG chunk: its length, its type, its body and the CRC of the two last. */
function pngChunk(type: string, body: Buffer): Buffer {
  const head = Buffer.alloc(8);
  head.writeUInt32BE(body.length, 0);
  head.write(type, 4, "ascii");
  const checked = Buffer.concat([head.subarray(4), body]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(checked), 0);
  return Buffer.concat([head, body, crc]);
}

/** A greyscale PNG of `width` x `height`, a corner-to-corner ramp, written byte by byte so the
 *  suite stores no large file and the server reads a real header for the version's own size. */
function writePng(width: number, height: number): Buffer {
  const stride = width + 1;
  const raw = Buffer.alloc(stride * height);
  for (let y = 0; y < height; y += 1) {
    for (let x = 0; x < width; x += 1) {
      raw[y * stride + 1 + x] = ((x * 255) / width + (y * 255) / height) / 2;
    }
  }
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  // Eight bits a sample, colour type 0 (greyscale), the rest of the header zero: no
  // interlacing, the one filter and the one compression PNG defines.
  header[8] = 8;
  header[9] = 0;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", header),
    pngChunk("IDAT", deflateSync(raw)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

const LARGE_PNG = writePng(PICTURE.width, PICTURE.height);

test.beforeEach(async () => {
  await resetDatabase();
});

/** Uploads the fixture to `issueKey` under `name`, through the route a pasted picture takes, and
 *  answers what a line and a page draw it from. */
async function uploadPicture(
  page: Page,
  issueKey: string,
  name: string
): Promise<{ reference: string; slug: string; src: string }> {
  const answer = await page.request.post(`/api/v1/issues/${issueKey}/artifacts`, {
    headers: { Origin: dashboardOrigin },
    multipart: {
      file: { buffer: LARGE_PNG, mimeType: "image/png", name },
      name,
    },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  const { artifact } = (await answer.json()) as ArtifactUploadResponse;
  return {
    reference: `dispatch://${issueKey}/artifact/${artifact.slug}@v1`,
    slug: artifact.slug,
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

/** Waits until the browser has drawn `picture`'s own bytes at reading size: a broken icon, an
 *  element the page never loaded and a link in its place all report no width of their own. */
async function expectDrawn(picture: Locator): Promise<void> {
  await picture.scrollIntoViewIfNeeded();
  await expect(picture).toBeVisible();
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
    .toBe(PICTURE.width);
}

/** The layer the viewer puts over the page. Its root carries no accessible name of its own, so
 *  a reader knows it by the control that closes it. */
function fullSize(page: Page): Locator {
  return page.getByRole("dialog").filter({ has: page.getByRole("button", { name: "Close" }) });
}

/** The picture the layer shows, once the browser has drawn its bytes there. */
async function fullSizePicture(page: Page, caption: string): Promise<Locator> {
  const picture = fullSize(page).getByRole("img", { name: caption });
  await expect(picture).toBeVisible();
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
    .toBe(PICTURE.width);
  return picture;
}

interface Box {
  height: number;
  width: number;
  x: number;
  y: number;
}

async function boxOf(element: Locator): Promise<Box> {
  const box = await element.boundingBox();
  if (box === null) {
    throw new Error("the element has no box on screen");
  }
  return box;
}

/** The window the project gave the browser, which the layer covers and the fitted picture sits
 *  inside. */
function windowOf(page: Page): { height: number; width: number } {
  const size = page.viewportSize();
  if (size === null) {
    throw new Error("the page has no window size");
  }
  return size;
}

/** Fails unless the picture is drawn whole inside the window: the view opens over the page, not
 *  past its edges. */
async function expectInsideTheWindow(page: Page, picture: Locator): Promise<void> {
  const box = await boxOf(picture);
  const window = windowOf(page);
  expect(box.width, "the full-size picture is wider than the window").toBeLessThanOrEqual(
    window.width + 1
  );
  expect(box.height, "the full-size picture is taller than the window").toBeLessThanOrEqual(
    window.height + 1
  );
  expect(box.x).toBeGreaterThanOrEqual(-1);
  expect(box.y).toBeGreaterThanOrEqual(-1);
}

/** Waits until the viewer draws the picture at its own pixels, which is what
 *  `secondaryZoomLevel: 1` asks of a zoom: the box on screen is the picture's own width. */
async function expectOwnPixels(picture: Locator): Promise<void> {
  await expect
    .poll(async () => Math.round((await boxOf(picture)).width), {
      message: "the zoom never reached the picture's own pixels",
    })
    .toBe(PICTURE.width);
}

/** Waits until the viewer draws the picture fitted to the window again, which is every zoom
 *  level below its own pixels. */
async function expectFittedAgain(picture: Locator): Promise<void> {
  await expect
    .poll(async () => Math.round((await boxOf(picture)).width), {
      message: "the picture never came back to the size that fits the window",
    })
    .toBeLessThan(PICTURE.width);
}

/** The counter the viewer shows while a set has more than one picture: `2 of 3`. */
function position(page: Page): Locator {
  return fullSize(page).getByText(/^\d+ of \d+$/);
}

/** Drags leftwards with the mouse from the middle of the window, as a reader moves a picture
 *  that is larger than the window they are looking at it in. */
async function dragLeft(page: Page, by: number): Promise<void> {
  const window = windowOf(page);
  const middle = { x: window.width / 2, y: window.height / 2 };
  await page.mouse.move(middle.x, middle.y);
  await page.mouse.down();
  await page.mouse.move(middle.x - by, middle.y, { steps: 10 });
  await page.mouse.up();
}

/** A finger drawn leftwards across the middle of the window, the gesture a phone reader steps
 *  or pans with. */
async function swipeLeft(page: Page): Promise<void> {
  const window = windowOf(page);
  await touchDrag(
    page,
    { x: window.width * 0.85, y: window.height / 2 },
    { x: window.width * 0.15, y: window.height / 2 },
    0
  );
}

test("a picture in a document opens over the page, inside the window, and a zoom reaches its own pixels @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "A picture in a document",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    await writeIntoSpec(
      issue.primary_artifact_id,
      `![wide](${picture.reference})\n\nBelow the picture.`
    );
    await openSpec(page, issue.key, "Below the picture.");
    const drawn = documentEditor(page).getByRole("img", { name: "wide" });
    await expectDrawn(drawn);

    await drawn.click();
    const shown = await fullSizePicture(page, "wide");
    await expectInsideTheWindow(page, shown);

    // The zoom control takes the picture to its own pixels, where a drag moves it.
    await fullSize(page).getByRole("button", { name: "Zoom" }).click();
    await expectOwnPixels(shown);
    const before = await boxOf(shown);
    await dragLeft(page, 100);
    await expect
      .poll(async () => (await boxOf(shown)).x, { message: "the drag panned nothing" })
      .toBeLessThan(before.x - 40);

    // The close control takes the view off the page, and the browser gives the focus back to
    // the editor the click left it in.
    await fullSize(page).getByRole("button", { name: "Close" }).click();
    await expect(fullSize(page)).toHaveCount(0);
    await expect(documentEditor(page)).toBeFocused();
  } finally {
    await alice.close();
  }
});

test("a picture in a past version opens at full size, and Escape closes it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "A picture in a past version",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const version = await writeIntoSpec(
      issue.primary_artifact_id,
      `![wide](${picture.reference})\n\nBelow the picture.`
    );
    await openSpec(page, issue.key, "Below the picture.");
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(version));
    const drawn = page.getByTestId("version-view").getByRole("img", { name: "wide" });
    await expectDrawn(drawn);

    await drawn.click();
    const shown = await fullSizePicture(page, "wide");
    await expectInsideTheWindow(page, shown);

    await page.keyboard.press("Escape");
    await expect(fullSize(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a picture in a rendered body opens at full size, a click on it zooms, and a click outside it closes", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A picture in a comment" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const comment = await createComment(
      issue.key,
      { body: `The screen:\n\n![wide](${picture.reference})` },
      agentSession
    );
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const drawn = turn.getByRole("img", { name: "wide" });
    await expectDrawn(drawn);
    // The body puts the picture inside the link to its artifact's page, which is what holds the
    // reader's focus while the view is open.
    const held = drawn.locator("xpath=ancestor::a[1]");

    await drawn.click();
    const shown = await fullSizePicture(page, "wide");
    await expectInsideTheWindow(page, shown);

    // A click on the picture itself zooms, and another brings it back to the window.
    const fitted = await boxOf(shown);
    await page.mouse.click(fitted.x + fitted.width / 2, fitted.y + fitted.height / 2);
    await expectOwnPixels(shown);
    const window = windowOf(page);
    await page.mouse.click(window.width / 2, window.height / 2);
    await expectFittedAgain(shown);

    // A click on the backdrop beside the picture closes the view, and the link has the focus
    // back.
    const beside = await boxOf(shown);
    expect(beside.x, "the fitted picture leaves no backdrop beside it").toBeGreaterThan(20);
    await page.mouse.click(beside.x / 2, beside.y + beside.height / 2);
    await expect(fullSize(page)).toHaveCount(0);
    await expect(held).toBeFocused();
  } finally {
    await alice.close();
  }
});

test("a picture on its own artifact page opens at full size with no position, where the z key reaches its own pixels", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A picture on its own page" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    await page.goto(`/issues/${issue.key}/artifacts/${picture.slug}?v=1`);
    const name = "wide.png version 1";
    const drawn = page.getByRole("img", { name });
    await expectDrawn(drawn);

    await drawn.click();
    const shown = await fullSizePicture(page, name);
    await expectInsideTheWindow(page, shown);
    // The page draws one picture, which is a set of one: no position, and no step control.
    await expect(position(page)).toBeHidden();
    await expect(fullSize(page).getByRole("button", { name: "Next" })).toHaveCount(0);
    await expect(fullSize(page).getByRole("button", { name: "Previous" })).toHaveCount(0);

    await page.keyboard.press("z");
    await expectOwnPixels(shown);

    await fullSize(page).getByRole("button", { name: "Close" }).click();
    await expect(fullSize(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a 40 px thumbnail opens the artifact's page and no full-size layer", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Thumbnails" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const artifactPath = `/issues/${issue.key}/artifacts/${picture.slug}`;
    const pictured = await createComment(
      issue.key,
      { body: `![wide](${picture.reference})` },
      agentSession
    );
    const cited = await createComment(
      issue.key,
      { body: `See dispatch://${issue.key}/artifact/${picture.slug} before deciding.` },
      agentSession
    );
    const ask = await createAsk(
      issue.key,
      {
        options: [{ label: `![wide](${picture.reference})` }, { label: "Neither" }],
        question: "Which screen is wrong?",
      },
      agentSession
    );

    /** A thumbnail is the 40 px square, drawn from the picture's own bytes. Where it stands
     *  beside words - a caption, a reference's title - those words name it and the square
     *  itself carries no name, so a reader finds it inside the link it is part of. */
    const expectThumbnail = async (thumbnail: Locator) => {
      await expectDrawn(thumbnail);
      const box = await boxOf(thumbnail);
      expect(Math.round(box.width)).toBe(40);
      expect(Math.round(box.height)).toBe(40);
    };

    // A collapsed turn's one-line preview.
    await page.goto(`/issues/${issue.key}/conversation`);
    const preview = page
      .locator(`[data-turn="comment:${pictured.id}"]`)
      .getByRole("link", { name: "wide" })
      .locator("img");
    await expectThumbnail(preview);
    await preview.click();
    await expect(page).toHaveURL(`${artifactPath}?v=1`);
    await expect(fullSize(page)).toHaveCount(0);

    // A reference link to the picture, in a body a reader opened.
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${cited.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const reference = turn.getByRole("link", { name: "wide.png" }).locator("img");
    await expectThumbnail(reference);
    await reference.click();
    await expect(page).toHaveURL(artifactPath);
    await expect(fullSize(page)).toHaveCount(0);

    // An ask's option.
    await page.goto("/");
    const option = page
      .getByTestId(`ask-${ask.id}`)
      .getByRole("link", { name: "wide" })
      .locator("img");
    await expectThumbnail(option);
    await option.click();
    await expect(page).toHaveURL(`${artifactPath}?v=1`);
    await expect(fullSize(page)).toHaveCount(0);

    // The Artifacts tab's row: the square stands beside the row's own link rather than inside
    // it, so a click on it opens nothing at all and the link opens the page.
    await page.goto(`/issues/${issue.key}/artifacts`);
    const row = page.getByTestId(`artifact-${picture.slug}`);
    const square = row.getByRole("img", { name: "wide.png version 1" });
    await expectThumbnail(square);
    await square.click();
    await expect(fullSize(page)).toHaveCount(0);
    await expect(page).toHaveURL(`/issues/${issue.key}/artifacts`);
    await row.getByRole("link", { name: "wide.png" }).click();
    await expect(page).toHaveURL(artifactPath);
  } finally {
    await alice.close();
  }
});

test("a document's three pictures open at the one clicked and step to the third and back @phone", async ({
  browser,
}, testInfo) => {
  const phone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "Three pictures",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const line = (caption: string) => `![${caption}](${picture.reference})`;
    await writeIntoSpec(
      issue.primary_artifact_id,
      `${line("one")}\n\n${line("two")}\n\n${line("three")}\n\nBelow the pictures.`
    );
    await openSpec(page, issue.key, "Below the pictures.");
    const editor = documentEditor(page);
    const clicked = editor.getByRole("img", { name: "two" });
    await expectDrawn(clicked);

    // The view opens at the picture the reader clicked, second of the document's three.
    await clicked.click();
    await expect(position(page)).toHaveText("2 of 3");
    await fullSizePicture(page, "two");

    await page.keyboard.press("ArrowRight");
    await expect(position(page)).toHaveText("3 of 3");
    await fullSizePicture(page, "three");
    await page.keyboard.press("ArrowLeft");
    await expect(position(page)).toHaveText("2 of 3");

    if (phone) {
      // A phone reader steps with a finger; the arrow controls are for a reader with a mouse,
      // and PhotoSwipe hides them on a touch screen.
      await swipeLeft(page);
      await expect(position(page)).toHaveText("3 of 3");
      await fullSizePicture(page, "three");
    } else {
      const layer = fullSize(page);
      await layer.getByRole("button", { name: "Next" }).click();
      await expect(position(page)).toHaveText("3 of 3");
      await fullSizePicture(page, "three");
      await layer.getByRole("button", { name: "Previous" }).click();
      await expect(position(page)).toHaveText("2 of 3");
      await fullSizePicture(page, "two");
    }
  } finally {
    await alice.close();
  }
});

test("a comment's two pictures are the whole set, in the Conversation and in the Inbox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A comment's own pictures" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const line = (caption: string) => `![${caption}](${picture.reference})`;
    // The question holds a picture of its own, in a body beside the reply's: a step through the
    // reply's two must never reach it.
    const ask = await createAsk(
      issue.key,
      { question: `Which screen is wrong?\n\n${line("asked")}` },
      agentSession
    );
    await createComment(
      issue.key,
      { ask_id: ask.id, body: `${line("first")}\n\n${line("second")}` },
      agentSession
    );

    for (const surface of [`/issues/${issue.key}/conversation`, "/"]) {
      await page.goto(surface);
      // The Conversation draws the card twice - once in the page and once in the review sheet
      // beside it - and the reader's own copy is the page's.
      const card = page.getByTestId("main-content").getByTestId(`ask-${ask.id}`);
      const clicked = card.getByRole("img", { name: "first" });
      await expectDrawn(clicked);

      await clicked.click();
      await expect(position(page), `the set read in ${surface}`).toHaveText("1 of 2");
      await fullSizePicture(page, "first");
      await page.keyboard.press("ArrowRight");
      await expect(position(page)).toHaveText("2 of 2");
      await fullSizePicture(page, "second");
      // The comment is the whole set: a step past its last picture comes round to its first,
      // and the question's own picture, in the body beside it, is in no step.
      await page.keyboard.press("ArrowRight");
      await expect(position(page)).toHaveText("1 of 2");
      await fullSizePicture(page, "first");

      await page.keyboard.press("Escape");
      await expect(fullSize(page)).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

test("a document with one picture shows no position", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "One picture",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    await writeIntoSpec(
      issue.primary_artifact_id,
      `![wide](${picture.reference})\n\nBelow the picture.`
    );
    await openSpec(page, issue.key, "Below the picture.");
    const drawn = documentEditor(page).getByRole("img", { name: "wide" });
    await expectDrawn(drawn);

    await drawn.click();
    await fullSizePicture(page, "wide");
    await expect(position(page)).toBeHidden();
    await expect(fullSize(page).getByRole("button", { name: "Next" })).toHaveCount(0);
    await expect(fullSize(page).getByRole("button", { name: "Previous" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a swipe at the picture's own pixels pans and does not step @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "a swipe is a finger on a touch screen, which the phone project is"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "A swipe at its own pixels",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const line = (caption: string) => `![${caption}](${picture.reference})`;
    await writeIntoSpec(
      issue.primary_artifact_id,
      `${line("one")}\n\n${line("two")}\n\nBelow the pictures.`
    );
    await openSpec(page, issue.key, "Below the pictures.");
    const clicked = documentEditor(page).getByRole("img", { name: "one" });
    await expectDrawn(clicked);

    await clicked.click();
    const shown = await fullSizePicture(page, "one");
    await expect(position(page)).toHaveText("1 of 2");

    await page.keyboard.press("z");
    await expectOwnPixels(shown);
    const before = await boxOf(shown);
    await swipeLeft(page);

    // The finger moved the picture the reader is looking at, and left them on it.
    await expect
      .poll(async () => (await boxOf(shown)).x, { message: "the swipe panned nothing" })
      .toBeLessThan(before.x - 40);
    await expect(position(page)).toHaveText("1 of 2");
    await fullSizePicture(page, "one");
  } finally {
    await alice.close();
  }
});

import { deflateSync } from "node:zlib";
import { expect, type Locator, type Page, test } from "@playwright/test";

import type { ArtifactUploadResponse } from "../web/src/api/types";
import {
  createComment,
  createIssue,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifact,
} from "./api";
import { documentEditor, openSpec } from "./editor";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * Nothing moves under the reader while a picture loads (INFRA-760 k14). Every surface that
 * draws a picture at reading size is driven here - a document in the live editor, a past
 * version of it, a rendered body and the artifact page - and each row holds the picture's bytes
 * back until the page has laid out, then reads where the block below the picture sits before
 * and after the bytes arrive. A browser reserves that box only when it is told the picture's
 * shape, so what the rows read is whether the drawn element carries the `width` and `height`
 * the record holds for the version it draws.
 *
 * The rows drive both reference forms: one pinned at the artifact's latest version, which is
 * the line `dispatch artifact` prints and a paste writes, and one naming no version. Four more
 * rows tell apart the cases that carry no shape and so keep today's behaviour: a version stored
 * with no size, a reference pinned below the artifact's latest version, the full-size view
 * opened on a picture whose bytes are still held, and a picture whose recorded size is taken
 * away after the upload and that the page never drew, which is the state every picture stored
 * before the filling pass is in.
 *
 * One last row drives a photograph a browser turns: its bytes store a landscape picture and
 * its Exif Orientation says the camera lay on its side, so the pair the upload records is the
 * upright one (`files.PictureDimensions`), the box the page reserves is upright too, and the
 * full-size view, which takes its shape off the picture the browser decoded, opens it upright
 * rather than squashed into the pair the bytes store.
 *
 * Both widths the check asks for are the two projects this file runs in: `chromium`'s 1280x720
 * window and `iphone`'s 390x844 one, which is why every row carries `@phone`.
 *
 * Every helper here is this file's own: the file is run against a build of the change's merge
 * base to see its rows fail, where a helper this change added would not load at all.
 */

/** The fixture's own shape, which the store reads out of the bytes at upload and every row
 *  below expects to find on the drawn element. Taller than the two pixels a row allows by a
 *  wide margin, so a box the browser never reserved is never mistaken for one it did. */
const PICTURE = { height: 480, width: 640 };

/** How far a block below a picture may sit from where it sat before the bytes arrived. */
const STILL = 2;

/** One line of the body's text. A picture sits on its line's baseline, so the paragraph it is
 *  in grows by a little more than the picture itself as the bytes arrive: the rows that read a
 *  move read the picture's own height within this, measured at 18 px in both projects. */
const LINE = 24;

const agentSession = {
  actor: { id: "e2e-picture-size", kind: "session" as const },
  as: "agent" as const,
};

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
 *  suite stores no file and the server reads a real header for the version's own shape. */
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

const PICTURE_PNG = writePng(PICTURE.width, PICTURE.height);

/** The photograph's own shape, the way round its bytes store it, and the way round a browser
 *  draws it, which its Exif Orientation exchanges. The upload records the second of the two
 *  (`files.PictureDimensions`), since that is the box a page must reserve. */
const PHOTOGRAPH_STORED = { height: 480, width: 640 };
const PHOTOGRAPH = { height: PHOTOGRAPH_STORED.width, width: PHOTOGRAPH_STORED.height };

/** One JPEG segment: `0xff`, its marker, the two-byte length that counts itself, and its body. */
function jpegSegment(marker: number, body: Buffer): Buffer {
  const head = Buffer.alloc(4);
  head[0] = 0xff;
  head[1] = marker;
  head.writeUInt16BE(body.length + 2, 2);
  return Buffer.concat([head, body]);
}

/** The APP1 segment a camera writes: the Exif header, then a TIFF block holding the Orientation
 *  tag alone - the byte order, the TIFF magic, where the first directory stands, one entry in
 *  it, and no directory after. A SHORT fits the entry's own four value bytes, so the block holds
 *  nothing beyond the entry. */
function exifOrientation(orientation: number): Buffer {
  const block = Buffer.alloc(26);
  block.write("II", 0, "ascii");
  block.writeUInt16LE(42, 2);
  block.writeUInt32LE(8, 4);
  block.writeUInt16LE(1, 8);
  block.writeUInt16LE(0x0112, 10);
  block.writeUInt16LE(3, 12);
  block.writeUInt32LE(1, 14);
  block.writeUInt16LE(orientation, 18);
  return Buffer.concat([Buffer.from("Exif\u0000\u0000", "latin1"), block]);
}

/** A grey baseline JPEG of `width` x `height` whose Exif says `orientation`, written byte by
 *  byte as the PNG above is, so the suite stores no file and the server and the browser each
 *  read a real picture. Every block holds one coefficient and that coefficient is zero, so the
 *  scan is the same two bits - no difference from the block before, and nothing after it - once
 *  per eight-by-eight block, and each of the two Huffman tables holds the one symbol one of
 *  those bits names. */
function writeJpeg(width: number, height: number, orientation: number): Buffer {
  // A table: its kind and number, how many codes it holds of each length from one to sixteen,
  // and then the symbols. One code, one bit long, for the one symbol the scan uses.
  const huffman = (kind: number) =>
    Buffer.concat([Buffer.from([kind, 1]), Buffer.alloc(15), Buffer.from([0])]);
  const blocks = Math.ceil(width / 8) * Math.ceil(height / 8);
  const scan = Buffer.alloc(Math.ceil(blocks / 4));
  // A scan ends on the ones that fill its last byte.
  scan[scan.length - 1] = (1 << (scan.length * 8 - blocks * 2)) - 1;
  return Buffer.concat([
    Buffer.from([0xff, 0xd8]),
    jpegSegment(0xe1, exifOrientation(orientation)),
    // The JFIF header, which is what lets Go's decoder answer the shape off the frame below
    // rather than reading the whole scan first (`image/jpeg`'s `decode`, `configOnly`).
    jpegSegment(0xe0, Buffer.from([0x4a, 0x46, 0x49, 0x46, 0, 1, 1, 0, 0, 1, 0, 1, 0, 0])),
    // The quantization table, whose numbers this picture never uses: its one coefficient is 0.
    jpegSegment(0xdb, Buffer.concat([Buffer.from([0]), Buffer.alloc(64, 16)])),
    // The frame: eight bits a sample, the shape, and one component, which makes the picture
    // grey, sampled one to one and read against the table above.
    jpegSegment(
      0xc0,
      Buffer.from([8, height >> 8, height & 0xff, width >> 8, width & 0xff, 1, 1, 0x11, 0])
    ),
    jpegSegment(0xc4, huffman(0x00)),
    jpegSegment(0xc4, huffman(0x10)),
    // The scan itself: the one component, read against both tables above, over the whole
    // spectrum.
    jpegSegment(0xda, Buffer.from([1, 1, 0x00, 0, 63, 0])),
    scan,
    Buffer.from([0xff, 0xd9]),
  ]);
}

const PHOTOGRAPH_JPEG = writeJpeg(PHOTOGRAPH_STORED.width, PHOTOGRAPH_STORED.height, 6);

test.beforeEach(async () => {
  await resetDatabase();
});

interface Uploaded {
  /** The line a paste and `dispatch artifact` write: the artifact's latest version, named. */
  pinned: string;
  /** The same artifact with no version named, which draws its latest. */
  plain: string;
  slug: string;
  /** The bytes route every drawing of that version loads from. */
  src: string;
}

/** Uploads a fixture to `issueKey` under `name`, through the route a pasted picture takes, and
 *  answers the two reference forms and the address they both draw from. */
async function uploadPicture(
  page: Page,
  issueKey: string,
  name: string,
  version = 1,
  file: { bytes: Buffer; mimeType: string } = { bytes: PICTURE_PNG, mimeType: "image/png" }
): Promise<Uploaded> {
  const answer = await page.request.post(`/api/v1/issues/${issueKey}/artifacts`, {
    headers: { Origin: dashboardOrigin },
    multipart: { file: { buffer: file.bytes, mimeType: file.mimeType, name }, name },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  const { artifact } = (await answer.json()) as ArtifactUploadResponse;
  return {
    pinned: `dispatch://${issueKey}/artifact/${artifact.slug}@v${version}`,
    plain: `dispatch://${issueKey}/artifact/${artifact.slug}`,
    slug: artifact.slug,
    src: `/api/v1/issues/${issueKey}/artifacts/${artifact.slug}/versions/${version}`,
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

/** Takes the recorded shape off every version of `slug`, which is the state of every picture
 *  Dispatch stored before it read a size at upload. The two columns are this change's own, so a
 *  build without them has nothing to clear and is left alone: there the rows that call this
 *  read that build's own behaviour, which is the move. */
async function forgetRecordedSize(issueKey: string, slug: string): Promise<void> {
  await sql(`do $$
begin
  update artifact_versions as v set width = null, height = null
  from artifacts as a
  where a.id = v.artifact_id and a.issue_key = '${issueKey}' and a.slug = '${slug}';
exception when undefined_column then
  null;
end $$`);
}

interface HeldBytes {
  /** Lets every held request through, and every one that follows. */
  release: () => void;
}

/** Holds back the bytes of `path` until the row lets them go, which is how each row below reads
 *  a page that has laid out and not yet loaded its picture. A route rather than a wait: the row
 *  decides the moment the bytes arrive, so nothing here races a clock. */
async function holdBytes(page: Page, path: string): Promise<HeldBytes> {
  const held = Promise.withResolvers<void>();
  await page.route(
    (url) => url.pathname === path,
    async (route) => {
      await held.promise;
      await route.continue();
    }
  );
  return { release: () => held.resolve() };
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

/** How far the block below a picture sits from the block above it. The two positions are read
 *  one after the other and subtracted, so a scroll the load itself causes moves both and shows
 *  as no move here: what is left is the room the picture takes. */
async function spanBetween(above: Locator, below: Locator): Promise<number> {
  const top = await boxOf(above);
  const bottom = await boxOf(below);
  return bottom.y - top.y;
}

/** Two frames, which is long enough for the browser to have laid out what it was last told. */
async function settle(page: Page): Promise<void> {
  await page.evaluate(() => {
    const frames = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
    return frames.promise;
  });
}

/** The span, once the page has stopped moving: two frames apart it reads the same, so the row
 *  reads a page that has laid out and not one still arranging itself. Everything a row compares
 *  is read this way, and the bytes are let go only after the first such reading, which is what
 *  makes the second reading's difference the picture's own doing. */
async function settledSpan(page: Page, above: Locator, below: Locator): Promise<number> {
  let last = Number.NaN;
  await expect
    .poll(
      async () => {
        await settle(page);
        const now = await spanBetween(above, below);
        const same = Math.abs(now - last) <= 0.5;
        last = now;
        return same;
      },
      { message: "the page never stopped moving under the row" }
    )
    .toBe(true);
  return last;
}

function naturalWidthOf(picture: Locator): Promise<number> {
  return picture.evaluate((image: HTMLImageElement) => image.naturalWidth);
}

/** The room the element takes on the page, which `boundingBox` would refuse to read while that
 *  room is none: an element the browser reserved no box for is not visible. */
function heightOf(picture: Locator): Promise<number> {
  return picture.evaluate((image: HTMLImageElement) => image.getBoundingClientRect().height);
}

/** Fails unless the browser has drawn none of the picture's own pixels: the row's own check
 *  that the bytes really are held, without which a row that reads no move reads nothing. */
async function expectHeld(...pictures: Locator[]): Promise<void> {
  for (const picture of pictures) {
    await expect(picture).toHaveCount(1);
    expect(
      await naturalWidthOf(picture),
      "the bytes reached the page before the row let them"
    ).toBe(0);
  }
}

/** Waits until the browser has drawn the picture's own bytes. */
async function expectLoaded(...pictures: Locator[]): Promise<void> {
  for (const picture of pictures) {
    await expect
      .poll(() => naturalWidthOf(picture), { message: "the released bytes were never drawn" })
      .toBe(PICTURE.width);
  }
}

/** Fails unless the element carries the shape the record holds for the version it draws, which
 *  is what the browser reserved the box from. */
async function expectRecordedBox(...pictures: Locator[]): Promise<void> {
  for (const picture of pictures) {
    await expect(picture).toHaveAttribute("width", String(PICTURE.width));
    await expect(picture).toHaveAttribute("height", String(PICTURE.height));
  }
}

async function expectNoBox(picture: Locator): Promise<void> {
  expect(await picture.evaluate((image: HTMLImageElement) => image.hasAttribute("width"))).toBe(
    false
  );
  expect(await picture.evaluate((image: HTMLImageElement) => image.hasAttribute("height"))).toBe(
    false
  );
}

/** The body of a card the reader opened, which is the rendered markdown beside the collapsed
 *  card's own one-line preview (`data-markdown-preview`): the preview holds the same words and
 *  the same references, at no reading size, and a row that read it would read the wrong block.
 */
function openedBody(turn: Locator): Locator {
  return turn.locator(".dispatch-markdown:not([data-markdown-preview])");
}

/** The layer the full-size view puts over the page. Its root carries no accessible name of its
 *  own, so a reader knows it by the control that closes it. */
function fullSize(page: Page): Locator {
  return page.getByRole("dialog").filter({ has: page.getByRole("button", { name: "Close" }) });
}

/** The box the view fits a fixture into: the window, and never larger than the picture's own
 *  pixels. The shape is the one the browser draws, which for a picture its Exif turns is not
 *  the one its bytes store. */
function windowFit(page: Page, shape = PICTURE): { height: number; width: number } {
  const size = page.viewportSize();
  if (size === null) {
    throw new Error("the page has no window size");
  }
  const scale = Math.min(1, size.width / shape.width, size.height / shape.height);
  return { height: shape.height * scale, width: shape.width * scale };
}

test("a document in the editor holds its place while a picture at the latest version and one with no version load @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "A document that holds its place",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    await writeIntoSpec(
      issue.primary_artifact_id,
      [
        `![pinned](${picture.pinned})`,
        "Below the pinned picture.",
        `![plain](${picture.plain})`,
        "Below the version-less picture.",
      ].join("\n\n")
    );

    const held = await holdBytes(page, picture.src);
    await openSpec(page, issue.key, "Below the version-less picture.");
    const editor = documentEditor(page);
    const pinned = editor.getByRole("img", { name: "pinned" });
    const plain = editor.getByRole("img", { name: "plain" });
    // The version-less line is a link until the summary is read, so the picture drawn in its
    // place says that read has landed - and the same read carries the shape of both.
    await expectHeld(pinned, plain);
    const first = editor.getByText("Below the pinned picture.");
    const second = editor.getByText("Below the version-less picture.");
    const overPinned = await settledSpan(page, editor.getByText("Intro."), first);
    const overPlain = await settledSpan(page, first, second);

    held.release();
    await expectLoaded(pinned, plain);
    expect(
      Math.abs((await settledSpan(page, editor.getByText("Intro."), first)) - overPinned),
      "the text below the pinned picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    expect(
      Math.abs((await settledSpan(page, first, second)) - overPlain),
      "the text below the version-less picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    await expectRecordedBox(pinned, plain);
  } finally {
    await alice.close();
  }
});

test("a past version of a document holds its place while its pictures load @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "A past version that holds its place",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const version = await writeIntoSpec(
      issue.primary_artifact_id,
      [
        `![pinned](${picture.pinned})`,
        "Below the pinned picture.",
        `![plain](${picture.plain})`,
        "Below the version-less picture.",
      ].join("\n\n")
    );

    const held = await holdBytes(page, picture.src);
    await openSpec(page, issue.key, "Below the version-less picture.");
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(version));
    const past = page.getByTestId("version-view");
    const pinned = past.getByRole("img", { name: "pinned" });
    const plain = past.getByRole("img", { name: "plain" });
    await expectHeld(pinned, plain);
    const first = past.getByText("Below the pinned picture.");
    const second = past.getByText("Below the version-less picture.");
    const overPinned = await settledSpan(page, past.getByText("Intro."), first);
    const overPlain = await settledSpan(page, first, second);

    held.release();
    await expectLoaded(pinned, plain);
    expect(
      Math.abs((await settledSpan(page, past.getByText("Intro."), first)) - overPinned),
      "the text below the pinned picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    expect(
      Math.abs((await settledSpan(page, first, second)) - overPlain),
      "the text below the version-less picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    await expectRecordedBox(pinned, plain);
  } finally {
    await alice.close();
  }
});

test("a rendered body holds its place while a picture at the latest version and one with no version load @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A comment that holds its place" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const comment = await createComment(
      issue.key,
      {
        body: [
          "The screens:",
          `![pinned](${picture.pinned})`,
          "Below the pinned picture.",
          `![plain](${picture.plain})`,
          "Below the version-less picture.",
        ].join("\n\n"),
      },
      agentSession
    );

    const held = await holdBytes(page, picture.src);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    // A collapsed card is a one-line preview, where no picture is at reading size; the card the
    // reader opened is the body.
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const body = openedBody(turn);
    const pinned = body.getByRole("img", { name: "pinned" });
    const plain = body.getByRole("img", { name: "plain" });
    await expectHeld(pinned, plain);
    const first = body.getByText("Below the pinned picture.");
    const second = body.getByText("Below the version-less picture.");
    const overPinned = await settledSpan(page, body.getByText("The screens:"), first);
    const overPlain = await settledSpan(page, first, second);

    held.release();
    await expectLoaded(pinned, plain);
    expect(
      Math.abs((await settledSpan(page, body.getByText("The screens:"), first)) - overPinned),
      "the text below the pinned picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    expect(
      Math.abs((await settledSpan(page, first, second)) - overPlain),
      "the text below the version-less picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    await expectRecordedBox(pinned, plain);
  } finally {
    await alice.close();
  }
});

test("the artifact page holds its place while the version it draws loads @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "An artifact page" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");

    const held = await holdBytes(page, picture.src);
    await page.goto(`/issues/${issue.key}/artifacts/${picture.slug}?v=1`);
    // The page draws the version the reader chose, from that version's own recorded pair.
    const drawn = page.getByRole("img", { name: "wide.png version 1" });
    const below = page.getByRole("link", { name: "Download version 1" });
    await expectHeld(drawn);
    const over = await settledSpan(page, page.getByTestId("artifact-header"), below);

    held.release();
    await expectLoaded(drawn);
    expect(
      Math.abs((await settledSpan(page, page.getByTestId("artifact-header"), below)) - over),
      "what sits under the picture moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);
    await expectRecordedBox(drawn);
  } finally {
    await alice.close();
  }
});

test("a version stored with no size moves the text below it as its bytes arrive @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A version with no size" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    await forgetRecordedSize(issue.key, picture.slug);
    const comment = await createComment(
      issue.key,
      { body: ["The screen:", `![pinned](${picture.pinned})`, "Below the picture."].join("\n\n") },
      agentSession
    );

    const held = await holdBytes(page, picture.src);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const body = openedBody(turn);
    const drawn = body.getByRole("img", { name: "pinned" });
    const below = body.getByText("Below the picture.");
    await expectHeld(drawn);
    // Nothing recorded this version's shape, so nothing is written on the element and the
    // browser has no box to reserve: this is what every picture did before R12, and the row
    // reads the move rather than hiding it.
    await expectNoBox(drawn);
    const over = await settledSpan(page, body.getByText("The screen:"), below);
    const empty = await heightOf(drawn);

    held.release();
    await expectLoaded(drawn);
    // The browser reserved nothing, so the text below moved by the whole room the picture took
    // as it arrived.
    const moved = (await settledSpan(page, body.getByText("The screen:"), below)) - over;
    const took = (await heightOf(drawn)) - empty;
    expect(took, "the picture took no room on the page").toBeGreaterThan(100);
    expect(
      Math.abs(moved - took),
      "the text below a picture of no recorded size did not move with it"
    ).toBeLessThanOrEqual(LINE);
  } finally {
    await alice.close();
  }
});

test("a reference pinned below the artifact's latest version moves the text below it as its bytes arrive @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A reference below the latest" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const first = await uploadPicture(page, issue.key, "wide.png");
    // A second version of the same artifact, which leaves the summary answering for the latest
    // alone: the shape of version 1 is nobody's answer any more.
    const second = await uploadPicture(page, issue.key, "wide.png", 2);
    expect(second.slug).toBe(first.slug);
    const comment = await createComment(
      issue.key,
      { body: ["The screen:", `![older](${first.pinned})`, "Below the picture."].join("\n\n") },
      agentSession
    );

    const held = await holdBytes(page, first.src);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const body = openedBody(turn);
    const drawn = body.getByRole("img", { name: "older" });
    const below = body.getByText("Below the picture.");
    await expect(drawn).toHaveAttribute("src", first.src);
    await expectHeld(drawn);
    await expectNoBox(drawn);
    const over = await settledSpan(page, body.getByText("The screen:"), below);
    const empty = await heightOf(drawn);

    held.release();
    await expectLoaded(drawn);
    const moved = (await settledSpan(page, body.getByText("The screen:"), below)) - over;
    const took = (await heightOf(drawn)) - empty;
    expect(took, "the picture took no room on the page").toBeGreaterThan(100);
    expect(
      Math.abs(moved - took),
      "the text below a version nobody answers the shape of did not move with it"
    ).toBeLessThanOrEqual(LINE);
  } finally {
    await alice.close();
  }
});

test("the full-size view opens a picture whose bytes are still held at the shape the record holds @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A full-size view of held bytes" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    const comment = await createComment(
      issue.key,
      { body: ["The screen:", `![pinned](${picture.pinned})`].join("\n\n") },
      agentSession
    );

    const held = await holdBytes(page, picture.src);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const drawn = openedBody(turn).getByRole("img", { name: "pinned" });
    await expectHeld(drawn);
    await expectRecordedBox(drawn);

    // The view opens on a picture the browser has drawn no pixel of, and is already the size
    // the window fits that shape into: it read the record off the element, and measured no
    // page.
    await drawn.click();
    const shown = fullSize(page).getByRole("img", { name: "pinned" });
    await expect(shown).toHaveCount(1);
    const fit = windowFit(page);
    await expect
      .poll(async () => Math.round((await boxOf(shown)).width), {
        message: "the full-size view never reached the shape the record holds",
      })
      .toBe(Math.round(fit.width));
    const sized = await boxOf(shown);
    expect(Math.abs(sized.height - fit.height)).toBeLessThanOrEqual(STILL);
    expect(await naturalWidthOf(shown), "the bytes reached the view before the row let them").toBe(
      0
    );

    // The bytes change nothing about where the view put the picture.
    held.release();
    await expectLoaded(shown);
    await settle(page);
    const loaded = await boxOf(shown);
    expect(
      Math.abs(loaded.width - sized.width),
      "the view resized as the bytes landed"
    ).toBeLessThanOrEqual(STILL);
    expect(
      Math.abs(loaded.height - sized.height),
      "the view resized as the bytes landed"
    ).toBeLessThanOrEqual(STILL);
  } finally {
    await alice.close();
  }
});

test("a picture whose recorded size is gone and that the page never drew opens at the window fit @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A picture below the fold" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadPicture(page, issue.key, "wide.png");
    await forgetRecordedSize(issue.key, picture.slug);
    // Far enough below the fold at both window heights that lazy loading has not asked for the
    // bytes: the state of a picture stored before the filling pass, in a body the reader has
    // not reached.
    const filler = Array.from({ length: 120 }, (_, line) => `Line ${line + 1} of the body.`);
    const comment = await createComment(
      issue.key,
      { body: [...filler, `![pinned](${picture.pinned})`].join("\n\n") },
      agentSession
    );

    const held = await holdBytes(page, picture.src);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const drawn = openedBody(turn).getByRole("img", { name: "pinned" });
    await expectHeld(drawn);
    await expectNoBox(drawn);

    // Neither a record nor a pixel to read a shape from, so the view sizes the picture by
    // loading it: it opens, and the bytes draw at the window fit when they arrive.
    await drawn.click();
    const shown = fullSize(page).getByRole("img", { name: "pinned" });
    await expect(fullSize(page)).toHaveCount(1);
    held.release();
    await expect(shown).toHaveCount(1);
    await expectLoaded(shown);
    const fit = windowFit(page);
    const box = await boxOf(shown);
    expect(
      Math.abs(box.width - fit.width),
      "the view drew the picture at another width"
    ).toBeLessThanOrEqual(STILL);
    expect(
      Math.abs(box.height - fit.height),
      "the view drew the picture at another height"
    ).toBeLessThanOrEqual(STILL);
  } finally {
    await alice.close();
  }
});

test("a photograph its Exif turns reserves the upright box and opens full-size upright @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A photograph on its side" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const photograph = await uploadPicture(page, issue.key, "photo.jpg", 1, {
      bytes: PHOTOGRAPH_JPEG,
      mimeType: "image/jpeg",
    });
    const comment = await createComment(
      issue.key,
      {
        body: ["The photograph:", `![pinned](${photograph.pinned})`, "Below the photograph."].join(
          "\n\n"
        ),
      },
      agentSession
    );

    const held = await holdBytes(page, photograph.src);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const body = openedBody(turn);
    const drawn = body.getByRole("img", { name: "pinned" });
    await expectHeld(drawn);
    // The box the page reserved is the way round the browser draws the photograph. The
    // landscape pair its bytes store would reserve a box of another shape altogether, and the
    // text below it would move once the upright picture landed.
    await expect(drawn).toHaveAttribute("width", String(PHOTOGRAPH.width));
    await expect(drawn).toHaveAttribute("height", String(PHOTOGRAPH.height));
    const above = body.getByText("The photograph:");
    const below = body.getByText("Below the photograph.");
    const span = await settledSpan(page, above, below);

    held.release();
    await expect
      .poll(() => naturalWidthOf(drawn), { message: "the released bytes were never drawn" })
      .toBe(PHOTOGRAPH.width);
    expect(
      Math.abs((await settledSpan(page, above, below)) - span),
      "the text below the photograph moved as its bytes arrived"
    ).toBeLessThanOrEqual(STILL);

    // The view takes the shape off the picture the browser decoded, so it opens the photograph
    // upright at the window's fit rather than squashed into the pair its bytes store.
    await drawn.click();
    const shown = fullSize(page).getByRole("img", { name: "pinned" });
    await expect(shown).toHaveCount(1);
    const fit = windowFit(page, PHOTOGRAPH);
    await expect
      .poll(async () => Math.round((await boxOf(shown)).width), {
        message: "the full-size view never reached the shape the browser draws",
      })
      .toBe(Math.round(fit.width));
    expect(
      Math.abs((await boxOf(shown)).height - fit.height),
      "the view opened the photograph at another height"
    ).toBeLessThanOrEqual(STILL);
  } finally {
    await alice.close();
  }
});

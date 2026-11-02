import { deflateSync } from "node:zlib";
import { expect, type Locator, type Page, test } from "@playwright/test";

import type { ArtifactUploadResponse } from "../web/src/api/types";
import { createIssue, createProject, dashboardOrigin, listComments, userHeaders } from "./api";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

/** CRC-32 of `bytes`, which every PNG chunk carries. */
function crc32(bytes: Uint8Array): number {
  let crc = ~0;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
  }
  return ~crc >>> 0;
}

function chunk(type: string, data: Buffer): Buffer {
  const length = Buffer.alloc(4);
  length.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([length, body, crc]);
}

/** A `width` by `height` PNG of one colour: a picture the page draws at a real size, which a box
 *  can be dragged over. */
function picturePng(width: number, height: number, [red, green, blue]: number[]): Buffer {
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header.set([8, 2, 0, 0, 0], 8);
  const row = Buffer.alloc(1 + width * 3);
  for (let x = 0; x < width; x += 1) row.set([red ?? 0, green ?? 0, blue ?? 0], 1 + x * 3);
  const pixels = Buffer.concat(Array.from({ length: height }, () => row));
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", header),
    chunk("IDAT", deflateSync(pixels)),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

/** Uploads `bytes` as `name` to the issue, as a person's file upload does. */
async function upload(issue: string, name: string, type: string, bytes: Buffer) {
  const form = new FormData();
  form.set("name", name);
  form.set("file", new Blob([new Uint8Array(bytes)], { type }), name);
  const response = await fetch(new URL(`/api/v1/issues/${issue}/artifacts`, dashboardOrigin), {
    body: form,
    headers: await userHeaders("alice"),
    method: "POST",
  });
  if (!response.ok) throw new Error(`upload ${name}: ${response.status} ${await response.text()}`);
  return (await response.json()) as ArtifactUploadResponse;
}

/** The picture's own place on the page, once it has loaded and been laid out. */
async function pictureBox(
  page: Page
): Promise<{ x: number; y: number; width: number; height: number }> {
  const picture = page.getByTestId("picture-boxes");
  await expect(picture).toBeVisible();
  await expect
    .poll(async () => (await picture.boundingBox())?.height ?? 0, { timeout: 10_000 })
    .toBeGreaterThan(50);
  const box = await picture.boundingBox();
  if (box === null) throw new Error("the picture has no box");
  return box;
}

/** Drags over the picture from one fraction of it to another, as a person does with the mouse. */
async function drag(page: Page, from: [number, number], to: [number, number]): Promise<void> {
  const box = await pictureBox(page);
  await page.mouse.move(box.x + box.width * from[0], box.y + box.height * from[1]);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width * to[0], box.y + box.height * to[1], { steps: 8 });
  await page.mouse.up();
}

function commentBox(page: Page, name: string): Locator {
  return page.getByRole("region", { name: `Comment on ${name}` });
}

function marginCard(page: Page, label: string): Locator {
  return page.locator("article").filter({ hasText: label });
}

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
});

test("a person drags a box over a picture and comments on it; the box and its thread choose each other", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Chart review\n",
    title: "Chart review",
  });
  await upload(issue.key, "chart.png", "image/png", picturePng(400, 200, [30, 120, 200]));

  await page.goto(`/issues/${issue.key}/artifacts/chart-png`);
  await expect(commentBox(page, "chart.png")).toContainText("On the whole picture, version 1.");

  // Drag from 60% across and 10% down to 90% across and 35% down.
  await drag(page, [0.6, 0.1], [0.9, 0.35]);
  await expect(page.getByTestId("picture-box-draft")).toBeVisible();
  await expect(commentBox(page, "chart.png")).toContainText("On the box you drew, version 1.");
  await commentBox(page, "chart.png")
    .getByLabel("Comment", { exact: true })
    .fill("This bar looks low.");
  await commentBox(page, "chart.png").getByRole("button", { name: "Send" }).click();

  const drawn = page
    .getByTestId("picture-boxes")
    .getByRole("button", { name: /^Box 1: This bar looks low\./ });
  await expect(drawn).toBeVisible();
  await expect(page.getByTestId("picture-box-draft")).toHaveCount(0);
  const card = marginCard(page, "Box 1 · version 1");
  await expect(card).toBeVisible();
  await expect(card).toContainText("This bar looks low.");

  // Dispatch kept the box as the fractions of the picture the drag covered.
  const [stored] = await listComments(issue.key);
  expect(stored?.anchor?.kind).toBe("file");
  expect(stored?.anchor?.version).toBe(1);
  const box = stored?.anchor?.box;
  expect(box?.left).toBeCloseTo(0.6, 1);
  expect(box?.top).toBeCloseTo(0.1, 1);
  expect(box?.width).toBeCloseTo(0.3, 1);
  expect(box?.height).toBeCloseTo(0.25, 1);

  // Choosing the box chooses its card, and choosing the card lights the box.
  await drawn.click();
  await expect(card).toHaveAttribute("aria-current", "true");
  await expect(drawn).toHaveAttribute("aria-pressed", "true");

  // A comment with no box is on the whole picture.
  await commentBox(page, "chart.png")
    .getByLabel("Comment", { exact: true })
    .fill("Which run is this?");
  await commentBox(page, "chart.png").getByRole("button", { name: "Send" }).click();
  await expect(marginCard(page, "Whole file · version 1")).toContainText("Which run is this?");

  // Both threads are on the issue's Conversation tab, each linked back to the picture.
  await page.getByRole("tab", { name: /^Conversation/ }).click();
  await expect(page.getByText("This bar looks low.")).toBeVisible();
  await expect(page.getByText("Which run is this?")).toBeVisible();
  await expect(page.getByRole("link", { name: "View on chart.png" })).toHaveCount(2);
});

test("a box drawn on an older version is listed with its version, and choosing it shows that version", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Chart review\n",
    title: "Chart review",
  });
  await upload(issue.key, "chart.png", "image/png", picturePng(400, 200, [30, 120, 200]));
  await page.goto(`/issues/${issue.key}/artifacts/chart-png`);
  await drag(page, [0.1, 0.1], [0.4, 0.5]);
  await commentBox(page, "chart.png").getByLabel("Comment", { exact: true }).fill("The first run.");
  await commentBox(page, "chart.png").getByRole("button", { name: "Send" }).click();
  await expect(
    page.getByTestId("picture-boxes").getByRole("button", { name: /^Box 1/ })
  ).toBeVisible();

  await upload(issue.key, "chart.png", "image/png", picturePng(400, 200, [200, 60, 30]));
  await page.reload();
  await expect(commentBox(page, "chart.png")).toContainText("version 2.");
  await expect(page.getByTestId("picture-boxes").getByRole("button")).toHaveCount(0);
  const card = marginCard(page, "Box 1 · version 1");
  await expect(card).toBeVisible();

  await card.click();
  await expect(page).toHaveURL(/[?&]v=1(&|$)/);
  await expect(
    page.getByTestId("picture-boxes").getByRole("button", { name: /^Box 1: The first run\./ })
  ).toBeVisible();
  await expect(commentBox(page, "chart.png")).toContainText("version 1.");
});

test("a comment on a file that is not a picture is on the whole file", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "# Results\n", title: "Results" });
  await upload(issue.key, "results.csv", "text/csv", Buffer.from("run,score\n1,0.5\n"));

  await page.goto(`/issues/${issue.key}/artifacts/results-csv`);
  await expect(commentBox(page, "results.csv")).toContainText("On the whole file, version 1.");
  await expect(page.getByRole("button", { name: "Draw a box" })).toHaveCount(0);
  await commentBox(page, "results.csv")
    .getByLabel("Comment", { exact: true })
    .fill("Run 1 looks off.");
  await commentBox(page, "results.csv").getByRole("button", { name: "Send" }).click();
  await expect(marginCard(page, "Whole file · version 1")).toContainText("Run 1 looks off.");
  const [stored] = await listComments(issue.key);
  expect(stored?.anchor).toMatchObject({ kind: "file", version: 1 });
  expect(stored?.anchor?.box).toBeUndefined();
});

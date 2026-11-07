import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

import {
  baseUrl,
  createComment,
  createIssue,
  createProject,
  editArtifact,
  userHeaders,
} from "./api";
import { cliSession, runDispatch } from "./dispatch-cli";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// `dispatch artifact` answers an upload a browser draws where it is read with the markdown line
// that draws it, pinned to the version the command just made, so the caller pastes the upload
// into a body instead of building the address by hand. The line is in the command's own output
// and in the `details` it records; a file no browser draws earns none. On the way back, an agent
// reading a body that holds a video line is told nothing about it: no model takes a video, and a
// line naming one would stand in every read of that body for ever.
//
// Every helper here is this file's own: the file is run against a build of the change's merge
// base to see its rows fail, where a helper this change added would not load at all.

/** A 2x2 PNG, the upload whose line the editor below draws. */
const TINY_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFUlEQVR4nGP8z4AATAwMDAwMDAwMAA3+AR6vmgTYAAAAAElFTkSuQmCC";

/** A 32x32 MP4 of three tenths of a second, H.264 video and AAC audio: the video the command
 *  writes a line for and the agent read below passes over. */
const TINY_MP4 = [
  "AAAAIGZ0eXBpc29tAAACAGlzb21pc28yYXZjMW1wNDEAAAAIZnJlZQAAAxVtZGF0AAACuwYF//+33EXpvebZSLeWLNgg2S",
  "Pu73gyNjQgLSBjb3JlIDE2NCByMzE5MSA0NjEzYWMzIC0gSC4yNjQvTVBFRy00IEFWQyBjb2RlYyAtIENvcHlsZWZ0IDIw",
  "MDMtMjAyNCAtIGh0dHA6Ly93d3cudmlkZW9sYW4ub3JnL3gyNjQuaHRtbCAtIG9wdGlvbnM6IGNhYmFjPTEgcmVmPTMgZG",
  "VibG9jaz0xOjA6MCBhbmFseXNlPTB4MzoweDExMyBtZT1oZXggc3VibWU9NyBwc3k9MSBwc3lfcmQ9MS4wMDowLjAwIG1p",
  "eGVkX3JlZj0xIG1lX3JhbmdlPTE2IGNocm9tYV9tZT0xIHRyZWxsaXM9MSA4eDhkY3Q9MSBjcW09MCBkZWFkem9uZT0yMS",
  "wxMSBmYXN0X3Bza2lwPTEgY2hyb21hX3FwX29mZnNldD0tMiB0aHJlYWRzPTEgbG9va2FoZWFkX3RocmVhZHM9MSBzbGlj",
  "ZWRfdGhyZWFkcz0wIG5yPTAgZGVjaW1hdGU9MSBpbnRlcmxhY2VkPTAgYmx1cmF5X2NvbXBhdD0wIGNvbnN0cmFpbmVkX2",
  "ludHJhPTAgYmZyYW1lcz0zIGJfcHlyYW1pZD0yIGJfYWRhcHQ9MSBiX2JpYXM9MCBkaXJlY3Q9MSB3ZWlnaHRiPTEgb3Bl",
  "bl9nb3A9MCB3ZWlnaHRwPTIga2V5aW50PTI1MCBrZXlpbnRfbWluPTEwIHNjZW5lY3V0PTQwIGludHJhX3JlZnJlc2g9MC",
  "ByY19sb29rYWhlYWQ9NDAgcmM9YWJyIG1idHJlZT0xIGJpdHJhdGU9OCByYXRldG9sPTEuMCBxY29tcD0wLjYwIHFwbWlu",
  "PTAgcXBtYXg9NjkgcXBzdGVwPTQgaXBfcmF0aW89MS40MCBhcT0xOjEuMDAAgAAAABNliIQAI//+8ivwKZ2BxsD3nD+B3A",
  "BMYXZjNjEuMy4xMDAAAjBADgAAAAhBmiJsR//+GAAAAAgBnkF5H/+DgQEYIAcBGCAHARggBwAABcRtb292AAAAbG12aGQA",
  "AAAAAAAAAAAAAAAAAAPoAAABLAABAAABAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAA",
  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADAAAClnRyYWsAAABcdGtoZAAAAAMAAAAAAAAAAAAAAAEAAAAAAAABLAAA",
  "AAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAAAAIAAAACAAAAAAACRlZHRzAAAAHG",
  "Vsc3QAAAAAAAAAAQAAASwAAAgAAAEAAAAAAg5tZGlhAAAAIG1kaGQAAAAAAAAAAAAAAAAAACgAAAAMAFXEAAAAAAAtaGRs",
  "cgAAAAAAAAAAdmlkZQAAAAAAAAAAAAAAAFZpZGVvSGFuZGxlcgAAAAG5bWluZgAAABR2bWhkAAAAAQAAAAAAAAAAAAAAJG",
  "RpbmYAAAAcZHJlZgAAAAAAAAABAAAADHVybCAAAAABAAABeXN0YmwAAAC9c3RzZAAAAAAAAAABAAAArWF2YzEAAAAAAAAA",
  "AQAAAAAAAAAAAAAAAAAAAAAAIAAgAEgAAABIAAAAAAAAAAEUTGF2YzYxLjMuMTAwIGxpYngyNjQAAAAAAAAAAAAAAAAY//",
  "8AAAAzYXZjQwFkAAr/4QAXZ2QACqzZSWwEQAAAAwBAAAAFA8SJZYABAAVo6+yyLP34+AAAAAAQcGFzcAAAAAEAAAABAAAA",
  "FGJ0cnQAAAAAAABOIAAATiAAAAAYc3R0cwAAAAAAAAABAAAAAwAABAAAAAAUc3RzcwAAAAAAAAABAAAAAQAAAChjdHRzAA",
  "AAAAAAAAMAAAABAAAIAAAAAAEAAAwAAAAAAQAABAAAAAAoc3RzYwAAAAAAAAACAAAAAQAAAAEAAAABAAAAAgAAAAIAAAAB",
  "AAAAIHN0c3oAAAAAAAAAAAAAAAMAAALWAAAADAAAAAwAAAAYc3RjbwAAAAAAAAACAAAAMAAAAxkAAAJZdHJhawAAAFx0a2",
  "hkAAAAAwAAAAAAAAAAAAAAAgAAAAAAAAEsAAAAAAAAAAAAAAABAQAAAAABAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAA",
  "AAAAQAAAAAAAAAAAAAAAAAAAJGVkdHMAAAAcZWxzdAAAAAAAAAABAAABLAAABAAAAQAAAAAB0W1kaWEAAAAgbWRoZAAAAA",
  "AAAAAAAAAAAAAAH0AAAA1gVcQAAAAAAC1oZGxyAAAAAAAAAABzb3VuAAAAAAAAAAAAAAAAU291bmRIYW5kbGVyAAAAAXxt",
  "aW5mAAAAEHNtaGQAAAAAAAAAAAAAACRkaW5mAAAAHGRyZWYAAAAAAAAAAQAAAAx1cmwgAAAAAQAAAUBzdGJsAAAAfnN0c2",
  "QAAAAAAAAAAQAAAG5tcDRhAAAAAAAAAAEAAAAAAAAAAAABABAAAAAAH0AAAAAAADZlc2RzAAAAAAOAgIAlAAIABICAgBdA",
  "FQAAAAAAH0AAAAJDBYCAgAUViFblAAaAgIABAgAAABRidHJ0AAAAAAAAH0AAAAJDAAAAIHN0dHMAAAAAAAAAAgAAAAMAAA",
  "QAAAAAAQAAAWAAAAAoc3RzYwAAAAAAAAACAAAAAQAAAAEAAAABAAAAAgAAAAMAAAABAAAAJHN0c3oAAAAAAAAAAAAAAAQA",
  "AAATAAAABAAAAAQAAAAEAAAAGHN0Y28AAAAAAAAAAgAAAwYAAAMxAAAAGnNncGQBAAAAcm9sbAAAAAIAAAAB//8AAAAcc2",
  "JncAAAAAByb2xsAAAAAQAAAAQAAAABAAAAYXVkdGEAAABZbWV0YQAAAAAAAAAhaGRscgAAAAAAAAAAbWRpcmFwcGwAAAAA",
  "AAAAAAAAAAAsaWxzdAAAACSpdG9vAAAAHGRhdGEAAAABAAAAAExhdmY2MS4xLjEwMA==",
].join("");

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-cli-picture-line" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** The `details` of the command's last call, as it recorded them in the session's own ledger
 *  (`results.jsonl` under `DISPATCH_STATE_DIR`, which `runDispatch` points at `home`). */
function lastDetails(home: string): Record<string, unknown> {
  const ledger = readFileSync(join(home, "sessions", cliSession, "results.jsonl"), "utf-8");
  const lines = ledger.trimEnd().split("\n");
  const last = JSON.parse(lines[lines.length - 1] ?? "{}") as {
    details?: Record<string, unknown>;
  };
  return last.details ?? {};
}

/** Uploads `base64` to the issue under `name` and `mimeType` through the API, as the Artifacts
 *  tab does, for a row that needs the upload without paying for a second command. */
async function uploadVersion(
  issueKey: string,
  name: string,
  mimeType: string,
  base64: string
): Promise<void> {
  const form = new FormData();
  form.append("name", name);
  form.append("file", new Blob([Buffer.from(base64, "base64")], { type: mimeType }), name);
  const response = await fetch(new URL(`/api/v1/issues/${issueKey}/artifacts`, baseUrl), {
    body: form,
    headers: await userHeaders(),
    method: "POST",
  });
  expect(response.status, await response.text()).toBeLessThan(300);
}

test("dispatch artifact prints the version's own picture line for a picture and for a video, and none for a file no browser draws", async ({
  browser,
}) => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  // Three cold `bun` starts, each its own process, take longer than the page interactions the
  // default deadline is sized for.
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Clips\n\nIntro.",
    title: "Lines the command prints",
  });
  const home = mkdtempSync(join(tmpdir(), "dispatch-cli-picture-line-"));
  const alice = await asUser(browser, "alice");
  try {
    writeFileSync(join(home, "panel.png"), Buffer.from(TINY_PNG, "base64"));
    writeFileSync(join(home, "reel.mp4"), Buffer.from(TINY_MP4, "base64"));
    writeFileSync(join(home, "rows.csv"), "row_id,note\n1,kept\n");

    const pictureLine = `![panel.png](dispatch://${issue.key}/artifact/panel-png@v1)`;
    const picture = await runDispatch(
      ["artifact", "--issue", issue.key, "--name", "panel.png", "--path", "panel.png"],
      home
    );
    expect(picture.code, `${picture.stdout}${picture.stderr}`).toBe(0);
    const pictureLines = picture.stdout.split("\n");
    // The first line keeps its shape; the drawing line follows it.
    expect(pictureLines[0]).toContain("Uploaded panel.png as version 1");
    expect(pictureLines[1]).toBe(pictureLine);
    expect(lastDetails(home).picture).toBe(pictureLine);

    const videoLine = `![reel.mp4](dispatch://${issue.key}/artifact/reel-mp4@v1)`;
    const video = await runDispatch(
      ["artifact", "--issue", issue.key, "--name", "reel.mp4", "--path", "reel.mp4"],
      home
    );
    expect(video.code, `${video.stdout}${video.stderr}`).toBe(0);
    const videoLines = video.stdout.split("\n");
    expect(videoLines[0]).toContain("Uploaded reel.mp4 as version 1");
    expect(videoLines[1]).toBe(videoLine);
    expect(lastDetails(home).picture).toBe(videoLine);

    // The negative control: a file no browser draws where it is read earns no line at all.
    const records = await runDispatch(
      ["artifact", "--issue", issue.key, "--name", "rows.csv", "--path", "rows.csv"],
      home
    );
    expect(records.code, `${records.stdout}${records.stderr}`).toBe(0);
    expect(records.stdout).toContain("Uploaded rows.csv as version 1");
    expect(records.stdout).not.toContain("![");
    expect(lastDetails(home)).not.toHaveProperty("picture");

    // The printed line is the drawing line: a document written with it draws the picture.
    await editArtifact(
      issue.primary_artifact_id,
      { ops: [{ after: "end", markdown: `${pictureLine}\n\nBelow the picture.`, op: "insert" }] },
      agentSession
    );
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the picture.");
    const drawn = documentEditor(page).getByRole("img", { name: "panel.png" });
    await expect(drawn).toHaveAttribute(
      "src",
      `/api/v1/issues/${issue.key}/artifacts/panel-png/versions/1`
    );
    await drawn.scrollIntoViewIfNeeded();
    await expect
      .poll(() => drawn.evaluate((image: HTMLImageElement) => image.naturalWidth))
      .toBeGreaterThan(0);
  } finally {
    await alice.close();
    rmSync(home, { force: true, recursive: true });
  }
});

test("an agent's read of a body holding a video line says nothing about the video, where a picture still earns its Pictures: line", async () => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  // Two cold `bun` starts, each its own process.
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "What a reader is told" });
  await uploadVersion(issue.key, "reel.mp4", "video/mp4", TINY_MP4);
  await uploadVersion(issue.key, "panel.png", "image/png", TINY_PNG);
  const video = await createComment(issue.key, {
    body: `Here it is.\n\n![reel.mp4](dispatch://${issue.key}/artifact/reel-mp4@v1)`,
  });
  const picture = await createComment(issue.key, {
    body: `And this.\n\n![panel.png](dispatch://${issue.key}/artifact/panel-png@v1)`,
  });
  const home = mkdtempSync(join(tmpdir(), "dispatch-cli-video-read-"));

  try {
    const read = await runDispatch(
      ["read", "--ref", `dispatch://${issue.key}/comment/${video.id}`],
      home
    );
    expect(read.code, `${read.stdout}${read.stderr}`).toBe(0);
    expect(read.stdout).toContain("Here it is.");
    // Not a line saying the file cannot be shown, and not an empty heading over nothing.
    expect(read.stdout).not.toContain("Pictures:");
    expect(read.stdout).not.toContain("reel.mp4 is");

    // The guard: the same read of a body holding a picture does name it, so the silence above
    // is the video's and not a read that names nothing.
    const shown = await runDispatch(
      ["read", "--ref", `dispatch://${issue.key}/comment/${picture.id}`],
      home
    );
    expect(shown.code, `${shown.stdout}${shown.stderr}`).toBe(0);
    expect(shown.stdout).toContain("Pictures:");
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
});

import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

import {
  baseUrl,
  createIssue,
  createProject,
  e2eAgentToken,
  getArtifactVersion,
  getIssue,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import { resetDatabase } from "./seed";

// `dispatch artifact --content` (and `--content-file`) stores its text as a Markdown document,
// which joins the lines of a JSONL file into one paragraph. The command refuses inline content
// under a name that is not a Markdown one before anything leaves the process, and names `--path`,
// which stores the file's bytes as they are.

/** Fifteen JSON records, one per line, as a records upload holds them. */
const rows = `${Array.from(
  { length: 15 },
  (_, index) => `{"row_id": ${index}, "file_name": "wave_${index}.json", "note": "*kept*"}`
).join("\n")}\n`;

/** The bytes the issue's rows.jsonl reads back at version 1: a document's markdown, a file's own
 *  bytes, or undefined when the issue holds no rows.jsonl. */
async function storedRows(issue: string): Promise<string | undefined> {
  const artifact = ((await getIssue(issue)).artifacts ?? []).find(
    ({ name }) => name === "rows.jsonl"
  );
  if (artifact === undefined) return undefined;
  if (artifact.kind === "doc") return (await getArtifactVersion(artifact.id, 1)).markdown;
  const response = await fetch(
    new URL(
      `/api/v1/issues/${encodeURIComponent(issue)}/artifacts/${artifact.slug}/versions/1`,
      baseUrl
    ),
    { headers: { Authorization: `Bearer ${e2eAgentToken}` } }
  );
  expect(response.status).toBe(200);
  return response.text();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("dispatch artifact refuses inline content under a .jsonl name and stores the file unchanged through --path", async () => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  // Two cold `bun` starts take longer than the page interactions the default deadline is sized for.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Records upload" });
  const home = mkdtempSync(join(tmpdir(), "dispatch-cli-artifact-name-"));

  try {
    writeFileSync(join(home, "rows.jsonl"), rows);

    const inline = await runDispatch(
      ["artifact", "--issue", issue.key, "--name", "rows.jsonl", "--content-file", "rows.jsonl"],
      home
    );
    const stored = await storedRows(issue.key);
    // Before the refusal the command exited 0 and the issue held a document whose markdown was
    // not the file: `{ code: 0, stored: "altered" }`.
    expect(
      {
        code: inline.code,
        stored: stored === undefined ? "nothing" : stored === rows ? "unchanged" : "altered",
      },
      `${inline.stdout}${inline.stderr}${stored ?? ""}`
    ).toEqual({ code: 1, stored: "nothing" });
    expect(inline.stdout).toContain('"rows.jsonl" is not a Markdown name');
    expect(inline.stdout).toContain("--path <file>");

    const file = await runDispatch(
      ["artifact", "--issue", issue.key, "--name", "rows.jsonl", "--path", "rows.jsonl"],
      home
    );
    expect(file.code, `${file.stdout}${file.stderr}`).toBe(0);
    expect(await storedRows(issue.key)).toBe(rows);
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});

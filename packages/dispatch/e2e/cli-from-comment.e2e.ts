import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "@playwright/test";
import { createComment, createIssue, createProject } from "./api";
import { runDispatch } from "./dispatch-cli";
import { resetDatabase } from "./seed";

test("dispatch doc-edit accepts from a comment block and both reads list the edge, while serves refuses it", async () => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(120_000);
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    title: "Requirement provenance",
    spec: "Keep this paragraph.\n",
  });
  const source = await createComment(issue.key, { body: "Require a bounded retry count." });
  const comment = `dispatch://${issue.key}/comment/${source.id}`;
  const target = `${comment}#b-b1`;
  const home = mkdtempSync(join(tmpdir(), "dispatch-cli-from-comment-"));
  try {
    const write = await runDispatch(
      [
        "doc-edit",
        "--issue",
        issue.key,
        "--ops-json",
        JSON.stringify([
          {
            op: "insert",
            after: "end",
            markdown: `:::requirement{#r9 from="${target}"}\nRetries are bounded.\n:::\n`,
          },
        ]),
      ],
      home
    );
    expect(write.code, write.stdout + write.stderr).toBe(0);
    const requirement = await runDispatch(
      ["read", "--ref", `dispatch://${issue.key}/spec#b-r9`],
      home
    );
    expect(requirement.code, requirement.stdout + requirement.stderr).toBe(0);
    expect(requirement.stdout).toContain(`from: ${target}`);
    expect(requirement.stdout).toContain("Links:");
    expect(requirement.stdout).toContain(`- from comment ${comment} block b1`);
    const readSource = await runDispatch(["read", "--ref", comment], home);
    expect(readSource.code, readSource.stdout + readSource.stderr).toBe(0);
    expect(readSource.stdout).toContain("Referenced by:");
    expect(readSource.stdout).toContain(`- from block dispatch://${issue.key}/spec#b-r9`);
    const control = await runDispatch(
      [
        "doc-edit",
        "--issue",
        issue.key,
        "--ops-json",
        JSON.stringify([
          {
            op: "insert",
            after: "end",
            markdown: `:::check{#c1 serves="${target}"}\nCheck retries.\n:::\n`,
          },
        ]),
      ],
      home
    );
    expect(control.code, control.stdout + control.stderr).toBe(1);
    expect(control.stdout + control.stderr).toContain(
      "block[] attribute points only at a block of a document"
    );
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});

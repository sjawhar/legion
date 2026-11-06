import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { runDispatch } from "./dispatch-cli";
import { resetDatabase } from "./seed";

test("the CLI shows a nested reply under the comment it answers", async () => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(120_000);
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Nested comment replies" });
  const home = mkdtempSync(join(tmpdir(), "dispatch-cli-reply-answers-"));
  const command = async (args: readonly string[]) => {
    const result = await runDispatch(args, home);
    expect(result.code, `${result.stdout}${result.stderr}`).toBe(0);
    return result.stdout;
  };
  const post = async (body: string, parent?: string) => {
    const output = await command([
      "comment",
      "--issue",
      issue.key,
      "--body",
      body,
      ...(parent === undefined ? [] : ["--reply-to", parent]),
    ]);
    const id = /Posted comment ([0-9a-f-]{36})/.exec(output)?.[1];
    expect(id, output).toBeDefined();
    if (id === undefined) throw new Error(`comment command returned no id: ${output}`);
    return id;
  };
  try {
    const root = await post("root");
    const middle = await post("middle", root);
    const leaf = await post("leaf", middle);
    const rootRead = await command(["read", "--ref", `dispatch://${issue.key}/comment/${root}`]);
    expect(rootRead).toContain(`Answers: ${middle}`);
    const direct = `- replies_to comment dispatch://${issue.key}/comment/${middle}`;
    expect(rootRead.split("\n").filter((line) => line.startsWith(direct))).toHaveLength(1);
    expect(rootRead).not.toContain(`- answers comment dispatch://${issue.key}/comment/${middle}`);
    const leafRead = await command(["read", "--ref", `dispatch://${issue.key}/comment/${leaf}`]);
    expect(leafRead).toContain(`- answers comment dispatch://${issue.key}/comment/${middle}`);
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});

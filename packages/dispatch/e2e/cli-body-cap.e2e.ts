import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DISPATCH_BLOCK_UNITS } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";

import {
  createIssue,
  createProject,
  getComment,
  getMessage,
  listComments,
  listIssueMessages,
} from "./api";
import { blockCapRefusal, bodyOf, paragraphsOf } from "./bodies";
import { type CliRun, runDispatch } from "./dispatch-cli";
import { resetDatabase } from "./seed";

// INFRA-370: `dispatch comment` and `dispatch message` carry no length of their own. A body of
// 40,000 characters in paragraphs posts against a running Dispatch, because the server bounds a
// body block by block; a body that is one paragraph of 32,001 characters is refused by the
// server, and the command prints that refusal as it came. The command is the one agents run,
// spawned as its own process (e2e/dispatch-cli.ts).

/** The id in `Posted comment <id>` / `Posted message <id> (dispatch://...)`. */
function postedId(what: "comment" | "message", run: CliRun): string {
  const posted = new RegExp(`Posted ${what} ([0-9a-f-]{36})`).exec(run.stdout);
  if (posted === null) {
    throw new Error(`dispatch ${what} posted nothing: ${run.code}\n${run.stdout}\n${run.stderr}`);
  }
  return posted[1];
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("dispatch posts a comment and a message of 40,000 characters, and prints the server's refusal of one long paragraph", async () => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  // Four cold `bun` starts and two bodies of 40,000 characters take longer than the page
  // interactions the default deadline is sized for.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long bodies" });
  const whole = paragraphsOf(40_000);
  // One paragraph, one character over the bound the server holds each block to.
  const tooLong = bodyOf(DISPATCH_BLOCK_UNITS + 1);
  const refusal = blockCapRefusal(DISPATCH_BLOCK_UNITS + 1);
  const home = mkdtempSync(join(tmpdir(), "dispatch-cli-body-cap-"));

  try {
    const comment = await runDispatch(["comment", "--issue", issue.key, "--body", whole], home);
    expect(comment.code, `${comment.stdout}${comment.stderr}`).toBe(0);
    const posted = await getComment(postedId("comment", comment));
    expect(posted.comment.body).toBe(whole);

    const message = await runDispatch(["message", "--issue", issue.key, "--body", whole], home);
    expect(message.code, `${message.stdout}${message.stderr}`).toBe(0);
    const sent = await getMessage(issue.key, postedId("message", message));
    expect(sent.message.body).toBe(whole);

    // The one long paragraph is sent, and Dispatch refuses it naming the block and the bound.
    // The command prints those words and nothing of its own.
    for (const command of ["comment", "message"] as const) {
      const refused = await runDispatch([command, "--issue", issue.key, "--body", tooLong], home);
      expect(refused.code, `${refused.stdout}${refused.stderr}`).not.toBe(0);
      expect(refused.stdout.trim()).toBe(refusal);
    }

    // Refused by the server, so nothing of the two refusals is stored: the issue holds the
    // comment and the message that posted and no more.
    expect((await listComments(issue.key)).map((row) => row.body.length)).toEqual([40_000]);
    expect((await listIssueMessages(issue.key)).map((row) => row.message.body.length)).toEqual([
      40_000,
    ]);
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
});

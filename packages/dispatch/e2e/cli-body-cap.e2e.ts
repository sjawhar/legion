import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { overCapMessage } from "@legion/contracts";
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";

import {
  createIssue,
  createProject,
  getComment,
  getMessage,
  listComments,
  listIssueMessages,
} from "./api";
import { bodyOf } from "./bodies";
import { type CliRun, runDispatch } from "./dispatch-cli";
import { resetDatabase } from "./seed";

// INFRA-45: `dispatch comment` and `dispatch message` post a body of exactly DISPATCH_BODY_MAX
// against a running Dispatch, and refuse one character more before anything leaves the process.
// The command is the one agents run, spawned as its own process (e2e/dispatch-cli.ts).

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

test("dispatch posts a comment and a message of the whole body limit, and refuses one longer before it sends", async () => {
  test.skip(test.info().project.name !== "chromium", "the command runs outside the browser");
  // Four cold `bun` starts and two bodies of DISPATCH_BODY_MAX characters take longer than the
  // page interactions the default deadline is sized for.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long bodies" });
  const whole = bodyOf(DISPATCH_BODY_MAX);
  const tooLong = bodyOf(DISPATCH_BODY_MAX + 1);
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

    // One character more is refused by the command itself, which names how far over the limit the
    // body is and by how much, so the refusal says what to trim.
    for (const command of ["comment", "message"] as const) {
      const refused = await runDispatch([command, "--issue", issue.key, "--body", tooLong], home);
      expect(refused.code, `${refused.stdout}${refused.stderr}`).not.toBe(0);
      expect(refused.stdout).toContain(
        `- --body ${overCapMessage(tooLong.length, DISPATCH_BODY_MAX)}`
      );
    }

    // Refused before it sent: the issue holds the comment and the message that posted, and
    // nothing of the two refusals.
    expect((await listComments(issue.key)).map((row) => row.body.length)).toEqual([
      DISPATCH_BODY_MAX,
    ]);
    expect((await listIssueMessages(issue.key)).map((row) => row.message.body.length)).toEqual([
      DISPATCH_BODY_MAX,
    ]);
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
});

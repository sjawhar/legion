import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, getIssueArtifactText } from "./api";
import { runDispatch } from "./dispatch-cli";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The whitespace-only lines that end an indented code block read as the browser's engine
// (milkdown/micromark) reads them: a line with spaces past the block's indent stays in the block
// as those spaces, a line without them drops off its end, and neither spreads the list or the item
// around the block. The server's reader reads a document the same way (dispatch://INFRA-719). One
// spec, written by an agent, reads alike in its live editor, in a past version and in a comment
// holding the same body; its stored text fences each block with the lines it keeps, is what
// `dispatch doc-read` prints, and reads back the same.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-indented-code-whitespace-lines" },
  as: "agent" as const,
};

const body =
  "> -\n>       code\n>       \n>       note\n>       \n>   \n\nBetween.\n\n- a\n\n          x\n        \n- b\n\nAfter.\n\n          y\n        \n       \nEnd.\n";

test.beforeEach(async () => {
  await resetDatabase();
});

async function seed(): Promise<{ commentId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: `# Code\n\n${body}`, title: "Indented code whitespace lines" },
    agentSession
  );
  const comment = await createComment(issue.key, { body }, agentSession);
  return { commentId: comment.id, key: issue.key };
}

/** What the engine draws for the body: the `data-spread` of each list and of each item, in
 *  document order, and the whole text of each code block. Neither list is spread; of the three
 *  items only `a` is, by the blank line between its paragraph and its code; the blockquote's code
 *  keeps its inner blank line and drops the trailing ones, and the other two blocks keep their
 *  whitespace-only lines. */
const engineReading = {
  code: ["code\n\nnote", "    x\n  ", "      y\n    \n   "],
  items: ["false", "true", "false"],
  lists: ["false", "false"],
};

/** Asserts `view` draws the body as `engineReading`, once the body's last paragraph shows. The
 *  whole reading is compared as one JSON line, so a failure prints every value, each code
 *  block's whitespace escaped. */
async function expectEngineReading(view: Locator): Promise<void> {
  await expect(view.getByText("End.", { exact: true })).toBeVisible();
  await expect
    .poll(async () =>
      JSON.stringify({
        code: await view
          .locator("pre code")
          .evaluateAll((blocks) => blocks.map((block) => block.textContent)),
        items: await view
          .locator("li")
          .evaluateAll((items) => items.map((item) => item.getAttribute("data-spread"))),
        lists: await view
          .locator("ul, ol")
          .evaluateAll((lists) => lists.map((list) => list.getAttribute("data-spread"))),
      })
    )
    .toBe(JSON.stringify(engineReading));
}

async function withAlice(browser: Browser, check: (page: Page) => Promise<void>): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await check(await alice.newPage());
  } finally {
    await alice.close();
  }
}

test("an indented code block's trailing whitespace lines stay in the block and spread nothing in the spec editor", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "End.");
    await expectEngineReading(documentEditor(page));
  });
});

test("an indented code block's trailing whitespace lines stay in the block and spread nothing in a past version of the spec", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/artifacts/spec?v=1`);
    await expectEngineReading(page.getByTestId("version-view"));
  });
});

test("an indented code block's trailing whitespace lines stay in the block and spread nothing in a comment", async ({
  browser,
}) => {
  const { commentId, key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/conversation`);
    // A collapsed card previews the body inline; the expanded thread draws its blocks.
    const turn = page.locator(`[data-turn="comment:${commentId}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    await expectEngineReading(turn.locator(".dispatch-markdown"));
  });
});

/** The spec as the server stores it: every code block fenced, holding the whitespace lines the
 *  engine keeps in it (none in the blockquote's block, whose trailing lines the engine drops). */
const stored =
  "# Code\n\n> - ```\n>   code\n>   \n>   note\n>   ```\n\nBetween.\n\n- a\n\n  ```\n      x\n    \n  ```\n- b\n\nAfter.\n\n```\n      y\n    \n   \n```\n\nEnd.\n";

test("the stored spec fences each code block with its whitespace lines, reads back the same and is what doc-read prints", async ({
  browser,
}, testInfo) => {
  const { key } = await seed();
  expect((await getIssueArtifactText(key, "spec")).markdown).toBe(stored);

  // The command runs outside the browser, so one project runs it.
  if (testInfo.project.name === "chromium") {
    const home = mkdtempSync(join(tmpdir(), "dispatch-cli-doc-read-"));
    try {
      const read = await runDispatch(["doc-read", "--issue", key, "--artifact", "spec"], home);
      expect(read.code, `${read.stdout}${read.stderr}`).toBe(0);
      const trailer = read.stdout.indexOf("Document token:");
      expect(trailer, read.stdout).toBeGreaterThan(-1);
      // The stored markdown, its own line ending, then the blank line before the trailer.
      expect(read.stdout.slice(0, trailer)).toBe(`${stored}\n\n`);
    } finally {
      rmSync(home, { force: true, recursive: true });
    }
  }

  // Written again as a spec, the stored text is stored unchanged and draws as the body did.
  const readBack = await createIssue(
    { project: "CORE", spec: stored, title: "Read back" },
    agentSession
  );
  expect((await getIssueArtifactText(readBack.key, "spec")).markdown).toBe(stored);
  await withAlice(browser, async (page) => {
    await openSpec(page, readBack.key, "End.");
    await expectEngineReading(documentEditor(page));
  });
});

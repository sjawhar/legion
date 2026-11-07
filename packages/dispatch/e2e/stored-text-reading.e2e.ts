/**
 * A stored text the browser's own parser reads differently from Go shows as Go reads it.
 *
 * The text is `pmdoc`'s `html-block` corpus document, read from the corpus itself so this spec
 * and Go's fixture hold one text. An HTML block is the shape the two readers disagree on: the
 * Proof schema carries markup inline only, so the browser's read-only engine refuses the whole
 * document ("Cannot create node for doc", its content being
 * `["paragraph", "html", "paragraph"]`), while Go reads it as three paragraphs and shows the
 * markup as the literal lines it is - the reading
 * `pmdoc/testdata/display.json` holds for `html-block` and
 * `pmdoc/display_test.go` pins. A parse the engine refuses leaves the body as its own literal
 * text under `data-markdown-fallback` (`useRenderedMarkdown.tsx`), which is what this comment
 * showed before the server sent its reading with the text.
 */
import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-html-block-agent", kind: "session" as const },
  as: "agent" as const,
};

const body = readFileSync(
  new URL(
    "../../envoy/internal/dispatch/pmdoc/testdata/corpus-display/html-block.md",
    import.meta.url
  ),
  "utf8"
);

test.beforeEach(async () => {
  await resetDatabase();
});

test("Conversation draws Go's reading of a comment whose markdown the browser's own parser refuses", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "HTML block" });
  const comment = await createComment(issue.key, { body }, agent);
  // The server stores the text as written: the reading is what differs, not the bytes.
  expect(comment.body).toBe(body);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const card = page.locator(`[data-turn="comment:${comment.id}"]`);
    await expect(card).toHaveCount(1);
    // The thread opens collapsed, with its body as a one-line preview; expanded, the card holds
    // the whole reading.
    await card.getByRole("button", { name: "Expand thread" }).click();

    // Go's reading: three paragraphs, the markup among them as the lines an author wrote, each
    // line of its own. A body the browser parsed itself holds nothing read out of it at all.
    const drawn = card.locator("div.dispatch-markdown").first();
    const paragraphs = drawn.locator("p");
    await expect(paragraphs).toHaveCount(3);
    await expect(paragraphs.first()).toHaveText("Before the block.");
    await expect(paragraphs.nth(1)).toHaveText(
      /^<div class="note">\s*<p>Markup the Proof schema carries inline only\.<\/p>\s*<\/div>$/
    );
    await expect(paragraphs.nth(1).locator("br")).toHaveCount(2);
    await expect(paragraphs.last()).toHaveText("After the block.");
    // The markup is text, never markup: no element of it reaches the page.
    await expect(card.locator("div.note")).toHaveCount(0);
    await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

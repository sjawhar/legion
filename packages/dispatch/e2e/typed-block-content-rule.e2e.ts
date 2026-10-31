import { type Browser, expect, test } from "@playwright/test";

import { getArtifactText, getIssue } from "./api";
import { documentEditor, openWithCaret, paste } from "./editor";
import { resetDatabase } from "./seed";

// INFRA-565: a typed block whose content breaks its type's content rule, pasted as plain text into
// a live document, keeps every word. Milkdown dropped such a block and all its text silently, with
// only "Cannot create node" in the console. The editor now draws the block as its literal lines:
// the opening and closing lines are text, the content keeps its own blocks, and the save escapes
// the opening line, so the block reads back as text and is never stored as an ask.

test.beforeEach(async () => {
  await resetDatabase();
});

/** Opens a document holding "Intro text." as alice with the caret on an empty line of its own
 * after it, collecting the page's console messages from then on. */
async function openOnEmptyLine(browser: Browser) {
  const opened = await openWithCaret(browser, "Broken ask paste", "Intro text.", "text.", "end");
  const messages: string[] = [];
  opened.page.on("console", (message) => messages.push(message.text()));
  await opened.page.keyboard.press("Enter");
  return { ...opened, messages };
}

test("an ask holding a heading, pasted as plain text, keeps every word as its lines", async ({
  browser,
}) => {
  const { alice, issue, messages, page } = await openOnEmptyLine(browser);
  try {
    await paste(page, { html: "", text: ":::ask{#a1}\n# Heading words\n:::\n" });

    const editor = documentEditor(page);
    await expect(editor.getByText(":::ask{#a1}", { exact: true })).toBeVisible();
    await expect(editor.getByRole("heading", { name: "Heading words" })).toBeVisible();
    await expect(editor.getByText(":::", { exact: true })).toBeVisible();
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("Intro text.\n\n\\:::ask{#a1}\n\n# Heading words\n\n:::\n");
    expect((await getIssue(issue.key)).open_asks).toEqual([]);
    expect(messages.filter((message) => message.includes("Cannot create node"))).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("an ask with a paragraph after its options, pasted as plain text, keeps every line", async ({
  browser,
}) => {
  const { alice, issue, messages, page } = await openOnEmptyLine(browser);
  try {
    await paste(page, { html: "", text: ":::ask{#a1}\nQ?\n\n- a\n\nAfter list\n:::\n" });

    const editor = documentEditor(page);
    await expect(editor.getByText("Q?", { exact: true })).toBeVisible();
    await expect(editor.getByRole("listitem").getByText("a", { exact: true })).toBeVisible();
    await expect(editor.getByText("After list", { exact: true })).toBeVisible();
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("Intro text.\n\n\\:::ask{#a1}\n\nQ?\n\n- a\n\nAfter list\n\n:::\n");
    expect((await getIssue(issue.key)).open_asks).toEqual([]);
    expect(messages.filter((message) => message.includes("Cannot create node"))).toEqual([]);
  } finally {
    await alice.close();
  }
});

// sjawhar/legion#2433: a block of a type the editor does not know, pasted from the system clipboard, keeps
// every word as its lines too; the paste handler threw `unknown typed block "note"` and drew
// nothing.
test("an unknown typed block pasted as plain text keeps every word as its lines", async ({
  browser,
}) => {
  const { alice, issue, page } = await openOnEmptyLine(browser);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await alice.grantPermissions(["clipboard-read", "clipboard-write"]);
    await page.evaluate(() => navigator.clipboard.writeText(":::note\nhello world\n:::\n"));
    const editor = documentEditor(page);
    // Counts the paste events the key press delivers to the editor, kept on the body where the
    // editor does not read it, so a press that delivers none fails here, not as a dropped paste.
    await editor.evaluate((root) => {
      document.body.dataset.pastes = "0";
      root.addEventListener("paste", () => {
        document.body.dataset.pastes = String(Number(document.body.dataset.pastes) + 1);
      });
    });
    await page.keyboard.press("ControlOrMeta+V");
    await expect(page.locator("body")).toHaveAttribute("data-pastes", "1");

    await expect(editor.locator("p", { hasText: /^:::note$/u })).toBeVisible();
    await expect(editor.locator("p", { hasText: /^hello world$/u })).toBeVisible();
    await expect(editor.locator("p", { hasText: /^:::$/u })).toBeVisible();
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("Intro text.\n\n\\:::note\n\nhello world\n\n:::\n");
    expect(errors).toEqual([]);
  } finally {
    await alice.close();
  }
});

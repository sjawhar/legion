import { DISPATCH_BLOCK_UNITS } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { blockCapRefusal, bodyOf, paragraphsOf } from "./bodies";
import { barAction, composer, openSpec, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// INFRA-370: no composer carries a length of its own. A comment field takes a body of 40,000
// characters whole, a question field takes a question of 2,001 characters whole, and a body the
// server refuses - one paragraph past the bound it holds each block to - shows the server's own
// refusal under the composer, with the draft kept.
//
// Each body goes in with `fill`, which Playwright inserts through the page's own editing pipeline
// (one `Input.insertText`, the path a paste takes), not by assigning `value`: React's `onChange`
// runs on the result, exactly as for a reader pasting a long body. Typing 40,000 characters key
// by key would take minutes and would exercise no part of the component that this does not.

test.beforeEach(async () => {
  await resetDatabase();
});

test("the comment composer takes a 40,000-character body, and shows the server's refusal of one long paragraph", async ({
  browser,
}) => {
  test.skip(test.info().project.name !== "chromium", "one browser proves the composer");
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long comment" });
  const whole = paragraphsOf(40_000);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    await expect(field).toBeVisible();

    // No maxLength holds the field to anything: a body of 40,000 characters stays whole.
    await field.fill(whole);
    // One readback, not two: `fill` resolves once the page has taken the text, and this one
    // proves its length and its content together.
    expect(await field.inputValue()).toBe(whole);

    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(page.getByText(whole.slice(0, 40))).toBeVisible();

    // One paragraph a character past the server's block bound: the composer sends it, and what
    // the reader is shown is the server's refusal, naming the block and the bound.
    const tooLong = bodyOf(DISPATCH_BLOCK_UNITS + 1);
    const refusal = blockCapRefusal(DISPATCH_BLOCK_UNITS + 1);
    await field.fill(tooLong);
    await expect.poll(async () => (await field.inputValue()).length).toBe(tooLong.length);
    await form.getByRole("button", { exact: true, name: "Send" }).click();

    await expect(form.getByText(refusal)).toBeVisible();
    await expect(field).toHaveValue(tooLong);
  } finally {
    await alice.close();
  }
});

test("the ask composer takes a question past the length it used to cut", async ({ browser }) => {
  test.skip(test.info().project.name !== "chromium", "one browser proves the composer");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The quick brown fox",
    title: "Long question",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "The quick brown fox");
    await selectEditorText(page, "brown");
    await barAction(page, "Ask");
    const question = composer(page).getByLabel("Question");
    await expect(question).toBeVisible();

    // 2,001 characters: one more than the length this field used to cut a question to. Dispatch
    // bounds a question and its options together, and this is far inside that bound.
    const whole = bodyOf(2_001);
    await question.fill(whole);
    await expect.poll(async () => (await question.inputValue()).length).toBe(2_001);
    expect(await question.inputValue()).toBe(whole);
  } finally {
    await alice.close();
  }
});

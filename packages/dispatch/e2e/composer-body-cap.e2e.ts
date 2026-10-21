import { ASK_QUESTION_MAX, DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { bodyOf } from "./bodies";
import { barAction, composer, openSpec, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// INFRA-45: the composer's own cap is DISPATCH_BODY_MAX, which is what the server, the agent
// tools and `dispatch` now take; the ask composer keeps ASK_QUESTION_MAX, which this issue does
// not move. Every body here is derived from those constants, so a cap that moves again moves
// these rows with it.
//
// Each body goes in with `fill`, which Playwright inserts through the page's own editing pipeline
// (one `Input.insertText`, the path a paste takes), not by assigning `value`: the textarea's
// `maxLength` is applied to it and React's `onChange` runs on the result, exactly as for a reader
// pasting a long body. Typing 32,000 characters key by key would take minutes and would exercise
// no part of the component that this does not.

test.beforeEach(async () => {
  await resetDatabase();
});

test("the comment composer takes a whole body of the limit, and holds a longer one to it", async ({
  browser,
}) => {
  test.skip(test.info().project.name !== "chromium", "one browser proves the cap");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long comment" });
  const whole = bodyOf(DISPATCH_BODY_MAX);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
    await expect(field).toBeVisible();

    // The composer's cap is DISPATCH_BODY_MAX: a body of exactly that many characters stays whole
    // in the field. A composer still capped at 2,000 keeps 2,000 of them.
    await field.fill(whole);
    await expect.poll(async () => (await field.inputValue()).length).toBe(DISPATCH_BODY_MAX);
    expect(await field.inputValue()).toBe(whole);

    // And the cap still holds: one character more than the limit is cut back to it.
    await field.fill(bodyOf(DISPATCH_BODY_MAX + 1));
    await expect.poll(async () => (await field.inputValue()).length).toBe(DISPATCH_BODY_MAX);
  } finally {
    await alice.close();
  }
});

test("an ask composer still stops at the question limit", async ({ browser }) => {
  test.skip(test.info().project.name !== "chromium", "one browser proves the cap");
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

    // A question is not a body: this composer caps at ASK_QUESTION_MAX, which INFRA-45 leaves
    // where it was, so a body-sized question is cut back to the question limit.
    await question.fill(bodyOf(DISPATCH_BODY_MAX));
    await expect.poll(async () => (await question.inputValue()).length).toBe(ASK_QUESTION_MAX);

    // The whole question limit, on the other hand, stays whole.
    const wholeQuestion = bodyOf(ASK_QUESTION_MAX);
    await question.fill(wholeQuestion);
    await expect.poll(async () => (await question.inputValue()).length).toBe(ASK_QUESTION_MAX);
    expect(await question.inputValue()).toBe(wholeQuestion);
  } finally {
    await alice.close();
  }
});

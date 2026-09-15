import { expect, test } from "@playwright/test";

import { createAsk, createIssue, createProject, getAsk } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: {
    id: "action-session",
    kind: "session" as const,
    origin: { session_title: "Action agent" },
  },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("an action ask waits on a human and Done clears the Inbox", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "spec", title: "Release checklist" });
  const action = await createAsk(
    issue.key,
    { kind: "action", question: "Confirm that the release is deployed." },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/");

    await expect(page.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect(page.getByText("Blocked on you: 1 item, oldest 0m")).toBeVisible();
    const card = page.getByTestId(`ask-${action.id}`);
    await expect(card.getByText("Action", { exact: true })).toBeVisible();
    await expect(card.getByText("0m", { exact: true })).toBeVisible();
    await expect(card.getByRole("radio", { name: "Done" })).toBeVisible();
    await expect(card.getByRole("radio", { name: "Can't" })).toBeVisible();

    await card.getByRole("radio", { name: "Done" }).click();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect(card).toHaveCount(0);
    await expect(page.getByText(/Blocked on you:/)).toHaveCount(0);
    await expect
      .poll(() => getAsk(action.id))
      .toMatchObject({
        ask: { answer: { selected: ["Done"] }, state: "answered" },
      });
  } finally {
    await alice.close();
  }
});

test("picking Can't names the reason it needs, focuses the field, and sends once one is typed", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "spec", title: "Release checklist" });
  const action = await createAsk(
    issue.key,
    { kind: "action", question: "Confirm that the release is deployed." },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${action.id}`);
    await expect(card.getByRole("radio", { name: "Can't" })).toBeVisible();
    await expect(card.getByText("Add a reason to send Can't")).toHaveCount(0);

    await card.getByRole("radio", { name: "Can't" }).click();
    const reason = card.getByLabel("Reason (required)");
    await expect(reason).toBeFocused();
    const answer = card.getByRole("button", { name: "Answer" });
    await expect(answer).toBeDisabled();
    await expect(answer).toHaveAccessibleDescription("Add a reason to send Can't");
    await expect(card.getByText("Add a reason to send Can't")).toBeVisible();

    await reason.fill("The deploy key expired.");
    await expect(answer).toBeEnabled();
    await expect(card.getByText("Add a reason to send Can't")).toHaveCount(0);
    await answer.click();
    await expect(card).toHaveCount(0);
    await expect
      .poll(() => getAsk(action.id))
      .toMatchObject({
        ask: {
          answer: { selected: ["Can't"], text: "The deploy key expired." },
          state: "answered",
        },
      });
  } finally {
    await alice.close();
  }
});

import { expect, type Locator, type Page, test } from "@playwright/test";

import { getSentMessages, plannerSession, resetFakeEnvoy, setLiveSessions } from "./agents";
import { claimIssue, createIssue, createProject, mintAgentToken } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Check k10 of dispatch://INFRA-1042/artifact/verification-md, serving R10: handing the request
// to the issue's own agent is one action among the rest. The bar offers it only while a live
// session holds the issue's claim, it always asks, and approving is what sends.
//
// The one phrase of that check this spec does not read is "even above the confidence
// threshold": no model decides anything here, so there is no confidence to be above. What this
// spec reads instead is the stronger half of the same sentence - the action is never run on its
// own, so nothing is sent until the person presses the row.

/** The words a person types into the bar, which ask for work rather than for a hit. */
const words = "write up the rota and say who is on call";

// The holder is the shared Planner fixture, which advertises `aside` and `btw`: Dispatch sends
// a targeted message in the mode it was asked for and never switches it to another, so a
// session advertising neither would have its delivery fail rather than reach the listener.
const session = plannerSession.session_id;
const holder = plannerSession.title;

function bar(page: Page): Locator {
  return page.getByRole("dialog", { name: "Search" });
}

/** The row the bar offers the typed words to the issue's agent on. */
function offer(page: Page, key: string): Locator {
  return bar(page).getByRole("option", { name: `Send this to the agent on ${key}` });
}

/** Opens the bar over the issue page and types the words. */
async function typeWords(page: Page, key: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await expect(page.getByRole("heading", { name: "The watch rota" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("/");
  await expect(bar(page)).toBeVisible();
  await page.getByRole("combobox", { name: "Search" }).fill(words);
  // The search has answered, so a row still absent is absent because the bar does not offer it.
  await expect(bar(page).getByRole("option").first()).toBeVisible();
}

/** An issue of its own, with a live session holding its claim when `claimed`. */
async function seedIssue(claimed: boolean): Promise<string> {
  const issue = await createIssue({ project: "CORE", title: "The watch rota" });
  await setLiveSessions([plannerSession]);
  if (claimed) {
    const token = await mintAgentToken("going-handoff", "alice");
    await claimIssue(issue.key, {
      actor: { id: session, kind: "session", origin: { session_title: holder } },
      as: "agent",
      token,
    });
  }
  return issue.key;
}

test.beforeEach(async () => {
  await resetDatabase();
  await resetFakeEnvoy();
  await createProject({ key: "CORE", name: "Core" });
});

test("the bar offers the words to the agent on the issue, and only approving sends them @phone", async ({
  browser,
}) => {
  const key = await seedIssue(true);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await typeWords(page, key);

    // Offering is not sending: the row is in the same list as the other actions, and nothing
    // has reached the session while it sits there.
    await expect(offer(page, key)).toBeVisible();
    expect(await getSentMessages()).toHaveLength(0);

    await offer(page, key).click();

    // One message, naming the issue and the words, and the bar says it went.
    await expect
      .poll(async () =>
        (await getSentMessages()).map((sent) => ({
          message: sent.message,
          target: sent.target_session,
        }))
      )
      .toEqual([{ message: `${key}: ${words}`, target: session }]);
    await expect(bar(page).getByTestId("search-send-to-agent-said")).toHaveText(
      `Sent to ${holder}, the agent on ${key}.`
    );
    // One approval is one message: the row is gone, so no second press can send again.
    await expect(offer(page, key)).toBeHidden();
  } finally {
    await context.close();
  }
});

test("on an issue no live session holds the action is absent @phone", async ({ browser }) => {
  const key = await seedIssue(false);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await typeWords(page, key);

    await expect(offer(page, key)).toBeHidden();
    expect(await getSentMessages()).toHaveLength(0);
  } finally {
    await context.close();
  }
});

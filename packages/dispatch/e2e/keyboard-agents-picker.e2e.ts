import { expect, type Locator, type Page, test } from "@playwright/test";

import { openAgents, plannerSession, seedAgents, setLiveSessions } from "./agents";
import { createMessage, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The issue picker's keyboard-step rule (`markKeyStep` in `AgentsPage.tsx`) holds only because
// every engine dispatches a closed select's `change` inside the key's own task, where the HTML spec
// queues it as a task of its own. These rows notice an engine that moves to the spec's queued task:
// its first arrow or letter would commit at once. So the `webkit` and `firefox` projects run this
// spec as well as Chromium, and its rows drive keys through `page.keyboard`, which every engine has.
// The rows that step and then leave the select without Enter run in the same engines, since each
// engine takes focus out of a select its own way. So do the rows about what the picker does to a
// message on its way to the server, and to an issue closed after it was picked.

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

/** The send the rows below make, and the path of the one `POST` it produces. A comment and a
 *  direct message share this shape, so the row that calls this names the path it expects. */
async function sendAndCapturePath(page: Page, field: Locator): Promise<string> {
  const sent = page.waitForRequest(
    (request) =>
      request.method() === "POST" && /\/(comments|messages)$/.test(new URL(request.url()).pathname)
  );
  await field.fill("Status please");
  await field.press("Control+Enter");
  return new URL((await sent).url()).pathname;
}

/** Holds every `POST` to `pattern` until `release`, as a slow server would, and counts them. */
async function holdPosts(
  page: Page,
  pattern: string
): Promise<{ posts: () => number; release: () => void }> {
  const { promise: held, resolve: release } = Promise.withResolvers<void>();
  let posts = 0;
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    posts += 1;
    await held;
    return route.fallback();
  });
  return { posts: () => posts, release };
}

/** Holds every `POST` to `pattern` until the returned call, then refuses it, as a server that is
 *  down: 503 with a reason the composer shows. */
async function refusePosts(page: Page, pattern: string): Promise<() => void> {
  const { promise: held, resolve: refuse } = Promise.withResolvers<void>();
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    await held;
    return route.fulfill({
      body: JSON.stringify({ code: "UNAVAILABLE", error: "the server is down" }),
      contentType: "application/json",
      status: 503,
    });
  });
  return refuse;
}

test.describe("agents page", () => {
  // The arrows choose and Enter commits, so a reader can pass the first option to reach the
  // second - and the Enter that picks is the picker's, not a newline at the top of the message.
  test("the arrows move the issue selection and Enter commits it", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const picker = row.getByRole("combobox", { name: "Issue" });
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("ArrowDown");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");

      // Escape after an arrow takes nothing: the selection was never committed. (The field is
      // emptied first, since Escape over a written composer is its own discard prompt.)
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowUp");
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-2");
      await expect(row).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A step is not a pick until Enter, so a reader who steps and then leaves the select any other
  // way has picked nothing: the open select goes back to the issue the message is addressed to,
  // and the toggle, the select and the send all name that one issue.
  for (const [way, leave] of [
    ["Tab", (page: Page) => page.keyboard.press("Tab")],
    ["Shift+Tab", (page: Page) => page.keyboard.press("Shift+Tab")],
    ["a click into the message", (_page: Page, field: Locator) => field.click()],
  ] as const) {
    test(`a step left by ${way} is dropped, so the select, the toggle and the send name one issue`, async ({
      browser,
    }) => {
      const issueKey = await seedAgents();
      const context = await asUser(browser, "alice");
      try {
        const page = await context.newPage();
        await openAgents(page);
        const row = page.locator("[data-agent-row]").nth(0);
        const toggle = row.getByRole("button", { name: "Choose issue" });
        const picker = row.getByRole("combobox", { name: "Issue" });
        const field = row.getByRole("textbox", { name: "Comment" });

        await page.keyboard.press("j");
        await page.keyboard.press("i");
        await expect(picker).toBeFocused();
        await page.keyboard.press("ArrowDown");
        await page.keyboard.press("Enter");
        await expect(toggle).toContainText(issueKey);
        await field.fill("");
        await page.keyboard.press("Escape");
        await expect(row).toBeFocused();

        await page.keyboard.press("i");
        await expect(picker).toBeFocused();
        await page.keyboard.press("ArrowDown");
        await expect(picker).not.toHaveValue(issueKey);
        await leave(page, field);
        await expect(picker).not.toBeFocused();
        await expect(picker).toHaveValue(issueKey);
        await expect(toggle).toContainText(issueKey);

        expect(await sendAndCapturePath(page, field)).toBe(`/api/v1/issues/${issueKey}/comments`);
      } finally {
        await context.close();
      }
    });
  }

  // From `No issue` the message is a direct one, so a step left by Tab must not show an issue.
  test("a step from No issue left by Tab is dropped, and the message goes to the agent directly", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await expect(picker).not.toHaveValue("");
      await page.keyboard.press("Tab");
      await expect(picker).not.toBeFocused();
      await expect(picker).toHaveValue("");
      await expect(toggle).toContainText("No issue");

      expect(await sendAndCapturePath(page, field)).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
    } finally {
      await context.close();
    }
  });

  // Type-ahead is a step as the arrows are: the letter moves the selection and Enter commits it.
  test("type-ahead from the keyboard moves the issue selection and Enter commits it", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("c");
      await expect(picker).toHaveValue("CORE-1");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");
    } finally {
      await context.close();
    }
  });

  // A message on its way to the server is addressed already, so its issue cannot change under it:
  // a pick then would remount the composer with the text still in the air, and hand the new
  // instance, enabled, a body the server was about to take - one Ctrl+Enter from sending it twice.
  test("a send in flight holds the picker, and the sent text never comes back", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await expect(picker).toBeFocused();
      await field.click();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");

      await expect(field).toBeDisabled();
      await expect(picker).toBeDisabled();
      await expect(toggle).toBeDisabled();
      // `?` says so as well: from the row, `i` is greyed while the picker it would open is held.
      await row.focus();
      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      await expect(
        help.getByRole("listitem").filter({ hasText: "Pick an issue for the message" })
      ).toHaveAttribute("data-enabled", "false");
      await page.keyboard.press("Escape");
      await expect(help).toHaveCount(0);
      send.release();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      await expect(picker).toBeEnabled();

      // The pick the reader could not make mid-send takes nothing from the message already sent.
      await picker.selectOption("CORE-1");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue("@Planner");
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A reply to a message on an issue is written on that issue, and the send ends the reply, which
  // takes the composer back to the direct channel: the remount that makes must start empty.
  test("a reply sent on an issue leaves the direct composer empty", async ({ browser }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const cancelReply = row.getByRole("button", { name: "Cancel reply" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Reply" }).first().click();
      await expect(cancelReply).toBeVisible();
      await field.fill("Once the build is green");
      const sent = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname === `/api/v1/issues/${issueKey}/messages`
      );
      await field.press("Control+Enter");
      expect((await sent).ok()).toBe(true);

      await expect(cancelReply).toHaveCount(0);
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
      await expect(field).toHaveValue("");
      await expect(field).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // Cancelling the reply while it is in the air changes the channel too; the composer that the
  // change mounts is one the server's answer can still reach only if it waits for that answer.
  test("a reply cancelled mid-send never brings its text back", async ({ browser }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const send = await holdPosts(page, "**/api/v1/issues/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Once the build is green");
      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await row.getByRole("button", { name: "Cancel reply" }).click();
      send.release();

      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
      await expect(field).toBeFocused();
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A reply started while a direct message is in the air belongs to an issue, but the composer
  // holding the message is still the direct one, and its reset after the send seeds what a direct
  // composer seeds - nothing - rather than the issue's mention for the channel it has not moved to.
  test("a reply started mid-send leaves the direct composer as a direct one", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await row.getByRole("button", { name: "Reply" }).first().click();
      send.release();

      // The send ends the reply it outlived, and the composer is the direct one, empty.
      await expect(row.getByRole("button", { name: "Cancel reply" })).toHaveCount(0);
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A send the server refuses after the reader cancelled their reply mid-flight has outlived the
  // composer that sent it: the channel change remounts the composer once the answer is in. The
  // refusal stays with the row, so the remounted composer says so, as the one that sent would
  // have, with the unsent draft where the reader left it.
  test("a send refused after its reply is cancelled mid-flight still says so, beside its draft", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const refuse = await refusePosts(page, "**/api/v1/issues/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Once the build is green");
      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await row.getByRole("button", { name: "Cancel reply" }).click();
      refuse();

      const refusal = row.getByText("Couldn't send — the server is down");
      await expect(refusal).toBeVisible();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("Once the build is green");
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");

      // The notice lasts until the composer sends again: now a direct message, which goes.
      const resent = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname ===
            `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      await field.press("Control+Enter");
      expect((await resent).ok()).toBe(true);
      await expect(refusal).toHaveCount(0);
      await expect(field).toHaveValue("");
    } finally {
      await context.close();
    }
  });

  // The same refusal after a reply was started mid-flight: the composer the reply remounts says
  // the message did not go.
  test("a send refused after a reply is started mid-flight still says so", async ({ browser }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await row.getByRole("button", { name: "Reply" }).first().click();
      refuse();

      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(row.getByRole("button", { name: "Cancel reply" })).toBeVisible();
      await expect(field).toBeEnabled();
    } finally {
      await context.close();
    }
  });

  // The list is every open issue, so an issue closed after it was picked drops out of it on the
  // next read. The select keeps showing the committed issue, marked closed - as the issue header's
  // Status select keeps a closed issue's own status among its options - so what the picker shows,
  // what the toggle names and where the message goes stay one issue until the reader picks
  // another; the server refuses the comment.
  test("an issue closed after the pick stays named in the select and the toggle, marked closed", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      // By id: closing the issue closes the Planner's ask on it, which moves its row below the
      // Reviewer's.
      const row = page.locator(`[data-agent-row="${plannerSession.session_id}"]`);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText(issueKey);

      await patchIssue(issueKey, { status: "done" });
      await toggle.click();
      await expect(picker).toBeVisible();
      // The reader comes back to the tab once the list has gone stale (30 s), which refetches it -
      // the page's own way to learn of the close, since no event names this list.
      await page.evaluate(() => {
        const now = Date.now();
        Date.now = () => now + 31_000;
        window.dispatchEvent(new Event("visibilitychange"));
        window.dispatchEvent(new Event("focus"));
      });
      await expect(picker.locator("option", { hasText: "Rollout plan" })).toHaveCount(1);
      await expect(picker.locator("option", { hasText: "Keyboard issue" })).toHaveCount(0);
      await expect(picker).toHaveValue(issueKey);
      await expect(picker.locator("option:checked")).toHaveText(`${issueKey} (closed)`);
      await expect(toggle).toContainText(`${issueKey} (closed)`);

      expect(await sendAndCapturePath(page, field)).toBe(`/api/v1/issues/${issueKey}/comments`);
      await expect(row.getByText("Couldn't send — issue is closed")).toBeVisible();

      // Picking another issue ends it: the closed one is no longer offered at all.
      await picker.selectOption("CORE-2");
      await expect(toggle).toContainText("CORE-2");
      await toggle.click();
      await expect(picker.locator("option", { hasText: issueKey })).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // An issue comment reaches this agent only by mentioning it, so the reset a send makes seeds the
  // mention again, as a mount does - the next message is as addressed as the first. A direct
  // message already reaches its session, and its channel seeds nothing.
  test("every message sent on an issue mentions the agent, and a direct one carries none", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator(`[data-agent-row="${plannerSession.session_id}"]`);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });
      const sends: { body: unknown; ok: boolean; path: string }[] = [];
      page.on("response", (response) => {
        const request = response.request();
        const path = new URL(request.url()).pathname;
        if (request.method() !== "POST" || !/\/(comments|messages)$/.test(path)) return;
        sends.push({ body: request.postDataJSON(), ok: response.ok(), path });
      });
      const send = async (text: string) => {
        const before = sends.length;
        await field.click();
        await page.keyboard.press("End");
        await page.keyboard.type(text);
        await page.keyboard.press("Control+Enter");
        await expect.poll(() => sends.length).toBe(before + 1);
      };

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText(issueKey);
      await expect(field).toHaveValue("@Planner");

      await send(" first");
      await expect(field).toHaveValue("@Planner");
      await send(" second");
      await expect(field).toHaveValue("@Planner");

      await toggle.click();
      await picker.selectOption("");
      await expect(field).toHaveValue("");
      await send("third");
      await expect(field).toHaveValue("");

      const mentions = [{ target: `session:${plannerSession.session_id}` }];
      expect(sends).toEqual([
        {
          body: { body: "@Planner first", delivery: "steer", mentions },
          ok: true,
          path: `/api/v1/issues/${issueKey}/comments`,
        },
        {
          body: { body: "@Planner second", delivery: "steer", mentions },
          ok: true,
          path: `/api/v1/issues/${issueKey}/comments`,
        },
        {
          body: { body: "third", delivery: "steer" },
          ok: true,
          path: `/api/v1/agents/${plannerSession.session_id}/messages`,
        },
      ]);
    } finally {
      await context.close();
    }
  });
});

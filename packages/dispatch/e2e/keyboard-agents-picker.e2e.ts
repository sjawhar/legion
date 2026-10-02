import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  agentRow,
  holdPosts,
  openAgents,
  plannerSession,
  refusePosts,
  seedAgents,
  setLiveSessions,
  shownAgentRows,
} from "./agents";
import { createAgentMessage, createMessage, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The issue picker's keyboard-step rule (`markKeyStep` in `AgentMessageComposer.tsx`) holds only
// because every engine dispatches a closed select's `change` inside the key's own task, where the
// HTML spec queues it as a task of its own. These rows notice an engine that moves to the spec's
// queued task: its first arrow or letter would commit at once. So the `webkit` and `firefox`
// projects run this spec as well as Chromium, and its rows drive keys through `page.keyboard`,
// which every engine has. The rows that step and then leave the select without Enter run in the
// same engines, since each engine takes focus out of a select its own way. So do the rows about
// what the picker and Reply do with a message on its way to the server, and about an issue closed
// after it was picked.

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

test.describe("agents page", () => {
  // The arrows choose and Enter commits, so a reader can pass the first option to reach the
  // second - and the Enter that picks is the picker's, not a newline at the top of the message.
  test("the arrows move the issue selection and Enter commits it", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
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
        const row = shownAgentRows(page).nth(0);
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
      const row = shownAgentRows(page).nth(0);
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
      const row = shownAgentRows(page).nth(0);
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

  // A message on its way to the server is addressed already, so its issue cannot change under it,
  // and the text the server is taking must never come back to the composer, enabled - one
  // Ctrl+Enter from sending it twice.
  test("a send in flight holds the picker, and the sent text never comes back", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
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
  // takes the composer back to the direct channel: it must come back empty.
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
      const row = shownAgentRows(page).nth(0);
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

  // Ending the reply while it is in the air would move the message to another channel under the
  // send, so Cancel reply holds until the server answers, as Reply and the picker do. The send
  // then ends the reply itself, and the text it took does not come back.
  test("Cancel reply holds while a reply is out, and the sent text never comes back", async ({
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
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const send = await holdPosts(page, "**/api/v1/issues/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Once the build is green");
      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(row.getByRole("button", { name: "Cancel reply" })).toBeDisabled();
      send.release();

      await expect(row.getByRole("button", { name: "Cancel reply" })).toHaveCount(0);
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
      await expect(field).toBeFocused();
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // The reply a refused send was for stays until the reader ends it. Ended then, the message goes
  // back to the issue picked and the draft takes that issue's mention, so Retry reaches the agent
  // there - a direct reply's refusal retried as an issue comment that still tells the agent.
  test("a reply refused and then cancelled retries on the issue, mentioning the agent", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createAgentMessage(plannerSession.session_id, {
      body: "Are you free?",
      delivery: "btw",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const cancelReply = row.getByRole("button", { name: "Cancel reply" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      // The picker takes focus once its list lands; the arrows are for it, not the row.
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(field).toHaveValue("@Planner");
      // A reply to a direct exchange is a direct message, whatever issue is picked.
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Yes");
      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(cancelReply).toBeDisabled();
      refuse();

      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toHaveValue("Yes");
      await cancelReply.click();
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText(issueKey);
      await expect(field).toHaveValue("@Planner Yes");
      const retried = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/issues/${issueKey}/comments`
      );
      await row.getByRole("button", { name: "Retry" }).click();
      expect((await retried).postDataJSON()).toEqual({
        body: "@Planner Yes",
        delivery: "steer",
        mentions: [{ target: `session:${plannerSession.session_id}` }],
      });
    } finally {
      await context.close();
    }
  });

  // A message on its way is addressed, so Reply holds until the server answers, as the picker
  // does. A refusal hands back the draft that was sent, and Retry sends exactly that again.
  test("Reply holds while a send is out, and a refusal's Retry posts exactly what was refused", async ({
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
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const reply = row.getByRole("button", { name: "Reply" }).first();
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(reply).toBeDisabled();
      refuse();

      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("Status please");
      await expect(reply).toBeEnabled();
      const retried = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      await row.getByRole("button", { name: "Retry" }).click();
      expect((await retried).postDataJSON()).toEqual({ body: "Status please", delivery: "steer" });
    } finally {
      await context.close();
    }
  });

  // A direct send owns its address in the same task that begins it. A Reply click before React
  // re-renders must not move that in-flight request onto this issue's message route.
  test("a same-task Reply cannot re-address a direct send", async ({ browser }) => {
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
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          /\/api\/v1\/(agents\/[^/]+|issues\/[^/]+)\/messages$/.test(
            new URL(request.url()).pathname
          )
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await field.fill("Status please");
      await row.evaluate((node) => {
        const send = node.querySelector<HTMLButtonElement>('button[type="submit"]');
        const reply = node.querySelector<HTMLButtonElement>('button[aria-label="Reply"]');
        if (send === null || reply === null) throw new Error("expected send and Reply controls");
        send.click();
        reply.click();
      });

      expect(new URL((await sent).url()).pathname).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      refuse();
      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toHaveValue("Status please");
      await expect(row.getByRole("button", { name: "Cancel reply" })).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // An already-open picker can commit an issue in the same task as Send. Its commit must not
  // turn the direct send into an issue message before the request takes its owner.
  test("a same-task issue pick cannot re-address a direct send", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          /\/api\/v1\/(agents\/[^/]+|issues\/[^/]+)\/messages$/.test(
            new URL(request.url()).pathname
          )
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await toggle.click();
      await expect(picker).toBeVisible();
      await field.fill("Status please");
      await row.evaluate((node) => {
        const send = node.querySelector<HTMLButtonElement>('button[type="submit"]');
        const picker = node.querySelector<HTMLSelectElement>('select[aria-label="Issue"]');
        if (send === null || picker === null)
          throw new Error("expected send and issue picker controls");
        send.click();
        picker.value = "CORE-1";
        picker.dispatchEvent(new Event("change", { bubbles: true }));
      });

      expect(new URL((await sent).url()).pathname).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      refuse();
      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toHaveValue("Status please");
      await expect(toggle).toContainText("No issue");
    } finally {
      await context.close();
    }
  });

  // An upload carries its target from the paste that starts it, as a send carries its request: a
  // pick in the paste's own task, before React re-renders, moves where the next message goes but
  // not where this file goes, and the reference the upload adds names the issue that holds it.
  test("a same-task issue pick cannot move an upload", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const uploaded = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          /\/api\/v1\/issues\/[^/]+\/artifacts$/.test(new URL(request.url()).pathname)
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await picker.selectOption("CORE-1");
      await expect(field).toHaveValue("@Planner");
      await toggle.click();
      await expect(picker).toBeVisible();
      await row.evaluate((node) => {
        const textarea = node.querySelector<HTMLTextAreaElement>('textarea[aria-label="Comment"]');
        const select = node.querySelector<HTMLSelectElement>('select[aria-label="Issue"]');
        if (textarea === null || select === null)
          throw new Error("expected the composer and the issue picker");
        const data = new DataTransfer();
        data.items.add(new File(["# Notes\n"], "notes.md", { type: "text/markdown" }));
        const paste = new Event("paste", { bubbles: true, cancelable: true });
        Object.defineProperty(paste, "clipboardData", { value: data });
        textarea.dispatchEvent(paste);
        select.value = "CORE-2";
        select.dispatchEvent(new Event("change", { bubbles: true }));
      });

      expect(new URL((await uploaded).url()).pathname).toBe("/api/v1/issues/CORE-1/artifacts");
      await expect(field).toHaveValue("@Planner dispatch://CORE-1/artifact/notes-md");
      await expect(toggle).toContainText("CORE-2");
    } finally {
      await context.close();
    }
  });

  // Cancel reply is the same address mutation in reverse. The send retains its direct reply
  // target until its refusal arrives, even when the click shares Send's task.
  test("a same-task Cancel reply keeps the direct reply it sent", async ({ browser }) => {
    await seedAgents();
    await createAgentMessage(plannerSession.session_id, {
      body: "Are you free?",
      delivery: "btw",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/agents/${plannerSession.session_id}/messages`
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Yes");
      await row.evaluate((node) => {
        const send = node.querySelector<HTMLButtonElement>('button[type="submit"]');
        const cancel = node.querySelector<HTMLButtonElement>('button[aria-label="Cancel reply"]');
        if (send === null || cancel === null)
          throw new Error("expected send and Cancel reply controls");
        send.click();
        cancel.click();
      });

      expect(new URL((await sent).url()).pathname).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      refuse();
      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toHaveValue("Yes");
      await expect(row.getByRole("button", { name: "Cancel reply" })).toBeVisible();
    } finally {
      await context.close();
    }
  });

  // A refusal is about the draft it turned down. Retry sends the draft as it stands, so it is
  // offered only when Send would be; Discard drops the draft, and the notice goes with it.
  test("Retry asks what Send asks, and Discard takes a refusal's notice with the draft", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const notice = row.getByText("Couldn't send — the server is down");
      const retry = row.getByRole("button", { name: "Retry" });
      let posts = 0;
      page.on("request", (request) => {
        if (request.method() === "POST" && new URL(request.url()).pathname.endsWith("/messages")) {
          posts += 1;
        }
      });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      refuse();
      await expect(notice).toBeVisible();
      await expect(retry).toBeVisible();

      // Nothing Send would send, so nothing Retry may: the button goes, the notice stays.
      await field.fill("");
      await expect(retry).toHaveCount(0);
      await expect(notice).toBeVisible();

      await field.fill("Status please");
      await field.press("Escape");
      await row.getByRole("button", { name: "Discard" }).click();
      await expect(notice).toHaveCount(0);
      await expect(retry).toHaveCount(0);
      await expect(field).toHaveValue("");
      expect(posts).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A message on its way is the server's until it answers. A `Discard draft?` the reader raised
  // before sending goes with the send, and the field holds, so nothing they discard or type in the
  // meantime can be overwritten by the outcome: the refusal hands back the draft that was sent,
  // with its notice and Retry.
  test("a send takes the Discard prompt with it, and a refusal hands back the draft with its notice", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const discard = row.getByRole("button", { name: "Discard" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Escape");
      await expect(discard).toBeVisible();
      await expect(field).toBeFocused();
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(discard).toHaveCount(0);
      refuse();

      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("Status please");
      await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
      await expect(discard).toHaveCount(0);
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
      const row = agentRow(page, plannerSession.session_id);
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
      const row = agentRow(page, plannerSession.session_id);
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

  // `i` opens the picker; it never shuts one. A row keeps its picker open when it collapses, so on
  // that row `i` goes back into the select rather than clicking the toggle closed.
  test("i on a collapsed row goes back into the picker it left open", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const opener = row.getByRole("button", { exact: true, name: "Planner" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await opener.click();
      await expect(opener).toHaveAttribute("aria-expanded", "false");
      // Back on the row itself - a click beside its controls - for the key.
      await row.getByText("box-1 · /srv/planner").click();
      await expect(row).toBeFocused();
      await page.keyboard.press("i");

      await expect(opener).toHaveAttribute("aria-expanded", "true");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A message on its way holds the picker and Reply of the row that sent it. A reader who leaves
  // the page mid-send and comes back gets a new row, with a composer of its own and no send of its
  // own out, so it holds nothing: the send it lost goes on without it.
  test("a row back after the reader left mid-send holds nothing for the send it lost", async ({
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
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const reply = row.getByRole("button", { name: "Reply" }).first();
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(toggle).toBeDisabled();
      await expect(reply).toBeDisabled();

      // Away to the Inbox and back, by the global keys, so the app - and the send - stay loaded.
      await page.locator("body").focus();
      await page.keyboard.press("g");
      await page.keyboard.press("i");
      await expect(row).toHaveCount(0);
      await page.keyboard.press("g");
      await page.keyboard.press("a");
      await row.getByRole("button", { exact: true, name: "Planner" }).click();

      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      await expect(toggle).toBeEnabled();
      await expect(reply).toBeEnabled();
      send.release();
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });
});

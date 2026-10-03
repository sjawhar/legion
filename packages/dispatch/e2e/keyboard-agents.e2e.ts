import { expect, type Page, test } from "@playwright/test";

import { agentRow, openAgents, plannerSession, seedAgents, shownAgentRows } from "./agents";
import { createMessage } from "./api";
import { resetDatabase } from "./seed";
import { holdPosts, pasteFile, refusePosts } from "./sends";
import { asUser } from "./users";

/** Resolves once every timer queued before it has run: the next task, the soonest a reader's
 *  pick can follow a key. The issue picker's keyboard-step mark lasts until then, so a row that
 *  presses a key on the select and then picks has to be this far along before it picks. */
async function nextTask(page: Page): Promise<void> {
  await page.evaluate(() => {
    const turn = Promise.withResolvers<void>();
    setTimeout(turn.resolve, 0);
    return turn.promise;
  });
}

test.beforeEach(async () => {
  await resetDatabase();
});
test.describe("agents page", () => {
  test("j/k rove the agent rows, Enter opens the composer, i the issue picker, x selects and Shift+P pins", async ({
    browser,
  }, testInfo) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      await expect(rows).toHaveCount(2);
      const planner = rows.nth(0);
      const reviewer = rows.nth(1);
      const plannerToggle = planner.getByRole("button", { exact: true, name: "Planner" });

      await page.keyboard.press("j");
      await expect(planner).toBeFocused();
      await page.keyboard.press("j");
      await expect(reviewer).toBeFocused();
      await page.keyboard.press("k");
      await expect(planner).toBeFocused();
      await expect(plannerToggle).toHaveAttribute("aria-expanded", "false");

      // Enter expands the collapsed row and hands focus to its composer; letters then type.
      await page.keyboard.press("Enter");
      const composer = planner.getByRole("textbox", { name: "Comment" });
      await expect(plannerToggle).toHaveAttribute("aria-expanded", "true");
      await expect(composer).toBeFocused();
      await page.keyboard.type("j");
      await expect(composer).toHaveValue("j");
      await expect(reviewer).not.toBeFocused();
      const agentsShot = testInfo.outputPath(`agents-keyboard-${testInfo.project.name}.png`);
      await page.screenshot({ fullPage: true, path: agentsShot });
      await testInfo.attach(`agents keyboard (${testInfo.project.name})`, {
        contentType: "image/png",
        path: agentsShot,
      });

      // Escape out of an untouched composer is one level out: back to the row it belongs to.
      await composer.fill("");
      await page.keyboard.press("Escape");
      await expect(planner).toBeFocused();

      await page.keyboard.press("x");
      await expect(
        planner.getByRole("checkbox", { name: "Select Planner for broadcast" })
      ).toBeChecked();

      // i expands a collapsed row too, and opens its issue picker.
      await page.keyboard.press("j");
      await expect(reviewer).toBeFocused();
      const reviewerPicker = reviewer.getByRole("button", { name: "Choose issue" });
      await page.keyboard.press("i");
      await expect(reviewer.getByRole("button", { exact: true, name: "Reviewer" })).toHaveAttribute(
        "aria-expanded",
        "true"
      );
      await expect(reviewerPicker).toHaveAttribute("aria-expanded", "true");
      // `i` leaves the reader in the picker's own select, where the letters are its type-ahead
      // and not the page's keys - Escape is the way back to the row, closing it behind them.
      await expect(reviewer.getByRole("combobox", { name: "Issue" })).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(reviewerPicker).toHaveAttribute("aria-expanded", "false");
      await expect(reviewer).toBeFocused();

      // Shift+P pins the focused agent, and a pin is the viewer's own remembered preference.
      await page.keyboard.press("k");
      await expect(planner).toBeFocused();
      await page.keyboard.press("Shift+P");
      await expect(planner.getByRole("button", { name: "Unpin Planner" })).toBeVisible();
      await page.reload();
      await expect(
        shownAgentRows(page).nth(0).getByRole("button", { name: "Unpin Planner" })
      ).toBeVisible();

      // `?` lists the new scope: a scope missing from `SCOPE_ORDER` is dropped silently.
      await page.locator("body").focus();
      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      const agentsSection = help.getByRole("region", { name: "Agents" });
      await expect(agentsSection).toBeVisible();
      await expect(
        agentsSection.getByRole("listitem").filter({ hasText: "Pin or unpin the focused agent" })
      ).toBeVisible();
      // Escape out of a row's composer is real behaviour, so `?` says so: the Inbox, Board and
      // Architecture all list theirs, and a help screen that omits one is a help screen a reader
      // cannot trust.
      await expect(
        agentsSection.getByRole("listitem").filter({ hasText: "Back to the agent row" })
      ).toBeVisible();
    } finally {
      await context.close();
    }
  });

  // `i` has to leave the reader inside what it opened: the picker's `<select>` is where the
  // arrows choose an issue and where Escape is the control's own way out. Left on the row, the
  // key would open something no keystroke could then use or close.
  test("i lands in the issue picker, and Escape there closes it and returns to the row", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const toggle = row.getByRole("button", { name: "Choose issue" });
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      const picker = row.getByRole("combobox", { name: "Issue" });
      await expect(picker).toBeFocused();

      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(picker).toHaveCount(0);
      await expect(row).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A pick is what `i` opened the picker for, so committing one hands the reader back to the
  // message it belongs to rather than dropping them on the document as the select unmounts.
  test("choosing an issue closes the picker and leaves focus in the composer", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const toggle = row.getByRole("button", { name: "Choose issue" });
      await expect(row.getByRole("combobox", { name: "Issue" })).toBeFocused();

      // The arrows move the selection; Enter is the pick.
      await page.keyboard.press("ArrowDown");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-1");
      await expect(row.getByRole("textbox", { name: "Comment" })).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // The select renders only once the issue list lands, so the whole latency of that read is a
  // window in which the reader is somewhere else. Focus is the picker's to take only if they
  // are still where the open left them.
  test("a slow issue list never pulls the reader off the row they moved to", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/issues?*", async (route) => {
        await held;
        return route.fallback();
      });
      await openAgents(page);
      const rows = shownAgentRows(page);

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await page.keyboard.press("j");
      await expect(rows.nth(1)).toBeFocused();

      release?.();
      await expect(rows.nth(0).getByRole("combobox", { name: "Issue" })).toBeVisible();
      await expect(rows.nth(1)).toBeFocused();

      // And the reader's next key is still a rove, not a write on the row they left.
      await page.keyboard.press("k");
      await expect(rows.nth(0)).toBeFocused();
      await expect(rows.nth(0).getByRole("button", { name: "Choose issue" })).toContainText(
        "No issue"
      );
    } finally {
      await context.close();
    }
  });

  // A typed message is the reader's work: choosing which issue it belongs to must not throw it
  // away, by either hand. The pick seeds the agent mention an issue-owned comment needs into the
  // draft the reader already has.
  test("a draft survives the issue pick, by keyboard and by pointer", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("please look at the migration");

      // The picker is reached with the pointer while a draft is open - Escape out of a written
      // composer is its own discard prompt, which is not this path - and the pick itself is made
      // from the keyboard the open hands over to.
      await toggle.click();
      await expect(row.getByRole("combobox", { name: "Issue" })).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue("@Planner please look at the migration");
      await expect(field).toBeFocused();

      // And with the pointer all the way: back to no issue, where the mention the channel
      // seeded goes with the channel and the reader's own words stay exactly as typed.
      await toggle.click();
      await row.getByRole("combobox", { name: "Issue" }).selectOption("");
      await expect(toggle).toContainText("No issue");
      await expect(field).toHaveValue("please look at the migration");
    } finally {
      await context.close();
    }
  });

  // A file goes to the issue the message was addressed to when the upload started, and its
  // reference names that issue's artifact, even when the reader picks another issue while the
  // file is in the air.
  test("an upload's reference names the issue it went to, not one picked while it was out", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const upload = await holdPosts(page, "**/api/v1/issues/*/artifacts");
      const row = agentRow(page, plannerSession.session_id);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await picker.selectOption("CORE-1");
      await expect(field).toHaveValue("@Planner");
      await pasteFile(field, "notes.md", "# Notes\n");
      await expect(row.getByRole("button", { name: "Uploading file…" })).toBeDisabled();
      await toggle.click();
      await picker.selectOption("CORE-2");
      await expect(toggle).toContainText("CORE-2");
      upload.release();

      await expect(field).toHaveValue("@Planner dispatch://CORE-1/artifact/notes-md");
      expect(upload.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A message on its way is the server's until it answers, and a refusal hands back exactly the
  // draft that was sent. An upload's Retry pressed in the meantime would append its reference to
  // the field for that refusal to drop, leaving the artifact on the issue with nothing pointing
  // at it, so the Retry waits for the answer, as the rest of the composer does.
  test("an upload's Retry waits while a send is out, and its reference joins the draft handed back", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      let uploads = 0;
      // The first upload fails, so its Retry is on screen when the message goes.
      await page.route("**/api/v1/issues/*/artifacts", (route) => {
        if (route.request().method() !== "POST") return route.fallback();
        uploads += 1;
        if (uploads > 1) return route.fallback();
        return route.fulfill({
          body: JSON.stringify({ code: "UNAVAILABLE", error: "storage is down" }),
          contentType: "application/json",
          status: 503,
        });
      });
      const refuse = await refusePosts(page, "**/api/v1/issues/*/comments");
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const uploadRetry = row
        .getByRole("alert")
        .filter({ hasText: "storage is down" })
        .getByRole("button", { name: "Retry" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Choose issue" }).click();
      await row.getByRole("combobox", { name: "Issue" }).selectOption("CORE-1");
      await expect(field).toHaveValue("@Planner");
      await field.press("End");
      await page.keyboard.type(" see attached");
      await pasteFile(field, "notes.md", "# Notes\n");
      await expect(uploadRetry).toBeVisible();

      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(uploadRetry).toBeDisabled();
      refuse();

      await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
      await expect(field).toHaveValue("@Planner see attached");
      await uploadRetry.click();
      await expect(field).toHaveValue("@Planner see attached dispatch://CORE-1/artifact/notes-md");
      expect(uploads).toBe(2);
    } finally {
      await context.close();
    }
  });

  // The same slow read, but the reader has gone into the composer rather than to another row:
  // the picker's focus belongs to the open, and typing is not where the open left them.
  test("a slow issue list never takes focus out of the message being typed", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/issues?*", async (route) => {
        await held;
        return route.fallback();
      });
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("still typing");

      release?.();
      await expect(row.getByRole("combobox", { name: "Issue" })).toBeVisible();
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("still typing");
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
    } finally {
      await context.close();
    }
  });

  // What `i` opens, Escape closes - including when the list never arrives. A failed or slow read
  // renders no select, so focus stays on the row, which is exactly where the key has to work.
  test("Escape closes the issue picker from the row when the list fails or is still out", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/issues?*", async (route) => {
        await held;
        return route.fulfill({
          body: JSON.stringify({ code: "INTERNAL", error: "issue store unavailable" }),
          contentType: "application/json",
          status: 500,
        });
      });
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });

      // While the read is still out: no select, focus on the row, Escape closes what it opened.
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(row).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(row).toBeFocused();

      // And once it has failed: the picker says so, and Escape still leaves.
      release?.();
      await page.keyboard.press("i");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(row.getByText(/Could not load issues/i)).toBeVisible();
      await expect(row).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(row).toBeFocused();

      // Escape on a row with nothing open is the row's own key, as before: it leaves the row.
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
    } finally {
      await context.close();
    }
  });

  // Enter commits whether or not it changes the issue, and the select unmounts either way. A
  // commit that left the reader on the document would hand the message they picked the issue
  // for to the page's shortcuts: `j` roves to a row and `x` then ticks its broadcast box.
  test("Enter on the selection already showing still hands the reader to the composer", async ({
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
      const broadcast = row.getByRole("checkbox", { name: "Select Planner for broadcast" });

      // "No issue", the selection the picker opens on, confirmed as it stands.
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(field).toBeFocused();
      await page.keyboard.type("jx");
      await expect(field).toHaveValue("jx");
      await expect(broadcast).not.toBeChecked();

      // The control: a pick that does change the issue lands in the composer too. (The field is
      // emptied first, since Escape over a written composer is its own discard prompt.)
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");

      // That issue re-confirmed: nothing changes, and the reader is still handed on.
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await page.keyboard.type("jx");
      await expect(field).toHaveValue("jx");
      await expect(broadcast).not.toBeChecked();
    } finally {
      await context.close();
    }
  });

  // A key pressed in the last open must not make this one's pointer pick wait for an Enter: the
  // mark a key leaves lasts one task, so nothing of it is left by the time the picker reopens.
  test("a pointer pick commits at once after an arrow and Escape in the last open", async ({
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
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(row).toBeFocused();

      // Reopened, and picked the way a pointer's choice arrives from the native popup: a change
      // with no key and no press on the select in front of it.
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await nextTask(page);
      await picker.selectOption("CORE-2");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");
    } finally {
      await context.close();
    }
  });

  // A key that opens the select's native popup - `Alt+ArrowDown` here, the arrows on macOS -
  // steps nothing, and the pick the reader then makes in that popup arrives in a later task.
  // That is a pick made, as a pointer's is, and it commits with no second Enter.
  test("a pick from the popup a key opened commits at once", async ({ browser }) => {
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
      await page.keyboard.press("Alt+ArrowDown");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await nextTask(page);
      await picker.selectOption("CORE-2");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");
    } finally {
      await context.close();
    }
  });

  // Only a change the key itself made is a step. A key that moved nothing - `ArrowUp` on the
  // first option - leaves the next change, a task later, a pick.
  test("a key that moves nothing leaves the next change a pick", async ({ browser }) => {
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
      await page.keyboard.press("ArrowUp");
      await expect(picker).toHaveValue("");
      await nextTask(page);
      await picker.selectOption("CORE-1");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");
    } finally {
      await context.close();
    }
  });

  // Type-ahead steps the selection from the key's `keypress`, not its `keydown`, and is a step
  // like the arrows: the picker stays open until Enter. A keyboard reaches the page as two input
  // events, `rawKeyDown` then `char`, and Playwright's `press` sends both in one; this row sends
  // them apart, with a task between, so the `keypress` is judged in a task of its own.
  test("type-ahead moves the issue selection and Enter commits it", async ({ browser }) => {
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
      const cdp = await context.newCDPSession(page);
      const key = { code: "KeyC", key: "c", windowsVirtualKeyCode: 67 };
      await cdp.send("Input.dispatchKeyEvent", { ...key, type: "rawKeyDown" });
      await nextTask(page);
      await cdp.send("Input.dispatchKeyEvent", { ...key, text: "c", type: "char" });
      await cdp.send("Input.dispatchKeyEvent", { ...key, type: "keyUp" });
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

  // The round trip Rev drove: the mention has to reach the wire, not just the screen, or the
  // agent is never told about the comment that names them.
  test("a comment sent after a round trip through the picker still mentions the agent", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const pick = async (value: string) => {
        await toggle.click();
        await row.getByRole("combobox", { name: "Issue" }).selectOption(value);
      };

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("hello");

      await pick("CORE-1");
      await pick("");
      await pick("CORE-1");
      // The reader typed ahead of the mention, which is where the caret lands after a pick.
      await field.click();
      await page.keyboard.press("Home");
      await page.keyboard.type("now ");
      await expect(field).toHaveValue("now @Planner hello");

      const posted = page.waitForRequest(
        (request) => request.method() === "POST" && request.url().includes("/comments")
      );
      await page.keyboard.press("Control+Enter");
      const body = (await posted).postDataJSON();
      expect(body.mentions).toEqual([{ target: `session:${plannerSession.session_id}` }]);
      expect(body.delivery).toBeTruthy();
      expect(body.body.match(/@Planner/g)).toHaveLength(1);
    } finally {
      await context.close();
    }
  });

  // A direct message already reaches the session it is addressed to, so the mention the issue
  // channel seeded goes with that channel - and a mention the reader accepted themselves does
  // not: it survives the trip and is on the wire when they come back.
  test("the channel's own mention leaves with it, and the reader's own survives", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const pick = async (value: string) => {
        await toggle.click();
        await row.getByRole("combobox", { name: "Issue" }).selectOption(value);
      };

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("who owns this");
      await pick("CORE-1");
      await expect(field).toHaveValue("@Planner who owns this");

      // The reader mentions the reviewer themselves, inside the issue channel.
      await field.click();
      await page.keyboard.press("End");
      await page.keyboard.type(" @Rev");
      await page
        .getByRole("listbox", { name: "Mention suggestions" })
        .getByRole("option", { exact: true, name: "Reviewer" })
        .first()
        .click();
      await expect(field).toHaveValue("@Planner who owns this @Reviewer");

      // Into the direct channel: the channel's own mention goes, theirs stays.
      await pick("");
      await expect(field).toHaveValue("who owns this @Reviewer");
      const sentMessage = page.waitForRequest(
        (request) => request.method() === "POST" && request.url().includes("/messages")
      );
      await field.click();
      await page.keyboard.press("Control+Enter");
      expect((await sentMessage).postDataJSON().body).toBe("who owns this @Reviewer");
    } finally {
      await context.close();
    }
  });

  // "Discard draft?" means the draft is gone. On this page `onClose` only moves focus, so a
  // composer that kept the text would hand it to the next channel on the next pick.
  test("Discard clears the draft, and no pick brings it back", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("discard me");
      await page.keyboard.press("Escape");
      await row.getByRole("button", { name: "Discard" }).click();
      await expect(field).toHaveValue("");

      await toggle.click();
      await row.getByRole("combobox", { name: "Issue" }).selectOption("CORE-1");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue("@Planner");
    } finally {
      await context.close();
    }
  });

  // The composer calls `onClose` after a successful send as well as on the way out, and only the
  // way out is "one level up". Focus on the row after a send would turn the next letters typed
  // into agent shortcuts.
  test("a sent message leaves focus in the composer, not on the row", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await expect(row).toBeFocused();
      await page.keyboard.press("Enter");
      const composer = row.getByRole("textbox", { name: "Comment" });
      await expect(composer).toBeFocused();

      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      // The composer clears only once the server has taken the message.
      await expect(composer).toHaveValue("");
      await expect(composer).toBeFocused();
      await page.keyboard.type("j");
      await expect(composer).toHaveValue("j");
      await expect(row).not.toBeFocused();
    } finally {
      await context.close();
    }
  });

  // The disable takes focus to the document; only that focus is the composer's to take back. A
  // reader who moved on during the round trip keeps where they went.
  test("a send in flight does not steal back focus the reader has moved", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      const planner = rows.nth(0);
      const reviewer = rows.nth(1);

      // Hold the send open so the reader has a window to click in, as a slow server would.
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      const composer = planner.getByRole("textbox", { name: "Comment" });
      await expect(composer).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");

      // Mid-send the reader ticks the other agent's box; that focus is theirs to keep.
      const reviewerBox = reviewer.getByRole("checkbox", { name: "Select Reviewer for broadcast" });
      await reviewerBox.focus();
      await expect(reviewerBox).toBeFocused();
      send.release();
      await expect(composer).toHaveValue("");
      await expect(reviewerBox).toBeFocused();
      await expect(composer).not.toBeFocused();
    } finally {
      await context.close();
    }
  });

  // `i` clicks a button that only renders while the row is not replying to a message, so it is
  // advertised only when that button is reachable - the rule this slice already applied to the
  // Inbox's own snooze.
  test("i is listed as unavailable while the row is answering a message", async ({ browser }) => {
    const issueKey = await seedAgents();
    const root = await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    expect(root.id).toBeTruthy();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(row.getByRole("textbox", { name: "Comment" })).toBeFocused();
      await expect(row.getByRole("button", { name: "Choose issue" })).toBeVisible();

      await row.getByRole("button", { name: "Reply" }).first().click();
      await expect(row.getByRole("button", { name: "Choose issue" })).toHaveCount(0);
      await row.focus();

      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      await expect(
        help.getByRole("listitem").filter({ hasText: "Pick an issue for the message" })
      ).toHaveAttribute("data-enabled", "false");
    } finally {
      await context.close();
    }
  });
});

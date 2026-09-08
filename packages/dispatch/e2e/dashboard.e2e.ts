import { expect, test } from "./fixtures";
import { answerComment, askComment, REPO, SESSION_ID } from "./threads";

const THREAD = `#${REPO}/12`;
const SUBJECT = `notifications.github.${REPO.replace("/", ".")}.issue`;
const COMMENT_EVENT = { subject: `${SUBJECT}.12.comment`, repo: REPO };

test("no marker plumbing is visible, and a legacy thread still renders", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const detail = page.locator("#detail-root");
  await expect(detail.locator("h1")).toHaveText("E2E harness: which identity submits test PRs?");
  const text = await detail.innerText();
  expect(text).not.toContain("dispatch:");
  expect(text).not.toContain("requestId");
  expect(text).not.toContain("---");
  await page.locator('.thread-row[data-thread-number="7"]').click();
  await expect(detail.locator("h1")).toHaveText("Pick a color");
  await expect(detail.locator("#detail-opening")).toContainText("Legacy thread.");
  await expect(detail.locator('.ask-history[data-ask-id="L7"] .answer-pill')).toHaveText("blue");
  await expect(detail.locator("#detail-ask-forms form")).toHaveCount(0);
});

test("one form per open ask, answers beneath their questions, needs-you in the sidebar", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const forms = page.locator("#detail-ask-forms form.ask-form");
  // The follow-up is the latest turn: its two asks take answers; the body's
  // unanswered Rollout ask was restated there and reads as superseded.
  await expect(forms).toHaveCount(2);
  await expect(forms.nth(0)).toHaveAttribute("data-ask-id", "F1");
  await expect(forms.nth(1)).toHaveAttribute("data-ask-id", "F1.1");
  await expect(
    page.locator('#detail-opening-asks .ask-history[data-ask-id="R12"] .answer-pill')
  ).toHaveText("Env var");
  await expect(
    page.locator('#detail-opening-asks .ask-history[data-ask-id="R12.1"] .ask-superseded')
  ).toHaveAttribute("href", "#turn-103");
  await expect(page.locator("#turn-103")).toContainText(
    "The bot cannot be told apart from real submitters."
  );
  await expect(page.locator('#turn-103 .ask-history[data-ask-id="F1"] .ask-waiting')).toBeVisible();
  await expect(page.locator('.thread-row[data-thread-number="12"] .state-needs-you')).toHaveText(
    "needs you"
  );
  await expect(page.locator('.thread-row[data-thread-number="7"] .state-needs-you')).toHaveCount(0);

  await page.locator('form[data-ask-id="F1"] input[value="E2E_SUBMITTER_EMAIL"]').check();
  await page.locator('form[data-ask-id="F1"] button[type=submit]').click();
  await expect(page.locator('form[data-ask-id="F1"]')).toHaveCount(0);
  await expect(page.locator('#turn-103 .ask-history[data-ask-id="F1"] .answer-pill')).toHaveText(
    "E2E_SUBMITTER_EMAIL"
  );
  const answer = dashboard.posted.at(-1);
  expect(answer?.body.startsWith("<!-- dispatch:answer\n")).toBe(true);
  expect(answer?.body).toContain('forAsk: "F1"');
  expect(answer?.body).toContain("**Variable** — Which variable name?\nE2E_SUBMITTER_EMAIL");
  expect(answer?.body).not.toContain("---");
});

test("typed text, form state, and search focus survive GitHub events", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const reply = page.locator("#reply-body");
  await reply.fill("draft text that must survive");
  await reply.evaluate<void, undefined, HTMLTextAreaElement>((el) => el.setSelectionRange(6, 10));

  dashboard.addComment(REPO, 12, {
    body: "another human reply",
    author: "sami",
    createdAt: new Date().toISOString(),
  });
  await dashboard.emit(COMMENT_EVENT);
  await expect(page.locator("#detail-conversation")).toContainText("another human reply");

  await expect(reply).toHaveValue("draft text that must survive");
  expect(
    await reply.evaluate<[number, number], undefined, HTMLTextAreaElement>((el) => [
      el.selectionStart,
      el.selectionEnd,
    ])
  ).toEqual([6, 10]);

  // The event names the one visible thread, so its highlight is the sidebar
  // paint that would destroy the search box if paints rebuilt the sidebar; the
  // highlight clearing is the timed repaint after the refetch.
  const search = page.locator("#search-input");
  await search.click();
  await search.pressSequentially("harn");
  await expect(page.locator(".thread-row")).toHaveCount(1);
  await dashboard.emit({ subject: `${SUBJECT}.12`, repo: REPO });
  const highlighted = page.locator('.thread-row[data-thread-number="12"].live-highlight');
  await expect(highlighted).toHaveCount(1);
  await expect(search).toBeFocused();
  await expect(search).toHaveValue("harn");
  await expect(highlighted).toHaveCount(0, { timeout: 5_000 });
  await expect(page.locator(".thread-row")).toHaveCount(1);
  await expect(search).toBeFocused();
  await expect(search).toHaveValue("harn");
});

test("an event answering one ask removes only its form; a half-filled sibling keeps value and focus", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const forms = page.locator("#detail-ask-forms form.ask-form");
  await expect(forms).toHaveCount(2);

  // A follow-up whose ask ids contain backslashes, created through the event
  // path. A selector that escaped only quotes never finds the `F2\` form and
  // re-creates it on every paint; for `F2\"` it throws inside querySelector
  // and aborts the paint. CSS.escape in the DOM layer covers both.
  dashboard.addComment(REPO, 12, {
    body: askComment("F2", [
      { askId: "F2\\", question: "Pin the browser build?", options: ["Yes", "No"] },
      { askId: 'F2\\"', question: "Cache it in CI?", options: ["Yes", "No"] },
    ]),
    author: "agent",
    createdAt: new Date().toISOString(),
  });
  await dashboard.emit(COMMENT_EVENT);
  // The new follow-up supersedes F1/F1.1: only its two asks keep forms.
  await expect(forms).toHaveCount(2);
  await expect(forms.nth(0)).toHaveAttribute("data-ask-id", "F2\\");
  await expect(forms.nth(1)).toHaveAttribute("data-ask-id", 'F2\\"');
  await expect(
    page.locator('#turn-103 .ask-history[data-ask-id="F1"] .ask-superseded')
  ).toHaveCount(1);

  // Playwright's CSS parser rejects a trailing backslash inside quotes; the
  // form is the first of the two the new turn owns.
  const formB = forms.nth(0);
  await formB.locator('input[value="Yes"]').check();
  await formB.locator('input[name="custom-enabled"]').check();
  const custom = formB.locator('textarea[name="custom"]');
  await custom.fill("half-typed custom answer");
  await expect(custom).toBeFocused();

  // An answer to a superseded ask still lands beneath its question; an answer
  // to the sibling removes only the sibling's form.
  dashboard.addComment(REPO, 12, {
    body: answerComment(12, "R12.1", "Next week"),
    author: "sami",
    createdAt: new Date().toISOString(),
  });
  dashboard.addComment(REPO, 12, {
    body: answerComment(12, 'F2\\"', "No"),
    author: "sami",
    createdAt: new Date().toISOString(),
  });
  await dashboard.emit(COMMENT_EVENT);
  await expect(
    page.locator('#detail-opening-asks .ask-history[data-ask-id="R12.1"] .answer-pill')
  ).toHaveText("Next week");
  await expect(forms).toHaveCount(1);
  await expect(forms.nth(0)).toHaveAttribute("data-ask-id", "F2\\");
  await expect(custom).toHaveValue("half-typed custom answer");
  await expect(custom).toBeFocused();
  await expect(formB.locator('input[value="Yes"]')).toBeChecked();
  await expect(formB.locator('input[name="custom-enabled"]')).toBeChecked();
});

test("a failed answer post keeps the half-filled form, shows the error, and lets the retry through", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const form = page.locator('form[data-ask-id="F1"]');
  await form.locator('input[value="E2E_SUBMITTER"]').check();
  await form.locator('input[name="custom-enabled"]').check();
  const custom = form.locator('textarea[name="custom"]');
  await custom.fill("use the org-level secret");

  dashboard.failNextComment();
  await form.locator("button[type=submit]").click();
  await expect(form.locator(".form-error")).toHaveText("GitHub REST request failed: 500");
  // Same form, same choice and text: nothing was torn down while the post was in flight.
  await expect(form.locator('input[value="E2E_SUBMITTER"]')).toBeChecked();
  await expect(form.locator('input[name="custom-enabled"]')).toBeChecked();
  await expect(custom).toHaveValue("use the org-level secret");
  await expect(form.locator("button[type=submit]")).toBeEnabled();
  // The optimistic answer was withdrawn: the question is still waiting.
  await expect(page.locator('#turn-103 .ask-history[data-ask-id="F1"] .ask-waiting')).toBeVisible();
  expect(dashboard.posted).toEqual([]);

  await form.locator("button[type=submit]").click();
  await expect(form).toHaveCount(0);
  await expect(page.locator('#turn-103 .ask-history[data-ask-id="F1"] .answer-pill')).toHaveText(
    "use the org-level secret"
  );
  expect(dashboard.posted.at(-1)?.body).toContain('forAsk: "F1"');
});

test("a gh-ref anchor planted in a comment is left alone: no proxy fetch, no error, real references still unfurl", async ({
  page,
  dashboard,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  const fetched: string[] = [];
  page.on("request", (request) => {
    const { pathname } = new URL(request.url());
    if (pathname.startsWith("/api/github/rest/repos/")) fetched.push(decodeURIComponent(pathname));
  });
  await page.goto(`${dashboard.url}/${THREAD}`);
  await expect(page.locator(`#detail-opening a.gh-ref[data-gh-ref="${REPO}#7"]`)).toHaveText(
    "Pick a color"
  );

  dashboard.addComment(REPO, 12, {
    body: `Planted: <a class="gh-ref" data-gh-ref='X"]#1' href="https://example.com/">bait</a> — and a real one, #7.`,
    author: "sami",
    createdAt: new Date().toISOString(),
  });
  await dashboard.emit(COMMENT_EVENT);
  const conversation = page.locator("#detail-conversation");
  await expect(conversation.locator('a[href="https://example.com/"]')).toHaveText("bait");
  await expect(conversation.locator(`a.gh-ref[data-gh-ref="${REPO}#7"]`)).toHaveText(
    "Pick a color"
  );
  expect(fetched.filter((path) => path.includes('X"]'))).toEqual([]);
  expect(pageErrors).toEqual([]);
});

test("a thread closed then reopened by events gets one fresh reply form; the search draft survives", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  await expect(page.locator("#detail-reply")).toHaveCount(1);
  const search = page.locator("#search-input");
  await search.click();
  await search.pressSequentially("harn");
  await expect(page.locator(".thread-row")).toHaveCount(1);

  dashboard.setState(REPO, 12, "closed");
  await dashboard.emit({ subject: `${SUBJECT}.12.closed`, repo: REPO });
  await expect(page.locator("#detail-reply")).toHaveCount(0);
  await expect(page.locator("#detail-ask-forms form")).toHaveCount(0);
  // A closed thread leaves the open-threads search; the selection stays put.
  await expect(page.locator(".thread-row")).toHaveCount(0);
  await expect(page.locator("#detail-root h1")).toHaveText(
    "E2E harness: which identity submits test PRs?"
  );

  dashboard.setState(REPO, 12, "open");
  await dashboard.emit({ subject: `${SUBJECT}.12.reopened`, repo: REPO });
  await expect(page.locator(".thread-row")).toHaveCount(1);
  await expect(page.locator("#detail-reply")).toHaveCount(1);
  await expect(page.locator("#detail-ask-forms form.ask-form")).toHaveCount(2);
  await expect(search).toHaveValue("harn");
  await expect(search).toBeFocused();

  // The fresh reply form is the one that stays: a later issue event leaves it alone.
  const reply = page.locator("#reply-body");
  await reply.fill("draft after reopen");
  await dashboard.emit({ subject: `${SUBJECT}.12`, repo: REPO });
  const highlighted = page.locator('.thread-row[data-thread-number="12"].live-highlight');
  await expect(highlighted).toHaveCount(1);
  await expect(highlighted).toHaveCount(0, { timeout: 5_000 });
  await expect(reply).toHaveValue("draft after reopen");
});

test("references unfurl into titled links; unknown ones stay plain", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const opening = page.locator("#detail-opening");
  await expect(opening.locator(`a.gh-ref[data-gh-ref="${REPO}#7"]`)).toHaveText("Pick a color");
  const pr = opening.locator(`a.gh-ref[data-gh-ref="${REPO}#9"]`);
  await expect(pr).toHaveCount(2);
  for (const anchor of await pr.all()) {
    await expect(anchor).toHaveText("Add e2e submitter identity");
  }
  const unknown = opening.locator(`a.gh-ref[data-gh-ref="${REPO}#999"]`);
  await expect(unknown).toHaveText("#999");
  await expect(unknown).toHaveAttribute("href", `https://github.com/${REPO}/issues/999`);
});

test("origin line shows session title and id with copy, and keeps the tmux jump", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  const origin = page.locator("#detail-header .origin-line");
  await expect(origin.locator(".origin-session-title")).toHaveText("pm: e2e submitter identity");
  await expect(origin.locator("code.origin-session-id")).toHaveText(SESSION_ID);
  await origin.locator('button[data-action="copy-session-id"]').click();
  await origin.locator('button[data-action="copy-origin"]').click();
  expect(await page.evaluate(() => window.__copied)).toEqual([
    SESSION_ID,
    "tmux switch-client -t %15",
  ]);
});

test("urgency changes post an HTML-comment marker with a summary line", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  await page.locator("#detail-header details.urgency-chip-wrap summary").click();
  await page.locator('#detail-header button[data-urgency-value="blocking"]').click();
  await expect.poll(() => dashboard.posted.length).toBe(1);
  const body = dashboard.posted[0]?.body ?? "";
  expect(body.startsWith("<!-- dispatch:urgency\n")).toBe(true);
  expect(body.endsWith("-->\n\nUrgency set to **blocking**.")).toBe(true);
});

test("events never rebuild the reply box, the ask forms, the search box, or an unchanged conversation", async ({
  page,
  dashboard,
}) => {
  await page.goto(`${dashboard.url}/${THREAD}`);
  await expect(page.locator("#detail-ask-forms form.ask-form")).toHaveCount(2);
  const reply = page.locator("#reply-body");
  await reply.fill("draft that must survive");
  const form = page.locator('form[data-ask-id="F1"]');
  await form.locator('input[name="custom-enabled"]').check();
  await form.locator('textarea[name="custom"]').fill("half an answer");
  const search = page.locator("#search-input");
  await search.click();
  // "acme" matches both fixture threads through their repo, so the unrelated
  // thread's row stays visible for its highlight below.
  await search.pressSequentially("acme");
  // Tag the live nodes; a rebuilt node would not carry the tag.
  const tag = () =>
    page.evaluate(() => {
      const nodes: Element[] = [];
      for (const id of ["reply-body", "search-input"])
        nodes.push(document.getElementById(id) as Element);
      nodes.push(document.querySelector('form[data-ask-id="F1"]') as Element);
      // The conversation's first card: an innerHTML rewrite replaces it, the section itself stays.
      nodes.push(document.querySelector("#detail-conversation > *") as Element);
      return nodes.map((node) => {
        if (!node) return "missing";
        const tagged = node as HTMLElement & { __tag?: string };
        tagged.__tag ??= `tag-${Math.random()}`;
        return tagged.__tag;
      });
    });
  const before = await tag();
  expect(before).not.toContain("missing");

  // An event on the selected thread repaints the conversation (new comment), so its
  // cards are new nodes; everything the human types into is the same node.
  dashboard.addComment(REPO, 12, {
    body: "another human reply",
    author: "sami",
    createdAt: new Date().toISOString(),
  });
  await dashboard.emit(COMMENT_EVENT);
  await expect(page.locator("#detail-conversation")).toContainText("another human reply");
  const afterSame = await tag();
  expect(afterSame.slice(0, 3)).toEqual(before.slice(0, 3));
  expect(afterSame[3]).not.toBe(before[3]);

  // An event on an unrelated thread: nothing in the detail pane is repainted.
  const conversationHtml = await page.locator("#detail-conversation").innerHTML();
  dashboard.addComment(REPO, 7, {
    body: "activity elsewhere",
    author: "sami",
    createdAt: new Date().toISOString(),
  });
  await dashboard.emit({ subject: `${SUBJECT}.7.comment`, repo: REPO });
  await expect(page.locator('.thread-row[data-thread-number="7"].live-highlight')).toHaveCount(1);
  const afterOther = await tag();
  expect(afterOther).toEqual(afterSame);
  expect(await page.locator("#detail-conversation").innerHTML()).toBe(conversationHtml);
  await expect(reply).toHaveValue("draft that must survive");
  await expect(form.locator('textarea[name="custom"]')).toHaveValue("half an answer");
  await expect(search).toBeFocused();
  await expect(search).toHaveValue("acme");
});

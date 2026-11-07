import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  mintAgentToken,
  patchIssue,
} from "./api";
import { setPendingCredentialRequests } from "./fake-broker-helpers";
import {
  closeAssigneeFilter,
  inboxSearchBox,
  openAssigneeFilter,
  pickAssignee,
  searchInbox,
} from "./inbox-filter";
import { askControls, inboxRow, paneScroller, selectedRow, selectRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's search box (INFRA-963): one field beside the assignee filter that keeps the rows
 * holding every word a reader types and drops the rest, matching an ask's issue key, its issue
 * title, the name of the document it stands on where it has no issue, its question and its
 * author, whatever the case.
 *
 * Every name here is made up: this repository is public, so nothing names a real person, host or
 * project. The seed is built from `e2e/api.ts`, `e2e/seed.ts` and `e2e/users.ts` alone (plus the
 * fake broker's one credential request, which no Dispatch write can seed), so this file runs
 * against the commit before the box and fails there.
 */

// ---------------------------------------------------------------------------
// The three agents whose asks this file lists. Each word of a title and of a
// session id belongs to that author alone, so a narrowing that keeps a row
// proves which field carried it.
// ---------------------------------------------------------------------------

/** The agent that wrote four of the questions; its title is two words nothing else holds. */
const PLANNER = {
  actor: {
    id: "e2e-nightly-planner",
    kind: "session" as const,
    origin: { session_title: "Nightly planner" },
  },
  as: "agent" as const,
};

/** The agent that wrote the two questions on the retry budget. */
const AUDITOR = {
  actor: {
    id: "e2e-morning-auditor",
    kind: "session" as const,
    origin: { session_title: "Morning auditor" },
  },
  as: "agent" as const,
};

/** The session id of the agent whose write carries an owner: a personal token mints that owner,
 *  so the ask records `author.owner`, which no body may set (`bearerSessionActor`). */
const RUNNER_SESSION = "e2e-sweep-runner";

/** A session id no ask names, for the agent filter's own empty state. */
const SILENT_SESSION = "e2e-silent-session";

/** The address every row but the drawer's reads: the cleared assignee filter, so the question on
 *  the project document - which nobody holds, a document having no assignee - and the unassigned
 *  issue's question are listed beside the four Alice holds. */
const INBOX = "/?assignee=everyone";

/** The pending credential request the fake broker lists for Alice throughout, so the empty state
 *  the typed text replaces is the silent one a pending request leaves behind. */
const CREDENTIAL_RECORD = "record-search-alice";

interface Seed {
  /** `LEDGER-2`, third question: "Does the budget reset after a successful write?" */
  budgetAskId: string;
  /** `LEDGER-1`, the only row whose title holds `clock` and `drift`. */
  clockAskId: string;
  /** `MIRROR-2`, the unassigned issue's question, by the agent whose owner is bob. */
  leaseAskId: string;
  /** `MIRROR-1`, the one row waiting on its agent. */
  replayAskId: string;
  /** `LEDGER-2`, first question: the only row holding both `ledger` and `lease`. */
  retryAskId: string;
  /** The question on the project document `Mirror runbook`, which has no issue. */
  runbookAskId: string;
  /** `LEDGER-2`, second question. */
  wakeAskId: string;
  /** `MIRROR-1`, second question. */
  windowAskId: string;
}

/**
 * Eight questions across two projects, three agent authors, two assignees and one project
 * document, plus one pending credential request.
 *
 * Alice holds `LEDGER-1` and `LEDGER-2` (four questions), Bob holds `MIRROR-1` (two), nobody
 * holds `MIRROR-2` (one, so its row offers "Assign to me"), and the project document's question
 * is nobody's. One of Bob's waits on its agent, because Alice replied to it: `?section=needs-you`
 * has a row to drop.
 */
async function seedInbox(): Promise<Seed> {
  await createProject({ key: "LEDGER", name: "Ledger service" });
  await createProject({ key: "MIRROR", name: "Mirror service" });
  // The owner a session's ask carries is the owner of the credential that wrote it, never a
  // field of the body, so bob's own token is what puts `bob` in that row's author.
  const bobsToken = await mintAgentToken("search-seed", "bob");
  const runner = {
    actor: {
      id: RUNNER_SESSION,
      kind: "session" as const,
      origin: { session_title: "Sweep runner" },
    },
    as: "agent" as const,
    token: bobsToken,
  };

  const clockIssue = await createIssue({ project: "LEDGER", title: "Clock drift" });
  await patchIssue(clockIssue.key, { priority: 0 });
  const clock = await createAsk(
    clockIssue.key,
    { question: "Which source of truth does the ledger read at startup?" },
    PLANNER
  );

  const retryIssue = await createIssue({ project: "LEDGER", title: "Retry budget" });
  await patchIssue(retryIssue.key, { priority: 1 });
  const retry = await createAsk(
    retryIssue.key,
    { question: "How many attempts before the lease is dropped?" },
    AUDITOR
  );
  const wake = await createAsk(
    retryIssue.key,
    { question: "Should a refused attempt page the operator when the drift widens?" },
    AUDITOR
  );
  const budget = await createAsk(
    retryIssue.key,
    { question: "Does the budget reset after a successful write?" },
    PLANNER
  );

  const replayIssue = await createIssue(
    { project: "MIRROR", title: "Replay window" },
    {
      login: "bob",
    }
  );
  expect(replayIssue.assignee).toBe("bob");
  await patchIssue(replayIssue.key, { priority: 2 });
  const replay = await createAsk(
    replayIssue.key,
    { question: "Does a cold start need a second pass?" },
    PLANNER
  );
  const window = await createAsk(
    replayIssue.key,
    { question: "Which window does the mirror replay first?" },
    PLANNER
  );
  // A person's reply hands the turn to the agent (`askReplyTurn`), which is the one row
  // `?section=needs-you` drops.
  await createComment(
    replayIssue.key,
    { ask_id: replay.id, body: "Cold start from which snapshot?" },
    { login: "alice" }
  );
  await expect.poll(() => getAsk(replay.id)).toMatchObject({ ask: { waiting_on: "agent" } });

  // The shared token holds nobody, so this issue is unassigned and its row offers "Assign to me".
  const leaseIssue = await createIssue(
    { project: "MIRROR", title: "Lease handover" },
    { as: "agent" }
  );
  expect(leaseIssue.assignee).toBeNull();
  await patchIssue(leaseIssue.key, { priority: 3 });
  const lease = await createAsk(
    leaseIssue.key,
    { question: "Who holds the lease during a cold handover?" },
    runner
  );
  expect(lease.author).toMatchObject({ owner: "bob" });

  const document = await createProjectDocument("MIRROR", {
    content: "# Mirror runbook\n\nThe evening sweep replays what the ledger wrote.\n",
    name: "Mirror runbook",
  });
  const runbook = await createArtifactAsk(
    document.artifact.id,
    { question: "Who signs off the evening checklist?" },
    PLANNER
  );

  await setPendingCredentialRequests([
    {
      approve_url: "https://secrets.internal.example/approve/search",
      approver: "alice",
      identifiers: ["LEDGER_API_KEY"],
      kind: "agent_secret",
      record_id: CREDENTIAL_RECORD,
      requested_at: new Date().toISOString(),
    },
  ]);

  return {
    budgetAskId: budget.id,
    clockAskId: clock.id,
    leaseAskId: lease.id,
    replayAskId: replay.id,
    retryAskId: retry.id,
    runbookAskId: runbook.id,
    wakeAskId: wake.id,
    windowAskId: window.id,
  };
}

/** Every row the list is drawing, at either width. */
function rows(page: Page): Locator {
  return page.locator("[data-inbox-row]");
}

/** The one line the list shows in place of its bands when no band has a row. */
function emptyState(page: Page): Locator {
  return page.getByRole("region", { name: "Inbox empty state" });
}

/** Opens the Inbox and waits until the seeded list has been drawn, so a count taken right after
 *  a narrowing cannot be the count of a list that had not arrived yet. */
async function openInbox(page: Page, address = INBOX): Promise<void> {
  await page.goto(address);
  await expect(rows(page)).toHaveCount(8);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test.describe("inbox search", () => {
  // The box is the Inbox's own control and the keyboard's: `f` reaches it from anywhere on the
  // page, and the assignee popover's own field - a `combobox` named "Search people" - is a
  // different control, so a reader (and a screen reader) has exactly one "Search asks".
  test("the header holds one Search asks box, which `f` puts the focus in", async ({ browser }) => {
    await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      const box = inboxSearchBox(page);
      await expect(box).toHaveCount(1);
      await expect(box).toHaveValue("");
      await expect(box).toHaveAttribute("placeholder", "Issue, title, question or author");
      // The cap is said rather than enforced by the field, so the change handler's cut is the
      // one rule (proved by the address row below).
      expect(await box.getAttribute("maxlength")).toBeNull();
      await expect(box).toHaveAttribute("title", `At most ${SEARCH_QUERY_MAX} characters`);

      // With the assignee popover open both fields are on the page, and they are not the same
      // control: only one of them is the Inbox's searchbox.
      await openAssigneeFilter(page);
      await expect(page.getByRole("combobox", { name: "Search people" })).toBeVisible();
      await expect(page.getByRole("searchbox", { name: "Search asks" })).toHaveCount(1);
      await closeAssigneeFilter(page);

      // `f` from the page itself, with the focus on nothing.
      await page.locator("body").focus();
      await page.keyboard.press("f");
      await expect(box).toBeFocused();
      await expect(box).toHaveValue("");

      // And `f` inside the box is a letter, not the key: the binding is not `inEditable` and a
      // `search` input takes typed text (`keymap.ts`'s `TEXT_INPUT_TYPES`), so the registry
      // passes the key through to the field rather than re-focusing what the reader is already
      // in. The word is the seed's own, so the narrowing it makes is real too.
      await page.keyboard.type("drift");
      await expect(box).toHaveValue("drift");
      await expect(box).toBeFocused();
      await expect(rows(page)).toHaveCount(2);
    } finally {
      await alice.close();
    }
  });

  // One narrowing per field the rule reads, each one two words: the row kept holds both, and the
  // rows holding one of them go. The second word is always carried by a field other than the one
  // under test, so a row that survives proves that field was read.
  test("two words keep the rows holding both, matched on the key, the title, the question and the author", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      // The issue key, which carries the project key: `ledger` is in no title, question or
      // author, so the four LEDGER rows hold it through their keys alone.
      await searchInbox(page, "ledger");
      await expect(rows(page)).toHaveCount(4);
      await searchInbox(page, "ledger lease");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.retryAskId)).toBeVisible();

      // The same two words in any case: the rule lowercases both sides.
      await searchInbox(page, "LEDGER Lease");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.retryAskId)).toBeVisible();

      // The issue title: `clock` and `drift` are both in "Clock drift" and nowhere else in that
      // row, and a second row holds `drift` in its question alone.
      await searchInbox(page, "drift");
      await expect(rows(page)).toHaveCount(2);
      await searchInbox(page, "clock drift");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.clockAskId)).toBeVisible();

      // The question: `cold` is in two questions, `pass` in one of them.
      await searchInbox(page, "cold");
      await expect(rows(page)).toHaveCount(2);
      await searchInbox(page, "cold pass");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.replayAskId)).toBeVisible();

      // The author, as the ask carries it: the title its origin recorded.
      await searchInbox(page, "nightly planner");
      await expect(rows(page)).toHaveCount(5);
      for (const askId of [
        seed.clockAskId,
        seed.budgetAskId,
        seed.replayAskId,
        seed.windowAskId,
        seed.runbookAskId,
      ]) {
        await expect(inboxRow(page, askId)).toBeVisible();
      }

      // The author's session id, which no title repeats whole.
      await searchInbox(page, "e2e-morning-auditor");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, seed.retryAskId)).toBeVisible();
      await expect(inboxRow(page, seed.wakeAskId)).toBeVisible();

      // The author's owner, a login: `bob` is in no key, title or question, and `lease` is in
      // two rows, so the row kept is the one whose agent writes for Bob.
      await searchInbox(page, "lease");
      await expect(rows(page)).toHaveCount(2);
      await searchInbox(page, "bob lease");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.leaseAskId)).toBeVisible();
    } finally {
      await alice.close();
    }
  });

  // The hold that keeps a row under the reader's hand while the server moves the list is not
  // consulted for the reader's own typing: a row the words exclude goes at once, whether they
  // last clicked it or are resting the pointer on it.
  test("the row last clicked and the row under the pointer drop with the rest", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      // Clicked: the reading column is showing it, and the focus is on it.
      await selectRow(page, seed.clockAskId);
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.clockAskId);

      // Pointed at: the pointer rests in the middle of another row and stays there.
      const pointed = inboxRow(page, seed.leaseAskId);
      const box = await pointed.boundingBox();
      if (box === null) {
        throw new Error("the Inbox drew no box for the row the pointer was to rest on");
      }
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      await expect(pointed).toBeVisible();

      // Neither survives words it does not hold.
      await searchInbox(page, "cold pass");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.clockAskId)).toHaveCount(0);
      await expect(inboxRow(page, seed.leaseAskId)).toHaveCount(0);
      await expect(inboxRow(page, seed.replayAskId)).toBeVisible();
    } finally {
      await alice.close();
    }
  });

  // The text narrows where the assignee filter's narrowing is applied, so the two compose
  // whichever way the list was already filtered; what sits above the bands is untouched.
  test("the text narrows inside the assignee filter, ?agent= and ?section=needs-you, and leaves the sections above the bands alone", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      // Above the bands: the credential request and the summary of what waits on the reader -
      // her four questions and the one request - which the typed text never moves.
      const credentials = page.getByRole("region", { name: "Credential requests" });
      const blocked = page.getByRole("link", { name: /^Blocked on you: 5 items/ });
      await expect(credentials.getByRole("link")).toHaveCount(1);
      await expect(credentials.getByRole("link")).toContainText("LEDGER_API_KEY");
      await expect(blocked).toBeVisible();

      // Inside the filter's own selection: Bob holds one issue, with two questions on it.
      await pickAssignee(page, "bob");
      await closeAssigneeFilter(page);
      await expect(rows(page)).toHaveCount(2);
      await searchInbox(page, "window first");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.windowAskId)).toBeVisible();
      await expect(credentials.getByRole("link")).toHaveCount(1);
      await expect(blocked).toBeVisible();

      // Inside `?agent=`: that agent's two questions, then the one of them holding the word.
      await page.goto(`/?agent=${AUDITOR.actor.id}&assignee=everyone`);
      await expect(rows(page)).toHaveCount(2);
      await searchInbox(page, "lease");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.retryAskId)).toBeVisible();

      // Inside `?section=needs-you`: the row waiting on its agent is already gone, so the word
      // `cold`, which two rows hold, keeps only the other one.
      await page.goto("/?section=needs-you&assignee=everyone");
      await expect(rows(page)).toHaveCount(7);
      await expect(inboxRow(page, seed.replayAskId)).toHaveCount(0);
      await searchInbox(page, "cold");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.leaseAskId)).toBeVisible();
      await expect(credentials.getByRole("link")).toHaveCount(1);
    } finally {
      await alice.close();
    }
  });

  // The counts a reader reads off the list are the narrowed list's: a group's own `· N asks` and
  // the `ask N of M` each row's controls are named by (`features/inbox/ask-name.ts`).
  test("the group counts and the row ordinals count the narrowed list", async ({ browser }) => {
    await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      // Three questions on one issue: one header, and three rows that say which of three.
      const group = page.locator('[data-inbox-group-header="issue:LEDGER-2"]');
      await expect(group).toContainText("· 3 asks");
      for (const index of [1, 2, 3]) {
        await expect(
          page.getByRole("checkbox", { name: new RegExp(`^Select LEDGER-2, ask ${index} of 3:`) })
        ).toHaveCount(1);
      }

      // The word drops one of the three; the header and the names count what is left.
      await searchInbox(page, "ledger auditor");
      await expect(rows(page)).toHaveCount(2);
      await expect(group).toContainText("· 2 asks");
      for (const index of [1, 2]) {
        await expect(
          page.getByRole("checkbox", { name: new RegExp(`^Select LEDGER-2, ask ${index} of 2:`) })
        ).toHaveCount(1);
      }
      await expect(
        page.getByRole("checkbox", { name: /^Select LEDGER-2, ask 3 of 3:/ })
      ).toHaveCount(0);

      // The bands hold what is left of them, and the band with nothing left is gone.
      await expect(page.locator('[data-inbox-section="human"]')).toHaveCount(2);
      await expect(page.locator('[data-inbox-section="agent"]')).toHaveCount(0);
      await expect(page.getByRole("heading", { name: "Waiting on agents" })).toHaveCount(0);

      // Cleared, every count is the whole list's again.
      await searchInbox(page, "");
      await expect(rows(page)).toHaveCount(8);
      await expect(group).toContainText("· 3 asks");
      await expect(page.locator('[data-inbox-section="agent"]')).toHaveCount(1);
    } finally {
      await alice.close();
    }
  });

  // A mark is the reader's, and the box is a view of the list: a row the box hides leaves the
  // bar's count, because the bar never counts a row that is not on the page, and the mark itself
  // comes back with the row.
  test("a marked row the box hides leaves the bulk count and comes back marked", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      await selectRow(page, seed.clockAskId);
      await page.keyboard.press("x");
      const bar = page.getByRole("group", { name: "Selected asks" });
      await expect(bar).toContainText("1 selected");
      await expect(inboxRow(page, seed.clockAskId).getByRole("checkbox")).toBeChecked();

      await searchInbox(page, "cold pass");
      await expect(inboxRow(page, seed.clockAskId)).toHaveCount(0);
      await expect(bar).toHaveCount(0);

      await searchInbox(page, "");
      await expect(rows(page)).toHaveCount(8);
      await expect(inboxRow(page, seed.clockAskId).getByRole("checkbox")).toBeChecked();
      await expect(bar).toContainText("1 selected");
    } finally {
      await alice.close();
    }
  });

  // A reader who narrowed the list is told why it is empty before anything else is: ahead of the
  // agent filter's own line, and ahead of the rule that leaves the list silent while a credential
  // request waits on them.
  test("text matching no row says so ahead of every other empty line", async ({ browser }) => {
    await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();

      // A filter nobody matches, with a request pending: the list says nothing at all, because
      // the request above it waits on the reader as much as a question does.
      await page.goto("/?assignee=carol");
      await expect(page.getByRole("region", { name: "Credential requests" })).toBeVisible();
      await expect(rows(page)).toHaveCount(0);
      await expect(emptyState(page)).toHaveCount(0);

      // The same address with a word nothing holds: now it says why.
      await page.goto("/?assignee=carol&q=zzzq");
      await expect(rows(page)).toHaveCount(0);
      await expect(emptyState(page)).toHaveText("Nothing matches this text");
      await expect(page.getByRole("region", { name: "Credential requests" })).toBeVisible();

      // An agent filter nothing matches names that agent.
      await page.goto(`/?agent=${SILENT_SESSION}&assignee=everyone`);
      await expect(rows(page)).toHaveCount(0);
      await expect(emptyState(page)).toContainText("No open asks from");

      // With a word nothing holds, the text's line comes first.
      await page.goto(`/?agent=${SILENT_SESSION}&assignee=everyone&q=zzzq`);
      await expect(rows(page)).toHaveCount(0);
      await expect(emptyState(page)).toHaveText("Nothing matches this text");

      // And a word nothing holds on the plain list says the same thing, typed rather than linked.
      await openInbox(page);
      await searchInbox(page, "zzzq");
      await expect(rows(page)).toHaveCount(0);
      await expect(emptyState(page)).toHaveText("Nothing matches this text");
    } finally {
      await alice.close();
    }
  });

  // A reader types one letter at a time, and the letter that empties the list must not take the
  // box out from under them. The empty list draws the header in a tree of its own
  // (`Inbox.tsx`'s `empty`, a plain div, against the list's `ViewportAnchor`), so React mounts a
  // fresh input there: the focus goes to the body, and the rest of the word, the Backspace that
  // would correct it and every later keystroke reach the page's own keys instead of the box.
  test("the box keeps the focus through a word that matches no row", async ({ browser }) => {
    await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);
      const box = inboxSearchBox(page);
      await page.keyboard.press("f");
      await expect(box).toBeFocused();

      await page.keyboard.type("ledger");
      await expect(rows(page)).toHaveCount(4);

      // The letter that empties the list.
      await page.keyboard.type("z");
      await expect(rows(page)).toHaveCount(0);
      await expect(emptyState(page)).toHaveText("Nothing matches this text");
      await expect(box).toBeFocused();

      // The rest of the word, and the correction, still reach the box.
      await page.keyboard.type("q");
      await expect(box).toHaveValue("ledgerzq");
      await page.keyboard.press("Backspace");
      await page.keyboard.press("Backspace");
      await expect(box).toHaveValue("ledger");
      await expect(rows(page)).toHaveCount(4);
    } finally {
      await alice.close();
    }
  });

  // A question on a project document belongs to that document, so the document's name is what a
  // reader has to search it by - and the document's name is read only where there is no issue, so
  // `mirror` from the two MIRROR keys does not drag those rows in beside it.
  test("a question on a project document is found by a word of the document's name", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      await searchInbox(page, "mirror");
      await expect(rows(page)).toHaveCount(4);

      await searchInbox(page, "runbook");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.runbookAskId)).toBeVisible();

      await searchInbox(page, "mirror runbook");
      await expect(rows(page)).toHaveCount(1);
      const row = inboxRow(page, seed.runbookAskId);
      await expect(row).toBeVisible();
      await expect(row.getByRole("link", { name: "Mirror runbook" })).toBeVisible();
    } finally {
      await alice.close();
    }
  });

  // One row outranks the typed text: the row whose "Assign to me" write is still in flight. Under
  // any narrowing the optimistic assignment would drop that row, and unmounting it would take
  // `Assigning…`, a refusal and its Retry off the screen with it.
  //
  // The width is this row's own, 1024 px: the control that registers the write with the list is
  // the card row's (`InboxCardRow`), and above 1280 px the page draws short rows beside the
  // reading column instead, whose head keeps its own `Assigning…` and registers nothing.
  test("a row whose Assign to me write is in flight stays listed with its control", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.setViewportSize({ height: 900, width: 1024 });

      const held = Promise.withResolvers<void>();
      const requested = Promise.withResolvers<void>();
      await page.route("**/api/v1/issues/MIRROR-2", async (route) => {
        if (route.request().method() !== "PATCH") return route.continue();
        requested.resolve();
        await held.promise;
        return route.continue();
      });

      await openInbox(page);
      const assign = (await askControls(page, seed.leaseAskId)).getByRole("button", {
        name: "Assign MIRROR-2 to me",
      });
      await assign.click();
      await requested.promise;
      await expect(assign).toHaveText("Assigning…");

      // Words this row does not hold leave it exactly where it was, with its control.
      await searchInbox(page, "clock drift");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, seed.leaseAskId)).toBeVisible();
      await expect(assign).toHaveText("Assigning…");
      await expect(inboxRow(page, seed.clockAskId)).toBeVisible();

      // Once the write settles the exemption ends, and the text has the row.
      held.resolve();
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.leaseAskId)).toHaveCount(0);
      await expect(inboxRow(page, seed.clockAskId)).toBeVisible();
    } finally {
      await alice.close();
    }
  });

  // The address is the whole memory of the typed text: a reader can link it, reload it and walk
  // the list without losing it, and what the page writes there is cut to what a URL may carry.
  test("the typed text rides in the address, cut to the cap, and survives a reload and `j`", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      await searchInbox(page, "ledger lease");
      await expect(rows(page)).toHaveCount(1);
      await expect.poll(() => new URL(page.url()).searchParams.get("q")).toBe("ledger lease");

      // One character over the cap: the change handler cuts what it writes, and the field shows
      // the cut text, so what is filtered by and what is in the address are the one string.
      await inboxSearchBox(page).fill("x".repeat(SEARCH_QUERY_MAX + 1));
      await expect
        .poll(() => new URL(page.url()).searchParams.get("q")?.length)
        .toBe(SEARCH_QUERY_MAX);
      await expect(inboxSearchBox(page)).toHaveValue("x".repeat(SEARCH_QUERY_MAX));
      await expect(emptyState(page)).toHaveText("Nothing matches this text");

      // A reload opens on the same narrowed list, with the same text in the box.
      await searchInbox(page, "ledger");
      await expect(rows(page)).toHaveCount(4);
      await page.reload();
      await expect(rows(page)).toHaveCount(4);
      await expect(inboxSearchBox(page)).toHaveValue("ledger");
      await expect(inboxRow(page, seed.clockAskId)).toBeVisible();

      // Walking the list rewrites the address for its selection and keeps the text in it. The
      // walk starts on the narrowed list's own first row, `LEDGER-1` being the one the issue
      // priorities put at the top of it, so there is a row below to step onto.
      await selectRow(page, seed.clockAskId);
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.clockAskId);
      await page.keyboard.press("j");
      await expect(selectedRow(page)).not.toHaveAttribute("data-inbox-row", seed.clockAskId);
      await expect(rows(page)).toHaveCount(4);
      await expect(inboxSearchBox(page)).toHaveValue("ledger");
      expect(new URL(page.url()).searchParams.get("q")).toBe("ledger");
    } finally {
      await alice.close();
    }
  });

  // Clearing the agent filter clears that filter alone: the reader's typed text is theirs, and
  // the list they get back is the whole inbox narrowed by it.
  test("Clear agent filter keeps the typed text and the list it narrows", async ({ browser }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.goto(`/?agent=${AUDITOR.actor.id}&q=lease&assignee=everyone`);
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.retryAskId)).toBeVisible();
      await expect(inboxSearchBox(page)).toHaveValue("lease");

      const clear = page.getByRole("link", { name: "Clear agent filter" });
      await expect(clear).toHaveText("Asks from Morning auditor · clear");
      await clear.click();

      await expect(page.getByRole("link", { name: "Clear agent filter" })).toHaveCount(0);
      await expect(inboxSearchBox(page)).toHaveValue("lease");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, seed.retryAskId)).toBeVisible();
      await expect(inboxRow(page, seed.leaseAskId)).toBeVisible();
      const address = new URL(page.url()).searchParams;
      expect(address.get("q")).toBe("lease");
      expect(address.get("agent")).toBeNull();
    } finally {
      await alice.close();
    }
  });

  // Escape is one level out, and out of the box is the list: the row the reading column is
  // showing. Clearing the box is the other way out - every row comes back.
  test("Escape leaves the box for the selected row, and clearing it restores every row", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      await selectRow(page, seed.budgetAskId);
      await page.locator("body").focus();
      await page.keyboard.press("f");
      await expect(inboxSearchBox(page)).toBeFocused();
      await searchInbox(page, "ledger auditor");
      await expect(rows(page)).toHaveCount(2);

      // The selected row moved with the narrowing; Escape puts the focus on whichever row the
      // column is showing now.
      await inboxSearchBox(page).focus();
      await page.keyboard.press("Escape");
      const selectedId = await selectedRow(page).getAttribute("data-inbox-row");
      await expect(inboxRow(page, selectedId ?? "")).toBeFocused();
      await expect(inboxSearchBox(page)).toHaveValue("ledger auditor");

      await searchInbox(page, "");
      await expect(rows(page)).toHaveCount(8);
      for (const askId of [seed.clockAskId, seed.leaseAskId, seed.runbookAskId]) {
        await expect(inboxRow(page, askId)).toBeVisible();
      }
    } finally {
      await alice.close();
    }
  });

  // The column keeps a question whose row the typed text hides while the reader has an unsent
  // answer in it (`pane-selection.ts`'s `paneAsk`), so there is a column to read with no
  // selected row beside it. Escape out of such a column is still one level out: it returns to
  // the row `l` was pressed on, rather than matching no binding and doing nothing.
  test("Escape leaves a column held open over a hidden row for the row `l` was pressed on", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openInbox(page);

      // The reader is part way through an answer to the clock question: that unsent text is what
      // keeps the question in the column once its row goes. The column is where this width
      // draws the card, the short rows beside it carrying none, so the one answer box on the
      // page is the column's.
      const typed = "The ledger, once the lease settles.";
      await selectRow(page, seed.clockAskId);
      const answer = page.locator("[data-ask-answer]");
      await expect(answer).toHaveCount(1);
      await answer.fill(typed);

      // Two words the clock row does not hold: its row goes, its question stays in the column,
      // and the list has no selected row left to return to.
      await inboxSearchBox(page).focus();
      await searchInbox(page, "ledger auditor");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, seed.clockAskId)).toHaveCount(0);
      await expect(selectedRow(page)).toHaveCount(0);
      await expect(answer).toHaveValue(typed);

      // Escape out of the box lands on the list's first row, and `l` from there records it. The
      // fill left the focus in the box, so the key needs no focus call of its own.
      await page.keyboard.press("Escape");
      const landedId = await rows(page).first().getAttribute("data-inbox-row");
      const landed = inboxRow(page, landedId ?? "");
      await expect(landed).toBeFocused();
      await page.keyboard.press("l");
      await expect(paneScroller(page)).toBeFocused();

      // And Escape hands the focus back to that row, with the answer still waiting in the
      // column above it. The list still holds no selected row at this moment, which is what
      // makes the recorded row the only way out: a column whose selection had come back would
      // prove nothing about the held one.
      await expect(selectedRow(page)).toHaveCount(0);
      await page.keyboard.press("Escape");
      await expect(landed).toBeFocused();
      await expect(answer).toHaveValue(typed);
    } finally {
      await alice.close();
    }
  });

  // A step taken inside a column held open over a hidden row can move nothing at all: the
  // question the column keeps stood above every row the text left, so `k` has no row to step
  // to and none to take the focus. The reader is still in the column, and the row `l` recorded
  // is the only way out it has, so a key that did nothing visible must not take that away:
  // Escape after it is still one level out, to the row `l` was pressed on.
  //
  // Its own three rows, not the file's eight: this needs the kept question to stand first with
  // the rows the text leaves below it, which the grouped seed above does not give.
  test("a step that moves nothing inside a held column leaves Escape its way back", async ({
    browser,
  }) => {
    // The list heads its band with the newest ask, so the question the column will keep is
    // written last: it stands above both rows the text leaves, which is what gives `k` nowhere
    // to step.
    await createProject({ key: "STEP", name: "Step service" });
    const replayIssue = await createIssue({ project: "STEP", title: "Cold replay" });
    await createAsk(
      replayIssue.key,
      { question: "Does a cold start need a second pass?" },
      PLANNER
    );
    const leaseIssue = await createIssue({ project: "STEP", title: "Cold lease handover" });
    await createAsk(leaseIssue.key, { question: "Who holds the cold lease?" }, PLANNER);
    const topIssue = await createIssue({ project: "STEP", title: "Ledger clock drift" });
    const top = await createAsk(
      topIssue.key,
      { question: "Which source of truth does the ledger read at startup?" },
      PLANNER
    );

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.goto(INBOX);
      await expect(rows(page)).toHaveCount(3);
      await expect(rows(page).first()).toHaveAttribute("data-inbox-row", top.id);

      // The unsent answer is what keeps the question in the column once the text hides its row.
      const typed = "The ledger, once the lease settles.";
      await selectRow(page, top.id);
      const answer = page.locator("[data-ask-answer]");
      await expect(answer).toHaveCount(1);
      await answer.fill(typed);

      await searchInbox(page, "cold");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, top.id)).toHaveCount(0);
      await expect(selectedRow(page)).toHaveCount(0);

      // Escape out of the box lands on the list's first row, and `l` from there records it.
      await page.keyboard.press("Escape");
      const landedId = await rows(page).first().getAttribute("data-inbox-row");
      const landed = inboxRow(page, landedId ?? "");
      await expect(landed).toBeFocused();
      await page.keyboard.press("l");
      await expect(paneScroller(page)).toBeFocused();

      // The step moves neither the selection nor the focus.
      await page.keyboard.press("k");
      await expect(paneScroller(page)).toBeFocused();
      await expect(selectedRow(page)).toHaveCount(0);

      await page.keyboard.press("Escape");
      await expect(landed).toBeFocused();
      await expect(answer).toHaveValue(typed);
    } finally {
      await alice.close();
    }
  });
});

test.describe("inbox search without a reading column", () => {
  // Below 1280 px the Inbox is the list of cards it has always been, and the box is the same
  // control doing the same thing to it: the header wraps, the cards narrow, and nothing else
  // about the narrowing changes with the width.
  test("the box narrows the card list at 1024 px and at 390 px", async ({ browser }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();

      for (const size of [
        { height: 900, width: 1024 },
        { height: 844, width: 390 },
      ]) {
        await page.setViewportSize(size);
        await page.goto(INBOX);
        await expect(rows(page)).toHaveCount(8);
        await expect(page.getByTestId("inbox-pane")).toHaveCount(0);

        await searchInbox(page, "ledger lease");
        await expect(rows(page)).toHaveCount(1);
        await expect(inboxRow(page, seed.retryAskId)).toBeVisible();
        // The row is the whole card here, so what the narrowing keeps is the question itself.
        await expect(
          page.getByTestId(`ask-${seed.retryAskId}`).getByText("How many attempts before the lease")
        ).toBeVisible();
        expect(new URL(page.url()).searchParams.get("q")).toBe("ledger lease");

        await searchInbox(page, "zzzq");
        await expect(emptyState(page)).toHaveText("Nothing matches this text");

        await searchInbox(page, "");
        await expect(rows(page)).toHaveCount(8);
      }
    } finally {
      await alice.close();
    }
  });

  // The drawer is a peek over another page, so its text is its own state and never that page's
  // address; it draws cards at every width, so there is no reading pane for `l` to move into and
  // Escape out of the box lands on the list's first row, the drawer having no selection.
  test("the drawer's box narrows its own list, writes no address, and leaves `l` idle", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.goto("/agents");
      await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
      await page.locator("body").focus();
      await page.keyboard.press("i");

      // The drawer opens on the reader's own work: the four questions on Alice's two issues.
      const drawer = page.getByRole("dialog", { name: "Inbox" });
      await expect(drawer).toBeVisible();
      const drawerRows = drawer.locator("[data-inbox-row]");
      await expect(drawerRows).toHaveCount(4);
      await expect(drawer.getByTestId("inbox-pane")).toHaveCount(0);
      await expect(drawer.locator("[data-inbox-selected]")).toHaveCount(0);

      // The box in the drawer narrows the drawer's list and writes nothing to the address.
      const box = drawer.getByRole("searchbox", { name: "Search asks" });
      await box.fill("ledger auditor");
      await expect(drawerRows).toHaveCount(2);
      await expect(drawer.locator(`[data-inbox-row="${seed.retryAskId}"]`)).toBeVisible();
      await expect(drawer.locator(`[data-inbox-row="${seed.wakeAskId}"]`)).toBeVisible();
      await expect(page).toHaveURL(/\/agents$/);
      expect(new URL(page.url()).searchParams.get("q")).toBeNull();

      // `l` has no reading pane to move into here, so it moves nothing and closes nothing.
      await page.keyboard.press("Escape");
      await expect(drawerRows.first()).toBeFocused();
      await page.keyboard.press("j");
      const walked = drawerRows.nth(1);
      await expect(walked).toBeFocused();
      await page.keyboard.press("l");
      await expect(walked).toBeFocused();
      await expect(drawer).toBeVisible();

      // Escape with the focus in the box leaves the drawer open and lands on its first row.
      await box.focus();
      await expect(box).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(drawer).toBeVisible();
      await expect(drawerRows.first()).toBeFocused();
      await expect(box).toHaveValue("ledger auditor");

      // Cleared, the drawer's own list is whole again, and the page under it never moved.
      await box.fill("");
      await expect(drawerRows).toHaveCount(4);
      await expect(page).toHaveURL(/\/agents$/);
    } finally {
      await alice.close();
    }
  });
});

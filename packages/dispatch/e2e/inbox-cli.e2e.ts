import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import {
  baseUrl,
  createAgentMessage,
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  listMyReplies,
  replyToMessageDelivery,
  requestApproval,
  snoozeAsk,
} from "./api";
import { cliSession, runDispatch } from "./dispatch-cli";
import { setPendingCredentialRequests } from "./fake-broker-helpers";
import { askCard, inboxRow, READING_PANE_WIDTH } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * INFRA-1017 end to end: the real `dispatch` command, run as its own process under a personal
 * token the person minted on the Settings page, against the real server, beside the page a real
 * browser renders from the same data.
 *
 * What it proves, in one seeded workspace: `dispatch inbox` lists exactly the questions and
 * credential requests the Inbox page lists; its counts are the page's own numbers; and every act
 * the token makes - an answer, a review, a message - is recorded as the person with the agent
 * beside them, which every reader renders. Last, the Settings checkbox that narrows a token to
 * the person scope alone stops it approving, and ticking it back lets it approve again.
 *
 * Two run details. The viewport is pinned to the reading column's width, because the Inbox draws
 * a row one way above it and another below (`e2e/inbox-pane.ts`). And the deferred ask's snooze
 * is a day out: the page bands a row by the browser's clock (`features/inbox/Inbox.tsx`) while the
 * write is judged by the database's (`api/ask_snooze.go`), so a snooze near its own boundary
 * would band one way in the command and the other on the page for no fault of the code's.
 */

/** The session that asks the questions of this seed: an agent of the team, told from the one the
 *  command runs as so a reader can see which agent acted for the person. */
const asker = {
  actor: {
    id: "e2e-inbox-cli-asker",
    kind: "session" as const,
    origin: { session_title: "Planner" },
  },
  as: "agent" as const,
};

/** The live session the person writes to: the target of the seed's one-off message, and of the
 *  message the command's token sends. */
const worker: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/w/inbox-cli",
  machine_id: "e2e-inbox-cli",
  roles: [],
  session_id: "e2e-inbox-cli-worker",
  title: "Worker",
};

/** How the person's own agent is named everywhere its acts are read. */
const actingThrough = `alice, acting through session ${cliSession}`;

/** One heading of `dispatch inbox`: the number it carries, and the lines printed under it. */
interface CommandSection {
  readonly count: number;
  readonly rows: string[];
}

/** The command's output as its headings and their rows. A heading is `<name> <count>` on its own
 *  line and every row under it begins `- `, so the shape is read without re-spelling a line. */
function printedSections(stdout: string): Map<string, CommandSection> {
  const sections = new Map<string, CommandSection>();
  let current: CommandSection | undefined;
  for (const line of stdout.split("\n")) {
    const heading = /^([A-Za-z][A-Za-z ]*) (\d+)$/.exec(line);
    if (heading !== null) {
      current = { count: Number(heading[2]), rows: [] };
      sections.set(heading[1] ?? "", current);
      continue;
    }
    if (line.startsWith("- ") && current !== undefined) current.rows.push(line.slice(2));
  }
  return sections;
}

/** The section a heading names, or a failure naming what was printed instead: a missing heading
 *  is the command's answer being wrong, not this spec's own undefined. */
function section(sections: Map<string, CommandSection>, heading: string): CommandSection {
  const found = sections.get(heading);
  expect(found, `${heading} is not among ${[...sections.keys()].join(", ")}`).toBeDefined();
  return found ?? { count: 0, rows: [] };
}

/** The asks a printed section is about, sorted: each row ends in the ask's own `dispatch://`
 *  reference, whose last segment is the id. Sorted, because the page groups an owner's rows
 *  together (`features/inbox/grouping.ts`) and the command prints the server's order flat, so
 *  the two agree on which asks are listed rather than on where a group puts them. */
function printedAsks(printed: CommandSection): string[] {
  return printed.rows.map((row) => (row.split(" · ").at(-1) ?? "").split("/").at(-1) ?? "").sort();
}

/** The asks the page has in one band, sorted the same way. */
async function bandAsks(page: Page, band: "human" | "agent" | "later"): Promise<string[]> {
  const ids = await page
    .locator(`[data-inbox-row][data-inbox-section="${band}"]`)
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-inbox-row") ?? ""));
  return ids.sort();
}

/** A sidebar entry's number: the badge after its name, 0 when it carries none, and undefined when
 *  the sidebar has no such entry at all. */
async function railCount(page: Page, name: RegExp): Promise<number | undefined> {
  const entry = page.getByRole("navigation", { name: "Navigation" }).getByRole("link", { name });
  if ((await entry.count()) === 0) return undefined;
  const digits = /(\d+)\s*$/.exec((await entry.innerText()).trim());
  return digits === null ? 0 : Number(digits[1]);
}

/** One write an agent makes for its person: the person's own token as the bearer, and `session=`
 *  naming the agent, which the server records as the act beside them (`requireScope`). */
function withToken(path: string, token: string, body: object): Promise<Response> {
  const url = new URL(path, baseUrl);
  url.searchParams.set("session", cliSession);
  return fetch(url, {
    body: JSON.stringify(body),
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    method: "POST",
  });
}

/** Everything the one workspace holds, by what each part is for. */
interface SeededInbox {
  readonly approvalAsk: string;
  readonly deferredAsk: string;
  readonly gateIssue: string;
  readonly ordinaryAsk: string;
  readonly reviewedArtifact: string;
  readonly reviewedIssue: string;
}

/**
 * One person's workspace as the Inbox shows it: questions waiting on them, one waiting on an
 * agent, one they deferred, an approval request, a credential request from the broker, a question
 * on someone else's issue and one on a document (neither is theirs, and neither list holds them),
 * an unread reply thread and a one-off message to a session that has been answered.
 */
async function seedInbox(): Promise<SeededInbox> {
  await setLiveSessions([worker]);
  await createProject({ key: "CORE", name: "Core" });

  // A human's issue is assigned to its creator, so each of these is alice's own.
  const ordinary = await createIssue({ project: "CORE", title: "Ship the inbox command" });
  const flag = await createIssue({ project: "CORE", title: "Name the rollout flag" });
  const running = await createIssue({ project: "CORE", title: "Migrate the ask snoozes" });
  const deferred = await createIssue({ project: "CORE", title: "Rotate the agent token" });
  const gate = await createIssue({ project: "CORE", spec: "The plan.", title: "Design gate" });
  const reviewed = await createIssue({
    project: "CORE",
    spec: "The rollout note.",
    title: "Write the rollout note",
  });
  const elsewhere = await createIssue(
    { project: "CORE", title: "Bob's own issue" },
    { login: "bob" }
  );

  const ordinaryAsk = await createAsk(
    ordinary.key,
    { question: "Which rollout flag should the command read?" },
    asker
  );
  await createAsk(flag.key, { question: "Is DISPATCH_INBOX the name we want?" }, asker);
  const runningAsk = await createAsk(running.key, { question: "Shall I run the backfill?" }, asker);
  // The agent took the question back, so its row sits in the Waiting-on-agents band.
  await createComment(
    running.key,
    { ask_id: runningAsk.id, body: "Running the backfill now.", turn: "agent" },
    asker
  );
  const deferredAsk = await createAsk(
    deferred.key,
    { question: "Rotate the token before or after the deploy?" },
    asker
  );
  // A day out, far from the band's boundary either clock judges it by.
  await snoozeAsk(deferredAsk.id, new Date(Date.now() + 24 * 3_600_000).toISOString(), {
    login: "alice",
  });
  const approval = await requestApproval(gate.primary_artifact_id, {}, asker);
  await createAsk(elsewhere.key, { question: "Does this block bob's release?" }, asker);

  // A document has no assignee, so its question is nobody's own: neither list holds it.
  const runbook = await createProjectDocument("CORE", {
    content: "# Runbook\n\nRestart, reseed, reopen the signed-in URL.\n",
    name: "Runbook",
  });
  await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    asker
  );

  await setPendingCredentialRequests([
    {
      approve_url: "https://secrets.internal.example/approve/record-inbox-cli",
      approver: "alice",
      identifiers: ["ROLLOUT_API_KEY"],
      kind: "agent_secret",
      record_id: "record-inbox-cli",
      requested_at: new Date().toISOString(),
      session: { enrollment: worker.session_id, request: worker.session_id },
    },
  ]);

  // A thread of alice's that an agent has written in since she last read it.
  const thread = await createComment(ordinary.key, { body: "Please draft the rollout note." });
  await createComment(
    ordinary.key,
    { body: "Drafted; it needs your flag name.", reply_to: thread.id },
    asker
  );

  // A one-off message to a session, answered, so the Agents page has a conversation to show.
  const message = await createAgentMessage(
    worker.session_id,
    { body: "How far is the backfill?", delivery: "aside" },
    { login: "alice" }
  );
  await replyToMessageDelivery(
    message.id,
    { attempt: 1, body: "Half-way; the flag name is next." },
    { id: worker.session_id, kind: "session" }
  );

  return {
    approvalAsk: approval.ask.id,
    deferredAsk: deferredAsk.id,
    gateIssue: gate.key,
    ordinaryAsk: ordinaryAsk.id,
    reviewedArtifact: reviewed.primary_artifact_id,
    reviewedIssue: reviewed.key,
  };
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("dispatch inbox under the person's own token lists the Inbox page's own rows, and every act the token makes is the person's with their agent beside it", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the command runs outside the browser");
  // Cold `bun` starts for the command, a seeded workspace and a page of its own.
  test.setTimeout(240_000);
  const seeded = await seedInbox();
  const home = mkdtempSync(join(tmpdir(), "dispatch-inbox-cli-"));
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    // The row markup differs above and below the reading column's width, so the width is the
    // spec's own rather than whatever the project's device leaves.
    await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH });

    // The person mints the token on the Settings page. The form holds both scopes unless the
    // reader clears one, and step 6 below reads those boxes; what proves this token holds both
    // is what it goes on to do with them.
    await page.goto("/settings");
    await page.getByLabel("Token label").fill("Inbox reader");
    await page.getByRole("button", { name: "New token" }).click();
    const token = await page.getByRole("status").locator("code").first().textContent();
    if (token === null) throw new Error("Settings minted no agent token");
    expect(token).toMatch(/^dsp_[A-Za-z0-9_-]{43}$/);

    // 1. The command, under that token, against the same server the page reads.
    const inbox = await runDispatch(["inbox"], home, token);
    expect(inbox.code, `${inbox.stdout}${inbox.stderr}`).toBe(0);
    const sections = printedSections(inbox.stdout);

    await page.goto("/");
    await expect(inboxRow(page, seeded.ordinaryAsk)).toBeVisible();
    const later = page.getByRole("button", { name: /^Later \(\d+\)$/ });
    await later.click();
    await expect(later).toHaveAttribute("aria-expanded", "true");
    await expect(inboxRow(page, seeded.deferredAsk)).toBeVisible();

    expect(printedAsks(section(sections, "Needs you"))).toEqual(await bandAsks(page, "human"));
    expect(printedAsks(section(sections, "Waiting on agents"))).toEqual(
      await bandAsks(page, "agent")
    );
    expect(printedAsks(section(sections, "Later"))).toEqual(await bandAsks(page, "later"));
    expect(section(sections, "Waiting on agents").count).toBe(
      (await bandAsks(page, "agent")).length
    );
    expect(section(sections, "Later").count).toBe((await bandAsks(page, "later")).length);
    // Neither list holds a question that is not the person's own: bob's issue's and the
    // document's are on the server's list and in neither of these.
    expect(await page.locator("[data-inbox-row]").count()).toBe(
      section(sections, "Needs you").rows.length +
        section(sections, "Waiting on agents").rows.length +
        section(sections, "Later").rows.length
    );

    // The credential requests, line for line: the page lists one row per request the command
    // prints, each naming the same secret.
    const requests = page.getByRole("region", { name: "Credential requests" });
    const printedRequests = section(sections, "Credential requests");
    expect(printedRequests.count).toBe(printedRequests.rows.length);
    await expect(requests.getByRole("listitem")).toHaveCount(printedRequests.rows.length);
    for (const [index, row] of printedRequests.rows.entries()) {
      const identifiers = row.split(" · ")[1] ?? "";
      expect(identifiers).not.toBe("");
      await expect(requests.getByRole("listitem").nth(index)).toContainText(identifiers);
    }

    // 2. The counts, read off the sidebar rather than summed here.
    //
    // Today the rail carries `Needs you n` on the Inbox entry and the unread threads on a
    // `Replies` entry of their own, and `needsYouCount` has no thread term at all
    // (`features/sidebar/Sidebar.tsx`, `features/inbox/BlockedOnYou.tsx`): the Inbox entry's
    // number is the command's `Needs you` alone. This base's command prints no `Unread threads`
    // heading - that waits on `GET /api/v1/me/replies` taking a `kind`, which dispatch://INFRA-450
    // adds - so the term is 0 here. Written as a branch so it still holds once that change folds
    // the Replies entry into the Inbox count: with no Replies entry the two terms together are
    // the Inbox entry's number, and with one the command's thread term is that entry's own.
    const needsYou = section(sections, "Needs you").count;
    const unreadThreads = sections.get("Unread threads")?.count;
    const inboxEntry = await railCount(page, /^Inbox/);
    const repliesEntry = await railCount(page, /^Replies/);
    expect(needsYou).toBe(section(sections, "Needs you").rows.length + printedRequests.count);
    if (repliesEntry === undefined) {
      expect(needsYou + (unreadThreads ?? 0)).toBe(inboxEntry);
    } else {
      expect(needsYou).toBe(inboxEntry);
      // The seed leaves a thread unread, which is the case the spec's single sum got wrong.
      expect(repliesEntry).toBe((await listMyReplies({}, { login: "alice" })).total);
      expect(repliesEntry).toBeGreaterThan(0);
      if (unreadThreads !== undefined) expect(unreadThreads).toBe(repliesEntry);
    }

    // 3. An answer the agent makes with the token is the person's answer, with the agent beside
    // it. The reading column keeps a question the reader had begun answering when it is answered
    // elsewhere (`features/inbox/pane-selection.ts`), which is where the Inbox shows this one.
    const card = await askCard(page, seeded.ordinaryAsk);
    await card.getByLabel("Your answer").fill("Drafting my own words here");
    const answered = await withToken(`/api/v1/asks/${seeded.ordinaryAsk}/answer`, token, {
      expected_edited_at: null,
      selected: [],
      text: "Read DISPATCH_INBOX_FLAG.",
    });
    expect(answered.status, await answered.text()).toBe(200);
    await expect(card).toContainText(`Answered by ${actingThrough}`);
    await expect(card).toContainText("Read DISPATCH_INBOX_FLAG.");

    // The answers page says the same of it. Its rows are the person's own list, so no row names
    // its answerer; the card a row's Change answer opens is the ask's own, and that card names
    // the person with the agent beside them. The card opens on the form seeded with the answer,
    // and Cancel leaves it on the record it would have replaced.
    await page.goto("/answers");
    const answerRow = page.locator(`[data-answer-row="${seeded.ordinaryAsk}"]`);
    await expect(answerRow).toContainText("Read DISPATCH_INBOX_FLAG.");
    await answerRow.getByRole("button", { name: "Change answer" }).click();
    const answerCard = page.locator(`[data-answer-card="${seeded.ordinaryAsk}"]`);
    await expect(answerCard.getByLabel("Your answer")).toHaveValue("Read DISPATCH_INBOX_FLAG.");
    await answerCard.getByRole("button", { name: "Cancel" }).click();
    await expect(answerCard).toContainText(`Answered by ${actingThrough}`);

    // 4. A review made with the token is the person's review, with the agent beside it.
    const reviewed = await withToken(
      `/api/v1/artifacts/${seeded.reviewedArtifact}/reviews`,
      token,
      { state: "approved" }
    );
    expect(reviewed.status, await reviewed.text()).toBe(201);
    await page.goto(`/issues/${seeded.reviewedIssue}`);
    await page.getByRole("button", { name: "Approved v1" }).click();
    await expect(page.getByRole("dialog", { name: "Reviews" })).toContainText(
      `${actingThrough} approved v1`
    );

    // 5. A message sent with the token reaches the session, and the Agents page shows it as a
    // card naming the agent that sent it for her.
    const sent = await withToken(`/api/v1/agents/${worker.session_id}/messages`, token, {
      body: "Pause the backfill until the flag lands.",
      delivery: "aside",
    });
    expect(sent.status, await sent.text()).toBe(201);
    await page.goto("/agents");
    const workerCard = page
      .getByRole("region", { name: "Agents" })
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: worker.title }) });
    await workerCard.getByRole("button", { exact: true, name: worker.title }).click();
    const conversation = workerCard.getByRole("list", {
      name: `Conversation with ${worker.title}`,
    });
    await expect(conversation).toContainText("Pause the backfill until the flag lands.");
    await expect(conversation).toContainText(actingThrough);

    // 6. The Approve box on the token's own row is what lets it settle an approval. Cleared, the
    // approval request is refused for want of the scope; ticked again, the same answer lands.
    await page.goto("/settings");
    // What the page offers a new token: both boxes, which is what the token above was minted
    // with and what every act it made rested on.
    await expect(page.getByLabel("Act as you (new token)")).toBeChecked();
    await expect(page.getByLabel("Approve and control credentials (new token)")).toBeChecked();
    // The box follows the write rather than the press (`AgentTokensSection.tsx`), so the press is
    // a click and the state is read once the server has answered it.
    const tokenApprove = page.getByLabel("Approve and control credentials (Inbox reader)");
    await expect(tokenApprove).toBeChecked();
    await tokenApprove.click();
    await expect(tokenApprove).not.toBeChecked();
    await expect(page.getByLabel("Act as you (Inbox reader)")).toBeChecked();

    const approvalBody = { expected_edited_at: null, selected: ["Approve"] };
    const refused = await withToken(
      `/api/v1/asks/${seeded.approvalAsk}/answer`,
      token,
      approvalBody
    );
    expect(refused.status).toBe(403);
    expect(await refused.json()).toMatchObject({ code: "OAUTH_SCOPE_REQUIRED" });
    expect(refused.headers.get("WWW-Authenticate")).toBe(
      'Bearer error="insufficient_scope", scope="approve"'
    );
    // The refusal left the request open: nothing was approved on the strength of a credential
    // the person had just narrowed.
    expect((await getAsk(seeded.approvalAsk, { login: "alice" })).ask.state).toBe("open");

    await page.reload();
    const tokenApproveAgain = page.getByLabel("Approve and control credentials (Inbox reader)");
    await expect(tokenApproveAgain).not.toBeChecked();
    await tokenApproveAgain.click();
    await expect(tokenApproveAgain).toBeChecked();
    const approvedNow = await withToken(
      `/api/v1/asks/${seeded.approvalAsk}/answer`,
      token,
      approvalBody
    );
    expect(approvedNow.status, await approvedNow.text()).toBe(200);
    await page.goto(`/issues/${seeded.gateIssue}`);
    await expect(page.getByRole("button", { name: "Approved v1" })).toBeVisible();
  } finally {
    await alice.close();
    rmSync(home, { force: true, recursive: true });
  }
});

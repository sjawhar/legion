import type { Actor, ActorOrigin } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  answerAsk,
  createAgentMessage,
  createArtifactAsk,
  createAsk,
  createBroadcast,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getIssue,
  patchIssue,
  replyToCommentDelivery,
  replyToMessageDelivery,
  resolveAsk,
  snoozeAsk,
} from "./api";

// One realistic workspace, built through the public API, that fills every surface only a human
// acts on: the Inbox (open, snoozed and document asks), the Agents page (open rows and both
// folds), a direct message, a broadcast and an issue's Conversation. `e2e/workspace.e2e.ts`
// proves each surface renders non-empty.

// Four sessions carry no `last_seen`, which the fake Envoy answers as "live now" on every read, so
// they never age into Inactive however long an instance runs. The Archivist is the one that must:
// twenty minutes stale already, it only grows staler after this module is evaluated.
export const workspaceSessions: FakeSession[] = [
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    machine_id: "build-host",
    roles: ["planner"],
    session_id: "planner-session",
    title: "Planner",
  },
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/tester",
    machine_id: "build-host",
    roles: ["tester"],
    session_id: "tester-session",
    title: "Tester",
  },
  {
    capabilities: ["aside"],
    dir: "/workspaces/reviewer",
    machine_id: "review-host",
    roles: ["reviewer"],
    session_id: "reviewer-session",
    title: "Reviewer",
  },
  {
    capabilities: ["btw", "steer"],
    dir: "/workspaces/observer",
    machine_id: "build-host",
    roles: [],
    session_id: "observer-session",
    title: "Observer",
  },
  // Twenty minutes stale: the Agents page folds it under Inactive (`AgentsPage.tsx`'s
  // `INACTIVE_AFTER_MS`, ten minutes).
  {
    capabilities: ["aside", "btw"],
    dir: "/workspaces/archivist",
    last_seen: Date.now() - 20 * 60_000,
    machine_id: "review-host",
    roles: ["archivist"],
    session_id: "archivist-session",
    title: "Archivist",
  },
];
export const workspaceSessionIds = workspaceSessions.map((session) => session.session_id);

const workflowSpec =
  "# Local testing workflow\n\nOne command, a seeded corpus, a signed-in browser.\n\n" +
  ':::ask{urgency="high" multiple="false"}\nWhich sign-in path?\n\n' +
  "- Cookie: the production identity, minted by a dev route\n" +
  "- Header: a trusted proxy header\n:::\n";

export interface SeededWorkspace {
  readonly issues: Record<
    "workflow" | "child" | "broadcastOrder" | "snooze" | "folds" | "phone" | "retracted" | "rotate",
    string
  >;
  readonly asks: Record<
    "blocking" | "snoozed" | "answered" | "retracted" | "document" | "rotate",
    string
  >;
  readonly broadcast: { readonly id: string; readonly sessionIds: readonly string[] };
  readonly directMessage: string;
  /** Rows alice's Inbox lists (open asks on open issues and documents); the snoozed one is among
   *  them, under Later. The Inbox opens on Mine, which shows alice's issues' asks and the
   *  Unassigned band, and every issue here is alice's. */
  readonly inboxRows: number;
}

/** The spec's decision block becomes an ask only when the document service settles the spec,
 *  which happens after `createIssue` answers. The Inbox count is exact only once it has. */
async function waitForSpecAsk(issueKey: string): Promise<void> {
  const deadline = Date.now() + 15_000;
  for (;;) {
    if ((await getIssue(issueKey)).open_asks.some((ask) => ask.block_id)) return;
    if (Date.now() > deadline) {
      throw new Error(`seedWorkspace: ${issueKey}'s spec decision was not indexed within 15 s`);
    }
    const tick = Promise.withResolvers<void>();
    setTimeout(tick.resolve, 100);
    await tick.promise;
  }
}

/** Seeds the workspace into an empty database and the fake Envoy. Every write goes through the
 *  public API, as alice for a human and through the agent bearer for a session. A session's
 *  write carries its title in `origin`, as an agent's Dispatch tools stamp it (`toolActor` in
 *  `packages/envoy-client/src/dispatch-execute.ts`), so the surfaces that read the stamped title
 *  rather than the live registry (the Inbox's author chip) name it too. */
export async function seedWorkspace(): Promise<SeededWorkspace> {
  const actor = (id: string, origin: Omit<ActorOrigin, "session_title"> = {}): Actor => {
    const session = workspaceSessions.find((candidate) => candidate.session_id === id);
    if (session === undefined) throw new Error(`seedWorkspace: no workspace session ${id}`);
    return { id, kind: "session", origin: { ...origin, session_title: session.title } };
  };
  const as = (id: string) => ({ actor: actor(id), as: "agent" as const });
  const alice = { login: "alice" };

  await setLiveSessions(workspaceSessions);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" });

  // A human's issue is assigned to its creator, so every issue here is alice's.
  const workflow = await createIssue({
    project: "CORE",
    spec: workflowSpec,
    title: "Local testing workflow for Dispatch",
  });
  await patchIssue(workflow.key, { labels: ["dispatch"], priority: 1, status: "in_progress" });
  const child = await createIssue({
    parent: workflow.key,
    project: "CORE",
    title: "Seed the local corpus",
  });
  const broadcastOrder = await createIssue({
    project: "CORE",
    title: "Broadcast recipients keep their send order",
  });
  await patchIssue(broadcastOrder.key, { status: "needs_review" });
  const snooze = await createIssue({
    project: "CORE",
    title: "Snooze an Inbox row until tomorrow",
  });
  const folds = await createIssue({ project: "CORE", title: "Agents page freshness folds" });
  await patchIssue(folds.key, { status: "backlog" });
  const phone = await createIssue({ project: "CORE", title: "Phone layout of the review sheet" });
  await patchIssue(phone.key, { status: "done" });
  const retracted = await createIssue({
    project: "CORE",
    title: "A retracted ask stays in history",
  });
  const rotate = await createIssue({ project: "OPS", title: "Rotate the agent token" });
  await patchIssue(rotate.key, { status: "triage" });

  const blocking = await createAsk(
    folds.key,
    { question: "Fold a session unseen for ten minutes under Inactive?", urgency: "blocking" },
    { actor: actor("planner-session", { tmux: "legion:1.2" }), as: "agent" }
  );
  const snoozed = await createAsk(
    snooze.key,
    { question: "Snooze until tomorrow morning, or for a full day?", urgency: "high" },
    as("tester-session")
  );
  await snoozeAsk(snoozed.id, new Date(Date.now() + 24 * 3_600_000).toISOString(), alice);
  const answered = await createAsk(
    broadcastOrder.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the recipient-order fix?" },
    as("reviewer-session")
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] }, alice);
  const withdrawn = await createAsk(
    retracted.key,
    { question: "Keep the retracted ask out of the Inbox?" },
    as("planner-session")
  );
  await resolveAsk(
    withdrawn.id,
    { kind: "retracted", reason: "Answered live." },
    as("planner-session")
  );
  const rotation = await createAsk(
    rotate.key,
    { question: "Rotate the token before or after the deploy?", urgency: "med" },
    as("reviewer-session")
  );
  const runbook = await createProjectDocument("CORE", {
    content: "# Runbook\n\nRestart, reseed, reopen the signed-in URL.\n",
    name: "Runbook",
  });
  const documentAsk = await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    as("tester-session")
  );

  const review = await createComment(
    broadcastOrder.key,
    { body: "Reviewing the diff." },
    as("reviewer-session")
  );
  await createComment(broadcastOrder.key, {
    body: "Thanks - the order the sender ticked is the whole point.",
    reply_to: review.id,
  });
  const mention = await createComment(broadcastOrder.key, {
    body: "@Tester please re-run the broadcast spec",
    delivery: "btw",
    mentions: [{ target: "session:tester-session" }],
  });
  await replyToCommentDelivery(
    mention.id,
    { attempt: 1, body: "Re-ran: 2 passed." },
    actor("tester-session")
  );

  const directMessage = await createAgentMessage(
    "planner-session",
    { body: "How far is the plan?", delivery: "aside" },
    alice
  );
  await replyToMessageDelivery(
    directMessage.id,
    { attempt: 1, body: "Half-way; the seed is next." },
    actor("planner-session")
  );

  // Not alphabetical on purpose: the broadcast page must list its recipients in the order sent.
  const broadcast = await createBroadcast(
    {
      body: "Stand down and report status.",
      delivery: "steer",
      session_ids: ["tester-session", "observer-session", "planner-session"],
    },
    alice
  );
  if (broadcast.excluded.length > 0) {
    throw new Error(`seedWorkspace: the broadcast excluded ${JSON.stringify(broadcast.excluded)}`);
  }
  const plannerCopy = broadcast.recipients.find(
    (recipient) => recipient.session_id === "planner-session"
  );
  if (plannerCopy === undefined) throw new Error("seedWorkspace: the planner received no copy");
  await replyToMessageDelivery(
    plannerCopy.message.id,
    { attempt: 1, body: "Standing down; build is green." },
    actor("planner-session")
  );

  await waitForSpecAsk(workflow.key);
  return {
    asks: {
      answered: answered.id,
      blocking: blocking.id,
      document: documentAsk.id,
      retracted: withdrawn.id,
      rotate: rotation.id,
      snoozed: snoozed.id,
    },
    broadcast: {
      id: broadcast.id,
      sessionIds: broadcast.recipients.map((recipient) => recipient.session_id),
    },
    directMessage: directMessage.id,
    // blocking, snoozed, rotate, the spec's decision block, and the document ask.
    inboxRows: 5,
    issues: {
      broadcastOrder: broadcastOrder.key,
      child: child.key,
      folds: folds.key,
      phone: phone.key,
      retracted: retracted.key,
      rotate: rotate.key,
      snooze: snooze.key,
      workflow: workflow.key,
    },
  };
}

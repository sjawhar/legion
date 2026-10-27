import { buildDispatchReference } from "../features/refs/routes";
import {
  agentMessagesQuery,
  architectureSourcesQuery,
  inboxQuery,
  myRepliesKey,
  projectArchitectureSourcesQuery,
  projectDefaultAssigneesQuery,
  projectsQuery,
  userAgentStateQuery,
  userStateQuery,
} from "./queries";
import type { RefreshTarget } from "./query-refresh";
import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssueRef,
  Artifact,
  ChangedReference,
  DocumentReferenceSummary,
  Event,
  EventType,
  GraphReferences,
  InboxRow,
  IssueSummary,
  MyRepliesResponse,
  MyReplyRow,
} from "./types";

/**
 * Which cached reads one stream event can change, and nothing else: `eventRefreshTargets` is the
 * whole of it, and `api/query-refresh.ts` decides when each read is fetched. Query keys are the
 * literal arrays their readers use (the reference reads' builders live in
 * `features/refs/reference-target.ts`; `__tests__/sse.test.ts` holds the two in step).
 *
 * The server says what a write moved where a rule needs it: `changed` and `ancestors` on
 * `issue.created|updated|closed`, `thread_logins` on the writes that can move someone's Replies
 * count, `changed` on `user_state.updated`. An event without the field - written before it
 * existed, or by an older server mid-rollout - falls back to what any value could have moved.
 */

const inboxKey = inboxQuery().queryKey;
const projectsKey = projectsQuery().queryKey;
const pinnedKey = ["issues", "pinned"] as const;

/** One untyped-payload read for the fields the rules take off an event. */
function payloadValue(event: Event, key: string): unknown {
  const payload = event.payload;
  return typeof payload === "object" && payload !== null ? Reflect.get(payload, key) : undefined;
}

function payloadString(event: Event, key: string): string | undefined {
  const value = payloadValue(event, key);
  return typeof value === "string" ? value : undefined;
}

/** A list field the server writes, or undefined on an event written before it existed. */
function payloadStrings(event: Event, key: string): readonly string[] | undefined {
  const value = payloadValue(event, key);
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : undefined;
}

/** Whether `login` is the signed-in viewer's, compared as the server compares logins. */
function isViewer(login: string | undefined, viewer: string | undefined): boolean {
  return (
    login !== undefined &&
    viewer !== undefined &&
    login.trim().toLowerCase() === viewer.trim().toLowerCase()
  );
}

/** A list read's rows, as the reader that holds the query wrote them; none for a query with no
 *  list (an error, or not loaded). */
function rowsOf<Row>(data: unknown): readonly Row[] {
  return Array.isArray(data) ? (data as Row[]) : [];
}

/** The sidebar's pinned list, when it holds `key`: an issue the viewer has not pinned shows in no
 *  row of it. */
function pinnedRow(key: string): RefreshTarget {
  return {
    exact: true,
    queryKey: pinnedKey,
    when: (data) => rowsOf<IssueSummary>(data).some((issue) => issue.key === key),
  };
}

/** Every Inbox read (the Inbox and a project's) that holds a row of issue `key`'s open asks. */
function inboxRowsOf(key: string): RefreshTarget {
  return {
    queryKey: inboxKey,
    when: (data) => rowsOf<InboxRow>(data).some((row) => row.issue_key === key),
  };
}

/** Every Inbox read that holds the row of ask `id`. */
function inboxRowOfAsk(id: string): RefreshTarget {
  return {
    queryKey: inboxKey,
    when: (data) => rowsOf<InboxRow>(data).some((row) => row.id === id),
  };
}

/** The Replies page (`RepliesPage`'s `[...myRepliesKey, "page"]`, every page it loaded), when a
 *  row of it is a thread on issue `key`: each row shows its issue's title. */
function repliesPageRowsOf(key: string): RefreshTarget {
  return {
    exact: true,
    queryKey: [...myRepliesKey, "page"],
    when: (data) =>
      rowsOf<MyRepliesResponse>((data as { pages?: unknown } | undefined)?.pages).some((page) =>
        rowsOf<MyReplyRow>(page.rows).some((row) => row.issue?.key === key)
      ),
  };
}

/** The Architecture page's tree of `project`, when it lists issue `key`: in a component's work
 *  list, which shows the issue's title, status, priority, parent, links and `updated_at`, or among
 *  the unassigned, the not-architectural and the retired links, which show its title and status. */
function architectureListing(project: string, key: string): RefreshTarget {
  const lists = (rows: unknown) =>
    rowsOf<ArchitectureTreeIssueRef>(rows).some((issue) => issue.key === key);
  return {
    exact: true,
    queryKey: ["architecture", project],
    when: (data) => {
      const tree = data as Partial<ArchitectureTree> | undefined;
      return (
        rowsOf<ArchitectureTreeComponent>(tree?.components).some((component) =>
          lists(component.issues)
        ) ||
        lists(tree?.unassigned) ||
        lists(tree?.not_architectural) ||
        lists(tree?.retired_links)
      );
    },
  };
}

/** Every backlink panel (`ReferencedBy`'s `["references", "to", ref]`) whose read `matches`. */
function referencePanels(matches: (references: GraphReferences) => boolean): RefreshTarget {
  return {
    queryKey: ["references", "to"],
    when: (data) => data !== undefined && matches(data as GraphReferences),
  };
}

/** The backlink panel of exactly one reference, as `ReferencedBy` keys it. */
function referencePanel(reference: string): RefreshTarget {
  return { exact: true, queryKey: ["references", "to", reference] };
}

// The document an ask or comment event touches: a block ask names the document that holds its
// block, an anchored ask or comment names the document its mark lives in, and an ask or comment
// with neither touches no document at all.
function eventDocument(event: Event): string | undefined {
  switch (event.type) {
    case "ask.opened":
    case "ask.edited":
    case "ask.handed_back":
    case "ask.answered":
    case "ask.resolved":
    case "ask.anchor_refreshed":
      return event.payload.block_artifact?.id ?? event.payload.anchor?.artifact_id;
    case "comment.created":
    case "comment.delivery":
    case "comment.answered":
    case "comment.resolved":
    case "comment.reopened":
    case "comment.edited":
    case "comment.anchor_refreshed":
    case "suggestion.accepted":
    case "suggestion.rejected":
      return "anchor" in event.payload ? event.payload.anchor?.artifact_id : undefined;
    default:
      return undefined;
  }
}

// Follower events carry the ask under `ask_id`; every other ask event is the ask itself and
// carries its `id`. Both refresh the ask's read (a reference to it, an ask page) and its thread,
// which lists followers.
function appendAskReads(targets: RefreshTarget[], event: Event): void {
  const id =
    event.type === "ask.follower_added" || event.type === "ask.follower_removed"
      ? payloadString(event, "ask_id")
      : payloadString(event, "id");
  if (id !== undefined) {
    targets.push({ queryKey: ["ask", id] }, { queryKey: ["ask-thread", id] });
  }
}

// A comment event names the comment (a delivery receipt as `comment_id`), the root it replies to,
// whose read carries its replies, and the ask whose thread holds it.
function appendCommentReads(targets: RefreshTarget[], event: Event): void {
  const id =
    event.type === "comment.delivery" ? event.payload.comment_id : payloadString(event, "id");
  if (id !== undefined) {
    targets.push({ queryKey: ["comment", id] });
  }
  const root = payloadString(event, "reply_to");
  if (root !== undefined) {
    targets.push({ queryKey: ["comment", root] });
  }
  const askID = payloadString(event, "ask_id");
  if (askID !== undefined) {
    targets.push({ queryKey: ["ask", askID] }, { queryKey: ["ask-thread", askID] });
  }
}

/** The signed-in person's answers page lists every answer and every reply on an ask, so the
 *  two events that write one refresh it. The query exists only while that page is open. */
function appendAnswersPage(targets: RefreshTarget[], event: Event): void {
  if (
    event.type === "ask.answered" ||
    (event.type === "comment.created" && payloadString(event, "ask_id") !== undefined)
  ) {
    targets.push({ queryKey: ["me", "answers"] });
  }
}

// An ask or comment that is written or re-anchored in a document is an `anchored_to` edge into it,
// and one a project document owns an `owned_by` edge: that document's backlink panel lists both.
function appendDocumentPanel(targets: RefreshTarget[], event: Event): void {
  if (
    event.type !== "ask.opened" &&
    event.type !== "ask.anchor_refreshed" &&
    event.type !== "comment.created" &&
    event.type !== "comment.answered" &&
    event.type !== "comment.anchor_refreshed"
  ) {
    return;
  }
  const documents = new Set([eventDocument(event), event.artifact_id ?? undefined]);
  for (const id of documents) {
    if (id !== undefined) {
      targets.push(referencePanels(({ node }) => node.kind === "artifact" && node.id === id));
    }
  }
}

// The ask lifecycle events, listed rather than matched on the `ask.` prefix so a new one falls
// to the default branch instead of silently routing here.
function isAskEvent(event: Event): boolean {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.anchor_refreshed" ||
    event.type === "ask.edited" ||
    event.type === "ask.handed_back" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "ask.follower_added" ||
    event.type === "ask.follower_removed"
  );
}

/** The ask events that open or close an ask: the sidebar's per-project open-ask counts and an
 *  issue's pinned row (`open_asks`) move with them. An edit, a hand-back, an anchor refresh or a
 *  follower change moves neither. A block event can open or retract a decision block's ask. */
function movesOpenAsks(event: Event): boolean {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "block.repaired" ||
    event.type === "block.invalid"
  );
}

// The events that change a comment thread: creation, deliveries, lifecycle, suggestion
// verdicts, and a cascaded anchor refresh (a document version orphaned or re-anchored it).
function isCommentLikeEvent(event: Event): boolean {
  return (
    event.type === "comment.created" ||
    event.type === "comment.delivery" ||
    event.type === "comment.answered" ||
    event.type === "comment.resolved" ||
    event.type === "comment.reopened" ||
    event.type === "comment.edited" ||
    event.type === "comment.anchor_refreshed" ||
    event.type === "suggestion.accepted" ||
    event.type === "suggestion.rejected"
  );
}

/** What an Inbox row reads of an ask or comment event: every ask event moves its ask's row (a
 *  follower, a hand-back between sections, an edit), and a reply in an ask's thread moves its
 *  `last_reply` and `waiting_on`. A comment outside every ask thread, and every message, moves no
 *  row: both write paths file a reply under the ask at the head of its thread (`ask_id`), and a
 *  delivery receipt names that ask too. A citation of an ask moves its row's backlink count, which
 *  the targets the event names restore (`changedReferenceTargets`). */
function movesInboxRows(event: Event): boolean {
  return (
    isAskEvent(event) ||
    (isCommentLikeEvent(event) && payloadString(event, "ask_id") !== undefined) ||
    event.type === "artifact.approved" ||
    event.type === "artifact.changes_requested" ||
    event.type === "block.repaired" ||
    event.type === "block.invalid"
  );
}

// The Agents page holds one conversation per session; `session:<id>` with an id after the prefix
// is the only target shape that names one. Every other target (a role, a name that never
// resolved) belongs to no conversation on that page.
function agentConversationKey(target: string | undefined): readonly unknown[] | undefined {
  if (target?.startsWith("session:") && target.length > "session:".length) {
    return agentMessagesQuery(target.slice("session:".length)).queryKey;
  }
  return undefined;
}

function appendAgentConversation(targets: RefreshTarget[], target: string | undefined): void {
  const queryKey = agentConversationKey(target);
  if (queryKey !== undefined) {
    targets.push({ queryKey });
  }
}

function isMessageEvent(event: Event): boolean {
  return (
    event.type === "message.created" ||
    event.type === "message.delivery" ||
    event.type === "message.accepted" ||
    event.type === "message.answered"
  );
}

// The writes that fire a `user_threads` trigger - a comment, a session's reply to a mention
// (`comment.answered`), an issue message or a reply to one, an answer - and an ask opening
// (settlement restoring one) or being resolved, which decides whether the Inbox lists its thread
// instead. An edit, a resolution of a comment, a delivery or a claim moves no row.
const REPLIES_WRITES: Partial<Record<EventType, true>> = {
  "comment.created": true,
  "comment.answered": true,
  "message.created": true,
  "message.answered": true,
  "ask.opened": true,
  "ask.answered": true,
  "ask.resolved": true,
};

/**
 * Whether the viewer's Replies page (`["me", "replies"]`, its count and every owner's unread set)
 * can have moved: a write above, or an issue closing or reopening, which moves its open asks'
 * threads between the Inbox and the Replies page (`myRepliesUnread` in the server's replies.go).
 * `thread_logins` names whose rows the write moved, so only theirs, and the writer's own, refresh.
 * Without it every one of those writes refreshes, except an `issue.updated` that says it moved no
 * status: the server names the logins on every status move, a close and a reopen among them.
 */
function movesReplies(event: Event, viewer: string | undefined): boolean {
  const issueWrite = event.type === "issue.closed" || event.type === "issue.updated";
  if (REPLIES_WRITES[event.type] !== true && !issueWrite) {
    return false;
  }
  const logins = payloadStrings(event, "thread_logins");
  if (logins === undefined) {
    return !(event.type === "issue.updated" && issueChanges(event)?.has("status") === false);
  }
  if (viewer === undefined) {
    return true;
  }
  return (
    logins.some((login) => isViewer(login, viewer)) ||
    (event.actor.kind === "user" && isViewer(event.actor.id, viewer))
  );
}

/** The fields an `issue.created|updated|closed` write moved, or undefined when the event does not
 *  say, in which case any of them may have. */
function issueChanges(event: Event): ReadonlySet<string> | undefined {
  const changed = payloadStrings(event, "changed");
  return changed === undefined ? undefined : new Set(changed);
}

/**
 * Every event whose server path rewrites the issue's own reference edges: a document version or
 * upload, an ask's creation or edit (a settled ask block among them), a comment or message body -
 * an answer's reply body is indexed like any other - an anchor refresh, a follower change, a child
 * link, and an issue write that moved a structural edge (its parent, its blockers, its components)
 * or the title every one of them shows as its excerpt.
 */
function changesReferenceGraph(event: Event): boolean {
  switch (event.type) {
    case "artifact.created":
    case "artifact.version":
    case "ask.anchor_refreshed":
    case "ask.edited":
    case "ask.follower_added":
    case "ask.follower_removed":
    case "ask.opened":
    case "child.added":
    case "child.removed":
    case "comment.answered":
    case "comment.anchor_refreshed":
    case "comment.created":
    case "comment.edited":
    case "issue.created":
    case "message.answered":
    case "message.created":
      return true;
    case "issue.updated": {
      const changes = issueChanges(event);
      return (
        changes === undefined ||
        ["parent", "blocked_by", "components", "title"].some((field) => changes.has(field))
      );
    }
    default:
      return false;
  }
}

/**
 * What an `issue.created`, `issue.updated` or `issue.closed` of `key` refreshes beyond what every
 * event of the issue does.
 */
function issueWriteTargets(
  event: Event,
  key: string,
  project: string | undefined
): RefreshTarget[] {
  const changes = issueChanges(event);
  const moved = (field: string) => changes === undefined || changes.has(field);
  const created = event.type === "issue.created";
  const closed = event.type === "issue.closed";
  const ancestors = payloadStrings(event, "ancestors");
  const targets: RefreshTarget[] = [];

  // Issue details. Components, the repository and the parent chain are resolved on read and
  // inherited downward, so moving one changes every descendant's detail, which gets no event of
  // its own: every loaded detail refreshes. Otherwise every write moves the issue's `updated_at`,
  // the newest of which each ancestor's Children tab shows as its child's activity (`active_at`
  // of the whole subtree), and a status rolls up into every ancestor's progress and child counts:
  // every ancestor's detail refreshes. A new issue has no descendants.
  if (changes === undefined || ancestors === undefined) {
    targets.push({ queryKey: ["issue"] });
  } else if (!created && (moved("parent") || moved("components") || moved("repo"))) {
    targets.push({ queryKey: ["issue"] });
  } else {
    for (const ancestor of ancestors) {
      targets.push({ exact: true, queryKey: ["issue", ancestor] });
    }
  }

  const titleOrStatus = moved("title") || moved("status");
  // A reference to the issue, or to one of its documents, shows its title and status.
  if (titleOrStatus) {
    targets.push({ exact: true, queryKey: ["issue-summary", key] });
  }
  // The Reference picker's project list and the Agents page's open-issue picker list titles and
  // whether an issue is open.
  if (created || closed || titleOrStatus) {
    if (project !== undefined) {
      targets.push({ exact: true, queryKey: ["issues", project] });
    }
    targets.push({ exact: true, queryKey: ["agents", "issue-picker"] });
  }
  if (!created && titleOrStatus) {
    targets.push(pinnedRow(key));
  }
  // A row of the Replies page shows its thread's issue title.
  if (!created && moved("title")) {
    targets.push(repliesPageRowsOf(key));
  }

  // An Inbox row shows its issue's title, status, priority and assignee, and lists an open ask
  // only while its issue is open. A close drops the issue's rows, which a test against them finds.
  // A reopen (`closed_at` moved, the issue left open) puts its open asks back, and the Inbox holds
  // no row of a closed issue to test, so that refreshes it whole.
  if (!created) {
    if (moved("closed_at") && !closed) {
      targets.push({ queryKey: inboxKey });
    } else if (
      closed ||
      moved("title") ||
      moved("status") ||
      moved("priority") ||
      moved("assignee")
    ) {
      targets.push(inboxRowsOf(key));
    }
  }

  // The sidebar's per-project open-ask count counts the open asks of open issues: only a close and
  // a reopen move it. A new issue has no asks.
  if (closed || (created ? changes === undefined : moved("closed_at"))) {
    targets.push({ queryKey: projectsKey });
  }
  // A status move, a new attachment or a new parent moves a count in the component tree. Any
  // other write moves at least the issue's `updated_at`, which the tree shows where it lists the
  // issue.
  if (project !== undefined) {
    if (moved("status") || moved("parent") || moved("components")) {
      targets.push({ queryKey: ["components", project] }, { queryKey: ["architecture", project] });
    } else if (!created) {
      targets.push(architectureListing(project, key));
    }
  }

  // A `child_of` edge into the parent and a `blocked_by` edge into each blocker are rows of their
  // backlink panels: the new parent and blockers are the payload's, the old ones are the panels
  // that still list an edge from this issue.
  if (moved("parent") || moved("blocked_by")) {
    const parent = payloadString(event, "parent");
    const blockers = payloadStrings(event, "blocked_by") ?? [];
    targets.push(
      referencePanels(
        ({ edges, node }) =>
          node.kind === "issue" &&
          (node.id === parent ||
            blockers.includes(node.id) ||
            edges.some(
              (edge) =>
                edge.node.kind === "issue" &&
                edge.node.id === key &&
                (edge.kind === "child_of" || edge.kind === "blocked_by")
            ))
      )
    );
  }
  return targets;
}

/** What a project document's event refreshes: an ask, a comment, a version or a review on a
 *  document that belongs to no issue. */
function documentTargets(event: Event, viewer: string | undefined): RefreshTarget[] {
  if (event.artifact_id === null || event.artifact_id === undefined) {
    throw new Error("document event is missing its artifact id");
  }
  if (event.project === undefined) {
    throw new Error("document event is missing its project");
  }
  const id = event.artifact_id;
  const targets: RefreshTarget[] = [
    { queryKey: ["artifact", id] },
    { queryKey: ["project", event.project, "artifacts"] },
    // The document page reads the document by `project/slug`, which the event does not name; its
    // id is the one the event does.
    {
      queryKey: ["artifact-ref"],
      when: (data) => data === undefined || (data as Artifact).id === id,
    },
  ];
  // A reference to the document shows its name, kind and latest version. A new document can be
  // the one a reference that found nothing named (no data to test), so it refreshes those too.
  if (event.type === "artifact.version" || event.type === "artifact.created") {
    const created = event.type === "artifact.created";
    targets.push({
      queryKey: ["doc-summary", event.project],
      when: (data) =>
        (created && data === undefined) ||
        (data as DocumentReferenceSummary | undefined)?.id === id,
    });
  }
  if (isAskEvent(event)) {
    appendAskReads(targets, event);
  }
  if (isCommentLikeEvent(event)) {
    appendCommentReads(targets, event);
  }
  if (movesInboxRows(event)) {
    targets.push({ queryKey: inboxKey });
  }
  if (movesOpenAsks(event)) {
    targets.push({ queryKey: projectsKey });
  }
  if (movesReplies(event, viewer)) {
    targets.push({ queryKey: myRepliesKey });
  }
  appendAnswersPage(targets, event);
  appendDocumentPanel(targets, event);
  if (event.type === "subscription.removed") {
    targets.push({ queryKey: ["subscribers", id] });
  }
  return targets;
}

function ownerTargets(event: Event, viewer: string | undefined): RefreshTarget[] {
  if (
    event.type === "project.created" ||
    event.type === "project.updated" ||
    event.type === "settings.repo_project.updated"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      { queryKey: projectsKey },
      { queryKey: ["project", event.project] },
      { queryKey: ["issues", "project", event.project] },
      { queryKey: ["repo-projects"] },
      // A mapping write can change the repository every issue of its project resolves to (a new
      // default, one removed or moved away), which the issue header reads from the issue's
      // detail. The detail's key names no project, so every loaded detail refreshes; mapping
      // writes are rare settings changes.
      ...(event.type === "settings.repo_project.updated" ? [{ queryKey: ["issue"] }] : []),
    ];
  }
  // The default decides who a later create lands on; no issue already listed moves, so only
  // the Settings table's own read refreshes.
  if (event.type === "settings.project_default_assignee.updated") {
    return [{ queryKey: projectDefaultAssigneesQuery().queryKey }];
  }
  if (
    event.type === "settings.architecture_source.updated" ||
    event.type === "architecture.synced" ||
    event.type === "architecture.sync_failed"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      { queryKey: architectureSourcesQuery().queryKey },
      { queryKey: projectArchitectureSourcesQuery(event.project).queryKey },
      { queryKey: ["components", event.project] },
      // A re-import changes which components exist, so every count in the tree.
      { queryKey: ["architecture", event.project] },
    ];
  }
  if (event.type === "user_state.updated") {
    // A pin, a read mark, a dismissal or a snooze in another tab or device of this viewer: the
    // state's own read always (its `seq` moved), the pinned list only for a pin. Of these the Inbox
    // reads only a snooze, its row's `snoozed_until`, from which its Later band, its banner and the
    // Needs-you badge are all counted: the Inbox reads holding the ask's row, or every Inbox read
    // when the event names no ask. An event without `changed` predates snooze events.
    if (!isViewer(payloadString(event, "login"), viewer)) {
      return [];
    }
    const changed = payloadStrings(event, "changed");
    const targets: RefreshTarget[] = [{ queryKey: userStateQuery().queryKey }];
    if (changed === undefined || changed.includes("pinned")) {
      targets.push({ exact: true, queryKey: pinnedKey });
    }
    if (changed?.includes("snoozed") === true) {
      const askID = payloadString(event, "ask_id");
      targets.push(askID === undefined ? { queryKey: inboxKey } : inboxRowOfAsk(askID));
    }
    return targets;
  }
  if (event.type === "user_agent_state.updated") {
    // Another tab or device of this viewer read or cleared a conversation: refresh the badge.
    return isViewer(payloadString(event, "login"), viewer)
      ? [{ queryKey: userAgentStateQuery().queryKey }]
      : [];
  }
  if (event.type === "user_thread_state.updated") {
    // Another tab or device of this viewer read a thread: drop its row and its count here too.
    return isViewer(payloadString(event, "login"), viewer) ? [{ queryKey: myRepliesKey }] : [];
  }
  if (event.type === "subscription.remove_requested") {
    return [];
  }
  if (event.issue_key === null && isMessageEvent(event)) {
    const queryKey = agentConversationKey(payloadString(event, "target"));
    if (queryKey === undefined) {
      throw new Error("issue-less message event is missing its session target");
    }
    // Every recipient of a broadcast holds an issue-less targeted message, so every event
    // that moves a broadcast's recipient table arrives here. The open broadcast views are
    // refreshed by key prefix rather than by threading a broadcast id through the message,
    // delivery and reply payloads; nothing else is keyed under ["broadcast"], and the
    // queries only exist while a broadcast page is mounted. A reply in a direct conversation can
    // be unread for its asker, and the count lives in the viewer's per-agent state; every reply
    // row is written with a message.answered event, so only that event can move the count.
    return event.type === "message.answered"
      ? [{ queryKey }, { queryKey: ["broadcast"] }, { queryKey: userAgentStateQuery().queryKey }]
      : [{ queryKey }, { queryKey: ["broadcast"] }];
  }
  if (event.issue_key === null) {
    return documentTargets(event, viewer);
  }

  const key = event.issue_key;
  const project = event.project;
  const issueReference = buildDispatchReference({ key, kind: "issue" });
  const targets: RefreshTarget[] = [
    // The issue's own detail; a read under it (`["issue", key, "references"]`) has its own rule.
    { exact: true, queryKey: ["issue", key] },
    { queryKey: ["events", key] },
    // Every event an issue owns moves its `last_seq`, which a project list's unread dot reads.
    { queryKey: project === undefined ? ["issues", "project"] : ["issues", "project", project] },
  ];
  if (movesReplies(event, viewer)) {
    targets.push({ queryKey: myRepliesKey });
  }
  if (changesReferenceGraph(event)) {
    targets.push({ exact: true, queryKey: ["issue", key, "references"] });
  }

  if (
    event.type === "issue.created" ||
    event.type === "issue.updated" ||
    event.type === "issue.closed"
  ) {
    targets.push(...issueWriteTargets(event, key, project));
    return targets;
  }
  if (
    event.type === "issue.claimed" ||
    event.type === "issue.released" ||
    event.type === "issue.deployed"
  ) {
    // A claim changes only who holds the issue: its detail, its log, and the project lists that
    // render the claim. No rollup, ask count or component attachment moves with it. A deploy is a
    // line in the issue's log and nothing the lists or rollups read.
    return targets;
  }
  if (event.type === "artifact.created" || event.type === "artifact.version") {
    targets.push(
      { queryKey: ["artifacts", key] },
      { queryKey: ["comments", key] },
      // A reference to one of the issue's documents shows its name, kind and latest version.
      { exact: true, queryKey: ["issue-summary", key] }
    );
    if (event.type === "artifact.version") {
      targets.push({ queryKey: ["artifact", event.payload.artifact_id] });
    } else {
      // An uploaded document is an `attached_to` edge into its issue.
      targets.push(referencePanel(issueReference));
    }
    return targets;
  }
  if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
    targets.push(
      { queryKey: ["artifacts", key] },
      { queryKey: ["artifact", event.payload.artifact_id] },
      { queryKey: ["asks", key] },
      { queryKey: inboxKey }
    );
    if (event.payload.ask_id !== null) {
      targets.push(
        { queryKey: ["ask", event.payload.ask_id] },
        { queryKey: ["ask-thread", event.payload.ask_id] }
      );
    }
    return targets;
  }
  if (isAskEvent(event)) {
    targets.push({ queryKey: inboxKey });
    appendAskReads(targets, event);
    if (event.type === "ask.follower_added" || event.type === "ask.follower_removed") {
      return targets;
    }
    targets.push({ queryKey: ["asks", key] });
    if (movesOpenAsks(event)) {
      targets.push({ queryKey: projectsKey }, pinnedRow(key));
    }
    const document = eventDocument(event);
    if (document !== undefined) {
      targets.push({ queryKey: ["artifact", document] });
    }
    appendAnswersPage(targets, event);
    appendDocumentPanel(targets, event);
    return targets;
  }
  if (event.type === "block.repaired" || event.type === "block.invalid") {
    targets.push(
      { queryKey: ["asks", key] },
      { queryKey: projectsKey },
      { queryKey: inboxKey },
      pinnedRow(key)
    );
    return targets;
  }
  if (isCommentLikeEvent(event)) {
    targets.push({ queryKey: ["comments", key] });
    // A reply on an ask moves its `waiting_on`; the issue's ask list carries it, and a decision
    // block reads its turn from that list rather than from the Inbox. The Conversation's card
    // for that ask carries the reply itself.
    if (payloadString(event, "ask_id") !== undefined) {
      targets.push({ queryKey: ["asks", key] }, { queryKey: inboxKey });
    }
    const document = eventDocument(event);
    if (document !== undefined) {
      targets.push({ queryKey: ["artifact", document] });
    }
    appendCommentReads(targets, event);
    appendAnswersPage(targets, event);
    appendDocumentPanel(targets, event);
    return targets;
  }
  if (isMessageEvent(event)) {
    const target = payloadString(event, "target");
    appendAgentConversation(targets, target);
    // The Agents page groups a conversation under its thread root, so a reply belongs to that
    // root's conversation and not to a target of its own: a session answering a message aimed
    // at itself names no target at all, and the event carries the root's as `thread_target`.
    const threadTarget = payloadString(event, "thread_target");
    if (threadTarget !== target) {
      appendAgentConversation(targets, threadTarget);
    }
    // Every message read of the issue (a reference to a message, which shows the thread under
    // its root): a reply changes its root's read, and a payload need not name the root.
    targets.push({ queryKey: ["issue-messages", key] }, { queryKey: ["issue-message", key] });
    return targets;
  }
  if (event.type === "subscription.removed") {
    targets.push({ queryKey: ["subscribers", key] });
    return targets;
  }
  if (event.type === "child.added" || event.type === "child.removed") {
    // The child's `child_of` edge into this issue is a row of its backlink panel. Its detail
    // (the Children tab, the rollups), above, is all a child's status move changes here: the
    // child's own event names the ancestors further up.
    targets.push(referencePanel(issueReference));
    return targets;
  }
  if (event.type === "child.status") {
    return targets;
  }
  // The branches above name every type an issue owns, so only a type added later reaches here,
  // and it refreshes what any issue write could have moved.
  targets.push({ queryKey: ["issue"] }, { queryKey: projectsKey }, { queryKey: inboxKey });
  return targets;
}

/** What the server says one write moved in the reference graph, read the same way on every
 *  event; an event recorded before the field existed carries nothing. */
function changedReferences(event: Event): readonly ChangedReference[] {
  const changed = payloadValue(event, "references_changed");
  return Array.isArray(changed) ? (changed as ChangedReference[]) : [];
}

/**
 * The reads that carry a changed target's backlinks, which `refs.ReplaceCounted` names in the
 * transaction that appends the event: an issue's detail (its count), its references tab and its
 * panel; an ask's read, its thread, the list its row lives in (its issue's asks, or its
 * document's), the Inbox row showing its count, and its panel; a document's panel. Past the
 * server's cap an event names no targets, so every read that holds a count or a panel refreshes.
 */
function changedReferenceTargets(event: Event): RefreshTarget[] {
  if (payloadValue(event, "references_changed_truncated") === true) {
    return [
      { queryKey: ["issue"] },
      { queryKey: ["asks"] },
      { queryKey: ["ask-thread"] },
      { queryKey: ["artifact"] },
      { queryKey: inboxKey },
      { queryKey: ["references", "to"] },
    ];
  }
  const targets: RefreshTarget[] = [];
  for (const target of changedReferences(event)) {
    if (target.kind === "issue") {
      targets.push(
        { exact: true, queryKey: ["issue", target.id] },
        { exact: true, queryKey: ["issue", target.id, "references"] },
        referencePanel(buildDispatchReference({ key: target.id, kind: "issue" }))
      );
      continue;
    }
    if (target.kind === "artifact") {
      // A cited document's panel: the one opened on the address the server names (`ref`, as
      // `ReferencedBy` keys it), and one opened on another spelling of the same document, or any
      // when the entry names no address, found by the node its read reports.
      if (target.ref !== undefined) {
        targets.push(referencePanel(target.ref));
      }
      const id = target.id;
      targets.push(referencePanels(({ node }) => node.kind === "artifact" && node.id === id));
      continue;
    }
    const id = target.id;
    // An answered card reads its ask from the thread, which is fetched once (staleTime
    // Infinity): without it a citation added after the answer never moves its control.
    targets.push({ queryKey: ["ask", id] }, { queryKey: ["ask-thread", id] }, inboxRowOfAsk(id));
    if (target.issue_key !== undefined && target.issue_key !== null) {
      targets.push(
        { queryKey: ["asks", target.issue_key] },
        referencePanel(buildDispatchReference({ id, key: target.issue_key, kind: "ask" }))
      );
      continue;
    }
    // An ask on a project document is referenced under the document's slug, which the event
    // does not carry: its panel is found by the node it reads.
    if (target.artifact_id !== undefined && target.artifact_id !== null) {
      targets.push({ queryKey: ["artifact", target.artifact_id, "asks"] });
    }
    targets.push(referencePanels(({ node }) => node.kind === "ask" && node.id === id));
  }
  return targets;
}

/**
 * The reads one stream event can change, each a query key (a prefix unless `exact`) and, where
 * only some of a family's reads can have moved, a test of the read's data. `signedInLogin` is the
 * viewer's: the per-viewer events and the Replies rule read it.
 */
export function eventRefreshTargets(event: Event, signedInLogin?: string): RefreshTarget[] {
  return [...ownerTargets(event, signedInLogin), ...changedReferenceTargets(event)];
}

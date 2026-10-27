import { expect, test } from "bun:test";
import { QueryClient, type QueryKey } from "@tanstack/react-query";

import { eventRefreshTargets } from "../api/event-refresh";
import {
  architectureSourcesQuery,
  inboxQuery,
  myRepliesCountQuery,
  projectArchitectureSourcesQuery,
  projectIssuesQuery,
  projectsQuery,
  userStateQuery,
} from "../api/queries";
import { refreshQueries } from "../api/query-refresh";
import { mergeEventPages, prependEventToLog } from "../api/sse";
import type { Event, EventType } from "../api/types";
import {
  askQuery,
  commentQuery,
  documentSummaryQuery,
  issueQuery,
  issueSummaryQuery,
  messageQuery,
} from "../features/refs/reference-target";

// The page every rule is read against: one read of each family a Dispatch tab holds, under the
// key its own reader builds, for issue CORE-1 (child of CORE-0, grandchild of CORE-00) and for
// what sits beside it. A rule is what an event marks stale here, through the real scheduler.
const READS = {
  agentsPicker: { key: ["agents", "issue-picker"] },
  answers: { key: ["me", "answers"] },
  architecture: { key: ["architecture", "CORE"] },
  ask: { key: askQuery("ask-1").queryKey },
  askPanel: {
    data: { edges: [], node: { id: "ask-9", kind: "ask" } },
    key: ["references", "to", "dispatch://CORE-9/ask/ask-9"],
  },
  asks: { key: ["asks", "CORE-1"] },
  comment: { key: commentQuery("comment-1").queryKey },
  comments: { key: ["comments", "CORE-1"] },
  components: { key: ["components", "CORE"] },
  conversationMessages: { key: ["issue-messages", "CORE-1"] },
  detail: { key: issueQuery("CORE-1").queryKey },
  docAskPanel: {
    data: { edges: [], node: { id: "ask-7", kind: "ask" } },
    key: ["references", "to", "dispatch://CORE/artifact/plan-md/ask/ask-7"],
  },
  docSummary: { data: { id: "artifact-1" }, key: documentSummaryQuery("CORE", "plan-md").queryKey },
  documentPage: { data: { id: "artifact-1" }, key: ["artifact-ref", "CORE/plan-md"] },
  documentPanel: {
    data: { edges: [], node: { id: "artifact-1", kind: "artifact" } },
    key: ["references", "to", "dispatch://CORE/artifact/plan-md"],
  },
  grandparent: { key: issueQuery("CORE-00").queryKey },
  inbox: { key: inboxQuery().queryKey },
  issuePanel: {
    data: { edges: [], node: { id: "CORE-1", kind: "issue" } },
    key: ["references", "to", "dispatch://CORE-1"],
  },
  labelledProjectList: { key: projectIssuesQuery("CORE", ["ui"]).queryKey },
  log: { key: ["events", "CORE-1"] },
  message: { key: messageQuery("CORE-1", "message-1").queryKey },
  newParentPanel: {
    data: { edges: [], node: { id: "CORE-8", kind: "issue" } },
    key: ["references", "to", "dispatch://CORE-8"],
  },
  otherAsk: { key: askQuery("ask-9").queryKey },
  otherDocSummary: {
    data: { id: "artifact-2" },
    key: documentSummaryQuery("CORE", "notes-md").queryKey,
  },
  otherDocumentPage: { data: { id: "artifact-2" }, key: ["artifact-ref", "CORE/notes-md"] },
  otherIssueMessage: { key: messageQuery("CORE-9", "message-9").queryKey },
  otherProjectList: { key: projectIssuesQuery("OTHER", []).queryKey },
  parent: { key: issueQuery("CORE-0").queryKey },
  // CORE-1 is CORE-0's child: CORE-0's panel lists the `child_of` edge from it.
  parentPanel: {
    data: {
      edges: [{ direction: "in", kind: "child_of", node: { id: "CORE-1", kind: "issue" } }],
      node: { id: "CORE-0", kind: "issue" },
    },
    key: ["references", "to", "dispatch://CORE-0"],
  },
  parentSummary: { key: issueSummaryQuery("CORE-0").queryKey },
  pinned: { key: ["issues", "pinned"] },
  projectInbox: { key: ["inbox", "CORE"] },
  projectList: { key: projectIssuesQuery("CORE", []).queryKey },
  projects: { key: projectsQuery().queryKey },
  referencePicker: { key: ["issues", "CORE"] },
  referencesTab: { key: ["issue", "CORE-1", "references"] },
  replies: { key: myRepliesCountQuery().queryKey },
  repliesPage: { key: ["me", "replies", "page"] },
  rootComment: { key: commentQuery("root-1").queryKey },
  rootMessage: { key: messageQuery("CORE-1", "message-0").queryKey },
  // CORE-1's spec, one document, opened on two spellings of its address.
  specPanel: {
    data: { edges: [], node: { id: "artifact-3", kind: "artifact" } },
    key: ["references", "to", "dispatch://CORE-1/spec"],
  },
  specPanelBySlug: {
    data: { edges: [], node: { id: "artifact-3", kind: "artifact" } },
    key: ["references", "to", "dispatch://CORE-1/artifact/spec"],
  },
  summary: { key: issueSummaryQuery("CORE-1").queryKey },
  unrelatedDetail: { key: issueQuery("CORE-9").queryKey },
  unrelatedPanel: {
    data: { edges: [], node: { id: "CORE-9", kind: "issue" } },
    key: ["references", "to", "dispatch://CORE-9"],
  },
  userState: { key: userStateQuery().queryKey },
} satisfies Record<string, { data?: unknown; key: QueryKey }>;

type ReadName = keyof typeof READS;

interface PageOptions {
  /** Whether the viewer has CORE-1 pinned. */
  readonly pinned: boolean;
  /** Whether the Inbox lists CORE-1's ask-1 and CORE-9's ask-9. */
  readonly inboxHolds: boolean;
  /** Where the Architecture page's tree lists CORE-1: in a component's work list, among the
   *  unassigned issues, or nowhere. */
  readonly treeLists: "component" | "unassigned" | "nowhere";
  /** Whether the Replies page lists a thread on CORE-1. */
  readonly repliesHold: boolean;
}

function dataFor(name: ReadName, page: PageOptions): unknown {
  if (name === "pinned") {
    return page.pinned ? [{ key: "CORE-1" }] : [{ key: "CORE-5" }];
  }
  if (name === "inbox" || name === "projectInbox") {
    return page.inboxHolds
      ? [
          { id: "ask-1", issue_key: "CORE-1" },
          { id: "ask-9", issue_key: "CORE-9" },
        ]
      : [{ id: "ask-5", issue_key: "CORE-5" }];
  }
  if (name === "architecture") {
    const listed = [{ key: page.treeLists === "nowhere" ? "CORE-5" : "CORE-1" }];
    return {
      components: [{ id: "web", issues: page.treeLists === "unassigned" ? [] : listed }],
      not_architectural: [],
      retired_links: [],
      unassigned: page.treeLists === "unassigned" ? listed : [],
    };
  }
  if (name === "repliesPage") {
    const key = page.repliesHold ? "CORE-1" : "CORE-5";
    return {
      pageParams: [0],
      pages: [{ rows: [{ issue: { key, title: "Ship the release" }, thread_id: "root-1" }] }],
    };
  }
  const read: { data?: unknown; key: QueryKey } = READS[name];
  return read.data ?? { loaded: name };
}

/** The reads of the page `event` marks stale, signed in as alice. */
function refreshedReads(event: Event, page: Partial<PageOptions> = {}): ReadName[] {
  const options: PageOptions = {
    inboxHolds: true,
    pinned: true,
    repliesHold: true,
    treeLists: "component",
    ...page,
  };
  const queryClient = new QueryClient();
  const names = Object.keys(READS) as ReadName[];
  for (const name of names) {
    queryClient.setQueryData(READS[name].key, dataFor(name, options));
  }
  refreshQueries(queryClient, eventRefreshTargets(event, "alice"));
  return names.filter((name) => queryClient.getQueryState(READS[name].key)?.isInvalidated);
}

function event(
  type: Event["type"],
  payload: Record<string, unknown> = {},
  overrides: Partial<Event> = {}
): Event {
  return {
    actor: { id: "planner", kind: "session" },
    created_at: "2026-10-26T00:00:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: true,
    payload,
    project: "CORE",
    seq: 4,
    type,
    ...overrides,
  } as Event;
}

/** An `issue.*` payload as the server writes it: the issue after the write, flat. */
function issue(fields: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    assignee: "alice@example.com",
    blocked_by: [],
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    key: "CORE-1",
    parent: "CORE-0",
    priority: 1,
    project: "CORE",
    status: "in_progress",
    title: "Ship the release",
    ...fields,
  };
}

const documentOwner = { artifact_id: "artifact-1", issue_key: null, project: "CORE" } as const;

interface Rule {
  readonly event: Event;
  readonly page?: Partial<PageOptions>;
  readonly refreshes: readonly ReadName[];
  readonly spares: readonly ReadName[];
}

const rules: Record<string, Rule> = {
  "a retitle refreshes the issue, its summary, the lists and pages showing its title, and every ancestor's children":
    {
      event: event(
        "issue.updated",
        issue({ ancestors: ["CORE-0", "CORE-00"], changed: ["title"] })
      ),
      refreshes: [
        "detail",
        "log",
        "projectList",
        "labelledProjectList",
        "summary",
        "referencePicker",
        "agentsPicker",
        "pinned",
        "inbox",
        "projectInbox",
        "parent",
        "grandparent",
        "referencesTab",
        "architecture",
        "repliesPage",
      ],
      spares: [
        "unrelatedDetail",
        "parentSummary",
        "otherProjectList",
        "projects",
        "replies",
        "userState",
        "components",
        "message",
        "ask",
        "comment",
        "issuePanel",
        "parentPanel",
        "docSummary",
      ],
    },
  "a retitle leaves the pinned list, the Inbox, the Replies page and the Architecture page alone when none lists the issue":
    {
      event: event("issue.updated", issue({ ancestors: ["CORE-0"], changed: ["title"] })),
      page: { inboxHolds: false, pinned: false, repliesHold: false, treeLists: "nowhere" },
      refreshes: ["summary"],
      spares: ["pinned", "inbox", "projectInbox", "repliesPage", "architecture", "components"],
    },
  "a retitle refreshes the Architecture page that lists the issue as unassigned": {
    event: event("issue.updated", issue({ ancestors: [], changed: ["title"] })),
    page: { treeLists: "unassigned" },
    refreshes: ["architecture"],
    spares: ["components"],
  },
  "a status move that leaves the issue open refreshes every ancestor's rollup, and neither the project counts nor an Inbox that does not list it":
    {
      event: event(
        "issue.updated",
        issue({ ancestors: ["CORE-0", "CORE-00"], changed: ["status"], thread_logins: [] })
      ),
      page: { inboxHolds: false },
      refreshes: [
        "detail",
        "parent",
        "grandparent",
        "summary",
        "pinned",
        "components",
        "architecture",
        "referencePicker",
        "agentsPicker",
      ],
      spares: [
        "inbox",
        "projectInbox",
        "projects",
        "unrelatedDetail",
        "replies",
        "parentPanel",
        "referencesTab",
        "parentSummary",
      ],
    },
  "a reopen refreshes the whole Inbox and the project counts, since its open asks come back": {
    event: event(
      "issue.updated",
      issue({
        ancestors: ["CORE-0", "CORE-00"],
        changed: ["status", "closed_at"],
        closed_at: null,
        status: "backlog",
        thread_logins: [],
      })
    ),
    page: { inboxHolds: false },
    refreshes: [
      "detail",
      "parent",
      "grandparent",
      "summary",
      "pinned",
      "inbox",
      "projectInbox",
      "projects",
      "components",
      "architecture",
      "referencePicker",
      "agentsPicker",
    ],
    spares: ["unrelatedDetail", "replies", "parentPanel", "referencesTab", "parentSummary"],
  },
  "a close refreshes the Inbox only where it lists the issue, and the Replies of those it moves": {
    event: event(
      "issue.closed",
      issue({
        ancestors: ["CORE-0"],
        changed: ["status", "closed_at"],
        closed_at: "2026-10-26T00:00:00Z",
        status: "done",
        thread_logins: ["alice"],
      })
    ),
    page: { inboxHolds: false },
    refreshes: ["detail", "parent", "summary", "projects", "replies", "agentsPicker"],
    spares: ["inbox", "projectInbox", "unrelatedDetail"],
  },
  "a close that moves someone else's Replies leaves the viewer's alone": {
    event: event(
      "issue.closed",
      issue({
        ancestors: [],
        changed: ["status", "closed_at"],
        closed_at: "2026-10-26T00:00:00Z",
        status: "done",
        thread_logins: ["bob"],
      })
    ),
    refreshes: ["inbox", "projects"],
    spares: ["replies"],
  },
  "a reparent refreshes every issue detail and the panels of the old parent and the new": {
    event: event(
      "issue.updated",
      issue({ ancestors: ["CORE-8"], changed: ["parent"], parent: "CORE-8" })
    ),
    refreshes: [
      "detail",
      "parent",
      "grandparent",
      "unrelatedDetail",
      "parentPanel",
      "newParentPanel",
      "components",
      "architecture",
      "referencesTab",
    ],
    spares: ["unrelatedPanel", "issuePanel", "summary", "projects", "inbox", "pinned"],
  },
  "a component change refreshes every issue detail and the component tree": {
    event: event("issue.updated", issue({ ancestors: ["CORE-0"], changed: ["components"] })),
    refreshes: ["unrelatedDetail", "components", "architecture"],
    spares: ["projects", "summary", "inbox", "pinned"],
  },
  "an issue write that does not say what it changed refreshes as broadly as before": {
    event: event("issue.updated", issue()),
    refreshes: [
      "detail",
      "unrelatedDetail",
      "summary",
      "referencePicker",
      "agentsPicker",
      "pinned",
      "inbox",
      "projects",
      "replies",
      "repliesPage",
      "components",
      "architecture",
      "referencesTab",
      "parentPanel",
    ],
    spares: ["otherProjectList", "userState"],
  },
  "a priority change refreshes the Inbox row, the work list showing it, every ancestor's children, and nothing that shows no priority":
    {
      event: event("issue.updated", issue({ ancestors: ["CORE-0"], changed: ["priority"] })),
      refreshes: ["detail", "projectList", "inbox", "parent", "architecture"],
      spares: [
        "summary",
        "pinned",
        "projects",
        "grandparent",
        "unrelatedDetail",
        "referencePicker",
        "agentsPicker",
        "components",
        "repliesPage",
      ],
    },
  "a write of any field refreshes every ancestor's children, which show the subtree's newest activity":
    {
      event: event(
        "issue.updated",
        issue({ ancestors: ["CORE-0", "CORE-00"], changed: ["labels"] })
      ),
      page: { treeLists: "nowhere" },
      refreshes: ["detail", "parent", "grandparent"],
      spares: ["unrelatedDetail", "parentSummary", "summary", "architecture", "components"],
    },
  "an issue write that does not name its ancestors refreshes every issue detail": {
    event: event("issue.updated", issue({ changed: ["priority"] })),
    refreshes: ["detail", "parent", "grandparent", "unrelatedDetail"],
    spares: ["summary", "pinned"],
  },
  "an assignee change leaves an Inbox that does not list the issue alone": {
    event: event("issue.updated", issue({ ancestors: [], changed: ["assignee"] })),
    page: { inboxHolds: false },
    refreshes: ["detail"],
    spares: ["inbox", "projectInbox"],
  },
  "a new issue refreshes its ancestors' rollups and the pickers, not every detail or the project counts":
    {
      event: event(
        "issue.created",
        issue({
          ancestors: ["CORE-0"],
          changed: ["title", "status", "parent", "components", "repo"],
        })
      ),
      refreshes: [
        "detail",
        "parent",
        "summary",
        "referencePicker",
        "agentsPicker",
        "components",
        "architecture",
        "referencesTab",
        "projectList",
        "parentPanel",
      ],
      spares: ["unrelatedDetail", "grandparent", "projects", "pinned", "inbox", "replies"],
    },
  "a child's status move refreshes its parent's detail alone": {
    event: event(
      "child.status",
      { child_key: "CORE-1", from: "todo", to: "in_progress" },
      { issue_key: "CORE-0" }
    ),
    refreshes: ["parent", "projectList"],
    spares: [
      "detail",
      "grandparent",
      "unrelatedDetail",
      "parentSummary",
      "projects",
      "inbox",
      "pinned",
      "parentPanel",
    ],
  },
  "a child added refreshes its parent's backlink panel": {
    event: event("child.added", { child_key: "CORE-1" }, { issue_key: "CORE-0" }),
    refreshes: ["parent", "parentPanel"],
    spares: ["issuePanel", "detail"],
  },
  "a claim refreshes the issue and its project list only": {
    event: event("issue.claimed", {
      claim: { actor: { id: "planner", kind: "session" }, at: "2026-10-26T00:00:00Z" },
      key: "CORE-1",
      reason: "claimed",
      status: "in_progress",
    }),
    refreshes: ["detail", "log", "projectList"],
    spares: ["summary", "pinned", "inbox", "referencePicker", "projects", "replies", "userState"],
  },
  "a message refreshes the issue's message reads, and the Replies of those in its thread": {
    event: event("message.created", { id: "message-2", thread_logins: ["alice"] }),
    refreshes: ["detail", "log", "message", "rootMessage", "conversationMessages", "replies"],
    spares: ["otherIssueMessage", "inbox", "pinned", "projects", "ask", "summary", "userState"],
  },
  "a message in a thread the viewer has no row in leaves the viewer's Replies alone": {
    event: event("message.created", { id: "message-2", thread_logins: ["bob"] }),
    refreshes: ["message"],
    spares: ["replies"],
  },
  "the viewer's own message refreshes the viewer's Replies": {
    event: event(
      "message.created",
      { id: "message-2", thread_logins: [] },
      { actor: { id: "alice", kind: "user" } }
    ),
    refreshes: ["replies"],
    spares: [],
  },
  "a message that does not say whose Replies it moves refreshes the viewer's": {
    event: event("message.answered", { id: "message-2", in_reply_to: "message-0" }),
    refreshes: ["replies", "rootMessage"],
    spares: ["inbox"],
  },
  "a reply refreshes the comment it replies to, and no Inbox row": {
    event: event("comment.created", {
      anchor: null,
      ask_id: null,
      id: "comment-1",
      reply_to: "root-1",
      thread_logins: [],
    }),
    refreshes: ["comment", "rootComment", "comments"],
    spares: ["inbox", "projectInbox", "ask", "asks", "answers", "replies"],
  },
  "a reply on an ask refreshes the ask, its Inbox row and the answers page": {
    event: event("comment.created", { anchor: null, ask_id: "ask-1", id: "comment-2" }),
    refreshes: ["ask", "inbox", "projectInbox", "asks", "answers"],
    spares: ["projects", "pinned"],
  },
  "an ask opening refreshes the Inbox, the project counts and the pinned row": {
    event: event("ask.opened", { anchor: null, id: "ask-1", thread_logins: [] }),
    refreshes: ["ask", "inbox", "projects", "pinned", "asks", "referencesTab"],
    spares: ["summary", "unrelatedDetail", "replies"],
  },
  "an ask edit moves its Inbox row and no count": {
    event: event("ask.edited", { anchor: null, id: "ask-1" }),
    refreshes: ["ask", "inbox"],
    spares: ["projects", "pinned"],
  },
  "a citation refreshes the cited issue's count, its references and its panel": {
    event: event("message.created", {
      id: "message-2",
      references_changed: [{ id: "CORE-9", kind: "issue" }],
    }),
    refreshes: ["unrelatedDetail", "unrelatedPanel"],
    spares: ["issuePanel", "parentPanel", "inbox"],
  },
  "a citation of an ask refreshes its reads, its Inbox row and its panel": {
    event: event("message.created", {
      id: "message-2",
      references_changed: [{ id: "ask-9", issue_key: "CORE-9", kind: "ask" }],
    }),
    refreshes: ["otherAsk", "askPanel", "inbox"],
    spares: ["ask", "unrelatedPanel"],
  },
  "a citation of an ask the Inbox does not list leaves the Inbox alone": {
    event: event("message.created", {
      id: "message-2",
      references_changed: [{ id: "ask-9", issue_key: "CORE-9", kind: "ask" }],
    }),
    page: { inboxHolds: false },
    refreshes: ["otherAsk"],
    spares: ["inbox", "projectInbox"],
  },
  "a citation of a project document's ask refreshes that ask's panel": {
    event: event("comment.edited", {
      anchor: null,
      id: "comment-1",
      references_changed: [{ artifact_id: "artifact-1", id: "ask-7", kind: "ask" }],
    }),
    refreshes: ["docAskPanel"],
    spares: ["askPanel", "documentPanel"],
  },
  "a citation of a project document refreshes the panel opened on its address": {
    event: event("comment.created", {
      anchor: null,
      id: "comment-2",
      references_changed: [
        { id: "artifact-1", kind: "artifact", ref: "dispatch://CORE/artifact/plan-md" },
      ],
    }),
    refreshes: ["documentPanel"],
    spares: ["specPanel", "specPanelBySlug", "docAskPanel", "issuePanel", "unrelatedPanel"],
  },
  "a citation of an issue's spec refreshes its panel under every spelling of the document": {
    event: event("message.created", {
      id: "message-2",
      references_changed: [{ id: "artifact-3", kind: "artifact", ref: "dispatch://CORE-1/spec" }],
    }),
    refreshes: ["specPanel", "specPanelBySlug"],
    spares: ["documentPanel", "issuePanel"],
  },
  "a citation of a document that names no address refreshes the panel whose read is that document":
    {
      event: event("comment.edited", {
        anchor: null,
        id: "comment-1",
        references_changed: [{ id: "artifact-1", kind: "artifact" }],
      }),
      refreshes: ["documentPanel"],
      spares: ["specPanel", "docAskPanel"],
    },
  "past the server's cap, every count and every panel refreshes": {
    event: event("message.created", { id: "message-2", references_changed_truncated: true }),
    refreshes: [
      "unrelatedDetail",
      "parent",
      "issuePanel",
      "unrelatedPanel",
      "documentPanel",
      "inbox",
    ],
    spares: ["projects"],
  },
  "the viewer's pin in another tab refreshes the pinned list": {
    event: event(
      "user_state.updated",
      { changed: ["pinned"], login: "alice", state: {} },
      { issue_key: null }
    ),
    refreshes: ["userState", "pinned"],
    spares: ["inbox", "projectInbox", "detail"],
  },
  "the viewer's read mark in another tab leaves the pinned list alone": {
    event: event(
      "user_state.updated",
      { changed: ["last_read_seq"], login: "alice", state: {} },
      { issue_key: null }
    ),
    refreshes: ["userState"],
    spares: ["pinned", "inbox"],
  },
  "the viewer's state write that does not say what it changed refreshes the pinned list": {
    event: event("user_state.updated", { login: "alice", state: {} }, { issue_key: null }),
    refreshes: ["userState", "pinned"],
    spares: ["inbox"],
  },
  "the viewer's snooze in another tab refreshes the Inbox reads that list the ask": {
    event: event(
      "user_state.updated",
      { ask_id: "ask-1", changed: ["snoozed"], login: "alice", state: {} },
      { issue_key: null }
    ),
    refreshes: ["userState", "inbox", "projectInbox"],
    spares: ["pinned", "detail", "replies"],
  },
  "the viewer's snooze of an ask no Inbox read lists leaves them alone": {
    event: event(
      "user_state.updated",
      { ask_id: "ask-1", changed: ["snoozed"], login: "alice", state: {} },
      { issue_key: null }
    ),
    page: { inboxHolds: false },
    refreshes: ["userState"],
    spares: ["inbox", "projectInbox", "pinned"],
  },
  "the viewer's snooze that names no ask refreshes every Inbox read": {
    event: event(
      "user_state.updated",
      { changed: ["snoozed"], login: "alice", state: {} },
      { issue_key: null }
    ),
    page: { inboxHolds: false },
    refreshes: ["userState", "inbox", "projectInbox"],
    spares: ["pinned"],
  },
  "another person's snooze refreshes nothing": {
    event: event(
      "user_state.updated",
      { ask_id: "ask-1", changed: ["snoozed"], login: "bob", state: {} },
      { issue_key: null }
    ),
    refreshes: [],
    spares: ["userState", "inbox", "projectInbox"],
  },
  "another person's state write refreshes nothing": {
    event: event(
      "user_state.updated",
      { changed: ["pinned"], login: "bob", state: {} },
      { issue_key: null }
    ),
    refreshes: [],
    spares: ["userState", "pinned", "inbox"],
  },
  "a version of an issue's document refreshes the issue's reference summary": {
    event: event("artifact.version", { artifact_id: "artifact-3", name: "Plan" }),
    refreshes: ["summary", "detail"],
    spares: ["projects", "inbox", "docSummary", "parentSummary"],
  },
  "a version of a project document refreshes that document's summary and page alone": {
    event: event("artifact.version", { artifact_id: "artifact-1", name: "Plan" }, documentOwner),
    refreshes: ["docSummary", "documentPage"],
    spares: ["otherDocSummary", "otherDocumentPage", "projects", "inbox", "detail", "summary"],
  },
  "a comment anchored in a project document refreshes that document's panel": {
    event: event(
      "comment.created",
      { anchor: { artifact_id: "artifact-1" }, ask_id: null, id: "comment-1" },
      documentOwner
    ),
    refreshes: ["documentPanel", "comment"],
    spares: ["inbox", "projects", "issuePanel"],
  },
  "a repository mapping refreshes every issue detail": {
    event: event(
      "settings.repo_project.updated",
      { deleted: false, mapping: { default: true, project: "CORE", repo: "acme/widgets" } },
      { issue_key: null }
    ),
    refreshes: ["detail", "unrelatedDetail", "projects", "projectList"],
    spares: ["inbox", "pinned"],
  },
};

test.each(Object.entries(rules))("%s", (_name, rule) => {
  const refreshed = refreshedReads(rule.event, rule.page);
  for (const name of rule.refreshes) {
    expect({ read: name, refreshed: refreshed.includes(name) }).toEqual({
      read: name,
      refreshed: true,
    });
  }
  for (const name of rule.spares) {
    expect({ read: name, refreshed: refreshed.includes(name) }).toEqual({
      read: name,
      refreshed: false,
    });
  }
});

// Settings lists every project's sources, the project page and an issue's components line read
// the project's own list, and the Architecture pane and the components picker read the model;
// each architecture event names one source and refreshes all of them for its project.
test("an architecture event refreshes the source lists and the project's model", () => {
  for (const incoming of [
    event(
      "settings.architecture_source.updated",
      { deleted: true, source: { project: "CORE", repo: "acme/infra" } },
      { issue_key: null }
    ),
    event(
      "architecture.synced",
      { branch: "main", commit: "c0ffee", components: 2, repo: "acme/infra" },
      { issue_key: null }
    ),
    event(
      "architecture.sync_failed",
      { branch: "main", error: "broken", repo: "acme/infra" },
      { issue_key: null }
    ),
  ]) {
    expect(eventRefreshTargets(incoming, "alice").map((target) => target.queryKey)).toEqual([
      architectureSourcesQuery().queryKey,
      projectArchitectureSourcesQuery("CORE").queryKey,
      ["components", "CORE"],
      ["architecture", "CORE"],
    ]);
  }
});

/** One event of every type the stream can carry, keyed by type so a new one fails to compile here
 *  until it has a row. */
const eventsByType: Record<EventType, Event> = {
  "architecture.sync_failed": event("architecture.sync_failed", {}, { issue_key: null }),
  "architecture.synced": event("architecture.synced", {}, { issue_key: null }),
  "artifact.approved": event("artifact.approved", { artifact_id: "artifact-3", ask_id: null }),
  "artifact.changes_requested": event("artifact.changes_requested", {
    artifact_id: "artifact-3",
    ask_id: "ask-1",
  }),
  "artifact.created": event("artifact.created", { artifact: { id: "artifact-3" } }),
  "artifact.version": event("artifact.version", { artifact_id: "artifact-3" }),
  "ask.anchor_refreshed": event("ask.anchor_refreshed", { anchor: null, id: "ask-1" }),
  "ask.answered": event("ask.answered", { anchor: null, id: "ask-1" }),
  "ask.edited": event("ask.edited", { anchor: null, id: "ask-1" }),
  "ask.follower_added": event("ask.follower_added", { ask_id: "ask-1", session_id: "s1" }),
  "ask.follower_removed": event("ask.follower_removed", { ask_id: "ask-1", session_id: "s1" }),
  "ask.handed_back": event("ask.handed_back", { anchor: null, id: "ask-1" }),
  "ask.opened": event("ask.opened", { anchor: null, id: "ask-1" }),
  "ask.resolved": event("ask.resolved", { anchor: null, id: "ask-1" }),
  "block.invalid": event("block.invalid", { block_id: "b-1" }),
  "block.repaired": event("block.repaired", { block_id: "b-1" }),
  "child.added": event("child.added", { child_key: "CORE-2" }),
  "child.removed": event("child.removed", { child_key: "CORE-2" }),
  "child.status": event("child.status", { child_key: "CORE-2", from: "todo", to: "done" }),
  "comment.anchor_refreshed": event("comment.anchor_refreshed", { anchor: null, id: "comment-1" }),
  "comment.answered": event("comment.answered", { anchor: null, id: "comment-1" }),
  "comment.created": event("comment.created", { anchor: null, id: "comment-1" }),
  "comment.delivery": event("comment.delivery", { comment_id: "comment-1" }),
  "comment.edited": event("comment.edited", { anchor: null, id: "comment-1" }),
  "comment.reopened": event("comment.reopened", { anchor: null, id: "comment-1" }),
  "comment.resolved": event("comment.resolved", { anchor: null, id: "comment-1" }),
  "issue.claimed": event("issue.claimed", { key: "CORE-1" }),
  "issue.closed": event("issue.closed", issue({ status: "done" })),
  "issue.created": event("issue.created", issue()),
  "issue.deployed": event("issue.deployed", { number: 1, repo: "acme/widgets" }),
  "issue.released": event("issue.released", { key: "CORE-1" }),
  "issue.updated": event("issue.updated", issue()),
  "message.accepted": event("message.accepted", { target: "session:planner" }),
  "message.answered": event("message.answered", { id: "message-2" }),
  "message.created": event("message.created", { id: "message-2" }),
  "message.delivery": event("message.delivery", { target: "session:planner" }),
  "project.created": event("project.created", {}, { issue_key: null }),
  "project.updated": event("project.updated", {}, { issue_key: null }),
  "settings.architecture_source.updated": event(
    "settings.architecture_source.updated",
    {},
    { issue_key: null }
  ),
  "settings.project_default_assignee.updated": event(
    "settings.project_default_assignee.updated",
    {},
    { issue_key: null }
  ),
  "settings.repo_project.updated": event("settings.repo_project.updated", {}, { issue_key: null }),
  "subscription.remove_requested": event("subscription.remove_requested", { session_id: "s1" }),
  "subscription.removed": event("subscription.removed", { session_id: "s1" }),
  "suggestion.accepted": event("suggestion.accepted", { anchor: null, id: "comment-1" }),
  "suggestion.rejected": event("suggestion.rejected", { anchor: null, id: "comment-1" }),
  "user_agent_state.updated": event(
    "user_agent_state.updated",
    { login: "alice" },
    { issue_key: null }
  ),
  "user_state.updated": event("user_state.updated", { login: "alice" }, { issue_key: null }),
  "user_thread_state.updated": event(
    "user_thread_state.updated",
    { login: "alice", thread_id: "c1" },
    { issue_key: null }
  ),
};

test.each(
  Object.entries(eventsByType).filter(
    ([type, incoming]) => incoming.issue_key !== null && type !== "subscription.remove_requested"
  )
)("%s refreshes its issue's detail, its log and its project list, and no other issue's detail", (_type, incoming) => {
  const refreshed = refreshedReads(incoming);
  expect(refreshed).toContain("detail");
  expect(refreshed).toContain("log");
  expect(refreshed).toContain("projectList");
  expect(refreshed).not.toContain("otherProjectList");
});

test("SSE event prepends a newer event to the loaded log page", () => {
  const client = new QueryClient();
  client.setQueryData(["events", "CORE-1"], {
    pageParams: [null],
    pages: [[event("message.created", {}, { id: 7, seq: 7 })]],
  });
  prependEventToLog(client, event("message.created", {}, { id: 8, seq: 8 }));
  expect(client.getQueryData<{ pages: Event[][] }>(["events", "CORE-1"])?.pages[0]).toEqual([
    event("message.created", {}, { id: 8, seq: 8 }),
    event("message.created", {}, { id: 7, seq: 7 }),
  ]);
});

test("a log page that predates streamed events keeps them in front of it", () => {
  const seq = (n: number) => event("message.created", { body: `m${n}` }, { id: n, seq: n });
  expect(
    mergeEventPages(
      { pageParams: [null], pages: [[seq(9), seq(8), seq(7)]] },
      { pageParams: [null], pages: [[seq(7), seq(6)]] }
    )
  ).toEqual({ pageParams: [null], pages: [[seq(9), seq(8), seq(7), seq(6)]] });
  // A response at or beyond the cached head is the newer truth and replaces the page whole -
  // including an event the stream had at an older shape.
  const edited = event("message.created", { body: "m9 edited" }, { id: 9, seq: 9 });
  expect(
    mergeEventPages(
      { pageParams: [null], pages: [[seq(9), seq(8)]] },
      { pageParams: [null], pages: [[edited, seq(8), seq(7)]] }
    )
  ).toEqual({ pageParams: [null], pages: [[edited, seq(8), seq(7)]] });
  expect(mergeEventPages(undefined, { pageParams: [null], pages: [[seq(7)]] })).toEqual({
    pageParams: [null],
    pages: [[seq(7)]],
  });
});

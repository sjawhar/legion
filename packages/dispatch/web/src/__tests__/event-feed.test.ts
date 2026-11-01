import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, type QueryKey } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { api } from "../api/client";
import {
  markAskThreadsSeededByAnInFlightInbox,
  mergeEventPages,
  prependEventToLog,
} from "../api/event-feed";
import { askThreadsIssue, eventRefreshTargets } from "../api/event-refresh";
import {
  architectureSourcesQuery,
  inboxQuery,
  issueQuery,
  myRepliesCountQuery,
  projectArchitectureSourcesQuery,
  projectIssuesQuery,
  projectsQuery,
  userStateQuery,
} from "../api/queries";
import { refreshQueries } from "../api/query-refresh";
import type { Ask, AskRead, Event, EventType, InboxRow } from "../api/types";
import { hasPendingAskThreadInvalidation } from "../features/inbox/ask-thread-freshness";
import { issueAsk } from "../features/inbox/inbox-fixture";
import { useAskAnswerForm } from "../features/inbox/useAskAnswerForm";
import {
  askQuery,
  blockQuery,
  commentQuery,
  documentSummaryQuery,
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
  // Block b2 of CORE-1's comment-1, read by a link to it and, pinned to the comment's revision 1,
  // by another link.
  commentBlock: {
    data: { edges: [], node: { id: "comment/comment-1#b2", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/comment/comment-1#b-b2").queryKey,
  },
  commentBlockPinned: {
    data: { edges: [], node: { id: "comment/comment-1#b2", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/comment/comment-1#b-b2@v1").queryKey,
  },
  comments: { key: ["comments", "CORE-1"] },
  components: { key: ["components", "CORE"] },
  conversationMessages: { key: ["issue-messages", "CORE-1"] },
  detail: { key: issueQuery("CORE-1").queryKey },
  docAskPanel: {
    data: { edges: [], node: { id: "ask-7", kind: "ask" } },
    key: ["references", "to", "dispatch://CORE/artifact/plan-md/ask/ask-7"],
  },
  // A project document's ask list and its block listing: the first's rows are asks.
  docAsks: {
    data: [{ id: "ask-7", question: "Ship it?" }],
    key: ["artifact", "artifact-1", "asks"],
  },
  docBlocks: {
    data: [{ from: 0, id: "intro", to: 4, type: "paragraph" }],
    key: ["artifact", "artifact-1", "blocks"],
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
  messageBlock: {
    data: { edges: [], node: { id: "message/message-0#b1", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/message/message-0#b-b1").queryKey,
  },
  newParentPanel: {
    data: { edges: [], node: { id: "CORE-8", kind: "issue" } },
    key: ["references", "to", "dispatch://CORE-8"],
  },
  otherAsk: { key: askQuery("ask-9").queryKey },
  otherCommentBlock: {
    data: { edges: [], node: { id: "comment/comment-9#b2", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/comment/comment-9#b-b2").queryKey,
  },
  otherDocSummary: {
    data: { id: "artifact-2" },
    key: documentSummaryQuery("CORE", "notes-md").queryKey,
  },
  otherDocumentPage: { data: { id: "artifact-2" }, key: ["artifact-ref", "CORE/notes-md"] },
  otherIssueMessage: { key: messageQuery("CORE-9", "message-9").queryKey },
  otherProjectList: { key: projectIssuesQuery("OTHER", []).queryKey },
  // Another block of CORE-1's spec, which a link elsewhere on the page names.
  otherSpecBlock: {
    data: { edges: [], node: { id: "CORE-1/spec#outro", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/spec#b-outro").queryKey,
  },
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
  // A block of CORE-1's spec, read by a link to it (its panel shares the key) and, pinned to
  // version 2, by another link.
  specBlock: {
    data: { edges: [], node: { id: "CORE-1/spec#intro", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/spec#b-intro").queryKey,
  },
  specBlockPinned: {
    data: { edges: [], node: { id: "CORE-1/spec#intro", kind: "block" } },
    key: blockQuery("dispatch://CORE-1/spec#b-intro@v2").queryKey,
  },
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
  "a blocker change refreshes the detail of each blocker it names, whose Referenced by count moved":
    {
      event: event(
        "issue.updated",
        issue({
          ancestors: [],
          changed: ["blocked_by"],
          references_changed: [{ id: "CORE-9", kind: "issue" }],
        })
      ),
      refreshes: ["detail", "unrelatedDetail"],
      spares: ["parent", "grandparent", "summary"],
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
  "a person's edit notice refreshes the issue's log only, since each edit's version refreshed the rest":
    {
      event: event("artifact.human_edited", {
        artifact_id: "artifact-1",
        from_version: 4,
        to_version: 7,
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
  "a citation of a block refreshes every read of that block and its document's panel": {
    event: event("message.created", {
      id: "message-2",
      references_changed: [
        {
          artifact_id: "artifact-3",
          id: "CORE-1/spec#intro",
          kind: "block",
          ref: "dispatch://CORE-1/spec#b-intro",
        },
      ],
    }),
    refreshes: ["specBlock", "specBlockPinned", "specPanel", "specPanelBySlug"],
    spares: ["otherSpecBlock", "documentPanel", "issuePanel", "summary", "unrelatedDetail"],
  },
  "a version of a document that changes a cited block refreshes the reads of that block": {
    event: event("artifact.version", {
      artifact_id: "artifact-3",
      name: "Plan",
      references_changed: [
        {
          artifact_id: "artifact-3",
          id: "CORE-1/spec#intro",
          kind: "block",
          ref: "dispatch://CORE-1/spec#b-intro",
        },
      ],
    }),
    refreshes: ["specBlock", "specBlockPinned", "specPanel", "summary"],
    spares: ["otherSpecBlock", "documentPanel"],
  },
  "a version of a document that names none of its blocks leaves every block read alone": {
    event: event("artifact.version", { artifact_id: "artifact-3", name: "Plan" }),
    refreshes: ["summary"],
    spares: ["specBlock", "specBlockPinned", "otherSpecBlock"],
  },
  "an event cut past the server's cap that names no kinds (an older server) refreshes every count and every panel":
    {
      event: event("message.created", { id: "message-2", references_changed_truncated: true }),
      refreshes: [
        "unrelatedDetail",
        "parent",
        "issuePanel",
        "unrelatedPanel",
        "documentPanel",
        "specBlock",
        "inbox",
      ],
      spares: ["projects"],
    },
  "an event that cut only documents refreshes every document panel and still the issue it names": {
    event: event("message.created", {
      id: "message-2",
      references_changed: [{ id: "CORE-9", kind: "issue" }],
      references_changed_truncated: true,
      references_changed_truncated_kinds: ["artifact"],
    }),
    refreshes: [
      "documentPanel",
      "specPanel",
      "specPanelBySlug",
      "unrelatedDetail",
      "unrelatedPanel",
    ],
    spares: [
      "parent",
      "issuePanel",
      "inbox",
      "projectInbox",
      "asks",
      "askPanel",
      "docAsks",
      "specBlock",
    ],
  },
  "an event that cut only issues refreshes every issue detail and issue panel": {
    event: event("message.created", {
      id: "message-2",
      references_changed_truncated: true,
      references_changed_truncated_kinds: ["issue"],
    }),
    refreshes: [
      "unrelatedDetail",
      "parent",
      "grandparent",
      "referencesTab",
      "issuePanel",
      "unrelatedPanel",
      "parentPanel",
    ],
    spares: ["inbox", "asks", "askPanel", "documentPanel", "specBlock", "docAsks"],
  },
  "an event that cut only asks refreshes every ask read, ask list and panel, and the Inbox": {
    event: event("message.created", {
      id: "message-2",
      references_changed_truncated: true,
      references_changed_truncated_kinds: ["ask"],
    }),
    refreshes: [
      "ask",
      "otherAsk",
      "asks",
      "docAsks",
      "inbox",
      "projectInbox",
      "askPanel",
      "docAskPanel",
    ],
    spares: ["unrelatedDetail", "parent", "issuePanel", "documentPanel", "docBlocks", "specBlock"],
  },
  "an event that cut only blocks refreshes every block read and every document panel": {
    event: event("message.created", {
      id: "message-2",
      references_changed_truncated: true,
      references_changed_truncated_kinds: ["block"],
    }),
    refreshes: ["specBlock", "specBlockPinned", "otherSpecBlock", "specPanel", "documentPanel"],
    spares: ["unrelatedDetail", "issuePanel", "inbox", "askPanel", "docAsks", "docBlocks"],
  },
  "an event that cut a kind this build does not know refreshes every count and every panel": {
    event: event("message.created", {
      id: "message-2",
      references_changed_truncated: true,
      references_changed_truncated_kinds: ["component"],
    }),
    refreshes: ["unrelatedDetail", "issuePanel", "documentPanel", "specBlock", "inbox"],
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
  "the viewer's snooze in another tab refreshes every Inbox read": {
    event: event(
      "user_state.updated",
      { changed: ["snoozed"], login: "alice" },
      { issue_key: null }
    ),
    refreshes: ["userState", "inbox", "projectInbox"],
    spares: ["pinned", "detail", "replies"],
  },
  "the viewer's snooze refreshes the Inbox reads whatever rows they hold": {
    event: event(
      "user_state.updated",
      { changed: ["snoozed"], login: "alice" },
      { issue_key: null }
    ),
    page: { inboxHolds: false },
    refreshes: ["userState", "inbox", "projectInbox"],
    spares: ["pinned"],
  },
  "another person's snooze refreshes nothing": {
    event: event("user_state.updated", { changed: ["snoozed"], login: "bob" }, { issue_key: null }),
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
  "an edit of a comment a project document owns refreshes that document's panel": {
    event: event("comment.edited", { anchor: null, ask_id: null, id: "comment-1" }, documentOwner),
    refreshes: ["documentPanel", "comment"],
    spares: ["specPanel", "issuePanel", "inbox"],
  },
  "an edit of a comment refreshes every read of its blocks, pinned or not, and no other block": {
    event: event("comment.edited", { anchor: null, ask_id: null, id: "comment-1" }),
    refreshes: ["commentBlock", "commentBlockPinned", "comment"],
    spares: ["otherCommentBlock", "messageBlock", "specBlock", "otherSpecBlock"],
  },
  "an edit of a comment a project document owns refreshes the reads of its blocks": {
    event: event("comment.edited", { anchor: null, ask_id: null, id: "comment-1" }, documentOwner),
    refreshes: ["commentBlock", "commentBlockPinned"],
    spares: ["otherCommentBlock", "messageBlock"],
  },
  "a resolution of a comment leaves the reads of its blocks alone": {
    event: event("comment.resolved", { anchor: null, ask_id: null, id: "comment-1" }),
    refreshes: ["comment"],
    spares: ["commentBlock", "commentBlockPinned"],
  },
  "a reply that names a block of a comment refreshes that block's reads, which list it": {
    event: event("comment.created", {
      anchor: null,
      ask_id: null,
      id: "comment-2",
      reply_block: { block_id: "b2", node: "comment/comment-1#b2" },
      reply_to: "comment-1",
    }),
    refreshes: ["commentBlock", "commentBlockPinned", "comment"],
    spares: ["otherCommentBlock", "messageBlock"],
  },
  "a reply that names a block of a message refreshes that block's read": {
    event: event("message.answered", {
      id: "message-2",
      in_reply_to: "message-0",
      reply_block: { block_id: "b1", node: "message/message-0#b1" },
    }),
    refreshes: ["messageBlock", "rootMessage"],
    spares: ["commentBlock", "otherCommentBlock"],
  },
  "an edit of an ask anchored in an issue's spec refreshes the spec's panel": {
    event: event("ask.edited", { anchor: { artifact_id: "artifact-3" }, id: "ask-1" }),
    refreshes: ["specPanel", "specPanelBySlug", "ask", "inbox"],
    spares: ["documentPanel", "issuePanel"],
  },
  "a resolution of a comment on a project document leaves that document's panel alone": {
    event: event(
      "comment.resolved",
      { anchor: { artifact_id: "artifact-1" }, ask_id: null, id: "comment-1" },
      documentOwner
    ),
    refreshes: ["comment"],
    spares: ["documentPanel"],
  },
  "a suggestion's verdict leaves the document's panel alone": {
    event: event(
      "suggestion.rejected",
      { anchor: { artifact_id: "artifact-1" }, ask_id: null, id: "comment-1" },
      documentOwner
    ),
    refreshes: ["comment"],
    spares: ["documentPanel"],
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

/** One event of every type the feed can carry, keyed by type so a new one fails to compile here
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
  "artifact.human_edited": event("artifact.human_edited", {
    artifact_id: "artifact-3",
    from_version: 4,
    to_version: 7,
  }),
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
  "decision.kept": event("decision.kept", {
    artifact_id: "artifact-3",
    block_id: "b-1",
    changed_version: 2,
  }),
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

test("a feed event prepends a newer event to the loaded log page", () => {
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

// Every open ask card lists who a person's answer reaches through its issue, read from its thread:
// the issue's claimant and the session its route reaches. The thread key holds only the ask id, so
// the rule tests each cached thread's ask for its issue.

/** A thread read as `GET /asks/{id}` answers it, cut to the one field the rule reads. */
function cacheThread(client: QueryClient, id: string, issueKey: string | null): void {
  client.setQueryData(["ask-thread", id], { ask: { id, issue_key: issueKey } });
}

function refreshedThreads(incoming: Event): string[] {
  const client = new QueryClient();
  cacheThread(client, "ask-1", "CORE-1");
  cacheThread(client, "ask-2", "CORE-2");
  cacheThread(client, "ask-doc", null);
  refreshQueries(client, eventRefreshTargets(incoming, "alice"));
  return ["ask-1", "ask-2", "ask-doc"].filter(
    (id) => client.getQueryState(["ask-thread", id])?.isInvalidated
  );
}

test("a claim, a release or a write that moves the route refreshes the ask threads on its issue and no other", () => {
  const onIssue = [
    event("issue.claimed", { key: "CORE-1", claim: null }),
    event("issue.released", { key: "CORE-1", claim: null }),
    event("issue.updated", issue({ changed: ["route"], route: "role:reviewer" })),
    // One PATCH that sets the route and closes the issue emits this alone.
    event(
      "issue.closed",
      issue({
        changed: ["status", "closed_at", "route"],
        closed_at: "2026-10-26T00:00:00Z",
        route: "role:reviewer",
        status: "done",
      })
    ),
    // An older server says nothing of what moved, so the route may have.
    event("issue.updated", issue()),
  ];
  for (const incoming of onIssue) {
    expect(askThreadsIssue(incoming)).toBe("CORE-1");
    expect(refreshedThreads(incoming)).toEqual(["ask-1"]);
  }
});

test("a write that moves neither the claimant nor the route refreshes no ask thread", () => {
  const elsewhere = [
    event("issue.updated", issue({ changed: ["title"], title: "Renamed" })),
    // A close that releases a claim appends its own issue.released.
    event(
      "issue.closed",
      issue({ changed: ["status", "closed_at"], closed_at: "2026-10-26T00:00:00Z", status: "done" })
    ),
    event("issue.deployed", { number: 501, repo: "acme/widgets" }),
  ];
  for (const incoming of elsewhere) {
    expect(askThreadsIssue(incoming)).toBeNull();
    expect(refreshedThreads(incoming)).toEqual([]);
  }
});

test("a thread on its first load is judged on the response that lands: refreshed on the claimed issue, left alone elsewhere", async () => {
  const client = new QueryClient();
  const onIssue = Promise.withResolvers<AskRead>();
  const elsewhere = Promise.withResolvers<AskRead>();
  // Neither ask is read yet, so the cache cannot tell their issues when the claim arrives.
  const loads = [
    client.prefetchQuery({ queryFn: () => onIssue.promise, queryKey: ["ask-thread", "ask-3"] }),
    client.prefetchQuery({ queryFn: () => elsewhere.promise, queryKey: ["ask-thread", "ask-4"] }),
  ];
  refreshQueries(client, eventRefreshTargets(event("issue.claimed", { key: "CORE-1" }), "alice"));
  onIssue.resolve({ ask: { id: "ask-3", issue_key: "CORE-1" } } as unknown as AskRead);
  elsewhere.resolve({ ask: { id: "ask-4", issue_key: "CORE-2" } } as unknown as AskRead);
  await Promise.all(loads);
  expect(client.getQueryState(["ask-thread", "ask-3"])?.isInvalidated).toBe(true);
  expect(client.getQueryState(["ask-thread", "ask-4"])?.isInvalidated).toBe(false);
});

function threadOf(row: InboxRow, claimant: string | null = row.thread.claimant): AskRead {
  return { ask: row, ...row.thread, claimant };
}

test("a claim that moves while the Inbox is in flight sends that issue's later cards to the ask endpoint, until a newer Inbox lands", async () => {
  const onIssue = issueAsk({ id: "ask-1" });
  const sibling = issueAsk({ id: "ask-3" });
  const elsewhere = issueAsk({
    id: "ask-2",
    issue: { assignee: "alice", key: "CORE-2", title: "Other" },
    issue_key: "CORE-2",
  });
  const rows = [onIssue, sibling, elsewhere];
  const inFlight = Promise.withResolvers<InboxRow[]>();
  const getInbox = spyOn(api, "getInbox").mockImplementationOnce(() => inFlight.promise);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const inboxKey = inboxQuery().queryKey;
  const reads: string[] = [];
  // The endpoint answers after the claim: every thread on CORE-1 names the new claimant.
  const getAskThread = async (id: string): Promise<AskRead> => {
    reads.push(id);
    const row = rows.find((candidate) => candidate.id === id) as InboxRow;
    return threadOf(row, row.issue_key === "CORE-1" ? "session-now" : null);
  };
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  const mountCard = (row: InboxRow) =>
    renderHook(
      () =>
        useAskAnswerForm({
          answer: async () => row as Ask,
          ask: row,
          createReply: async () => {
            throw new Error("no reply in this test");
          },
          getAskThread,
          initialThread: threadOf(row),
          initialThreadUpdatedAt: Date.now(),
        }),
      { wrapper }
    );
  const cards: { unmount: () => void }[] = [];
  try {
    void client.prefetchQuery(inboxQuery());
    await waitFor(() => expect(client.getQueryState(inboxKey)?.fetchStatus).toBe("fetching"));
    const claimed = event("issue.claimed", {
      claim: { actor: { id: "session-now", kind: "session" }, at: "2026-09-24T06:00:00Z" },
      reason: "claimed",
    });
    markAskThreadsSeededByAnInFlightInbox(
      client,
      eventRefreshTargets(claimed, "alice"),
      askThreadsIssue(claimed)
    );
    // The in-flight body was composed before the claim.
    inFlight.resolve(rows);
    await waitFor(() => expect(client.getQueryState(inboxKey)?.fetchStatus).toBe("idle"));
    expect(hasPendingAskThreadInvalidation(client, onIssue)).toBe(true);
    expect(hasPendingAskThreadInvalidation(client, elsewhere)).toBe(false);

    const first = mountCard(onIssue);
    cards.push(first);
    await waitFor(() =>
      expect(first.result.current.threadQuery.data?.claimant).toBe("session-now")
    );
    const other = mountCard(elsewhere);
    cards.push(other);
    expect(other.result.current.threadQuery.data?.claimant).toBeNull();
    // One card's own read leaves the issue's mark for the next card on it.
    const second = mountCard(sibling);
    cards.push(second);
    await waitFor(() =>
      expect(second.result.current.threadQuery.data?.claimant).toBe("session-now")
    );
    expect([...reads].sort()).toEqual(["ask-1", "ask-3"]);

    // A newer Inbox, composed after the claim, clears the issue's mark.
    getInbox.mockResolvedValueOnce(rows);
    await client.refetchQueries({ queryKey: inboxKey });
    expect(hasPendingAskThreadInvalidation(client, onIssue)).toBe(false);
    expect(hasPendingAskThreadInvalidation(client, sibling)).toBe(false);
  } finally {
    for (const card of cards) card.unmount();
    getInbox.mockRestore();
  }
});

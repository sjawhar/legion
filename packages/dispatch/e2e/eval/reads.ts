/**
 * `READS`: the evaluation proxy's GET allow-list. The proxy forwards each route to the server
 * with its own bearer; a route that reads something the agent wrote folds the write in (`own`
 * for what the agent created, `merge` for the server's answer), so the agent reads back what
 * the server would show had the write landed.
 */
import { createHash } from "node:crypto";
import type {
  Artifact,
  Ask,
  AskRead,
  Comment,
  CommentRead,
  Event,
  IssueDetails,
  IssueSummary,
  MessageRead,
  OpenAsksResponse,
} from "@legion/contracts";

import {
  type Answer,
  askList,
  askRead,
  askState,
  byCreated,
  byLifecycle,
  commentList,
  eventPage,
  eventsPage,
  followerList,
  issueRead,
  listed,
  mergeIssueRead,
  mergeOpenAsks,
  messageRead,
  messageThread,
  notFound,
  ok,
  type ReadContext,
  type ReadRoute,
  recountOpenAsks,
  requireProjectDocument,
  summaryOf,
  touchedIssues,
  unmodelledRead,
  withApproval,
} from "./overlay";

/**
 * A read route whose handlers take the server's answer as its contract type `T`: the upstream
 * is Dispatch itself, so its 2xx JSON is `T`, and this is the one place that says so.
 */
function reading<T>(route: {
  path: string;
  own?: (context: ReadContext) => Promise<Answer | undefined>;
  merge?: (context: ReadContext, body: T) => Promise<T> | T;
  owner?: (body: T) => string | null | undefined;
  recount?: (body: T) => T;
}): ReadRoute {
  const { merge, owner, recount } = route;
  return {
    path: route.path,
    own: route.own,
    merge: merge && (async (context, body) => merge(context, body as T)),
    owner: owner && ((body) => owner(body as T)),
    recount: recount && ((body) => recount(body as T)),
  };
}

const ownIssue = (context: ReadContext) =>
  context.overlay.created.has(context.params.key ?? "") ? context.params.key : undefined;

/** The GET allow-list: every route `DispatchClient` reads, and the four the Dispatch skill tells
 *  an agent to call (the route index, the block schema, a project's architecture, and an issue
 *  document's blocks by slug). The proxy forwards each to the server with its own bearer. */
export const READS = {
  index: reading({ path: "" }),
  "block-schema": reading({ path: "schema/blocks" }),
  whoami: reading({ path: "whoami" }),
  agents: reading({ path: "agents" }),
  search: reading({ path: "search" }),
  references: reading({ path: "references" }),
  "open-asks": reading<OpenAsksResponse>({
    path: "asks/open",
    merge: mergeOpenAsks,
    recount: recountOpenAsks,
  }),
  ask: reading<AskRead>({
    path: "asks/:ask",
    own: async ({ overlay, params }) => {
      const ask = overlay.created.has(params.ask ?? "")
        ? overlay.asks.get(params.ask ?? "")
        : undefined;
      if (!ask) return undefined;
      return ok({
        ask: askRead(overlay, ask, undefined),
        replies: [...overlay.comments.values()]
          .filter((comment) => comment.ask_id === ask.id)
          .sort(byCreated),
        edits: overlay.askEdits.get(ask.id) ?? [],
        followers: followerList([], overlay.followers.get(ask.id)),
      } satisfies AskRead);
    },
    merge: ({ overlay }, body) => ({
      ask: askRead(overlay, overlay.asks.get(body.ask.id) ?? body.ask, body.ask),
      replies: commentList(overlay, body.replies, (comment) => comment.ask_id === body.ask.id),
      edits: [...body.edits, ...(overlay.askEdits.get(body.ask.id) ?? [])],
      followers: followerList(body.followers, overlay.followers.get(body.ask.id)),
    }),
    owner: (body) => body.ask.issue_key,
  }),
  comment: reading<CommentRead>({
    path: "comments/:comment",
    own: async ({ overlay, params }) => {
      const id = params.comment ?? "";
      const comment = overlay.created.has(id) ? overlay.comments.get(id) : undefined;
      if (!comment) return undefined;
      return ok({
        comment,
        replies: commentList(overlay, [], (reply) => reply.reply_to === id),
      } satisfies CommentRead);
    },
    merge: ({ overlay }, body) => ({
      comment: overlay.comments.get(body.comment.id) ?? body.comment,
      replies: commentList(overlay, body.replies, (reply) => reply.reply_to === body.comment.id),
    }),
    owner: (body) => body.comment.issue_key,
  }),
  "message-thread": reading<MessageRead>({
    path: "messages/:message",
    own: async (context) => {
      const id = context.params.message ?? "";
      if (!context.overlay.created.has(id)) return undefined;
      const read = await messageThread(
        context,
        id,
        context.query.size > 0 ? `?${context.query}` : ""
      );
      return read ? ok(read) : notFound();
    },
    merge: ({ overlay }, body) => messageRead(overlay, body.message, body.replies),
    owner: (body) => body.message.issue_key,
  }),
  "issue-list": reading<IssueSummary[]>({
    path: "issues",
    merge: async (context, body) => {
      const touched = touchedIssues(context.overlay);
      const rows = body.filter((row) => !touched.has(row.key));
      for (const key of touched) {
        const details = await issueRead(context, key);
        if (details && listed(details, context.query)) rows.push(summaryOf(details));
      }
      return rows.sort(byLifecycle);
    },
  }),
  "issue-resolve": reading<{ key: string }>({
    path: "issues/resolve",
    own: async ({ overlay, query }) => {
      const ref = query.get("ref")?.trim() ?? "";
      return overlay.created.has(ref) ? ok({ key: ref }) : undefined;
    },
    owner: (body) => body.key,
  }),
  issue: reading<IssueDetails>({
    path: "issues/:key",
    own: async (context) => {
      const key = ownIssue(context);
      const details = key === undefined ? undefined : await issueRead(context, key);
      return details && ok(details);
    },
    merge: (context, body) => mergeIssueRead(context, body.key, body),
  }),
  "issue-events": reading<Event[]>({
    path: "issues/:key/events",
    own: async (context) => {
      const key = ownIssue(context);
      if (key === undefined) return undefined;
      return ok(eventsPage([], context.overlay.events.get(key) ?? [], eventPage(context.query)));
    },
    merge: ({ overlay, params, query }, body) =>
      eventsPage(body, overlay.events.get(params.key ?? "") ?? [], eventPage(query)),
  }),
  "issue-asks": reading<Ask[]>({
    path: "issues/:key/asks",
    own: async (context) => {
      const key = ownIssue(context);
      if (key === undefined) return undefined;
      return ok(
        askList(context.overlay, [], (ask) => ask.issue_key === key, askState(context.query))
      );
    },
    merge: ({ overlay, params, query }, body) =>
      askList(overlay, body, (ask) => ask.issue_key === params.key, askState(query)),
  }),
  "issue-comments": reading<Comment[]>({
    path: "issues/:key/comments",
    own: async (context) => {
      const key = ownIssue(context);
      if (key === undefined) return undefined;
      // The agent's comments anchor nothing, so a document filter matches none of them.
      const filtered = (context.query.get("artifact")?.trim() ?? "") !== "";
      return ok(
        filtered ? [] : commentList(context.overlay, [], (comment) => comment.issue_key === key)
      );
    },
    merge: ({ overlay, params, query }, body) =>
      (query.get("artifact")?.trim() ?? "") !== ""
        ? body.map((comment) => overlay.comments.get(comment.id) ?? comment)
        : commentList(overlay, body, (comment) => comment.issue_key === params.key),
  }),
  "issue-references": reading({
    path: "issues/:key/references",
    own: async (context) =>
      ownIssue(context) === undefined
        ? undefined
        : unmodelledRead("the reference closure of an issue the agent created"),
  }),
  "issue-message": reading<MessageRead>({
    path: "issues/:key/messages/:message",
    own: async ({ overlay, params }) => {
      const message = overlay.messages.get(params.message ?? "");
      if (!message) return undefined;
      return message.issue_key === params.key ? ok(messageRead(overlay, message, [])) : notFound();
    },
    merge: ({ overlay }, body) => messageRead(overlay, body.message, body.replies),
  }),
  "issue-document-blocks": reading({
    path: "issues/:key/artifacts/:slug/blocks",
    own: async (context) => {
      const { overlay, params } = context;
      const mine = overlay.documentsOf(
        (artifact) => artifact.issue_key === params.key && artifact.slug === params.slug
      );
      return mine.length > 0 || ownIssue(context) !== undefined
        ? unmodelledRead("the blocks of a document the agent created")
        : undefined;
    },
  }),
  artifact: reading<Artifact>({
    path: "artifacts/:artifact",
    own: async ({ overlay, params }) => {
      const stored = overlay.documents.get(params.artifact ?? "");
      return stored && ok(withApproval(overlay, stored.artifact));
    },
    merge: ({ overlay }, body) => withApproval(overlay, body),
    owner: (body) => body.issue_key,
  }),
  "artifact-text": reading({
    path: "artifacts/:artifact/text",
    own: async ({ overlay, params }) => {
      const stored = overlay.documents.get(params.artifact ?? "");
      if (!stored) return undefined;
      return ok({
        markdown: stored.markdown,
        version: stored.artifact.versions.at(-1)?.number ?? null,
        // The server's token hashes the live Proof state; the proxy applies no edit, so no edit
        // ever checks this one.
        token: createHash("sha256").update(stored.markdown).digest("hex"),
      });
    },
  }),
  "artifact-version": reading({
    path: "artifacts/:artifact/versions/:version",
    own: async ({ overlay, params }) => {
      const stored = overlay.documents.get(params.artifact ?? "");
      if (!stored) return undefined;
      const found = stored.artifact.versions.find((each) => String(each.number) === params.version);
      if (!found) return notFound();
      return found === stored.artifact.versions.at(-1)
        ? ok({ ...found, markdown: stored.markdown })
        : unmodelledRead("the text of an earlier version of a document the agent created");
    },
  }),
  "artifact-blocks": reading({
    path: "artifacts/:artifact/blocks",
    own: async ({ overlay, params }) =>
      overlay.documents.has(params.artifact ?? "")
        ? unmodelledRead("the blocks of a document the agent created")
        : undefined,
  }),
  "artifact-references": reading({
    path: "artifacts/:artifact/references",
    own: async ({ overlay, params }) =>
      overlay.documents.has(params.artifact ?? "")
        ? unmodelledRead("the references of a document the agent created")
        : undefined,
  }),
  "artifact-events": reading<Event[]>({
    path: "artifacts/:artifact/events",
    own: async ({ overlay, params, query }) => {
      const stored = overlay.documents.get(params.artifact ?? "");
      if (!stored) return undefined;
      requireProjectDocument(stored.artifact);
      return ok(
        eventsPage([], overlay.events.get(`artifact:${stored.artifact.id}`) ?? [], eventPage(query))
      );
    },
    merge: ({ overlay, params, query }, body) =>
      eventsPage(body, overlay.events.get(`artifact:${params.artifact}`) ?? [], eventPage(query)),
  }),
  "artifact-asks": reading<Ask[]>({
    path: "artifacts/:artifact/asks",
    own: async ({ overlay, params, query }) => {
      const stored = overlay.documents.get(params.artifact ?? "");
      if (!stored) return undefined;
      requireProjectDocument(stored.artifact);
      return ok(
        askList(overlay, [], (ask) => ask.artifact_id === stored.artifact.id, askState(query))
      );
    },
    merge: ({ overlay, params, query }, body) =>
      askList(overlay, body, (ask) => ask.artifact_id === params.artifact, askState(query)),
  }),
  "artifact-comments": reading<Comment[]>({
    path: "artifacts/:artifact/comments",
    own: async ({ overlay, params }) => {
      const stored = overlay.documents.get(params.artifact ?? "");
      if (!stored) return undefined;
      requireProjectDocument(stored.artifact);
      return ok(commentList(overlay, [], (comment) => comment.artifact_id === stored.artifact.id));
    },
    merge: ({ overlay, params }, body) =>
      commentList(overlay, body, (comment) => comment.artifact_id === params.artifact),
  }),
  "project-documents": reading<Artifact[]>({
    path: "projects/:project/artifacts",
    merge: ({ overlay, params, query }, body) => {
      const unlinked = query.get("unlinked") === "true";
      const ids = new Set(body.map((artifact) => artifact.id));
      return [
        ...body,
        ...overlay.documentsOf(
          (artifact) =>
            artifact.project === params.project &&
            !artifact.primary &&
            !ids.has(artifact.id) &&
            (!unlinked || artifact.issue_key === null)
        ),
      ].map((artifact) => withApproval(overlay, artifact));
    },
  }),
  "project-document": reading<Artifact>({
    path: "projects/:project/artifacts/:slug",
    own: async ({ overlay, params }) => {
      const [artifact] = overlay.documentsOf(
        (each) =>
          each.project === params.project && each.issue_key === null && each.slug === params.slug
      );
      return artifact && ok(withApproval(overlay, artifact));
    },
    merge: ({ overlay }, body) => withApproval(overlay, body),
    owner: (body) => body.issue_key,
  }),
  "architecture-source": reading({ path: "projects/:project/architecture-source" }),
  architecture: reading({ path: "projects/:project/architecture" }),
} as const satisfies Record<string, ReadRoute>;

export type ReadName = keyof typeof READS;

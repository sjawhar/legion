/**
 * going-catalogue.ts is the request side of Dispatch's action catalogue: one schema per route an
 * agent may propose an operation on, keyed `"<METHOD> <pattern>"` exactly as the server's own
 * route table names it (`api/routes_table.go`).
 *
 * The catalogue itself is built on the server, from the route table rather than from this file:
 * which rows are eligible is computed there (every mutating row that is not a test hook and that
 * a person's credential may call), and a row with no entry here fails the server's catalogue
 * test, so a route added later either reaches the catalogue or breaks the build. What this file
 * owns is what the table cannot say: the arguments each action takes, an example call that
 * satisfies them, the dispatch tool whose words describe the action where one does, and the
 * graph relations the action's verbs write.
 *
 * Each schema is written zod-first through the `SchemaApi` facade (`tool-schema.ts`), as the tool
 * specs are (`dispatch-tools.ts`), so one declaration serves every host: `zodSchemaApi` builds
 * zod from it, and `scripts/gen-go.ts` records the same calls to generate the Go the server
 * validates a proposed call with.
 *
 * Two things no action names, because neither is the action's to decide: `actor`, which the
 * caller's own credential settles, and the `Idempotency-Key` header, which the caller mints.
 * A path placeholder is an argument like any other: `{key}` is a required `key`, and the server's
 * catalogue test holds every schema to the placeholders of its own pattern.
 */

import {
  ASK_URGENCIES,
  DOC_EDIT_OPS,
  ISSUE_COMPONENTS_MODES,
  ISSUE_STATUSES,
} from "./dispatch-tools";
import type { SchemaApi, SchemaNode, ToolArgumentsShape } from "./tool-schema";

/** One catalogue-eligible route's request, as the catalogue serves and the server checks it. */
export interface GoingRequestSpec {
  /** The action's arguments: its pattern's placeholders and its body's fields, as one shape. */
  readonly arguments: <E extends SchemaNode<E>>(z: SchemaApi<E>) => ToolArgumentsShape;
  /** A call this schema accepts, naming every required argument and nothing else required. */
  readonly example: Readonly<Record<string, unknown>>;
  /** The dispatch tool that performs this route, whose description the catalogue prefers to the
   *  route row's; absent where no tool names it. */
  readonly tool?: string;
  /** The graph relations this action's verbs write, each a structural edge kind
   *  (`model.GraphEdgeKinds`) or a `block[]` attribute of a typed block (`pmdoc.LinkKinds`).
   *  Absent where the action writes no edge at all. */
  readonly relations?: readonly string[];
}

/** Delivery modes a targeted write chooses between, as the server reads them. */
const DELIVERY_MODES = ["btw", "aside", "steer"] as const;

/** The turn a comment hands on: who acts next on the ask it replies in. */
const COMMENT_TURNS = ["agent", "human"] as const;

/** A document's canonical uuid, the id every `/artifacts/{id}` route addresses. */
function documentID<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z.string({ min: 1 }).describe("The document's uuid.");
}

/** An issue key, as every `/issues/{key}` route spells it. */
function issueKey<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z.string({ min: 1 }).describe("The issue's key, such as INFRA-1042.");
}

/** A project key, as every `/projects/{key}` route spells it. */
function projectKey<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z.string({ min: 1 }).describe("The project's key, such as INFRA.");
}

/** The slug or unambiguous filename the by-name document routes resolve. */
function documentSlug<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z.string({ min: 1 }).describe("The document's slug, or an unambiguous filename.");
}

/** The session an action addresses, never the session the caller's credential proved. */
function sessionID<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z.string({ min: 1 }).describe("The agent session's id.");
}

/** The two halves of a GitHub repository, which the repository routes take as path segments. */
function repositoryPath<E extends SchemaNode<E>>(z: SchemaApi<E>): Record<string, E> {
  return {
    owner: z.string({ min: 1 }).describe("The repository's owner on GitHub."),
    repo: z.string({ min: 1 }).describe("The repository's name on GitHub."),
  };
}

/** The anchor that hangs an ask or a comment from a quote of a document. */
function anchorArgument<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z
    .object({
      artifact: z.string({ min: 1 }).describe("The document the quote is in."),
      quote: z
        .string({ min: 1 })
        .describe("The quoted text, exactly as the document holds it.")
        .optional(),
      occurrence: z
        .number({ int: true, min: 1 })
        .describe("Which occurrence of the quote to anchor to, from 1.")
        .optional(),
      mark_id: z
        .string({ min: 1 })
        .describe("An existing mark to anchor to instead of a quote.")
        .optional(),
    })
    .describe("Where in a document this hangs: a quote of it, or a mark it already carries.")
    .optional();
}

/** The options a question ask offers. */
function askOptions<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z
    .array(
      z.object({
        id: z
          .string({ min: 1 })
          .describe("The option's id; the server mints one when absent.")
          .optional(),
        label: z.string({ min: 1 }).describe("The option as the person reads it."),
        description: z.string({ min: 1 }).describe("What choosing it means.").optional(),
      })
    )
    .describe("The options the person chooses between; omit them for an open question.")
    .optional();
}

/** The arguments shared by the two routes that open a question ask. */
function askArguments<E extends SchemaNode<E>>(z: SchemaApi<E>): Record<string, E> {
  return {
    question: z.string({ min: 1 }).describe("The question, as the person reads it."),
    kind: z.enum(["question"]).describe("Only question asks are opened by a caller.").optional(),
    options: askOptions(z),
    multiple: z.boolean().describe("Whether more than one option may be chosen.").optional(),
    urgency: z.enum(ASK_URGENCIES).describe("How hard this blocks the work.").optional(),
    anchor: anchorArgument(z),
  };
}

/** The arguments shared by the two routes that post a comment. */
function commentArguments<E extends SchemaNode<E>>(z: SchemaApi<E>): Record<string, E> {
  return {
    body: z.string({ min: 1 }).describe("The comment's markdown."),
    anchor: anchorArgument(z),
    reply_to: z.string({ min: 1 }).describe("The comment this answers.").optional(),
    reply_to_block: z
      .string({ min: 1 })
      .describe("The block of reply_to, or the part of ask_id, this answers.")
      .optional(),
    ask_id: z
      .string({ min: 1 })
      .describe("The ask this replies in, instead of reply_to.")
      .optional(),
    mentions: z
      .array(z.object({ target: z.string({ min: 1 }).describe("role:<name> or session:<id>.") }))
      .describe("The sessions or roles this comment reaches.")
      .optional(),
    delivery: z.enum(DELIVERY_MODES).describe("How a mention reaches its session.").optional(),
    turn: z.enum(COMMENT_TURNS).describe("Who acts next on the ask this replies in.").optional(),
    suggestion: z
      .object({
        replace_with: z.string().describe("The exact replacement for the anchored quote."),
      })
      .describe("Propose an exact replacement for the quote this is anchored to.")
      .optional(),
  };
}

/** The arguments shared by the three routes that apply document edit operations. */
function documentEditArguments<E extends SchemaNode<E>>(z: SchemaApi<E>): Record<string, E> {
  return {
    ops: z
      .array(
        z.object({
          op: z.enum(DOC_EDIT_OPS).describe("Which edit this is."),
          find: z.string().describe("The quoted text a replace or delete addresses.").optional(),
          with: z.string().describe("What a replace writes in its place.").optional(),
          occurrence: z
            .number({ int: true, min: 1 })
            .describe("Which occurrence of find to address, from 1.")
            .optional(),
          markdown: z.string().describe("The markdown an insert writes.").optional(),
          after: z.string().describe("The quote an insert goes after.").optional(),
          before: z.string().describe("The quote an insert goes before.").optional(),
          block: z
            .string()
            .describe("The block a retype, move, delete or table edit addresses.")
            .optional(),
          index: z
            .number({ int: true, min: 0 })
            .describe("The row or column a table edit removes.")
            .optional(),
          type: z.string().describe("The typed block a retype writes.").optional(),
          attributes: z.unknown().describe("The typed block's attributes, by name.").optional(),
        }),
        { min: 1 }
      )
      .describe("The edits to apply, in order; all of them land or none does."),
    summary: z.string().describe("What this edit did, for the document's history.").optional(),
    precondition: z
      .object({
        document: z
          .string({ min: 1 })
          .describe("The whole-document token the edit is held to.")
          .optional(),
        blocks: z
          .array(
            z.object({
              id: z.string({ min: 1 }).describe("The block the token covers."),
              token: z.string({ min: 1 }).describe("The token that block was read at."),
            }),
            { min: 1 }
          )
          .describe("One token per block the edit is held to.")
          .optional(),
      })
      .describe("Refuse the edit when the document moved since it was read.")
      .optional(),
  };
}

/** The arguments shared by the three routes that name a document's current version. */
function namedVersionArguments<E extends SchemaNode<E>>(z: SchemaApi<E>): Record<string, E> {
  return { summary: z.string({ min: 1 }).describe("What this version is, as its name.") };
}

/** The arguments an upload of a document or file takes beside its owner: a multipart upload
 *  sends its bytes as the request's file part, which no argument names. */
function uploadArguments<E extends SchemaNode<E>>(z: SchemaApi<E>): Record<string, E> {
  return {
    name: z.string({ min: 1 }).describe("The artifact's file name, such as plan.md."),
    content: z.string().describe("Inline markdown, for a document sent as JSON.").optional(),
    summary: z.string().describe("What this upload is, for the document's history.").optional(),
  };
}

/** The `components` argument the two issue writes share, as `dispatch-tools.ts` states it. */
function componentsArgument<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z
    .object({
      mode: z
        .enum(ISSUE_COMPONENTS_MODES)
        .describe("inherit, explicit with ids, or none with a reason."),
      ids: z
        .array(z.string({ min: 1 }))
        .describe("For mode explicit: the components it attaches to.")
        .optional(),
      reason: z
        .string({ min: 1 })
        .describe("For mode none: why it is not architectural.")
        .optional(),
    })
    .describe("Attach the issue to architecture components.")
    .optional();
}

/**
 * The request of every catalogue-eligible route, by `"<METHOD> <pattern>"`. The server decides
 * which routes are eligible and holds this map to that set in both directions, so an entry here
 * for a route that is not eligible fails its test exactly as a missing one does.
 */
export const goingRequests = {
  "DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {
    arguments: (z) => ({ id: documentID(z), session_id: sessionID(z) }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", session_id: "ses_42" },
  },
  "DELETE /api/v1/asks/{id}/followers/{session_id}": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The ask's uuid."),
      session_id: sessionID(z),
    }),
    example: { id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c", session_id: "ses_42" },
    tool: "dispatch_follow",
    relations: ["followed_by"],
  },
  "DELETE /api/v1/delivery/session-groups/{id}": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The session group's uuid.") }),
    example: { id: "1a7b3c9d-2e4f-4a6b-8c0d-1e2f3a4b5c6d" },
  },
  "DELETE /api/v1/issues/{key}/claim": {
    arguments: (z) => ({ key: issueKey(z) }),
    example: { key: "INFRA-1042" },
    tool: "dispatch_claim",
  },
  "DELETE /api/v1/issues/{key}/subscribers/{session_id}": {
    arguments: (z) => ({ key: issueKey(z), session_id: sessionID(z) }),
    example: { key: "INFRA-1042", session_id: "ses_42" },
  },
  "DELETE /api/v1/me/agent-tokens/{id}": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The personal token's uuid.") }),
    example: { id: "9d8c7b6a-5e4f-4a3b-2c1d-0e9f8a7b6c5d" },
  },
  "DELETE /api/v1/me/asks/{id}/snooze": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The ask's uuid.") }),
    example: { id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c" },
  },
  "DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}": {
    arguments: (z) => ({ key: projectKey(z), ...repositoryPath(z) }),
    example: { key: "INFRA", owner: "acme", repo: "platform" },
    relations: ["depends_on", "affects"],
  },
  "DELETE /api/v1/settings/repo-projects/{owner}/{repo}": {
    arguments: (z) => repositoryPath(z),
    example: { owner: "acme", repo: "platform" },
  },
  "PATCH /api/v1/asks/{id}": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The ask's uuid."),
      question: z.string({ min: 1 }).describe("The reworded question.").optional(),
      options: askOptions(z),
      multiple: z.boolean().describe("Whether more than one option may be chosen.").optional(),
      urgency: z.enum(ASK_URGENCIES).describe("How hard this blocks the work.").optional(),
    }),
    example: { id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c", question: "Which column drops first?" },
    tool: "dispatch_edit_ask",
    relations: ["mentions"],
  },
  "PATCH /api/v1/comments/{id}": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The comment's uuid."),
      body: z.string({ min: 1 }).describe("The comment's new markdown."),
      base_revision: z
        .number({ int: true, min: 0 })
        .describe("The revision the blocks below were read at.")
        .optional(),
      blocks: z
        .array(
          z.object({
            id: z.string({ min: 1 }).describe("The block keeping its id."),
            from: z
              .number({ int: true, min: 0 })
              .describe("Where it starts in the new body, in UTF-16 units."),
          })
        )
        .describe("Where each kept block starts in the new body, so its id survives the edit.")
        .optional(),
    }),
    example: {
      id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091",
      body: "Corrected: the drop is reviewed.",
    },
    relations: ["mentions"],
  },
  "PATCH /api/v1/delivery/session-groups/{id}": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The session group's uuid."),
      name: z.string({ min: 1 }).describe("The group's new name.").optional(),
      add: z.array(sessionID(z)).describe("Sessions to put in the group.").optional(),
      remove: z.array(sessionID(z)).describe("Sessions to take out of it.").optional(),
    }),
    example: { id: "1a7b3c9d-2e4f-4a6b-8c0d-1e2f3a4b5c6d", name: "Platform agents" },
  },
  "PATCH /api/v1/issues/{key}": {
    arguments: (z) => ({
      key: issueKey(z),
      title: z.string({ min: 1 }).describe("The issue's new title.").optional(),
      status: z.enum(ISSUE_STATUSES).describe("The lifecycle status to move it to.").optional(),
      priority: z
        .number({ int: true, min: 0, max: 3 })
        .describe("Coarse priority, 0 highest; null clears it.")
        .nullable()
        .optional(),
      labels: z
        .array(z.string({ min: 1 }))
        .describe("The whole label set to hold.")
        .optional(),
      labels_add: z
        .array(z.string({ min: 1 }))
        .describe("Labels to add to the ones it holds.")
        .optional(),
      route: z
        .string()
        .describe("Where this issue's messages go: role:<name> or session:<id>.")
        .optional(),
      parent: z
        .string()
        .describe("The issue this one hangs under; null detaches it.")
        .nullable()
        .optional(),
      rank: z
        .object({
          before: z.string({ min: 1 }).describe("Rank it above this issue.").optional(),
          after: z.string({ min: 1 }).describe("Rank it below this issue.").optional(),
        })
        .describe("Where it sits on its project's board.")
        .optional(),
      assignee: z
        .string()
        .describe("The email of the person who answers its asks; null unassigns it.")
        .nullable()
        .optional(),
      blocked_by: z
        .array(z.string({ min: 1 }))
        .describe("The issues it waits on.")
        .optional(),
      components: componentsArgument(z),
      repo: z
        .string()
        .describe("The repository it targets, owner/name; null clears it.")
        .nullable()
        .optional(),
      external_links: z
        .array(
          z.object({
            url: z.string({ min: 1 }).describe("The link's URL."),
            kind: z
              .string({ min: 1 })
              .describe("What the link is: pull_request, run, document.")
              .optional(),
          })
        )
        .describe("Links merged into the issue's own by URL; an empty array removes them all.")
        .optional(),
      expected_sequence: z
        .number({ int: true, min: 0 })
        .describe("Apply only while this is still the issue's last event.")
        .optional(),
    }),
    example: { key: "INFRA-1042", status: "in_progress" },
    tool: "dispatch_issue_update",
    relations: ["child_of", "blocked_by", "affects"],
  },
  "PATCH /api/v1/me/agent-tokens/{id}": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The personal token's uuid."),
      scopes: z.array(z.string({ min: 1 })).describe("The whole scope set the token is to hold."),
    }),
    example: { id: "9d8c7b6a-5e4f-4a3b-2c1d-0e9f8a7b6c5d", scopes: ["person"] },
  },
  "POST /api/v1/agents/{session_id}/artifacts": {
    arguments: (z) => ({
      session_id: sessionID(z),
      name: z
        .string({ min: 1 })
        .describe("The file's name; this route holds files and images only."),
      summary: z.string().describe("What this upload is.").optional(),
    }),
    example: { session_id: "ses_42", name: "screenshot.png" },
    tool: "dispatch_artifact",
  },
  "POST /api/v1/agents/{session_id}/messages": {
    arguments: (z) => ({
      session_id: sessionID(z),
      body: z.string({ min: 1 }).describe("The message's markdown."),
      delivery: z.enum(DELIVERY_MODES).describe("How it reaches the session."),
      in_reply_to: z.string({ min: 1 }).describe("The message this answers.").optional(),
      in_reply_to_block: z
        .string({ min: 1 })
        .describe("The block of that message this answers.")
        .optional(),
    }),
    example: { session_id: "ses_42", body: "The deploy is green.", delivery: "btw" },
    tool: "dispatch_message",
    relations: ["replies_to", "mentions"],
  },
  "POST /api/v1/artifacts/{id}/approval-requests": {
    arguments: (z) => ({
      id: documentID(z),
      summary: z.string().describe("What the person is being asked to approve.").optional(),
      include_verification: z
        .boolean()
        .describe("Ask for the issue's Verification document in the same request.")
        .optional(),
    }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", summary: "Spec ready for review." },
    tool: "dispatch_request_approval",
    relations: ["owned_by"],
  },
  "POST /api/v1/artifacts/{id}/asks": {
    arguments: (z) => ({ id: documentID(z), ...askArguments(z) }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", question: "Which column drops first?" },
    tool: "dispatch_ask",
    relations: ["owned_by", "anchored_to", "followed_by", "mentions"],
  },
  "POST /api/v1/artifacts/{id}/blocks/{block_id}/keep": {
    arguments: (z) => ({
      id: documentID(z),
      block_id: z.string({ min: 1 }).describe("The decision's heading block."),
      text_sha256: z.string({ min: 1 }).describe("The decision's text_sha256, as it was read."),
    }),
    example: {
      id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11",
      block_id: "b7",
      text_sha256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
    },
  },
  "POST /api/v1/artifacts/{id}/comments": {
    arguments: (z) => ({ id: documentID(z), ...commentArguments(z) }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", body: "This passage needs a check." },
    tool: "dispatch_comment",
    relations: ["owned_by", "anchored_to", "answers", "replies_to", "mentions"],
  },
  "POST /api/v1/artifacts/{id}/edits": {
    arguments: (z) => ({ id: documentID(z), ...documentEditArguments(z) }),
    example: {
      id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11",
      ops: [{ op: "replace", find: "five seconds", with: "ten seconds" }],
    },
    tool: "dispatch_doc_edit",
    relations: ["mentions", "needs", "serves"],
  },
  "POST /api/v1/artifacts/{id}/plan-reviews": {
    arguments: (z) => ({
      id: documentID(z),
      version: z.number({ int: true, min: 1 }).describe("The plan version the review approved."),
    }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", version: 3 },
    tool: "dispatch_plan_approve",
  },
  "POST /api/v1/artifacts/{id}/rebuild": {
    arguments: (z) => ({
      id: documentID(z),
      markdown: z
        .string()
        .describe("The markdown to rebuild from; the saved markdown when absent.")
        .optional(),
    }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11" },
  },
  "POST /api/v1/artifacts/{id}/reviews": {
    arguments: (z) => ({
      id: documentID(z),
      state: z.enum(["approved", "changes_requested"]).describe("The review's verdict."),
      reason: z
        .string({ min: 1 })
        .describe("Why changes are requested; required for that state.")
        .optional(),
    }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", state: "approved" },
  },
  "POST /api/v1/artifacts/{id}/versions": {
    arguments: (z) => ({ id: documentID(z), ...namedVersionArguments(z) }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", summary: "Reviewed draft" },
  },
  "POST /api/v1/asks/{id}/answer": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The ask's uuid."),
      selected: z
        .array(z.string({ min: 1 }))
        .describe("The ids of the options chosen.")
        .optional(),
      text: z.string().describe("The answer in words.").optional(),
      expected_edited_at: z
        .string({ min: 1 })
        .describe("Answer only while the question is still the one read at this time.")
        .optional(),
      expected_answer_at: z
        .string({ min: 1 })
        .describe("Answer only while the current answer is still the one read at this time.")
        .nullable()
        .optional(),
    }),
    example: { id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c", text: "Drop the column on Friday." },
  },
  "POST /api/v1/asks/{id}/resolve": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The ask's uuid."),
      kind: z
        .enum(["retracted", "resolved"])
        .describe("Whether the question is moot or answered elsewhere."),
      reason: z.string({ min: 1 }).describe("Why it closes without an answer."),
    }),
    example: {
      id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c",
      kind: "resolved",
      reason: "Answered on the parent issue.",
    },
    tool: "dispatch_resolve_ask",
  },
  "POST /api/v1/broadcasts": {
    arguments: (z) => ({
      body: z.string({ min: 1 }).describe("The message every recipient receives."),
      delivery: z.enum(DELIVERY_MODES).describe("How it reaches each session."),
      session_ids: z.array(sessionID(z), { min: 1 }).describe("The sessions to send it to."),
    }),
    example: { body: "Staging is back.", delivery: "btw", session_ids: ["ses_42", "ses_43"] },
    relations: ["mentions"],
  },
  "POST /api/v1/comments/{id}/accept": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The suggestion comment's uuid.") }),
    example: { id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091" },
  },
  "POST /api/v1/comments/{id}/deliveries": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The comment's uuid."),
      target: z
        .string({ min: 1 })
        .describe("Which mention to retry: role:<name> or session:<id>.")
        .optional(),
      delivery: z.enum(DELIVERY_MODES).describe("How the retry reaches its session.").optional(),
    }),
    example: { id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091" },
  },
  "POST /api/v1/comments/{id}/reject": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The suggestion comment's uuid.") }),
    example: { id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091" },
  },
  "POST /api/v1/comments/{id}/reopen": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The comment's uuid.") }),
    example: { id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091" },
  },
  "POST /api/v1/comments/{id}/resolve": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The comment's uuid.") }),
    example: { id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091" },
    tool: "dispatch_resolve_comment",
  },
  "POST /api/v1/credential-grants/{id}/revoke": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The credential grant's id.") }),
    example: { id: "grant_7f3" },
  },
  "POST /api/v1/credential-requests/machine-lookup": {
    arguments: (z) => ({
      code: z.string({ min: 1 }).describe("The confirmation code the machine printed."),
    }),
    example: { code: "WXYZ-1234" },
  },
  "POST /api/v1/credential-requests/{id}/approve": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The credential request's id."),
      code: z.string({ min: 1 }).describe("The confirmation code the machine printed.").optional(),
    }),
    example: { id: "req_7f3", code: "WXYZ-1234" },
  },
  "POST /api/v1/credential-requests/{id}/deny": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The credential request's id."),
      code: z.string({ min: 1 }).describe("The confirmation code the machine printed.").optional(),
    }),
    example: { id: "req_7f3", code: "WXYZ-1234" },
  },
  "POST /api/v1/delivery/session-groups": {
    arguments: (z) => ({
      name: z.string({ min: 1 }).describe("What the group is called on the Delivery page."),
      sessions: z.array(sessionID(z), { min: 1 }).describe("The sessions the group holds."),
    }),
    example: { name: "Platform agents", sessions: ["ses_42", "ses_43"] },
  },
  "POST /api/v1/issues": {
    arguments: (z) => ({
      project: projectKey(z),
      title: z.string({ min: 1 }).describe("The issue's title."),
      parent: z.string({ min: 1 }).describe("The issue this one hangs under.").optional(),
      blocked_by: z
        .array(z.string({ min: 1 }))
        .describe("The issues it waits on.")
        .optional(),
      external: z
        .string({ min: 1 })
        .describe("An owner/repo#n reference to link it to.")
        .optional(),
      force: z
        .boolean()
        .describe("Create it although the duplicate check named similar issues.")
        .optional(),
      spec: z.string().describe("The primary document's markdown.").optional(),
      labels: z
        .array(z.string({ min: 1 }))
        .describe("The labels it starts with.")
        .optional(),
      priority: z
        .number({ int: true, min: 0, max: 3 })
        .describe("Coarse priority, 0 highest.")
        .nullable()
        .optional(),
      assignee: z
        .string()
        .describe("The email of the person who answers its asks.")
        .nullable()
        .optional(),
      components: componentsArgument(z),
      repo: z.string().describe("The repository it targets, owner/name.").nullable().optional(),
    }),
    example: { project: "INFRA", title: "The Going bar names only actions that exist" },
    tool: "dispatch_issue",
    relations: ["child_of", "blocked_by", "affects", "mentions"],
  },
  "POST /api/v1/issues/{key}/artifacts": {
    arguments: (z) => ({ key: issueKey(z), ...uploadArguments(z) }),
    example: { key: "INFRA-1042", name: "plan.md", content: "# Plan\n\nOne step." },
    tool: "dispatch_artifact",
    relations: ["attached_to", "mentions"],
  },
  "POST /api/v1/issues/{key}/artifacts/{slug}/edits": {
    arguments: (z) => ({ key: issueKey(z), slug: documentSlug(z), ...documentEditArguments(z) }),
    example: {
      key: "INFRA-1042",
      slug: "spec",
      ops: [{ op: "insert", markdown: "A new passage.", after: "the last line" }],
    },
    tool: "dispatch_doc_edit",
    relations: ["mentions", "needs", "serves"],
  },
  "POST /api/v1/issues/{key}/artifacts/{slug}/versions": {
    arguments: (z) => ({ key: issueKey(z), slug: documentSlug(z), ...namedVersionArguments(z) }),
    example: { key: "INFRA-1042", slug: "spec", summary: "Approved draft" },
  },
  "POST /api/v1/issues/{key}/asks": {
    arguments: (z) => ({ key: issueKey(z), ...askArguments(z) }),
    example: { key: "INFRA-1042", question: "Which column drops first?" },
    tool: "dispatch_ask",
    relations: ["owned_by", "anchored_to", "followed_by", "mentions"],
  },
  "POST /api/v1/issues/{key}/claim": {
    arguments: (z) => ({
      key: issueKey(z),
      force: z.boolean().describe("Take the claim although another holder is live.").optional(),
    }),
    example: { key: "INFRA-1042" },
    tool: "dispatch_claim",
  },
  "POST /api/v1/issues/{key}/comments": {
    arguments: (z) => ({ key: issueKey(z), ...commentArguments(z) }),
    example: { key: "INFRA-1042", body: "The harness measures this already." },
    tool: "dispatch_comment",
    relations: ["owned_by", "anchored_to", "answers", "replies_to", "mentions"],
  },
  "POST /api/v1/issues/{key}/messages": {
    arguments: (z) => ({
      key: issueKey(z),
      body: z.string({ min: 1 }).describe("The message's markdown."),
      target: z
        .string({ min: 1 })
        .describe("Who it reaches: role:<name> or session:<id>.")
        .optional(),
      delivery: z.enum(DELIVERY_MODES).describe("How it reaches that target.").optional(),
      urgency: z.enum(ASK_URGENCIES).describe("How hard it interrupts.").optional(),
      in_reply_to: z.string({ min: 1 }).describe("The message this answers.").optional(),
      in_reply_to_block: z
        .string({ min: 1 })
        .describe("The block of that message this answers.")
        .optional(),
    }),
    example: { key: "INFRA-1042", body: "The branch is green." },
    tool: "dispatch_message",
    relations: ["replies_to", "mentions"],
  },
  "POST /api/v1/issues/{key}/results": {
    arguments: (z) => ({
      key: issueKey(z),
      check: z.string({ min: 1 }).describe("The check block this result is for."),
      role: z.string({ min: 1, max: 32 }).describe("The phase that ran it, lowercase."),
      verdict: z.enum(["pass", "fail"]).describe("Whether the check passed."),
      commit: z.string({ min: 40, max: 40 }).describe("The 40-character commit it ran at."),
      surface: z.string().describe("Where it ran.").optional(),
      run: z.string().describe("The run it ran in.").optional(),
      observed: z.string().describe("What was observed.").optional(),
      negative_control: z
        .string()
        .describe("The control that proves the check can fail.")
        .optional(),
      failed: z.number({ int: true, min: 0 }).describe("How many cases failed.").optional(),
      skipped: z.number({ int: true, min: 0 }).describe("How many cases were skipped.").optional(),
      excluded: z
        .number({ int: true, min: 0 })
        .describe("How many cases were excluded.")
        .optional(),
      screenshots: z
        .array(z.string({ min: 1 }))
        .describe("Image documents showing the result.")
        .optional(),
    }),
    example: {
      key: "INFRA-1042",
      check: "b12",
      role: "tester",
      verdict: "pass",
      commit: "0123456789abcdef0123456789abcdef01234567",
    },
    tool: "dispatch_result",
  },
  "POST /api/v1/machine-logins/{id}/revoke": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The machine login's id.") }),
    example: { id: "login_7f3" },
  },
  "POST /api/v1/me/agent-tokens": {
    arguments: (z) => ({
      name: z.string({ min: 1 }).describe("What the token is for."),
      scopes: z
        .array(z.string({ min: 1 }))
        .describe("The scopes it holds; the caller's own when absent.")
        .optional(),
    }),
    example: { name: "laptop" },
  },
  "POST /api/v1/messages/{id}/deliveries": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The message's uuid."),
      delivery: z.enum(DELIVERY_MODES).describe("How the retry reaches its session.").optional(),
    }),
    example: { id: "3c4d5e6f-7081-4923-ab4c-5d6e7f809123" },
  },
  "POST /api/v1/projects": {
    arguments: (z) => ({
      key: projectKey(z),
      name: z.string({ min: 1 }).describe("The project's name, as a person reads it."),
    }),
    example: { key: "INFRA", name: "Infrastructure" },
    tool: "dispatch_project",
  },
  "POST /api/v1/projects/{key}/architecture-source/sync": {
    arguments: (z) => ({ key: projectKey(z) }),
    example: { key: "INFRA" },
    relations: ["depends_on", "affects"],
  },
  "POST /api/v1/projects/{key}/architecture-sources/sync": {
    arguments: (z) => ({ key: projectKey(z) }),
    example: { key: "INFRA" },
    tool: "dispatch_architecture_sync",
    relations: ["depends_on", "affects"],
  },
  "POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": {
    arguments: (z) => ({ key: projectKey(z), ...repositoryPath(z) }),
    example: { key: "INFRA", owner: "acme", repo: "platform" },
    relations: ["depends_on", "affects"],
  },
  "POST /api/v1/projects/{key}/artifacts": {
    arguments: (z) => ({ key: projectKey(z), ...uploadArguments(z) }),
    example: { key: "INFRA", name: "notes.md", content: "# Notes\n\nA project document." },
    tool: "dispatch_artifact",
    relations: ["mentions"],
  },
  "POST /api/v1/projects/{key}/artifacts/{slug}/edits": {
    arguments: (z) => ({ key: projectKey(z), slug: documentSlug(z), ...documentEditArguments(z) }),
    example: { key: "INFRA", slug: "notes", ops: [{ op: "delete", find: "A stale line." }] },
    tool: "dispatch_doc_edit",
    relations: ["mentions", "needs", "serves"],
  },
  "POST /api/v1/projects/{key}/artifacts/{slug}/versions": {
    arguments: (z) => ({ key: projectKey(z), slug: documentSlug(z), ...namedVersionArguments(z) }),
    example: { key: "INFRA", slug: "notes", summary: "Quarter close" },
  },
  "POST /api/v1/projects/{key}/related": {
    arguments: (z) => ({
      key: projectKey(z),
      title: z
        .string()
        .describe("The title to check, as an issue create would send it.")
        .optional(),
      text: z.string().describe("The text to check, as long as a spec.").optional(),
      exclude_issue: z
        .string({ min: 1 })
        .describe("Leave this issue and everything it holds out.")
        .optional(),
      keywords_only: z
        .boolean()
        .describe("Skip meaning search and rank by words alone.")
        .optional(),
    }),
    example: { key: "INFRA", title: "The Going bar names only actions that exist" },
    tool: "dispatch_related",
  },
  "POST /api/v1/signins/{id}/revoke": {
    arguments: (z) => ({ id: z.string({ min: 1 }).describe("The sign-in's id.") }),
    example: { id: "signin_7f3" },
  },
  "PUT /api/v1/asks/{id}/followers/{session_id}": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The ask's uuid."),
      session_id: sessionID(z),
    }),
    example: { id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c", session_id: "ses_42" },
    tool: "dispatch_follow",
    relations: ["followed_by"],
  },
  "PUT /api/v1/me/agents/{session_id}/state": {
    arguments: (z) => ({
      session_id: sessionID(z),
      cleared_before: z
        .string({ min: 1 })
        .describe("Hide exchanges at or before this time.")
        .optional(),
      read_through: z
        .string({ min: 1 })
        .describe("Mark the conversation read through this time.")
        .optional(),
      read_replies: z
        .array(z.string({ min: 1 }))
        .describe("Mark these replies read by id.")
        .optional(),
    }),
    example: { session_id: "ses_42", read_through: "2026-11-06T12:00:00Z" },
  },
  "PUT /api/v1/me/asks/{id}/snooze": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The ask's uuid."),
      snoozed_until: z.string({ min: 1 }).describe("When the row returns to the Inbox (RFC3339)."),
    }),
    example: { id: "7c2a1f08-8f6e-4f0a-9b7c-1d2e3f4a5b6c", snoozed_until: "2026-11-07T09:00:00Z" },
  },
  "PUT /api/v1/me/documents/{id}/read": {
    arguments: (z) => ({
      id: documentID(z),
      version: z.number({ int: true, min: 0 }).describe("The version the caller read."),
    }),
    example: { id: "4f1d9d2e-0b4a-4a62-9d0e-2b9a7c6f1b11", version: 7 },
  },
  "PUT /api/v1/me/issues/{key}/state": {
    arguments: (z) => ({
      key: issueKey(z),
      pinned: z.boolean().describe("Whether the caller pins this issue.").optional(),
      last_read_seq: z
        .number({ int: true, min: 0 })
        .describe("The event the caller has read through.")
        .optional(),
      dismissed: z
        .array(z.string({ min: 1 }))
        .describe("The notices the caller dismissed.")
        .optional(),
      seq: z
        .number({ int: true, min: 0 })
        .describe("Apply only while this is still the state's sequence.")
        .optional(),
    }),
    example: { key: "INFRA-1042", pinned: true },
  },
  "PUT /api/v1/me/threads/{id}/read": {
    arguments: (z) => ({
      id: z.string({ min: 1 }).describe("The thread's uuid."),
      read_through: z.string({ min: 1 }).describe("How far the caller has read (RFC3339)."),
    }),
    example: { id: "5e6f7081-9234-4ab5-8c6d-7e8f90123456", read_through: "2026-11-06T12:00:00Z" },
  },
  "PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}": {
    arguments: (z) => ({
      key: projectKey(z),
      ...repositoryPath(z),
      branch: z.string({ min: 1 }).describe("The branch the model is read from."),
    }),
    example: { key: "INFRA", owner: "acme", repo: "platform", branch: "main" },
    relations: ["depends_on", "affects"],
  },
  "PUT /api/v1/projects/{key}/default-assignee": {
    arguments: (z) => ({
      key: projectKey(z),
      assignee: z
        .string()
        .describe("The email of the person unassigned issues go to; null clears it.")
        .nullable(),
    }),
    example: { key: "INFRA", assignee: "someone@example.com" },
  },
  "PUT /api/v1/settings/delivery": {
    arguments: (z) => ({
      deploy_repo: z.string({ min: 1 }).describe("The deploy repository, owner/name."),
      deploy_workflow_path: z.string({ min: 1 }).describe("The deploy workflow's path in it."),
      production_job_names: z
        .array(z.string({ min: 1 }), { min: 1 })
        .describe("The jobs that deploy production."),
      pr_checks_workflow_path: z.string({ min: 1 }).describe("The PR-checks workflow's path."),
      production_apply_step_name: z
        .string({ min: 1 })
        .describe("The step that applies production.")
        .nullable()
        .optional(),
      population_authors: z
        .array(z.string({ min: 1 }))
        .describe("The authors whose pull requests count.")
        .optional(),
      excluded_repos: z
        .array(z.string({ min: 1 }))
        .describe("Repositories the timeline leaves out.")
        .optional(),
      staging_e2e_job_name: z
        .string({ min: 1 })
        .describe("The staging end-to-end job.")
        .nullable()
        .optional(),
      staging_apply_job_name: z
        .string({ min: 1 })
        .describe("The staging deploy job.")
        .nullable()
        .optional(),
      pr_checks_rollup_job_name: z
        .string({ min: 1 })
        .describe("The PR-checks roll-up job.")
        .nullable()
        .optional(),
    }),
    example: {
      deploy_repo: "acme/platform",
      deploy_workflow_path: ".github/workflows/deploy.yml",
      production_job_names: ["deploy-production"],
      pr_checks_workflow_path: ".github/workflows/checks.yml",
    },
  },
  "PUT /api/v1/settings/repo-projects/{owner}/{repo}": {
    arguments: (z) => ({
      ...repositoryPath(z),
      project: projectKey(z),
      default: z.boolean().describe("Make this the project's one default repository.").optional(),
    }),
    example: { owner: "acme", repo: "platform", project: "INFRA" },
  },
} as const satisfies Readonly<Record<string, GoingRequestSpec>>;

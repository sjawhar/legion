import type { SchemaApi, SchemaNode, ToolArgumentsShape } from "./tool-schema";

export interface DispatchToolSpec {
  readonly name: string;
  readonly description: string;
  readonly arguments: <E extends SchemaNode<E>>(z: SchemaApi<E>) => ToolArgumentsShape;
  readonly validation?: {
    readonly check: (value: unknown) => boolean;
    readonly message: string;
  };
  /** Tool has no extensible arguments; reject unknown keys in every host. */
  readonly strict?: boolean;
}

/** Builds a host-owned schema and applies any tool-level cross-field validation. */
export function dispatchToolSchema<E extends SchemaNode<E>>(
  spec: DispatchToolSpec,
  z: SchemaApi<E>,
  opts?: { readonly strict?: boolean }
): E {
  const shape = spec.arguments(z) as Record<string, E>;
  const strict = opts?.strict ?? spec.strict;
  const schemaOptions = strict === undefined ? undefined : { strict };
  return spec.validation === undefined
    ? z.object(shape, schemaOptions)
    : z.refineObject(shape, spec.validation.check, spec.validation.message, schemaOptions);
}

const ISSUE_REFERENCE =
  "An issue is a native KEY or external owner/repo#n reference; an external reference creates its native issue in the repository's dashboard-configured project or, failing that, the default project (DISPATCH_DEFAULT_PROJECT).";

const OWNER_REFERENCE =
  "Exactly one of issue and project is required. An issue is a native KEY or external owner/repo#n reference; a project is a project key such as CORE and addresses an unlinked project document named by artifact.";

function documentOwnerValidation(
  requireArtifact: boolean,
  alwaysRequireArtifact = false
): NonNullable<DispatchToolSpec["validation"]> {
  return {
    check: (value) => {
      const input = value as {
        readonly issue?: unknown;
        readonly project?: unknown;
        readonly artifact?: unknown;
        readonly ref?: unknown;
      };
      const hasIssue = typeof input.issue === "string";
      const hasProject = typeof input.project === "string";
      const hasArtifact = typeof input.artifact === "string";
      const hasRef = typeof input.ref === "string";
      return (
        (hasIssue !== hasProject || (!hasIssue && !hasProject && hasRef)) &&
        (!hasProject || !requireArtifact || hasArtifact) &&
        (!alwaysRequireArtifact || hasArtifact || hasRef)
      );
    },
    message: alwaysRequireArtifact
      ? "Exactly one of issue and project is required; artifact or ref must name the document."
      : "Exactly one of issue and project is required; with project, artifact names the document.",
  };
}

/** dispatch_comment: the document-owner rule plus `turn` only alongside `reply_to_ask`. */
const commentValidation: NonNullable<DispatchToolSpec["validation"]> = (() => {
  const owner = documentOwnerValidation(true);
  return {
    check: (value) => {
      const input = value as { readonly turn?: unknown; readonly reply_to_ask?: unknown };
      return (
        owner.check(value) && (input.turn === undefined || typeof input.reply_to_ask === "string")
      );
    },
    message: `${owner.message} turn requires reply_to_ask.`,
  };
})();
export const SPEC_SECTIONS = [
  "Summary",
  "Decisions needed",
  "New since we talked",
  "Acceptance",
  "Requirements",
  "Design",
  "Errors",
  "Testing",
  "Rejected",
] as const;

const SPEC_WRITING_GUIDANCE =
  `When writing a spec, use these sections in order: ${SPEC_SECTIONS.join(", ")}. ` +
  "Write for a reader who has not seen the code: plain sentences, every identifier expanded on " +
  "first use, no coined shorthand; see skills/dispatch Writing for the human and Writing a spec.";

/** Ask urgency levels the Dispatch server accepts, in ascending order. */
export const ASK_URGENCIES = ["low", "med", "high", "blocking"] as const;

/** Issue lifecycle statuses the Dispatch server accepts (`model.IssueStatuses`), in lifecycle order. */
export const ISSUE_STATUSES = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;
export type IssueStatus = (typeof ISSUE_STATUSES)[number];

/** Document edit operations the Dispatch server applies. */
export const DOC_EDIT_OPS = ["replace", "delete", "insert", "retype", "move"] as const;

export const dispatchToolSpecs = [
  {
    name: "dispatch_issue",
    description:
      "Create a native Dispatch issue for newly tracked work. Search first with dispatch_search; if potentially duplicate issues exist, this returns 409 POSSIBLE_DUPLICATE unless force is true after reading them. " +
      `Do not use it when an existing issue already covers the work; read or update that issue instead. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      project: z.string().describe("Project key for the new issue."),
      title: z.string().describe("Concise issue title."),
      parent: z.string().describe("Optional parent issue.").optional(),
      external: z.string().describe("Optional external issue reference.").optional(),
      force: z
        .boolean()
        .describe(
          "Create even though POSSIBLE_DUPLICATE listed similar issues; pass it only after reading them."
        )
        .optional(),
      spec: z
        .string()
        .describe(`Optional initial primary-document markdown. ${SPEC_WRITING_GUIDANCE}`)
        .optional(),
      labels: z
        .array(z.string({ min: 1, max: 40 }), { max: 20 })
        .describe("Optional initial labels, at most 20 labels of up to 40 characters.")
        .optional(),
      priority: z
        .number({ int: true, min: 0, max: 3 })
        .describe("Optional coarse priority: P0 is highest and P3 is lowest.")
        .optional(),
      assignee: z
        .string()
        .describe(
          "GitHub login of the human who answers this issue's asks; defaults to your owner when you act for a person, else the parent's assignee, else unassigned."
        )
        .optional(),
    }),
  },
  {
    name: "dispatch_issue_update",
    description:
      "Update an existing issue: move its lifecycle status, retitle it, replace its labels, link a URL " +
      "(the pull request that delivers it, a run, a document), or set its route. Status is one of " +
      `${ISSUE_STATUSES.join(", ")}; outside Legion, move it yourself as the work advances; inside ` +
      "Legion the daemon moves it. external_links are " +
      "merged into the issue's existing links by URL, so linking the pull request you just opened " +
      "keeps every earlier link. Priority is the human's and is not settable here. At least one " +
      `field besides issue is required. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      status: z.enum(ISSUE_STATUSES).describe("New lifecycle status.").optional(),
      title: z.string({ min: 1 }).describe("Replacement title.").optional(),
      labels: z
        .array(z.string({ min: 1, max: 40 }), { max: 20 })
        .describe(
          "Replacement label set, at most 20 labels of up to 40 characters; replaces every existing label."
        )
        .optional(),
      external_links: z
        .array(z.string({ min: 1 }))
        .describe("URLs to link; merged into the issue's existing external links by URL.")
        .optional(),
      route: z
        .string()
        .describe("Route the issue to role:<name> or session:<id>; an empty string clears it.")
        .optional(),
    }),
    validation: {
      check: (value) => {
        const input = value as {
          readonly status?: unknown;
          readonly title?: unknown;
          readonly labels?: unknown;
          readonly external_links?: unknown;
          readonly route?: unknown;
        };
        return (
          typeof input.status === "string" ||
          typeof input.title === "string" ||
          Array.isArray(input.labels) ||
          Array.isArray(input.external_links) ||
          typeof input.route === "string"
        );
      },
      message:
        "Issue update requires at least one field besides issue: status, title, labels, external_links, or route.",
    },
    strict: true,
  },
  {
    name: "dispatch_ask",
    description:
      "Open a durable, answerable decision or human to-do on an issue or project document. Do not use it for a status update or discussion; " +
      "use dispatch_message instead. Use kind: action for a to-do a human must complete; it has fixed Done / Can't answers. " +
      "Anchor a document question, thread reply_to/reply_to_ask, or cite a dispatch:// " +
      'reference — it must be answerable from its own text and anchor alone, never "see above". A quote anchor is pinned to its block. Question is at most 800 ' +
      `characters and has at most 8 options. ${OWNER_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z.string().describe("Project document artifact id, slug, or filename.").optional(),
      ref: z
        .string()
        .describe(
          "Optional dispatch:// reference (issue, document, message, or ask); appended to the question and rendered as a link."
        )
        .optional(),
      question: z.string({ max: 800 }).describe("Decision question, at most 800 characters."),
      kind: z.enum(["action"]).describe("Optional human to-do ask kind.").optional(),
      options: z
        .array(
          z.object({
            label: z.string().describe("Selectable option label."),
            description: z.string().describe("Optional option context.").optional(),
          }),
          { max: 8 }
        )
        .describe("Up to 8 choices, each an object { label, description? } (never a bare string).")
        .optional(),
      multiple: z.boolean().describe("Whether multiple choices may be selected.").optional(),
      urgency: z.enum(ASK_URGENCIES).describe("Optional decision urgency.").optional(),
      anchor: z
        .object({
          artifact: z.string().describe("Artifact slug or id containing the quoted text."),
          quote: z.string().describe("Exact text the decision concerns."),
          occurrence: z
            .number({ int: true, min: 0 })
            .describe("Zero-based occurrence of the quote.")
            .optional(),
        })
        .describe("Optional document location for the question.")
        .optional(),
    }),
    validation: documentOwnerValidation(true),
  },
  {
    name: "dispatch_edit_ask",
    description:
      "Edit an open question in place. Use it to correct or refine the same decision; retract the " +
      "old ask and open a new one when the decision itself changes. Previous text remains in the " +
      "event log. Only the asking session can edit it; answered or resolved asks cannot be edited.",
    arguments: (z) => ({
      ask: z.string().describe("Ask id to edit."),
      question: z
        .string({ max: 800 })
        .describe("Replacement decision question, at most 800 characters.")
        .optional(),
      options: z
        .array(
          z.object({
            label: z.string().describe("Selectable option label."),
            description: z.string().describe("Optional option context.").optional(),
          }),
          { max: 8 }
        )
        .describe("Replacement choices, at most 8.")
        .optional(),
      multiple: z.boolean().describe("Whether multiple choices may be selected.").optional(),
      urgency: z.enum(ASK_URGENCIES).describe("Replacement decision urgency.").optional(),
    }),
    validation: {
      check: (value) => {
        const input = value as {
          readonly question?: unknown;
          readonly options?: unknown;
          readonly multiple?: unknown;
          readonly urgency?: unknown;
        };
        return (
          typeof input.question === "string" ||
          Array.isArray(input.options) ||
          typeof input.multiple === "boolean" ||
          typeof input.urgency === "string"
        );
      },
      message: "Ask edit requires at least one field besides ask.",
    },
  },
  {
    name: "dispatch_resolve_ask",
    description:
      "Retract an open question that is moot or resolve one after finding the answer. This closes the question without answering it.",
    arguments: (z) => ({
      ask: z.string().describe("Ask id to close."),
      kind: z
        .enum(["retracted", "resolved"])
        .describe("Whether the ask is retracted or self-resolved."),
      reason: z.string({ min: 1 }).describe("Why the open ask no longer needs a human answer."),
    }),
  },
  {
    name: "dispatch_resolve_comment",
    description:
      "Resolve a review comment thread once it has been addressed - typically your own comment " +
      "after the document was fixed. Any session or human may resolve any open comment on an " +
      "open issue or project document; reopening a resolved comment is human-only (the dashboard). " +
      "Not for asks: use dispatch_resolve_ask.",
    arguments: (z) => ({
      comment: z
        .string()
        .describe(
          "Comment id (uuid), or a dispatch://KEY/comment/<id> or " +
            "dispatch://PROJECT/artifact/<slug>/comment/<id> reference; a reference accepts an " +
            "8+ character id prefix that is unique on its owner."
        ),
    }),
    strict: true,
  },
  {
    name: "dispatch_follow",
    description:
      "Follow or unfollow an ask. Every session that opens or replies to an ask follows it: its answer, " +
      "edits, resolution, and replies reach that session directly. Unfollow to stop; follow to rejoin or " +
      "to hear an ask you never wrote to. Whole-issue subscription is separate: envoy_subscribe " +
      "notifications.dispatch.issue.<KEY>.>",
    arguments: (z) => ({
      ask: z.string().describe("Full ask id (uuid)."),
      action: z.enum(["follow", "unfollow"]).describe("follow | unfollow"),
    }),
    strict: true,
  },
  {
    name: "dispatch_comment",
    description:
      "Add review feedback to an issue or project document quote, or reply to a question asked with dispatch_ask. " +
      "Do not use it for an exact replacement; use " +
      `dispatch_suggest instead. A quote anchor is pinned to its block. Body is at most 2,000 characters. ${OWNER_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z
        .string()
        .describe("Project document artifact id, slug, or filename required when quote is given.")
        .optional(),
      ref: z.string().describe("Optional dispatch:// issue or document reference.").optional(),
      quote: z.string().describe("Optional exact quoted document text.").optional(),
      occurrence: z
        .number({ int: true, min: 0 })
        .describe("Optional zero-based occurrence of quote.")
        .optional(),
      body: z.string({ max: 2000 }).describe("Review comment, at most 2,000 characters."),
      reply_to: z
        .string()
        .describe(
          "A comment id (uuid); replying to any comment in a thread continues that thread (an " +
            "ask's clarification thread included). To reply to an ask, use reply_to_ask with the " +
            "ask id instead."
        )
        .optional(),
      reply_to_ask: z
        .string()
        .describe(
          "Optional ask id to reply to, threading this comment under that question. Mutually " +
            "exclusive with reply_to."
        )
        .optional(),
      turn: z
        .enum(["agent", "human"])
        .describe(
          "Only with reply_to_ask: who holds the turn after this reply. agent: a progress note - " +
            "you keep the turn and the ask stays 'Waiting on agents' for the human; human (default): " +
            "you need the human to act - the ask returns to 'Waiting on you'."
        )
        .optional(),
    }),
    validation: commentValidation,
  },
  {
    name: "dispatch_suggest",
    description:
      "Propose an exact replacement for quoted document text. Do not use it for general feedback; use " +
      `dispatch_comment instead. Optional explanation is at most 2,000 characters. ${OWNER_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z
        .string()
        .describe(
          "Project document artifact id, slug, or filename containing the quoted text; optional when ref names it."
        )
        .optional(),
      ref: z.string().describe("Optional dispatch:// issue or document reference.").optional(),
      quote: z.string().describe("Exact document text to replace."),
      replace_with: z.string().describe("Replacement text."),
      body: z
        .string({ max: 2000 })
        .describe("Optional rationale, at most 2,000 characters.")
        .optional(),
      occurrence: z
        .number({ int: true, min: 0 })
        .describe("Optional zero-based occurrence of quote.")
        .optional(),
    }),
    validation: documentOwnerValidation(true, true),
  },
  {
    name: "dispatch_message",
    description:
      "Post a note humans must read now: a reply to a human's message, a deliverable that landed, or a blocker only " +
      "they can clear. Never progress or status updates - Dispatch is a high-signal record, not a log. Not a decision " +
      `(dispatch_ask) or document feedback (dispatch_comment). Body is at most 2,000 characters. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      body: z.string({ max: 2000 }).describe("Update text, at most 2,000 characters."),
      in_reply_to: z
        .string()
        .describe(
          "Optional message id or dispatch://KEY/message/<id> reference to reply to, threading " +
            "this message under it so the reply stays with the original in the Conversation."
        )
        .optional(),
    }),
  },
  {
    name: "dispatch_doc_edit",
    description:
      "Apply deterministic document edits: replace or delete quoted text, insert markdown at an anchor, retype an identified paragraph or typed block into a schema-declared typed block, and delete or move a whole block by its id. " +
      "Do not use it for review feedback or for reading; use dispatch_comment, dispatch_suggest, or dispatch_doc_read instead. " +
      "For replace, delete, and quote anchors, find text as rendered: inline Markdown (**bold**, `code`) is tolerated; a leading '# ' matches a heading. replace is inline: with is the new text of the matched span, so a leading list or heading marker stays literal text. " +
      "A delete whose find is a block's entire text removes the block (a list emptied of its items goes too); delete with block removes any block by id, and move with block relocates one. " +
      'Insert and move anchors also accept "start", "end", "heading:<exact heading text>", and "block:<id>"; block ids are the #id of a typed block or a row of GET /api/v1/artifacts/{id}/blocks. ' +
      `The spec (or any document) holds requirements, design, and decisions - never progress, status, or timestamps. ${OWNER_REFERENCE} ${SPEC_WRITING_GUIDANCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z
        .string()
        .describe("Project document artifact id, slug, or filename; optional when ref names it.")
        .optional(),
      ref: z.string().describe("Optional dispatch:// issue or document reference.").optional(),
      ops: z
        .array(
          z.object({
            op: z.enum(DOC_EDIT_OPS).describe("Edit operation."),
            find: z
              .string()
              .describe(
                "Text of the target as rendered, for replace or delete; inline markdown (**bold**, `code`) is tolerated; a leading '# ' matches a heading. A delete of a block's entire text removes the block."
              )
              .optional(),
            with: z
              .string()
              .describe(
                "Replacement text for replace, parsed as inline markdown within the matched block; a leading list or heading marker is literal text."
              )
              .optional(),
            occurrence: z
              .number({ int: true, min: 0 })
              .describe("Optional zero-based match occurrence.")
              .optional(),
            markdown: z.string().describe("Markdown to insert.").optional(),
            after: z
              .string()
              .describe(
                'Insert or move after this anchor: a quote of the neighbouring block\'s text, or one of "start", "end", "heading:<exact heading text>", "block:<id>".'
              )
              .optional(),
            before: z
              .string()
              .describe(
                'Insert or move before this anchor: a quote of the neighbouring block\'s text, or one of "start", "end", "heading:<exact heading text>", "block:<id>".'
              )
              .optional(),
            block: z
              .string()
              .describe(
                "Block id for retype, delete, or move: the #id of a typed block, or an id from GET /api/v1/artifacts/{id}/blocks."
              )
              .optional(),
            type: z.string().describe("Typed block name for retype.").optional(),
            attributes: z.unknown().describe("Typed block attributes for retype.").optional(),
          })
        )
        .describe("Flat tagged edits; the server validates fields required for each operation."),
      summary: z.string().describe("Optional named-version summary.").optional(),
    }),
    validation: documentOwnerValidation(true, true),
  },
  {
    name: "dispatch_doc_read",
    description:
      "Read a live document or a named document version. Do not use it for issue status, asks, or events; " +
      "use dispatch_read instead. Supply ref, issue, or project plus artifact; issue plus an omitted artifact reads the primary document. " +
      OWNER_REFERENCE,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z
        .string()
        .describe(
          "Optional project document artifact id, slug, or filename; primary document by default for an issue."
        )
        .optional(),
      version: z.number({ int: true, min: 1 }).describe("Optional version number.").optional(),
      ref: z.string().describe("Optional dispatch:// document reference.").optional(),
    }),
    validation: documentOwnerValidation(true),
  },
  {
    name: "dispatch_request_approval",
    description:
      "Ask a human to approve a document at its current version - the exception path for a spec " +
      "that departs from what was settled or proposes children, not a step for every issue. Opens an " +
      "approval ask (Approve / Request changes) in the human's Inbox; the answer pins a review to the " +
      "document version and arrives as artifact.approved or artifact.changes_requested. A later edit " +
      "makes an approval stale; request again for the new version. Idempotent while a request is open. " +
      OWNER_REFERENCE,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z
        .string()
        .describe(
          "Project document artifact id, slug, or filename; primary document by default for an issue."
        )
        .optional(),
    }),
    validation: documentOwnerValidation(true),
  },
  {
    name: "dispatch_artifact",
    description:
      "Attach a local file or inline text as an issue artifact or project document. Do not use it to edit a live document; use " +
      `dispatch_doc_edit instead. Exactly one of path or content is required; artifacts are limited to 25 MiB. ${OWNER_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key for an unlinked document.").optional(),
      name: z.string().describe("Artifact filename shown in Dispatch."),
      path: z.string().describe("Local path to the file to upload.").optional(),
      content: z.string().describe("Inline text to store as a Markdown document.").optional(),
      summary: z.string().describe("Optional version summary.").optional(),
    }),
    validation: {
      check: (value) => {
        const input = value as {
          readonly issue?: unknown;
          readonly project?: unknown;
          readonly path?: unknown;
          readonly content?: unknown;
        };
        return (
          documentOwnerValidation(false).check(value) &&
          (typeof input.path === "string") !== (typeof input.content === "string")
        );
      },
      message:
        "Exactly one of path or content is required. Exactly one of issue and project is required; with project, artifact names the document.",
    },
  },
  {
    name: "dispatch_read",
    description:
      "Read an issue or project-document summary, targeted ask, or targeted comment reply chain. Do not use it for document " +
      "contents; use dispatch_doc_read instead. Supply ref, issue, or project plus artifact. " +
      "Every read ends with `Referenced by:` (what cites or hangs off this node, each with its dispatch:// address, " +
      "an excerpt, and when) and `Links:` (what it cites), so tracing provenance is one call. " +
      OWNER_REFERENCE,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z.string().describe("Project document artifact id, slug, or filename.").optional(),
      ref: z.string().describe("Optional dispatch:// issue or document reference.").optional(),
    }),
    validation: documentOwnerValidation(true),
  },
  {
    name: "dispatch_search",
    description:
      "Search every issue, document, comment, ask, and message for a keyword or phrase and get deep links. " +
      "Use it before creating an issue or a design document, and to find where a word was written. " +
      'Websearch syntax: "quoted phrase", -excluded, OR.',
    arguments: (z) => ({
      query: z
        .string({ min: 2 })
        .describe("Keyword, phrase, or websearch expression; at least 2 characters."),
      project: z.string().describe("Optional project key to search within.").optional(),
      limit: z
        .number({ int: true, min: 1, max: 50 })
        .describe("Maximum results, 1-50; default 20.")
        .optional(),
    }),
  },
  {
    name: "dispatch_open_asks",
    description:
      "List this session's active unanswered asks across issues and project documents, including age and whose reply is needed. Call before saying you are waiting for human input.",
    arguments: () => ({}),
    strict: true,
  },
  {
    name: "dispatch_whoami",
    description:
      "Who Dispatch takes this session for: {session, owner}. owner is the lowercase GitHub login of the human whose personal token you run under (the default assignee of issues you create), or null under the shared token.",
    arguments: () => ({}),
    strict: true,
  },
] as const satisfies readonly DispatchToolSpec[];

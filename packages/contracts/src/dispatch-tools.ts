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
  /** A schema-valid call shown after this tool rejects malformed arguments. */
  readonly example: Readonly<Record<string, unknown>>;
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

const documentEditValidation: NonNullable<DispatchToolSpec["validation"]> = {
  check: (value) => {
    if (!documentOwnerValidation(true, true).check(value)) return false;
    const input = value as { readonly precondition?: unknown };
    if (input.precondition === undefined) return true;
    if (typeof input.precondition !== "object" || input.precondition === null) return false;
    const precondition = input.precondition as {
      readonly document?: unknown;
      readonly blocks?: unknown;
    };
    return (typeof precondition.document === "string") !== Array.isArray(precondition.blocks);
  },
  message:
    "Exactly one of issue and project is required; artifact or ref must name the document. A precondition selects exactly one of document or blocks.",
};

/** dispatch_comment: the document-owner rule plus `turn` only alongside `reply_to_ask`. */
const commentOwner = documentOwnerValidation(true);
const commentValidation: NonNullable<DispatchToolSpec["validation"]> = {
  check: (value) => {
    const input = value as { readonly turn?: unknown; readonly reply_to_ask?: unknown };
    return (
      commentOwner.check(value) &&
      (input.turn === undefined || typeof input.reply_to_ask === "string")
    );
  },
  message: `${commentOwner.message} turn requires reply_to_ask.`,
};

/** Component attachment modes an issue write accepts. */
export const ISSUE_COMPONENTS_MODES = ["inherit", "explicit", "none"] as const;

/** The `components` argument of dispatch_issue and dispatch_issue_update. */
function componentsArgument<E extends SchemaNode<E>>(z: SchemaApi<E>): E {
  return z
    .object({
      mode: z
        .enum(ISSUE_COMPONENTS_MODES)
        .describe(
          "inherit: take the parent chain's attachment (the default; deletes this issue's own). explicit: attach to ids. none: not architectural work, with reason."
        ),
      ids: z
        .array(z.string({ min: 1 }), { min: 1, max: 50 })
        .describe(
          "For mode explicit: bare component ids from the project's architecture model (web, dispatch-server), not external ones."
        )
        .optional(),
      reason: z
        .string({ min: 1 })
        .describe("For mode none: why this issue is not architectural (process, hiring, ops).")
        .optional(),
    })
    .describe(
      "Attach the issue to architecture components. Attach the root before decomposing it; children inherit unless they choose."
    );
}

export const SPEC_SECTIONS = [
  "Summary",
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

/** Longest ask question the Dispatch server accepts, in characters. */
export const ASK_QUESTION_MAX = 800;

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

export function isIssueStatus(value: string): value is IssueStatus {
  return (ISSUE_STATUSES as readonly string[]).includes(value);
}

/** Document edit operations the Dispatch server applies. */
export const DOC_EDIT_OPS = [
  "replace",
  "delete",
  "insert",
  "retype",
  "move",
  "delete_row",
  "delete_column",
] as const;

export const dispatchToolSpecs = [
  {
    name: "dispatch_issue",
    example: { project: "DSP", title: "Native workspace" },
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
      components: componentsArgument(z).optional(),
    }),
  },
  {
    name: "dispatch_issue_update",
    example: { issue: "DSP-1", status: "in_progress" },
    description:
      "Update an existing issue: move its lifecycle status, retitle it, replace its labels, set " +
      "its priority, link a URL (the pull request that delivers it, a run, a document), set its " +
      "route, set or clear its parent, or attach it to architecture components. Status is one of " +
      `${ISSUE_STATUSES.join(", ")}; outside Legion, move it yourself as the work advances; inside ` +
      "Legion the daemon moves it. external_links are " +
      "merged into the issue's existing links by URL, so linking the pull request you just opened " +
      "keeps every earlier link. components replaces the issue's own attachment. A closed issue " +
      "takes only rank, components, and a reopening status (any status but done); everything " +
      "else, priority included, waits for the reopen. " +
      "priority is yours to set and a human overrides it; rank, the board's own order, is not " +
      "settable here. At least one " +
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
      // `.describe()` comes last here, unlike every other field. On the OMP host's Zod facade a
      // description attached between `.nullable()` and `.optional()` is lost entirely — the
      // emitted property carries no description key at all — and describing last is the order
      // that puts it on the field.
      priority: z
        .number({ int: true, min: 0, max: 3 })
        .nullable()
        .optional()
        .describe("Coarse priority: 0 is P0 (highest) through 3 is P3 (lowest); null clears it."),
      external_links: z
        .array(z.string({ min: 1 }))
        .describe("URLs to link; merged into the issue's existing external links by URL.")
        .optional(),
      route: z
        .string()
        .describe("Route the issue to role:<name> or session:<id>; an empty string clears it.")
        .optional(),
      parent: z
        .string()
        .describe("Parent issue key in the same project; an empty string clears the parent.")
        .optional(),
      components: componentsArgument(z).optional(),
    }),
    validation: {
      check: (value) => {
        const input = value as {
          readonly status?: unknown;
          readonly title?: unknown;
          readonly labels?: unknown;
          readonly priority?: unknown;
          readonly external_links?: unknown;
          readonly route?: unknown;
          readonly parent?: unknown;
          readonly components?: unknown;
        };
        return (
          typeof input.status === "string" ||
          typeof input.title === "string" ||
          Array.isArray(input.labels) ||
          typeof input.priority === "number" ||
          input.priority === null ||
          Array.isArray(input.external_links) ||
          typeof input.route === "string" ||
          typeof input.parent === "string" ||
          (typeof input.components === "object" && input.components !== null)
        );
      },
      message:
        "Issue update requires at least one field besides issue: status, title, labels, priority, external_links, route, parent, or components.",
    },
    strict: true,
  },
  {
    name: "dispatch_claim",
    example: { issue: "DSP-1" },
    description:
      "Claim a Dispatch issue before you start implementing it, so no other session takes the same work, " +
      "and release it when you stop. Pass the issue alone to claim it, or release: true to give it up. " +
      "Your claim records your own session and shows on every read of the issue: the dashboard header, the " +
      "issue list and board, dispatch_read, and dispatch_issues. Claiming is refused with 409 ISSUE_CLAIMED " +
      "when another session holds the issue and is still running; the refusal names that session, so talk to " +
      "it instead of working the same issue in parallel. A claim whose session is no longer running may be " +
      "taken: the takeover is recorded on the issue and the session that lost it is told. A claim is not the " +
      "issue's status — claiming moves nothing, so also move the issue to in_progress with " +
      "dispatch_issue_update when you start. Only the holder or a human releases a claim. " +
      `${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      release: z
        .boolean()
        .describe("Give up your claim instead of taking it; the issue's status does not change.")
        .optional(),
    }),
    strict: true,
  },
  {
    name: "dispatch_ask",
    example: { issue: "DSP-1", question: "Ship this?" },
    description:
      "Open a durable, answerable decision on an issue or project document. Do not use it for a status update or discussion; " +
      "use dispatch_message instead. A to-do a human must complete is a question phrased as that to-do, with the options you want (for example Done / Can't). " +
      "Anchor a document question, thread reply_to/reply_to_ask, or cite a dispatch:// " +
      `reference — it must be answerable from its own text and anchor alone, never "see above". A quote anchor is pinned to its block. Question is at most ${ASK_QUESTION_MAX} ` +
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
      question: z
        .string({ max: ASK_QUESTION_MAX })
        .describe(`Decision question, at most ${ASK_QUESTION_MAX} characters.`),
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
    example: {
      ask: "01234567-0000-4000-8000-000000000001",
      question: "Ship the revised plan?",
    },
    description:
      "Edit an open question in place. Use it to correct or refine the same decision; retract the " +
      "old ask and open a new one when the decision itself changes. Previous text remains in the " +
      "event log. Only the asking session can edit it; answered or resolved asks cannot be edited.",
    arguments: (z) => ({
      ask: z
        .string()
        .describe(
          "Ask id (uuid); an 8+ hex prefix unique among this session's own open asks works too."
        ),
      question: z
        .string({ max: ASK_QUESTION_MAX })
        .describe(`Replacement decision question, at most ${ASK_QUESTION_MAX} characters.`)
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
    example: {
      ask: "01234567-0000-4000-8000-000000000001",
      kind: "retracted",
      reason: "A newer question supersedes this one.",
    },
    description:
      "Retract an open question that is moot or resolve one after finding the answer. This closes the question without answering it.",
    arguments: (z) => ({
      ask: z
        .string()
        .describe(
          "Ask id (uuid) to close; an 8+ hex prefix unique among this session's own open asks " +
            "works too."
        ),
      kind: z
        .enum(["retracted", "resolved"])
        .describe("Whether the ask is retracted or self-resolved."),
      reason: z.string({ min: 1 }).describe("Why the open ask no longer needs a human answer."),
    }),
  },
  {
    name: "dispatch_resolve_comment",
    example: { comment: "01234567-0000-4000-8000-000000000001" },
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
    example: { ask: "01234567-0000-4000-8000-000000000001", action: "follow" },
    description:
      "Follow or unfollow an ask. Every session that opens or replies to an ask follows it: its answer, " +
      "edits, resolution, and replies reach that session directly. Unfollow to stop; follow to rejoin or " +
      "to hear an ask you never wrote to. Whole-issue subscription is separate: envoy_subscribe " +
      "notifications.dispatch.issue.<KEY>.>",
    arguments: (z) => ({
      ask: z
        .string()
        .describe(
          "Ask id (uuid); an 8+ hex prefix unique among this session's own open asks works too."
        ),
      action: z.enum(["follow", "unfollow"]).describe("follow | unfollow"),
    }),
    strict: true,
  },
  {
    name: "dispatch_comment",
    example: { issue: "DSP-1", body: "Looks good." },
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
    example: {
      issue: "DSP-1",
      artifact: "spec",
      quote: "old wording",
      replace_with: "new wording",
    },
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
    example: { issue: "DSP-1", body: "Implementation started." },
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
    example: {
      issue: "DSP-1",
      artifact: "spec",
      ops: [{ op: "delete_column", block: "table-123", index: 1 }],
      precondition: { blocks: [{ id: "table-123", token: "sha256:current-table-token" }] },
    },
    description:
      "Apply deterministic document edits: replace or delete quoted text, insert markdown at an anchor, retype an identified paragraph or typed block into a schema-declared typed block, delete or move a whole block by its id, or delete a table row or column in place. " +
      "Do not use it for review feedback or for reading; use dispatch_comment, dispatch_suggest, or dispatch_doc_read instead. " +
      "For replace, delete, and quote anchors, find text as rendered: inline Markdown (**bold**, `code`) is tolerated; a leading '# ' matches a heading. replace is inline: with is the new text of the matched span, so a leading list or heading marker stays literal text. " +
      "A delete whose find is a block's entire text removes the block (a list emptied of its items goes too); delete with block removes any block by id, and move with block relocates one. delete_row and delete_column take a table block and a zero-based index, preserving the table block id and refusing to remove cells with open asks or unresolved comments. " +
      'Insert and move anchors also accept "start", "end", "heading:<exact heading text>", and "block:<id>"; block ids and their tokens come from GET /api/v1/artifacts/{artifact UUID}/blocks (the route takes the artifact UUID, not its slug). ' +
      "Optionally require the state just read: precondition selects exactly one of a document token from dispatch_doc_read, or block {id, token} values from /blocks. A block guard must include every block the batch changes; Dispatch resolves quote targets and rejects an uncovered batch rather than applying it. Use a document token for insert or move, which depend on document order. Prefer block tokens when the covered content blocks are independent sections. Tokens include inline marks, so a fresh human comment also makes a stale edit fail. PRECONDITION_FAILED means re-read; EDIT_QUEUE_FULL means back off before retrying. " +
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
                "Block id for retype, delete, move, delete_row, or delete_column: the #id of a typed block, or an id from GET /api/v1/artifacts/{id}/blocks."
              )
              .optional(),
            index: z
              .number({ int: true, min: 0 })
              .describe("Zero-based row or column index for delete_row or delete_column.")
              .optional(),
            type: z.string().describe("Typed block name for retype.").optional(),
            attributes: z.unknown().describe("Typed block attributes for retype.").optional(),
          })
        )
        .describe("Flat tagged edits; the server validates fields required for each operation."),
      precondition: z
        .object({
          document: z
            .string({ min: 1 })
            .describe("Token for the exact canonical document returned by dispatch_doc_read.")
            .optional(),
          blocks: z
            .array(
              z.object({
                id: z
                  .string({ min: 1 })
                  .describe("Stable block id from GET /api/v1/artifacts/{id}/blocks."),
                token: z
                  .string({ min: 1 })
                  .describe("That block's full-state token, including inline marks."),
              }),
              { min: 1 }
            )
            .describe(
              "Every content block this batch changes, each with the token returned by /blocks."
            )
            .optional(),
        })
        .describe(
          "Optional optimistic-concurrency guard; select exactly one of document or blocks."
        )
        .optional(),
      summary: z.string().describe("Optional named-version summary.").optional(),
    }),
    validation: documentEditValidation,
  },
  {
    name: "dispatch_doc_read",
    example: { issue: "DSP-1" },
    description:
      "Read a live document or a named document version. Do not use it for issue status, asks, or events; " +
      "use dispatch_read instead. Supply ref, issue, or project plus artifact; issue plus an omitted artifact reads the primary document. " +
      "A live read returns its document token for an optional dispatch_doc_edit precondition; use /blocks for per-block tokens. " +
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
    example: { issue: "DSP-1" },
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
    example: { issue: "DSP-1", name: "design.md", content: "# Design\n" },
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
    example: { issue: "DSP-1" },
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
    example: { query: "astrolabe" },
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
    name: "dispatch_issues",
    example: { project: "AGENTC" },
    description:
      "List a project's issues for a roadmap or backlog pass: every issue in one project, each carrying " +
      "its status, priority, parent, labels, and open-ask count, so you can see backlog shape without " +
      "opening every issue. Optionally filter by status, parent, label, or how recently it changed. Do " +
      "not use it to search by keyword or phrase; dispatch_search remains the keyword surface. Rows are " +
      "capped at limit (default 50, max 250), applied to the response here, not by the server.",
    arguments: (z) => ({
      project: z.string().describe("Project key to list issues from."),
      status: z.enum(ISSUE_STATUSES).describe("Optional lifecycle status filter.").optional(),
      parent: z.string().describe("Optional parent issue key filter.").optional(),
      label: z.string().describe("Optional label filter.").optional(),
      updated_since: z
        .string()
        .describe("Optional RFC3339 timestamp; only issues updated at or after it.")
        .optional(),
      limit: z
        .number({ int: true, min: 1, max: 250 })
        .describe("Maximum rows, 1-250; default 50.")
        .optional(),
    }),
  },
  {
    name: "dispatch_architecture_sync",
    example: { project: "CORE" },
    description:
      "Import a project's architecture model from its configured source repository now, instead of " +
      "waiting for the server's five-minute schedule. Returns the imported commit and component " +
      "count, or the recorded error when the model was rejected (the previous model stays up). " +
      "The source itself is configured by a human in Settings; 404 SOURCE_NOT_FOUND without one.",
    arguments: (z) => ({
      project: z.string().describe("Project key whose architecture source to sync, such as CORE."),
    }),
    strict: true,
  },
  {
    name: "dispatch_open_asks",
    example: {},
    description:
      "List active unanswered asks, oldest first, with age and whose reply is needed. Omit project to " +
      "see only this session's own authored asks (call before saying you are waiting for human input); " +
      "supply project to see every open ask across that project's issues and documents, whoever authored " +
      "them.",
    arguments: (z) => ({
      project: z
        .string()
        .describe(
          "Project key; when supplied, lists every open ask in the project instead of only this session's own."
        )
        .optional(),
    }),
    strict: true,
  },
  {
    name: "dispatch_whoami",
    example: {},
    description:
      "Who Dispatch takes this session for: {session, owner}. owner is the lowercase GitHub login of the human whose personal token you run under (the default assignee of issues you create), or null under the shared token.",
    arguments: () => ({}),
    strict: true,
  },
] as const satisfies readonly DispatchToolSpec[];

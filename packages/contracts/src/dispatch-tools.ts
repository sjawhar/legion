import type { SchemaApi, SchemaNode, ToolArgumentsShape } from "./tool-schema";

export interface DispatchToolSpec {
  readonly name: string;
  readonly description: string;
  readonly arguments: <E extends SchemaNode<E>>(z: SchemaApi<E>) => ToolArgumentsShape;
  readonly validation?: {
    readonly check: (value: unknown) => boolean;
    readonly message: string;
  };
}

/** Builds a host-owned schema and applies any tool-level cross-field validation. */
export function dispatchToolSchema<E extends SchemaNode<E>>(
  spec: DispatchToolSpec,
  z: SchemaApi<E>,
  opts?: { readonly strict?: boolean }
): E {
  const shape = spec.arguments(z) as Record<string, E>;
  return spec.validation === undefined
    ? z.object(shape, opts)
    : z.refineObject(shape, spec.validation.check, spec.validation.message, opts);
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

/** Document edit operations the Dispatch server applies. */
export const DOC_EDIT_OPS = ["replace", "delete", "insert", "retype"] as const;

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
    }),
  },
  {
    name: "dispatch_ask",
    description:
      "Open a durable, answerable decision or human to-do on an issue or project document. Do not use it for a status update or discussion; " +
      "use dispatch_message instead. Use kind: action for a to-do a human must complete; it has fixed Done / Can't answers. " +
      "Anchor a document question, thread reply_to/reply_to_ask, or cite a dispatch:// " +
      `reference — it must be answerable from its own text and anchor alone, never "see above". Question is at most 800 ` +
      `characters and has at most 8 options. ${OWNER_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      project: z.string().describe("Project key owning the document.").optional(),
      artifact: z.string().describe("Project document artifact id, slug, or filename.").optional(),
      ref: z.string().describe("Optional dispatch:// issue or document reference.").optional(),
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
        .describe("Optional choices, at most 8.")
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
    name: "dispatch_comment",
    description:
      "Add review feedback to an issue or project document quote, or reply to a question asked with dispatch_ask. " +
      "Do not use it for an exact replacement; use " +
      `dispatch_suggest instead. Body is at most 2,000 characters. ${OWNER_REFERENCE}`,
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
          "Full id of a comment to reply to; replying to any comment in a thread continues that " +
            "thread (an ask's clarification thread included)."
        )
        .optional(),
      reply_to_ask: z
        .string()
        .describe(
          "Optional ask id to reply to, threading this comment under that question. Mutually " +
            "exclusive with reply_to."
        )
        .optional(),
    }),
    validation: documentOwnerValidation(true),
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
      "Apply deterministic document edits, including retyping an identified paragraph into a schema-declared typed block. " +
      "Do not use it for review feedback or for reading; use dispatch_comment, dispatch_suggest, or dispatch_doc_read instead. " +
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
            find: z.string().describe("Text to find for replace or delete.").optional(),
            with: z.string().describe("Replacement text for replace.").optional(),
            occurrence: z
              .number({ int: true, min: 0 })
              .describe("Optional zero-based match occurrence.")
              .optional(),
            markdown: z.string().describe("Markdown to insert.").optional(),
            after: z.string().describe("Anchor after which to insert.").optional(),
            before: z.string().describe("Anchor before which to insert.").optional(),
            block: z.string().describe("Block id to retype.").optional(),
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
] as const satisfies readonly DispatchToolSpec[];

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

/** Ask urgency levels the Dispatch server accepts, in ascending order. */
export const ASK_URGENCIES = ["low", "med", "high", "blocking"] as const;

/** Document edit operations the Dispatch server applies. */
export const DOC_EDIT_OPS = ["replace", "delete", "insert"] as const;

export const dispatchToolSpecs = [
  {
    name: "dispatch_issue",
    description:
      "Create a native Dispatch issue for newly tracked work. Do not use it when an existing issue " +
      `already covers the work; read or update that issue instead. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      project: z.string().describe("Project key for the new issue."),
      title: z.string().describe("Concise issue title."),
      parent: z.string().describe("Optional parent issue.").optional(),
      external: z.string().describe("Optional external issue reference.").optional(),
      spec: z.string().describe("Optional initial primary-document markdown.").optional(),
    }),
  },
  {
    name: "dispatch_ask",
    description:
      "Open a durable, answerable decision on an issue. Do not use it for a status update or discussion; " +
      `use dispatch_message instead. Question is at most 800 characters and has at most 8 options. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      question: z.string({ max: 800 }).describe("Decision question, at most 800 characters."),
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
      "Add review feedback to an issue or document quote, or reply to a question asked with dispatch_ask. " +
      "Do not use it for an exact replacement; use " +
      `dispatch_suggest instead. Body is at most 2,000 characters. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      artifact: z.string().describe("Artifact slug or id required when quote is given.").optional(),
      quote: z.string().describe("Optional exact quoted document text.").optional(),
      occurrence: z
        .number({ int: true, min: 0 })
        .describe("Optional zero-based occurrence of quote.")
        .optional(),
      body: z.string({ max: 2000 }).describe("Review comment, at most 2,000 characters."),
      reply_to: z.string().describe("Optional comment id to reply to.").optional(),
      reply_to_ask: z
        .string()
        .describe(
          "Optional ask id to reply to, threading this comment under that question. Mutually " +
            "exclusive with reply_to."
        )
        .optional(),
    }),
  },
  {
    name: "dispatch_suggest",
    description:
      "Propose an exact replacement for quoted document text. Do not use it for general feedback; use " +
      `dispatch_comment instead. Optional explanation is at most 2,000 characters. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      artifact: z.string().describe("Artifact slug or id containing the quoted text."),
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
  },
  {
    name: "dispatch_message",
    description:
      "Post a plain issue update. Do not use it for a decision or line-specific review; use dispatch_ask " +
      `or dispatch_comment instead. Body is at most 2,000 characters. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      body: z.string({ max: 2000 }).describe("Update text, at most 2,000 characters."),
    }),
  },
  {
    name: "dispatch_doc_edit",
    description:
      "Apply deterministic text edits to a document. Do not use it for review feedback or for reading; use " +
      `dispatch_comment, dispatch_suggest, or dispatch_doc_read instead. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      artifact: z.string().describe("Artifact slug or id for the document."),
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
          })
        )
        .describe("Flat tagged edits; the server validates fields required for each operation."),
      summary: z.string().describe("Optional named-version summary.").optional(),
    }),
  },
  {
    name: "dispatch_doc_read",
    description:
      "Read a live document or a named document version. Do not use it for issue status, asks, or events; " +
      "use dispatch_read instead. Supply ref or issue; issue plus an omitted artifact reads the primary document. " +
      `${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      artifact: z
        .string()
        .describe("Optional artifact slug or id; primary document by default.")
        .optional(),
      version: z.number({ int: true, min: 1 }).describe("Optional version number.").optional(),
      ref: z.string().describe("Optional dispatch:// document reference.").optional(),
    }),
  },
  {
    name: "dispatch_artifact",
    description:
      "Attach a local file or inline text as an issue artifact. Do not use it to edit a live document; use " +
      `dispatch_doc_edit instead. Exactly one of path or content is required; artifacts are limited to 25 MiB. ${ISSUE_REFERENCE}`,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE),
      name: z.string().describe("Artifact filename shown in Dispatch."),
      path: z.string().describe("Local path to the file to upload.").optional(),
      content: z.string().describe("Inline text to store as a Markdown document.").optional(),
      primary: z.boolean().describe("Make this document the issue primary artifact.").optional(),
      summary: z.string().describe("Optional version summary.").optional(),
    }),
    validation: {
      check: (value) => {
        const input = value as { readonly path?: unknown; readonly content?: unknown };
        return (typeof input.path === "string") !== (typeof input.content === "string");
      },
      message: "Exactly one of path or content is required.",
    },
  },
  {
    name: "dispatch_read",
    description:
      "Read an issue summary, targeted ask, or targeted comment reply chain. Do not use it for document " +
      "contents; use dispatch_doc_read instead. Supply issue or ref. " +
      ISSUE_REFERENCE,
    arguments: (z) => ({
      issue: z.string().describe(ISSUE_REFERENCE).optional(),
      ref: z.string().describe("Optional dispatch:// issue reference.").optional(),
    }),
  },
] as const satisfies readonly DispatchToolSpec[];

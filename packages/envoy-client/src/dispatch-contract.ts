// The `dispatch` tool as every host plugin exposes it: the strings the model
// reads, the two call shapes, and the validator each plugin runs before it
// touches the network. Hosts build their own schema object from
// DISPATCH_ARGUMENTS with their own builder (OMP's pi.zod, OpenCode's
// tool.schema, Claude's zod) because tool schemas must be a single flat
// object at the top level; the open/continue rule is enforced here.

import { z } from "zod";

export const DISPATCH_TOOL_NAME = "dispatch";
export const DISPATCH_CONTEXT_MAX = 1200;
export const DISPATCH_QUESTION_MAX = 800;
export const DISPATCH_URGENCIES = ["low", "med", "high", "blocking"] as const;
export type DispatchUrgency = (typeof DISPATCH_URGENCIES)[number];

export const DISPATCH_TOOL_DESCRIPTION =
  "Raise a durable question to the human as a Dispatch thread (a GitHub issue shown on the dashboard), or continue an existing thread with a follow-up question. The reader has NOT seen your transcript. Open a thread with `subject`; continue one with `thread`. The reply arrives in this session as a steer.";

export const DISPATCH_ARGUMENTS = {
  subject:
    "Open a new thread: one line naming the decision needed (the issue title). Omit when continuing a thread with `thread`.",
  thread:
    'Continue an existing thread: "<n>" (an issue in the working directory\'s repo) or "owner/name#<n>". When set, omit subject, urgency, repo, and parent.',
  context: `What you are doing, what you found, why you are stuck — at most ${DISPATCH_CONTEXT_MAX} characters, at most three short paragraphs or a bullet list. The reader has NOT seen your transcript: no nouns you coined this session, no internal identifiers unless the question is about them. GitHub references (#N, owner/repo#N, URLs) may be bare; the dashboard unfurls them.`,
  question: `The ask, at most ${DISPATCH_QUESTION_MAX} characters, as a list: current state → desired state → your recommendation and why; options go in \`ask\`.`,
  ask: "Structured questions rendered as buttons on the dashboard. Each: { question, header?, options: [{ label, description? }], multiple?, custom? }. Use this whenever the answer is one of N choices.",
  urgency: "low | med | high | blocking (default med). Opening a thread only.",
  repo: "owner/name. Opening a thread only; defaults to the working directory's GitHub repo.",
  parent:
    "<n> | owner/name#<n>[#<commentId>]. Opening a thread only: link the thread as a sub-issue of an existing issue and append a breadcrumb to the comment.",
} as const;

export interface DispatchQuestionOption {
  readonly label: string;
  readonly description?: string;
}

export interface DispatchQuestion {
  readonly question: string;
  readonly header?: string;
  readonly options?: readonly DispatchQuestionOption[];
  readonly multiple?: boolean;
  readonly custom?: boolean;
}

export interface OpenThreadCall {
  readonly subject: string;
  readonly context: string;
  readonly question: string;
  readonly ask?: readonly DispatchQuestion[];
  readonly urgency?: DispatchUrgency;
  readonly repo?: string;
  readonly parent?: string;
}

export interface ContinueThreadCall {
  readonly thread: string;
  readonly context: string;
  readonly question: string;
  readonly ask?: readonly DispatchQuestion[];
}

export type DispatchCall = OpenThreadCall | ContinueThreadCall;

export class DispatchArgumentError extends Error {
  override readonly name = "DispatchArgumentError";
}

const QuestionOptionSchema = z.strictObject({
  label: z.string().min(1),
  description: z.string().optional(),
});

export const DispatchQuestionSchema = z.strictObject({
  question: z.string().min(1),
  header: z.string().optional(),
  options: z.array(QuestionOptionSchema).optional(),
  multiple: z.boolean().optional(),
  custom: z.boolean().optional(),
});

const prose = {
  context: z.string(),
  question: z.string(),
  ask: z.array(DispatchQuestionSchema).optional(),
};

const OpenThreadCallSchema = z.strictObject({
  subject: z.string(),
  ...prose,
  urgency: z.enum(DISPATCH_URGENCIES).optional(),
  repo: z.string().optional(),
  parent: z.string().optional(),
});

const ContinueThreadCallSchema = z.strictObject({ thread: z.string(), ...prose });

/**
 * The flat, LLM-facing shape for hosts whose builder is zod v4 (the Claude
 * bridge). Descriptions come from DISPATCH_ARGUMENTS so every host shows the
 * model the same words.
 */
export const dispatchToolShape = {
  subject: z.string().describe(DISPATCH_ARGUMENTS.subject).optional(),
  thread: z.string().describe(DISPATCH_ARGUMENTS.thread).optional(),
  context: z.string().describe(DISPATCH_ARGUMENTS.context),
  question: z.string().describe(DISPATCH_ARGUMENTS.question),
  ask: z.array(DispatchQuestionSchema).describe(DISPATCH_ARGUMENTS.ask).optional(),
  urgency: z.enum(DISPATCH_URGENCIES).describe(DISPATCH_ARGUMENTS.urgency).optional(),
  repo: z.string().describe(DISPATCH_ARGUMENTS.repo).optional(),
  parent: z.string().describe(DISPATCH_ARGUMENTS.parent).optional(),
} satisfies z.ZodRawShape;

export function isContinueCall(call: DispatchCall): call is ContinueThreadCall {
  return "thread" in call;
}

/** Drop keys whose value is undefined: hosts pass optional args as undefined, the schema treats them as absent. */
function present(raw: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(raw).filter(([, value]) => value !== undefined));
}

function describeIssues(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "(root)"}: ${issue.message}`)
    .join("; ");
}

/**
 * Validate the model's arguments into one of the two call shapes. Throws
 * DispatchArgumentError with a message the model can act on; nothing here
 * touches the network.
 */
export function parseDispatchCall(raw: unknown): DispatchCall {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw new DispatchArgumentError("dispatch: invalid arguments — expected an object");
  }
  const args = present(raw as Record<string, unknown>);
  const hasSubject = "subject" in args;
  const hasThread = "thread" in args;
  if (hasSubject && hasThread) {
    throw new DispatchArgumentError(
      "dispatch: pass either subject (open a thread) or thread (continue one), not both"
    );
  }
  if (!hasSubject && !hasThread) {
    throw new DispatchArgumentError("dispatch: subject or thread is required");
  }
  if (hasThread && ("urgency" in args || "repo" in args || "parent" in args)) {
    throw new DispatchArgumentError(
      "dispatch: thread cannot be combined with urgency, repo, or parent"
    );
  }
  const parsed = hasThread
    ? ContinueThreadCallSchema.safeParse(args)
    : OpenThreadCallSchema.safeParse(args);
  if (!parsed.success) {
    throw new DispatchArgumentError(
      `dispatch: invalid arguments — ${describeIssues(parsed.error)}`
    );
  }
  return parsed.data;
}

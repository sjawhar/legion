import type { QuestionAnswer, QuestionInfo } from "@opencode-ai/sdk/v2";
import { parse as yamlParse, stringify as yamlStringify } from "yaml";
import type { Origin, OriginHost, Urgency } from "./types";

export type { QuestionAnswer, QuestionInfo };

/**
 * One selectable option as a marker carries it. The Go marshaller drops an
 * empty `description`, so it is optional here where the SDK requires it.
 */
export type MarkerQuestionOption = Omit<QuestionInfo["options"][number], "description"> & {
  description?: string;
};

/**
 * One question as a marker carries it. `header` and `options` are `omitempty`
 * on the Go side, so neither is guaranteed; every SDK `QuestionInfo` is still
 * a valid `MarkerQuestion`.
 */
export type MarkerQuestion = Omit<QuestionInfo, "header" | "options"> & {
  header?: string;
  options?: MarkerQuestionOption[];
};

export interface ParsedMetaMarker {
  urgency: Urgency;
  ask?: MarkerQuestion[];
  requestId: string;
  origin?: Origin;
}

export interface ParsedAnswerMarker {
  forThread: number;
  answers: QuestionAnswer[];
}

const ORIGIN_HOSTS = ["omp", "claude"] as const satisfies readonly OriginHost[];

function isOriginHost(value: unknown): value is OriginHost {
  return ORIGIN_HOSTS.some((host) => host === value);
}

// Reads only the known origin fields, dropping unrecognized keys, non-string
// values, and a host outside the union envoy-client can emit. A malformed or
// absent origin block yields undefined rather than making the whole marker
// unparseable.
function parseOrigin(raw: unknown): Origin | undefined {
  if (typeof raw !== "object" || raw === null) return undefined;
  const origin: Origin = {};
  if ("host" in raw && isOriginHost(raw.host)) origin.host = raw.host;
  if ("machine" in raw && typeof raw.machine === "string" && raw.machine) {
    origin.machine = raw.machine;
  }
  if ("cwd" in raw && typeof raw.cwd === "string" && raw.cwd) origin.cwd = raw.cwd;
  if ("tmux" in raw && typeof raw.tmux === "string" && raw.tmux) origin.tmux = raw.tmux;
  if ("pane" in raw && typeof raw.pane === "string" && raw.pane) origin.pane = raw.pane;
  return Object.keys(origin).length > 0 ? origin : undefined;
}

const URGENCIES = ["low", "med", "high", "blocking"] as const satisfies readonly Urgency[];

export function isUrgency(value: unknown): value is Urgency {
  return URGENCIES.some((urgency) => urgency === value);
}

function isMarkerQuestionOption(value: unknown): value is MarkerQuestionOption {
  if (typeof value !== "object" || value === null) return false;
  if (!("label" in value) || typeof value.label !== "string") return false;
  if (!("description" in value)) return true;
  return value.description === undefined || typeof value.description === "string";
}

function isMarkerQuestion(value: unknown): value is MarkerQuestion {
  if (typeof value !== "object" || value === null) return false;
  if (!("question" in value) || typeof value.question !== "string") return false;
  if ("header" in value && value.header !== undefined && typeof value.header !== "string") {
    return false;
  }
  if (!("options" in value) || value.options === undefined) return true;
  return Array.isArray(value.options) && value.options.every(isMarkerQuestionOption);
}

/**
 * A marker lives in the issue body, so any repo collaborator can write one. An
 * `ask` block that isn't a well-formed question list is dropped rather than
 * handed to the renderer, which would otherwise iterate over a string.
 */
function parseAsk(raw: unknown): MarkerQuestion[] | undefined {
  if (!Array.isArray(raw) || raw.length === 0) return undefined;
  const questions: unknown[] = raw;
  return questions.every(isMarkerQuestion) ? questions : undefined;
}

/**
 * Read the leading YAML frontmatter block. Every field stays `unknown`: the
 * body is user-editable, so each one is narrowed by its caller before use.
 */
function parseFrontmatter(body: string): Record<string, unknown> | null {
  if (!body.startsWith("---\n")) return null;
  const after = body.slice(4);
  const close = after.indexOf("\n---");
  if (close < 0) return null;
  let data: unknown;
  try {
    data = yamlParse(after.slice(0, close));
  } catch {
    return null;
  }
  if (typeof data !== "object" || data === null || Array.isArray(data)) return null;
  return data as Record<string, unknown>;
}

export function parseMetaMarker(body: string): ParsedMetaMarker | null {
  const data = parseFrontmatter(body);
  if (!data) return null;
  const { urgency, requestId } = data;
  if (!isUrgency(urgency) || typeof requestId !== "string" || !requestId) return null;
  const parsed: ParsedMetaMarker = { urgency, requestId };
  const ask = parseAsk(data.ask);
  if (ask) parsed.ask = ask;
  const origin = parseOrigin(data.origin);
  if (origin) parsed.origin = origin;
  return parsed;
}

export function parseUrgencyMarker(commentBody: string): Urgency | null {
  const data = parseFrontmatter(commentBody);
  if (data?.kind !== "urgency") return null;
  return isUrgency(data.urgency) ? data.urgency : null;
}

/**
 * Compute the effective urgency for a thread by folding the body's dispatch:meta marker
 * with any dispatch:urgency=X marker comments in chronological order. Latest marker wins,
 * per spec §5.4. Returns the body's urgency if no comment markers exist.
 */
export function effectiveUrgency(bodyUrgency: Urgency, comments: { body: string }[]): Urgency {
  let urgency = bodyUrgency;
  for (const comment of comments) {
    const marker = parseUrgencyMarker(comment.body);
    if (marker) urgency = marker;
  }
  return urgency;
}

export function parseAnswerMarker(commentBody: string): ParsedAnswerMarker | null {
  const data = parseFrontmatter(commentBody);
  if (data?.kind !== "answer") return null;
  const { forThread, answers } = data;
  if (typeof forThread !== "number" || !Array.isArray(answers)) return null;
  // Each answer is the list of values chosen for one question; a marker whose
  // answers aren't string arrays would crash the renderer that maps over them.
  const values: unknown[] = answers;
  if (!values.every(isQuestionAnswer)) return null;
  return { forThread, answers: values };
}

function isQuestionAnswer(value: unknown): value is QuestionAnswer {
  return Array.isArray(value) && value.every((entry) => typeof entry === "string");
}

export function buildAnswerMarkerComment(
  threadNumber: number,
  answers: QuestionAnswer[],
  summary: string
): string {
  const yaml = yamlStringify({ kind: "answer", forThread: threadNumber, answers });
  return `---\n${yaml}---\n\n${summary}`;
}

export function buildUrgencyMarkerComment(urgency: Urgency): string {
  const yaml = yamlStringify({ kind: "urgency", urgency });
  return `---\n${yaml}---\n`;
}

export function stripMetaMarker(body: string): string {
  if (!body.startsWith("---\n")) return body;
  const after = body.slice(4);
  const close = after.indexOf("\n---");
  if (close < 0) return body;
  return after.slice(close + 4).replace(/^\n+/, "");
}

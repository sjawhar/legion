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
 * One question as a marker carries it. `askId` is written by the service for
 * every turn it posts; a legacy thread marker has none and the reader
 * synthesises one (see asks.ts). `header` and `options` are `omitempty` on
 * the Go side, so neither is guaranteed.
 */
export type MarkerQuestion = Omit<QuestionInfo, "header" | "options"> & {
  askId?: string;
  header?: string;
  options?: MarkerQuestionOption[];
};

export interface ParsedThreadMarker {
  requestId: string;
  urgency: Urgency;
  ask?: MarkerQuestion[];
  origin?: Origin;
}

export interface ParsedAskMarker {
  requestId: string;
  ask: MarkerQuestion[];
  origin?: Origin;
}

export interface ParsedAnswerMarker {
  forThread: number;
  /** The `askId` this answer settles; null for a legacy answer, which settles every ask in the body. */
  forAsk: string | null;
  answers: QuestionAnswer[];
}

export type MarkerKind = "thread" | "ask" | "answer" | "urgency";

const MARKER_KINDS = [
  "thread",
  "ask",
  "answer",
  "urgency",
] as const satisfies readonly MarkerKind[];
const MARKER_OPEN = "<!-- dispatch:";
const MARKER_CLOSE = "-->";

function isMarkerKind(value: unknown): value is MarkerKind {
  return MARKER_KINDS.some((kind) => kind === value);
}

const ORIGIN_HOSTS = ["omp", "opencode", "claude"] as const satisfies readonly OriginHost[];

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
  for (const key of ["machine", "cwd", "tmux", "pane", "sessionId", "sessionTitle"] as const) {
    if (key in raw) {
      const value = (raw as Record<string, unknown>)[key];
      if (typeof value === "string" && value) origin[key] = value;
    }
  }
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
  if ("askId" in value && value.askId !== undefined && typeof value.askId !== "string") {
    return false;
  }
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

function parseYamlObject(text: string): Record<string, unknown> | null {
  let data: unknown;
  try {
    data = yamlParse(text);
  } catch {
    return null;
  }
  if (typeof data !== "object" || data === null || Array.isArray(data)) return null;
  return data as Record<string, unknown>;
}

interface RawMarker {
  kind: MarkerKind;
  data: Record<string, unknown>;
  /** Index into the body just past the marker's closing delimiter. */
  end: number;
}

/**
 * Read the marker at the very start of a body, in either encoding: the HTML
 * comment `<!-- dispatch:<kind>\n<yaml>\n-->` or legacy front matter
 * `---\n<yaml>\n---` (a `kind` key names the kind; absent means thread). Every
 * field stays `unknown`: the body is user-editable, so each one is narrowed by
 * its caller before use.
 */
function readMarker(body: string): RawMarker | null {
  if (body.startsWith(MARKER_OPEN)) {
    const newline = body.indexOf("\n", MARKER_OPEN.length);
    if (newline < 0) return null;
    const kind = body.slice(MARKER_OPEN.length, newline).trim();
    if (!isMarkerKind(kind)) return null;
    const close = body.indexOf(`\n${MARKER_CLOSE}`, newline);
    if (close < 0) return null;
    const data = parseYamlObject(body.slice(newline + 1, close));
    if (!data) return null;
    return { kind, data, end: close + 1 + MARKER_CLOSE.length };
  }
  if (body.startsWith("---\n")) {
    const close = body.indexOf("\n---", 4);
    if (close < 0) return null;
    const data = parseYamlObject(body.slice(4, close));
    if (!data) return null;
    const kind = data.kind === undefined ? "thread" : data.kind;
    if (!isMarkerKind(kind)) return null;
    return { kind, data, end: close + 4 };
  }
  return null;
}

export function parseThreadMarker(body: string): ParsedThreadMarker | null {
  const marker = readMarker(body);
  if (marker?.kind !== "thread") return null;
  const { urgency, requestId } = marker.data;
  if (!isUrgency(urgency) || typeof requestId !== "string" || !requestId) return null;
  const parsed: ParsedThreadMarker = { urgency, requestId };
  const ask = parseAsk(marker.data.ask);
  if (ask) parsed.ask = ask;
  const origin = parseOrigin(marker.data.origin);
  if (origin) parsed.origin = origin;
  return parsed;
}

export function parseAskMarker(body: string): ParsedAskMarker | null {
  const marker = readMarker(body);
  if (marker?.kind !== "ask") return null;
  const { requestId } = marker.data;
  if (typeof requestId !== "string" || !requestId) return null;
  const parsed: ParsedAskMarker = { requestId, ask: parseAsk(marker.data.ask) ?? [] };
  const origin = parseOrigin(marker.data.origin);
  if (origin) parsed.origin = origin;
  return parsed;
}

export function parseUrgencyMarker(commentBody: string): Urgency | null {
  const marker = readMarker(commentBody);
  if (marker?.kind !== "urgency") return null;
  return isUrgency(marker.data.urgency) ? marker.data.urgency : null;
}

/**
 * Compute the effective urgency for a thread by folding the body's thread
 * marker with any urgency marker comments in chronological order. Latest
 * marker wins. Returns the body's urgency if no comment markers exist.
 */
export function effectiveUrgency(bodyUrgency: Urgency, comments: { body: string }[]): Urgency {
  let urgency = bodyUrgency;
  for (const comment of comments) {
    const marker = parseUrgencyMarker(comment.body);
    if (marker) urgency = marker;
  }
  return urgency;
}

function isQuestionAnswer(value: unknown): value is QuestionAnswer {
  return Array.isArray(value) && value.every((entry) => typeof entry === "string");
}

export function parseAnswerMarker(commentBody: string): ParsedAnswerMarker | null {
  const marker = readMarker(commentBody);
  if (marker?.kind !== "answer") return null;
  const { forThread, forAsk, answers } = marker.data;
  if (typeof forThread !== "number" || !Array.isArray(answers)) return null;
  // Each answer is the list of values chosen for one question; a marker whose
  // answers aren't string arrays would crash the renderer that maps over them.
  const values: unknown[] = answers;
  if (!values.every(isQuestionAnswer)) return null;
  return {
    forThread,
    forAsk: typeof forAsk === "string" && forAsk ? forAsk : null,
    answers: values,
  };
}

/**
 * YAML that can sit inside an HTML comment. The comment ends at the first
 * `-->`, so every string is emitted double-quoted (where `\u` escapes are
 * legal) and the three delimiter sequences are escaped; they decode back to
 * the original characters. `lineWidth: 0` keeps each scalar on one line.
 */
function commentSafeYaml(value: unknown): string {
  return yamlStringify(value, {
    defaultStringType: "QUOTE_DOUBLE",
    defaultKeyType: "PLAIN",
    lineWidth: 0,
  })
    .replaceAll("-->", "--\\u003e")
    .replaceAll("<!--", "\\u003c!--")
    .replaceAll("--!>", "--!\\u003e");
}

function buildMarker(kind: MarkerKind, payload: Record<string, unknown>): string {
  return `${MARKER_OPEN}${kind}\n${commentSafeYaml(payload)}${MARKER_CLOSE}`;
}

/** The answer comment: marker naming the ask, then the human-readable summary — the only part a GitHub reader sees. */
export function buildAnswerMarkerComment(
  threadNumber: number,
  askId: string,
  answers: QuestionAnswer[],
  summary: string
): string {
  return `${buildMarker("answer", { forThread: threadNumber, forAsk: askId, answers })}\n\n${summary}`;
}

export function buildUrgencyMarkerComment(urgency: Urgency): string {
  return `${buildMarker("urgency", { urgency })}\n\nUrgency set to **${urgency}**.`;
}

/** The body without its leading marker, in either encoding. */
export function stripMarker(body: string): string {
  const marker = readMarker(body);
  if (!marker) return body;
  return body.slice(marker.end).replace(/^\n+/, "");
}

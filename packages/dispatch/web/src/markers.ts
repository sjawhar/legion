import type { QuestionAnswer, QuestionInfo } from "@opencode-ai/sdk/v2";
import { parse as yamlParse, stringify as yamlStringify } from "yaml";
import type { Urgency } from "./types";

export type { QuestionAnswer, QuestionInfo };

export interface ParsedMetaMarker {
  urgency: Urgency;
  ask?: QuestionInfo[];
  requestId: string;
}

export interface ParsedAnswerMarker {
  forThread: number;
  answers: QuestionAnswer[];
}

const URGENCIES = new Set(["low", "med", "high", "blocking"]);

export function parseMetaMarker(body: string): ParsedMetaMarker | null {
  if (!body.startsWith("---\n")) return null;
  const after = body.slice(4);
  const close = after.indexOf("\n---");
  if (close < 0) return null;
  try {
    const data = yamlParse(after.slice(0, close)) as {
      urgency?: string;
      requestId?: string;
      ask?: QuestionInfo[];
    } | null;
    if (!data?.urgency || !data.requestId) return null;
    if (!URGENCIES.has(data.urgency)) return null;
    const parsed: ParsedMetaMarker = {
      urgency: data.urgency as Urgency,
      requestId: data.requestId,
    };
    if (data.ask) parsed.ask = data.ask;
    return parsed;
  } catch {
    return null;
  }
}

export function parseUrgencyMarker(commentBody: string): Urgency | null {
  if (!commentBody.startsWith("---\n")) return null;
  const after = commentBody.slice(4);
  const close = after.indexOf("\n---");
  if (close < 0) return null;
  try {
    const data = yamlParse(after.slice(0, close)) as { kind?: string; urgency?: string } | null;
    if (data?.kind !== "urgency") return null;
    if (!data.urgency || !URGENCIES.has(data.urgency)) return null;
    return data.urgency as Urgency;
  } catch {
    return null;
  }
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
  if (!commentBody.startsWith("---\n")) return null;
  const after = commentBody.slice(4);
  const close = after.indexOf("\n---");
  if (close < 0) return null;
  try {
    const data = yamlParse(after.slice(0, close)) as {
      kind?: string;
      forThread?: number;
      answers?: QuestionAnswer[];
    } | null;
    if (data?.kind !== "answer") return null;
    if (typeof data.forThread !== "number" || !Array.isArray(data.answers)) return null;
    return { forThread: data.forThread, answers: data.answers };
  } catch {
    return null;
  }
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

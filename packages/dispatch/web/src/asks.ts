// The ask/answer model of a thread: which questions were asked on which turn,
// which answers settle them, and which are still open. Pure functions over the
// issue body and its comments; the renderer and the sidebar both consume this.

import {
  type MarkerQuestion,
  parseAnswerMarker,
  parseAskMarker,
  parseThreadMarker,
  type QuestionAnswer,
} from "./markers";
import type { Comment } from "./types";

export interface ThreadAsk {
  readonly askId: string;
  readonly question: MarkerQuestion;
  /** Position within its turn (a legacy answer maps by this index). */
  readonly index: number;
  readonly source:
    | { readonly kind: "body" }
    | { readonly kind: "comment"; readonly commentId: number };
}

export interface ThreadAnswer {
  /** The ask this answer settles; null for a legacy answer, which settles every body ask by index. */
  readonly forAsk: string | null;
  readonly answers: QuestionAnswer[];
  readonly commentId: number;
  readonly authorLogin: string;
  readonly createdAt: string;
}

export interface ResolvedAnswer {
  readonly values: QuestionAnswer;
  readonly answer: ThreadAnswer;
}

/** The service's rule: the first ask of a turn reuses the turn's request id; later ones append ".<index>". */
export function askIdFor(requestId: string, index: number): string {
  return index === 0 ? requestId : `${requestId}.${index}`;
}

export function collectAsks(body: string, comments: readonly Comment[]): ThreadAsk[] {
  const asks: ThreadAsk[] = [];
  const thread = parseThreadMarker(body);
  if (thread) {
    for (const [index, question] of (thread.ask ?? []).entries()) {
      asks.push({
        askId: question.askId ?? askIdFor(thread.requestId, index),
        question,
        index,
        source: { kind: "body" },
      });
    }
  }
  for (const comment of comments) {
    const marker = parseAskMarker(comment.body);
    if (!marker) continue;
    for (const [index, question] of marker.ask.entries()) {
      asks.push({
        askId: question.askId ?? askIdFor(marker.requestId, index),
        question,
        index,
        source: { kind: "comment", commentId: comment.id },
      });
    }
  }
  return asks;
}

export function collectAnswers(comments: readonly Comment[]): ThreadAnswer[] {
  const answers: ThreadAnswer[] = [];
  for (const comment of comments) {
    const marker = parseAnswerMarker(comment.body);
    if (!marker) continue;
    answers.push({
      forAsk: marker.forAsk,
      answers: marker.answers,
      commentId: comment.id,
      authorLogin: comment.authorLogin,
      createdAt: comment.createdAt,
    });
  }
  return answers;
}

export function answerFor(ask: ThreadAsk, answers: readonly ThreadAnswer[]): ResolvedAnswer | null {
  const named = answers.find((answer) => answer.forAsk === ask.askId);
  if (named) return { values: named.answers[0] ?? [], answer: named };
  if (ask.source.kind !== "body") return null;
  const legacy = answers.find((answer) => answer.forAsk === null);
  if (!legacy) return null;
  return { values: legacy.answers[ask.index] ?? [], answer: legacy };
}

export function openAsks(
  asks: readonly ThreadAsk[],
  answers: readonly ThreadAnswer[]
): ThreadAsk[] {
  return asks.filter((ask) => answerFor(ask, answers) === null);
}

/** The asks an answer settles; empty when it names an ask that is not on the thread. */
export function answerTargets(answer: ThreadAnswer, asks: readonly ThreadAsk[]): ThreadAsk[] {
  if (answer.forAsk !== null) return asks.filter((ask) => ask.askId === answer.forAsk);
  return asks.filter((ask) => ask.source.kind === "body");
}

// The ask/answer model of a thread: which questions were asked on which turn,
// which answers settle them, and which are still open. Pure functions over the
// issue body and its comments; the renderer and the sidebar both consume this.
//
// A thread is a sequence of turns: the opening body, then every `dispatch:ask`
// follow-up comment. A follow-up supersedes the thread's earlier unanswered
// questions — the asker is restating what it needs — so only the latest turn's
// unanswered asks are open. A follow-up with no structured `ask` list is one
// free-text question, taken from its `## Question` section.

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
  /** Synthesised from a follow-up's prose because the turn carried no `ask` list; answered free-text. */
  readonly prose?: true;
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

/**
 * The question a follow-up asks in prose: the text of its `## Question`
 * section (the service writes `## Context` then `## Question`), or the whole
 * comment minus the marker when the section is missing.
 */
export function proseQuestion(commentBody: string): string {
  const withoutMarker = commentBody.replace(/^\s*<!--[\s\S]*?-->\s*/, "");
  const section = /^##\s+Question\s*\n([\s\S]*)$/m.exec(withoutMarker);
  return (section?.[1] ?? withoutMarker).trim();
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
    const source = { kind: "comment", commentId: comment.id } as const;
    if (marker.ask.length === 0) {
      asks.push({
        askId: askIdFor(marker.requestId, 0),
        question: { question: proseQuestion(comment.body) },
        index: 0,
        source,
        prose: true,
      });
      continue;
    }
    for (const [index, question] of marker.ask.entries()) {
      asks.push({
        askId: question.askId ?? askIdFor(marker.requestId, index),
        question,
        index,
        source,
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

function sameTurn(left: ThreadAsk["source"], right: ThreadAsk["source"]): boolean {
  return left.kind === "body"
    ? right.kind === "body"
    : right.kind === "comment" && right.commentId === left.commentId;
}

/**
 * The asks still waiting for an answer: the unanswered asks of the latest
 * turn. Earlier unanswered asks were superseded by that turn (see
 * `supersededAsks`).
 */
export function openAsks(
  asks: readonly ThreadAsk[],
  answers: readonly ThreadAnswer[]
): ThreadAsk[] {
  const latest = asks.at(-1);
  if (!latest) return [];
  return asks.filter(
    (ask) => sameTurn(ask.source, latest.source) && answerFor(ask, answers) === null
  );
}

/** Unanswered asks of earlier turns: a later follow-up restated the question, so they take no answer. */
export function supersededAsks(
  asks: readonly ThreadAsk[],
  answers: readonly ThreadAnswer[]
): ThreadAsk[] {
  const latest = asks.at(-1);
  if (!latest) return [];
  return asks.filter(
    (ask) => !sameTurn(ask.source, latest.source) && answerFor(ask, answers) === null
  );
}

/** The asks an answer settles; empty when it names an ask that is not on the thread. */
export function answerTargets(answer: ThreadAnswer, asks: readonly ThreadAsk[]): ThreadAsk[] {
  if (answer.forAsk !== null) return asks.filter((ask) => ask.askId === answer.forAsk);
  return asks.filter((ask) => ask.source.kind === "body");
}

export interface ThreadAsks {
  readonly asks: ThreadAsk[];
  readonly answers: ThreadAnswer[];
  /** The asks no answer settles. */
  readonly open: ThreadAsk[];
}

/** The ask model of one thread from its body and comments: every ask, every answer, and what is still open. */
export function threadAsks(body: string, comments: readonly Comment[]): ThreadAsks {
  const asks = collectAsks(body, comments);
  const answers = collectAnswers(comments);
  return { asks, answers, open: openAsks(asks, answers) };
}

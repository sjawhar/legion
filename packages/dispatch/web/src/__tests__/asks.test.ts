import { describe, expect, it } from "bun:test";
import {
  answerFor,
  answerTargets,
  askIdFor,
  collectAnswers,
  collectAsks,
  openAsks,
  proseQuestion,
  supersededAsks,
  type ThreadAnswer,
  type ThreadAsk,
} from "../asks";
import { buildAnswerMarkerComment } from "../markers";
import type { Comment } from "../types";

const now = "2026-09-05T12:00:00Z";

function comment(id: number, body: string, authorLogin = "sami"): Comment {
  return { id, body, createdAt: now, updatedAt: now, authorLogin };
}

const newBody =
  "<!-- dispatch:thread\nrequestId: R\nurgency: med\nask:\n    - askId: R\n      question: Color?\n    - askId: R.1\n      question: Size?\n-->\n\nBody";
const legacyBody =
  "---\nurgency: med\nrequestId: L\nask:\n  - question: Color?\n  - question: Size?\n---\n\nBody";
const followUp = comment(
  30,
  "<!-- dispatch:ask\nrequestId: F\nask:\n    - askId: F\n      question: Which lane?\n-->\n\n## Context\n\nc",
  "agent"
);

describe("askIdFor", () => {
  it("reuses the request id for the first ask and suffixes later ones", () => {
    expect(askIdFor("R", 0)).toBe("R");
    expect(askIdFor("R", 2)).toBe("R.2");
  });
});

describe("collectAsks", () => {
  it("collects body asks and follow-up asks with their explicit ids", () => {
    const asks = collectAsks(newBody, [comment(1, "plain reply"), followUp]);
    expect(asks.map((ask) => [ask.askId, ask.source.kind, ask.index])).toEqual([
      ["R", "body", 0],
      ["R.1", "body", 1],
      ["F", "comment", 0],
    ]);
    expect(asks[2]?.source).toEqual({ kind: "comment", commentId: 30 });
  });

  it("synthesises ids for a legacy thread from its requestId", () => {
    expect(collectAsks(legacyBody, []).map((ask) => ask.askId)).toEqual(["L", "L.1"]);
  });

  it("is empty for a thread without asks", () => {
    expect(collectAsks("<!-- dispatch:thread\nrequestId: R\nurgency: med\n-->", [])).toEqual([]);
  });
});

describe("answers", () => {
  it("maps a named answer to its ask; the follow-up supersedes the body's other open ask", () => {
    const answers = collectAnswers([
      comment(40, buildAnswerMarkerComment(12, "R.1", [["large"]], "s")),
    ]);
    const asks = collectAsks(newBody, [followUp]);
    expect(answerFor(asks[1] as ThreadAsk, answers)?.values).toEqual(["large"]);
    expect(answerFor(asks[0] as ThreadAsk, answers)).toBeNull();
    expect(openAsks(asks, answers).map((ask) => ask.askId)).toEqual(["F"]);
    expect(supersededAsks(asks, answers).map((ask) => ask.askId)).toEqual(["R"]);
    expect(answerTargets(answers[0] as ThreadAnswer, asks).map((ask) => ask.askId)).toEqual([
      "R.1",
    ]);
  });

  it("maps a legacy answer to every body ask by index and never to a follow-up", () => {
    const legacy = comment(
      41,
      "---\nkind: answer\nforThread: 12\nanswers:\n  - [blue]\n  - [small]\n---\n\nsummary"
    );
    const answers = collectAnswers([legacy]);
    const asks = collectAsks(legacyBody, [followUp]);
    expect(answerFor(asks[0] as ThreadAsk, answers)?.values).toEqual(["blue"]);
    expect(answerFor(asks[1] as ThreadAsk, answers)?.values).toEqual(["small"]);
    expect(answerFor(asks[2] as ThreadAsk, answers)).toBeNull();
    expect(openAsks(asks, answers).map((ask) => ask.askId)).toEqual(["F"]);
    expect(answerTargets(answers[0] as ThreadAnswer, asks).map((ask) => ask.askId)).toEqual([
      "L",
      "L.1",
    ]);
  });

  it("an answer naming an unknown ask targets nothing", () => {
    const answers = collectAnswers([
      comment(42, buildAnswerMarkerComment(12, "nope", [["x"]], "s")),
    ]);
    expect(answerTargets(answers[0] as ThreadAnswer, collectAsks(newBody, []))).toEqual([]);
  });
});

const proseFollowUp = (id: number, requestId: string, question: string): Comment =>
  comment(
    id,
    `<!-- dispatch:ask\nrequestId: ${requestId}\norigin:\n    host: omp\n-->\n\n## Context\n\nMore numbers.\n\n## Question\n\n${question}`,
    "agent"
  );

describe("prose follow-ups", () => {
  it("reads the question from the ## Question section, or the whole comment without it", () => {
    expect(proseQuestion(proseFollowUp(1, "P", "Cordon -231 - yes/no?").body)).toBe(
      "Cordon -231 - yes/no?"
    );
    expect(proseQuestion("<!-- dispatch:ask\nrequestId: P\n-->\n\nJust asking.")).toBe(
      "Just asking."
    );
  });

  it("becomes one free-text ask keyed by the follow-up's request id", () => {
    const asks = collectAsks(newBody, [proseFollowUp(50, "P", "Cordon -231 - yes/no?")]);
    expect(asks.at(-1)).toEqual({
      askId: "P",
      question: { question: "Cordon -231 - yes/no?" },
      index: 0,
      source: { kind: "comment", commentId: 50 },
      prose: true,
    });
  });

  it("many unanswered follow-ups leave exactly one open ask: the latest", () => {
    const comments = [1, 2, 3, 4, 5].map((n) => proseFollowUp(100 + n, `P${n}`, `Round ${n}?`));
    const asks = collectAsks(newBody, comments);
    expect(openAsks(asks, []).map((ask) => ask.askId)).toEqual(["P5"]);
    expect(supersededAsks(asks, []).map((ask) => ask.askId)).toEqual([
      "R",
      "R.1",
      "P1",
      "P2",
      "P3",
      "P4",
    ]);
  });

  it("an answer to the latest prose ask settles it; a later answer to a superseded ask still resolves", () => {
    const comments = [proseFollowUp(50, "P", "Cordon?")];
    const asks = collectAsks(newBody, comments);
    const answers = collectAnswers([
      comment(60, buildAnswerMarkerComment(12, "P", [["yes"]], "s")),
      comment(61, buildAnswerMarkerComment(12, "R", [["A"]], "s")),
    ]);
    expect(openAsks(asks, answers)).toEqual([]);
    expect(answerFor(asks[0] as ThreadAsk, answers)?.values).toEqual(["A"]);
  });
});

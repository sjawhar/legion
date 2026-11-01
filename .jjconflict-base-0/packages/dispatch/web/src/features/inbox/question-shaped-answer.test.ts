import { expect, test } from "bun:test";

import { isQuestionShapedAnswer } from "./question-shaped-answer";

test("detects punctuation- and interrogative-led clarification requests", () => {
  for (const answer of [
    "Maybe?",
    "   Who owns this release",
    "Can we ship after the audit",
    "Does the artifact already exist",
  ]) {
    expect(isQuestionShapedAnswer(answer), answer).toBe(true);
  }
});

test("does not treat declarative Other responses as clarification requests", () => {
  for (const answer of ["Ship after the audit.", "However, I would wait.", "This is approved."]) {
    expect(isQuestionShapedAnswer(answer), answer).toBe(false);
  }
});

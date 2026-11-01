import { expect, test } from "bun:test";

import { answerAskInput } from "./answer-ask";

test("builds an answer with its reviewed revision and optional trimmed text", () => {
  expect(
    answerAskInput({ edited_at: "2026-09-12T10:00:00Z" }, ["Ship"], "  Ready to release.  ")
  ).toEqual({
    expected_edited_at: "2026-09-12T10:00:00Z",
    selected: ["Ship"],
    text: "Ready to release.",
  });
  expect(answerAskInput({ edited_at: null }, [], "  ")).toEqual({
    expected_edited_at: null,
    selected: [],
  });
});

test("serializes answer fields in the established request order", () => {
  expect(
    JSON.stringify(
      answerAskInput({ edited_at: "2026-09-12T10:00:00Z" }, ["Ship"], "Ready to release.")
    )
  ).toBe(
    '{"selected":["Ship"],"text":"Ready to release.","expected_edited_at":"2026-09-12T10:00:00Z"}'
  );
});

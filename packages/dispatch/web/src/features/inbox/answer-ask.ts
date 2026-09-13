import type { AnswerAskInput, Ask } from "../../api/types";

export function answerAskInput(
  ask: Pick<Ask, "edited_at">,
  selected: string[],
  text: string
): AnswerAskInput {
  const trimmedText = text.trim();
  return {
    selected,
    ...(trimmedText === "" ? {} : { text: trimmedText }),
    expected_edited_at: ask.edited_at,
  };
}

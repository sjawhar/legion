import type { AnswerAskInput, Ask, AskAnswer } from "../../api/types";

export function answerAskInput(
  ask: Pick<Ask, "edited_at">,
  selected: string[],
  text: string,
  replacing?: AskAnswer
): AnswerAskInput {
  const trimmedText = text.trim();
  return {
    selected,
    ...(trimmedText === "" ? {} : { text: trimmedText }),
    expected_edited_at: ask.edited_at,
    ...(replacing === undefined ? {} : { expected_answer_at: replacing.at }),
  };
}

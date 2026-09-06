import { describe, expect, test } from "bun:test";
import { renderDispatchQuestionSchema } from "../scripts/dispatch-question-schema";
import { DispatchQuestionSchema } from "./dispatch-question";

const checkedInSchema = new URL("../schemas/dispatch-question.schema.json", import.meta.url);

describe("DispatchQuestionSchema", () => {
  test("checked-in JSON Schema exactly matches the Zod emission", async () => {
    expect(await Bun.file(checkedInSchema).text()).toBe(renderDispatchQuestionSchema());
  });

  test("accepts a question without optional fields", () => {
    expect(DispatchQuestionSchema.parse({ question: "Proceed?" })).toEqual({
      question: "Proceed?",
    });
  });

  test("requires each option to include a label", () => {
    expect(
      DispatchQuestionSchema.safeParse({
        question: "Proceed?",
        options: [{ description: "Cannot be selected without a label." }],
      }).success
    ).toBe(false);
  });
});

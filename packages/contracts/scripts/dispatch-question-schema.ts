import { z } from "zod";
import { DispatchQuestionSchema } from "../src/dispatch-question";

/**
 * The checked-in `schemas/dispatch-question.schema.json`, byte for byte. The
 * zod schema is the source of truth; `gen-go.ts` writes this file and then
 * generates Go from it, and the contracts test asserts the checked-in copy has
 * not drifted from the zod.
 */
export function renderDispatchQuestionSchema(): string {
  return `${JSON.stringify(z.toJSONSchema(DispatchQuestionSchema), null, 2)}\n`;
}

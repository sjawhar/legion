import { z } from "zod";

export const DispatchQuestionOptionSchema = z.strictObject({
  label: z.string().min(1),
  description: z.string().optional(),
});

export const DispatchQuestionSchema = z.strictObject({
  askId: z.string().optional(),
  question: z.string().min(1),
  header: z.string().optional(),
  options: z.array(DispatchQuestionOptionSchema).optional(),
  multiple: z.boolean().optional(),
  custom: z.boolean().optional(),
});

export const DispatchQuestionInputSchema = DispatchQuestionSchema.omit({ askId: true });

export type DispatchQuestionOption = z.infer<typeof DispatchQuestionOptionSchema>;
export type DispatchQuestion = z.infer<typeof DispatchQuestionSchema>;
export type DispatchQuestionInput = z.infer<typeof DispatchQuestionInputSchema>;

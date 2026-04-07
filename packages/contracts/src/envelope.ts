import { z } from "zod";

const isSubject = (value: unknown): value is string =>
  typeof value === "string" && value.length > 0;

export const EnvelopeSchema = z.object({
  event_id: z.string().min(1),
  source: z.enum(["agent", "github", "slack", "whatsapp", "ghostwispr"]),
  source_event_id: z.string().min(1),
  source_session: z.string().optional(),
  topic: z.custom<string>(isSubject, { message: "topic must be a non-empty subject" }),
  dedupe_key: z.string().min(1),
  issued_at: z.number().int(),
  expires_at: z.number().int().optional(),
  payload_summary: z.string().min(1),
  payload_ref: z.string().optional(),
  trace_id: z.string().min(1),
});

export type Envelope = z.infer<typeof EnvelopeSchema>;

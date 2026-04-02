import { z } from "zod";

export const EnvelopeSchema = z.object({
  event_id: z.string().min(1),
  source: z.enum(["agent", "github", "slack", "whatsapp"]),
  source_event_id: z.string().min(1),
  topic: z.string().min(1),
  dedupe_key: z.string().min(1),
  issued_at: z.number().int(),
  expires_at: z.number().int().optional(),
  payload_summary: z.string().min(1),
  payload_ref: z.string().optional(),
  trace_id: z.string().min(1),
});

export type Envelope = z.infer<typeof EnvelopeSchema>;

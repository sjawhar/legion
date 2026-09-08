import { z } from "zod";

const isSubject = (value: unknown): value is string =>
  typeof value === "string" && value.length > 0;

export const EnvelopeSchema = z.object({
  event_id: z.string().min(1),
  source: z.enum(["agent", "human", "envoy", "github", "slack", "whatsapp", "ghostwispr"]),
  source_event_id: z.string().min(1),
  source_session: z.string().optional(),
  topic: z.custom<string>(isSubject, { message: "topic must be a non-empty subject" }),
  dedupe_key: z.string().min(1),
  issued_at: z.number().int(),
  expires_at: z.number().int().optional(),
  payload_summary: z.string().min(1),
  payload: z.string().optional(),
  payload_ref: z.string().optional(),
  trace_id: z.string().min(1),
  sender: z
    .object({
      session_id: z.string().min(1),
      machine: z.string().optional(),
      cwd: z.string().optional(),
      title: z.string().optional(),
      roles: z.array(z.string()).optional(),
    })
    .optional(),
  in_reply_to: z.string().min(1).optional(),
  supersedes: z.string().min(1).optional(),
  urgency: z.enum(["low", "med", "high", "blocking"]).optional(),
  expects_reply: z.enum(["none", "optional", "required"]).optional(),
});

export type Envelope = z.infer<typeof EnvelopeSchema>;

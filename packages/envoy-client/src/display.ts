import { EnvelopeSchema } from "@legion/contracts";

export type EnvelopeDisplay = {
  readonly eventID: string;
  readonly source: string;
  readonly sourceSessionID: string | undefined;
  readonly topic: string;
  readonly summary: string;
  readonly dedupeKey: string;
  readonly issuedAt: number;
  readonly traceID: string;
};

export function toEnvelopeDisplay(value: unknown): EnvelopeDisplay {
  const envelope = EnvelopeSchema.parse(value);
  return {
    eventID: envelope.event_id,
    source: envelope.source,
    sourceSessionID: envelope.source_session,
    topic: envelope.topic,
    summary: envelope.payload_summary,
    dedupeKey: envelope.dedupe_key,
    issuedAt: envelope.issued_at,
    traceID: envelope.trace_id,
  };
}

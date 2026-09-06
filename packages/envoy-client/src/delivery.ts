import type { Envelope } from "@legion/contracts";

export type DeliveryEnvelope = Pick<Envelope, "source" | "source_session" | "payload">;

export function senderLabel(envelope: DeliveryEnvelope): string {
  return envelope.source_session ?? envelope.source;
}

export function replyWith(envelope: DeliveryEnvelope): string | undefined {
  if (envelope.source !== "agent" || envelope.source_session === undefined) return undefined;
  return `envoy_send(session_id="${envelope.source_session}", message="...")`;
}

export function isOwnDispatchEcho(envelope: DeliveryEnvelope, sessionID: string): boolean {
  if (envelope.source !== "github" || envelope.payload === undefined) return false;

  try {
    const payload: unknown = JSON.parse(envelope.payload);
    return (
      typeof payload === "object" &&
      payload !== null &&
      "dispatch_session" in payload &&
      payload.dispatch_session === sessionID
    );
  } catch {
    return false;
  }
}

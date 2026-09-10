import type { IssueKey } from "@legion/contracts";
import { ISSUE_KEY_PATTERN } from "./legion-state";

/** A decoded Dispatch Event from the issue durable consumer. `key` is the validated event and
 * reducer identity; issue-event payload keys are checked against it before this value is made. */
export interface DispatchIssueEvent {
  type: string;
  key: IssueKey;
  seq: number;
  notify: boolean;
  payload: unknown;
  eventId: string;
}

/** Thrown when a Dispatch durable message's inner Event cannot be decoded without risking a
 * reducer mutating a different issue from the subject/event identity. */
export class DispatchDecodeFailure extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DispatchDecodeFailure";
  }
}

interface DispatchEnvelope {
  payload?: unknown;
}

type JsonRecord = Record<string, unknown>;

function asRecord(value: unknown): JsonRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as JsonRecord)
    : undefined;
}

function recordPayload(envelope: DispatchEnvelope): JsonRecord | undefined {
  if (typeof envelope.payload !== "string") return asRecord(envelope.payload);
  try {
    return asRecord(JSON.parse(envelope.payload));
  } catch {
    return undefined;
  }
}

/** Decodes the common Dispatch Event envelope. `issue.*` event payloads must identify the same
 * Dispatch issue as the event header; a parent relationship, when set, must be a Dispatch key.
 * All nonempty event type strings are valid because Dispatch evolves additively. */
export function dispatchIssueEvent(envelope: DispatchEnvelope): DispatchIssueEvent {
  const event = recordPayload(envelope);
  const id = event?.id;
  const key = event?.issue_key;
  const seq = event?.seq;
  const notify = event?.notify;
  const type = event?.type;
  const payload = event?.payload;
  const payloadRecord = asRecord(payload);
  if (
    typeof id !== "number" ||
    !Number.isSafeInteger(id) ||
    id <= 0 ||
    typeof key !== "string" ||
    !ISSUE_KEY_PATTERN.test(key) ||
    typeof seq !== "number" ||
    !Number.isSafeInteger(seq) ||
    seq <= 0 ||
    typeof notify !== "boolean" ||
    typeof type !== "string" ||
    type.length === 0 ||
    !payloadRecord
  ) {
    throw new DispatchDecodeFailure(
      "Dispatch durable message payload is not a valid Dispatch event"
    );
  }
  if (type === "issue.created" || type === "issue.updated" || type === "issue.closed") {
    const parent = payloadRecord.parent;
    if (typeof payloadRecord.key !== "string" || payloadRecord.key !== key) {
      throw new DispatchDecodeFailure(
        "Dispatch issue event payload key disagrees with event issue_key"
      );
    }
    if (
      parent !== undefined &&
      parent !== null &&
      (typeof parent !== "string" || !ISSUE_KEY_PATTERN.test(parent))
    ) {
      throw new DispatchDecodeFailure("Dispatch issue event payload has an invalid parent key");
    }
  }
  return { type, key: key as IssueKey, seq, notify, payload, eventId: `dispatch-${id}` };
}

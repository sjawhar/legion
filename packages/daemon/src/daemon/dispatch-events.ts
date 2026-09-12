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

/** A positive safe integer: a Dispatch version number or event seq. */
function positiveInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

/** Decodes the common Dispatch Event envelope. `issue.*` event payloads must identify the same
 * Dispatch issue as the event header; a parent relationship, when set, must be a Dispatch key.
 * The three artifact events the design gate consumes (`artifact.approved`,
 * `artifact.changes_requested`, `artifact.version`) must carry the fields their reducers read —
 * a non-empty `artifact_id`, a positive integer version (`version` for the two reviews, and
 * `version.number` for a new version), and a non-empty `reason` for changes requested — so a
 * malformed one is poison here (logged and terminated by the durable consumer) rather than a
 * reducer throw, which would be fatal. `actor` is not checked: the wake it feeds is best-effort.
 * All other nonempty event type strings are valid because Dispatch evolves additively. */
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
  if (type === "artifact.approved" || type === "artifact.changes_requested") {
    if (typeof payloadRecord.artifact_id !== "string" || payloadRecord.artifact_id.length === 0) {
      throw new DispatchDecodeFailure(`Dispatch ${type} payload has no artifact_id`);
    }
    if (!positiveInteger(payloadRecord.version)) {
      throw new DispatchDecodeFailure(`Dispatch ${type} payload has no positive integer version`);
    }
    if (
      type === "artifact.changes_requested" &&
      (typeof payloadRecord.reason !== "string" || payloadRecord.reason.length === 0)
    ) {
      throw new DispatchDecodeFailure("Dispatch artifact.changes_requested payload has no reason");
    }
  }
  if (type === "artifact.version") {
    if (typeof payloadRecord.artifact_id !== "string" || payloadRecord.artifact_id.length === 0) {
      throw new DispatchDecodeFailure("Dispatch artifact.version payload has no artifact_id");
    }
    const version = asRecord(payloadRecord.version);
    if (!version || !positiveInteger(version.number)) {
      throw new DispatchDecodeFailure(
        "Dispatch artifact.version payload has no positive integer version.number"
      );
    }
  }
  return { type, key: key as IssueKey, seq, notify, payload, eventId: `dispatch-${id}` };
}

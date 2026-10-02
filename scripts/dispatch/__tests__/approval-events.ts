import type {
  Actor,
  Ask,
  AskOption,
  DispatchEvent,
} from "../../../packages/contracts/src/dispatch-api";
import fixtureJson from "../__fixtures__/ask-census.json";
import type { CensusAsk, CensusEvent } from "../ask-census.ts";

/** The recorded asks and events of the three approval rounds LEGION-470 measured, from before
 * F1, each event reduced to the fields the census reads. They are Dispatch's JSON, whose literal
 * fields a JSON import widens to `string`. */
export const recordedRounds = fixtureJson as unknown as {
  readonly issues: ReadonlyArray<{
    readonly key: string;
    readonly asks: ReadonlyArray<Pick<CensusAsk, "id" | "created_at" | "kind" | "block_id">>;
    readonly events: readonly CensusEvent[];
  }>;
};

/** The events an approval round is read from, as contracts' `DispatchEvent` types them, so the
 * type checker holds every test event to the shape Dispatch records. */
export type ApprovalHistoryEvent = Extract<
  DispatchEvent,
  {
    readonly type:
      | "ask.opened"
      | "ask.edited"
      | "ask.handed_back"
      | "ask.answered"
      | "ask.follower_added"
      | "comment.created"
      | "artifact.approved";
  }
>;

export const ISSUE = "TEST-1";
export const ARTIFACT = "artifact-1";
export const ASK = "approval-1";

const agent: Actor = { kind: "session", id: "session-a" };
const human: Actor = { kind: "user", id: "alice" };
const SUMMARY = "Proposes a nightly export.";
const OPTIONS: AskOption[] = [
  { label: "Approve", description: "Approve this version of the document." },
  { label: "Request changes", description: "Say what must change before it can be approved." },
];

/** When the event numbered `seq` was recorded: one minute after 10:00 per number. */
export function recordedAt(seq: number): string {
  return new Date(Date.UTC(2026, 9, 1, 10, seq)).toISOString();
}

function question(version: number, summary: string): string {
  return `Approve spec.md (version ${version})? ${summary}`;
}

/** The approval ask an `ask.*` event carries after F1: `version` is the document version it
 * names, `requestedVersion` the version last handed to the human. */
export function approvalAsk(version: number, requestedVersion: number, summary = SUMMARY): Ask {
  return {
    id: ASK,
    issue_key: ISSUE,
    artifact_id: null,
    block_id: null,
    author: agent,
    kind: "approval",
    question: question(version, summary),
    options: OPTIONS,
    multiple: false,
    urgency: "high",
    anchor: null,
    state: "open",
    answer: null,
    opened_event_id: 1,
    created_at: recordedAt(1),
    edited_at: null,
    approval: {
      artifact_id: ARTIFACT,
      name: "spec.md",
      version,
      requested_version: requestedVersion,
    },
  };
}

function recorded(seq: number, actor: Actor) {
  return {
    id: seq,
    issue_key: ISSUE,
    project: "TEST",
    seq,
    actor,
    notify: false,
    created_at: recordedAt(seq),
  };
}

/** The agent's first request, at `version`. */
export function opened(seq: number, version: number): ApprovalHistoryEvent {
  return {
    ...recorded(seq, agent),
    type: "ask.opened",
    payload: {
      ...approvalAsk(version, version),
      opened_event_id: seq,
      created_at: recordedAt(seq),
    },
  };
}

/** A rewording: a new document version moves the request to `version` (left waiting on the
 * agent while `requestedVersion` is below it), or a request with a new `summary` rewrites it. */
export function edited(
  seq: number,
  version: number,
  requestedVersion: number,
  from: { readonly version: number; readonly summary?: string },
  summary = SUMMARY
): ApprovalHistoryEvent {
  return {
    ...recorded(seq, agent),
    type: "ask.edited",
    payload: {
      ...approvalAsk(version, requestedVersion, summary),
      edited_at: recordedAt(seq),
      previous: {
        question: question(from.version, from.summary ?? SUMMARY),
        options: OPTIONS,
        multiple: false,
        urgency: "high",
      },
      edited_by: agent,
    },
  };
}

/** The agent hands the request back to the human at `version`. */
export function handedBack(seq: number, version: number, summary = SUMMARY): ApprovalHistoryEvent {
  return {
    ...recorded(seq, agent),
    type: "ask.handed_back",
    payload: approvalAsk(version, version, summary),
  };
}

/** A reply in the request's thread: a human's hands the turn to the agent; a session's, by
 * default, to the human. */
export function reply(seq: number, author: "human" | "agent"): ApprovalHistoryEvent {
  const actor = author === "human" ? human : agent;
  const turn = author === "human" ? "agent" : "human";
  return {
    ...recorded(seq, actor),
    type: "comment.created",
    payload: {
      id: `comment-${seq}`,
      issue_key: ISSUE,
      artifact_id: null,
      author: actor,
      body: author === "human" ? "Why nightly?" : "The export runs off-peak.",
      anchor: null,
      reply_to: null,
      ask_id: ASK,
      waiting_on: turn,
      turn,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      created_at: recordedAt(seq),
      mentions: [],
      deliveries: [],
      artifact_name: "",
      ask_question: question(1, SUMMARY),
      ask_state: "open",
      ask_waiting_on: turn,
    },
  };
}

/** A session starts following the request: activity on it that is no turn of anyone's. */
export function followed(seq: number): ApprovalHistoryEvent {
  return {
    ...recorded(seq, agent),
    type: "ask.follower_added",
    payload: { ask_id: ASK, session_id: agent.id, by: agent },
  };
}

/** The human approves `version` from the request: its `ask.answered` and `artifact.approved`. */
export function approved(seq: number, version: number): ApprovalHistoryEvent[] {
  const answered = { user: human.id, selected: ["Approve"], text: null, at: recordedAt(seq) };
  return [
    {
      ...recorded(seq, human),
      type: "ask.answered",
      payload: { ...approvalAsk(version, version), state: "answered", answer: answered },
    },
    {
      ...recorded(seq + 1, human),
      type: "artifact.approved",
      payload: {
        artifact_id: ARTIFACT,
        name: "spec.md",
        version,
        actor: human,
        reason: null,
        ask_id: ASK,
      },
    },
  ];
}

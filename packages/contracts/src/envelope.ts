import { z } from "zod";

const isSubject = (value: unknown): value is string =>
  typeof value === "string" && value.length > 0;

export const EnvelopeSchema = z.object({
  event_id: z.string().min(1),
  source: z.enum([
    "agent",
    "human",
    "envoy",
    "github",
    "slack",
    "whatsapp",
    "ghostwispr",
    "dispatch",
  ]),
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

/**
 * The shape of a dedupe key the code that sends a message mints once for it: the listener's own
 * `publish.<id>` and `agent.<session>.<id>` (`id.New`, 32 hex digits, `cmd/listener/api.go`), the
 * same two around the shared transport's idempotency key (`crypto.randomUUID`,
 * `@legion/envoy-client/transport`), and any of them behind the role arbiter's forward mark. Each
 * is repeated only by a re-send of its one message: the transport's retry of a send whose answer
 * was lost, the Legion daemon's copy of a role-lane notice (LEGION-108). `scripts/gen-go.ts` emits
 * it as `contracts.MintedDedupeKeyPattern`.
 */
export const MINTED_DEDUPE_KEY_PATTERN =
  "^(?:envoy\\.role\\.forward\\.)?(?:publish|agent\\.[^.]+)\\.(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$";

const mintedDedupeKey = new RegExp(MINTED_DEDUPE_KEY_PATTERN);

/**
 * Whether an envelope's dedupe key names its event, so a second envelope under it is that event
 * arriving again and may be dropped: every Dispatch key (the idempotency key Dispatch supplies),
 * a webhook key that is the source plus the upstream's delivery id (the normalizers' shape), and a
 * key minted once per message (`MINTED_DEDUPE_KEY_PATTERN`). Any other key is not a dedupe key at
 * all, because its producer can give two distinct events one: the MCP bridge's content hash
 * (LEGION-423), the Go daemon's outbox row id, which restarts with each new store (LEGION-426).
 * The stream decides its MsgId by the same rule (`contracts.DedupeKeyNamesTheUpstreamEvent`), and
 * every host that subscribes over core NATS decides its own dedupe by this one.
 */
export function dedupeKeyNamesItsEvent(envelope: {
  readonly source?: string | undefined;
  readonly source_event_id?: string | undefined;
  readonly dedupe_key?: string | undefined;
}): boolean {
  const key = envelope.dedupe_key;
  if (key === undefined) return false;
  if (envelope.source === "dispatch" || mintedDedupeKey.test(key)) return true;
  switch (envelope.source) {
    case "github":
    case "slack":
    case "ghostwispr":
      return (
        envelope.source_event_id !== undefined &&
        envelope.source_event_id !== "" &&
        key === `${envelope.source}.${envelope.source_event_id}`
      );
    default:
      return false;
  }
}

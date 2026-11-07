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
  /**
   * The Unix-millisecond time the listener kept this copy because it could not deliver it,
   * present only on a copy the listener forwards from the kept lane, so the holder it reaches can
   * judge how old the message is.
   */
  kept_at: z.number().int().optional(),
  /**
   * `mention_dedupe_keys` holds, per mentioned session id, the dedupe key that session's own
   * send of this comment went under - the key `agent.<session>.<comment>:<target>:<the mention's
   * mode>` its host claims when the mention's send lands - for every session of the comment
   * whose mention has settled: one the mention delivered to as well as one whose every attempt
   * then failed. A mention still in flight is left out, its key being one no host has claimed
   * yet: a copy carrying it would claim that key in the mention's place, and the mention's own
   * send would then be dropped at the host.
   *
   * Dispatch's outbox writes it, on a copy it keeps on a role's kept lane, and only there; both
   * forwarders of that lane read it - the handover behind the keep and the drain that hands the
   * lane to the role's next holder - and key the copy they hand a holder by that holder's own
   * entry. The holder a kept copy reaches is unknown when it is kept, and a host drops only a
   * dedupe key it has claimed itself, so one stored key shows the comment twice to every other
   * mentioned session that claims the role.
   */
  mention_dedupe_keys: z.record(z.string(), z.string()).optional(),
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

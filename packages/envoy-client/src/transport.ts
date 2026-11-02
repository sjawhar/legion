import {
  DELIVERY_DUPLICATE_WINDOW_MS,
  type Envelope,
  EnvelopeSchema,
  MINTED_DEDUPE_KEY_PATTERN,
} from "@legion/contracts";
import { z } from "zod";
import { normalizeEnvoyUrl } from "./envoy-settings";
import { machineID } from "./machine";

const DEFAULT_TIMEOUT_MS = 5_000;
const RETRY_DELAY_MS = 250;

/**
 * How long one replay call may take. The listener stops reading at 5 s and its HTTP server writes
 * for at most 10 s, so this waits out a full answer without outliving the server's own deadline.
 */
const REPLAY_TIMEOUT_MS = 10_000;

/**
 * The `code` a listener answers a replay with, under 503, when NATS refuses the read outright (the
 * listener's user lacks the grant, or there is no notification stream). Retrying at once cannot
 * help, unlike a 503 the listener's start-up gate answers, so a caller tells its session instead.
 */
export const REPLAY_UNAVAILABLE_CODE = "REPLAY_UNAVAILABLE";

/**
 * A session's state, which the listener computes from that session's last registration and from
 * nothing else (dispatch://INFRA-392): `live` within the live window (5 minutes), `unresponsive`
 * past it and inside the gone horizon (an hour), `gone` past that or with no record at all. An
 * unresponsive session is not dead — it keeps its role and issue claims, a message sent to it is
 * stored for it, and nothing starts a replacement for it.
 */
const SessionStateSchema = z.enum(["live", "unresponsive", "gone"]);

export type SessionState = z.infer<typeof SessionStateSchema>;

/**
 * What an agent must do about a session that stopped answering (dispatch://INFRA-392). Every
 * answer that names a session in that state says this, in one wording: the tool descriptions
 * (`tool-contract.ts`), the send confirmation, and the refusal an unresponsive holder's role
 * lookup or publish gets, which is read at the moment an agent decides whether its peer is
 * alive. The 20 minutes are counted from the holder's last registration (`last_seen`), the sum
 * the Dispatch server itself refuses another agent's take by
 * (`ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS`).
 */
export const UNRESPONSIVE_RULE =
  "An unresponsive session is not dead: it keeps its role claims and its issue claims, and a " +
  "message sent to it is stored until it answers. Start no replacement for it and take no claim " +
  "from it until it is gone, or until 20 minutes have passed since its last_seen, the time of " +
  "its last registration.";

// A listener predating the field names no state, and it lists only sessions it holds a
// registration for: `live` is what such an answer means, and reading an absent state as anything
// else would make every session on an older listener look dead.
const WireStateSchema = SessionStateSchema.default("live");

const InterestWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  dir: z.string(),
  topics: z.array(z.string()),
  updated_at: z.number().int().optional(),
  warnings: z.array(z.string()).optional(),
});

/**
 * How the listener delivers a session's stored subjects, answered to a registration that asked
 * for consumer delivery: `consumer` relays the session's durable consumer on `subject`;
 * `core` (`reason` is the listener's machine-readable why) keeps the core subscriptions and
 * `POST /v1/replay`. The reason is read as a string, so one a later listener adds still parses.
 */
const DeliveryAnswerWireSchema = z.discriminatedUnion("mode", [
  z.object({
    mode: z.literal("consumer"),
    subject: z.string().min(1),
    stream_id: z.string(),
    gap: z.boolean(),
  }),
  z.object({ mode: z.literal("core"), reason: z.string() }),
]);

const SessionWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  // The session's real machine, as the client reported it (machineID()); machine_id stays the
  // listener it last registered through. The session's owner is the listener its stored
  // subscription names (the one store.Registry.Match fans its topics out on), else the one on
  // machine_id; only the owner delivers to a serve-port session, at this host unless the session
  // runs on the owner's machine (registered through it, or this host is the owner's machine id)
  // and the owner has a host bridge, and any other listener leaves inbox messages
  // to it silently (session.go's ErrWrongMachine). Optional here only for a listener predating
  // the field — the live listener always fills it in, falling back to machine_id itself
  // (session.SessionEntry.HostOrMachine).
  host: z.string().optional(),
  dir: z.string(),
  port: z.number().int(),
  title: z.string(),
  topics: z.array(z.string()),
  roles: z.array(z.string()).optional(),
  capabilities: z.array(z.string()).optional(),
  self_subscribed: z.boolean(),
  last_seen: z.number().int().optional(),
  state: WireStateSchema,
  // The plugin the session registered through and its build; absent from a listener predating the
  // fields and for a session whose plugin predates them.
  plugin: z.string().optional(),
  plugin_version: z.string().optional(),
});

const RoleWireSchema = z.object({
  role: z.string(),
  holder: z.string(),
  host: z.string().optional(),
  last_seen: z.number().int(),
  // The holder's own state. A role answer names it `holder_state`; `state` is the same session's
  // state as a session row reports it, so both are read and `holder_state` wins.
  state: WireStateSchema,
  holder_state: SessionStateSchema.optional(),
});

const RoleHeldWireSchema = z.object({
  error: z.string(),
  role: z.string(),
  holder: z.string(),
});

/**
 * A listener's error body. Beyond the message and its `code`, a refusal that names a session
 * names that session's state and its last registration, and a role's 404 names its holder too:
 * a send's `no live session <id>` carries `state` and `last_seen`
 * (`cmd/listener/api.go`'s `noLiveSessionRefusal`), and a role lookup's or a role publish's
 * `holder lapsed` carries `holder`, `holder_state`, `last_seen` and `claim_released`
 * (`cmd/listener/role_holder.go`'s `roleHolderError`). The listener's own text names none of
 * them, so `EnvoyApiError` says them itself (`refusalStateText`): an agent told its peer did not
 * answer must be told whether that peer is unresponsive — still holding its role and its claims
 * — or gone.
 */
const ErrorWireSchema = z.object({
  error: z.string(),
  code: z.string().optional(),
  expected: z.array(z.string()).optional(),
  state: SessionStateSchema.optional(),
  holder: z.string().optional(),
  holder_state: SessionStateSchema.optional(),
  last_seen: z.number().int().optional(),
  /** Always false on a lapsed role's 404: a lookup releases no claim, whatever the state. */
  claim_released: z.boolean().optional(),
});

/**
 * What a send or a publish answers: the envelope, and the fields the listener adds beside it.
 * `duplicate` is a send's: the listener already stored a send under this target and idempotency
 * key, so this one stored nothing new. Absent reads as false, as an older listener's answer does.
 */
const EnvelopeResponseSchema = EnvelopeSchema.extend({
  recipient: z.string().optional(),
  holder: z.string().optional(),
  recipient_state: SessionStateSchema.optional(),
  // The recipient's last registration (epoch ms), which the listener sends with an unresponsive
  // recipient's state so the sender can say how long it has been stalled; omitted when it knows
  // none, and absent from a listener predating the field.
  last_seen: z.number().int().optional(),
  duplicate: z.boolean().optional(),
});

const ReplayWireSchema = z.object({
  stream: z.object({
    // The stream's `envoy_stream_id` metadata: absent from an older listener's answer, or for a
    // stream without it. `created` is what identifies the stream then.
    id: z.string().min(1).optional(),
    created: z.string(),
    first_seq: z.number().int().nonnegative(),
    last_seq: z.number().int().nonnegative(),
  }),
  subjects: z.array(
    z.object({
      subject: z.string(),
      messages: z.array(
        z.object({
          seq: z.number().int().positive(),
          subject: z.string(),
          time: z.string(),
          data: z.string(),
        })
      ),
      next_after: z.number().int().nonnegative(),
      more: z.boolean(),
      // `not_followed` or `not_retained` today; read as a string so a reason a later listener adds
      // still parses, as a subject that answered nothing.
      refused: z.string().optional(),
    })
  ),
});

export type EnvoyFetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

export type EnvoyClientConfig = {
  readonly baseUrl: string;
  readonly fetch: EnvoyFetch;
  readonly timeoutMs?: number;
  /** Whether a request a network error or a 5xx failed is sent once more, after 250 ms. Default
   * true; false for a best-effort read that must not hold its caller past one timeout. */
  readonly retry?: boolean;
  /** The listener's bearer (`envoySettings`), sent on every call; none sends no `Authorization`
   * header, for a loopback listener that asks for none. */
  readonly token?: string | undefined;
};

export type Interest = z.infer<typeof InterestWireSchema>;

export type DeliveryAnswer = z.infer<typeof DeliveryAnswerWireSchema>;

/**
 * The registration's answer: the interest, and the delivery mode. A listener of an earlier release
 * answers no `delivery` field, which reads as `core` (`not_answered`).
 */
export type SubscribeResult = Interest & { readonly delivery: DeliveryAnswer };

/** Where a session's stream sequence was up to: what a new consumer may be seeded from. */
export interface ResumePoint {
  /** The stream's `envoy_stream_id`. */
  readonly stream_id: string;
  readonly seq: number;
}

/** The registration's `delivery` field: this session asks for consumer delivery. */
export interface DeliveryRequest {
  readonly consumer: true;
  /** A random string per plugin NATS connection; a new one restarts the relay at its ack floor. */
  readonly epoch: string;
  readonly resume?: ResumePoint;
}

/** The reason a registration answer without a `delivery` field (an earlier listener) reads as. */
const DELIVERY_NOT_ANSWERED = "not_answered";

/** The reason an answer whose `delivery` field this build cannot read reads as. */
const DELIVERY_UNREADABLE = "unreadable_answer";

// An answer without a `delivery` field (an earlier listener), or with an unreadable one (a mode a
// later listener adds), is `core`: the core path still works, so neither fails the registration.
function parseSubscribeAnswer(body: unknown): SubscribeResult {
  const interest = InterestWireSchema.parse(body);
  const raw =
    typeof body === "object" && body !== null && "delivery" in body ? body.delivery : undefined;
  if (raw === undefined || raw === null) {
    return { ...interest, delivery: { mode: "core", reason: DELIVERY_NOT_ANSWERED } };
  }
  const delivery = DeliveryAnswerWireSchema.safeParse(raw);
  return {
    ...interest,
    delivery: delivery.success ? delivery.data : { mode: "core", reason: DELIVERY_UNREADABLE },
  };
}

/**
 * Live NATS subscriptions and the listener's registry can disagree after a reconnect or a
 * human removal; this reports both, and which side knows each topic. Registry topics come
 * first in insertion order, then the live-only ones.
 */
export function mergeInterestSources(
  registryTopics: readonly string[],
  liveTopics: readonly string[]
): Map<string, "registry" | "live" | "both"> {
  const interests = new Map<string, "registry" | "live" | "both">();
  for (const topic of registryTopics) {
    interests.set(topic, liveTopics.includes(topic) ? "both" : "registry");
  }
  for (const topic of liveTopics) {
    if (!interests.has(topic)) interests.set(topic, "live");
  }
  return interests;
}

export type SessionInfo = z.infer<typeof SessionWireSchema>;

export type RoleInfo = z.infer<typeof RoleWireSchema>;

/**
 * The state a role answer reports for its holder: `holder_state` where the listener names it,
 * else the row's own state. A holder that is only unresponsive still holds the role — nothing on
 * a lookup releases its claim — so this never reads as "no holder".
 */
export function holderStateOf(role: RoleInfo): SessionState {
  return role.holder_state ?? role.state;
}

export type SubscribeInput = {
  readonly sessionID: string;
  readonly directory: string;
  readonly topics: readonly string[];
  readonly port: number;
  readonly title: string;
  readonly capabilities?: readonly string[];
  readonly driving: boolean;
  readonly selfSubscribed?: boolean;
  /** Asks for consumer delivery (`DeliveryRequest`); absent keeps the core path. */
  readonly delivery?: DeliveryRequest;
  /**
   * The client instance that drives this session: one Oh My Pi Envoy extension instance, one
   * Claude Code channel-server process, minted once when that instance starts. The listener
   * stores it on the session's registry entry, a soft role claim naming a predecessor compares
   * it, and a deregistration carrying it deletes only an entry that is this instance's.
   */
  readonly clientInstanceID?: string;
  /** The plugin this session registers through (its npm package name) and that plugin's build,
   * sent on every registration so the listener's session list shows which build each session
   * runs. */
  readonly plugin: { readonly name: string; readonly version: string };
};

/**
 * A new {@link SubscribeInput.clientInstanceID}: 128 random bits as 32 lower-case hex characters.
 *
 * A client instance is one host process driving sessions at the listener — one Oh My Pi Envoy
 * extension instance, one Claude Code channel-server process — and mints this once, when it
 * starts, for every registration and deregistration it makes.
 */
export function newClientInstanceID(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
    byte.toString(16).padStart(2, "0")
  ).join("");
}

export type UnsubscribeInput = {
  readonly sessionID: string;
  readonly topics: readonly string[];
};

export function expandSubscriptionTopics(topics: readonly string[]): readonly string[] {
  const expanded = new Set<string>();
  for (const topic of topics) {
    const base = topic.endsWith(".>") ? topic.slice(0, -2) : undefined;
    if (base === undefined || base.includes("*") || base.includes(">")) {
      expanded.add(topic);
      continue;
    }
    const segments = base.split(".");
    if (segments.length < 2 || segments.some((segment) => segment.length === 0)) {
      throw new TypeError(
        `cannot expand Envoy wildcard topic "${topic}": its concrete base must have at least two non-empty segments`
      );
    }
    expanded.add(base);
    expanded.add(topic);
  }
  return [...expanded];
}

export type MessageSource = "agent" | "human";

export type AgentSourceInput = {
  readonly source?: "agent";
  readonly sourceSessionID: string;
};

export type HumanSourceInput = {
  readonly source: "human";
  readonly sourceSessionID?: string;
};

export type MessageMetadataInput = {
  readonly inReplyTo?: string;
  readonly supersedes?: string;
  readonly urgency?: "low" | "med" | "high" | "blocking";
  readonly expectsReply?: "none" | "optional" | "required";
  readonly expiresAt?: number;
};

export type SendInput = (AgentSourceInput | HumanSourceInput) &
  MessageMetadataInput & {
    readonly targetSessionID: string;
    readonly message: string;
    /**
     * Sent on every attempt; one is minted when absent. Pass the `idempotencyKey` of an
     * `EnvoySendUnconfirmedError` to send that message again: the listener stores it at most once.
     */
    readonly idempotencyKey?: string;
  };

export type PublishInput = (AgentSourceInput | HumanSourceInput) &
  MessageMetadataInput & {
    readonly topic: string;
    readonly message: string;
    readonly payload?: string;
    readonly idempotencyKey?: string;
  };

export type SetRoleInput = {
  readonly sessionID: string;
  readonly role: string;
  /**
   * Claim only if the role is unheld, its holder is no longer live, or its
   * holder is `previousSessionID` and that predecessor is this client
   * instance's (see `previousSessionID`). Any other live holder is left in
   * place and reported in the result instead of being displaced. Default
   * (false) is last-claim-wins.
   */
  readonly soft?: boolean;
  /**
   * The session id this claimant continues (a fork or branch mints a new id
   * in the same process). A soft claim takes the role from that predecessor
   * only when the predecessor has no live registry entry, when its live entry
   * carries this client instance's own `clientInstanceID`, or when either
   * side carries no instance id; a predecessor still live under another
   * client instance keeps the role. Sent only with `soft`.
   */
  readonly previousSessionID?: string;
};

export type SetRoleResult =
  | { readonly claimed: true; readonly interest: Interest }
  | { readonly claimed: false; readonly holder: string };

export type ListSessionsInput = {
  readonly directory?: string;
  readonly title?: string;
  /** `unresponsive` asks the listener for the sessions past their live window as well; without it
   *  the list holds the live ones alone. A gone session is never listed either way. */
  readonly include?: "unresponsive";
};

export type SendResult = {
  readonly envelope: Envelope;
  readonly recipient: string;
  readonly confirmed: boolean;
  /** The recipient's state as the listener resolved it; `live` from a listener that names none.
   *  A message to an unresponsive recipient is stored and reaches it when it answers again. */
  readonly recipientState: SessionState;
  /** The recipient's last registration in epoch milliseconds, which the listener sends beside an
   *  unresponsive recipient's state so the sender can say since when it has been silent. Absent
   *  when the listener knows none, and from a listener predating the field. */
  readonly lastSeen?: number;
  /** The listener already held a send under this target and idempotency key: delivered once. */
  readonly duplicate: boolean;
};

export type PublishResult = {
  readonly envelope: Envelope;
  readonly holder?: string;
};

/** One followed subject of a replay request, read from after `after`, or, without one, started. */
export interface ReplaySubjectInput {
  readonly subject: string;
  /** The last stream sequence handled; absent, the listener answers no messages and the watermark,
   *  which is where a new cursor starts. */
  readonly after?: number;
}

export interface ReplayInput {
  readonly sessionID: string;
  readonly subjects: readonly ReplaySubjectInput[];
  /** The most messages across the whole answer; the listener defaults to 100 and caps it at 500. */
  readonly maxMessages?: number;
}

/** The listener's answer to `POST /v1/replay`, in its wire shape. */
export type ReplayResult = z.infer<typeof ReplayWireSchema>;
export type ReplaySubjectResult = ReplayResult["subjects"][number];
export type ReplayMessage = ReplaySubjectResult["messages"][number];

/**
 * What a refusal naming a session's state adds to its message: which of the three states the
 * listener put that session in, when it last registered, and, for one that is unresponsive, the
 * rule that it is not dead. The listener's own text says neither ("no live session ses_x",
 * "role reviewer holder ses_x lapsed; claim retained (last seen 1762...)"), and an agent reading
 * only the message cannot tell a peer that stopped answering from one that ended — which is the
 * mistake this change exists to stop (dispatch://INFRA-392, R7). A body that names no state adds
 * nothing, so an older listener's message is what it always was.
 */
function refusalStateText(parsed: z.infer<typeof ErrorWireSchema>): string {
  const state = parsed.holder_state ?? parsed.state;
  if (state === undefined) return "";
  const who = parsed.holder === undefined ? "That session" : `Holder ${parsed.holder}`;
  // A listener that can name no registration at all sends none (the field is omitted, or 0):
  // the state alone is said, since a made-up time would read as knowledge nobody has.
  const seen =
    parsed.last_seen === undefined || parsed.last_seen <= 0
      ? ""
      : `, last seen ${new Date(parsed.last_seen).toISOString()}`;
  const rule = state === "unresponsive" ? ` ${UNRESPONSIVE_RULE}` : "";
  return ` ${who} is ${state}${seen}.${rule}`;
}

export class EnvoyApiError extends Error {
  readonly name = "EnvoyApiError";
  readonly expected: readonly string[] | undefined;
  /** The listener's machine-readable `code`, when its error body carries one. */
  readonly code: string | undefined;
  /** The session the refusal is about: a role's holder, where its body names one. */
  readonly holder: string | undefined;
  /** That session's state as the listener's record put it, when the body names one: a refused
   *  send's `state`, a lapsed role's `holder_state`. */
  readonly sessionState: SessionState | undefined;
  /** That session's last registration in epoch milliseconds, when the body names one. */
  readonly lastSeen: number | undefined;
  /** Whether the refusal released the holder's role claim. The listener answers it on every
   *  lapsed role and it is always false: a lookup or a publish takes nothing from a holder,
   *  whatever its state. */
  readonly claimReleased: boolean | undefined;

  constructor(
    readonly details: {
      readonly method: string;
      readonly url: string;
      readonly status: number;
      readonly responseBody: string;
    }
  ) {
    let parsed: z.infer<typeof ErrorWireSchema> | undefined;
    try {
      const result = ErrorWireSchema.safeParse(JSON.parse(details.responseBody));
      if (result.success) parsed = result.data;
    } catch {
      // A non-JSON error remains useful in the transport diagnostic.
    }
    super(
      parsed === undefined
        ? `${details.method} ${details.url} failed with ${details.status}: ${details.responseBody}`
        : `${parsed.error}${parsed.expected === undefined ? "" : ` (expected: ${parsed.expected.join(", ")})`}${refusalStateText(parsed)}`
    );
    this.expected = parsed?.expected;
    this.code = parsed?.code;
    this.holder = parsed?.holder;
    this.sessionState = parsed?.holder_state ?? parsed?.state;
    this.lastSeen = parsed?.last_seen;
    this.claimReleased = parsed?.claim_released;
  }
}

/** The listener's own test of whether a send's dedupe key lets the stream and hosts drop a repeat. */
const mintedDedupeKey = new RegExp(MINTED_DEDUPE_KEY_PATTERN);

/**
 * A send that ended without the listener's word that it was stored: an attempt got no answer at
 * all (the request timed out, its connection failed, or a 2xx answer's body did not arrive or did
 * not parse) or got a 5xx, so the message may or may not have reached its target, even when a
 * later attempt was refused. Every attempt carried `idempotencyKey`. The listener sends under the
 * dedupe key `agent.<target>.<idempotencyKey>`, and when that key is one a repeat is recognised by
 * (`MINTED_DEDUPE_KEY_PATTERN`: a lower-case UUID and a target with no dot), the stream stores
 * one copy and the recipient's plugin drops a repeat inside `DELIVERY_DUPLICATE_WINDOW_MS`, which
 * states the rare cases where a recipient that restarted still sees it twice (`resendSafe`). A
 * send refused with a 4xx on every attempt it made stays an `EnvoyApiError`. `last` (also the
 * `cause`) is what the last attempt got.
 */
export class EnvoySendUnconfirmedError extends Error {
  override readonly name = "EnvoySendUnconfirmedError";
  /** Whether Envoy recognises a resend under `idempotencyKey` inside the window as a repeat. */
  readonly resendSafe: boolean;

  constructor(
    readonly targetSessionID: string,
    readonly idempotencyKey: string,
    readonly attempts: number,
    readonly last: unknown
  ) {
    const got =
      last instanceof EnvoyApiError
        ? `the listener answered ${last.details.status} (${last.message})`
        : `the listener did not answer (${last instanceof Error ? last.message : String(last)})`;
    const tried = attempts === 1 ? "on its only attempt" : `on the last of ${attempts} attempts`;
    const resendSafe = mintedDedupeKey.test(`agent.${targetSessionID.trim()}.${idempotencyKey}`);
    const resend = resendSafe
      ? `Sending it again with idempotency_key ${idempotencyKey} within ` +
        `${DELIVERY_DUPLICATE_WINDOW_MS / 3_600_000} hours is the safe resend: Envoy drops it as ` +
        "a repeat if the first copy arrived (a recipient that restarted in between can rarely " +
        "see it twice)."
      : `Envoy cannot recognise a resend under idempotency_key ${idempotencyKey} as a repeat ` +
        "(the key is not a lower-case UUID, or the session id holds a dot): ask the recipient " +
        "whether it arrived before sending it again.";
    super(
      `The send to ${targetSessionID} is not confirmed delivered: ${got} ${tried}. It may or may ` +
        `not have arrived. ${resend}`,
      { cause: last }
    );
    this.resendSafe = resendSafe;
  }
}

export type EnvoyClient = {
  readonly subscribe: (input: SubscribeInput) => Promise<SubscribeResult>;
  readonly unsubscribe: (input: UnsubscribeInput) => Promise<void>;
  readonly getInterest: (sessionID: string) => Promise<Interest>;
  readonly send: (input: SendInput) => Promise<SendResult>;
  readonly publish: (input: PublishInput) => Promise<PublishResult>;
  /**
   * Deletes the session's registry entry. With `clientInstanceID` the listener deletes the entry
   * only when it carries the same instance id or none, so a departing client instance never
   * deletes the registration a successor instance made under the same session id; without one
   * the entry is deleted as before. The listener's answer says whether the entry stayed, and
   * this resolves `void` because no caller acts on it: a deregistering client has nothing to
   * retry either way.
   */
  readonly unregisterSession: (input: {
    readonly sessionID: string;
    readonly clientInstanceID?: string;
  }) => Promise<void>;
  /**
   * Claims `role` for the session. A soft claim (`soft`) naming a predecessor
   * (`previousSessionID`) takes the role from it only when that predecessor has no live registry
   * entry, when its live entry carries this client instance's own instance id, or when either
   * side carries no instance id; any other live holder keeps the role and is reported as
   * `{ claimed: false, holder }`.
   */
  readonly setRole: (input: SetRoleInput) => Promise<SetRoleResult>;
  readonly getRole: (role: string) => Promise<RoleInfo>;
  readonly listSessions: (input?: ListSessionsInput) => Promise<readonly SessionInfo[]>;
  /**
   * Reads what the notification stream stored for a session's followed subjects after each one's
   * cursor. Tried once, with no retry of a 5xx: a sweep runs again on its next trigger, and a 503
   * `REPLAY_UNAVAILABLE` must reach the caller at once so the session can be told.
   */
  readonly replay: (input: ReplayInput) => Promise<ReplayResult>;
};

type RequestOptions = {
  readonly timeoutMs?: number;
  readonly retry?: boolean;
  /** Called for each attempt that got no answer or a 5xx: one the listener may have acted on. */
  readonly onUnanswered?: () => void;
};

/** One attempt's outcome: the listener's status and body, or no answer and why. */
type AttemptOutcome =
  | { readonly response: Response; readonly body: string }
  | { readonly unanswered: unknown };

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  const baseUrl = normalizeEnvoyUrl(config.baseUrl);
  const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const apiToken = config.token;
  const attempts = config.retry === false ? 1 : 2;

  const attemptOnce = async (
    url: string,
    init: RequestInit,
    deadlineMs: number
  ): Promise<AttemptOutcome> => {
    const headers = new Headers(init.headers);
    if (apiToken) headers.set("Authorization", `Bearer ${apiToken}`);
    let response: Response;
    try {
      response = await config.fetch(url, {
        ...init,
        headers,
        signal: AbortSignal.timeout(deadlineMs),
      });
    } catch (error) {
      return { unanswered: error };
    }
    // The body is read under the same deadline. A refusal whose body does not arrive is still the
    // listener's refusal; any other answer whose body does not arrive is no answer.
    try {
      return { response, body: await response.text() };
    } catch (error) {
      if (response.status >= 400 && response.status < 500) {
        return { response, body: `(its body did not arrive: ${String(error)})` };
      }
      return { unanswered: error };
    }
  };

  const request = async (
    path: string,
    init: RequestInit,
    options: RequestOptions = {}
  ): Promise<string> => {
    const url = `${baseUrl}${path}`;
    const tries = options.retry === false ? 1 : attempts;
    for (let attempt = 0; attempt < tries; attempt += 1) {
      const outcome = await attemptOnce(url, init, options.timeoutMs ?? timeoutMs);
      if ("unanswered" in outcome) {
        options.onUnanswered?.();
        if (attempt + 1 < tries) {
          await waitForRetry();
          continue;
        }
        throw outcome.unanswered;
      }
      const { response, body } = outcome;
      if (response.ok) return body;

      const error = new EnvoyApiError({
        method: init.method ?? "GET",
        url,
        status: response.status,
        responseBody: body,
      });
      if (response.status >= 500) {
        options.onUnanswered?.();
        if (attempt + 1 < tries) {
          await waitForRetry();
          continue;
        }
      }
      throw error;
    }
    throw new Error("Envoy request exhausted retries");
  };

  const post = (path: string, body: object, options?: RequestOptions) =>
    request(
      path,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      },
      options
    );

  return {
    subscribe: async (input) =>
      parseSubscribeAnswer(
        JSON.parse(
          await post("/v1/interests/subscribe", {
            session_id: input.sessionID,
            dir: input.directory,
            topics: expandSubscriptionTopics(input.topics),
            port: input.port,
            title: input.title,
            ...(input.capabilities === undefined ? {} : { capabilities: input.capabilities }),
            driving: input.driving,
            ...(input.selfSubscribed === undefined
              ? {}
              : { self_subscribed: input.selfSubscribed }),
            ...(input.delivery === undefined ? {} : { delivery: input.delivery }),
            // The client instance driving this session (`clientInstanceID`), omitted when the
            // caller has none so an older listener sees an unchanged body. The listener stores it
            // on the registry entry, where a soft role claim and a deregistration compare it.
            ...(input.clientInstanceID === undefined
              ? {}
              : { client_instance_id: input.clientInstanceID }),
            // Where this session process actually runs (machineID(): ENVOY_MACHINE_ID, falling
            // back to the kernel hostname), sent on every subscribe and heartbeat (registerSession
            // re-subscribes on each tick). The listener stores it separately from the registering
            // listener's own machine_id, and the session's owner reaches a serve-port session at
            // it unless the session runs on the owner's machine (registered through it, or this
            // value is the owner's machine id) and the owner has a host bridge.
            // The owner is the listener the session's stored subscription names, else the one it
            // registered through; only the owner delivers to the session, and any other listener
            // leaves inbox messages to it silently. With a port, a value that is not a host name
            // or IP address is refused with 400 (expected: ["host"]).
            host: machineID(),
            plugin: input.plugin.name,
            plugin_version: input.plugin.version,
          })
        )
      ),
    unsubscribe: async (input) => {
      await post("/v1/interests/unsubscribe", {
        session_id: input.sessionID,
        topics: expandSubscriptionTopics(input.topics),
      });
    },
    getInterest: async (sessionID) =>
      InterestWireSchema.parse(JSON.parse(await request(`/v1/interests/${sessionID}`, {}))),
    send: async (input) => {
      const idempotencyKey = input.idempotencyKey ?? crypto.randomUUID();
      // An attempt the listener may have acted on keeps the send unconfirmed, whatever a later
      // attempt got: a first attempt can store the message and lose its answer, and the retry
      // then find the target gone.
      let unanswered = false;
      let response: z.infer<typeof EnvelopeResponseSchema>;
      try {
        const answer = await post(
          "/v1/messages/send",
          {
            source: input.source ?? "agent",
            ...sourceSession(input.sourceSessionID),
            target_session: input.targetSessionID,
            message: input.message,
            idempotency_key: idempotencyKey,
            ...messageMetadata(input),
          },
          {
            onUnanswered: () => {
              unanswered = true;
            },
          }
        );
        // A 2xx whose body does not parse is the listener's word that cannot be read.
        response = EnvelopeResponseSchema.parse(JSON.parse(answer));
      } catch (error) {
        if (error instanceof EnvoyApiError && error.details.status < 500 && !unanswered) {
          throw error;
        }
        throw new EnvoySendUnconfirmedError(input.targetSessionID, idempotencyKey, attempts, error);
      }
      return {
        envelope: response,
        recipient: response.recipient ?? input.targetSessionID,
        confirmed: response.recipient !== undefined,
        recipientState: response.recipient_state ?? "live",
        // Omitted rather than zero when the listener names none, so a reader never reads 1970
        // as a last registration.
        ...(response.last_seen === undefined || response.last_seen <= 0
          ? {}
          : { lastSeen: response.last_seen }),
        duplicate: response.duplicate === true,
      };
    },
    publish: async (input) => {
      const idempotencyKey = input.idempotencyKey ?? crypto.randomUUID();
      const response = EnvelopeResponseSchema.parse(
        JSON.parse(
          await post("/v1/messages/publish", {
            source: input.source ?? "agent",
            ...sourceSession(input.sourceSessionID),
            topic: input.topic,
            message: input.message,
            ...(input.payload === undefined ? {} : { payload: input.payload }),
            idempotency_key: idempotencyKey,
            ...messageMetadata(input),
          })
        )
      );
      return {
        envelope: response,
        ...(response.holder === undefined ? {} : { holder: response.holder }),
      };
    },
    unregisterSession: async (input) => {
      // The query parameter is omitted without an instance id, so that request stays
      // byte-for-byte what an older client sent and the listener deletes unconditionally.
      const query =
        input.clientInstanceID === undefined
          ? ""
          : `?client_instance_id=${encodeURIComponent(input.clientInstanceID)}`;
      await request(`/v1/sessions/${encodeURIComponent(input.sessionID)}${query}`, {
        method: "DELETE",
      });
    },
    setRole: async (input) => {
      // `soft` and `previous_session_id` are omitted from the wire unless set,
      // so an older listener sees an unchanged hard-claim request. The listener
      // decides a soft claim against a live predecessor by the instance id each
      // side's registry entry carries (`SubscribeInput.clientInstanceID`).
      const soft = input.soft === true;
      const previous = soft ? (input.previousSessionID ?? "") : "";
      const body: { session_id: string; role: string; soft?: true; previous_session_id?: string } =
        {
          session_id: input.sessionID,
          role: input.role,
          ...(soft ? { soft: true } : {}),
          ...(previous === "" ? {} : { previous_session_id: previous }),
        };
      let raw: string;
      try {
        raw = await post("/v1/roles/set", body);
      } catch (error) {
        // A soft claim refused by a live holder is an outcome, not a failure:
        // the listener answers 409 with the holder's id.
        if (error instanceof EnvoyApiError && error.details.status === 409) {
          let answer: unknown;
          try {
            answer = JSON.parse(error.details.responseBody);
          } catch {
            // A 409 whose body did not arrive, or is not JSON, names no holder: it stays the refusal.
            throw error;
          }
          const held = RoleHeldWireSchema.safeParse(answer);
          if (held.success) return { claimed: false, holder: held.data.holder };
        }
        throw error;
      }
      return { claimed: true, interest: InterestWireSchema.parse(JSON.parse(raw)) };
    },
    getRole: async (role) =>
      RoleWireSchema.parse(
        JSON.parse(await request(`/v1/roles/${encodeURIComponent(role)}`, { method: "GET" }))
      ),
    listSessions: async (input = {}) => {
      const search = new URLSearchParams();
      if (input.directory !== undefined) search.set("dir", input.directory);
      if (input.title !== undefined) search.set("title", input.title);
      if (input.include !== undefined) search.set("include", input.include);
      const query = search.size === 0 ? "" : `?${search.toString()}`;
      return SessionWireSchema.array().parse(JSON.parse(await request(`/v1/sessions${query}`, {})));
    },
    replay: async (input) =>
      ReplayWireSchema.parse(
        JSON.parse(
          await post(
            "/v1/replay",
            {
              session_id: input.sessionID,
              subjects: input.subjects.map((cursor) =>
                cursor.after === undefined
                  ? { subject: cursor.subject }
                  : { subject: cursor.subject, after: cursor.after }
              ),
              ...(input.maxMessages === undefined ? {} : { max_messages: input.maxMessages }),
            },
            { timeoutMs: REPLAY_TIMEOUT_MS, retry: false }
          )
        )
      ),
  };
}

function waitForRetry(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, RETRY_DELAY_MS);
  return promise;
}

function messageMetadata(input: MessageMetadataInput) {
  return {
    ...(input.inReplyTo === undefined ? {} : { in_reply_to: input.inReplyTo }),
    ...(input.supersedes === undefined ? {} : { supersedes: input.supersedes }),
    ...(input.urgency === undefined ? {} : { urgency: input.urgency }),
    ...(input.expectsReply === undefined ? {} : { expects_reply: input.expectsReply }),
    ...(input.expiresAt === undefined ? {} : { expires_at: input.expiresAt }),
  };
}

/**
 * A blank source session is no source session. The listener's own JSON drops an empty
 * `source_session` on the way back out (`omitempty`), so sending one costs the recipient both
 * the sender label and the reply hint while looking, to the sender, like an attributed message.
 */
function sourceSession(sessionID: string | undefined) {
  return sessionID === undefined || sessionID === "" ? {} : { source_session: sessionID };
}

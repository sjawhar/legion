import { type Envelope, EnvelopeSchema } from "@legion/contracts";
import { z } from "zod";
import { normalizeEnvoyUrl } from "./envoy-settings";
import { machineID } from "./machine";

const DEFAULT_TIMEOUT_MS = 5_000;
const RETRY_DELAY_MS = 250;

const InterestWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  dir: z.string(),
  topics: z.array(z.string()),
  updated_at: z.number().int().optional(),
  warnings: z.array(z.string()).optional(),
});

/**
 * How the listener delivers a session's stored subjects, answered to every registration it
 * accepts: `consumer` relays the session's durable consumer on `subject`. There is no other
 * mode — a registration no session consumer can be given is refused (503 `consumer_error` or
 * `no_grant`, 409 `not_requested` or `invalid_session_id`), never answered.
 */
const DeliveryAnswerWireSchema = z.object({
  mode: z.literal("consumer"),
  subject: z.string().min(1),
  stream_id: z.string(),
  gap: z.boolean(),
});

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
  // The plugin the session registered through and its build; absent from a listener predating the
  // fields and for a session whose plugin predates them.
  plugin: z.string().optional(),
  plugin_version: z.string().optional(),
  // How the listener delivers to the session: "consumer" when its own session consumer holds its
  // stored messages, "" for a session on the core path. A listener that reports delivery writes
  // the key for every session, so an absent key is only a listener predating the field, never a
  // session on the core path; optional here so such a listener's row still parses.
  delivery: z.string().optional(),
});

const RoleWireSchema = z.object({
  role: z.string(),
  holder: z.string(),
  host: z.string().optional(),
  last_seen: z.number().int(),
});

const RoleHeldWireSchema = z.object({
  error: z.string(),
  role: z.string(),
  holder: z.string(),
});

const ErrorWireSchema = z.object({
  error: z.string(),
  code: z.string().optional(),
  // The machine-readable why of a registration the listener refused because it can give the
  // session no consumer (`consumer_error`, `no_grant`, `not_requested`, `invalid_session_id`).
  reason: z.string().optional(),
  expected: z.array(z.string()).optional(),
});

const EnvelopeResponseSchema = EnvelopeSchema.extend({
  recipient: z.string().optional(),
  holder: z.string().optional(),
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
 * The registration's answer: the interest, and the delivery it was given. Every answer carries
 * one; a registration the listener can give no consumer is refused, never answered
 * (`parseSubscribeAnswer`).
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

/** A delivery answer this build does not take, read far enough to name it in the failure. */
const DeliveryRefusalSchema = z.object({
  mode: z.string(),
  reason: z.string().optional(),
});

/** What an answer whose `delivery` field this build cannot read at all is reported as. */
const DELIVERY_UNREADABLE = "answered a delivery field this build cannot read";

/**
 * Reads a registration answer. A session receives only what its own consumer holds, so an answer
 * that names no consumer delivery — none at all (a listener predating session consumers), the
 * `core` mode this build no longer accepts, or one this build cannot read — fails the
 * registration here, loudly and with the listener's own words in the message, rather than
 * leaving the session registered and deaf.
 */
export function parseSubscribeAnswer(body: unknown): SubscribeResult {
  const interest = InterestWireSchema.parse(body);
  const raw =
    typeof body === "object" && body !== null && "delivery" in body ? body.delivery : undefined;
  if (raw === undefined || raw === null) {
    throw new Error(
      `the listener answered no delivery for ${interest.session_id}: it predates session consumers, and a session receives only what its own consumer holds`
    );
  }
  const delivery = DeliveryAnswerWireSchema.safeParse(raw);
  if (!delivery.success) {
    const named = DeliveryRefusalSchema.safeParse(raw);
    const said = named.success
      ? `answered delivery mode ${named.data.mode}${named.data.reason === undefined ? "" : ` (${named.data.reason})`}`
      : DELIVERY_UNREADABLE;
    throw new Error(
      `the listener gave ${interest.session_id} no consumer: it ${said}, and a session receives only what its own consumer holds`
    );
  }
  return { ...interest, delivery: delivery.data };
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

export type SubscribeInput = {
  readonly sessionID: string;
  readonly directory: string;
  readonly topics: readonly string[];
  readonly port: number;
  readonly title: string;
  readonly capabilities?: readonly string[];
  readonly driving: boolean;
  readonly selfSubscribed?: boolean;
  /**
   * Asks for consumer delivery (`DeliveryRequest`). Required: a session receives only what its
   * own consumer holds, so a registration that asked for none is refused `409 not_requested`.
   */
  readonly delivery: DeliveryRequest;
  /** The plugin this session registers through (its npm package name) and that plugin's build,
   * sent on every registration so the listener's session list shows which build each session
   * runs. */
  readonly plugin: { readonly name: string; readonly version: string };
};

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
   * holder is `previousSessionID`. Any other live holder is left in place and
   * reported in the result instead of being displaced. Default (false) is
   * last-claim-wins.
   */
  readonly soft?: boolean;
  /**
   * The session id this claimant continues (a fork or branch mints a new id
   * in the same process). Lets a soft claim take the role from that still-live
   * predecessor. Sent only with `soft`.
   */
  readonly previousSessionID?: string;
};

export type SetRoleResult =
  | { readonly claimed: true; readonly interest: Interest }
  | { readonly claimed: false; readonly holder: string };

export type ListSessionsInput = {
  readonly directory?: string;
  readonly title?: string;
};

export type SendResult = {
  readonly envelope: Envelope;
  readonly recipient: string;
  readonly confirmed: boolean;
};

export type PublishResult = {
  readonly envelope: Envelope;
  readonly holder?: string;
};

export class EnvoyApiError extends Error {
  readonly name = "EnvoyApiError";
  readonly expected: readonly string[] | undefined;
  /** The listener's machine-readable `code`, when its error body carries one. */
  readonly code: string | undefined;
  /**
   * Why the listener refused, when its error body carries one: the reason a registration no
   * session consumer can be given is refused with (`consumer_error`, `no_grant`,
   * `not_requested`, `invalid_session_id`).
   */
  readonly reason: string | undefined;

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
    // The reason joins the message: a refused registration must fail with the listener's own
    // machine-readable why in what the session is shown, not only in the client's field.
    super(
      parsed === undefined
        ? `${details.method} ${details.url} failed with ${details.status}: ${details.responseBody}`
        : `${parsed.error}${parsed.reason === undefined ? "" : ` (reason: ${parsed.reason})`}${parsed.expected === undefined ? "" : ` (expected: ${parsed.expected.join(", ")})`}`
    );
    this.expected = parsed?.expected;
    this.code = parsed?.code;
    this.reason = parsed?.reason;
  }
}

export type EnvoyClient = {
  readonly subscribe: (input: SubscribeInput) => Promise<SubscribeResult>;
  readonly unsubscribe: (input: UnsubscribeInput) => Promise<void>;
  readonly getInterest: (sessionID: string) => Promise<Interest>;
  readonly send: (input: SendInput) => Promise<SendResult>;
  readonly publish: (input: PublishInput) => Promise<PublishResult>;
  readonly unregisterSession: (sessionID: string) => Promise<void>;
  readonly setRole: (input: SetRoleInput) => Promise<SetRoleResult>;
  readonly getRole: (role: string) => Promise<RoleInfo>;
  readonly listSessions: (input?: ListSessionsInput) => Promise<readonly SessionInfo[]>;
};

type RequestOptions = { readonly timeoutMs?: number; readonly retry?: boolean };

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  const baseUrl = normalizeEnvoyUrl(config.baseUrl);
  const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const apiToken = config.token;

  const request = async (
    path: string,
    init: RequestInit,
    options: RequestOptions = {}
  ): Promise<string> => {
    const url = `${baseUrl}${path}`;
    const attempts = config.retry === false ? 1 : 2;
    const tries = options.retry === false ? 1 : attempts;
    for (let attempt = 0; attempt < tries; attempt += 1) {
      let response: Response;
      try {
        const headers = new Headers(init.headers);
        if (apiToken) headers.set("Authorization", `Bearer ${apiToken}`);
        response = await config.fetch(url, {
          ...init,
          headers,
          signal: AbortSignal.timeout(options.timeoutMs ?? timeoutMs),
        });
      } catch (error) {
        if (attempt + 1 < tries) {
          await waitForRetry();
          continue;
        }
        throw error;
      }
      const responseBody = await response.text();
      if (response.ok) return responseBody;

      const error = new EnvoyApiError({
        method: init.method ?? "GET",
        url,
        status: response.status,
        responseBody,
      });
      if (response.status >= 500 && attempt + 1 < tries) {
        await waitForRetry();
        continue;
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
            delivery: input.delivery,
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
      const response = EnvelopeResponseSchema.parse(
        JSON.parse(
          await post("/v1/messages/send", {
            source: input.source ?? "agent",
            ...sourceSession(input.sourceSessionID),
            target_session: input.targetSessionID,
            message: input.message,
            idempotency_key: idempotencyKey,
            ...messageMetadata(input),
          })
        )
      );
      return {
        envelope: response,
        recipient: response.recipient ?? input.targetSessionID,
        confirmed: response.recipient !== undefined,
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
    unregisterSession: async (sessionID) => {
      await request(`/v1/sessions/${encodeURIComponent(sessionID)}`, { method: "DELETE" });
    },
    setRole: async (input) => {
      // `soft` and `previous_session_id` are omitted from the wire unless set,
      // so an older listener sees an unchanged hard-claim request.
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
          const held = RoleHeldWireSchema.safeParse(JSON.parse(error.details.responseBody));
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
      const query = search.size === 0 ? "" : `?${search.toString()}`;
      return SessionWireSchema.array().parse(JSON.parse(await request(`/v1/sessions${query}`, {})));
    },
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

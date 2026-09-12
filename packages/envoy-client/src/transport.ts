import { type Envelope, EnvelopeSchema } from "@legion/contracts";
import { z } from "zod";
import { normalizeEnvoyUrl } from "./defaults";

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

const SessionWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  dir: z.string(),
  port: z.number().int(),
  title: z.string(),
  topics: z.array(z.string()),
  roles: z.array(z.string()).optional(),
  capabilities: z.array(z.string()).optional(),
  self_subscribed: z.boolean(),
  last_seen: z.number().int().optional(),
});

const RoleWireSchema = z.object({
  role: z.string(),
  holder: z.string(),
  last_seen: z.number().int(),
});

const RoleHeldWireSchema = z.object({
  error: z.string(),
  role: z.string(),
  holder: z.string(),
});

const ErrorWireSchema = z.object({
  error: z.string(),
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
};

export type Interest = z.infer<typeof InterestWireSchema>;

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
        : `${parsed.error}${parsed.expected === undefined ? "" : ` (expected: ${parsed.expected.join(", ")})`}`
    );
    this.expected = parsed?.expected;
  }
}

export type EnvoyClient = {
  readonly subscribe: (input: SubscribeInput) => Promise<Interest>;
  readonly unsubscribe: (input: UnsubscribeInput) => Promise<void>;
  readonly getInterest: (sessionID: string) => Promise<Interest>;
  readonly send: (input: SendInput) => Promise<SendResult>;
  readonly publish: (input: PublishInput) => Promise<PublishResult>;
  readonly unregisterSession: (sessionID: string) => Promise<void>;
  readonly setRole: (input: SetRoleInput) => Promise<SetRoleResult>;
  readonly getRole: (role: string) => Promise<RoleInfo>;
  readonly listSessions: (input?: ListSessionsInput) => Promise<readonly SessionInfo[]>;
};

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  const baseUrl = normalizeEnvoyUrl(config.baseUrl);
  const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const { ENVOY_TOKEN: apiToken } = process.env;

  const request = async (path: string, init: RequestInit): Promise<string> => {
    const url = `${baseUrl}${path}`;
    for (let attempt = 0; attempt < 2; attempt += 1) {
      let response: Response;
      try {
        const headers = new Headers(init.headers);
        if (apiToken) headers.set("Authorization", `Bearer ${apiToken}`);
        response = await config.fetch(url, {
          ...init,
          headers,
          signal: AbortSignal.timeout(timeoutMs),
        });
      } catch (error) {
        if (attempt === 0) {
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
      if (response.status >= 500 && attempt === 0) {
        await waitForRetry();
        continue;
      }
      throw error;
    }
    throw new Error("Envoy request exhausted retries");
  };

  const post = (path: string, body: object) =>
    request(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });

  return {
    subscribe: async (input) =>
      InterestWireSchema.parse(
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
            ...(input.sourceSessionID === undefined
              ? {}
              : { source_session: input.sourceSessionID }),
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
            ...(input.sourceSessionID === undefined
              ? {}
              : { source_session: input.sourceSessionID }),
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

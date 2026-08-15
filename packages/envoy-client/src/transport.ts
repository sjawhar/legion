import { type Envelope, EnvelopeSchema } from "@legion/contracts";
import { z } from "zod";
import { normalizeEnvoyUrl } from "./defaults";

const DEFAULT_TIMEOUT_MS = 5_000;

const InterestWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  dir: z.string(),
  topics: z.array(z.string()),
  updated_at: z.number().int().optional(),
});

const SessionWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  dir: z.string(),
  port: z.number().int(),
  title: z.string(),
  topics: z.array(z.string()),
  self_subscribed: z.boolean(),
});

export type EnvoyFetch = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export type EnvoyClientConfig = {
  readonly baseUrl: string;
  readonly fetch: EnvoyFetch;
  readonly timeoutMs?: number;
};

export type Interest = z.infer<typeof InterestWireSchema>;

export type SessionInfo = z.infer<typeof SessionWireSchema>;

export type SubscribeInput = {
  readonly sessionID: string;
  readonly directory: string;
  readonly topics: readonly string[];
  readonly port: number;
  readonly title: string;
  readonly driving: boolean;
  readonly selfSubscribed?: boolean;
};

export type UnsubscribeInput = {
  readonly sessionID: string;
  readonly topics: readonly string[];
};

export type SendInput = {
  readonly sourceSessionID: string;
  readonly targetSessionID: string;
  readonly message: string;
  readonly idempotencyKey?: string;
};

export type PublishInput = {
  readonly sourceSessionID: string;
  readonly topic: string;
  readonly message: string;
  readonly idempotencyKey?: string;
};

export type SetRoleInput = {
  readonly sessionID: string;
  readonly role: string;
};

export class EnvoyApiError extends Error {
  readonly name = "EnvoyApiError";

  constructor(
    readonly details: {
      readonly method: string;
      readonly url: string;
      readonly status: number;
      readonly responseBody: string;
    }
  ) {
    super(
      `${details.method} ${details.url} failed with ${details.status}: ${details.responseBody}`
    );
  }
}

export type EnvoyClient = {
  readonly subscribe: (input: SubscribeInput) => Promise<Interest>;
  readonly unsubscribe: (input: UnsubscribeInput) => Promise<void>;
  readonly getInterest: (sessionID: string) => Promise<Interest>;
  readonly send: (input: SendInput) => Promise<Envelope>;
  readonly publish: (input: PublishInput) => Promise<Envelope>;
  readonly setRole: (input: SetRoleInput) => Promise<Interest>;
  readonly listSessions: () => Promise<readonly SessionInfo[]>;
};

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  const baseUrl = normalizeEnvoyUrl(config.baseUrl);
  const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  const request = async (path: string, init: RequestInit): Promise<string> => {
    const url = `${baseUrl}${path}`;
    const response = await config.fetch(url, { ...init, signal: AbortSignal.timeout(timeoutMs) });
    const responseBody = await response.text();
    if (!response.ok) {
      throw new EnvoyApiError({
        method: init.method ?? "GET",
        url,
        status: response.status,
        responseBody,
      });
    }
    return responseBody;
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
            topics: input.topics,
            port: input.port,
            title: input.title,
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
        topics: input.topics,
      });
    },
    getInterest: async (sessionID) =>
      InterestWireSchema.parse(JSON.parse(await request(`/v1/interests/${sessionID}`, {}))),
    send: async (input) =>
      toEnvelope(
        await post("/v1/messages/send", {
          source_session: input.sourceSessionID,
          target_session: input.targetSessionID,
          message: input.message,
          ...(input.idempotencyKey === undefined ? {} : { idempotency_key: input.idempotencyKey }),
        })
      ),
    publish: async (input) =>
      toEnvelope(
        await post("/v1/messages/publish", {
          source_session: input.sourceSessionID,
          topic: input.topic,
          message: input.message,
          ...(input.idempotencyKey === undefined ? {} : { idempotency_key: input.idempotencyKey }),
        })
      ),
    setRole: async (input) =>
      InterestWireSchema.parse(
        JSON.parse(await post("/v1/roles/set", { session_id: input.sessionID, role: input.role }))
      ),
    listSessions: async () =>
      SessionWireSchema.array().parse(JSON.parse(await request("/v1/sessions", {}))),
  };
}

function toEnvelope(value: string): Envelope {
  return EnvelopeSchema.parse(JSON.parse(value));
}

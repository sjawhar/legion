import { messageFor } from "@legion/envoy-client/errors";
import {
  LegionGoErrorResponse,
  LegionGoRegisterResponse,
  type LegionGoRegistration,
  type LegionGoState,
  LegionGoStateResponse,
} from "@legion/contracts/legion-go-api";
import type { z } from "zod";

/**
 * The plugin's client for the Go daemon (`packages/daemon-go`), the one `extensions/legion.ts`
 * boots through when the pane carries `LEGION_DAEMON_API=go`. `daemon-client.ts` stays the
 * TypeScript daemon's client until Stage 7 deletes it and that daemon together.
 *
 * Every response is read through the strict schemas of `@legion/contracts/legion-go-api`, which the
 * Go daemon's golden tests hold to its own types; the requests are the claim wire of
 * `packages/daemon-go/internal/claim/wire.go`, which refuses a member it does not read. There is no
 * secret recovery: the Go daemon persists a registration's capability before it answers, so a
 * restart forgets no secret.
 */

/** `claim.RegisterRequest`: the pane's boot token and the session this agent became.
 * `pluginContract` is this build's `legion.goDaemonApiVersion`. */
export interface GoRegisterInput {
  readonly bootToken: string;
  readonly sessionId: string;
  readonly ompSessionFile: string;
  readonly agentId: string;
  readonly pluginContract: number;
}

/** `claim.ReadyRequest`: the claim the registration issued, authenticated by its secret. */
export interface GoReadyInput {
  readonly claimToken: string;
  readonly sessionId: string;
  readonly secret: string;
  readonly generation: number;
}

/** `claim.ExitRequest`: the agent reporting its own end, and why. */
export interface GoExitInput extends GoReadyInput {
  readonly reason: string;
}

export interface LegionGoDaemonClient {
  readonly state: () => Promise<LegionGoState>;
  readonly register: (input: GoRegisterInput) => Promise<LegionGoRegistration>;
  readonly ready: (input: GoReadyInput) => Promise<void>;
  readonly exit: (input: GoExitInput) => Promise<void>;
}

/** The Go daemon answered with a status that is not success. `detail` is the sentence it put
 * under `error` (`claim.Refusal` for the claim routes) or, when the body is not that shape, the
 * body and what the strict parse found. */
export class LegionGoDaemonApiError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly detail: string
  ) {
    super(`${method} ${path} failed with ${status}: ${detail}`);
    this.name = "LegionGoDaemonApiError";
  }
}

/** A success the Go daemon API contract does not describe. The body is never quoted: a
 * registration's carries the claim's secret. */
export class LegionGoDaemonContractError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    problem: string
  ) {
    super(
      `${method} ${path} answered ${status} with a body the Go daemon API contract does not describe: ${problem}`
    );
    this.name = "LegionGoDaemonContractError";
  }
}

/** `body` read through `schema`, or the reason it is not what the schema describes. */
function parseStrictly<T>(
  schema: z.ZodType<T>,
  body: string
): { readonly value: T } | { readonly problem: string } {
  let json: unknown;
  try {
    json = JSON.parse(body);
  } catch (error) {
    return { problem: `not JSON (${messageFor(error)})` };
  }
  const parsed = schema.safeParse(json);
  if (parsed.success) return { value: parsed.data };
  return {
    problem: parsed.error.issues
      .map((issue) => `${issue.path.join(".") || "(root)"}: ${issue.message}`)
      .join("; "),
  };
}

export function createLegionGoDaemonClient(
  baseUrl: string,
  fetchFn: typeof fetch = fetch
): LegionGoDaemonClient {
  const endpoint = baseUrl.replace(/\/+$/, "");

  /** The response's status and body once the daemon answered success; a refusal throws. */
  const call = async (
    method: "GET" | "POST",
    path: string,
    body?: object
  ): Promise<{ readonly status: number; readonly text: string }> => {
    const response = await fetchFn(
      `${endpoint}${path}`,
      body === undefined
        ? undefined
        : {
            method,
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
          }
    );
    const text = await response.text();
    if (response.ok) return { status: response.status, text };
    const refusal = parseStrictly(LegionGoErrorResponse, text);
    throw new LegionGoDaemonApiError(
      method,
      path,
      response.status,
      "value" in refusal
        ? refusal.value.error
        : `${JSON.stringify(text)} is not the Go daemon's refusal shape (${refusal.problem})`
    );
  };

  const read = async <T>(
    method: "GET" | "POST",
    path: string,
    schema: z.ZodType<T>,
    body?: object
  ): Promise<T> => {
    const { status, text } = await call(method, path, body);
    const parsed = parseStrictly(schema, text);
    if ("problem" in parsed) {
      throw new LegionGoDaemonContractError(method, path, status, parsed.problem);
    }
    return parsed.value;
  };

  /** The claim routes that answer 204 and nothing else. */
  const acknowledge = async (path: string, body: object): Promise<void> => {
    const { status, text } = await call("POST", path, body);
    if (status !== 204 || text !== "") {
      throw new LegionGoDaemonContractError(
        "POST",
        path,
        status,
        "the route answers 204 with no body"
      );
    }
  };

  return {
    state: () => read("GET", "/legion/v1/state", LegionGoStateResponse),
    register: (input) =>
      read("POST", "/legion/v1/claims/register", LegionGoRegisterResponse, input),
    ready: (input) => acknowledge("/legion/v1/claims/ready", input),
    exit: (input) => acknowledge("/legion/v1/claims/exit", input),
  };
}

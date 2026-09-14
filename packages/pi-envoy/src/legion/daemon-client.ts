import {
  type ControllerReadyInput,
  type DaemonStateResponse,
  type EscalateInput,
  type GatesRegisterInput,
  type GitHubTokenInput,
  type GitHubTokenResponse,
  type GrantInput,
  type GrantResponse,
  type IssueStatusInput,
  LegionDaemonApi,
  type ProcessExitInput,
  type ProcessReadyInput,
  type ProcessStartedInput,
  type ProcessStartedResponse,
  type ProvisioningCredentialInput,
  type ProvisioningCredentialResponse,
  type SpawnWorkerInput,
  type SpawnWorkerResponse,
  type WaveReleaseInput,
  type WaveReleaseResponse,
  type WorkerReadyInput,
  type WorkerSessionInput,
  type WorkerSessionResponse,
  type WorkerStartedInput,
  type WorkerStartedResponse,
} from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";

type ResponseSchema<T> = { parse(value: unknown): T };
/** The one call shape this client needs from a `fetch` — `typeof fetch` itself also carries
 * Bun's `preconnect`, which a per-call wrapper (`transportRetryingFetch`) has no reason to. */
type FetchCall = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

function defaultSleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

/** Waits between rejected spawn requests, sized for the daemon's restart window (boot to API
 * bind) as measured on the smoke rig (LEGION-102, step 10); adjust these delays if that window
 * is longer. */
export const SPAWN_WORKER_RETRY_DELAYS_MS = [2_000, 5_000] as const;

/** The initial request plus one retry per configured delay. */
export const SPAWN_WORKER_ATTEMPTS = SPAWN_WORKER_RETRY_DELAYS_MS.length + 1;

export interface LegionDaemonClient {
  readonly state: () => Promise<DaemonStateResponse>;
  readonly controllerReady: (input: ControllerReadyInput) => Promise<void>;
  readonly processStarted: (input: ProcessStartedInput) => Promise<ProcessStartedResponse>;
  readonly processReady: (input: ProcessReadyInput) => Promise<void>;
  readonly workerStarted: (input: WorkerStartedInput) => Promise<WorkerStartedResponse>;
  readonly workerReady: (input: WorkerReadyInput) => Promise<void>;
  readonly spawnWorker: (input: SpawnWorkerInput) => Promise<SpawnWorkerResponse>;
  readonly provisioningCredential: (
    input: ProvisioningCredentialInput
  ) => Promise<ProvisioningCredentialResponse>;
  readonly releaseWave: (input: WaveReleaseInput) => Promise<WaveReleaseResponse>;
  readonly escalate: (input: EscalateInput) => Promise<void>;
  readonly issueStatus: (input: IssueStatusInput) => Promise<void>;
  readonly gatesRegister: (input: GatesRegisterInput) => Promise<void>;
  readonly processExit: (input: ProcessExitInput) => Promise<void>;
  readonly grant: (input: GrantInput) => Promise<GrantResponse>;
  readonly githubToken: (input: GitHubTokenInput) => Promise<GitHubTokenResponse>;
  readonly workerSession: (input: WorkerSessionInput) => Promise<WorkerSessionResponse>;
}

export class LegionDaemonApiError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly responseBody: string
  ) {
    super(`${method} ${path} failed with ${status}: ${responseBody}`);
  }
}

/** `spawnWorker` got no response from the daemon in any attempt: the fetch itself rejected every
 * time (never an HTTP status — those are `LegionDaemonApiError` and never retried). The daemon may
 * still have received one of the attempts, which is why the message tells the caller to read
 * `legion state` before sending the task again. */
export class LegionDaemonTransportError extends Error {
  constructor(
    readonly path: string,
    readonly requestId: string,
    readonly attempts: number,
    cause: unknown
  ) {
    super(
      `POST ${path} got no response in ${attempts} attempts (request ${requestId}): ${messageFor(cause)}. The daemon may have received the request; read legion state (workerAdmission.queue and the role's claim) before sending it again.`,
      { cause }
    );
    this.name = "LegionDaemonTransportError";
  }
}

/** `spawnWorker` received response headers but could not read the body. This is not retried
 * because the fetch fulfilled, but the daemon may have accepted the request before the body
 * stream failed. */
export class LegionDaemonResponseReadError extends Error {
  constructor(readonly path: string, readonly requestId: string, cause: unknown) {
    super(
      `POST ${path} response body could not be read (request ${requestId}): ${messageFor(cause)}. The daemon may have received the request; read legion state (workerAdmission.queue and the role's claim) before sending it again.`,
      { cause }
    );
    this.name = "LegionDaemonResponseReadError";
  }
}

export interface LegionSessionRecovery {
  readonly recoveryToken: (sessionId: string) => string;
  readonly onRecovered?: (sessionId: string, session: WorkerSessionResponse) => void;
}

function isInvalidSessionSecret(error: unknown): error is LegionDaemonApiError {
  if (!(error instanceof LegionDaemonApiError) || error.status !== 403) return false;
  try {
    const body: unknown = JSON.parse(error.responseBody);
    return (
      typeof body === "object" &&
      body !== null &&
      "error" in body &&
      body.error === "Invalid session secret"
    );
  } catch {
    return error.responseBody === "Invalid session secret";
  }
}

function sessionCapability(
  body: object
): { readonly sessionId: string; readonly secret: string } | undefined {
  const candidate = body as { readonly sessionId?: unknown; readonly secret?: unknown };
  if (
    typeof candidate.sessionId !== "string" ||
    candidate.sessionId.length === 0 ||
    typeof candidate.secret !== "string" ||
    candidate.secret.length === 0
  ) {
    return undefined;
  }
  return { sessionId: candidate.sessionId, secret: candidate.secret };
}

const WORKER_SESSION_PATH = "/legion/v1/worker-session";

export function createLegionDaemonClient(
  baseUrl: string,
  fetchFn: typeof fetch = fetch,
  recovery?: LegionSessionRecovery,
  sleep: (ms: number) => Promise<void> = defaultSleep
): LegionDaemonClient {
  const endpoint = baseUrl.replace(/\/+$/, "");

  const postOnce = async <T>(
    path: string,
    body: object,
    schema: ResponseSchema<T>,
    fetchImpl: FetchCall = fetchFn,
    onResponseReadError?: (error: unknown) => Error
  ): Promise<T> => {
    const response = await fetchImpl(`${endpoint}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    let responseBody: string;
    try {
      responseBody = await response.text();
    } catch (error) {
      throw onResponseReadError?.(error) ?? error;
    }
    if (!response.ok) throw new LegionDaemonApiError("POST", path, response.status, responseBody);
    return schema.parse(JSON.parse(responseBody));
  };

  /** A `fetch` for one `spawn_worker` call that redials a rejected connection up to
   * `SPAWN_WORKER_ATTEMPTS` times with the same request id — the daemon's ledger dedupes it — and
   * hands any response, whatever its status, straight back: an HTTP error is never retried here,
   * and neither is a response-shape error from `schema.parse` (not a transport failure). The
   * attempt counter is per tool call, so the budget is three rejections even across a 403
   * recovery's retried request. */
  const transportRetryingFetch = (path: string, requestId: string): FetchCall => {
    let transportFailures = 0;
    return async (input, init) => {
      for (;;) {
        try {
          return await fetchFn(input, init);
        } catch (error) {
          transportFailures += 1;
          const delay = SPAWN_WORKER_RETRY_DELAYS_MS[transportFailures - 1];
          if (delay === undefined) {
            throw new LegionDaemonTransportError(path, requestId, transportFailures, error);
          }
          console.error(
            `[legion] POST ${path} attempt ${transportFailures}/${SPAWN_WORKER_ATTEMPTS} got no response (${messageFor(error)}); retrying request ${requestId} in ${delay} ms`
          );
          await sleep(delay);
        }
      }
    };
  };

  /** One record per session id: the newest secret a completed recovery returned, and the
   * recovery currently in flight, shared by every request refused while it runs. */
  interface SessionRecoveryRecord {
    latestSecret?: string;
    inFlight?: Promise<WorkerSessionResponse>;
  }
  const recoveries = new Map<string, SessionRecoveryRecord>();

  /** The secret a request refused with `refusedSecret` retries with: a newer secret already
   * recovered, else the result of the recovery in flight, else the one recovery this call starts.
   * `onRecovered` runs once per recovery, before the secret is recorded, so a throw there caches
   * nothing; a settled recovery clears the in-flight slot so the next refusal starts a new one. */
  const secretAfterRefusal = async (
    active: LegionSessionRecovery,
    sessionId: string,
    refusedSecret: string
  ): Promise<string> => {
    let record = recoveries.get(sessionId);
    if (record === undefined) {
      record = {};
      recoveries.set(sessionId, record);
    }
    if (record.latestSecret !== undefined && record.latestSecret !== refusedSecret) {
      return record.latestSecret;
    }
    if (record.inFlight === undefined) {
      const current = record;
      const inFlight = postOnce(
        WORKER_SESSION_PATH,
        { sessionId, recoveryToken: active.recoveryToken(sessionId) },
        LegionDaemonApi.WorkerSession.response
      )
        .then((recovered) => {
          active.onRecovered?.(sessionId, recovered);
          current.latestSecret = recovered.secret;
          return recovered;
        })
        .finally(() => {
          if (current.inFlight === inFlight) current.inFlight = undefined;
        });
      record.inFlight = inFlight;
    }
    return (await record.inFlight).secret;
  };

  const post = async <T>(
    path: string,
    body: object,
    schema: ResponseSchema<T>,
    fetchImpl: FetchCall = fetchFn,
    onResponseReadError?: (error: unknown) => Error
  ): Promise<T> => {
    try {
      return await postOnce(path, body, schema, fetchImpl, onResponseReadError);
    } catch (error) {
      const capability = sessionCapability(body);
      if (
        path === WORKER_SESSION_PATH ||
        recovery === undefined ||
        capability === undefined ||
        !isInvalidSessionSecret(error)
      ) {
        throw error;
      }
      const secret = await secretAfterRefusal(recovery, capability.sessionId, capability.secret);
      return await postOnce(path, { ...body, secret }, schema, fetchImpl, onResponseReadError);
    }
  };
  const get = async <T>(path: string, schema: ResponseSchema<T>): Promise<T> => {
    const response = await fetchFn(`${endpoint}${path}`);
    const responseBody = await response.text();
    if (!response.ok) throw new LegionDaemonApiError("GET", path, response.status, responseBody);
    return schema.parse(JSON.parse(responseBody));
  };

  const noContent = async <T>(
    path: string,
    body: object,
    schema: ResponseSchema<T>
  ): Promise<void> => {
    await post(path, body, schema);
  };

  return {
    state: () => get("/legion/v1/state", LegionDaemonApi.State.response),
    controllerReady: (input) =>
      noContent("/legion/v1/controller/ready", input, LegionDaemonApi.ControllerReady.response),
    processStarted: (input) =>
      post("/legion/v1/process/started", input, LegionDaemonApi.ProcessStarted.response),
    processReady: (input) =>
      noContent("/legion/v1/process/ready", input, LegionDaemonApi.ProcessReady.response),
    workerStarted: (input) =>
      post("/legion/v1/worker/started", input, LegionDaemonApi.WorkerStarted.response),
    workerReady: (input) =>
      noContent("/legion/v1/worker/ready", input, LegionDaemonApi.WorkerReady.response),
    spawnWorker: (input) =>
      post(
        "/legion/v1/worker/spawn",
        input,
        LegionDaemonApi.SpawnWorker.response,
        transportRetryingFetch("/legion/v1/worker/spawn", input.requestId),
        (error) =>
          new LegionDaemonResponseReadError("/legion/v1/worker/spawn", input.requestId, error)
      ),
    releaseWave: (input) =>
      post("/legion/v1/waves/release", input, LegionDaemonApi.WaveRelease.response),
    provisioningCredential: (input) =>
      post(
        "/legion/v1/provisioning-credential",
        input,
        LegionDaemonApi.ProvisioningCredential.response
      ),
    escalate: (input) => noContent("/legion/v1/escalate", input, LegionDaemonApi.Escalate.response),
    issueStatus: (input) =>
      noContent("/legion/v1/issues/status", input, LegionDaemonApi.IssueStatus.response),
    gatesRegister: (input) =>
      noContent("/legion/v1/gates/register", input, LegionDaemonApi.GatesRegister.response),
    workerSession: (input) =>
      post("/legion/v1/worker-session", input, LegionDaemonApi.WorkerSession.response),
    processExit: (input) =>
      noContent("/legion/v1/process/exit", input, LegionDaemonApi.ProcessExit.response),
    grant: (input) => post("/legion/v1/grants", input, LegionDaemonApi.Grant.response),
    githubToken: (input) =>
      post("/legion/v1/gh-token", input, LegionDaemonApi.GitHubToken.response),
  };
}

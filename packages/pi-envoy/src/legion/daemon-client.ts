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
  type MergeGateInput,
  type MergeGateResponse,
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

type ResponseSchema<T> = { parse(value: unknown): T };

export interface LegionDaemonClient {
  readonly state: () => Promise<DaemonStateResponse>;
  readonly controllerReady: (input: ControllerReadyInput) => Promise<void>;
  readonly processStarted: (input: ProcessStartedInput) => Promise<ProcessStartedResponse>;
  readonly processReady: (input: ProcessReadyInput) => Promise<void>;
  readonly workerStarted: (input: WorkerStartedInput) => Promise<WorkerStartedResponse>;
  readonly workerReady: (input: WorkerReadyInput) => Promise<void>;
  readonly spawnWorker: (input: SpawnWorkerInput) => Promise<SpawnWorkerResponse>;
  readonly mergeGate: (input: MergeGateInput) => Promise<MergeGateResponse>;
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
  recovery?: LegionSessionRecovery
): LegionDaemonClient {
  const endpoint = baseUrl.replace(/\/+$/, "");

  const postOnce = async <T>(path: string, body: object, schema: ResponseSchema<T>): Promise<T> => {
    const response = await fetchFn(`${endpoint}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const responseBody = await response.text();
    if (!response.ok) throw new LegionDaemonApiError("POST", path, response.status, responseBody);
    return schema.parse(JSON.parse(responseBody));
  };
  const post = async <T>(path: string, body: object, schema: ResponseSchema<T>): Promise<T> => {
    try {
      return await postOnce(path, body, schema);
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
      const recovered = await postOnce(
        WORKER_SESSION_PATH,
        {
          sessionId: capability.sessionId,
          recoveryToken: recovery.recoveryToken(capability.sessionId),
        },
        LegionDaemonApi.WorkerSession.response
      );
      recovery.onRecovered?.(capability.sessionId, recovered);
      return await postOnce(path, { ...body, secret: recovered.secret }, schema);
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
      post("/legion/v1/worker/spawn", input, LegionDaemonApi.SpawnWorker.response),
    mergeGate: (input) => post("/legion/v1/merge-gate", input, LegionDaemonApi.MergeGate.response),
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

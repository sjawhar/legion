import {
  type AdmissionResponse,
  type BacklogInput,
  type CommentResponse,
  type ControllerIssueInput,
  type ControllerReadyInput,
  type DaemonStateResponse,
  type DispatchThreadInput,
  type DispatchThreadResponse,
  type EscalateInput,
  type GitHubTokenInput,
  type GitHubTokenResponse,
  type GrantInput,
  type GrantResponse,
  type IssueCloseInput,
  type IssueCreateInput,
  type IssueCreateResponse,
  type IssueTextInput,
  type LabelsInput,
  type LabelsResponse,
  type MergeGateInput,
  type MergeGateResponse,
  LegionDaemonApi,
  type PhaseInput,
  type PhaseResponse,
  type ProcessExitInput,
  type ProcessReadyInput,
  type ProcessStartedInput,
  type ProcessStartedResponse,
  type ProvisioningCredentialInput,
  type ProvisioningCredentialResponse,
  type RoleBackingInput,
  type SpawnTokenInput,
  type SpawnTokenResponse,
  type WaveReleaseInput,
  type WaveReleaseResponse,
  type WorkerSessionInput,
  type WorkerSessionResponse,
} from "@legion/contracts";

type ResponseSchema<T> = { parse(value: unknown): T };

export interface LegionDaemonClient {
  readonly state: () => Promise<DaemonStateResponse>;
  readonly controllerReady: (input: ControllerReadyInput) => Promise<void>;
  readonly processStarted: (input: ProcessStartedInput) => Promise<ProcessStartedResponse>;
  readonly processReady: (input: ProcessReadyInput) => Promise<void>;
  readonly mergeGate: (input: MergeGateInput) => Promise<MergeGateResponse>;
  readonly issueCreate: (input: IssueCreateInput) => Promise<IssueCreateResponse>;
  readonly provisioningCredential: (
    input: ProvisioningCredentialInput
  ) => Promise<ProvisioningCredentialResponse>;
  readonly waveRelease: (input: WaveReleaseInput) => Promise<WaveReleaseResponse>;
  readonly comment: (input: IssueTextInput) => Promise<CommentResponse>;
  readonly postBody: (input: IssueTextInput) => Promise<void>;
  readonly labels: (input: LabelsInput) => Promise<LabelsResponse>;
  readonly escalate: (input: EscalateInput) => Promise<void>;
  readonly issueClose: (input: IssueCloseInput) => Promise<void>;
  readonly dispatchThread: (input: DispatchThreadInput) => Promise<DispatchThreadResponse>;
  readonly spawnToken: (input: SpawnTokenInput) => Promise<SpawnTokenResponse>;
  readonly roleBacking: (input: RoleBackingInput) => Promise<void>;
  readonly phase: (input: PhaseInput) => Promise<PhaseResponse>;
  readonly gatesApprove: (input: ControllerIssueInput) => Promise<void>;
  readonly admission: (input: ControllerIssueInput) => Promise<AdmissionResponse>;
  readonly backlog: (input: BacklogInput) => Promise<void>;
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

  const postOnce = async <T>(
    path: string,
    body: object,
    schema: ResponseSchema<T>
  ): Promise<T> => {
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
    mergeGate: (input) =>
      post("/legion/v1/merge-gate", input, LegionDaemonApi.MergeGate.response),
    issueCreate: (input) => post("/legion/v1/issues", input, LegionDaemonApi.IssueCreate.response),
    waveRelease: (input) =>
      post("/legion/v1/waves/release", input, LegionDaemonApi.WaveRelease.response),
    comment: (input) => post("/legion/v1/issues/comment", input, LegionDaemonApi.Comment.response),
    provisioningCredential: (input) =>
      post(
        "/legion/v1/provisioning-credential",
        input,
        LegionDaemonApi.ProvisioningCredential.response
      ),
    postBody: (input) =>
      noContent("/legion/v1/issues/body", input, LegionDaemonApi.PostBody.response),
    labels: (input) => post("/legion/v1/issues/labels", input, LegionDaemonApi.Labels.response),
    escalate: (input) => noContent("/legion/v1/escalate", input, LegionDaemonApi.Escalate.response),
    dispatchThread: (input) =>
      post("/legion/v1/dispatch-threads", input, LegionDaemonApi.DispatchThread.response),
    spawnToken: (input) =>
      post("/legion/v1/spawn-token", input, LegionDaemonApi.SpawnToken.response),
    roleBacking: (input) =>
      noContent("/legion/v1/role-backing", input, LegionDaemonApi.RoleBacking.response),
    issueClose: (input) =>
      noContent("/legion/v1/issues/close", input, LegionDaemonApi.IssueClose.response),
    phase: (input) => post("/legion/v1/phase", input, LegionDaemonApi.Phase.response),
    workerSession: (input) =>
      post("/legion/v1/worker-session", input, LegionDaemonApi.WorkerSession.response),
    gatesApprove: (input) =>
      noContent("/legion/v1/gates/approve", input, LegionDaemonApi.GatesApprove.response),
    admission: (input) => post("/legion/v1/admission", input, LegionDaemonApi.Admission.response),
    backlog: (input) => noContent("/legion/v1/backlog", input, LegionDaemonApi.Backlog.response),
    processExit: (input) =>
      noContent("/legion/v1/process/exit", input, LegionDaemonApi.ProcessExit.response),
    grant: (input) => post("/legion/v1/grants", input, LegionDaemonApi.Grant.response),
    githubToken: (input) =>
      post("/legion/v1/gh-token", input, LegionDaemonApi.GitHubToken.response),
  };
}

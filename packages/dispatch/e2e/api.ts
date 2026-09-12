import type {
  Actor,
  Artifact,
  ArtifactDetails,
  ArtifactUploadResponse,
  ArtifactVersionText,
  Ask,
  AskRead,
  Comment,
  CreateArtifactInput,
  CreateAskInput,
  CreateCommentInput,
  CreateMessageInput,
  CreateProjectInput,
  EditArtifactInput,
  EditAskInput,
  Event,
  Issue,
  IssueReferences,
  Message,
  MessageDelivery,
  Project,
  UpdateIssueInput,
  UserIssueState,
  Version,
} from "../web/src/api/types";

const e2ePort = process.env.DISPATCH_E2E_PORT || "8777";
const baseUrl = process.env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${e2ePort}`;
export const e2eAgentToken = process.env.E2E_AGENT_TOKEN ?? "e2e-token";
const seedActor: Actor = { kind: "session", id: "e2e-seed" };

interface ApiOptions {
  actor?: Actor;
  as?: "agent" | "user";
  login?: string;
}

async function request<T>(
  path: string,
  method: string,
  body?: object,
  options: ApiOptions = {}
): Promise<T> {
  const as = options.as ?? "user";
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const payload =
    body === undefined
      ? undefined
      : as === "agent"
        ? { ...body, actor: options.actor ?? seedActor }
        : body;

  if (as === "agent") {
    headers.Authorization = `Bearer ${e2eAgentToken}`;
  } else {
    headers["X-Dispatch-User"] = options.login ?? "alice";
  }

  const response = await fetch(new URL(path, baseUrl), {
    body: payload === undefined ? undefined : JSON.stringify(payload),
    headers,
    method,
  });

  if (!response.ok) {
    throw new Error(`${method} ${path} failed: ${response.status} ${await response.text()}`);
  }

  return (await response.json()) as T;
}

export function createProject(input: CreateProjectInput, login = "alice"): Promise<Project> {
  return request<Project>("/api/v1/projects", "POST", input, { login });
}

export function getIssue(key: string, options: ApiOptions = {}): Promise<Issue> {
  return request<Issue>(`/api/v1/issues/${encodeURIComponent(key)}`, "GET", undefined, options);
}

export function getIssueEvents(
  key: string,
  options: { after?: number; limit?: number } = {},
  requestOptions: ApiOptions = {}
): Promise<Event[]> {
  const query = new URLSearchParams();
  if (options.after !== undefined) {
    query.set("after", String(options.after));
  }
  if (options.limit !== undefined) {
    query.set("limit", String(options.limit));
  }
  const suffix = query.size === 0 ? "" : `?${query}`;
  return request<Event[]>(
    `/api/v1/issues/${encodeURIComponent(key)}/events${suffix}`,
    "GET",
    undefined,
    requestOptions
  );
}

export function patchIssue(
  key: string,
  input: UpdateIssueInput,
  options: ApiOptions = {}
): Promise<Issue> {
  return request<Issue>(`/api/v1/issues/${encodeURIComponent(key)}`, "PATCH", input, options);
}
export function createIssue(
  input: Partial<Pick<Issue, "project" | "title">> & {
    external?: string;
    parent?: string;
    spec?: string;
  },
  options: ApiOptions = {}
): Promise<Issue> {
  return request<Issue>("/api/v1/issues", "POST", input, options);
}

export function createAsk(
  issue: string,
  input: CreateAskInput,
  options: ApiOptions = {}
): Promise<Ask> {
  return request<Ask>(`/api/v1/issues/${encodeURIComponent(issue)}/asks`, "POST", input, options);
}

export function createProjectDocument(
  project: string,
  input: CreateArtifactInput,
  options: ApiOptions = {}
): Promise<ArtifactUploadResponse> {
  return request<ArtifactUploadResponse>(
    `/api/v1/projects/${encodeURIComponent(project)}/artifacts`,
    "POST",
    input,
    options
  );
}

export function createIssueArtifact(
  issue: string,
  input: CreateArtifactInput,
  options: ApiOptions = {}
): Promise<ArtifactUploadResponse> {
  return request<ArtifactUploadResponse>(
    `/api/v1/issues/${encodeURIComponent(issue)}/artifacts`,
    "POST",
    input,
    options
  );
}

export function listProjectArtifacts(
  project: string,
  options: ApiOptions = {}
): Promise<Artifact[]> {
  return request<Artifact[]>(
    `/api/v1/projects/${encodeURIComponent(project)}/artifacts`,
    "GET",
    undefined,
    options
  );
}

export function getProjectArtifact(
  project: string,
  slug: string,
  options: ApiOptions = {}
): Promise<ArtifactDetails> {
  return request<ArtifactDetails>(
    `/api/v1/projects/${encodeURIComponent(project)}/artifacts/${encodeURIComponent(slug)}`,
    "GET",
    undefined,
    options
  );
}
export function getIssueReferences(
  key: string,
  options: ApiOptions = {}
): Promise<IssueReferences> {
  return request<IssueReferences>(
    `/api/v1/issues/${encodeURIComponent(key)}/references`,
    "GET",
    undefined,
    options
  );
}

export function createArtifactAsk(
  artifactID: string,
  input: CreateAskInput,
  options: ApiOptions = {}
): Promise<Ask> {
  return request<Ask>(
    `/api/v1/artifacts/${encodeURIComponent(artifactID)}/asks`,
    "POST",
    input,
    options
  );
}

export function createArtifactComment(
  artifactID: string,
  input: CreateCommentInput,
  options: ApiOptions = {}
): Promise<Comment> {
  return request<Comment>(
    `/api/v1/artifacts/${encodeURIComponent(artifactID)}/comments`,
    "POST",
    input,
    options
  );
}

export function editAsk(id: string, input: EditAskInput, options: ApiOptions = {}): Promise<Ask> {
  return request<Ask>(`/api/v1/asks/${encodeURIComponent(id)}`, "PATCH", input, options);
}

export function resolveAsk(
  id: string,
  input: { kind: "retracted" | "resolved"; reason: string },
  options: ApiOptions = {}
): Promise<Ask> {
  return request<Ask>(`/api/v1/asks/${encodeURIComponent(id)}/resolve`, "POST", input, options);
}

export function createComment(
  issue: string,
  input: CreateCommentInput,
  options: ApiOptions = {}
): Promise<Comment> {
  return request<Comment>(
    `/api/v1/issues/${encodeURIComponent(issue)}/comments`,
    "POST",
    input,
    options
  );
}

export function listComments(
  issue: string,
  artifact: string | undefined = undefined,
  options: ApiOptions = {}
): Promise<Comment[]> {
  const suffix = artifact === undefined ? "" : `?artifact=${encodeURIComponent(artifact)}`;
  return request<Comment[]>(
    `/api/v1/issues/${encodeURIComponent(issue)}/comments${suffix}`,
    "GET",
    undefined,
    options
  );
}

export function getAsk(id: string, options: ApiOptions = {}): Promise<AskRead> {
  return request<AskRead>(`/api/v1/asks/${encodeURIComponent(id)}`, "GET", undefined, options);
}

export function getArtifact(id: string, options: ApiOptions = {}): Promise<Artifact> {
  return request<Artifact>(
    `/api/v1/artifacts/${encodeURIComponent(id)}`,
    "GET",
    undefined,
    options
  );
}

export function requestApproval(
  id: string,
  options: ApiOptions = {}
): Promise<{ ask: Ask; artifact_id: string; version: number }> {
  return request(
    `/api/v1/artifacts/${encodeURIComponent(id)}/approval-requests`,
    "POST",
    {},
    options
  );
}

export function createNamedVersion(
  id: string,
  summary: string,
  options: ApiOptions = {}
): Promise<Version> {
  return request<Version>(
    `/api/v1/artifacts/${encodeURIComponent(id)}/versions`,
    "POST",
    { summary },
    options
  );
}

export function getArtifactVersion(
  id: string,
  version: number,
  options: ApiOptions = {}
): Promise<ArtifactVersionText> {
  return request<ArtifactVersionText>(
    `/api/v1/artifacts/${encodeURIComponent(id)}/versions/${version}`,
    "GET",
    undefined,
    options
  );
}

export function editArtifact(
  id: string,
  input: EditArtifactInput,
  options: ApiOptions = {}
): Promise<{ applied: number }> {
  return request<{ applied: number }>(
    `/api/v1/artifacts/${encodeURIComponent(id)}/edits`,
    "POST",
    input,
    options
  );
}

export function acceptComment(id: string, options: ApiOptions = {}): Promise<Comment> {
  return request<Comment>(`/api/v1/comments/${encodeURIComponent(id)}/accept`, "POST", {}, options);
}

export function createMessage(
  issue: string,
  input: CreateMessageInput,
  options: ApiOptions = {}
): Promise<{ id: string }> {
  return request<{ id: string }>(
    `/api/v1/issues/${encodeURIComponent(issue)}/messages`,
    "POST",
    input,
    options
  );
}

export function replyToMessageDelivery(
  messageID: string,
  input: { attempt: number; body?: string; error?: string },
  actor: Actor
): Promise<Message | MessageDelivery> {
  return request<Message | MessageDelivery>(
    `/api/v1/messages/${encodeURIComponent(messageID)}/reply`,
    "POST",
    input,
    {
      actor,
      as: "agent",
    }
  );
}

export function putIssueState(
  key: string,
  input: Partial<UserIssueState>,
  options: ApiOptions = {}
): Promise<UserIssueState> {
  return request<UserIssueState>(
    `/api/v1/me/issues/${encodeURIComponent(key)}/state`,
    "PUT",
    input,
    options
  );
}

/**
 * Forces every open SSE connection on the server closed, as if it had restarted.
 * Test-only endpoint (mounted when DISPATCH_TEST_HOOKS=1, see run-server.sh) that
 * proves the client's reconnect-from-lastId path without seeding thousands of
 * events to trip the real replay cap.
 */
export function disconnectAllStreams(options: ApiOptions = {}): Promise<{ ok: boolean }> {
  return request<{ ok: boolean }>(
    "/api/v1/events/_test/disconnect",
    "POST",
    {},
    {
      as: "agent",
      ...options,
    }
  );
}

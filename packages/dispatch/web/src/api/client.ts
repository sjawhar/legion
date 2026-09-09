import type {
  AnswerAskInput,
  Artifact,
  ArtifactVersionContent,
  ArtifactVersionText,
  Ask,
  AuthenticatedUser,
  Comment,
  CreateArtifactInput,
  CreateAskInput,
  CreateCommentInput,
  CreateIssueInput,
  CreateMessageInput,
  CreateVersionInput,
  EditArtifactInput,
  Event,
  Issue,
  IssueSummary,
  Message,
  Project,
  UpdateIssueInput,
  UserIssueState,
  UserState,
  Version,
} from "./types";

export type FetchImplementation = (
  input: RequestInfo | URL,
  init?: RequestInit
) => Promise<Response>;

interface ErrorPayload {
  code?: string;
  error?: string;
}

export class ApiError extends Error {
  readonly code: string | undefined;
  readonly status: number;

  constructor(status: number, payload: ErrorPayload) {
    super(payload.error ?? `Dispatch request failed (${status})`);
    this.name = "ApiError";
    this.code = payload.code;
    this.status = status;
  }
}

export interface ListIssuesOptions {
  project?: string;
  status?: string;
  parent?: string;
}

export interface ListEventsOptions {
  after?: number;
  before?: number;
  ids?: string[];
  limit?: number;
  order?: "desc";
}

export interface CreateProjectInput {
  key: string;
  name: string;
}

interface UploadArtifactResponse {
  artifact: Artifact;
  version: Version;
}

function normalizeAsk(ask: Ask): Ask {
  return Array.isArray(ask.options) ? ask : { ...ask, options: [] };
}

function pathSegment(value: string): string {
  return encodeURIComponent(value);
}

function pathWithQuery(path: string, values: object): string {
  const query = new URLSearchParams();

  for (const [key, value] of Object.entries(values)) {
    if (typeof value === "string" || typeof value === "number") {
      query.set(key, String(value));
    }
  }

  const search = query.toString();
  return search ? `${path}?${search}` : path;
}

async function errorFrom(response: Response): Promise<ApiError> {
  const payload = (await response.json().catch(() => ({}))) as ErrorPayload;
  return new ApiError(response.status, payload);
}

export class DispatchApiClient {
  constructor(private readonly fetchImpl: FetchImplementation) {}

  private async response(path: string, init: RequestInit = {}): Promise<Response> {
    const response = await this.fetchImpl(path, {
      credentials: "same-origin",
      ...init,
    });

    if (!response.ok) {
      throw await errorFrom(response);
    }

    return response;
  }

  private async json<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await this.response(path, init);
    return (await response.json()) as T;
  }

  private send<T>(method: "PATCH" | "POST" | "PUT", path: string, body: unknown): Promise<T> {
    return this.json<T>(path, {
      body: JSON.stringify(body),
      headers: { "Content-Type": "application/json" },
      method,
    });
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return this.send<T>("POST", path, body);
  }

  getProjects(): Promise<Project[]> {
    return this.json<Project[]>("/api/v1/projects");
  }

  createProject(input: CreateProjectInput): Promise<Project> {
    return this.post<Project>("/api/v1/projects", input);
  }

  listIssues(options: ListIssuesOptions = {}): Promise<IssueSummary[]> {
    return this.json<IssueSummary[]>(pathWithQuery("/api/v1/issues", options));
  }

  createIssue(input: CreateIssueInput): Promise<Issue> {
    return this.post<Issue>("/api/v1/issues", input);
  }

  getIssue(key: string): Promise<Issue> {
    return this.json<Issue>(`/api/v1/issues/${pathSegment(key)}`);
  }

  patchIssue(key: string, input: UpdateIssueInput): Promise<Issue> {
    return this.send<Issue>("PATCH", `/api/v1/issues/${pathSegment(key)}`, input);
  }

  resolveIssue(ref: string): Promise<{ key: string }> {
    return this.json<{ key: string }>(pathWithQuery("/api/v1/issues/resolve", { ref }));
  }

  getIssueEvents(key: string, options: ListEventsOptions = {}): Promise<Event[]> {
    const { ids, ...query } = options;
    return this.json<Event[]>(
      pathWithQuery(`/api/v1/issues/${pathSegment(key)}/events`, { ...query, ids: ids?.join(",") })
    );
  }

  async getInbox(project?: string): Promise<Ask[]> {
    const asks = await this.json<Ask[]>(pathWithQuery("/api/v1/inbox", { project }));
    return asks.map(normalizeAsk);
  }

  async createAsk(key: string, input: CreateAskInput): Promise<Ask> {
    return normalizeAsk(await this.post<Ask>(`/api/v1/issues/${pathSegment(key)}/asks`, input));
  }

  async answerAsk(id: string, input: AnswerAskInput): Promise<Ask> {
    return normalizeAsk(await this.post<Ask>(`/api/v1/asks/${pathSegment(id)}/answer`, input));
  }

  async getAsk(id: string): Promise<Ask> {
    return normalizeAsk(await this.json<Ask>(`/api/v1/asks/${pathSegment(id)}`));
  }

  listComments(key: string, artifact?: string): Promise<Comment[]> {
    return this.json<Comment[]>(
      pathWithQuery(`/api/v1/issues/${pathSegment(key)}/comments`, { artifact })
    );
  }

  createComment(key: string, input: CreateCommentInput): Promise<Comment> {
    return this.post<Comment>(`/api/v1/issues/${pathSegment(key)}/comments`, input);
  }

  resolveComment(id: string): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/resolve`, {});
  }

  acceptComment(id: string): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/accept`, {});
  }

  rejectComment(id: string): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/reject`, {});
  }

  createMessage(key: string, input: CreateMessageInput): Promise<Message> {
    return this.post<Message>(`/api/v1/issues/${pathSegment(key)}/messages`, input);
  }

  listArtifacts(key: string): Promise<Artifact[]> {
    return this.json<Artifact[]>(`/api/v1/issues/${pathSegment(key)}/artifacts`);
  }

  async uploadArtifact(key: string, input: CreateArtifactInput): Promise<UploadArtifactResponse> {
    const body = new FormData();
    body.set("name", input.name);
    if (input.primary !== undefined) {
      body.set("primary", String(input.primary));
    }
    if (input.summary !== undefined) {
      body.set("summary", input.summary);
    }
    if (input.actor !== undefined) {
      body.set("actor", JSON.stringify(input.actor));
    }
    body.set("file", input.file);

    return this.json<UploadArtifactResponse>(`/api/v1/issues/${pathSegment(key)}/artifacts`, {
      body,
      method: "POST",
    });
  }

  makeArtifactPrimary(id: string): Promise<Issue> {
    return this.post<Issue>(`/api/v1/artifacts/${pathSegment(id)}/primary`, {});
  }

  getArtifact(id: string): Promise<Artifact> {
    return this.json<Artifact>(`/api/v1/artifacts/${pathSegment(id)}`);
  }

  getArtifactText(id: string): Promise<ArtifactVersionText> {
    return this.json<ArtifactVersionText>(`/api/v1/artifacts/${pathSegment(id)}/text`);
  }

  async getArtifactVersion(id: string, version: number): Promise<ArtifactVersionContent> {
    const response = await this.response(
      `/api/v1/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}`
    );
    const mime = response.headers.get("Content-Type") ?? "application/octet-stream";

    if (mime.includes("application/json")) {
      return (await response.json()) as ArtifactVersionText;
    }

    return { blob: await response.blob(), mime, version };
  }

  createArtifactVersion(id: string, input: CreateVersionInput): Promise<Version> {
    return this.post<Version>(`/api/v1/artifacts/${pathSegment(id)}/versions`, input);
  }

  editArtifact(
    id: string,
    input: EditArtifactInput
  ): Promise<{ applied: number; version?: Version }> {
    return this.post<{ applied: number; version?: Version }>(
      `/api/v1/artifacts/${pathSegment(id)}/edits`,
      input
    );
  }

  getMyState(): Promise<UserState> {
    return this.json<UserState>("/api/v1/me/state");
  }

  putIssueState(key: string, input: Partial<UserIssueState>): Promise<UserIssueState> {
    return this.send<UserIssueState>("PUT", `/api/v1/me/issues/${pathSegment(key)}/state`, input);
  }

  whoAmI(): Promise<AuthenticatedUser> {
    return this.json<AuthenticatedUser>("/auth/whoami");
  }

  logout(): Promise<{ ok: true }> {
    return this.post<{ ok: true }>("/auth/logout", {});
  }

  async githubRest(path: string, init: RequestInit = {}): Promise<Response> {
    return this.response(`/api/github/rest/${path.replace(/^\/+/, "")}`, init);
  }

  githubGraphql<T>(query: string, variables?: Record<string, unknown>): Promise<T> {
    return this.post<T>("/api/github/graphql", { query, variables });
  }

  health(): Promise<{ ok: boolean; db: boolean; nats: boolean | null }> {
    return this.json<{ ok: boolean; db: boolean; nats: boolean | null }>("/healthz");
  }
}

export function createApiClient(
  fetchImpl: FetchImplementation = (...args) => fetch(...args)
): DispatchApiClient {
  return new DispatchApiClient(fetchImpl);
}

export const api = createApiClient();

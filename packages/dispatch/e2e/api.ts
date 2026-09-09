import type { Actor, Ask, Comment, Issue, Project } from "../web/src/api/types";

const baseUrl = process.env.PLAYWRIGHT_BASE_URL ?? "http://127.0.0.1:8766";
const agentToken = process.env.E2E_AGENT_TOKEN ?? "e2e-token";
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
    headers.Authorization = `Bearer ${agentToken}`;
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

export function createProject(input: Project, login = "alice"): Promise<Project> {
  return request<Project>("/api/v1/projects", "POST", input, { login });
}

export function createIssue(
  input: Pick<Issue, "project" | "title"> & { spec?: string },
  options: ApiOptions = {}
): Promise<Issue> {
  return request<Issue>("/api/v1/issues", "POST", input, options);
}

export function createAsk(
  issue: string,
  input: Pick<Ask, "question">,
  options: ApiOptions = {}
): Promise<Ask> {
  return request<Ask>(`/api/v1/issues/${encodeURIComponent(issue)}/asks`, "POST", input, options);
}

export function createComment(
  issue: string,
  input: Pick<Comment, "body">,
  options: ApiOptions = {}
): Promise<Comment> {
  return request<Comment>(
    `/api/v1/issues/${encodeURIComponent(issue)}/comments`,
    "POST",
    input,
    options
  );
}

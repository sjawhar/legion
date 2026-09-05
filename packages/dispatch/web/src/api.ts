import { collectAnswers, collectAsks, openAsks } from "./asks";
import { parseThreadMarker } from "./markers";
import type { CloseReason, Comment, Issue, IssueState, Thread, Urgency } from "./types";

interface GraphqlResponse<T> {
  data?: T;
  errors?: Array<{ message: string }>;
}

interface SearchResponse {
  search: {
    nodes: GraphqlThreadNode[];
  };
}

interface GraphqlCommentNode {
  databaseId: number;
  body: string;
  createdAt: string;
  updatedAt: string;
  author?: { login: string } | null;
}

interface GraphqlThreadNode {
  number: number;
  title: string;
  body: string;
  state: string;
  updatedAt: string;
  createdAt: string;
  author?: { login: string } | null;
  comments?: { totalCount: number; nodes?: Array<GraphqlCommentNode | null> | null } | null;
  parent?: { number: number } | null;
  repository: { owner: { login: string }; name: string };
}

interface RestIssueResponse {
  number: number;
  title: string;
  body?: string | null;
  state: string;
  state_reason?: string | null;
  updated_at: string;
  created_at: string;
  user?: { login: string } | null;
}

interface RestCommentResponse {
  id: number;
  body?: string | null;
  updated_at: string;
  created_at: string;
  user?: { login: string } | null;
}

export interface GithubEventData {
  subject: string;
  repo: string;
  payload: unknown;
}

/**
 * The SSE stream is JSON off the wire; an event missing `subject` or `repo`
 * would otherwise reach the router and blow up on a string method.
 */
function isGithubEventData(value: unknown): value is GithubEventData {
  if (typeof value !== "object" || value === null) return false;
  if (!("subject" in value) || typeof value.subject !== "string") return false;
  return "repo" in value && typeof value.repo === "string";
}

export interface SseRouterHandlers {
  refetchSidebar: () => void | Promise<void>;
  refetchComments: (repo: string, threadNumber: number) => void | Promise<void>;
  refetchIssue: (repo: string, threadNumber: number) => void | Promise<void>;
  highlightThread: (repo: string, threadNumber: number) => void;
}

function normalizeState(value: string): IssueState {
  return value.toUpperCase() === "CLOSED" ? "CLOSED" : "OPEN";
}

async function githubGraphql<T>(query: string, variables: Record<string, unknown>): Promise<T> {
  const response = await fetch("/api/github/graphql", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ query, variables }),
  });
  if (!response.ok) throw new Error(`GitHub GraphQL request failed: ${response.status}`);
  const body = (await response.json()) as GraphqlResponse<T>;
  if (body.errors?.length) throw new Error(body.errors.map((error) => error.message).join("; "));
  if (!body.data) throw new Error("GitHub GraphQL response missing data");
  return body.data;
}

async function githubRest<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`/api/github/rest/${path}`, init);
  if (!response.ok) throw new Error(`GitHub REST request failed: ${response.status}`);
  return (await response.json()) as T;
}

function jsonRequest(method: "POST" | "PATCH", body: unknown): RequestInit {
  return {
    method,
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  };
}

function commentFromResponse(comment: RestCommentResponse): Comment {
  return {
    id: comment.id,
    body: comment.body ?? "",
    updatedAt: comment.updated_at,
    createdAt: comment.created_at,
    authorLogin: comment.user?.login ?? "unknown",
  };
}

function threadFromNode(node: GraphqlThreadNode): Thread {
  const meta = parseThreadMarker(node.body);
  const parentNumber = node.parent?.number ?? node.number;
  const windowComments: Comment[] = (node.comments?.nodes ?? []).flatMap((comment) =>
    comment
      ? [
          {
            id: comment.databaseId,
            body: comment.body,
            createdAt: comment.createdAt,
            updatedAt: comment.updatedAt,
            authorLogin: comment.author?.login ?? "unknown",
          },
        ]
      : []
  );
  const asks = collectAsks(node.body, windowComments);
  const thread: Thread = {
    repo: `${node.repository.owner.login}/${node.repository.name}`,
    number: node.number,
    title: node.title,
    body: node.body,
    state: normalizeState(node.state),
    urgency: meta?.urgency ?? "med",
    openAskCount: openAsks(asks, collectAnswers(windowComments)).length,
    parentNumber,
    updatedAt: node.updatedAt,
    createdAt: node.createdAt,
    authorLogin: node.author?.login ?? "unknown",
    commentCount: node.comments?.totalCount ?? 0,
  };
  if (meta?.origin) thread.origin = meta.origin;
  return thread;
}

interface InstallationsResponse {
  installations: Array<{ account?: { login?: string } | null } | null>;
}

// Distinct owner logins across every Envoy App installation the signed-in
// user can see. These are the accounts (users or orgs) the owner-scoped
// dashboard search spans — there is no separate watched-repos config.
// GitHub pages this endpoint at 30 by default; 100 is its maximum.
export async function getInstallationOwners(): Promise<string[]> {
  const response = await fetch("/api/installations?per_page=100");
  if (!response.ok) throw new Error(`GET /api/installations failed: ${response.status}`);
  const data = (await response.json()) as InstallationsResponse;
  const owners = new Set<string>();
  for (const installation of data.installations ?? []) {
    const login = installation?.account?.login;
    if (login) owners.add(login);
  }
  return [...owners];
}

export async function searchDispatchThreads(owners: string[]): Promise<Thread[]> {
  if (owners.length === 0) return [];
  const query = `
    query SearchDispatchThreads($search: String!) {
      search(query: $search, type: ISSUE, first: 100) {
        nodes {
          ... on Issue {
            number
            title
            body
            state
            updatedAt
            createdAt
            author { login }
            comments(last: 30) {
              totalCount
              nodes { databaseId body createdAt updatedAt author { login } }
            }
            parent { number }
            repository { owner { login } name }
          }
        }
      }
    }
  `;
  const search = [
    "is:issue",
    "is:open",
    "label:dispatch-thread",
    ...owners.map((owner) => `user:${owner}`),
  ].join(" ");
  const data = await githubGraphql<SearchResponse>(query, { search });
  return data.search.nodes.filter((node) => parseThreadMarker(node.body)).map(threadFromNode);
}

export async function getIssue(repo: string, number: number): Promise<Issue> {
  const issue = await githubRest<RestIssueResponse>(`repos/${repo}/issues/${number}`);
  return {
    repo,
    number: issue.number,
    title: issue.title,
    body: issue.body ?? "",
    state: normalizeState(issue.state),
    stateReason: issue.state_reason ?? null,
    updatedAt: issue.updated_at,
    createdAt: issue.created_at,
    authorLogin: issue.user?.login ?? "unknown",
  };
}

/** Title of an issue or pull request for unfurling; null when it cannot be read (private, deleted, network). */
export async function getReferenceTitle(repo: string, number: number): Promise<string | null> {
  try {
    const issue = await githubRest<{ title?: string }>(`repos/${repo}/issues/${number}`);
    return issue.title ?? null;
  } catch {
    return null;
  }
}

export async function getComments(repo: string, number: number): Promise<Comment[]> {
  const comments = await githubRest<RestCommentResponse[]>(
    `repos/${repo}/issues/${number}/comments`
  );
  return comments.map(commentFromResponse);
}

export async function postComment(repo: string, number: number, body: string): Promise<Comment> {
  const comment = await githubRest<RestCommentResponse>(
    `repos/${repo}/issues/${number}/comments`,
    jsonRequest("POST", { body })
  );
  return commentFromResponse(comment);
}

const CLOSE_REASONS = ["completed", "not_planned"] as const satisfies readonly CloseReason[];

export function isCloseReason(value: unknown): value is CloseReason {
  return CLOSE_REASONS.some((reason) => reason === value);
}

export async function closeIssue(
  repo: string,
  number: number,
  stateReason: CloseReason
): Promise<Issue> {
  const issue = await githubRest<RestIssueResponse>(
    `repos/${repo}/issues/${number}`,
    jsonRequest("PATCH", { state: "closed", state_reason: stateReason })
  );
  return {
    repo,
    number: issue.number,
    title: issue.title,
    body: issue.body ?? "",
    state: normalizeState(issue.state),
    stateReason: issue.state_reason ?? null,
    updatedAt: issue.updated_at,
    createdAt: issue.created_at,
    authorLogin: issue.user?.login ?? "unknown",
  };
}

export function extractIssueNumberFromSubject(subject: string): number | null {
  const match = subject.match(/\.issue\.(\d+)(?:\.|$)/);
  return match ? Number(match[1]) : null;
}

export function createSseRouter(handlers: SseRouterHandlers): (event: GithubEventData) => void {
  return (event) => {
    const number = extractIssueNumberFromSubject(event.subject);
    if (!number || !event.repo) return;
    // Comment events: refetch the conversation for that thread.
    if (event.subject.endsWith(".comment")) {
      void handlers.refetchComments(event.repo, number);
      handlers.highlightThread(event.repo, number);
      return;
    }
    // Sub-issue link events: refetch the sidebar to pick up the new edge.
    if (event.subject.endsWith(".sub_issue")) {
      void handlers.refetchSidebar();
      handlers.highlightThread(event.repo, number);
      return;
    }
    // All other issue events (bare, closed, reopened, labeled, edited, …)
    // refetch BOTH the issue (state/labels/body) AND the sidebar (the open/
    // closed filter may now include/exclude this thread). Previously dropped
    // *.issue.N.closed and .reopened entirely, leaving the sidebar stale.
    void handlers.refetchIssue(event.repo, number);
    void handlers.refetchSidebar();
    handlers.highlightThread(event.repo, number);
  };
}

export function openGithubEventSource(handlers: SseRouterHandlers): EventSource {
  const router = createSseRouter(handlers);
  const source = new EventSource("/api/events");
  source.addEventListener("github_event", (event) => {
    const data: unknown = JSON.parse(event.data);
    if (isGithubEventData(data)) router(data);
  });
  return source;
}

export const urgencyWeights: Record<Urgency, number> = {
  low: 1,
  med: 2,
  high: 3,
  blocking: 4,
};

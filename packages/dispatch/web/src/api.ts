import { parseMetaMarker } from "./markers";
import type { Comment, Issue, IssueState, Thread, Urgency } from "./types";

interface GraphqlResponse<T> {
  data?: T;
  errors?: Array<{ message: string }>;
}

interface SearchResponse {
  search: {
    nodes: GraphqlThreadNode[];
  };
}

interface GraphqlThreadNode {
  number: number;
  title: string;
  body: string;
  state: IssueState;
  updatedAt: string;
  createdAt: string;
  author?: { login: string } | null;
  comments?: { totalCount: number } | null;
  parent?: { number: number } | null;
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

export interface SseRouterHandlers {
  refetchSidebar: () => void | Promise<void>;
  refetchComments: (repo: string, threadNumber: number) => void | Promise<void>;
  refetchIssue: (repo: string, threadNumber: number) => void | Promise<void>;
  highlightThread: (repo: string, threadNumber: number) => void;
}

function normalizeState(value: string): IssueState {
  return value.toUpperCase() === "CLOSED" ? "CLOSED" : "OPEN";
}

function repoParts(repo: string): { owner: string; name: string } {
  const [owner, name] = repo.split("/");
  if (!owner || !name) throw new Error(`Invalid repo slug: ${repo}`);
  return { owner, name };
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

function threadFromNode(repo: string, node: GraphqlThreadNode): Thread {
  const meta = parseMetaMarker(node.body);
  const parentNumber = node.parent?.number ?? node.number;
  return {
    repo,
    number: node.number,
    title: node.title,
    body: node.body,
    state: normalizeState(node.state),
    urgency: meta?.urgency ?? "med",
    hasAsk: Boolean(meta?.ask?.length),
    parentNumber,
    updatedAt: node.updatedAt,
    createdAt: node.createdAt,
    authorLogin: node.author?.login ?? "unknown",
    commentCount: node.comments?.totalCount ?? 0,
  };
}

export async function searchDispatchThreads(repo: string): Promise<Thread[]> {
  const { owner, name } = repoParts(repo);
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
            comments { totalCount }
            parent { number }
          }
        }
      }
    }
  `;
  const data = await githubGraphql<SearchResponse>(query, {
    search: `repo:${owner}/${name} label:dispatch-thread is:issue is:open`,
  });
  return data.search.nodes
    .filter((node) => parseMetaMarker(node.body))
    .map((node) => threadFromNode(repo, node));
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

export async function closeIssue(
  repo: string,
  number: number,
  stateReason: "completed" | "not_planned"
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
    router(JSON.parse(event.data) as GithubEventData);
  });
  return source;
}

export const urgencyWeights: Record<Urgency, number> = {
  low: 1,
  med: 2,
  high: 3,
  blocking: 4,
};

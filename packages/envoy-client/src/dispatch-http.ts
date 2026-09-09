import type { ASK_URGENCIES, DOC_EDIT_OPS } from "@legion/contracts";
import type { DispatchHost } from "./dispatch-cwd";

/** Ask urgency and document edit operations come from the shared tool contract. */
export type AskUrgency = (typeof ASK_URGENCIES)[number];
export type DocEditOp = (typeof DOC_EDIT_OPS)[number];

export type Actor =
  | { readonly kind: "user"; readonly id: string }
  | {
      readonly kind: "session";
      readonly id: string;
      readonly origin?: {
        readonly host?: DispatchHost;
        readonly machine?: string;
        readonly cwd?: string;
        readonly tmux?: string;
        readonly pane?: string;
        readonly session_title?: string;
      };
    };

export interface Anchor {
  readonly artifact_id: string;
  readonly version: number;
  readonly quote: string;
  readonly from: number;
  readonly to: number;
  readonly orphaned: boolean;
}

export interface Version {
  readonly number: number;
  readonly named: boolean;
  readonly summary: string | null;
  readonly authors: Actor[];
  readonly created_at: string;
  readonly size?: number;
  readonly mime?: string;
  readonly sha256?: string;
}

export interface Artifact {
  readonly id: string;
  readonly issue_key: string;
  readonly slug: string;
  readonly name: string;
  readonly kind: "doc" | "image" | "file";
  readonly primary: boolean;
  readonly created_by: Actor;
  readonly created_at: string;
  readonly versions: Version[];
}

export interface Issue {
  readonly key: string;
  readonly project: string;
  readonly number: number;
  readonly title: string;
  readonly status: string;
  readonly labels: string[];
  readonly parent: string | null;
  readonly external_links: {
    readonly url: string;
    readonly kind?: "github_issue" | "github_pr" | "url";
  }[];
  readonly route: string | null;
  readonly created_by: Actor;
  readonly created_at: string;
  readonly updated_at: string;
  readonly closed_at: string | null;
  readonly primary_artifact_id: string;
  readonly last_seq: number;
}

export interface IssueSummary
  extends Pick<Issue, "key" | "title" | "status" | "parent" | "updated_at"> {
  readonly open_asks: number;
}

export interface Ask {
  readonly id: string;
  readonly issue_key: string;
  readonly author: Actor;
  readonly question: string;
  readonly options: { readonly label: string; readonly description?: string }[];
  readonly multiple: boolean;
  readonly custom: boolean;
  readonly urgency: AskUrgency;
  readonly anchor: Anchor | null;
  readonly state: "open" | "answered";
  readonly answer: {
    /** Answering user's login: the server stores the acting actor's id, not an actor object. */
    readonly user: string;
    readonly selected: string[];
    readonly text: string | null;
    readonly at: string;
  } | null;
  readonly created_at: string;
}

export interface Comment {
  readonly id: string;
  readonly issue_key: string;
  readonly author: Actor;
  readonly body: string;
  readonly anchor: Anchor | null;
  readonly reply_to: string | null;
  readonly resolved: boolean;
  readonly suggestion: { readonly replace_with: string; readonly accepted: boolean | null } | null;
  readonly created_at: string;
}

export interface Message {
  readonly id: string;
  readonly issue_key: string;
  readonly author: Actor;
  readonly body: string;
  readonly created_at: string;
}

export type EventType =
  | "issue.created"
  | "issue.updated"
  | "issue.closed"
  | "artifact.created"
  | "artifact.version"
  | "ask.opened"
  | "ask.answered"
  | "comment.created"
  | "comment.resolved"
  | "suggestion.accepted"
  | "suggestion.rejected"
  | "message.created"
  | "child.status";

export interface Event {
  readonly id: number;
  readonly issue_key: string;
  readonly seq: number;
  readonly type: EventType;
  readonly actor: Actor;
  readonly notify: boolean;
  readonly created_at: string;
  /** Per-type JSON object; the shapes are validated where they are read (see delivery.ts). */
  readonly payload: Record<string, unknown>;
}

export interface IssueDetails extends Issue {
  readonly artifacts: Artifact[];
  readonly open_asks: Ask[];
  readonly children: { readonly key: string; readonly title: string; readonly status: string }[];
}
export interface CommentRead {
  readonly comment: Comment;
  readonly replies: Comment[];
}

export interface IssueRead {
  readonly issue: IssueDetails;
  readonly events: Event[];
}

export interface ArtifactText {
  readonly markdown: string;
  readonly version: number | null;
}

export interface TargetCandidate {
  readonly from: number;
  readonly to: number;
  readonly context: string;
}

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[];
}

export class DispatchServiceError extends Error {
  override readonly name = "DispatchServiceError";

  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
    readonly candidates?: TargetCandidate[]
  ) {
    super(message);
  }
}

export interface CreateIssueInput {
  readonly project: string;
  readonly title: string;
  readonly parent?: string;
  readonly external?: string;
  readonly spec?: string;
  readonly actor: Actor;
}

export interface AskInput {
  readonly question: string;
  readonly options?: { readonly label: string; readonly description?: string }[];
  readonly multiple?: boolean;
  readonly custom?: boolean;
  readonly urgency?: AskUrgency;
  readonly anchor?: {
    readonly artifact: string;
    readonly quote: string;
    readonly occurrence?: number;
  };
  readonly actor: Actor;
}

export interface CommentInput {
  readonly body: string;
  readonly anchor?: {
    readonly artifact: string;
    readonly quote: string;
    readonly occurrence?: number;
  };
  readonly reply_to?: string;
  readonly suggestion?: { readonly replace_with: string };
  readonly actor: Actor;
}

export interface ArtifactUploadInput {
  readonly name: string;
  readonly file: Blob;
  readonly primary?: boolean;
  readonly summary?: string;
  readonly actor: Actor;
}

export interface EditOperation {
  readonly op: DocEditOp;
  readonly find?: string;
  readonly with?: string;
  readonly occurrence?: number;
  readonly markdown?: string;
  readonly after?: string;
  readonly before?: string;
}

function asErrorShape(value: unknown): DispatchServiceErrorShape {
  return typeof value === "object" && value !== null ? (value as DispatchServiceErrorShape) : {};
}

function isJson(response: Response): boolean {
  return response.headers.get("content-type")?.includes("application/json") ?? false;
}

/** JSON HTTP client for Dispatch's native-tool API. */
export class DispatchClient {
  readonly #baseUrl: string;
  readonly #resolvedIssues = new Map<string, Promise<string>>();

  constructor(
    baseUrl: string,
    readonly token: string,
    readonly fetchImpl: typeof fetch = fetch
  ) {
    this.#baseUrl = baseUrl.replace(/\/+$/, "");
  }

  async issue(input: CreateIssueInput): Promise<Issue> {
    return this.#json("POST", ["api", "v1", "issues"], input);
  }

  async getIssue(issue: string): Promise<IssueDetails> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue)]);
  }

  async getIssueEvents(issue: string, after = 0, limit = 200): Promise<Event[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "events"],
      undefined,
      { after, limit }
    );
  }
  async read(issueReference: string): Promise<IssueRead> {
    const issueKey = await this.#resolveIssue(issueReference);
    const issue = await this.getIssue(issueKey);
    const events = await this.getIssueEvents(issueKey, Math.max(0, issue.last_seq - 10), 10);
    return { issue, events };
  }

  async ask(issue: string, input: AskInput): Promise<Ask> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      input
    );
  }

  async comment(issue: string, input: CommentInput): Promise<Comment> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      input
    );
  }

  async suggest(
    issue: string,
    input: Omit<CommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Comment> {
    const { replace_with, ...comment } = input;
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      {
        ...comment,
        suggestion: { replace_with },
      }
    );
  }

  async message(
    issue: string,
    input: { readonly body: string; readonly actor: Actor }
  ): Promise<Message> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "messages"],
      input
    );
  }

  async artifact(
    issue: string,
    input: ArtifactUploadInput
  ): Promise<{ artifact: Artifact; version: Version }> {
    const form = new FormData();
    form.set("name", input.name);
    if (input.primary !== undefined) form.set("primary", String(input.primary));
    if (input.summary !== undefined) form.set("summary", input.summary);
    form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"],
      form
    );
  }

  async getArtifact(id: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "artifacts", id]);
  }

  async docRead(id: string, version?: number): Promise<ArtifactText> {
    return version === undefined
      ? this.#json("GET", ["api", "v1", "artifacts", id, "text"])
      : this.#json("GET", ["api", "v1", "artifacts", id, "versions", String(version)]);
  }

  async docEdit(
    id: string,
    input: { readonly ops: EditOperation[]; readonly summary?: string; readonly actor: Actor }
  ): Promise<{ applied: number; version: Version | null }> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "edits"], input);
  }

  async nameArtifactVersion(
    id: string,
    input: { readonly summary: string; readonly actor: Actor }
  ): Promise<Version> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "versions"], input);
  }

  async getAsk(id: string): Promise<Ask> {
    return this.#json("GET", ["api", "v1", "asks", id]);
  }

  async getComment(id: string): Promise<CommentRead> {
    return this.#json("GET", ["api", "v1", "comments", id]);
  }

  async getComments(issue: string, artifact?: string): Promise<Comment[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      undefined,
      artifact ? { artifact } : undefined
    );
  }

  async ensureIssue(issueReference: string, actor: Actor): Promise<string> {
    if (!issueReference.includes("#")) return issueReference;
    try {
      return await this.#resolveIssue(issueReference);
    } catch (error) {
      if (!(error instanceof DispatchServiceError) || error.status !== 404) throw error;
    }
    const created = await this.#json<Issue>("POST", ["api", "v1", "issues"], {
      external: issueReference,
      actor,
    });
    this.#resolvedIssues.set(issueReference, Promise.resolve(created.key));
    return created.key;
  }

  async #resolveIssue(issueReference: string): Promise<string> {
    if (!issueReference.includes("#")) return issueReference;
    let resolved = this.#resolvedIssues.get(issueReference);
    if (!resolved) {
      resolved = this.#json<{ key: string }>("GET", ["api", "v1", "issues", "resolve"], undefined, {
        ref: issueReference,
      }).then((resolution) => resolution.key);
      this.#resolvedIssues.set(issueReference, resolved);
    }
    try {
      return await resolved;
    } catch (error) {
      this.#resolvedIssues.delete(issueReference);
      throw error;
    }
  }
  async #json<T>(
    method: string,
    path: readonly string[],
    body?: unknown,
    query?: Record<string, string | number>
  ): Promise<T> {
    const headers: Record<string, string> = {
      Accept: "application/json",
      Authorization: `Bearer ${this.token}`,
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const response = await this.fetchImpl(this.#url(path, query), {
      method,
      headers,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    return this.#response<T>(response);
  }

  async #form<T>(method: string, path: readonly string[], body: FormData): Promise<T> {
    const response = await this.fetchImpl(this.#url(path), {
      method,
      headers: { Accept: "application/json", Authorization: `Bearer ${this.token}` },
      body,
    });
    return this.#response<T>(response);
  }

  #url(path: readonly string[], query?: Record<string, string | number>): string {
    const url = new URL(
      `${path.map((segment) => encodeURIComponent(segment)).join("/")}`,
      `${this.#baseUrl}/`
    );
    if (query) {
      for (const [name, value] of Object.entries(query)) url.searchParams.set(name, String(value));
    }
    return url.toString();
  }

  async #response<T>(response: Response): Promise<T> {
    const text = await response.text();
    let payload: unknown = text;
    if (text && isJson(response)) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = text;
      }
    }
    if (!response.ok) {
      const error = asErrorShape(payload);
      throw new DispatchServiceError(
        error.code ?? `HTTP_${response.status}`,
        response.status,
        error.error ?? (typeof payload === "string" && payload ? payload : response.statusText),
        error.candidates
      );
    }
    return payload as T;
  }
}

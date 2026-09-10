import type {
  Actor,
  ArtifactDetails,
  ArtifactText,
  ArtifactUploadResponse,
  ArtifactVersionText,
  Ask,
  AskRead,
  Comment,
  CommentRead,
  CreateArtifactInput,
  CreateAskInput,
  CreateCommentInput,
  CreateIssueInput,
  CreateMessageInput,
  CreateVersionInput,
  DispatchServiceErrorShape,
  EditArtifactInput,
  EditArtifactResponse,
  Event,
  Issue,
  IssueDetails,
  IssueRead,
  IssueSummary,
  Message,
  TargetCandidate,
  Version,
} from "@legion/contracts";

export type { DispatchServiceErrorShape } from "@legion/contracts";

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

function asErrorShape(value: unknown): DispatchServiceErrorShape {
  return typeof value === "object" && value !== null ? (value as DispatchServiceErrorShape) : {};
}

function isJson(response: Response): boolean {
  return response.headers.get("content-type")?.includes("application/json") ?? false;
}

export interface ListIssuesOptions {
  readonly project?: string;
  readonly status?: string;
  readonly parent?: string;
  readonly updated_since?: string;
}

/** JSON HTTP client for Dispatch's native-tool API. */
export class DispatchClient {
  readonly #baseUrl: string;
  readonly #resolvedIssues = new Map<string, Promise<string>>();
  readonly #creatingIssues = new Map<string, Promise<string>>();

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

  async listIssues(options: ListIssuesOptions = {}): Promise<IssueSummary[]> {
    return this.#json("GET", ["api", "v1", "issues"], undefined, options);
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

  async ask(issue: string, input: CreateAskInput): Promise<Ask> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      input
    );
  }

  async comment(issue: string, input: CreateCommentInput): Promise<Comment> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      input
    );
  }

  async suggest(
    issue: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
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

  async message(issue: string, input: CreateMessageInput): Promise<Message> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "messages"],
      input
    );
  }

  async artifact(issue: string, input: CreateArtifactInput): Promise<ArtifactUploadResponse> {
    const artifactPath = ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"];
    if ("content" in input) return this.#json("POST", artifactPath, input);

    const form = new FormData();
    form.set("name", input.name);
    if (input.primary !== undefined) form.set("primary", String(input.primary));
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form("POST", artifactPath, form);
  }

  async getArtifact(id: string): Promise<ArtifactDetails> {
    return this.#json("GET", ["api", "v1", "artifacts", id]);
  }

  async docRead(id: string): Promise<ArtifactText>;
  async docRead(id: string, version: number): Promise<ArtifactVersionText>;
  async docRead(id: string, version?: number): Promise<ArtifactText | ArtifactVersionText>;
  async docRead(id: string, version?: number): Promise<ArtifactText | ArtifactVersionText> {
    return version === undefined
      ? this.#json("GET", ["api", "v1", "artifacts", id, "text"])
      : this.#json("GET", ["api", "v1", "artifacts", id, "versions", String(version)]);
  }

  async docEdit(id: string, input: EditArtifactInput): Promise<EditArtifactResponse> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "edits"], input);
  }

  async nameArtifactVersion(id: string, input: CreateVersionInput): Promise<Version> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "versions"], input);
  }

  async getAsk(id: string): Promise<AskRead> {
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

    let creating = this.#creatingIssues.get(issueReference);
    if (!creating) {
      creating = this.#createExternalIssue(issueReference, actor);
      this.#creatingIssues.set(issueReference, creating);
    }
    try {
      return await creating;
    } finally {
      if (this.#creatingIssues.get(issueReference) === creating) {
        this.#creatingIssues.delete(issueReference);
      }
    }
  }

  async #createExternalIssue(issueReference: string, actor: Actor): Promise<string> {
    try {
      const created = await this.#json<Issue>("POST", ["api", "v1", "issues"], {
        external: issueReference,
        actor,
      });
      this.#resolvedIssues.set(issueReference, Promise.resolve(created.key));
      return created.key;
    } catch (error) {
      if (error instanceof DispatchServiceError && (error.status === 409 || error.status === 500)) {
        return this.#resolveIssue(issueReference);
      }
      throw error;
    }
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
    query?: object
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

  #url(path: readonly string[], query?: object): string {
    const url = new URL(
      `${path.map((segment) => encodeURIComponent(segment)).join("/")}`,
      `${this.#baseUrl}/`
    );
    if (query) {
      for (const [name, value] of Object.entries(query)) {
        if (typeof value === "string" || typeof value === "number") {
          url.searchParams.set(name, String(value));
        }
      }
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

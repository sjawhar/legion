import type {
  AcceptedMessageDelivery,
  Actor,
  Advised,
  Agent,
  ArchitectureSource,
  Artifact,
  ArtifactApproval,
  ArtifactBlock,
  ArtifactReferences,
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
  EditAskInput,
  Event,
  GraphEdgeKind,
  GraphReferences,
  Issue,
  IssueDetails,
  IssuePriority,
  IssueRead,
  IssueReferences,
  IssueRouteStatus,
  IssueSummaryPage,
  Message,
  MessageRead,
  MessageReplyInput,
  MessageReplyResult,
  OpenAsksResponse,
  ResolveAskInput,
  SearchResponse,
  UpdateIssueInput,
  Version,
  WhoamiResponse,
} from "@legion/contracts";

export type { DispatchServiceErrorShape } from "@legion/contracts";

export class DispatchServiceError extends Error {
  override readonly name = "DispatchServiceError";

  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
    readonly candidates?: DispatchServiceErrorShape["candidates"],
    readonly current?: DispatchServiceErrorShape["current"],
    readonly mismatches?: DispatchServiceErrorShape["mismatches"]
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
  readonly label?: string;
  /** Each value repeats as `priority=`; `"none"` matches an issue with no priority. */
  readonly priority?: readonly (IssuePriority | "none")[];
  readonly updated_since?: string;
  /** Only open issues whose route is in this state. */
  readonly route_status?: IssueRouteStatus;
}

/** The page `listIssuePage` asks for: `limit` issues from position `offset` of the listing. */
export interface IssuePageRequest {
  readonly limit: number;
  readonly offset: number;
}

export interface SearchOptions {
  readonly project?: string;
  readonly limit?: number;
}

/** One node's edges: `to` reads what points at it, `from` what it points to; exactly one. */
export type GraphReferencesQuery = ({ readonly to: string } | { readonly from: string }) & {
  readonly kind?: readonly GraphEdgeKind[];
  /** events.id; keeps mentions introduced after it and excludes structural edges. */
  readonly since?: number;
};

export const DISPATCH_TOOL_DEADLINE_MS = 60_000;

function requestSignal(signal: AbortSignal | undefined): AbortSignal {
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  return signal === undefined ? deadline : AbortSignal.any([signal, deadline]);
}

/** What `listIssuePage` got in place of the page it asked for, for its refusal. */
function notAPage(answer: unknown): string {
  if (Array.isArray(answer)) {
    return (
      `a bare array of ${answer.length} entries, the unpaged listing: a Dispatch older than ` +
      "sjawhar/legion#1612, which pages it, or a regression of that change"
    );
  }
  if (answer === null) return "null";
  if (typeof answer !== "object") return `a ${typeof answer}`;
  const fields = answer as Partial<Record<keyof IssueSummaryPage, unknown>>;
  const lacking = [
    ...(Array.isArray(fields.issues) ? [] : ["an issues array"]),
    ...(["total", "limit", "offset"] as const)
      .filter((name) => typeof fields[name] !== "number")
      .map((name) => `a numeric ${name}`),
  ];
  return `an object without ${lacking.join(" or ")}`;
}

/** JSON HTTP client for Dispatch's native-tool API. */
export class DispatchClient {
  readonly #baseUrl: string;
  readonly #resolvedIssues = new Map<string, Promise<string>>();
  readonly #signal: AbortSignal;

  constructor(
    baseUrl: string,
    readonly token: string,
    readonly fetchImpl: typeof fetch = fetch,
    signal?: AbortSignal
  ) {
    this.#baseUrl = baseUrl.replace(/\/+$/, "");
    this.#signal = requestSignal(signal);
  }

  async issue(input: CreateIssueInput): Promise<Advised<Issue>> {
    return this.#json("POST", ["api", "v1", "issues"], input);
  }

  /** Resolves an existing native key or external reference without creating an issue. */
  async resolveIssue(issueReference: string): Promise<string> {
    return this.#resolveIssue(issueReference);
  }

  /**
   * One page of `GET /api/v1/issues?limit=&offset=`, used as Dispatch served it: `IssueSummaryPage`
   * (sjawhar/legion#1612). Any other answer is refused with what arrived; a bare array is the
   * unpaged listing, which a Dispatch older than that change or a regression of it answers.
   */
  async listIssuePage(
    options: ListIssuesOptions,
    page: IssuePageRequest
  ): Promise<IssueSummaryPage> {
    const answer: unknown = await this.#json("GET", ["api", "v1", "issues"], undefined, {
      ...options,
      limit: page.limit,
      offset: page.offset,
    });
    const served = answer as Partial<IssueSummaryPage> | null;
    if (
      typeof served === "object" &&
      served !== null &&
      Array.isArray(served.issues) &&
      typeof served.total === "number" &&
      typeof served.limit === "number" &&
      typeof served.offset === "number"
    ) {
      return served as IssueSummaryPage;
    }
    throw new Error(
      `GET /api/v1/issues?limit=${page.limit}&offset=${page.offset} asked for a page ` +
        `({issues, total, limit, offset}) and got ${notAPage(answer)}`
    );
  }

  async listProjectArtifacts(project: string, unlinked = false): Promise<Artifact[]> {
    return this.#json(
      "GET",
      ["api", "v1", "projects", project, "artifacts"],
      undefined,
      unlinked ? { unlinked: "true" } : undefined
    );
  }

  async projectArtifact(
    project: string,
    input: CreateArtifactInput
  ): Promise<ArtifactUploadResponse> {
    const artifactPath = ["api", "v1", "projects", project, "artifacts"];
    if ("content" in input) return this.#json("POST", artifactPath, input);

    const form = new FormData();
    form.set("name", input.name);
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form("POST", artifactPath, form);
  }

  async getProjectArtifact(project: string, slug: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "projects", project, "artifacts", slug]);
  }

  async search(query: string, options: SearchOptions = {}): Promise<SearchResponse> {
    return this.#json("GET", ["api", "v1", "search"], undefined, { q: query, ...options });
  }

  async getIssue(issue: string): Promise<IssueDetails> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue)]);
  }

  /** `PATCH /api/v1/issues/{key}`: the server replaces every field the body names. */
  async updateIssue(issue: string, input: UpdateIssueInput): Promise<Advised<Issue>> {
    return this.#json("PATCH", ["api", "v1", "issues", await this.#resolveIssue(issue)], input);
  }

  /** `POST /api/v1/issues/{key}/claim`: this session takes the issue. 409 ISSUE_CLAIMED when a
   *  running session, or any human, holds it; 409 CLAIM_CONTENDED when the holder changed twice
   *  while the request ran, so nothing was applied. */
  async claimIssue(issue: string, input: { readonly actor?: Actor } = {}): Promise<Issue> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
  }

  /** `DELETE /api/v1/issues/{key}/claim`: give up the claim, or clear one whose session is
   *  gone. The status does not move. It answers the same two 409s, and has no force. */
  async releaseIssueClaim(issue: string, input: { readonly actor?: Actor } = {}): Promise<Issue> {
    return this.#json(
      "DELETE",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
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

  async ask(issue: string, input: CreateAskInput): Promise<Advised<Ask>> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      input
    );
  }

  async openAsks(sessionID: string, since?: string): Promise<OpenAsksResponse> {
    return this.#json("GET", ["api", "v1", "asks", "open"], undefined, {
      author_session: sessionID,
      ...(since === undefined ? {} : { since }),
    });
  }

  /** Every open ask on a project's issues and documents (issue- or document-owned), oldest first. */
  async openAsksForProject(project: string): Promise<OpenAsksResponse> {
    return this.#json("GET", ["api", "v1", "asks", "open"], undefined, { project });
  }

  /** `GET /api/v1/agents`: the Envoy listener's live sessions, for naming a session by the
   *  title it is running under rather than the one it stamped on an old write. */
  async listAgents(): Promise<Agent[]> {
    return this.#json("GET", ["api", "v1", "agents"]);
  }

  async whoami(): Promise<WhoamiResponse> {
    return this.#json("GET", ["api", "v1", "whoami"]);
  }

  /** `POST /api/v1/projects/{key}/architecture-source/sync`: import the
   *  project's architecture model now; the row's `last_error` says whether the
   *  import replaced the model or the previous one stays up. */
  async syncArchitectureSource(project: string): Promise<ArchitectureSource> {
    return this.#json("POST", ["api", "v1", "projects", project, "architecture-source", "sync"]);
  }

  /** `GET /api/v1/projects/{key}/architecture-source`: the configured source row, or `null`
   *  when the project has none. Having none is an answer, not a failure: a current server says
   *  `200 null`, an older one `404 SOURCE_NOT_FOUND`, and a client meets both while a rollout
   *  mixes versions. Any other failure, a different 404 included, throws. */
  async getArchitectureSource(project: string): Promise<ArchitectureSource | null> {
    try {
      return await this.#json("GET", ["api", "v1", "projects", project, "architecture-source"]);
    } catch (error) {
      if (
        error instanceof DispatchServiceError &&
        error.status === 404 &&
        error.code === "SOURCE_NOT_FOUND"
      ) {
        return null;
      }
      throw error;
    }
  }

  async resolveAsk(id: string, input: ResolveAskInput): Promise<Ask> {
    return this.#json("POST", ["api", "v1", "asks", id, "resolve"], input);
  }

  /**
   * Opens an approval ask for a document at its latest version, its question the document, the
   * version and `summary`. An open ask at that version is returned unchanged; one naming an older
   * version is retracted and replaced. When that version is already approved, `ask` is null and
   * `approval` carries the standing approval.
   */
  async requestApproval(
    artifactID: string,
    input: { actor: Actor; summary: string }
  ): Promise<{
    ask: Ask | null;
    artifact_id: string;
    version: number;
    approval: ArtifactApproval;
  }> {
    return this.#json("POST", ["api", "v1", "artifacts", artifactID, "approval-requests"], input);
  }

  async editAsk(id: string, input: EditAskInput): Promise<Ask> {
    return this.#json("PATCH", ["api", "v1", "asks", id], input);
  }

  async comment(issue: string, input: CreateCommentInput): Promise<Advised<Comment>> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      input
    );
  }

  /** Every ask on an issue (all states unless narrowed). */
  async listIssueAsks(issue: string, state?: "all" | "open" | "answered"): Promise<Ask[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      undefined,
      state === undefined ? undefined : { state }
    );
  }

  async getArtifactAsks(id: string, state?: "all" | "open" | "answered"): Promise<Ask[]> {
    return this.#json(
      "GET",
      ["api", "v1", "artifacts", id, "asks"],
      undefined,
      state === undefined ? undefined : { state }
    );
  }

  async artifactAsk(id: string, input: CreateAskInput): Promise<Advised<Ask>> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "asks"], input);
  }

  async suggest(
    issue: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Advised<Comment>> {
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

  async getArtifactComments(id: string): Promise<Comment[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "comments"]);
  }

  async artifactComment(id: string, input: CreateCommentInput): Promise<Advised<Comment>> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "comments"], input);
  }

  async artifactSuggest(
    id: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Advised<Comment>> {
    const { replace_with, ...comment } = input;
    return this.#json("POST", ["api", "v1", "artifacts", id, "comments"], {
      ...comment,
      suggestion: { replace_with },
    });
  }

  async message(issue: string, input: CreateMessageInput): Promise<Advised<Message>> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "messages"],
      input
    );
  }

  /**
   * `POST /api/v1/messages/{id}/reply`: the targeted session's reply to the delivery it
   * received. It takes no issue, so it is the route that answers a human's direct message -
   * a conversation Dispatch keeps without one - and the server settles the delivery attempt
   * with the reply it inserts. `followUp` asks to post more once the attempt is answered
   * (`?follow_up=true`); without it an answered attempt hands back its stored reply and posts
   * nothing. The result's `duplicate` says the route posted nothing.
   */
  async messageReply(
    id: string,
    input: MessageReplyInput,
    options: { readonly followUp?: boolean } = {}
  ): Promise<MessageReplyResult> {
    return this.#json(
      "POST",
      ["api", "v1", "messages", id, "reply"],
      input,
      options.followUp === true ? { follow_up: "true" } : undefined
    );
  }

  async getMessage(issue: string, id: string): Promise<MessageRead> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "messages",
      id,
    ]);
  }

  /** `GET /api/v1/messages/{id}`: the conversation a message belongs to, by any message id in
   *  it - the root and every reply. It takes no issue, so it reads a human's direct message to
   *  a session and the replies to it, which belong to none. It reads as `session`; which
   *  threads a session may read is the route's rule (`GET /api/v1` describes it). */
  async getMessageThread(id: string, session: string): Promise<MessageRead> {
    return this.#json("GET", ["api", "v1", "messages", id], undefined, { session });
  }

  /** `POST /api/v1/messages/{id}/deliveries/{attempt}/accept`: this session records that it took
   *  that attempt of the message as its user's own turn, and gets back the attempt with the
   *  message's stored body, which is what it injects. Dispatch allows one acceptance per message,
   *  of a person's own direct Send or Aside to this session (no issue, no broadcast), of its
   *  latest attempt, which a person asked for within the last minute and which did not fail; any
   *  refusal throws a `DispatchServiceError` naming the check. */
  async acceptMessageDelivery(
    id: string,
    attempt: number,
    input: { readonly actor: Actor }
  ): Promise<AcceptedMessageDelivery> {
    return this.#json(
      "POST",
      ["api", "v1", "messages", id, "deliveries", String(attempt), "accept"],
      input
    );
  }

  async artifact(
    issue: string,
    input: CreateArtifactInput
  ): Promise<
    ArtifactUploadResponse & { readonly artifact: Artifact & { readonly issue_key: string } }
  > {
    const artifactPath = ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"];
    if ("content" in input) return this.#json("POST", artifactPath, input);

    const form = new FormData();
    form.set("name", input.name);
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form("POST", artifactPath, form);
  }

  async getArtifact(id: string): Promise<Artifact> {
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

  async artifactBlocks(id: string): Promise<ArtifactBlock[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "blocks"]);
  }

  async docEdit(id: string, input: EditArtifactInput): Promise<EditArtifactResponse> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "edits"], input);
  }

  async nameArtifactVersion(id: string, input: CreateVersionInput): Promise<Version> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "versions"], input);
  }

  async getArtifactEvents(id: string, after = 0, limit = 200): Promise<Event[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "events"], undefined, { after, limit });
  }

  async getArtifactReferences(id: string): Promise<ArtifactReferences> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "references"]);
  }

  async getAsk(id: string): Promise<AskRead> {
    return this.#json("GET", ["api", "v1", "asks", id]);
  }

  /** A bearer follows only its own session: the body names it and the path repeats it. */
  async followAsk(id: string, sessionId: string, actor: Actor): Promise<void> {
    await this.#json("PUT", ["api", "v1", "asks", id, "followers", sessionId], { actor });
  }

  async unfollowAsk(id: string, sessionId: string, actor: Actor): Promise<void> {
    await this.#json("DELETE", ["api", "v1", "asks", id, "followers", sessionId], { actor });
  }

  async getComment(id: string): Promise<CommentRead> {
    return this.#json("GET", ["api", "v1", "comments", id]);
  }

  /** Resolves a comment thread as `actor`; the server takes no reason and any actor may resolve. */
  async resolveComment(id: string, actor: Actor): Promise<Comment> {
    return this.#json("POST", ["api", "v1", "comments", id, "resolve"], { actor });
  }

  async getComments(issue: string, artifact?: string): Promise<Comment[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      undefined,
      artifact ? { artifact } : undefined
    );
  }

  async getIssueReferences(issue: string): Promise<IssueReferences> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "references",
    ]);
  }

  async getReferences(query: GraphReferencesQuery): Promise<GraphReferences> {
    return this.#json("GET", ["api", "v1", "references"], undefined, {
      ...("to" in query ? { to: query.to } : { from: query.from }),
      ...(query.kind === undefined || query.kind.length === 0
        ? {}
        : { kind: query.kind.join(",") }),
      ...(query.since === undefined ? {} : { since: query.since }),
    });
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
      signal: this.#signal,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    return this.#response<T>(response);
  }

  async #form<T>(method: string, path: readonly string[], body: FormData): Promise<T> {
    const response = await this.fetchImpl(this.#url(path), {
      method,
      headers: { Accept: "application/json", Authorization: `Bearer ${this.token}` },
      body,
      signal: this.#signal,
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
        } else if (Array.isArray(value)) {
          for (const item of value) url.searchParams.append(name, String(item));
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
        error.candidates,
        error.current,
        error.mismatches
      );
    }
    return payload as T;
  }
}

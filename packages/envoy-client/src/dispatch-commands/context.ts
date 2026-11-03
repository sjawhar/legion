import type {
  Actor,
  Artifact,
  ArtifactUploadResponse,
  CreateArtifactInput,
  IssueComponentsInput,
  IssueComponentsMode,
  IssueDetails,
  IssuePriority,
} from "@legion/contracts";
import type { ActiveDispatchConfig } from "../dispatch-config";
import type { DispatchHost, ExecFn } from "../dispatch-cwd";
import { DispatchClient, type DispatchWriteRetry } from "../dispatch-http";
import type { Owner, ParsedDispatchRef } from "../dispatch-owner";
import type { AgentArtifactRef, LocalPicture, PictureTextLimit } from "../dispatch-picture-tools";
import type { ToolImage } from "../dispatch-pictures";

/**
 * Tool arguments as the model supplied them. The tool's Zod schema validates
 * them before use; the named keys are the ones the executor inspects itself,
 * everything else is forwarded to the API unchanged.
 */
export type ToolArguments = {
  readonly urgency?: unknown;
  readonly ref?: unknown;
  readonly issue?: unknown;
  readonly project?: unknown;
  readonly artifact?: unknown;
  readonly version?: unknown;
  readonly anchor?: unknown;
  readonly options?: unknown;
  readonly labels?: unknown;
  readonly blocked_by?: unknown;
  readonly external_links?: unknown;
  readonly ops?: unknown;
  readonly in_reply_to?: unknown;
  readonly message?: unknown;
  readonly images?: unknown;
} & Record<string, unknown>;

export type ExecutorEnvironment = {
  readonly LEGION_ISSUE?: string;
} & Record<string, string | undefined>;

export interface ExecuteDispatchToolInput {
  readonly tool: string;
  readonly args: ToolArguments;
  readonly cwd: string;
  /**
   * The bytes of an upload's `--path` file as the CLI already read them for AskGate's check, sent
   * in place of a fresh read, so the upload sends exactly the bytes that check digested.
   */
  readonly uploadBytes?: Uint8Array;
  readonly host: DispatchHost;
  readonly sessionId?: string;
  readonly sessionTitle?: string;
  /** The resolved Dispatch (`activeDispatchSettings`): the server, the bearer it takes, and the
   *  public dashboard its printed links point at when there is one. */
  readonly config: Pick<ActiveDispatchConfig, "url" | "token" | "publicUrl">;
  /** Host cancellation for the currently executing tool call. */
  readonly signal?: AbortSignal;
  readonly env?: ExecutorEnvironment;
  readonly fetchImpl?: typeof fetch;
  /** How each write asks Dispatch again under its key (`DISPATCH_WRITE_RETRY` unless set). */
  readonly writeRetry?: DispatchWriteRetry;
  readonly exec?: ExecFn;
  /** The topics the session's Envoy subscriptions hold, or null when they cannot be read (Envoy
   *  off, or no answer from its listener); called only by a write that would name its owner as
   *  unsubscribed. A write names the owner the session is not subscribed to only from this. */
  readonly subscriptions?: () => Promise<readonly string[] | null>;
}

export interface DispatchToolResult {
  readonly text: string;
  readonly details: Record<string, unknown>;
  /** Pictures the model is shown after the text, in the order the text names them. */
  readonly images?: readonly ToolImage[];
  /** What the caller should hear beside the result rather than in it: the dispatch CLI writes
   *  each line to stderr. The text already says what the reader needs. */
  readonly warnings?: readonly string[];
}

export interface ResolvedArtifact {
  readonly owner: Owner;
  readonly issue?: IssueDetails;
  readonly artifact: Artifact;
}
export interface OwnerResolution {
  readonly args: ToolArguments;
  readonly ref: ParsedDispatchRef | null;
  readonly owner: Owner | null;
  /** The conversation upload a `dispatch://agent/...` ref names; it has no issue or project. */
  readonly agentArtifact?: AgentArtifactRef;
}
export interface CheckedCall {
  /** Every problem the check found; the call is refused once, listing them all. */
  readonly problems: string[];
  readonly owner: OwnerResolution;
  /** The arguments as the schema parsed them (the owner-resolved ones when it refused them). */
  readonly args: ToolArguments;
  readonly pictures: LocalPicture[];
}

/** What the pre-send check reads: the call's arguments, its checkout and environment, and the
 *  Dispatch server's addresses, whose dashboard URLs a `ref` may be (none when the session has no
 *  Dispatch configured, which only a dry run checks with). */
export interface DispatchCallCheckInput
  extends Pick<ExecuteDispatchToolInput, "tool" | "args" | "cwd" | "env" | "exec"> {
  readonly serverUrls: readonly string[];
}
export function stringArg(args: Record<string, unknown>, name: string): string {
  const value = args[name];
  if (typeof value !== "string") throw new Error(`${name} is required`);
  return value;
}

export function optionalString(args: Record<string, unknown>, name: string): string | undefined {
  const value = args[name];
  return typeof value === "string" ? value : undefined;
}

export function optionalBoolean(args: Record<string, unknown>, name: string): boolean | undefined {
  const value = args[name];
  return typeof value === "boolean" ? value : undefined;
}

export function optionalNumber(args: Record<string, unknown>, name: string): number | undefined {
  const value = args[name];
  return typeof value === "number" ? value : undefined;
}

/** `priority` as the server's tri-state: absent leaves it, null clears it, 0–3 set it. */
export function optionalPriority(
  args: Record<string, unknown>,
  name: string
): IssuePriority | null | undefined {
  const value = args[name];
  if (value === null) return null;
  // The zod spec already refused anything but an integer 0–3.
  return typeof value === "number" ? (value as IssuePriority) : undefined;
}

/** A `priority` filter list as the issue list takes it: null, meaning no priority, is `none`. */
export function optionalPriorityFilter(
  args: Record<string, unknown>,
  name: string
): (IssuePriority | "none")[] | undefined {
  const value = args[name];
  // The zod spec already refused anything but a list of integers 0–3 and null.
  return Array.isArray(value)
    ? (value as (IssuePriority | null)[]).map((item) => item ?? "none")
    : undefined;
}

/** The `components` argument as the server takes it; the zod spec already checked its shape. */
export function optionalComponents(
  args: Record<string, unknown>,
  name: string
): IssueComponentsInput | undefined {
  const value = args[name];
  if (typeof value !== "object" || value === null) return undefined;
  const input = value as { mode: IssueComponentsMode; ids?: string[]; reason?: string };
  return {
    mode: input.mode,
    ...(input.ids === undefined ? {} : { ids: input.ids }),
    ...(input.reason === undefined ? {} : { reason: input.reason }),
  };
}
/** `value` as a JSON object (not null, not an array), or null for anything else. */
export function asObject(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

/**
 * What every command is handed: the call as `checkDispatchCall` left it, and the Dispatch this
 * session speaks to. A command that answers before the origin is resolved (`dispatch_open_asks`,
 * `dispatch_answers`, `dispatch_whoami`) takes this alone, so it starts no subprocess.
 */
export interface SessionContext {
  readonly input: ExecuteDispatchToolInput;
  /** The arguments as the tool's schema parsed them. */
  readonly args: ToolArguments;
  /** The owner the check resolved, and the arguments its ref filled. */
  readonly ownerArguments: OwnerResolution;
  /** The pictures the call's `images` argument names, read from disk by the check. */
  readonly pictures: LocalPicture[];
  /** The API's own address, which an unreachable server is named by. */
  readonly configUrl: string;
  /**
   * Every link a command prints is to Dispatch's dashboard as people open it: the public URL when
   * the session has one (a pod reaches Dispatch at an in-cluster address nobody else can open),
   * and the API's URL otherwise.
   */
  readonly linkBase: string;
  /** This host's own session id, trimmed; a command that needs one refuses an empty one itself. */
  readonly hostSession: string | undefined;
  /**
   * A factory, not one instance: the constructor starts the request deadline, and the main path
   * resolves the origin (a subprocess) before it needs a client.
   */
  newClient(): DispatchClient;
}

/**
 * What a command that acts for this session is handed: the session's own context, the client and
 * actor its writes carry, the owner its arguments name, and the pictures helpers a write posts
 * with. `commandContext` builds it once per call, after the origin is resolved.
 */
export interface CommandContext extends SessionContext {
  readonly client: DispatchClient;
  readonly actor: Actor;
  /** The call's owner, an issue reference resolved to the issue's key. */
  readonly owner: Owner | null;
  /** The owner, refused when the call names none. */
  documentOwner(): Owner;
  /** The document `reference` names under `owner`, read as the call's own reference reads it. */
  resolveDocument(owner: Owner, reference: string | undefined): Promise<ResolvedArtifact>;
  /** The issue the call acts on, refused when its owner is not an issue. */
  issue(): string;
  /**
   * The text a call posts with its pictures, each uploaded first to the owner the address names.
   * An upload is a step before the call's own write, so one that landed without its answer leaves
   * the call undone: it fails, rather than reading as the call's landing.
   */
  postWithPictures(
    text: string,
    limit: PictureTextLimit,
    owner: string,
    upload: (upload: CreateArtifactInput) => Promise<ArtifactUploadResponse>
  ): Promise<string>;
  /** An ask or comment uploads to the project of the project document it is on, else to its issue. */
  writePictures(
    resolved: ResolvedArtifact | undefined,
    text: string,
    limit: PictureTextLimit
  ): Promise<string>;
  /**
   * The pictures this host session was already shown: `dispatch_read` names them instead of
   * sending them again, and it and `dispatch_doc_read` add the ones their result shows.
   */
  readonly shown: Set<string> | undefined;
}

/** One Dispatch command: the whole of what `dispatch <command>` does, after the check passed. */
export type DispatchCommand = (context: CommandContext) => Promise<DispatchToolResult>;

/**
 * The context every command starts from, built from the checked call. The fetch it hands the
 * client names the configured server in a transport error, since a session whose Dispatch moved
 * reads that message and nothing else.
 */
export function sessionContext(
  input: ExecuteDispatchToolInput,
  checked: CheckedCall
): SessionContext {
  const { url: configUrl, token: configToken } = input.config;
  const baseFetch = input.fetchImpl ?? fetch;
  const fetchImpl = Object.assign(
    async (...args: Parameters<typeof fetch>): Promise<Response> => {
      try {
        return await baseFetch(...args);
      } catch (error) {
        if (error instanceof TypeError) {
          throw new Error(
            `Dispatch at ${configUrl} is unreachable: ${error.message}. If the Dispatch URL changed, restart this agent process so it picks up the new configuration.`
          );
        }
        throw error;
      }
    },
    { preconnect: baseFetch.preconnect }
  );
  return {
    input,
    args: checked.args,
    ownerArguments: checked.owner,
    pictures: checked.pictures,
    configUrl,
    linkBase: input.config.publicUrl ?? configUrl,
    hostSession: input.sessionId?.trim(),
    newClient: () =>
      new DispatchClient(configUrl, configToken, fetchImpl, input.signal, input.writeRetry),
  };
}

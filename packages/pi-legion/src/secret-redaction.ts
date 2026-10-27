import { readFileSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import { messageFor } from "@legion/envoy-client/errors";
import type {
  ContentBlock,
  SessionContext,
  ToolResultEvent,
  ToolResultEventResult,
} from "@legion/pi-shared/pi-types";
import { classifySession } from "./classify";

// Every tool result a Legion session's agent gets — a `read` of `/proc/self/environ`, `env` in
// bash, a token file it opened — is written into the session's transcript and sent to the model.
// The agent's process environment holds credentials: the shim exports the provider keys into it
// (`--provider-env-dir`, packages/daemon/internal/shim/config.go), since Oh My Pi reads them from
// there. So before Oh My Pi keeps a result, the Legion entry's tool_result hook replaces every
// value of that environment that may be a credential with a marker naming its variable.
//
// Which values: every value of a variable that is not one of `PUBLIC_VARIABLES` is taken for a
// credential once it is 16 characters long, whatever its name; this fails closed for a credential
// nobody named. A credential-shaped name (the daemon's own rule, runtime.IsSecretLikeName) lowers
// that floor to 8. A value naming an existing path is never a credential, but the token a `*_FILE`
// variable's file holds is. The password of a URL is one under any name.
//
// What it misses, by design (src/secret-redaction.test.ts holds each): a credential this process's
// environment does not hold (a file no `*_FILE` variable names, a command's output such as
// `gh auth token`, another process's environment); one shorter than the floors; one transformed on
// its way out (encoded, sliced, split); and anything Oh My Pi writes without a tool result.

/** The variables whose values a tool result shows as they are: Legion's own, which the runtimes
 * set on every pane and pod and none of which holds a credential (a secret reaches a process as a
 * file behind a `*_FILE` pointer), and the host's ordinary ones. A variable joins with the process
 * that sets it named in a comment, never as a prefix or a pattern. Everything else 16 characters
 * or longer is masked: a variable the runtime starts setting is masked in every tool result until
 * it joins here. */
export const PUBLIC_VARIABLES: Readonly<Record<string, true>> = {
  // Identity, locale, terminal and directories: the tmux runtime's pane allow-list
  // (packages/daemon/internal/runtime/tmux/environment.go) and the shell's own.
  HOME: true,
  USER: true,
  LOGNAME: true,
  SHELL: true,
  TERM: true,
  TZ: true,
  LANG: true,
  LANGUAGE: true,
  LC_ADDRESS: true,
  LC_ALL: true,
  LC_COLLATE: true,
  LC_CTYPE: true,
  LC_IDENTIFICATION: true,
  LC_MEASUREMENT: true,
  LC_MESSAGES: true,
  LC_MONETARY: true,
  LC_NAME: true,
  LC_NUMERIC: true,
  LC_PAPER: true,
  LC_TELEPHONE: true,
  LC_TIME: true,
  PWD: true,
  OLDPWD: true,
  SHLVL: true,
  HOSTNAME: true,
  PATH: true,
  TMPDIR: true,
  TMUX_TMPDIR: true,
  XDG_CACHE_HOME: true,
  XDG_CONFIG_HOME: true,
  XDG_DATA_HOME: true,
  XDG_RUNTIME_DIR: true,
  XDG_STATE_HOME: true,
  OMP_PROFILE: true,
  PI_PROFILE: true,
  MISE_CACHE_DIR: true,
  MISE_CONFIG_DIR: true,
  MISE_DATA_DIR: true,
  MISE_STATE_DIR: true,
  SECRETSD_SOCK: true,
  // The outbound network policy; a proxy URL's password is still masked (environmentSecrets).
  HTTP_PROXY: true,
  HTTPS_PROXY: true,
  NO_PROXY: true,
  http_proxy: true,
  https_proxy: true,
  no_proxy: true,
  SSL_CERT_DIR: true,
  SSL_CERT_FILE: true,
  NODE_EXTRA_CA_CERTS: true,
  // What Kubernetes tells every container about the API server.
  KUBERNETES_SERVICE_HOST: true,
  KUBERNETES_SERVICE_PORT: true,
  KUBERNETES_SERVICE_PORT_HTTPS: true,
  KUBERNETES_PORT: true,
  KUBERNETES_PORT_443_TCP: true,
  KUBERNETES_PORT_443_TCP_ADDR: true,
  KUBERNETES_PORT_443_TCP_PORT: true,
  KUBERNETES_PORT_443_TCP_PROTO: true,
  // The Legion pane's environment (packages/pi-legion/AGENTS.md, "Daemon contract"): the tmux
  // runtime's runtimeOwned and controllerOwned (runtime/tmux/spawn.go), the Sandbox runtime's
  // owned names (runtime/sandbox/manifest.go), the pod baseline (internal/podsafety) and the
  // worker image's ENV (packages/daemon/docker/worker.Dockerfile).
  LEGION_TREE: true,
  LEGION_ISSUE: true,
  LEGION_ROLE: true,
  LEGION_CONTROLLER: true,
  LEGION_PROJECT: true,
  LEGION_GENERATION: true,
  LEGION_DAEMON_URL: true,
  LEGION_STATE_DIR: true,
  LEGION_WORKSPACE: true,
  LEGION_WORKSPACE_RECREATED: true,
  LEGION_SLACK_CHANNELS: true,
  LEGION_GH_OWNERS_DIR: true,
  LEGION_GH_PATH: true,
  LEGION_OMP_PATH: true,
  LEGION_CLAUDE_INBOX: true,
  LEGION_REPOSITORY_CACHE_URL: true,
  ENVOY_URL: true,
  ENVOY_NATS_URL: true,
  DISPATCH_URL: true,
  GH_CONFIG_DIR: true,
  GH_HOST: true,
  GIT_TERMINAL_PROMPT: true,
  PI_SHELL_PREFIX: true,
  PI_CONFIG_FILES: true,
  PI_CONFIG_DIR: true,
  OMP_SESSION_STORAGE: true,
  POD_UID: true,
  GOCACHE: true,
  GOMODCACHE: true,
  BUN_INSTALL_CACHE_DIR: true,
  UV_PYTHON_INSTALL_DIR: true,
  UV_CACHE_DIR: true,
  UV_LINK_MODE: true,
  AGENT_SECRETS_URL: true,
  AGENT_SECRETS_KEY_DIR: true,
  DO_NOT_TRACK: true,
  CLAUDE_CONFIG_DIR: true,
  CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: true,
  CLAUDE_CODE_DISABLE_CRON: true,
  // The role's git identity (runtime.GitIdentity.Env).
  JJ_USER: true,
  JJ_EMAIL: true,
  GIT_AUTHOR_NAME: true,
  GIT_AUTHOR_EMAIL: true,
  GIT_COMMITTER_NAME: true,
  GIT_COMMITTER_EMAIL: true,
  // What the Envoy entry sets in its own process (packages/pi-envoy/extensions/envoy.ts): the
  // session's envoy.json, which need not exist, and the session's Dispatch identity.
  ENVOY_CONFIG_FILE: true,
  DISPATCH_HOST: true,
  DISPATCH_SESSION_ID: true,
};

/** The daemon's credential-shaped name (packages/daemon/internal/runtime/secretname.go). */
const SECRET_LIKE_NAME =
  /(?:_SECRET|_TOKEN|_GRANT|_KEY|_SEED|_PASSWORD|_PASSWD|_PAT|_CREDENTIALS)(?:_FILE)?$|PRIVATE_KEY/i;

/** A URL whose userinfo carries a password (packages/daemon/internal/runtime/paneenv.go's
 * urlPassword), the password captured. */
const URL_PASSWORD = /[A-Za-z][A-Za-z0-9+.-]*:\/\/[^\s/?#@:]*:([^\s/?#@]+)@/g;

/** The shortest value masked under a credential-shaped name, in a URL's password, or in a token
 * file: shorter text would mask ordinary words, and no credential Legion handles is that short. */
const MIN_SECRET_LENGTH = 8;

/** The shortest value masked under any other name that is not public: long enough that a word an
 * unknown variable holds (`NODE_ENV=production`) stays readable everywhere it appears. */
const MIN_UNNAMED_SECRET_LENGTH = 16;

/** The largest file a `*_FILE` variable names that is read for the token it holds. */
const TOKEN_FILE_MAX_BYTES = 64 * 1024;

/** One value no tool result may carry, and the text that takes its place. */
export interface Secret {
  readonly value: string;
  readonly marker: string;
}

/** An environment as name and value pairs; a name may appear twice with two values. */
export type Environment = Iterable<readonly [string, string]>;

function marker(what: string): string {
  return `[redacted by Legion: ${what}]`;
}

/** Whether value names paths rather than holding a secret: one absolute path, or a `:`-separated
 * list of them, at least one of which exists. A random credential names no existing file. */
function namesPaths(value: string): boolean {
  const parts = value.split(":");
  return (
    parts.every((part) => part.startsWith("/")) &&
    parts.some((part) => statSync(part, { throwIfNoEntry: false }) !== undefined)
  );
}

/** The one token a small regular file holds, or nothing when it holds anything else or cannot be
 * read (then no tool of this process can read it either). */
function tokenIn(file: string): string | undefined {
  try {
    const stat = statSync(file);
    if (!stat.isFile() || stat.size > TOKEN_FILE_MAX_BYTES) return undefined;
    const text = readFileSync(file, "utf8").trim();
    return /^\S+$/.test(text) ? text : undefined;
  } catch {
    return undefined;
  }
}

/**
 * The values of `environment` that may be credentials, longest first, so a secret holding another
 * is replaced whole. A value two variables hold is marked with the first one's name.
 */
export function environmentSecrets(environment: Environment): Secret[] {
  const secrets = new Map<string, string>();
  const add = (value: string, label: string): void => {
    if (value.length >= MIN_SECRET_LENGTH && !secrets.has(value)) secrets.set(value, label);
  };
  for (const [name, value] of environment) {
    for (const match of value.matchAll(URL_PASSWORD)) {
      add(match[1] ?? "", marker(`password in $${name}`));
    }
    if (namesPaths(value)) {
      const token = name.endsWith("_FILE") ? tokenIn(value) : undefined;
      if (token !== undefined) add(token, marker(`contents of $${name}`));
      continue;
    }
    if (PUBLIC_VARIABLES[name] === true) continue;
    const label = marker(`$${name}`);
    if (
      value.length >= (SECRET_LIKE_NAME.test(name) ? MIN_SECRET_LENGTH : MIN_UNNAMED_SECRET_LENGTH)
    ) {
      add(value, label);
    }
    // A multi-line value (a PEM key) printed a line at a time loses each line too.
    if (value.includes("\n")) {
      for (const line of value.split("\n")) {
        const piece = line.trim();
        if (piece.length >= MIN_UNNAMED_SECRET_LENGTH) add(piece, label);
      }
    }
  }
  return [...secrets]
    .map(([value, label]) => ({ value, marker: label }))
    .sort((a, b) => b.value.length - a.value.length);
}

/** text with every secret replaced by its marker. */
export function redactText(text: string, secrets: readonly Secret[]): string {
  let redacted = text;
  for (const secret of secrets) {
    if (redacted.includes(secret.value))
      redacted = redacted.split(secret.value).join(secret.marker);
  }
  return redacted;
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  if (typeof value !== "object" || value === null) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

/** value with every string in it redacted, at any depth of its arrays and plain objects; the same
 * value when nothing in it changed. */
function redactDeep(value: unknown, secrets: readonly Secret[]): unknown {
  if (typeof value === "string") return redactText(value, secrets);
  if (Array.isArray(value)) {
    const items = value.map((item) => redactDeep(item, secrets));
    return items.some((item, index) => item !== value[index]) ? items : value;
  }
  if (isPlainObject(value)) {
    let changed = false;
    const entries = Object.entries(value).map(([key, item]) => {
      const redacted = redactDeep(item, secrets);
      if (redacted !== item) changed = true;
      return [key, redacted] as const;
    });
    return changed ? Object.fromEntries(entries) : value;
  }
  return value;
}

/**
 * The result with every secret replaced in its text blocks and in every string of its details
 * (where a `read` keeps its display copy), or nothing when it held none. Images pass unchanged.
 */
export function redactResult(
  result: { readonly content: readonly ContentBlock[]; readonly details: unknown },
  secrets: readonly Secret[]
): { readonly content: readonly ContentBlock[]; readonly details: unknown } | undefined {
  if (secrets.length === 0) return undefined;
  let changed = false;
  const content = result.content.map((block): ContentBlock => {
    if (block.type !== "text") return block;
    const text = redactText(block.text, secrets);
    if (text === block.text) return block;
    changed = true;
    return { ...block, text };
  });
  const details = redactDeep(result.details, secrets);
  if (details !== result.details) changed = true;
  return changed ? { content, details } : undefined;
}

let initialBlock: string | undefined | null = null;

/** The environment this process was started with, as Linux keeps it: what `/proc/self/environ`
 * shows whatever the process changed since. Read once; undefined where it cannot be. */
function initialEnvironment(): string | undefined {
  if (initialBlock === null) {
    try {
      initialBlock = readFileSync("/proc/self/environ", "utf8");
    } catch {
      initialBlock = undefined;
    }
  }
  return initialBlock;
}

/**
 * This process's environment: `env` as it is now, which every command the agent runs inherits,
 * and each pair of the block the process started with (`initial`, NUL-separated) that `env` no
 * longer holds, since the agent can read that block whatever the process deleted since.
 */
export function ownEnvironment(
  env: Readonly<Record<string, string | undefined>>,
  initial: string | undefined
): [string, string][] {
  const entries: [string, string][] = [];
  for (const [name, value] of Object.entries(env)) {
    if (value !== undefined) entries.push([name, value]);
  }
  for (const pair of initial?.split("\0") ?? []) {
    const at = pair.indexOf("=");
    if (at <= 0) continue;
    const name = pair.slice(0, at);
    const value = pair.slice(at + 1);
    if (env[name] !== value) entries.push([name, value]);
  }
  return entries;
}

/** The artifact id a result's details name, where the tool spilled its full text. */
function artifactIdOf(details: unknown): string | undefined {
  if (!isPlainObject(details) || !isPlainObject(details.meta)) return undefined;
  const truncation = details.meta.truncation;
  if (!isPlainObject(truncation) || typeof truncation.artifactId !== "string") return undefined;
  return truncation.artifactId;
}

/** Rewrites the artifact a result spilled to with every secret replaced; a file that cannot be
 * rewritten is removed, and one that cannot be removed either fails the redaction. */
async function redactArtifact(
  details: unknown,
  context: SessionContext,
  secrets: readonly Secret[]
): Promise<void> {
  const id = artifactIdOf(details);
  if (id === undefined || secrets.length === 0) return;
  const file = await context.sessionManager.getArtifactPath?.(id);
  if (file === undefined || file === null) return;
  const text = readFileSync(file, "utf8");
  const redacted = redactText(text, secrets);
  if (redacted === text) return;
  try {
    writeFileSync(file, redacted);
  } catch {
    unlinkSync(file);
  }
}

export interface ToolResultRedactorOptions {
  /** This process's environment, read at every result: process.env and the block it started with. */
  readonly environment?: () => Environment;
}

/**
 * The Legion entry's tool_result handler: in a Legion session (a root architect's, a phase
 * worker's, the controller's, and every `task` subagent's of them, which share the process and its
 * environment), the result with this process's credentials replaced, and the artifact it spilled to
 * rewritten the same way. A session with no Legion environment is left as it is. A failure withholds
 * the whole result rather than let it through.
 */
export function createToolResultRedactor(
  options: ToolResultRedactorOptions = {}
): (event: ToolResultEvent, context: SessionContext) => Promise<ToolResultEventResult | undefined> {
  const environment =
    options.environment ?? (() => ownEnvironment(process.env, initialEnvironment()));
  return async (event, context) => {
    try {
      const entries = [...environment()];
      if (classifySession(Object.fromEntries(entries)).kind === "not-legion") return undefined;
      const secrets = environmentSecrets(entries);
      await redactArtifact(event.details, context, secrets);
      return redactResult(event, secrets);
    } catch (error) {
      // Nothing of the tool's own output stands in for a result whose redaction failed.
      return {
        content: [
          {
            type: "text",
            text: `Legion withheld this tool result: redacting credentials from it failed (${messageFor(error)})`,
          },
        ],
        details: {},
        isError: true,
      };
    }
  };
}

import { afterEach, beforeEach, describe, expect, mock, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:net";
import { hostname, tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { dispatchIssueSubject } from "@legion/contracts";
import { shownPictures } from "@legion/envoy-client/dispatch-picture-tools";
import {
  appendResult,
  readSessionTitle,
  sessionDirectory,
} from "@legion/envoy-client/dispatch-session-state";
import { machineID } from "@legion/envoy-client/machine";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { matchInjectedUserTurn } from "@legion/pi-shared/injected-user-turns";
import {
  envoyPluginInterface,
  LOCAL_ENVOY_NOTICE,
  resetEnvoyPluginInterfaceForTests,
} from "@legion/pi-shared/interface";
import type {
  HostSlashCommand,
  MessageRenderer,
  MessageRendererTheme,
  PiApi,
  UserInputResult,
} from "@legion/pi-shared/pi-types";
import {
  claimEnvoyRole,
  onEnvoyRoleRegained,
  subscribeLegionNotice,
} from "@legion/pi-shared/role-claim-bridge";
import { hostAgentRegistryMock, testAgentRoster } from "@legion/pi-shared/test/host-registry";
import type { UserInputBuiltinCommand } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler";
import { logger, procmgr } from "@oh-my-pi/pi-utils";
import { decode } from "@toon-format/toon";
import { z } from "zod";
import { resetEnvoySessionsForTests } from "../src/envoy-session";

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

type RegisteredTool = {
  readonly name: string;
  readonly description: string;
  readonly parameters: unknown;
  readonly execute: (
    id: string,
    params: Record<string, unknown>,
    signal?: AbortSignal,
    onUpdate?: unknown,
    context?: SessionContext
  ) => Promise<ToolResult>;
};

type RegisteredCommand = {
  readonly name: string;
  readonly description: string | undefined;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

type SessionContext = {
  readonly cwd: string;
  /** Oh My Pi's agent identity: the top-level session's runner is `main`, a `task` subagent's `sub`. */
  readonly agent?: { readonly kind: "main" | "sub" };
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionName?: () => string | undefined;
    readonly getBranch?: () => readonly unknown[];
    readonly getEntries: () => readonly unknown[];
    readonly getSessionFile: () => string | undefined;
    readonly ensureOnDisk: () => Promise<void>;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
    readonly onTerminalInput: (handler: (input: unknown) => void) => () => void;
    readonly getEditorText: () => string;
  };
};

type CommandContext = {
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning" | "error") => void;
  };
};

type TestPi = {
  readonly zod: typeof z;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
  readonly registerMessageRenderer: PiApi["registerMessageRenderer"];
  readonly on: (
    event:
      | "resources_discover"
      | "session_start"
      | "session_switch"
      | "session_branch"
      | "session_tree"
      | "session_shutdown"
      | "before_agent_start"
      | "agent_end"
      | "message_start"
      | "session_stop"
      | "input"
      | "tool_call"
      | "tool_result",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>
  ) => void;
  readonly sendMessage: (
    message: { readonly content: string; readonly customType?: string; readonly display?: boolean },
    options: unknown
  ) => void;
  readonly sendUserMessage: (content: string, options?: unknown) => void;
  readonly appendEntry: PiApi["appendEntry"];
};
type Subscription = {
  readonly unsubscribe: () => void;
  readonly [Symbol.asyncIterator]: () => AsyncIterator<{
    readonly subject: string;
    readonly data: Uint8Array;
    readonly reply: string;
  }>;
};

type SubscriptionControls = {
  readonly push: (data: string, reply?: string) => void;
  readonly end: () => void;
  readonly fail: (error: Error) => void;
  readonly active: () => boolean;
};

const natsState = {
  subscriptions: new Map<string, Subscription>(),
  controls: new Map<string, SubscriptionControls>(),
  controlsByTopic: new Map<string, SubscriptionControls[]>(),
  published: [] as { readonly subject: string; readonly data: Uint8Array | undefined }[],
  onPublish: undefined as ((subject: string) => void) | undefined,
  failPublishes: 0,
  failSubscribes: 0,
  connectedNames: [] as string[],
  /** What each connect was given: its servers, and whether it carried an nkey authenticator. */
  connects: [] as { readonly servers: readonly string[]; readonly authenticated: boolean }[],
  failConnects: 0,
  /** Every connection reports itself closed, as one NATS dropped does. */
  connectionsClosed: false,
  /** Hands each connect, once recorded, to nats.js itself, for a row that watches what it dials. */
  realConnect: false,
  drainHangs: false,
  drainStarted: false,
};

const targetedDispatchPayload = readFileSync(
  new URL("../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
  "utf8"
);

const clipboardState = {
  copiedSessionIDs: [] as string[],
  error: undefined as Error | undefined,
};

// @legion/envoy-client/nats-auth resolves the NATS credential with the real nkey exports, and a row
// that sets `natsState.realConnect` gets nats.js's own connect. They are copied out of the module
// before the mock replaces it: a static import's live bindings would read the mock.
const { connect: realConnect, nkeyAuthenticator, nkeys } = await import("nats");
mock.module("nats", () => ({
  nkeyAuthenticator,
  nkeys,
  connect: async (options: {
    readonly name: string;
    readonly servers: readonly string[];
    readonly authenticator?: unknown;
  }) => {
    const { name, servers, authenticator } = options;
    if (natsState.failConnects > 0) {
      natsState.failConnects -= 1;
      throw new Error("CONNECTION_REFUSED");
    }
    natsState.connectedNames.push(name);
    natsState.connects.push({ servers, authenticated: authenticator !== undefined });
    if (natsState.realConnect) {
      return realConnect(options as unknown as Parameters<typeof realConnect>[0]);
    }
    return {
      isClosed: () => natsState.connectionsClosed,
      drain: async () => {
        natsState.drainStarted = true;
        if (natsState.drainHangs) await Promise.withResolvers<never>().promise;
      },
      publish: (subject: string, data?: Uint8Array) => {
        if (natsState.failPublishes > 0) {
          natsState.failPublishes -= 1;
          throw new Error("PUBLISH_FAILED");
        }
        natsState.published.push({ subject, data });
        natsState.onPublish?.(subject);
      },
      subscribe: (topic: string) => {
        if (natsState.failSubscribes > 0) {
          natsState.failSubscribes -= 1;
          throw new Error("SUBSCRIBE_FAILED");
        }
        let active = true;
        const queue: {
          readonly subject: string;
          readonly data: Uint8Array;
          readonly reply: string;
        }[] = [];
        let wake: (() => void) | undefined;
        let ended = false;
        let failure: Error | undefined;
        const notify = () => {
          wake?.();
          wake = undefined;
        };
        const controls = {
          push: (data: string, reply = "") => {
            queue.push({ subject: topic, data: new TextEncoder().encode(data), reply });
            notify();
          },
          end: () => {
            ended = true;
            notify();
          },
          fail: (error: Error) => {
            failure = error;
            notify();
          },
          active: () => active,
        };
        natsState.controls.set(topic, controls);
        const controlsForTopic = natsState.controlsByTopic.get(topic);
        if (controlsForTopic === undefined) natsState.controlsByTopic.set(topic, [controls]);
        else controlsForTopic.push(controls);
        const subscription: Subscription = {
          unsubscribe: () => {
            active = false;
            // nats.js ends the async iterator on unsubscribe(), which is what
            // makes a deliberate close indistinguishable from a dropped
            // connection inside the pump. A mock that leaves the iterator
            // parked cannot observe that, and lets resubscribe bugs ship green.
            notify();
          },
          [Symbol.asyncIterator]: () => ({
            next: async (): Promise<
              IteratorResult<{
                readonly subject: string;
                readonly data: Uint8Array;
                readonly reply: string;
              }>
            > => {
              for (;;) {
                if (failure) throw failure;
                if (!active || ended) return { done: true, value: undefined };
                const item = queue.shift();
                if (item) return { done: false, value: item };
                const pending = Promise.withResolvers<void>();
                wake = pending.resolve;
                await pending.promise;
              }
            },
          }),
        };
        natsState.subscriptions.set(topic, subscription);
        return subscription;
      },
    };
  },
  StringCodec: () => ({
    decode: (data: Uint8Array) => new TextDecoder().decode(data),
    encode: (text: string) => new TextEncoder().encode(text),
  }),
}));

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async (text: string) => {
    if (clipboardState.error !== undefined) throw clipboardState.error;
    clipboardState.copiedSessionIDs.push(text);
  },
  ...hostAgentRegistryMock,
}));
// The host's built-in slash commands, which our fork lists beside `sendUserInput`
// (`listUserInputBuiltinCommands`): a test fills this list, and `afterEach` empties it. No package
// in this workspace provides the module, so without this stand-in the extension lists no
// built-ins, as on a host without the method.
const hostBuiltins: UserInputBuiltinCommand[] = [];
mock.module("@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler", () => ({
  listUserInputBuiltinCommands: () => hostBuiltins,
}));
// Reads the host package, so it loads only after the mock above is in place.
const { recordBootstrappedSession } = await import("@legion/pi-shared/subagent-session");

const originalFetch = globalThis.fetch;

const originalNatsUrl = process.env.ENVOY_NATS_URL;
const originalEnvoyUrl = process.env.ENVOY_URL;
const originalHome = process.env.HOME;
const originalPath = process.env.PATH;
const originalDispatchUrl = process.env.DISPATCH_URL;
const originalDispatchToken = process.env.DISPATCH_TOKEN;
// The Legion daemon exports DISPATCH_TOKEN_FILE into every pane it spawns; a suite run from one
// must not see it, or the extension resolves a real Dispatch config and registers the tools.
const originalDispatchTokenFile = process.env.DISPATCH_TOKEN_FILE;
const originalTmuxPane = process.env.TMUX_PANE;
// The dispatch command's contract with the extension: the session id and host it exports, and the
// state directory both read, which each test points at a scratch directory of its own.
const originalDispatchSessionId = process.env.DISPATCH_SESSION_ID;
const originalDispatchHost = process.env.DISPATCH_HOST;
const originalDispatchStateDir = process.env.DISPATCH_STATE_DIR;
// What picks the envoy.json the extension reads, and the credentials a Legion pane carries beside
// it: each test starts with none of them but a file that holds no section (`beforeEach`).
const configVariables = [
  "ENVOY_CONFIG_FILE",
  "OMP_PROFILE",
  "PI_CODING_AGENT_DIR",
  "XDG_CONFIG_HOME",
  "ENVOY_TOKEN",
  "ENVOY_TOKEN_FILE",
  "NATS_NKEY_SEED",
  "NATS_NKEY_SEED_FILE",
] as const;
const originalConfigVariables = configVariables.map((name) => [name, process.env[name]] as const);
let configDirectory = "";

beforeEach(() => {
  // `bun test` runs every file in one process: a Legion suite's bootstrapped-session record on
  // the process-wide interface would otherwise make every transcript here look like a
  // subagent's, the top-level session one test publishes would be the reply address the next
  // test's subagent instance reports, and a stale claim instance would capture a later claim.
  resetEnvoyPluginInterfaceForTests();
  resetEnvoySessionsForTests();
  testAgentRoster().splice(0);
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  // A test that never stubs fetch must not register its `ses_*` fixture on the real listener
  // (the client defaults to http://127.0.0.1:9020, which on a devbox is production): point the
  // URL at an unroutable host and answer every request with the registration echo by default.
  process.env.ENVOY_URL = "http://envoy.test";
  globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
  delete process.env.DISPATCH_URL;
  delete process.env.DISPATCH_TOKEN;
  delete process.env.DISPATCH_TOKEN_FILE;
  process.env.HOME = "/nonexistent-home-for-envoy-tests";
  delete process.env.TMUX_PANE;
  process.env.DISPATCH_STATE_DIR = mkdtempSync(join(tmpdir(), "envoy-dispatch-state-"));
  delete process.env.DISPATCH_SESSION_ID;
  // The extension picks its envoy.json when its module loads, and each test loads it fresh: a file
  // that holds no section, so Dispatch and Envoy come from the variables the test sets and never
  // from the caller's own envoy.json or Oh My Pi profile.
  for (const name of configVariables) delete process.env[name];
  configDirectory = mkdtempSync(join(tmpdir(), "envoy-config-"));
  process.env.ENVOY_CONFIG_FILE = join(configDirectory, "envoy.json");
  writeFileSync(process.env.ENVOY_CONFIG_FILE, "{}\n");
});

afterEach(async () => {
  // A managed-timer callback that rejects fails the test that scheduled it.
  await Promise.all(contextTimers.splice(0));
  if (originalNatsUrl === undefined) delete process.env.ENVOY_NATS_URL;
  else process.env.ENVOY_NATS_URL = originalNatsUrl;
  if (originalEnvoyUrl === undefined) delete process.env.ENVOY_URL;
  else process.env.ENVOY_URL = originalEnvoyUrl;
  globalThis.fetch = originalFetch;
  if (originalDispatchUrl === undefined) delete process.env.DISPATCH_URL;
  else process.env.DISPATCH_URL = originalDispatchUrl;
  if (originalDispatchToken === undefined) delete process.env.DISPATCH_TOKEN;
  else process.env.DISPATCH_TOKEN = originalDispatchToken;
  if (originalDispatchTokenFile === undefined) delete process.env.DISPATCH_TOKEN_FILE;
  else process.env.DISPATCH_TOKEN_FILE = originalDispatchTokenFile;
  if (originalHome === undefined) delete process.env.HOME;
  else process.env.HOME = originalHome;
  if (originalPath === undefined) delete process.env.PATH;
  else process.env.PATH = originalPath;
  if (originalTmuxPane === undefined) delete process.env.TMUX_PANE;
  else process.env.TMUX_PANE = originalTmuxPane;
  if (process.env.DISPATCH_STATE_DIR !== undefined) {
    rmSync(process.env.DISPATCH_STATE_DIR, { recursive: true, force: true });
  }
  for (const [name, value] of [
    ["DISPATCH_SESSION_ID", originalDispatchSessionId],
    ["DISPATCH_HOST", originalDispatchHost],
    ["DISPATCH_STATE_DIR", originalDispatchStateDir],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  for (const [name, value] of originalConfigVariables) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  rmSync(configDirectory, { recursive: true, force: true });
  procmgr.__resetShellConfigCacheForTests();
  clipboardState.copiedSessionIDs.length = 0;
  clipboardState.error = undefined;
  delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
  natsState.connectedNames.length = 0;
  natsState.connects.length = 0;
  natsState.published.length = 0;
  natsState.onPublish = undefined;
  natsState.failPublishes = 0;
  natsState.subscriptions.clear();
  natsState.controls.clear();
  natsState.controlsByTopic.clear();
  natsState.failConnects = 0;
  natsState.connectionsClosed = false;
  natsState.failSubscribes = 0;
  natsState.realConnect = false;
  natsState.drainHangs = false;
  natsState.drainStarted = false;
  hostBuiltins.length = 0;
});

function createPi(options: { readonly clipboardError?: Error; readonly zod?: typeof z } = {}) {
  clipboardState.error = options.clipboardError;
  const commands: RegisteredCommand[] = [];
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, (event: unknown, context: SessionContext) => Promise<unknown>>();
  const renderers = new Map<string, MessageRenderer>();
  const messages: string[] = [];
  const deliveries: {
    readonly content: string;
    readonly customType?: string;
    readonly details?: unknown;
    readonly display?: boolean;
    readonly options: unknown;
  }[] = [];
  // User turns the extension started, in the order it sent them.
  const userMessages: { readonly content: string; readonly options: unknown }[] = [];
  // Persisted custom entries, in the shape a later `getBranch()` or `getEntries()` returns them.
  const entries: {
    readonly type: "custom";
    readonly customType: string;
    readonly data: unknown;
  }[] = [];
  const pi: TestPi = {
    zod: options.zod ?? z,
    registerTool: (tool) => tools.push(tool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
    registerMessageRenderer: (customType, renderer) => renderers.set(customType, renderer),
    on: (event, handler) => handlers.set(event, handler),
    sendMessage: (message, options) => {
      messages.push(message.content);
      deliveries.push({
        content: message.content,
        customType: message.customType,
        details: "details" in message ? message.details : undefined,
        display: message.display,
        options,
      });
    },
    sendUserMessage: (content, options) => {
      userMessages.push({ content, options });
    },
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data });
    },
  };
  return {
    commands,
    copiedSessionIDs: clipboardState.copiedSessionIDs,
    deliveries,
    entries,
    handlers,
    messages,
    pi,
    renderers,
    tools,
    userMessages,
  };
}

/** Every callback the default `sessionContext()` timer ran, awaited after each test. */
const contextTimers: Promise<void>[] = [];

// The session-manager surface `isSubagentSession` reads. No transcript path reads as a
// top-level session, so every fixture here registers unless a test supplies a transcript that
// sits inside a parent's directory.
const topLevelSession = {
  getSessionFile: (): string | undefined => undefined,
  ensureOnDisk: async (): Promise<void> => undefined,
  getEntries: (): readonly unknown[] => [],
};

function sessionContext(sessionID = "ses_omp", hasUI = true): SessionContext {
  return {
    cwd: "/tmp/envoy-omp-test",
    hasUI,
    sessionManager: { ...topLevelSession, getSessionId: () => sessionID },
    setInterval: () => undefined,
    setTimeout: (callback) => {
      contextTimers.push(Promise.resolve().then(callback));
    },
    ui: {
      notify: () => undefined,
      onTerminalInput: () => () => undefined,
      getEditorText: () => "",
    },
  };
}

function commandContext(notifications: string[]): CommandContext {
  return {
    // A host command context always carries a session manager; an empty ID is
    // how it reports a session that has not been created yet.
    sessionManager: { ...topLevelSession, getSessionId: () => "" },
    ui: { notify: (message) => notifications.push(message) },
  };
}

// The role arbiter forwards an envelope under its original key behind the forward mark, and a
// publish the daemon makes carries a key the listener minted, 32 hex digits: `label` stands for it.
function forwardedRoleEnvelope(role: string, summary: string, label: string) {
  const minted = createHash("sha256").update(label).digest("hex").slice(0, 32);
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "envoy",
    source_event_id: `source-${label}`,
    topic: `notifications.role.${role}`,
    dedupe_key: `envoy.role.forward.publish.${minted}`,
    issued_at: 1,
    payload_summary: summary,
    trace_id: `trace-${label}`,
  });
}

function targetedDispatchEnvelope(mode: "aside" | "btw" | "steer", dedupeKey: string): string {
  return JSON.stringify({
    dedupe_key: dedupeKey,
    event_id: `dispatch-${dedupeKey}`,
    issued_at: 1,
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: "CORE-1",
        payload: {
          author: { id: "alice", kind: "user" },
          body: `Delivery ${mode} ${dedupeKey}`,
          created_at: "2026-09-12T00:00:00Z",
          deliveries: [],
          id: "11111111-1111-4111-8111-111111111111",
          in_reply_to: null,
          issue_key: "CORE-1",
          target: "session:ses_delivery",
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode },
    }),
    payload_summary: `Delivery ${mode} ${dedupeKey}`,
    source: "dispatch",
    source_event_id: "1",
    topic: "notifications.agent.ses_delivery",
    trace_id: dedupeKey,
  });
}

function targetedCommentEnvelope(mode: "aside" | "btw" | "steer", dedupeKey: string): string {
  return JSON.stringify({
    dedupe_key: dedupeKey,
    event_id: `dispatch-${dedupeKey}`,
    issued_at: 1,
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: "CORE-1",
        payload: {
          artifact_id: null,
          artifact_name: "spec.md",
          artifact_slug: "spec",
          author: { id: "alice", kind: "user" },
          body: `Comment ${dedupeKey}`,
          created_at: "2026-09-12T00:00:00Z",
          deliveries: [],
          mentions: [],
          id: "22222222-2222-4222-8222-222222222222",
          issue_key: "CORE-1",
          project_key: "CORE",
          reply_to: null,
          ask_id: null,
        },
        type: "comment.created",
      },
      delivery: {
        attempt: 1,
        mode,
        comment_id: "33333333-3333-4333-8333-333333333333",
        target: "session:ses_delivery",
      },
    }),
    payload_summary: `Comment ${dedupeKey}`,
    source: "dispatch",
    source_event_id: "1",
    topic: "notifications.agent.ses_delivery",
    trace_id: dedupeKey,
  });
}

const DIRECT_MESSAGE_ID = "44444444-4444-4444-8444-444444444444";
const DIRECT_MESSAGE_BODY = "Where is the dashboard?";
/** Another direct message from the same person: their next Send, not a retry of the first. */
const NEXT_MESSAGE_ID = "55555555-5555-4555-8555-555555555555";

/**
 * A person's direct message to ses_delivery from Dispatch's Agents page, as the listener hands the
 * session its delivery frame: no issue, the person as the event's actor, `attempt` (1 unless
 * given) in `mode`. `actor` is who the frame claims sent it and `body` what it claims the message
 * says, which any publisher on the subject can write.
 */
function directDispatchEnvelope(
  mode: "aside" | "btw" | "steer",
  dedupeKey: string,
  frame: {
    readonly actor?: { readonly kind: string; readonly id: string };
    readonly attempt?: number;
    readonly body?: string;
    readonly broadcastID?: string;
    readonly messageID?: string;
  } = {}
): string {
  const actor = frame.actor ?? { id: "alice", kind: "user" };
  const body = frame.body ?? DIRECT_MESSAGE_BODY;
  return JSON.stringify({
    dedupe_key: dedupeKey,
    event_id: `dispatch-${dedupeKey}`,
    issued_at: 1,
    payload: JSON.stringify({
      event: {
        actor,
        issue_key: null,
        payload: {
          author: actor,
          body,
          ...(frame.broadcastID === undefined ? {} : { broadcast_id: frame.broadcastID }),
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id: frame.messageID ?? DIRECT_MESSAGE_ID,
          in_reply_to: null,
          issue_key: null,
          target: "session:ses_delivery",
        },
        type: "message.created",
      },
      delivery: { attempt: frame.attempt ?? 1, mode },
    }),
    payload_summary: body,
    source: "dispatch",
    source_event_id: "1",
    topic: "notifications.agent.ses_delivery",
    trace_id: dedupeKey,
  });
}

/** One delivery attempt of the direct message, as Dispatch stores it. */
function storedAttempt(
  attempt: number,
  mode: "aside" | "btw" | "steer",
  messageID = DIRECT_MESSAGE_ID,
  requestedBy: { readonly kind: string; readonly id: string } = { id: "alice", kind: "user" }
) {
  return {
    accepted_as: null,
    accepted_at: null,
    attempt,
    created_at: "2026-09-30T00:00:00Z",
    delivery: mode,
    duplicate: false,
    envelope_id: null,
    error: null,
    message_id: messageID,
    reply_id: null,
    requested_by: requestedBy,
    session_id: "ses_delivery",
    state: "pending",
  };
}

/** What Dispatch answers an accept with: 200 with the accepted attempt and the stored body (a
 *  Send of DIRECT_MESSAGE_BODY unless given; `body: null` answers none), a refusal, or a request
 *  that never arrives. */
type AcceptAnswer =
  | {
      readonly accepted: true;
      readonly delivery?: "aside" | "steer";
      readonly body?: string | null;
    }
  | { readonly status: number; readonly code?: string }
  | { readonly reject: string };

/** Dispatch's own rule for one message (`message_delivery.go`): the first accept succeeds, with
 *  `body` as the stored body when given, and every later one is refused, since a message is taken
 *  at most once. */
const acceptOnce =
  (body?: string) =>
  (count: number): AcceptAnswer =>
    count === 1
      ? { accepted: true, ...(body !== undefined && { body }) }
      : { code: "ACCEPT_ALREADY_ACCEPTED", status: 409 };

/** The accept route's response for `answer` to an accept of `attempt`. */
function acceptResponse(
  attempt: number,
  answer: AcceptAnswer,
  messageID = DIRECT_MESSAGE_ID
): Response {
  if ("reject" in answer) throw new TypeError(answer.reject);
  if (!("accepted" in answer)) {
    return new Response(JSON.stringify({ code: answer.code, error: "refused" }), {
      headers: { "content-type": "application/json" },
      status: answer.status,
    });
  }
  const accepted = {
    ...storedAttempt(attempt, answer.delivery ?? "steer", messageID),
    accepted_as: "user_turn",
  };
  const body = answer.body === undefined ? DIRECT_MESSAGE_BODY : answer.body;
  return new Response(JSON.stringify(body === null ? accepted : { ...accepted, body }), {
    headers: { "content-type": "application/json" },
    status: 200,
  });
}

/**
 * Boots ses_delivery against a Dispatch whose accept answers `accept` for the nth accept
 * (Dispatch's own once-only rule by default), recording every accept and every other Dispatch
 * message call. `entries` are what the session's file already holds, as a restarted session reads
 * them, and `branch` its active branch (the entries by default); every entry the session appends
 * is read back with them. `delivered(n)` resolves once the session has taken `n` deliveries, cards
 * and user turns together, and `recordedWhenDelivered` says how many entries the session had
 * appended when each went out.
 */
async function bootDirectSession(
  query: string,
  options: {
    readonly accept?: (count: number) => AcceptAnswer | Promise<AcceptAnswer>;
    readonly entries?: readonly unknown[];
    readonly branch?: readonly unknown[];
    /** Answers a Dispatch request outside `/api/v1/messages/`, such as the pictures a delivery
     *  reads; undefined leaves it to the registration echo. */
    readonly dispatch?: (url: URL) => Response | undefined;
    /** Runs as the host is handed each delivery, before it takes it: a throw is the host refusing
     *  the send. */
    readonly beforeSend?: () => void;
    /** The host's `pi.sendUserInput`, which a host without it lacks; each call is recorded in
     *  `inputs` and counts as a delivery. */
    readonly sendUserInput?: (text: string, options: unknown) => Promise<UserInputResult>;
    /** The host's `pi.getCommands`. */
    readonly getCommands?: () => readonly HostSlashCommand[];
  } = {}
) {
  process.env.DISPATCH_URL = "http://dispatch.test";
  process.env.DISPATCH_TOKEN = "dispatch-token";
  const accepts: {
    readonly path: string;
    readonly authorization: string | null;
    readonly body: unknown;
  }[] = [];
  const posts: string[] = [];
  // The session's replies to the direct message's attempts, as Dispatch's reply route took them.
  const replies: { readonly path: string; readonly body: unknown }[] = [];
  const replyWaiters: { readonly count: number; readonly resolve: () => void }[] = [];
  globalThis.fetch = async (input, init) => {
    const url = new URL(input.toString());
    if (url.pathname.startsWith("/api/v1/messages/")) {
      const authorization = new Headers(init?.headers).get("Authorization");
      if ((init?.method ?? "GET") === "POST" && url.pathname.endsWith("/accept")) {
        accepts.push({
          authorization,
          body: JSON.parse(init?.body?.toString() ?? "null"),
          path: url.pathname,
        });
        return acceptResponse(
          Number(url.pathname.split("/").at(-2)),
          await (options.accept ?? acceptOnce())(accepts.length),
          url.pathname.split("/").at(-4)
        );
      }
      // The session asks Dispatch nothing else about a direct message but its reply: any other
      // call is recorded and refused, so a read-back would keep the card.
      posts.push(`${init?.method ?? "GET"} ${url.pathname}`);
      if ((init?.method ?? "GET") === "POST" && url.pathname.endsWith("/reply")) {
        replies.push({ body: JSON.parse(init?.body?.toString() ?? "null"), path: url.pathname });
        for (const waiter of replyWaiters.splice(0)) {
          if (replies.length >= waiter.count) waiter.resolve();
          else replyWaiters.push(waiter);
        }
        return new Response(JSON.stringify({}), { status: 201 });
      }
      return new Response(JSON.stringify({ code: "NOT_FOUND" }), { status: 404 });
    }
    const answered = options.dispatch?.(url);
    if (answered !== undefined) return answered;
    return responseWithRegistration(input, init, {});
  };
  // Query-string isolation gives each caller its own instance of this stateful extension, and a
  // restart its second one; the specifier is the caller's, so it cannot be a static import.
  const { default: envoyExtension } = await import(`./envoy.ts?${query}`);
  const fixture = createPi();
  const waiters: { readonly count: number; readonly resolve: () => void }[] = [];
  const recordedWhenDelivered: number[] = [];
  const inputs: { readonly text: string; readonly options: unknown }[] = [];
  const taken = () => {
    const count = fixture.deliveries.length + fixture.userMessages.length + inputs.length;
    for (const waiter of waiters.splice(0)) {
      if (count >= waiter.count) waiter.resolve();
      else waiters.push(waiter);
    }
  };
  const run = options.sendUserInput;
  envoyExtension({
    ...fixture.pi,
    sendMessage: (message: never, sendOptions: unknown) => {
      options.beforeSend?.();
      recordedWhenDelivered.push(fixture.entries.length);
      fixture.pi.sendMessage(message, sendOptions);
      taken();
    },
    sendUserMessage: (content: string, sendOptions?: unknown) => {
      options.beforeSend?.();
      recordedWhenDelivered.push(fixture.entries.length);
      fixture.pi.sendUserMessage(content, sendOptions);
      taken();
    },
    ...(run !== undefined && {
      sendUserInput: (text: string, inputOptions: unknown) => {
        options.beforeSend?.();
        recordedWhenDelivered.push(fixture.entries.length);
        inputs.push({ options: inputOptions, text });
        taken();
        return run(text, inputOptions);
      },
    }),
    getCommands: options.getCommands ?? (() => []),
  });
  const prior = options.entries ?? [];
  const base = sessionContext("ses_delivery");
  const context: SessionContext = {
    ...base,
    sessionManager: {
      ...base.sessionManager,
      getBranch: () => options.branch ?? [...prior, ...fixture.entries],
      getEntries: () => [...prior, ...fixture.entries],
    },
  };
  await fixture.handlers.get("session_start")?.({}, context);
  const agent = natsState.controls.get("notifications.agent.ses_delivery");
  if (agent === undefined) throw new Error("agent subject was not subscribed");
  const delivered = (count: number): Promise<void> => {
    const waiter = Promise.withResolvers<void>();
    waiters.push({ count, resolve: waiter.resolve });
    taken();
    return waiter.promise;
  };
  const replied = (count: number): Promise<void> => {
    const waiter = Promise.withResolvers<void>();
    if (replies.length >= count) waiter.resolve();
    else replyWaiters.push({ count, resolve: waiter.resolve });
    return waiter.promise;
  };
  return {
    accepts,
    agent,
    context,
    delivered,
    fixture,
    inputs,
    posts,
    recordedWhenDelivered,
    replied,
    replies,
  };
}

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200 });
}

function responseWithRegistration(
  input: string | URL | Request,
  init: RequestInit | undefined,
  fallback: unknown
): Response {
  if (new URL(input.toString()).pathname !== "/v1/interests/subscribe") return response(fallback);
  const body = JSON.parse(init?.body?.toString() ?? "{}") as {
    readonly session_id: string;
    readonly dir: string;
    readonly topics: readonly string[];
  };
  return response({
    session_id: body.session_id,
    machine_id: "test",
    dir: body.dir,
    topics: body.topics,
  });
}

const UNASKED_WAIT_NUDGE =
  "You just said you are waiting on a human for something no open ask in Dispatch covers. Open it now: a decision block in the document it concerns (dispatch doc-edit with an ask block), or dispatch ask for a to-do only a human can do, naming exactly what you need and from whom.";

/** Custom-message type of the nudge itself, which a session hears amid other deliveries. */
const ASK_REMINDER_TYPE = "dispatch-ask-reminder";

/**
 * Boots one session against a Dispatch whose open-ask snapshot each call reads from
 * `snapshot(since)` — `since` being the window the extension queried with, so a test can answer
 * `opened_since` the way the server does — and hands back the lifecycle edges the run-end nudge
 * lives between: a user turn (`before_agent_start`, which arms a period, then `agent_start`),
 * a run nobody typed (`agent_start` alone — an Envoy delivery waking the session), a stop
 * (`agent_end`), a tool result, and a tick of every interval the session registered. The host
 * answers the hidden self-check WAITING unless `selfCheck` says otherwise, since that is the
 * only verdict that reaches the visible steer.
 */
async function bootAskNudge(
  envoyExtension: (pi: TestPi) => void,
  sessionID: string,
  snapshot: (since: string | undefined) => {
    readonly as_of?: string;
    readonly count?: number;
    readonly opened_since?: boolean;
    readonly asks?: readonly { readonly question: string }[];
  },
  options: {
    /** Transcript this session resumes from, as `getBranch()` returns it. */
    readonly branch?: readonly unknown[];
    /** False is a headless run (`omp -p`), where the host supplies no UI context. */
    readonly hasUI?: boolean;
    /** Awaited before the stop-time query answers, to hold its round trip open. */
    readonly holdStopQuery?: () => Promise<void>;
    /** How the host answers the hidden self-check. */
    readonly selfCheck?: (input: {
      readonly prompt: string;
      readonly signal?: AbortSignal;
    }) => Promise<{ readonly replyText: string }>;
    /**
     * Whether the host serves a side turn (the extension context's `runEphemeralTurn`; true by
     * default). A host without one cannot serve a Dispatch BTW either.
     */
    readonly sideTurn?: boolean;
    /**
     * A fresh TUI, whose session id the host mints only after `session_start`: the extension's
     * own `sessionID` stays empty until the registration heartbeat heals the drift.
     */
    readonly lazySessionID?: boolean;
    /** `ENVOY_SELF_CHECK_TIMEOUT_MS` for this instance, so a hung host is bounded in ms. */
    readonly selfCheckTimeoutMs?: number;
    /** Queue the managed timer's callbacks instead of running them, until `runHeldTimers()`. */
    readonly holdTimers?: boolean;
  } = {}
) {
  const branch = options.branch ?? [];
  // A distinct server clock per query, so a period's window can only be the `as_of` of the
  // snapshot it was taken from: a regression that pinned it to the arming turn, or that failed
  // to move it, shows up in the `since=` of a later stop.
  let asOfCalls = 0;
  // Set by `holdNextArming()`: the next arming read signals `out` and waits for `gate`.
  let armingHold:
    | { readonly out: PromiseWithResolvers<void>; readonly gate: PromiseWithResolvers<void> }
    | undefined;
  let lastAsOf = "";
  let dispatchCalls = 0;
  process.env.DISPATCH_URL = "http://dispatch.test";
  process.env.DISPATCH_TOKEN = "token";
  const queries: string[] = [];
  const registrations: string[] = [];
  let registration = Promise.withResolvers<void>();
  globalThis.fetch = async (input, init) => {
    const url = new URL(input.toString());
    if (url.pathname === "/v1/roles/set") {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as { readonly role?: string };
      return response({
        session_id: sessionID,
        machine_id: "test",
        dir: "/tmp/envoy-omp-test",
        topics: [`notifications.role.${body.role ?? ""}`],
      });
    }
    if (url.pathname === "/v1/interests/subscribe") {
      registrations.push(url.pathname);
      registration.resolve();
    }
    if (url.pathname !== "/api/v1/asks/open") return responseWithRegistration(input, init, {});
    queries.push(url.search);
    if (url.search.includes("since=")) await options.holdStopQuery?.();
    else if (armingHold !== undefined) {
      const hold = armingHold;
      armingHold = undefined;
      hold.out.resolve();
      await hold.gate.promise;
    }
    const open = snapshot(url.searchParams.get("since") ?? undefined);
    asOfCalls += 1;
    lastAsOf = open.as_of ?? `2026-09-13T00:00:0${asOfCalls}Z`;
    // DispatchClient parses a body only when the response says it is JSON.
    return new Response(
      JSON.stringify({
        session_id: sessionID,
        as_of: lastAsOf,
        opened_since: open.opened_since ?? false,
        count: open.count ?? open.asks?.length ?? 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: (open.asks ?? []).map((ask, index) => ({
          id: `ask-${index}`,
          ref: `/issues/LEGION-${index}#ask-${index}`,
          question: ask.question,
          kind: "question",
          urgency: "normal",
          created_at: "2026-09-13T00:00:00Z",
          age_seconds: 0,
          priority: null,
          owner: { issue: { key: `LEGION-${index}`, title: "Test" } },
          human_replied: false,
          last_reply: null,
          waiting_on: "human",
        })),
      }),
      { headers: { "Content-Type": "application/json" } }
    );
  };
  const fixture = createPi();
  const asked: { readonly prompt: string; readonly signal?: AbortSignal }[] = [];
  const selfCheck = options.selfCheck;
  if (options.selfCheckTimeoutMs === undefined) delete process.env.ENVOY_SELF_CHECK_TIMEOUT_MS;
  else process.env.ENVOY_SELF_CHECK_TIMEOUT_MS = String(options.selfCheckTimeoutMs);
  const answer = (input: { readonly prompt: string; readonly signal?: AbortSignal }) => {
    asked.push(input);
    return selfCheck === undefined ? Promise.resolve({ replyText: "WAITING" }) : selfCheck(input);
  };
  envoyExtension(fixture.pi);
  // A fresh TUI has no session yet at `session_start`; the host mints the id before the first
  // turn, and the extension's own `sessionID` heals only on the next heartbeat.
  let live = options.lazySessionID === true ? "" : sessionID;
  const intervals: (() => void)[] = [];
  // The stop-time check runs on the host's managed timer once `agent_end` returns; a stop is
  // over when every timer it scheduled has run.
  const timers: Promise<void>[] = [];
  const held: (() => void | Promise<void>)[] = [];
  const drainTimers = async (): Promise<void> => {
    while (timers.length > 0) await timers.shift();
  };
  const context: SessionContext = {
    ...sessionContext(sessionID, options.hasUI ?? true),
    sessionManager: {
      ...topLevelSession,
      getSessionId: () => live,
      getBranch: () => branch,
    },
    setInterval: (callback) => intervals.push(callback),
    setTimeout: (callback) => {
      if (options.holdTimers === true) held.push(callback);
      else timers.push(Promise.resolve().then(callback));
    },
    ...(options.sideTurn === false
      ? {}
      : {
          runEphemeralTurn: ({ promptText, signal }) => answer({ prompt: promptText, signal }),
        }),
  };
  await fixture.handlers.get("session_start")?.({}, context);
  live = sessionID;
  const beforeAgentStart = fixture.handlers.get("before_agent_start");
  const agentStart = fixture.handlers.get("agent_start");
  const agentEnd = fixture.handlers.get("agent_end");
  const toolResult = fixture.handlers.get("tool_result");
  if (
    beforeAgentStart === undefined ||
    agentStart === undefined ||
    agentEnd === undefined ||
    toolResult === undefined
  ) {
    throw new Error("the run-end nudge's lifecycle handlers were not registered");
  }
  return {
    asked,
    /** The server clock of the last open-asks snapshot this Dispatch served. */
    asOf: () => lastAsOf,
    context,
    fixture,
    queries,
    registrations,
    /** Resolves the next time the extension registers this session with the listener. */
    nextRegistration: () => {
      registration = Promise.withResolvers<void>();
      return registration.promise;
    },
    /**
     * Runs every interval the session registered — the registration heartbeat, and the
     * `session_start` retry loop when NATS was down. The tick re-establishes the session
     * through the extension's own establishment queue, so its local-state restore lands in the
     * microtasks this yields to; `awaiting` is the signal for work that goes further than that
     * (`nextRegistration()`), never a wait on the clock.
     */
    heartbeat: async (awaiting?: Promise<unknown>) => {
      for (const tick of intervals) tick();
      await Promise.resolve();
      await Promise.resolve();
      if (awaiting !== undefined) await awaiting;
    },
    /** The host's order: `before_agent_start` is awaited, then the run starts. */
    userTurn: async (prompt = "finish the task") => {
      const injected = await beforeAgentStart({ prompt }, context);
      await agentStart({}, context);
      return injected;
    },
    /** A run the user did not type: an Envoy delivery, or another extension's continuation. */
    runStart: () => agentStart({}, context),
    /**
     * Holds the next turn's arming read open: `out` resolves once that read is in flight, and
     * the read answers when `release()` is called.
     */
    holdNextArming: () => {
      const hold = {
        out: Promise.withResolvers<void>(),
        gate: Promise.withResolvers<void>(),
      };
      armingHold = hold;
      return { out: hold.out.promise, release: () => hold.gate.resolve() };
    },
    /** Runs, in order, every timer callback `holdTimers` queued, each to completion. */
    runHeldTimers: async () => {
      for (const callback of held.splice(0)) await callback();
    },
    /**
     * A stop defaults to a normal settle: the run's last reply ended `stopReason: "stop"`. It
     * resolves once the check the stop scheduled has finished, or at once under `holdTimers`.
     */
    stop: async (
      event: {
        readonly willContinue?: boolean;
        readonly messages?: readonly { readonly role?: string; readonly stopReason?: string }[];
      } = {}
    ) => {
      await agentEnd({ messages: [{ role: "assistant", stopReason: "stop" }], ...event }, context);
      await drainTimers();
    },
    toolResult: (event: Record<string, unknown>) => toolResult(event, context),
    /**
     * A `dispatch` command the agent ran through `via` (bash by default; an `eval` cell runs
     * `command` as its code): the CLI's ledger line for `tool` with `details` under this session,
     * then the shell tool's own result, in the order Oh My Pi reports them.
     */
    dispatch: async (
      command: string,
      tool: string,
      details: Readonly<Record<string, unknown>>,
      via: "bash" | "eval" = "bash"
    ) => {
      appendResult(sessionDirectory(process.env, sessionID), { tool, details });
      dispatchCalls += 1;
      await toolResult(
        {
          toolName: via,
          toolCallId: `call-dispatch-${dispatchCalls}`,
          input: via === "bash" ? { command } : { language: "py", code: command },
          details: {},
          isError: false,
        },
        context
      );
    },
  };
}

/**
 * The same session on a host with the run-end self-check available. Without a side turn the nudge
 * has no trigger at all, so neither lifecycle edge reads Dispatch.
 */
function withSelfCheck(context: SessionContext): SessionContext {
  return { ...context, runEphemeralTurn: async () => ({ replyText: "PROCEEDING" }) };
}

describe("envoy OMP extension", () => {
  test("discovers the bundled envoy skill from the repository root", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?resources-discover");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    const resourcesDiscover = fixture.handlers.get("resources_discover");
    if (resourcesDiscover === undefined) throw new Error("resources_discover was not registered");

    const result = await resourcesDiscover({}, sessionContext());
    if (
      typeof result !== "object" ||
      result === null ||
      !("skillPaths" in result) ||
      !Array.isArray(result.skillPaths) ||
      typeof result.skillPaths[0] !== "string"
    ) {
      throw new Error("resources_discover did not return a skill path");
    }

    expect(existsSync(join(result.skillPaths[0], "envoy", "SKILL.md"))).toBe(true);
  });

  test("injects nothing at turn start and reads open asks only to arm the run-end nudge", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "token";
    const requests: string[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/api/v1/asks/open") {
        requests.push(url.pathname + url.search);
        return new Response(
          JSON.stringify({
            session_id: "ses_reminder",
            as_of: "2026-09-13T00:00:00Z",
            opened_since: false,
            count: 0,
            waiting_on_human: 0,
            waiting_on_agent: 0,
            asks: [],
          }),
          { headers: { "Content-Type": "application/json" } }
        );
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?open-ask-summary");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const context = withSelfCheck(sessionContext("ses_reminder"));
    await fixture.handlers.get("session_start")?.({}, context);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    if (beforeAgentStart === undefined) throw new Error("before_agent_start was not registered");

    const injected = await beforeAgentStart({ prompt: "finish the task" }, context);
    expect(injected).toBeUndefined();
    expect(requests).toEqual(["/api/v1/asks/open?author_session=ses_reminder"]);
  });

  test("injects nothing and warns once when the open-ask check is unavailable", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "token";
    const notifications: string[] = [];
    globalThis.fetch = async (input, init) => {
      if (new URL(input.toString()).pathname === "/api/v1/asks/open") {
        throw new Error("network offline");
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?open-ask-unavailable");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const context = withSelfCheck({
      ...sessionContext("ses_unknown"),
      ui: {
        ...sessionContext("ses_unknown").ui,
        notify: (message: string) => notifications.push(message),
      },
    });
    await fixture.handlers.get("session_start")?.({}, context);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    if (beforeAgentStart === undefined) throw new Error("before_agent_start was not registered");

    const injected = await beforeAgentStart({ prompt: "wait" }, context);
    expect(injected).toBeUndefined();
    expect(notifications).toEqual([expect.stringContaining("Dispatch open-ask check unavailable")]);
  });

  test("keeps each session's availability warning latched until that session recovers", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "token";
    const notifications: string[] = [];
    let activeSessionID = "ses_availability_a";
    let available = false;
    globalThis.fetch = async (input, init) => {
      if (new URL(input.toString()).pathname === "/api/v1/asks/open") {
        if (!available) throw new Error("network offline");
        return new Response(
          JSON.stringify({
            session_id: activeSessionID,
            as_of: "2026-09-13T00:00:00Z",
            opened_since: false,
            count: 0,
            waiting_on_human: 0,
            waiting_on_agent: 0,
            asks: [],
          }),
          { headers: { "Content-Type": "application/json" } }
        );
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?open-ask-availability-latch");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const context = withSelfCheck({
      ...sessionContext(),
      sessionManager: { ...topLevelSession, getSessionId: () => activeSessionID },
      ui: {
        ...sessionContext().ui,
        notify: (message: string) => notifications.push(message),
      },
    });
    await fixture.handlers.get("session_start")?.({}, context);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    if (beforeAgentStart === undefined) throw new Error("before_agent_start was not registered");

    await beforeAgentStart({ prompt: "first" }, context);
    activeSessionID = "ses_availability_b";
    await beforeAgentStart({ prompt: "second session" }, context);
    expect(notifications).toHaveLength(2);

    available = true;
    await beforeAgentStart({ prompt: "second session recovered" }, context);
    available = false;
    activeSessionID = "ses_availability_a";
    await beforeAgentStart({ prompt: "first session still unavailable" }, context);
    expect(notifications).toHaveLength(2);

    available = true;
    await beforeAgentStart({ prompt: "first session recovered" }, context);
    available = false;
    await beforeAgentStart({ prompt: "first session failed again" }, context);
    expect(notifications).toHaveLength(3);
  });

  test("nudges a WAITING stop, and its own reply-only continuation nudges nothing", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-fires");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_fires", () => ({}));

    await session.userTurn();
    await session.stop();
    expect(session.fixture.deliveries).toEqual([
      {
        content: UNASKED_WAIT_NUDGE,
        customType: "dispatch-ask-reminder",
        details: undefined,
        display: false,
        options: { deliverAs: "steer", triggerTurn: true },
      },
    ]);

    // The nudge's own continuation: it replies and stops, having called no tool, so it owes the
    // period no check and cannot nudge itself. This is the whole loop safety of the design.
    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);

    await session.userTurn("now the next thing");
    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(2);
    // One query per user turn, one per stop, and each stop carries the baseline of the turn
    // that armed its own period — the second stop queries from the second turn's `as_of`, not
    // the first's.
    expect(session.queries).toEqual([
      "?author_session=ses_nudge_fires",
      "?author_session=ses_nudge_fires&since=2026-09-13T00%3A00%3A01Z",
      "?author_session=ses_nudge_fires",
      "?author_session=ses_nudge_fires&since=2026-09-13T00%3A00%3A03Z",
    ]);
    // One self-check per checked stop — the continuation's stop ran none — each asking for the one
    // word the steer hangs on, in the /btw wrapper (the host sends the prompt as given), with the
    // extension's own abort signal.
    expect(session.asked.map((ask) => ask.prompt)).toEqual([
      expect.stringMatching(/^<btw>\n[\s\S]*WAITING or PROCEEDING[\s\S]*\n<\/btw>$/),
      expect.stringMatching(/^<btw>\n[\s\S]*WAITING or PROCEEDING[\s\S]*\n<\/btw>$/),
    ]);
    expect(session.asked[0]?.signal?.aborted).toBe(false);
    // The arming period lives in memory only: nothing about it reaches the transcript.
    expect(session.fixture.entries).toEqual([]);
  });

  test("runs the stop-time check on the managed timer, after agent_end has returned", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-after-handler");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_after_handler", () => ({}), {
      holdTimers: true,
    });

    await session.userTurn();
    await session.stop();
    // On Oh My Pi 18.3 a side turn started inside the handler would carry the handler's 30 s
    // abort, so nothing about the check may have happened by the time it returns.
    expect(session.queries).toEqual(["?author_session=ses_nudge_after_handler"]);
    expect(session.asked).toEqual([]);

    await session.runHeldTimers();
    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toMatchObject([{ customType: "dispatch-ask-reminder" }]);
  });

  test("a user turn between a stop and its timer leaves that stop unchecked", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-turn-before-timer");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_turn_before_timer", () => ({}), {
      holdTimers: true,
    });

    await session.userTurn();
    await session.stop();
    // The user types, and the timer fires while the turn's arming read is still out: no run has
    // started, so only the turn's generation says the stop has been overtaken.
    const arming = session.holdNextArming();
    const typed = session.userTurn("the next thing");
    await arming.out;
    await session.runHeldTimers();
    arming.release();
    await typed;

    // Only the two arming reads: the stop's check never read Dispatch or asked the model.
    expect(session.queries).toEqual([
      "?author_session=ses_nudge_turn_before_timer",
      "?author_session=ses_nudge_turn_before_timer",
    ]);
    expect(session.asked).toEqual([]);
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("a woken run between a stop and its timer leaves that stop unchecked", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-run-before-timer");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_run_before_timer", () => ({}), {
      holdTimers: true,
    });

    await session.userTurn();
    await session.stop();
    await session.runStart();
    await session.runHeldTimers();

    // The stop is about a run the session has moved past, so its check reads nothing.
    expect(session.queries).toEqual(["?author_session=ses_nudge_run_before_timer"]);
    expect(session.asked).toEqual([]);
  });

  test("stays silent when the self-check answers PROCEEDING", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-proceeding");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_proceeding", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.stop();
    // The agent said it is not waiting on anyone: the whole check cost one hidden call and
    // the user saw nothing.
    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("the agent's next real work owes the period another check", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-rearm");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_rearm", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);

    // Nothing happened between the two settles, so the second is not worth a second opinion.
    await session.stop();
    expect(session.asked).toHaveLength(1);

    // A successful tool call that is not a Dispatch write is work: the agent has moved, so the
    // settle after it is checked again without the user typing anything.
    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-1",
      input: {},
      details: {},
      isError: false,
    });
    await session.stop();
    expect(session.asked).toHaveLength(2);
    // Each check moved the window it reads Dispatch through: the second asks what happened
    // since the first one's snapshot, not since the turn that armed the period, so an ask the
    // agent opened and a human has since answered stops counting against it.
    expect(session.queries).toEqual([
      "?author_session=ses_nudge_rearm",
      "?author_session=ses_nudge_rearm&since=2026-09-13T00%3A00%3A01Z",
      "?author_session=ses_nudge_rearm&since=2026-09-13T00%3A00%3A02Z",
    ]);
  });

  test("neither a dispatch command nor a failed tool call re-arms the check", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-rearm-excluded");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_rearm_no", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);

    // A Dispatch read or write is the agent talking to the humans this nudge is about, not the
    // work it would be nudged for; a tool that failed moved nothing at all.
    await session.dispatch("dispatch read --issue DSP-1", "dispatch_read", { issue: "DSP-1" });
    await session.stop();
    await session.dispatch("dispatch search --query 'saved carts'", "dispatch_search", {
      query: "saved carts",
    });
    await session.stop();
    await session.dispatch(
      "dispatch comment --issue DSP-1 --body-file - <<'EOF'\nLooks right.\nEOF",
      "dispatch_comment",
      { issue: "DSP-1", comment: "c-1" }
    );
    await session.stop();
    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-failed",
      input: { command: "ls" },
      details: {},
      isError: true,
    });
    await session.stop();
    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("a bash command that is not a dispatch command owes a check, ledger or not", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-rearm-ls");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_rearm_ls", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);

    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-ls",
      input: { command: "ls" },
      details: {},
      isError: false,
    });
    await session.stop();
    expect(session.asked).toHaveLength(2);

    // A command that ran `dispatch` beside other work is work, even though the ledger grew.
    await session.dispatch("dispatch search --query x && make", "dispatch_search", {});
    await session.stop();
    expect(session.asked).toHaveLength(3);
  });

  test("a session starts reading the dispatch ledger at its end, so an earlier run's calls never count", async () => {
    // The CLI's ledger from this session's previous life, a resume ago: an ask that run opened.
    appendResult(sessionDirectory(process.env, "ses_nudge_resumed_ledger"), {
      tool: "dispatch_ask",
      details: { ask: "ask-old" },
    });
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-resumed-ledger");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_resumed_ledger", () => ({}));

    await session.userTurn();
    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-ls",
      input: { command: "ls" },
      details: {},
      isError: false,
    });
    await session.stop();
    expect(session.asked).toHaveLength(1);
  });

  test("a malformed ledger line is logged with its file and line and never stalls the check", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-malformed-ledger");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_malformed_ledger", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });
    const ledger = join(
      sessionDirectory(process.env, "ses_nudge_malformed_ledger"),
      "results.jsonl"
    );
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    try {
      await session.userTurn();
      await session.stop();
      expect(session.asked).toHaveLength(1);

      // Two appends that interleaved into one line, then a dispatch ask that opened an ask.
      mkdirSync(dirname(ledger), { recursive: true });
      writeFileSync(ledger, '{"tool":"dispatch_se{"tool":"dispatch_ask"}\n', { flag: "a" });
      await session.dispatch("dispatch ask --issue DSP-1 --question x", "dispatch_ask", {
        issue: "DSP-1",
        ask: "ask-1",
      });
      await session.stop();
      // The ask the agent opened still counts: it spent the check rather than owing another.
      expect(session.asked).toHaveLength(1);

      // A later command reads on from past the bad line, which is named once.
      await session.dispatch("dispatch search --query x", "dispatch_search", {});
      await session.stop();
      expect(session.asked).toHaveLength(1);
    } finally {
      stopSink();
    }
    expect(warnings.filter((warning) => warning.includes(`${ledger}:1:`))).toHaveLength(1);
  });

  test("one armed period pays for at most five checks however much work happens", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-rearm-cap");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_rearm_cap", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    for (let step = 0; step < 9; step += 1) {
      await session.stop();
      await session.toolResult({
        toolName: "bash",
        toolCallId: `call-${step}`,
        input: {},
        details: {},
        isError: false,
      });
    }
    // Nine settles' worth of work, five hidden calls: the cap holds an unattended run's cost.
    expect(session.asked).toHaveLength(5);

    // The budget belongs to the period, so the user's next turn starts a fresh one.
    await session.userTurn("now the next thing");
    await session.stop();
    expect(session.asked).toHaveLength(6);
  });

  test("a host that never answers the self-check is bounded by the extension's own clock", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-hangs");
    // A host that ignores the AbortSignal: the call it started never settles, ever. Without a
    // bound of its own the handler would hold the in-flight latch — and every later check with
    // it — for as long as that call hung.
    const hung = Promise.withResolvers<{ readonly replyText: string }>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_hangs", () => ({}), {
      selfCheck: () => hung.promise,
      selfCheckTimeoutMs: 40,
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toEqual([]);

    // The latch was released, so the next check the period owes still runs.
    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-1",
      input: {},
      details: {},
      isError: false,
    });
    await session.stop();
    expect(session.asked).toHaveLength(2);
    // The first call was told to stop even though its host never listened.
    expect(session.asked[0]?.signal?.aborted).toBe(true);

    // A late answer changes nothing: the check it belonged to was spent long ago.
    hung.resolve({ replyText: "WAITING" });
    await Promise.resolve();
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("checks a fresh TUI whose session id the extension has not healed to yet", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-lazy-id");
    // `session_start` ran before the host minted an id, so the extension's own `sessionID` is
    // empty until the registration heartbeat heals it — up to two minutes of a brand-new
    // terminal. The period carries the id that armed it, which is the live one.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_lazy", () => ({}), {
      lazySessionID: true,
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toHaveLength(1);
  });

  test("work re-arms the check in a fresh TUI too", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-lazy-rearm");
    // The same drift window as above, now on the edge that re-arms: a `tool_result` that read
    // the module's own `sessionID` would match nothing until the heartbeat healed it, so a
    // brand-new terminal got one check for the whole window and none of the re-arming this
    // period is built on.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_lazy_rearm", () => ({}), {
      lazySessionID: true,
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);

    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-1",
      input: {},
      details: {},
      isError: false,
    });
    await session.stop();
    expect(session.asked).toHaveLength(2);
  });

  test("the heartbeat's drift heal leaves a fresh TUI's armed period alone", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-drift-heal");
    // The heal re-establishes the session the extension is already in — it is how a fresh TUI's
    // id reaches the registry at all — so it must not count as a session change. It fires every
    // `ENVOY_HEARTBEAT_MS` (120 s), and a first turn routinely runs longer than that.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_drift", () => ({}), {
      lazySessionID: true,
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.heartbeat(session.nextRegistration());
    // The heal ran: the session the extension started with no id is now registered under one.
    expect(session.registrations).toHaveLength(1);

    await session.stop();
    expect(session.asked).toHaveLength(1);
  });

  test("the drift heal names the live id for the dispatch command and reads that id's ledger", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-drift-ledger");
    // The host minted the id after `session_start` (a fresh TUI's first, or one it moved the
    // session file off to), with no session event after which it refreshes the shell's
    // environment: the heal names the id and refreshes that environment itself.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_drift_ledger", () => ({}), {
      lazySessionID: true,
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });
    expect(process.env.DISPATCH_SESSION_ID).toBeUndefined();
    procmgr.refreshShellConfigCache();

    await session.userTurn();
    await session.heartbeat(session.nextRegistration());
    expect(process.env.DISPATCH_SESSION_ID).toBe("ses_nudge_drift_ledger");
    expect(procmgr.getShellConfig().env.DISPATCH_SESSION_ID).toBe("ses_nudge_drift_ledger");

    // The ask the CLI records under the live id spends the period's check.
    await session.dispatch(
      "dispatch ask --issue DSP-1 --question 'Rotate the token?'",
      "dispatch_ask",
      { issue: "DSP-1", ask: "ask-1" }
    );
    await session.stop();
    expect(session.asked).toEqual([]);
  });

  test("re-establishing the same session during an Envoy outage keeps the armed period", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-nats-retry");
    // NATS is down when the session starts, so the extension retries `establishSession` every
    // `NATS_RETRY_INTERVAL_MS` for the whole outage, each attempt re-entering the restore path
    // a `/fork` uses. The nudge is not an Envoy feature — it reads Dispatch — so an outage must
    // not silently disable it.
    natsState.failConnects = 99;
    const session = await bootAskNudge(envoyExtension, "ses_nudge_outage", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.heartbeat();
    await session.stop();
    expect(session.asked).toHaveLength(1);
  });

  test("a session change resets the period and aborts the check it invalidated", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-switch");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_switch", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const stopped = session.stop();
    await started.promise;
    // A `/fork`, `/handoff` or resume mints a different id: the conversation the pending verdict
    // describes is not the one this session is in any more.
    await session.fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      sessionContext("ses_nudge_forked")
    );
    // The superseded call is told to stop now, not once it answers: the abort is what spares the
    // host a whole-context request nobody will read.
    expect(session.asked[0]?.signal?.aborted).toBe(true);
    release();
    await stopped;

    // The fork's own stale-identity notice is the only thing the session hears.
    expect(
      session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
    ).toEqual([]);
  });

  test("never self-checks in a task subagent's own instance", async () => {
    const fixture = transcriptFixture();
    try {
      // OMP's layout: the subagent's transcript sits inside the parent's directory. Its own
      // extension instance arms no period — `before_agent_start` returns for a subagent — so
      // the stop has nothing to check, with no session-id comparison doing the work.
      const parentFile = fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent.jsonl");
      const childFile = fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl");
      process.env.DISPATCH_URL = "http://dispatch.test";
      process.env.DISPATCH_TOKEN = "token";
      const queries: string[] = [];
      globalThis.fetch = async (input, init) => {
        const url = new URL(input.toString());
        if (url.pathname === "/api/v1/asks/open") {
          queries.push(url.search);
          return new Response(
            JSON.stringify({
              session_id: "ses_child",
              as_of: "2026-09-13T00:00:00Z",
              opened_since: false,
              count: 0,
              waiting_on_human: 0,
              waiting_on_agent: 0,
              asks: [],
            }),
            { headers: { "Content-Type": "application/json" } }
          );
        }
        return responseWithRegistration(input, init, {});
      };
      const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-subagent");
      const asked: string[] = [];
      const parent = createPi();
      envoyExtension(parent.pi);
      await parent.handlers.get("session_start")?.(
        {},
        {
          ...sessionWithTranscript("ses_parent", parentFile, []),
          runEphemeralTurn: async () => ({ replyText: "WAITING" }),
        }
      );
      const child = createPi();
      envoyExtension(child.pi);
      const childContext: SessionContext = {
        ...sessionWithTranscript("ses_child", childFile, []),
        runEphemeralTurn: async () => {
          asked.push("child");
          return { replyText: "WAITING" };
        },
      };
      await child.handlers.get("session_start")?.({}, childContext);

      await child.handlers.get("before_agent_start")?.({ prompt: "scout the repo" }, childContext);
      await child.handlers.get("agent_end")?.(
        { messages: [{ role: "assistant", stopReason: "stop" }] },
        childContext
      );
      expect(asked).toEqual([]);
      expect(child.deliveries).toEqual([]);
      expect(queries).toEqual([]);
    } finally {
      fixture.remove();
    }
  });

  test("stays silent when the self-check answers anything but WAITING", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-unparsed");
    let verdict = "I think I am probably waiting on Sami here.";
    const session = await bootAskNudge(envoyExtension, "ses_nudge_unparsed", () => ({}), {
      selfCheck: async () => ({ replyText: verdict }),
    });

    // A verdict that did not answer the question is not a verdict; silence is the safe reading.
    await session.userTurn();
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);

    // A reply that names both words is the model echoing the choice back, not making it. The
    // steer it would produce asserts "you just said you are waiting", which sends an agent that
    // said no such thing to page a human with a question nobody had.
    for (const echo of ["WAITING or PROCEEDING", "WAITING/PROCEEDING", "WAITING or PROCEEDING?"]) {
      verdict = echo;
      await session.userTurn(`and now ${echo}`);
      await session.stop();
    }
    expect(session.fixture.deliveries).toEqual([]);

    // Markdown emphasis and a trailing clause are still WAITING, as in the phase-stall reply.
    verdict = "**WAITING** — on your call about the schema.";
    await session.userTurn("and now this");
    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);
  });

  test("stays silent when the self-check fails, and logs it once per session", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-selfcheck-fails");
    const warnings: { readonly message: string; readonly context?: unknown }[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push({ message: entry.message, context: entry.context });
    });
    try {
      const session = await bootAskNudge(envoyExtension, "ses_nudge_selfcheck_fails", () => ({}), {
        selfCheck: async ({ signal }) => {
          // What a host reports when the bound below expires, and what a dead provider throws.
          signal?.throwIfAborted();
          throw new Error("provider unavailable");
        },
      });

      await session.userTurn();
      await session.stop();
      expect(session.fixture.deliveries).toEqual([]);

      // A second failing period says nothing new; the log is one line per session.
      await session.userTurn("try again");
      await session.stop();
      expect(session.fixture.deliveries).toEqual([]);
      expect(warnings).toEqual([
        {
          message: expect.stringContaining("self-check failed"),
          context: {
            sessionID: "ses_nudge_selfcheck_fails",
            error: expect.stringContaining("provider unavailable"),
          },
        },
      ]);
    } finally {
      stopSink();
    }
  });

  test("never nudges on a host that cannot ask ephemerally", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-no-ephemeral");
    // The same OMP builds that cannot serve a Dispatch BTW: with no self-check there is no
    // trigger, so the session does not pay the arming round trip either.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_no_ephemeral", () => ({}), {
      sideTurn: false,
    });

    await session.userTurn();
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);
    expect(session.queries).toEqual([]);
  });

  test("stays silent when a new user turn arrives while the self-check is in flight", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-selfcheck-stale");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_selfcheck_stale", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const stopped = session.stop();
    await started.promise;
    // The user typed again while the hidden call was open: the WAITING it is about to answer
    // describes a run the user has already moved past, and steering it would talk over them.
    await session.userTurn("actually, do this instead");
    // And the call the user superseded is told to stop while it is still running: it is a
    // whole-context request, competing with the turn they are waiting on for the same provider.
    expect(session.asked[0]?.signal?.aborted).toBe(true);
    release();
    await stopped;
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("drops the verdict when the agent opened the ask itself while the check was in flight", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-ask-midflight");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_midflight", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const stopped = session.stop();
    await started.promise;
    // An Envoy delivery woke the session while the hidden call was open, and that turn opened
    // the ask. Steering "you have no open ask" at an agent that has just opened one would send
    // it to open a second.
    await session.dispatch(
      "dispatch ask --issue DSP-1 --question 'Rotate the token?'",
      "dispatch_ask",
      { ask: "ask-1" }
    );
    // A verdict it can no longer use is not left running either.
    expect(session.asked[0]?.signal?.aborted).toBe(true);
    release();
    await stopped;
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("a slow arming query does not let a superseded check steer the turn just typed", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-slow-arming");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_slow_arming", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const stopped = session.stop();
    await started.promise;
    // The user types again while the hidden call is open, and Dispatch is slow, so the arming
    // query their turn makes has not answered when the verdict does.
    const arming = Promise.withResolvers<void>();
    const dispatch = globalThis.fetch;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/api/v1/asks/open" && !url.search.includes("since=")) {
        await arming.promise;
      }
      return dispatch(input, init);
    };
    try {
      const typed = session.userTurn("actually, do this instead");
      for (let tick = 0; tick < 20; tick += 1) await Promise.resolve();
      release();
      await stopped;
      expect(
        session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
      ).toEqual([]);
      arming.resolve();
      await typed;
    } finally {
      globalThis.fetch = dispatch;
    }
  });

  test("a run that starts while the check is open is not steered, and its own settle is checked", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-run-midflight");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_run_midflight", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const stopped = session.stop();
    await started.promise;
    // An Envoy delivery wakes the session while the hidden call is open — perhaps the very reply
    // the agent was waiting for. The WAITING about to arrive describes the run before it.
    await session.runStart();
    release();
    await stopped;
    expect(session.fixture.deliveries).toEqual([]);

    // The check still counted, but it did not spend what the period owes: the newer run's own
    // settle is checked, and that verdict is about where the session is now.
    await session.stop();
    expect(session.asked).toHaveLength(2);
    expect(
      session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
    ).toHaveLength(1);
  });

  test("a run that settles while the check is open is checked once the open check ends", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-settle-midflight");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_settle_midflight", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const first = session.stop();
    await started.promise;
    // The woken run is short: it starts and settles inside the check's window, so its stop
    // arrives while the first check still holds the one-at-a-time slot.
    await session.runStart();
    const second = session.stop();
    release();
    await Promise.all([first, second]);

    // Nothing else will settle, so the newer stop is checked as soon as the open check ends —
    // once, with a verdict about the run the session actually finished.
    expect(session.asked).toHaveLength(2);
    expect(
      session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
    ).toHaveLength(1);
  });

  test("a recorded settle that a newer turn has overtaken is not re-checked into that turn", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-settle-overtaken");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_settle_overtaken", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const first = session.stop();
    await started.promise;
    // A woken run works and settles inside the check's window, so its stop is recorded…
    await session.runStart();
    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-1",
      input: {},
      details: {},
      isError: false,
    });
    const woken = session.stop();
    // …and then the user types, before the check comes back. The recorded stop is about a run
    // the user has already moved past: re-checking it would steer into the turn they just typed.
    await session.userTurn("actually, do this instead");
    release();
    await Promise.all([first, woken]);
    expect(
      session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
    ).toEqual([]);
    // Nor does it pay for a Dispatch read: two arming queries and the first stop's, nothing more.
    expect(session.queries).toHaveLength(3);

    // The typed turn's own stop is where the check belongs.
    await session.stop();
    expect(
      session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
    ).toHaveLength(1);
  });

  test("a recorded settle is not re-checked while the turn that overtook it is still arming", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-settle-arming");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const started = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_settle_arming", () => ({}), {
      selfCheck: async () => {
        started.resolve();
        await held;
        return { replyText: "WAITING" };
      },
    });

    await session.userTurn();
    const first = session.stop();
    await started.promise;
    // A woken run works and settles inside the check's window, so its stop is recorded…
    await session.runStart();
    await session.toolResult({
      toolName: "bash",
      toolCallId: "call-1",
      input: {},
      details: {},
      isError: false,
    });
    const woken = session.stop();
    // …then the user types, and the check comes back while that turn's arming read is still
    // out. No run has started, so only the turn's generation marks the recorded stop as
    // overtaken; re-checking it would steer into the turn just typed.
    const arming = session.holdNextArming();
    const typed = session.userTurn("actually, do this instead");
    await arming.out;
    release();
    await Promise.all([first, woken]);
    arming.release();
    await typed;

    expect(session.asked).toHaveLength(1);
    expect(
      session.fixture.deliveries.filter((delivery) => delivery.customType === ASK_REMINDER_TYPE)
    ).toEqual([]);
    expect(session.queries).toHaveLength(3);
  });

  test("a run that starts while Dispatch answers the stop pays for no check", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-run-during-query");
    const query = Promise.withResolvers<void>();
    const queryOut = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_run_during_query", () => ({}), {
      holdStopQuery: () => {
        queryOut.resolve();
        return query.promise;
      },
    });

    await session.userTurn();
    const stopped = session.stop();
    // The run starts only once the stop's query is out: a run before it is the pre-flight's case.
    await queryOut.promise;
    await session.runStart();
    query.resolve();
    await stopped;
    // The conversation a check would read is already past this settle: no model call, and the
    // check the period owes waits for the newer run's settle.
    expect(session.asked).toEqual([]);
    expect(session.fixture.deliveries).toEqual([]);

    await session.stop();
    expect(session.asked).toHaveLength(1);
  });

  test("a check this extension aborts is not logged as a failure", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-abort-quiet");
    const warnings: { readonly message: string; readonly context?: unknown }[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push({ message: entry.message, context: entry.context });
    });
    try {
      const started = Promise.withResolvers<void>();
      let calls = 0;
      const session = await bootAskNudge(envoyExtension, "ses_nudge_abort_quiet", () => ({}), {
        selfCheck: ({ signal }) => {
          calls += 1;
          if (calls > 1) return Promise.reject(new Error("provider unavailable"));
          // A host that honours the signal rejects with its reason, as the pin's does.
          const pending = Promise.withResolvers<{ readonly replyText: string }>();
          signal?.addEventListener("abort", () => pending.reject(signal.reason));
          started.resolve();
          return pending.promise;
        },
      });

      await session.userTurn();
      const stopped = session.stop();
      await started.promise;
      // The user typing supersedes the check: an ordinary event, not a failure to report.
      await session.userTurn("actually, do this instead");
      await stopped;
      expect(warnings).toEqual([]);

      // So the one warning a session gets is still there for the failure that matters.
      await session.stop();
      expect(warnings).toEqual([
        {
          message: expect.stringContaining("self-check failed"),
          context: {
            sessionID: "ses_nudge_abort_quiet",
            error: expect.stringContaining("provider unavailable"),
          },
        },
      ]);
    } finally {
      stopSink();
    }
  });

  test("nudges a WAITING session even when it already holds an open ask", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-held-waiting");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_held_waiting", () => ({
      asks: [{ question: "Which deployment window should I use?" }],
    }));

    await session.userTurn();
    await session.stop();

    expect(session.fixture.deliveries).toEqual([
      expect.objectContaining({ content: UNASKED_WAIT_NUDGE }),
    ]);
    expect(session.asked[0]?.prompt).toContain("Which deployment window should I use?");
  });

  test("leaves a held ask silent when the self-check answers PROCEEDING", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-held-proceeding");
    const session = await bootAskNudge(
      envoyExtension,
      "ses_nudge_held_proceeding",
      () => ({ asks: [{ question: "Which deployment window should I use?" }] }),
      { selfCheck: async () => ({ replyText: "PROCEEDING" }) }
    );

    await session.userTurn();
    await session.stop();

    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("names no open asks or the first five held ask questions in the self-check", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-prompt-asks");
    const longQuestion = `A held question that exceeds the prompt limit: ${"x".repeat(130)}`;
    const heldQuestions = [
      "First held question",
      "Second held question\u2028with details that do not belong in the prompt",
      "Third held question\rwith details that do not belong in the prompt",
      "Fourth held question with\ta bell\u0007character",
      longQuestion,
      "Sixth held question",
    ];
    const held = await bootAskNudge(envoyExtension, "ses_nudge_prompt_held", () => ({
      asks: heldQuestions.map((question) => ({ question })),
    }));
    await held.userTurn();
    await held.stop();

    const heldPrompt = held.asked[0]?.prompt ?? "";
    const askListStart = heldPrompt.indexOf("Your open asks in Dispatch:");
    const askListEnd = heldPrompt.indexOf("\n\nAre you right now waiting");
    expect(heldPrompt.slice(askListStart, askListEnd).split("\n")).toEqual([
      "Your open asks in Dispatch:",
      "- First held question",
      "- Second held question",
      "- Third held question",
      "- Fourth held question with a bell character",
      `- ${longQuestion.slice(0, 119)}…`,
      "+1 more",
    ]);
    expect(heldPrompt).not.toContain("with details that do not belong in the prompt");
    expect(heldPrompt).not.toContain(longQuestion);
    expect(heldPrompt).not.toContain("Sixth held question");
    expect(heldPrompt).toContain("+1 more");

    const none = await bootAskNudge(envoyExtension, "ses_nudge_prompt_none", () => ({}));
    await none.userTurn();
    await none.stop();
    expect(none.asked[0]?.prompt).toContain("There are no open asks");
  });

  test("the ask the agent opens itself spends the check the period owed", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-opens-ask");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_opens", () => ({}));

    await session.userTurn();
    await session.dispatch(
      "dispatch ask --issue DSP-1 --question 'Rotate the token?'",
      "dispatch_ask",
      { issue: "DSP-1", ask: "ask-1" }
    );
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);

    // The next user turn arms a fresh period, and an approval request settles it the same way.
    await session.userTurn("write the spec");
    await session.dispatch(
      "dispatch request-approval --issue DSP-1 --summary 'A live sync replaces the nightly export.'",
      "dispatch_request_approval",
      { issue: "DSP-1", ask: "ask-2" }
    );
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);
    expect(session.asked).toEqual([]);
  });

  // Whether a document edit opened an ask is the server's reading of the document it wrote
  // (`advice.decision_blocks_added`), never a reading of the operations' markdown here: whether an
  // opener is a block depends on where it lands, as code or inside a blockquote or a list item.
  test("a dispatch doc-edit spends the check exactly when the server reports a decision block it added", async () => {
    // The query creates a fresh extension module with isolated module-level awareness state.
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-document-block");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_document_block", () => ({}));
    const editThenStop = async (
      markdown: string,
      advice: Record<string, unknown> | undefined
    ): Promise<void> => {
      await session.userTurn();
      await session.dispatch(
        [
          "dispatch doc-edit --issue DSP-1 --ops-json-file - <<'EOF'",
          JSON.stringify([{ op: "insert", markdown, after: "end" }]),
          "EOF",
        ].join("\n"),
        "dispatch_doc_edit",
        advice === undefined ? { issue: "DSP-1" } : { issue: "DSP-1", advice }
      );
      await session.stop();
    };

    // A block in a blockquote, which the server reads as one: the agent asked, so no check runs.
    await editThenStop("> :::ask{#window}\n> Which deployment window?\n> :::", {
      issue_status: "in_progress",
      decision_blocks_added: 1,
    });
    expect(session.asked).toEqual([]);
    expect(session.fixture.deliveries).toEqual([]);

    // An example fenced four columns into a nested list item, which the server holds as code: the
    // stop checks, and the agent waiting with nothing asked gets the reminder.
    await editThenStop(
      "- Format notes:\n  - Example:\n\n    ```md\n    :::ask{#example}\n    Which?\n    :::\n    ```",
      { issue_status: "in_progress", decision_blocks_added: 0 }
    );
    expect(session.asked).toHaveLength(1);
    expect(session.fixture.deliveries).toEqual([
      expect.objectContaining({ content: UNASKED_WAIT_NUDGE }),
    ]);

    // A Dispatch server predating the count reports none, so the stop still checks.
    await editThenStop(":::ask{#region}\nWhich region?\n:::", undefined);
    expect(session.asked).toHaveLength(2);
    expect(session.fixture.deliveries).toHaveLength(2);
  });

  test("an eval cell during which the ledger grew owes no check, and an ask it opened spends one", async () => {
    // An eval cell reaches `dispatch` only through a shell its code starts, which no scan of the
    // code can read, so the ledger growing during the cell is what says it ran one.
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-eval");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_eval", () => ({}), {
      selfCheck: async () => ({ replyText: "PROCEEDING" }),
    });

    await session.userTurn();
    await session.stop();
    expect(session.asked).toHaveLength(1);

    await session.dispatch(
      'run(["dispatch", "comment", "--issue", "DSP-1", "--body", "Asked above."])',
      "dispatch_comment",
      { issue: "DSP-1", comment: "c-1" },
      "eval"
    );
    await session.stop();
    expect(session.asked).toHaveLength(1);

    // A cell that wrote nothing to the ledger is work.
    await session.toolResult({
      toolName: "eval",
      toolCallId: "call-eval-work",
      input: { language: "py", code: "print(len(open('notes.md').read()))" },
      details: {},
      isError: false,
    });
    await session.stop();
    expect(session.asked).toHaveLength(2);

    await session.userTurn("next");
    await session.dispatch(
      'run(["dispatch", "ask", "--issue", "DSP-1", "--question", "Rotate the token?"])',
      "dispatch_ask",
      { issue: "DSP-1", ask: "ask-1" },
      "eval"
    );
    await session.stop();
    expect(session.asked).toHaveLength(2);
  });

  // `--help` prints a command's flags and calls nothing, so the CLI's ledger gains no line and the
  // call opens no ask: it must not spend the check the turn owes.
  test("dispatch ask --help or dispatch request-approval --help spends no check", async () => {
    for (const command of ["ask", "request-approval"]) {
      const { default: envoyExtension } = await import(`./envoy.ts?ask-nudge-help-${command}`);
      const session = await bootAskNudge(envoyExtension, `ses_nudge_help_${command}`, () => ({}));
      await session.userTurn();
      await session.toolResult({
        toolName: "bash",
        toolCallId: `call-help-${command}`,
        input: { command: `dispatch ${command} --help` },
        details: {},
        isError: false,
      });
      await session.stop();
      expect(session.fixture.deliveries).toEqual([
        expect.objectContaining({ content: UNASKED_WAIT_NUDGE }),
      ]);
    }
  });

  test("a document written through dispatch issue or dispatch artifact spends the check only when it holds a decision block", async () => {
    // The query creates a fresh extension module with isolated module-level awareness state.
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-document-upload");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_document_upload", () => ({}));

    // The server's count, which a `--path` upload carries too, not the command's text.
    await session.userTurn();
    await session.dispatch(
      "dispatch issue --project DSP --title 'Saved carts' --spec-file spec.md",
      "dispatch_issue",
      { issue: "DSP-1", advice: { decision_blocks: 1 } }
    );
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);

    await session.userTurn();
    await session.dispatch(
      "dispatch artifact --issue DSP-1 --name spec.md --path /tmp/spec.md",
      "dispatch_artifact",
      { issue: "DSP-1", advice: { decision_blocks: 2 } }
    );
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);

    await session.userTurn();
    await session.dispatch(
      "dispatch artifact --issue DSP-1 --name notes.md --content Notes.",
      "dispatch_artifact",
      { issue: "DSP-1", advice: { decision_blocks: 0 } }
    );
    await session.stop();
    expect(session.fixture.deliveries).toEqual([
      expect.objectContaining({ content: UNASKED_WAIT_NUDGE }),
    ]);
  });

  test("the five-check period budget applies while the session holds open asks", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-held-budget");
    const session = await bootAskNudge(
      envoyExtension,
      "ses_nudge_held_budget",
      () => ({ asks: [{ question: "Which deployment window should I use?" }] }),
      { selfCheck: async () => ({ replyText: "PROCEEDING" }) }
    );

    await session.userTurn();
    for (let step = 0; step < 9; step += 1) {
      await session.stop();
      await session.toolResult({
        toolName: "bash",
        toolCallId: `call-${step}`,
        input: {},
        details: {},
        isError: false,
      });
    }

    expect(session.asked).toHaveLength(5);
    expect(session.fixture.deliveries).toEqual([]);
  });

  test("waits for the real stop when OMP has already scheduled a continuation", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-will-continue");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_continue", () => ({}));

    await session.userTurn();
    await session.stop({ willContinue: true });
    expect(session.fixture.deliveries).toEqual([]);

    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);
  });

  test("never nudges a Legion-managed session", async () => {
    const extension = await import("./envoy.ts?ask-nudge-legion");
    const session = await bootAskNudge(extension.default, "ses_nudge_legion", () => ({}));
    await claimEnvoyRole("ses_nudge_legion", "legion-project-issue-worker", session.context);

    await session.userTurn("complete the handoff");
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);
    // Neither edge asks Dispatch anything, and no hidden model call is made: a session that
    // cannot be nudged does not pay the arming round trip either.
    expect(session.queries).toEqual([]);
    expect(session.asked).toEqual([]);
    // The claim is recorded in the transcript, which is the only thing a fresh process can read.
    expect(session.fixture.entries).toContainEqual({
      type: "custom",
      customType: "legion-managed-session",
      data: { session_id: "ses_nudge_legion" },
    });
  });

  test("a transcript that records Legion driving the session excludes it under any id", async () => {
    // A fresh process has only the transcript; a `/fork` or `/handoff` then carries that
    // transcript into a newly minted id while the session stays Legion-driven, so the entry is
    // never matched against the id the guard runs under.
    for (const recordedID of ["ses_nudge_restored", "ses_nudge_before_the_fork"]) {
      const extension = await import(`./envoy.ts?ask-nudge-legion-resume-${recordedID}`);
      const session = await bootAskNudge(extension.default, "ses_nudge_restored", () => ({}), {
        branch: [
          {
            type: "custom",
            customType: "legion-managed-session",
            data: { session_id: recordedID },
          },
        ],
      });

      await session.userTurn("carry on with the issue");
      await session.stop();
      expect(session.fixture.deliveries).toEqual([]);
      expect(session.queries).toEqual([]);
      expect(session.asked).toEqual([]);
    }
  });

  test("stays silent when Dispatch could not be reached this turn", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-unreachable");
    let reachable = false;
    const session = await bootAskNudge(envoyExtension, "ses_nudge_offline", () => {
      if (!reachable) throw new Error("network offline");
      return {};
    });

    await session.userTurn();
    reachable = true;
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);
    expect(session.asked).toEqual([]);
  });

  test("does not arm a period for a turn that carries no user text", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-synthetic");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_synthetic", () => ({}));

    await session.userTurn();
    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);

    // Whatever the host prepares without the user in it cannot re-arm the nudge — least of
    // all the nudge's own steered turn, or the session would talk to itself forever.
    await session.userTurn("");
    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);
  });

  test("a session that starts cold cannot nudge until a user turn arms a period", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-resume");
    // The arming period never reaches the transcript, so a resumed session restores none: it
    // begins at period 0, which the stop guard refuses outright — it does not even ask
    // Dispatch. A prior process's own nudge in the branch changes nothing.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_resume", () => ({}), {
      branch: [
        {
          type: "custom_message",
          customType: "dispatch-ask-reminder",
          content: UNASKED_WAIT_NUDGE,
        },
      ],
    });

    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);
    expect(session.queries).toEqual([]);

    // The user's first turn after the resume arms the next period, which nudges as usual.
    await session.userTurn();
    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);
  });

  test("a second stop inside one Dispatch round trip is one nudge and one query", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-race");
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const session = await bootAskNudge(envoyExtension, "ses_nudge_race", () => ({}), {
      holdStopQuery: () => held,
    });

    await session.userTurn();
    // The host does not await `agent_end`, so a second stop can arrive while the first one's
    // query is still in flight — an inbound steer or a host continuation starts a turn that
    // ends on a bare reply well inside the three-second window. Both nudging is what "one per
    // stop" forbids; both querying is a Dispatch round trip whose answer can change nothing.
    const first = session.stop();
    const second = session.stop();
    release();
    await Promise.all([first, second]);

    expect(session.fixture.deliveries).toHaveLength(1);
    // The second stop returned on the latch without asking Dispatch anything.
    expect(session.queries).toEqual([
      "?author_session=ses_nudge_race",
      "?author_session=ses_nudge_race&since=2026-09-13T00%3A00%3A01Z",
    ]);
  });

  test("never nudges a headless run the host gave no UI context", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-headless");
    // `omp -p`: the host disposes the session when its one run ends and has already printed its
    // output, so a steered continuation is either aborted or billed for text nobody reads.
    const session = await bootAskNudge(envoyExtension, "ses_nudge_headless", () => ({}), {
      hasUI: false,
    });

    await session.userTurn();
    await session.stop();
    expect(session.fixture.deliveries).toEqual([]);
    // Neither edge asks Dispatch anything and no hidden model call is made: a headless run does
    // not pay the arming round trip.
    expect(session.queries).toEqual([]);
    expect(session.asked).toEqual([]);
  });

  test("never nudges a stop the run did not settle normally", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?ask-nudge-unsettled");
    const session = await bootAskNudge(envoyExtension, "ses_nudge_unsettled", () => ({}));

    await session.userTurn();
    // An interrupt, a provider failure, a truncation, and a run with no reply of its own:
    // steering any of them answers the user's cancel, or a failure, with a turn nobody asked for.
    for (const messages of [
      [{ role: "assistant", stopReason: "aborted" }],
      [
        { role: "assistant", stopReason: "stop" },
        { role: "assistant", stopReason: "error" },
      ],
      [{ role: "assistant", stopReason: "length" }],
      [{ role: "user" }],
    ]) {
      await session.stop({ messages });
    }
    expect(session.fixture.deliveries).toEqual([]);
    // Only the arming query ran: an unsettled stop asks Dispatch nothing, asks the model
    // nothing, and latches nothing.
    expect(session.queries).toHaveLength(1);
    expect(session.asked).toEqual([]);

    await session.stop();
    expect(session.fixture.deliveries).toHaveLength(1);
  });

  test("registers the shared eight-tool contract and delegates HTTP operations to EnvoyClient", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/v1/roles/set") {
        const role = typeof body?.role === "string" ? body.role : "";
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_omp"],
        });
      }
      if (url.pathname === "/v1/sessions") return response([]);
      if (url.pathname === "/v1/interests/ses_omp")
        return response({ session_id: "ses_omp", machine_id: "test", dir: "/tmp", topics: [] });
      return response({
        event_id: "evt_1",
        source: "agent",
        source_event_id: "evt_1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "dedupe_1",
        issued_at: 1,
        payload_summary: "message",
        trace_id: "trace_1",
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());

    expect(
      fixture.tools
        .filter((tool) => tool.name.startsWith("envoy_") || tool.name === "slack_post")
        .map((tool) => ({ name: tool.name, description: tool.description }))
    ).toEqual(envoyToolSpecs.map((spec) => ({ name: spec.name, description: spec.description })));

    await fixture.tools
      .find((tool) => tool.name === "envoy_role_set")
      ?.execute("", { role: "controller" });
    await fixture.tools.find((tool) => tool.name === "envoy_list")?.execute("", {});
    const sent = await fixture.tools
      .find((tool) => tool.name === "envoy_send")
      ?.execute("", {
        session_id: "ses_target",
        message: "direct",
        in_reply_to: "event-before",
        supersedes: "event-obsolete",
        urgency: "blocking",
        expects_reply: "required",
        expires_at: 1_788_956_000_000,
      });
    expect(sent?.content[0]?.text).toBe(
      "sent evt_1 to ses_target (recipient unconfirmed by listener)"
    );
    expect(sent?.details).toMatchObject({
      event_id: "evt_1",
      recipient: "ses_target",
      confirmed: false,
    });
    await fixture.tools
      .find((tool) => tool.name === "envoy_publish")
      ?.execute("", {
        topic: "team.test",
        message: "broadcast",
      });
    await fixture.tools.find((tool) => tool.name === "envoy_sessions")?.execute("", {});

    expect(requests).toEqual([
      // session_start registers the direct subject before tool calls.
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_omp",
          dir: "/tmp/envoy-omp-test",
          topics: ["notifications.agent.ses_omp"],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
          capabilities: ["aside", "steer"],
          host: machineID(),
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_omp", role: "controller" } },
      { path: "/v1/interests/ses_omp", body: undefined },
      {
        path: "/v1/messages/send",
        body: {
          source: "agent",
          source_session: "ses_omp",
          target_session: "ses_target",
          message: "direct",
          in_reply_to: "event-before",
          supersedes: "event-obsolete",
          urgency: "blocking",
          expects_reply: "required",
          expires_at: 1_788_956_000_000,
          idempotency_key: expect.any(String),
        },
      },
      {
        path: "/v1/messages/publish",
        body: {
          source: "agent",
          source_session: "ses_omp",
          topic: "team.test",
          message: "broadcast",
          idempotency_key: expect.any(String),
        },
      },
      { path: "/v1/sessions", body: undefined },
    ]);
  });

  test("builds every tool parameter schema through the injected pi.zod, never the bundled zod", async () => {
    // OMP's converter reads internals only its own Zod produces; a field built from this
    // package's zod import registers here but fails to load in a real session. The fixture
    // normally hands the extension the same zod the contract imports, which cannot tell the
    // two apart, so this pi.zod tracks every schema it (or a chained call on one) creates.
    const built = new WeakSet<object>();
    const track = <Schema extends object>(schema: Schema): Schema => {
      const proxy = new Proxy(schema, {
        get(target, property, receiver) {
          const value: unknown = Reflect.get(target, property, receiver);
          if (typeof value !== "function") return value;
          return (...args: unknown[]) => {
            const result: unknown = Reflect.apply(value, target, args);
            return result instanceof z.ZodType ? track(result) : result;
          };
        },
      });
      built.add(proxy);
      return proxy;
    };
    const foreign: string[] = [];
    const injected = {
      ...z,
      object: (shape: z.ZodRawShape) => {
        for (const field of Object.values(shape)) {
          if (!built.has(field)) foreign.push("object field");
        }
        return track(z.object(shape));
      },
      string: () => track(z.string()),
      number: () => track(z.number()),
      boolean: () => track(z.boolean()),
      array: (item: z.ZodType) => {
        if (!built.has(item)) foreign.push("array element");
        return track(z.array(item));
      },
      enum: (values: readonly [string, ...string[]]) => track(z.enum(values)),
      unknown: () => track(z.unknown()),
    } as unknown as typeof z;
    const fixture = createPi({ zod: injected });
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const { default: envoyExtension } = await import("./envoy.ts?injected-zod-identity");
    envoyExtension(fixture.pi);

    for (const tool of fixture.tools) {
      const parameters = tool.parameters as z.ZodObject<z.ZodRawShape>;
      if (!built.has(parameters)) foreign.push(tool.name);
      for (const [key, field] of Object.entries(parameters.shape)) {
        if (!built.has(field)) foreign.push(`${tool.name}.${key}`);
      }
    }
    expect(foreign).toEqual([]);
  });

  test("uses the shared metadata schema to reject an invalid urgency", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?shared-metadata-validation");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const send = fixture.tools.find((tool) => tool.name === "envoy_send");
    const spec = envoyToolSpecs.find((candidate) => candidate.name === "envoy_send");
    if (send === undefined || spec === undefined) throw new Error("envoy_send was not registered");

    const input = { session_id: "ses_target", message: "direct", urgency: "urgent" };
    const expected = z.object(spec.arguments(z) as unknown as z.ZodRawShape).safeParse(input);
    const actual = (send.parameters as z.ZodType).safeParse(input);
    if (expected.success || actual.success) throw new Error("invalid urgency unexpectedly parsed");

    expect(actual.error.message).toBe(expected.error.message);
  });

  test("registers every Envoy tool with lenient host validation so execute is the one error surface", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const { default: envoyExtension } = await import("./envoy.ts?lenient-arg-validation");
    const fixture = createPi();
    envoyExtension(fixture.pi);

    expect(fixture.tools.map((tool) => tool.name)).toEqual(envoyToolSpecs.map((spec) => spec.name));
    expect(fixture.tools.filter((tool) => tool.lenientArgValidation !== true)).toEqual([]);
  });

  test("refuses an Envoy tool call once with every problem, before reaching the listener", async () => {
    let requests = 0;
    globalThis.fetch = async () => {
      requests += 1;
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?envoy-tool-one-pass");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    requests = 0;
    const send = fixture.tools.find((tool) => tool.name === "envoy_send");
    if (send === undefined) throw new Error("envoy_send was not registered");

    const result = await send.execute("", {
      session_id: "ses_target",
      message: "hello",
      expects_reply: "no",
      urgent: true,
    });

    expect(result.isError).toBe(true);
    expect(result.content[0]?.text).toBe(
      [
        "envoy_send was not called: 2 problems",
        '- expects_reply must be one of none|optional|required; got "no"',
        '- unknown field "urgent"; allowed: session_id, message, in_reply_to, supersedes, urgency, expects_reply, expires_at',
      ].join("\n")
    );
    expect(requests).toBe(0);
  });

  test("surfaces a missing target session as an envoy_send tool error", async () => {
    globalThis.fetch = async (input) => {
      if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_omp"],
        });
      }
      return new Response(JSON.stringify({ error: "no live session ses_missing" }), {
        status: 404,
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?missing-target-error");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const send = fixture.tools.find((tool) => tool.name === "envoy_send");
    if (send === undefined) throw new Error("envoy_send was not registered");

    const result = await send.execute("", { session_id: "ses_missing", message: "hello" });

    expect(result.isError).toBe(true);
    expect(result.content[0]?.text).toContain("no live session ses_missing");
  });

  // slack_post's broker and Slack, as the session's process meets them: the broker's client on
  // PATH (a stand-in that runs the poster with the granted token, as the exec form does once the
  // broker grants LEGION_SLACK_BOT_TOKEN), and a fake Slack Web API at SLACK_POST_API_URL.
  async function slackPostRig(answer: unknown) {
    const dir = mkdtempSync(join(tmpdir(), "pi-envoy-slack-"));
    const client = join(dir, "agent-secrets");
    writeFileSync(
      client,
      '#!/bin/sh\nwhile [ "$1" != "--" ]; do shift; done\nshift\nLEGION_SLACK_BOT_TOKEN=xoxb-granted exec "$@"\n'
    );
    chmodSync(client, 0o755);
    const posts: { authorization: string | null; body: unknown }[] = [];
    const slack = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      async fetch(request) {
        posts.push({
          authorization: request.headers.get("authorization"),
          body: await request.json(),
        });
        return Response.json(answer);
      },
    });
    process.env.PATH = `${dir}${delimiter}${process.env.PATH ?? ""}`;
    process.env.SLACK_POST_API_URL = `http://127.0.0.1:${slack.port}/api`;
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import(`./envoy.ts?slack-post-${slack.port}`);
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const tool = fixture.tools.find((candidate) => candidate.name === "slack_post");
    if (tool === undefined) throw new Error("slack_post was not registered");
    return {
      tool,
      posts,
      stop() {
        slack.stop(true);
        delete process.env.SLACK_POST_API_URL;
        rmSync(dir, { recursive: true, force: true });
      },
    };
  }

  test("slack_post replies in the thread with the broker's token and names the posted ts", async () => {
    const rig = await slackPostRig({ ok: true, channel: "C00000000", ts: "1700000000.000200" });
    try {
      const result = await rig.tool.execute("", {
        channel: "C00000000",
        thread_ts: "1700000000.000100",
        text: "Filed as ACME-12.",
      });

      expect(result.isError).not.toBe(true);
      expect(result.content[0]?.text).toBe(
        "posted 1700000000.000200 in C00000000, thread 1700000000.000100"
      );
      expect(result.details).toEqual({
        channel: "C00000000",
        thread_ts: "1700000000.000100",
        ts: "1700000000.000200",
      });
      expect(rig.posts).toEqual([
        {
          authorization: "Bearer xoxb-granted",
          body: { channel: "C00000000", thread_ts: "1700000000.000100", text: "Filed as ACME-12." },
        },
      ]);
    } finally {
      rig.stop();
    }
  });

  test("slack_post surfaces Slack's refusal verbatim and posts once", async () => {
    const rig = await slackPostRig({ ok: false, error: "not_in_channel" });
    try {
      const result = await rig.tool.execute("", {
        channel: "C00000000",
        thread_ts: "1700000000.000100",
        text: "hello",
      });

      expect(result.isError).toBe(true);
      expect(result.content[0]?.text).toContain("Slack refused chat.postMessage: not_in_channel");
      expect(rig.posts).toHaveLength(1);
    } finally {
      rig.stop();
    }
  });

  test("rejects malformed wildcard bases before opening subscriptions", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?malformed-wildcard-base");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const subscribe = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribe === undefined) throw new Error("subscription tool was not registered");

    for (const topic of ["a..b.>", "a.>"]) {
      const result = await subscribe.execute("", { topics: [topic] });
      expect(result.isError).toBe(true);
      expect(result.content[0]?.text).toContain("concrete base");
      expect(natsState.controls.has(topic)).toBe(false);
    }
  });

  test("a resumed session re-subscribes its persisted interests and re-claims its held role", async () => {
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_omp") {
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: [
            "notifications.agent.ses_omp",
            "notifications.github.sjawhar.legion.issue.91.>",
            "notifications.role.legion-controller",
          ],
        });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?interest-recovery");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    const resumed = {
      ...sessionContext(),
      sessionManager: { ...sessionContext().sessionManager, getBranch: () => [{}] },
    };
    await fixture.handlers.get("session_start")?.({}, resumed);
    expect(natsState.controls.has("notifications.github.sjawhar.legion.issue.91.>")).toBe(true);
    expect(natsState.controls.has("notifications.github.sjawhar.legion.issue.91")).toBe(true);

    // Role deliveries arrive on the agent subject; a role is never a NATS
    // subscription of its own. It is a server-side claim re-asserted by the
    // heartbeat, not an `envoy_unsubscribe` target.
    expect(natsState.controls.has("notifications.role.legion-controller")).toBe(false);
    expect(roleClaims).toEqual([{ session_id: "ses_omp", role: "legion-controller" }]);
  });

  test("a session rebind moves a held role from the dead id to the new one", async () => {
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_before") {
        return response({
          session_id: "ses_before",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_before", "notifications.role.release-captain"],
        });
      }
      // The new id has no registry row yet — exactly the post-branch state.
      if (url.pathname === "/v1/interests/ses_after") return response({ error: "not found" });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-rebind");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "release-captain" });
    expect(roleClaims).toEqual([{ session_id: "ses_before", role: "release-captain" }]);

    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after"));

    expect(roleClaims).toEqual([
      { session_id: "ses_before", role: "release-captain" },
      { session_id: "ses_after", role: "release-captain" },
    ]);
    // The id-change notice is the extension's own, not an inbound event: the Legion phase-stall
    // check skips it.
    expect(fixture.deliveries.map((delivery) => delivery.details)).toContainEqual(
      LOCAL_ENVOY_NOTICE
    );
  });

  test("a rebind re-claims a held role from memory when the registry rows are gone", async () => {
    const registeredSessions = new Set<string>();
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id?: unknown;
        };
        if (typeof body.session_id === "string") registeredSessions.add(body.session_id);
        return responseWithRegistration(input, init, {});
      }
      if (url.pathname === "/v1/interests/ses_before_memory")
        return response({ error: "nats: key not found" });
      if (url.pathname === "/v1/interests/ses_after_memory")
        return response({ error: "nats: key not found" });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id?: unknown;
          readonly role?: unknown;
        };
        if (typeof body.session_id !== "string" || typeof body.role !== "string") {
          return new Response(JSON.stringify({ error: "invalid role claim" }), { status: 400 });
        }
        if (!registeredSessions.has(body.session_id)) {
          return new Response(JSON.stringify({ error: "session is not registered" }), {
            status: 404,
          });
        }
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-memory-rebind");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_memory"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "release-captain" });
    expect(roleClaims).toEqual([{ session_id: "ses_before_memory", role: "release-captain" }]);

    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_memory"));

    expect(roleClaims).toEqual([
      { session_id: "ses_before_memory", role: "release-captain" },
      { session_id: "ses_after_memory", role: "release-captain" },
    ]);
  });

  test("new and resume switches do not carry the outgoing session role", async () => {
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        return responseWithRegistration(input, init, {});
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      if (url.pathname.startsWith("/v1/interests/")) {
        const session = url.pathname.slice("/v1/interests/".length);
        const topics =
          session === "ses_loaded"
            ? [`notifications.agent.${session}`, "notifications.role.reviewer"]
            : [`notifications.agent.${session}`];
        return response({
          session_id: session,
          machine_id: "test",
          dir: "/tmp",
          topics,
        });
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-new-resume-switch");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_outgoing"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "release-captain" });
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, sessionContext("ses_fresh"));
    await roleTool?.execute("", { role: "reviewer" });
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      {
        ...sessionContext("ses_loaded"),
        sessionManager: {
          ...sessionContext("ses_loaded").sessionManager,
          getBranch: () => [
            { type: "custom", customType: "envoy-role-claim", data: { role: "sre" } },
          ],
        },
      }
    );

    // The role held by ses_outgoing does not follow a `new` switch, and the
    // `resume` switch claims what the loaded transcript itself recorded (sre),
    // not what the outgoing process remembered (reviewer) or what a stale
    // listener row for the loaded id says (reviewer).
    expect(roleClaims).toEqual([
      { session_id: "ses_outgoing", role: "release-captain" },
      { session_id: "ses_fresh", role: "reviewer" },
      { session_id: "ses_loaded", role: "sre" },
    ]);
  });

  test("resume after listener reaping re-claims the role from the transcript", async () => {
    // Life 1: claim a role. The listener later reaps the dead session's rows,
    // so life 2 finds no interest row at all — only the transcript remembers.
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_reaped")
        return response({ error: "nats: key not found" });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: firstLife } = await import("./envoy.ts?role-reaped-life-1");
    const first = createPi();
    firstLife(first.pi);
    await first.handlers.get("session_start")?.({}, sessionContext("ses_reaped"));
    await first.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role: "sre" });
    expect(roleClaims).toEqual([{ session_id: "ses_reaped", role: "sre" }]);

    const { default: secondLife } = await import("./envoy.ts?role-reaped-life-2");
    const second = createPi();
    secondLife(second.pi);
    const resumed = {
      ...sessionContext("ses_reaped"),
      sessionManager: {
        ...sessionContext("ses_reaped").sessionManager,
        getBranch: () => first.entries,
      },
    };
    await second.handlers.get("session_start")?.({}, resumed);

    expect(roleClaims).toEqual([
      { session_id: "ses_reaped", role: "sre" },
      { session_id: "ses_reaped", role: "sre" },
    ]);
  });

  test("automatic reclaim is a soft claim: a live holder's 409 is honoured, an explicit claim stays hard", async () => {
    // The parent of a /fork: its transcript still records `release-captain`, but the
    // fork moved the live claim to the child. The listener arbitrates: the
    // extension sends soft:true and the listener answers 409 with the holder.
    const roleClaims: { readonly session_id: string; readonly soft: boolean | undefined }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_fork_parent") {
        return response({ error: "nats: key not found" });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, soft: body.soft });
        if (body.soft === true) {
          return Response.json(
            {
              error: "role release-captain is held by ses_fork_child",
              role: "release-captain",
              holder: "ses_fork_child",
            },
            { status: 409 }
          );
        }
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-no-steal");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const resumedParent = {
      ...sessionContext("ses_fork_parent"),
      sessionManager: {
        ...sessionContext("ses_fork_parent").sessionManager,
        getBranch: () => [
          { type: "custom", customType: "envoy-role-claim", data: { role: "release-captain" } },
        ],
      },
    };
    await fixture.handlers.get("session_start")?.({}, resumedParent);

    // One attempt, soft, refused — and the parent does not believe it holds
    // the role afterwards.
    expect(roleClaims).toEqual([{ session_id: "ses_fork_parent", soft: true }]);
    const unsubscribe = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const refused = await unsubscribe?.execute("", {
      topics: ["notifications.role.release-captain"],
    });
    expect(refused?.content[0]?.text).toBe("Unsubscribed: (none)");

    // The user's explicit envoy_role_set is a hard claim and takes it.
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "release-captain" });
    expect(roleClaims).toEqual([
      { session_id: "ses_fork_parent", soft: true },
      { session_id: "ses_fork_parent", soft: undefined },
    ]);
  });

  test("registers no dispatch_* tool where Dispatch is configured: agents run the dispatch command", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const { default: envoyExtension } = await import("./envoy.ts?no-dispatch-tools");
    const fixture = createPi();

    envoyExtension(fixture.pi);

    expect(fixture.tools.filter((tool) => tool.name.startsWith("dispatch_"))).toEqual([]);
    expect(fixture.tools.map((tool) => tool.name)).toEqual(envoyToolSpecs.map((spec) => spec.name));
  });

  test("puts the plugin's dispatch command first on PATH and names Oh My Pi as its host", async () => {
    // A PATH without the plugin's bin, so the load is what puts it first (afterEach restores it).
    process.env.PATH = "/usr/bin:/bin";
    await import("./envoy.ts?plugin-bin-path");

    const first = (process.env.PATH ?? "").split(delimiter)[0] ?? "";
    expect(first).toBe(join(import.meta.dir, "..", "bin"));
    const shim = statSync(join(first, "dispatch"));
    expect(shim.isFile()).toBe(true);
    expect(shim.mode & 0o111).toBe(0o111);
    expect(process.env.DISPATCH_HOST).toBe("omp");
  });

  test("names the top-level session in DISPATCH_SESSION_ID at start and after a switch", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?dispatch-session-id");
    const fixture = createPi();
    envoyExtension(fixture.pi);

    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_dispatch_env"));
    expect(process.env.DISPATCH_SESSION_ID).toBe("ses_dispatch_env");

    await fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      sessionContext("ses_dispatch_env_new")
    );
    expect(process.env.DISPATCH_SESSION_ID).toBe("ses_dispatch_env_new");
  });

  /** A context for `ses_title` whose live session name is `name()`, as agent `kind` runs it. */
  const titledContext = (kind: "main" | "sub", name: () => string | undefined): SessionContext => ({
    ...sessionContext("ses_title"),
    agent: { kind },
    sessionManager: { ...topLevelSession, getSessionId: () => "ses_title", getSessionName: name },
  });

  test("the main agent's shell command finds the session's current title in its title file", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?title-file-main");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    let title: string | undefined = "Saved carts";
    const context = titledContext("main", () => title);
    await fixture.handlers.get("session_start")?.({}, context);
    const toolCall = fixture.handlers.get("tool_call");
    if (toolCall === undefined) throw new Error("tool_call was not registered");
    const titleFile = () => readSessionTitle(sessionDirectory(process.env, "ses_title"));

    // Only a tool that runs a shell needs it.
    await toolCall({ toolName: "read", toolCallId: "call-read", input: { path: "x" } }, context);
    expect(titleFile()).toBeUndefined();
    await toolCall(
      { toolName: "bash", toolCallId: "call-bash", input: { command: "dispatch whoami" } },
      context
    );
    expect(titleFile()).toBe("Saved carts");
    // The session is renamed after it started; the next shell command, an eval cell's too, sees it.
    title = "Saved carts, renamed";
    await toolCall(
      { toolName: "eval", toolCallId: "call-eval", input: { language: "py", code: "1" } },
      context
    );
    expect(titleFile()).toBe("Saved carts, renamed");
  });

  test("a task subagent's shell command writes no title file: the session it names is its parent's", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?title-file-sub");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.(
      {},
      titledContext("main", () => "Parent")
    );
    const toolCall = fixture.handlers.get("tool_call");
    if (toolCall === undefined) throw new Error("tool_call was not registered");

    await toolCall(
      { toolName: "bash", toolCallId: "call-sub", input: { command: "dispatch whoami" } },
      titledContext("sub", () => "Scout the repo")
    );
    expect(readSessionTitle(sessionDirectory(process.env, "ses_title"))).toBeUndefined();
  });

  test("reports a broken Dispatch configuration once at session start", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    delete process.env.DISPATCH_TOKEN;
    process.env.HOME = "/nonexistent-home-for-dispatch-gating";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?native-dispatch-disabled");
    const fixture = createPi();
    envoyExtension(fixture.pi);

    expect(fixture.tools.map((tool) => tool.name)).toEqual(envoyToolSpecs.map((spec) => spec.name));
    const notifications: string[] = [];
    await fixture.handlers.get("session_start")?.(
      {},
      {
        ...sessionContext(),
        ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
      }
    );
    expect(notifications).toEqual([
      `envoy: Dispatch is off: DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${process.env.ENVOY_CONFIG_FILE}`,
    ]);
  });

  describe("the session's envoy.json", () => {
    /** A context for `sessionID` whose notifications land in `notifications`. */
    const notifying = (sessionID: string, notifications: string[]): SessionContext => ({
      ...sessionContext(sessionID),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    });
    const userTurn = { messages: [{ role: "user", content: "Fix the bug.", timestamp: 1 }] };

    test("envoy.json: a named profile with none keeps the old user file unread and says where it belongs", async () => {
      const home = mkdtempSync(join(tmpdir(), "envoy-profile-home-"));
      const agentDirectory = mkdtempSync(join(tmpdir(), "envoy-profile-"));
      const marker = join(home, "old-token-ran");
      const old = join(home, ".config", "opencode", "envoy.json");
      process.env.HOME = home;
      process.env.OMP_PROFILE = "work";
      process.env.PI_CODING_AGENT_DIR = agentDirectory;
      mkdirSync(dirname(old), { recursive: true });
      writeFileSync(
        old,
        JSON.stringify({
          dispatch: {
            enabled: true,
            serverUrl: "https://old.dispatch.example",
            token: `!touch ${marker}; echo old-token`,
          },
        })
      );
      delete process.env.ENVOY_URL;
      delete process.env.ENVOY_NATS_URL;
      const requests: string[] = [];
      globalThis.fetch = async (input) => {
        requests.push(input.toString());
        return new Response("no request belongs here", { status: 500 });
      };
      try {
        const { default: envoyExtension } = await import("./envoy.ts?envoy-json-profile");
        const fixture = createPi();
        envoyExtension(fixture.pi);
        const file = join(agentDirectory, "envoy.json");
        const notifications: string[] = [];
        const context = notifying("ses_profile", notifications);
        await fixture.handlers.get("session_start")?.({}, context);

        expect(process.env.ENVOY_CONFIG_FILE).toBe(file);
        expect(fixture.tools).toEqual([]);
        expect(fixture.commands.map((command) => command.name)).not.toContain("whoami");
        expect(notifications).toEqual([
          `envoy: no envoy.json at ${file}: Dispatch and Envoy are off; ${old} is not read for this profile; copy or link it to ${file}, or write that file`,
        ]);
        expect(existsSync(marker)).toBe(false);
        // A Legion claim reaches the listener through the one accessor, which refuses it.
        await expect(
          claimEnvoyRole("ses_profile", "legion-omp-repo-1-implementer", context)
        ).rejects.toThrow(
          `Envoy is off for this session: no envoy.json at ${file}: Envoy is off; ${old} is not read for this profile; copy or link it to ${file}, or write that file`
        );
        await fixture.handlers.get("session_shutdown")?.({}, context);
        expect(requests).toEqual([]);
        expect(natsState.connectedNames).toEqual([]);
      } finally {
        rmSync(home, { recursive: true, force: true });
        rmSync(agentDirectory, { recursive: true, force: true });
      }
    });

    test("envoy.json: an envoy section on and dispatch off registers the Envoy tools and leaves dispatch-first out", async () => {
      delete process.env.ENVOY_URL;
      delete process.env.ENVOY_NATS_URL;
      writeFileSync(
        process.env.ENVOY_CONFIG_FILE ?? "",
        JSON.stringify({
          dispatch: { enabled: false },
          envoy: {
            enabled: true,
            url: "http://envoy-from-file.test",
            natsUrls: ["nats://nats-under-test:4222"],
          },
        })
      );
      const hosts: string[] = [];
      globalThis.fetch = async (input, init) => {
        hosts.push(new URL(input.toString()).host);
        return responseWithRegistration(input, init, []);
      };
      const { default: envoyExtension } = await import("./envoy.ts?envoy-json-envoy-only");
      const fixture = createPi();
      envoyExtension(fixture.pi);
      const notifications: string[] = [];
      await fixture.handlers.get("session_start")?.({}, notifying("ses_envoy_only", notifications));

      expect(fixture.tools.map((tool) => tool.name)).toEqual(
        envoyToolSpecs.map((spec) => spec.name)
      );
      const contextHandler = fixture.handlers.get("context");
      expect(
        contextHandler === undefined ? undefined : await contextHandler(userTurn, sessionContext())
      ).toBeUndefined();
      expect(notifications).toEqual([]);
      expect(hosts.length).toBeGreaterThan(0);
      expect(new Set(hosts)).toEqual(new Set(["envoy-from-file.test"]));
    });

    // The listener's bearer and the NATS seed are resolved once and handed to the client and the
    // connection: a plugin that dropped either would leave every pane unregistered or signed out.
    test("envoy.json: the listener gets the file's envoy.token, and NATS the file's servers and seed", async () => {
      delete process.env.ENVOY_URL;
      delete process.env.ENVOY_NATS_URL;
      const user = nkeys.createUser();
      writeFileSync(
        process.env.ENVOY_CONFIG_FILE ?? "",
        JSON.stringify({
          envoy: {
            enabled: true,
            url: "http://envoy-from-file.test",
            natsUrls: ["nats://nats-from-file:4222"],
            token: "file-listener-token",
            natsSeed: new TextDecoder().decode(user.getSeed()),
          },
        })
      );
      const auth: (string | null)[] = [];
      globalThis.fetch = async (input, init) => {
        auth.push(new Headers(init?.headers).get("Authorization"));
        return responseWithRegistration(input, init, []);
      };
      const { default: envoyExtension } = await import("./envoy.ts?envoy-json-credentials");
      const fixture = createPi();
      envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_credentials"));

      expect(auth.length).toBeGreaterThan(0);
      expect(new Set(auth)).toEqual(new Set(["Bearer file-listener-token"]));
      expect(natsState.connects).toEqual([
        { servers: ["nats://nats-from-file:4222"], authenticated: true },
      ]);
    });

    test("envoy.json: a pane's ENVOY_TOKEN_FILE and NATS_NKEY_SEED_FILE reach the listener and NATS", async () => {
      const user = nkeys.createUser();
      const tokenFile = join(configDirectory, "envoy-token");
      const seedFile = join(configDirectory, "nats-seed");
      writeFileSync(tokenFile, "pane-listener-token\n");
      writeFileSync(seedFile, `${new TextDecoder().decode(user.getSeed())}\n`);
      process.env.ENVOY_TOKEN_FILE = tokenFile;
      process.env.NATS_NKEY_SEED_FILE = seedFile;
      const auth: (string | null)[] = [];
      globalThis.fetch = async (input, init) => {
        auth.push(new Headers(init?.headers).get("Authorization"));
        return responseWithRegistration(input, init, []);
      };
      const { default: envoyExtension } = await import("./envoy.ts?envoy-json-pane-credentials");
      const fixture = createPi();
      envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_pane_credentials"));

      expect(auth.length).toBeGreaterThan(0);
      expect(new Set(auth)).toEqual(new Set(["Bearer pane-listener-token"]));
      expect(natsState.connects).toEqual([
        { servers: ["nats://nats-under-test:4222"], authenticated: true },
      ]);
    });

    test("envoy.json: without a profile, the file ENVOY_CONFIG_FILE names is read and stays the one handed on", async () => {
      delete process.env.ENVOY_URL;
      delete process.env.ENVOY_NATS_URL;
      const handed = join(configDirectory, "handed.json");
      writeFileSync(
        handed,
        JSON.stringify({
          dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "handed-token" },
        })
      );
      process.env.ENVOY_CONFIG_FILE = handed;
      const { default: envoyExtension } = await import("./envoy.ts?envoy-json-handed");
      const fixture = createPi();
      envoyExtension(fixture.pi);

      expect(process.env.ENVOY_CONFIG_FILE).toBe(handed);
      expect(fixture.tools).toEqual([]);
      const contextHandler = fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("the handed file's Dispatch was not read");
      expect(JSON.stringify(await contextHandler(userTurn, sessionContext()))).toContain(
        "<dispatch-first-skill>"
      );
    });

    test("envoy.json: a relative XDG_CONFIG_HOME still loads the plugin, with both sections off and the refusal said once", async () => {
      delete process.env.ENVOY_CONFIG_FILE;
      process.env.XDG_CONFIG_HOME = "relative/config";
      const { default: envoyExtension } = await import("./envoy.ts?envoy-json-relative-xdg");
      const fixture = createPi();
      envoyExtension(fixture.pi);
      const notifications: string[] = [];
      await fixture.handlers.get("session_start")?.({}, notifying("ses_relative", notifications));

      expect(process.env.ENVOY_CONFIG_FILE).toBeUndefined();
      expect(fixture.tools).toEqual([]);
      expect(fixture.handlers.has("context")).toBe(false);
      // What needs no configuration still registers.
      expect(fixture.handlers.has("resources_discover")).toBe(true);
      expect(fixture.renderers.has("envoy-message")).toBe(true);
      expect(notifications).toEqual([
        "envoy: XDG_CONFIG_HOME is relative/config, which is not an absolute path: Dispatch and Envoy are off",
      ]);
      expect(natsState.connectedNames).toEqual([]);
    });
  });

  test("envoy_sessions rejects a non-string machine filter before calling Envoy", async () => {
    const requests: string[] = [];
    globalThis.fetch = async (input) => {
      requests.push(new URL(input.toString()).pathname);
      return response([
        {
          session_id: "ses_other",
          machine_id: "other-machine",
          dir: "/tmp",
          port: 0,
          title: "",
          topics: [],
          self_subscribed: true,
        },
      ]);
    };
    const { default: envoyExtension } = await import("./envoy.ts?invalid-session-machine");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    const sessionsTool = fixture.tools.find((tool) => tool.name === "envoy_sessions");
    if (sessionsTool === undefined) throw new Error("envoy_sessions was not registered");

    const result = await sessionsTool.execute("", { machine: 42 });

    expect(result.isError).toBe(true);
    expect(result.content[0]?.text).toBe(
      "envoy_sessions was not called: 1 problem\n- machine must be a string, not 42"
    );
    expect(requests).toEqual([]);
  });

  test("injects a structured envelope payload as a TOON block that round-trips to the payload", async () => {
    const payload = {
      failed: [
        { check: "typecheck", conclusion: "failure" },
        { check: "unit", conclusion: "failure" },
      ],
      kind: "checks",
      number: 42,
    };
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?toon-structured-payload");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-toon-structured",
        source: "envoy",
        source_event_id: "source-toon-structured",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.toon-structured",
        issued_at: 1,
        payload_summary: "CI failures",
        payload: JSON.stringify(payload),
        trace_id: "trace-toon-structured",
      })
    );
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("structured delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_omp)",
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-toon-structured",
        summary: "CI failures",
        message: payload,
      },
    });
  });

  test("injects a non-JSON envelope payload as raw text without serializing its envelope", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?toon-plain-text-payload");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-toon-plain-text",
        source: "envoy",
        source_event_id: "source-toon-plain-text",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.toon-plain-text",
        issued_at: 1,
        payload_summary: "plain text",
        payload: "plain-text payload",
        trace_id: "trace-toon-plain-text",
      })
    );
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("plain-text delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_omp)",
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-toon-plain-text",
        summary: "plain text",
        message: "plain-text payload",
      },
    });
  });

  test("names the sender and injects a reply instruction for a peer agent message", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?sender-reply-hint");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-peer-message",
        source: "agent",
        source_session: "ses_peer",
        source_event_id: "source-peer-message",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.peer-message",
        issued_at: 1,
        payload_summary: "hello from a peer",
        trace_id: "trace-peer-message",
      })
    );
    await injected.promise;
    const content = fixture.messages[0];
    if (content === undefined) throw new Error("peer delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_omp)",
        from: "ses_peer",
        at: "1970-01-01T00:00:00Z",
        id: "evt-peer-message",
        reply_with: {
          tool: "envoy_send",
          args: { session_id: "ses_peer", in_reply_to: "evt-peer-message", message: "..." },
        },
        summary: "hello from a peer",
      },
    });
  });

  test("renders a human envelope without reply metadata", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?human-rendering");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-human-message",
        source: "human",
        source_event_id: "human-message",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "human.agent.message",
        issued_at: 1,
        payload_summary: "please review this",
        trace_id: "trace-human-message",
      })
    );
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("human delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_omp)",
        from: "human",
        at: "1970-01-01T00:00:00Z",
        id: "evt-human-message",
        summary: "please review this",
      },
    });
  });

  test("renders a message this session sent to itself as an ordinary delivery", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?self-echo");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-self-echo",
        source: "agent",
        source_session: "ses_omp",
        source_event_id: "source-self-echo",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.self-echo",
        issued_at: 1,
        payload_summary: "note to self",
        trace_id: "trace-self-echo",
      })
    );
    await injected.promise;
    const content = fixture.messages[0];
    if (content === undefined) throw new Error("echo delivery was not injected");
    const note = decode(content) as { envoy: Record<string, unknown> };
    expect(note.envoy).toEqual({
      to: "you (ses_omp)",
      from: "ses_omp",
      at: "1970-01-01T00:00:00Z",
      id: "evt-self-echo",
      reply_with: {
        tool: "envoy_send",
        args: { session_id: "ses_omp", in_reply_to: "evt-self-echo", message: "..." },
      },
      summary: "note to self",
    });
  });
  test("deduplicates a webhook redelivery without suppressing a later envelope", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?dispatch-echo");
    const fixture = createPi();
    const afterEcho = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("new message")) afterEcho.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    // GitHub redelivers one delivery id under a new event id: the key names the delivery.
    for (const eventID of ["evt-webhook-echo", "evt-webhook-redelivery"]) {
      agent.push(
        JSON.stringify({
          event_id: eventID,
          source: "github",
          source_event_id: "webhook-echo",
          topic: "notifications.agent.ses_omp",
          dedupe_key: "github.webhook-echo",
          issued_at: 1,
          payload_summary: "Keep the thread open?",
          payload: JSON.stringify({}),
          trace_id: `trace-${eventID}`,
        })
      );
    }
    agent.push(
      JSON.stringify({
        event_id: "evt-after-dispatch-echo",
        source: "github",
        source_event_id: "github-after-dispatch-echo",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "github.after-dispatch-echo",
        issued_at: 1,
        payload_summary: "new message",
        trace_id: "trace-after-dispatch-echo",
      })
    );
    await afterEcho.promise;

    expect(fixture.messages).toHaveLength(2);
    expect(fixture.messages[0]).toContain("Keep the thread open?");
    expect(fixture.messages[1]).toContain("new message");
  });

  test("keeps a valid event id when a frame's dedupe key is empty", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?empty-dedupe-key");
    const fixture = createPi();
    const sentinel = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("identity sentinel")) sentinel.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_identity"));
    const agent = natsState.controls.get("notifications.agent.ses_identity");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    const frame = (summary: string, identity: Record<string, string>) =>
      JSON.stringify({
        source: "github",
        topic: "notifications.agent.ses_identity",
        issued_at: 1,
        payload_summary: summary,
        ...identity,
      });
    for (const [summary, identity] of [
      [
        "identity valid",
        { event_id: "evt-valid", source_event_id: "valid", dedupe_key: "github.valid" },
      ],
      ["identity malformed", { event_id: "evt-malformed", dedupe_key: "" }],
      ["identity missing", {}],
      // The mirror case: an empty event id leaves the key, which names its event, to drop the copy.
      ["identity keyed", { event_id: "", source_event_id: "keyed", dedupe_key: "github.keyed" }],
    ] as const) {
      agent.push(frame(summary, identity));
      agent.push(frame(summary, identity));
    }
    agent.push(frame("identity sentinel", { event_id: "evt-sentinel" }));
    await sentinel.promise;
    const count = (name: string) =>
      fixture.messages.filter((message) => message.includes(`identity ${name}`)).length;
    expect({
      valid: count("valid"),
      malformed: count("malformed"),
      missing: count("missing"),
      keyed: count("keyed"),
    }).toEqual({
      valid: 1,
      malformed: 1,
      missing: 2,
      keyed: 1,
    });
  });

  test("an empty event id identifies nothing, so two distinct frames that carry one both arrive", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?empty-event-id");
    const fixture = createPi();
    const sentinel = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("sentinel frame")) sentinel.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_empty_id"));
    const agent = natsState.controls.get("notifications.agent.ses_empty_id");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    const frame = (eventId: string, summary: string) =>
      JSON.stringify({
        event_id: eventId,
        source: "github",
        topic: "notifications.agent.ses_empty_id",
        issued_at: 1,
        payload_summary: summary,
      });
    const summaries = ["distinct frame one", "distinct frame two", "sentinel frame"];
    agent.push(frame("", "distinct frame one"));
    agent.push(frame("", "distinct frame two"));
    agent.push(frame("evt-empty-id-sentinel", "sentinel frame"));
    await sentinel.promise;

    expect(
      fixture.messages.map((message) => summaries.find((summary) => message.includes(summary)))
    ).toEqual(summaries);
  });

  test("never exposes a malformed envelope frame", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?toon-malformed-envelope");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push("{this is not JSON");
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("malformed delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: { topic: "notifications.agent.ses_omp", unrecognised: "payload was not JSON" },
    });
    expect(content).not.toContain("{this is not JSON");
  });

  test("injects a role event forwarded to the claimed agent subject with original role-topic framing", async () => {
    let currentRoleTopic = "";
    const requests: string[] = [];
    const unregistrations: (readonly string[])[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      requests.push(url.pathname);
      if (url.pathname === "/v1/roles/set") {
        const role = typeof body?.role === "string" ? body.role : "";
        currentRoleTopic = `notifications.role.${role}`;
        return response({
          session_id: "ses_role_a",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [currentRoleTopic],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const topics = Array.isArray(body?.topics)
          ? body.topics.filter((topic): topic is string => typeof topic === "string")
          : [];
        unregistrations.push(topics);
        return response({});
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-agent-delivery");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_role_a"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    if (roleTool === undefined || unsubscribeTool === undefined)
      throw new Error("role tools were not registered");
    await roleTool.execute("", { role: "legion-controller" });

    expect(natsState.controls.get(currentRoleTopic)).toBeUndefined();
    const agent = natsState.controls.get("notifications.agent.ses_role_a");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(forwardedRoleEnvelope("legion-controller", "role message", "role-agent-delivery"));
    await injected.promise;
    const roleDelivery = fixture.messages[0];
    if (roleDelivery === undefined) throw new Error("role delivery was not injected");
    expect(decode(roleDelivery)).toEqual({
      envoy: {
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-role-agent-delivery",
        summary: "role message",
      },
    });
    expect(requests).not.toContain("/v1/sessions");

    const moved = await roleTool.execute("", { role: "legion-reviewer" });
    // Moving to another role releases the held one, and the answer says which.
    expect(moved.isError).toBeFalsy();
    expect(moved.details).toEqual({ role: "legion-reviewer", released: "legion-controller" });
    expect(moved.content[0]?.text).toContain("legion-controller");
    expect(unregistrations).toEqual([["notifications.role.legion-controller"]]);
    const result = await unsubscribeTool.execute("", {
      topics: ["notifications.role.legion-reviewer"],
    });
    expect(result.content[0]?.text).toBe("Unsubscribed: (none)");
    expect(unregistrations).toEqual([["notifications.role.legion-controller"]]);
  });

  test("the model's role and unsubscribe tools keep a held role's role-bound subscriptions open", async () => {
    const role = "legion-controller";
    const notice = "notifications.legion.ACME.controller";
    const ordinary = "notifications.github.acme.widgets.pr.7";
    const roleClaims: string[] = [];
    const unregistered: string[][] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      if (url.pathname === "/v1/roles/set") {
        const claimed = typeof body?.role === "string" ? body.role : "";
        roleClaims.push(claimed);
        return response({
          session_id: "ses_role_bound",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${claimed}`],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        unregistered.push(Array.isArray(body?.topics) ? body.topics : []);
        return response({});
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-set-keeps-bound-role");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    const context = sessionContext("ses_role_bound");
    await fixture.handlers.get("session_start")?.({}, context);
    await claimEnvoyRole("ses_role_bound", role, context);
    await subscribeLegionNotice("ses_role_bound", notice, context, role);
    const bound = natsState.controls.get(notice);
    if (bound === undefined) throw new Error("the role-bound subject was not subscribed");
    const tool = (name: string) => {
      const found = fixture.tools.find((candidate) => candidate.name === name);
      if (found === undefined) throw new Error(`${name} was not registered`);
      return found;
    };

    const refused = await tool("envoy_role_set").execute("", { role: "release-captain" });
    expect(refused.isError).toBe(true);
    const reason = refused.content[0]?.text ?? "";
    expect(reason).toContain(role);
    expect(reason).toContain(notice);
    // Nothing was claimed for the refused role, and the held role and its subscription stand.
    expect(roleClaims).toEqual([role]);
    expect(bound.active()).toBe(true);
    bound.push(
      JSON.stringify({
        event_id: "evt-still-bound",
        source: "legion",
        source_event_id: "source-still-bound",
        topic: notice,
        dedupe_key: "legion.notice.still-bound",
        issued_at: 1,
        payload_summary: "wake after the refused move",
        trace_id: "trace-still-bound",
      })
    );
    await injected.promise;
    expect(fixture.messages.at(-1)).toContain("wake after the refused move");

    // Claiming the role it already holds is no move, and leaves the subscription alone.
    const again = await tool("envoy_role_set").execute("", { role });
    expect(again.isError).toBeFalsy();
    expect(again.details).toEqual({ role });
    expect(roleClaims).toEqual([role, role]);
    expect(bound.active()).toBe(true);

    // The model's own subscriptions: an ordinary topic, and a wildcard over the bound subject.
    await tool("envoy_subscribe").execute("", { topics: [ordinary, `${notice}.>`] });
    const wildcard = natsState.controls.get(`${notice}.>`);
    expect(wildcard?.active()).toBe(true);

    // An explicit empty list closes nothing.
    const none = await tool("envoy_unsubscribe").execute("", { topics: [] });
    expect(none.details).toEqual({ removed: [] });
    expect(natsState.controls.get(ordinary)?.active()).toBe(true);
    expect(unregistered).toEqual([]);

    // Naming the bound subject keeps it, and says so.
    const named = await tool("envoy_unsubscribe").execute("", { topics: [notice] });
    expect(named.isError).toBeFalsy();
    expect(named.details).toEqual({ removed: [], kept: [notice] });
    expect(named.content[0]?.text).toContain(notice);
    expect(bound.active()).toBe(true);

    // Unsubscribing everything closes the model's own, its wildcard included, and keeps the
    // bound subject.
    const all = await tool("envoy_unsubscribe").execute("", {});
    expect(all.isError).toBeFalsy();
    expect(all.details).toMatchObject({ kept: [notice] });
    expect(all.details.removed).toEqual(expect.arrayContaining([ordinary, `${notice}.>`]));
    expect(wildcard?.active()).toBe(false);
    expect(bound.active()).toBe(true);

    // The plugin that owns the role still moves it through its own claim, which ends the
    // role's subscriptions as the bridge documents.
    await claimEnvoyRole("ses_role_bound", "legion-reviewer", context);
    expect(bound.active()).toBe(false);
  });

  // A fixture holding `legion-controller`. Once `gate(role)` is called, the listener holds the
  // next claim of `role` until its `release` resolves. The model's envoy_role_set reaches the move
  // queue in the call itself, so a second call made after `reached` is queued behind the first.
  const gatedRoleFixture = async (
    label: string,
    sessionID: string,
    {
      claimAtStart = true,
      transcriptRole,
      holdStart = false,
    }: {
      claimAtStart?: boolean;
      // A role an earlier process claimed under this session's id: the transcript records it and
      // the listener still names this session, while this process holds nothing.
      transcriptRole?: string;
      // Hold the start's registration, so `started` resolves only once `releaseStart` is called.
      holdStart?: boolean;
    } = {}
  ) => {
    const claims: string[] = [];
    // Each claim as `<role>@<session it was sent for>`.
    const claimSessions: string[] = [];
    const handedBack: string[] = [];
    let gated: { role: string; reached: () => void; release: Promise<void> } | undefined;
    let registered: { session: string; seen: () => void } | undefined;
    // Every registration, as the session id it registered.
    const registrations: string[] = [];
    // While `refusing` is positive, each registration is refused, as by a listener outage (the
    // client retries a refusal once), and `over` resolves once the last one is refused.
    let outage: { refusing: number; over: () => void } | undefined;
    const refuseRegistrations = (count: number) => {
      const over = Promise.withResolvers<void>();
      outage = { refusing: count, over: over.resolve };
      return over.promise;
    };
    // A listener read of `session`'s interest rows held until `release`, then answered `topics`.
    let interestGate:
      | { session: string; topics: readonly string[]; reached: () => void; release: Promise<void> }
      | undefined;
    // The listener's interest rows per session, as every read of them answers.
    const rows = new Map<string, readonly string[]>();
    // A listener read of `role`'s holder, held until `release`.
    let roleReadGate: { role: string; reached: () => void; release: Promise<void> } | undefined;
    // A registration of `session`, held until `release`.
    let registrationHold:
      | { session: string; reached: () => void; release: Promise<void> }
      | undefined;
    // While positive, each claim fails with a 500 (the client retries a failure once).
    let failingClaims = 0;
    // While positive, each hand-back fails with a 500 before the listener acts on it.
    let failingHandBacks = 0;
    // Runs as the listener takes a hand-back (an unsubscribe), before it answers.
    let handBackHook: (() => void) | undefined;
    // The listener's holder per role, and its rule for a soft claim: it takes the role only when
    // the role is unheld, already held by the claimant, or held by the session it names as its
    // predecessor (every holder here is live).
    const holders = new Map<string, string>();
    // The listener's `last_seen` per session, which every registration of that session moves, by
    // this process or (set by a test) another one.
    const lastSeen = new Map<string, number>();
    // While positive, each read of a role's holder fails with a 500 (the client retries once).
    let failingRoleReads = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      if (url.pathname === "/v1/roles/set") {
        const claimed = typeof body?.role === "string" ? body.role : "";
        const claimant = body?.session_id;
        claims.push(claimed);
        claimSessions.push(`${claimed}@${claimant}`);
        if (failingClaims > 0) {
          failingClaims -= 1;
          return new Response(JSON.stringify({ error: "listener failed" }), { status: 500 });
        }
        if (gated?.role === claimed) {
          const { reached, release } = gated;
          gated = undefined;
          reached();
          await release;
        }
        const holder = holders.get(claimed);
        if (
          body?.soft === true &&
          holder !== undefined &&
          holder !== claimant &&
          holder !== body?.previous_session_id
        ) {
          return new Response(JSON.stringify({ error: "role held", role: claimed, holder }), {
            status: 409,
          });
        }
        // The listener moves the role's interest row off the holder a claim replaces.
        const replaced = rows.get(holder ?? "");
        if (holder !== undefined && holder !== claimant && replaced !== undefined) {
          rows.set(
            holder,
            replaced.filter((topic) => topic !== `notifications.role.${claimed}`)
          );
        }
        holders.set(claimed, claimant);
        return response({
          session_id: claimant,
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${claimed}`],
        });
      }
      const roleRead = /^\/v1\/roles\/([^/]+)$/.exec(url.pathname);
      if (roleRead !== null && (init?.method ?? "GET") === "GET") {
        const role = decodeURIComponent(roleRead[1] ?? "");
        if (roleReadGate?.role === role) {
          const { reached, release } = roleReadGate;
          roleReadGate = undefined;
          reached();
          await release;
        }
        if (failingRoleReads > 0) {
          failingRoleReads -= 1;
          return new Response(JSON.stringify({ error: "listener failed" }), { status: 500 });
        }
        const holder = holders.get(role);
        if (holder === undefined) {
          return new Response(JSON.stringify({ error: "no live holder" }), { status: 404 });
        }
        return response({ role, holder, last_seen: lastSeen.get(holder) ?? 1 });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        if (failingHandBacks > 0) {
          failingHandBacks -= 1;
          return new Response(JSON.stringify({ error: "listener failed" }), { status: 500 });
        }
        for (const topic of body?.topics ?? []) {
          handedBack.push(`${topic}@${body?.session_id}`);
          const role = String(topic).replace("notifications.role.", "");
          if (holders.get(role) === body?.session_id) holders.delete(role);
          const listed = rows.get(body?.session_id ?? "");
          if (listed !== undefined) {
            rows.set(
              body?.session_id,
              listed.filter((each) => each !== topic)
            );
          }
        }
        handBackHook?.();
        return response({});
      }
      const interestRead = /^\/v1\/interests\/([^/]+)$/.exec(url.pathname);
      if (interestRead !== null && interestGate?.session === interestRead[1]) {
        const { session, topics, reached, release } = interestGate;
        interestGate = undefined;
        reached();
        await release;
        return response({ session_id: session, machine_id: "test", dir: "/tmp", topics });
      }
      const listed = rows.get(interestRead?.[1] ?? "");
      if (listed !== undefined) {
        const session = interestRead?.[1];
        return response({ session_id: session, machine_id: "test", dir: "/tmp", topics: listed });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        registrations.push(body?.session_id);
        if (outage !== undefined && outage.refusing > 0) {
          outage.refusing -= 1;
          if (outage.refusing === 0) outage.over();
          return new Response(JSON.stringify({ error: "listener unavailable" }), { status: 503 });
        }
        // The listener's registry entry for the session is written now, which moves its
        // `last_seen` whoever registers it.
        lastSeen.set(body?.session_id, (lastSeen.get(body?.session_id) ?? 1) + 1);
        if (registered?.session === body?.session_id) {
          const { seen } = registered;
          registered = undefined;
          seen();
        }
        if (registrationHold?.session === body?.session_id) {
          const { reached, release } = registrationHold;
          registrationHold = undefined;
          reached();
          await release;
        }
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import(`./envoy.ts?${label}`);
    const fixture = createPi();
    envoyExtension(fixture.pi);
    // The host's one session manager. Every context it hands out reads the manager as it stands:
    // a session event moves its id first, and a `/new` its transcript too, so a reclaim reads the
    // transcript as of its read. The first context to establish keeps the heartbeat the extension
    // registers on it, which `tick` runs once.
    const manager: { id: string; branch: () => readonly unknown[] } = {
      id: sessionID,
      branch: () => [...fixture.entries],
    };
    // Each message the extension sent, with the host's session it landed in.
    const landed: { session: string; content: string }[] = [];
    const send = fixture.pi.sendMessage;
    fixture.pi.sendMessage = (message, options) => {
      landed.push({ session: manager.id, content: String(message.content) });
      return send(message, options);
    };
    let heartbeat: (() => void) | undefined;
    const notifications: string[] = [];
    const host = (): SessionContext => {
      const base = sessionContext(sessionID);
      return {
        ...base,
        sessionManager: {
          ...base.sessionManager,
          getSessionId: () => manager.id,
          getBranch: () => manager.branch(),
        },
        setInterval: (callback: () => void) => {
          heartbeat = callback;
        },
        ui: { ...base.ui, notify: (message: string) => notifications.push(message) },
      };
    };
    // The context a session event for `id` carries: the host has moved its manager to `id`, and a
    // `/new` (`fresh`) to a transcript of its own, which holds what is appended from then on.
    const live = (id: string, { fresh = false }: { fresh?: boolean } = {}): SessionContext => {
      manager.id = id;
      if (fresh) {
        const from = fixture.entries.length;
        manager.branch = () => fixture.entries.slice(from);
      }
      return host();
    };
    const context = host();
    const tick = () => {
      if (heartbeat === undefined) throw new Error("no heartbeat was registered");
      heartbeat();
    };
    if (transcriptRole !== undefined) {
      fixture.pi.appendEntry("envoy-role-claim", { role: transcriptRole });
      holders.set(transcriptRole, sessionID);
    }
    const startReached = Promise.withResolvers<void>();
    const startRelease = Promise.withResolvers<void>();
    if (holdStart) {
      registrationHold = {
        session: sessionID,
        reached: startReached.resolve,
        release: startRelease.promise,
      };
    }
    const started = fixture.handlers.get("session_start")?.({}, context);
    if (!holdStart) await started;
    if (claimAtStart) await claimEnvoyRole(sessionID, "legion-controller", context);
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    const gate = (role: string) => {
      const reached = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      gated = { role, reached: reached.resolve, release: release.promise };
      return { reached: reached.promise, release: release.resolve };
    };
    // Resolves once `session` next registers with the listener, which a re-establishment does
    // just before it reclaims.
    const nextRegistration = (session: string) => {
      const seen = Promise.withResolvers<void>();
      registered = { session, seen: seen.resolve };
      return seen.promise;
    };
    const gateInterest = (session: string, topics: readonly string[]) => {
      const reached = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      interestGate = { session, topics, reached: reached.resolve, release: release.promise };
      return { reached: reached.promise, release: release.resolve };
    };
    const gateRoleRead = (role: string) => {
      const reached = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      roleReadGate = { role, reached: reached.resolve, release: release.promise };
      return { reached: reached.promise, release: release.resolve };
    };
    const holdRegistration = (session: string) => {
      const reached = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      registrationHold = { session, reached: reached.resolve, release: release.promise };
      return { reached: reached.promise, release: release.resolve };
    };
    const failClaims = (count: number) => {
      failingClaims = count;
    };
    const failHandBacks = (count: number) => {
      failingHandBacks = count;
    };
    const failRoleReads = (count: number) => {
      failingRoleReads = count;
    };
    return {
      fixture,
      live,
      context,
      roleTool,
      claims,
      claimSessions,
      handedBack,
      holders,
      lastSeen,
      manager,
      notifications,
      landed,
      refuseRegistrations,
      registrations,
      tick,
      gate,
      gateInterest,
      gateRoleRead,
      holdRegistration,
      failClaims,
      failHandBacks,
      failRoleReads,
      onHandBack: (hook: () => void) => {
        handBackHook = hook;
      },
      nextRegistration,
      rows,
      started,
      startReached: startReached.promise,
      releaseStart: startRelease.resolve,
    };
  };

  test("a reclaim that waited behind the model's move reclaims the role that move took", async () => {
    const { fixture, live, roleTool, claims, gate, nextRegistration } = await gatedRoleFixture(
      "reclaim-after-tool-move",
      "ses_reclaim_tool"
    );
    const held = gate("release-captain");
    const moved = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    // A tree navigation re-establishes the same session and reclaims its transcript's role; the
    // transcript is read while the model's move is still at the listener.
    const registering = nextRegistration("ses_reclaim_tool");
    const rebound = fixture.handlers.get("session_tree")?.(
      { newLeafId: "leaf" },
      live("ses_reclaim_tool")
    );
    await registering;
    held.release();
    expect((await moved).details).toEqual({
      role: "release-captain",
      released: "legion-controller",
    });
    await rebound;
    // The reclaim re-asserts the role the model just took instead of moving it back.
    expect(claims.at(-1)).toBe("release-captain");
    const next = await roleTool.execute("", { role: "reviewer" });
    expect(next.details).toEqual({ role: "reviewer", released: "release-captain" });
  });

  // The roles the transcript's claim entries record, in order.
  const claimEntryRoles = (entries: readonly unknown[]): string[] =>
    entries.flatMap((entry) => {
      if (typeof entry !== "object" || entry === null) return [];
      if (!("customType" in entry) || entry.customType !== "envoy-role-claim") return [];
      if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return [];
      return "role" in entry.data && typeof entry.data.role === "string" ? [entry.data.role] : [];
    });

  test("a plugin's claim that waited across a /new is refused, and the new session claims nothing", async () => {
    const { fixture, live, context, roleTool, claimSessions, gate, nextRegistration } =
      await gatedRoleFixture("claim-across-new", "ses_claim_old");
    const held = gate("release-captain");
    const moved = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    const claimed = claimEnvoyRole("ses_claim_old", "reviewer", context);
    // A `/new` installs a transcript of its own, which records no role.
    const registering = nextRegistration("ses_claim_new");
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_claim_new", { fresh: true })
    );
    await registering;
    held.release();
    await moved;
    await switched;
    await expect(claimed).rejects.toThrow("ses_claim_old");
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_claim_new"))).toEqual([]);
    expect(claimEntryRoles(fixture.entries)).not.toContain("reviewer");
  });

  test("a reclaim still waiting when a /new opens another session claims nothing for it", async () => {
    const { fixture, live, roleTool, claimSessions, gate, nextRegistration } =
      await gatedRoleFixture("reclaim-across-new", "ses_rq_old");
    const held = gate("release-captain");
    const moved = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    // A tree navigation's reclaim for the old session waits behind the held move.
    const registering = nextRegistration("ses_rq_old");
    const rebound = fixture.handlers.get("session_tree")?.(
      { newLeafId: "leaf" },
      live("ses_rq_old")
    );
    await registering;
    // A `/new` sets the new id at once; its own establishment waits behind the tree's.
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_rq_new", { fresh: true })
    );
    held.release();
    await moved;
    await rebound;
    await switched;
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_rq_new"))).toEqual([]);
  });

  test("a re-claim of a held role at the listener across a /fork keeps the old hold for the fork to carry", async () => {
    const { fixture, live, context, handedBack, claimSessions, gate, nextRegistration } =
      await gatedRoleFixture("reclaim-held-across-fork", "ses_rh_parent");
    const held = gate("legion-controller");
    const claimed = claimEnvoyRole("ses_rh_parent", "legion-controller", context);
    await held.reached;
    const registering = nextRegistration("ses_rh_child");
    const forked = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_rh_child")
    );
    await registering;
    held.release();
    await expect(claimed).rejects.toThrow("the session moved from ses_rh_parent");
    await forked;
    expect(handedBack).not.toContain("notifications.role.legion-controller@ses_rh_parent");
    // The fork carries the role over to the child.
    expect(claimSessions.at(-1)).toBe("legion-controller@ses_rh_child");
  });

  test("a fork that overtakes another fork's waiting reclaim still carries the role", async () => {
    const { fixture, live, context, roleTool, claimSessions, gate, nextRegistration } =
      await gatedRoleFixture("fork-over-fork", "ses_ff");
    // A re-claim of the held role holds the role-move queue, so the first fork's reclaim waits.
    const held = gate("legion-controller");
    const claimed = claimEnvoyRole("ses_ff", "legion-controller", context).catch(() => undefined);
    await held.reached;
    const registering = nextRegistration("ses_ff_fork");
    const forked = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_ff_fork")
    );
    await registering;
    // A second fork sets its id while the first fork's reclaim still waits.
    const forkedAgain = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_ff_fork2")
    );
    held.release();
    await claimed;
    await forked;
    await forkedAgain;
    // The hold is still under the parent's id, so the second fork names it as its predecessor.
    expect(claimSessions.at(-1)).toBe("legion-controller@ses_ff_fork2");
    const next = await roleTool.execute("", { role: "reviewer" });
    expect(next.details).toEqual({ role: "reviewer", released: "legion-controller" });
  });

  test("a heartbeat's role check in a fork-over-fork gap leaves the role to the second fork's reclaim", async () => {
    const {
      fixture,
      live,
      context,
      roleTool,
      handedBack,
      registrations,
      tick,
      gate,
      nextRegistration,
    } = await gatedRoleFixture("heartbeat-fork-gap", "ses_hb");
    const held = gate("legion-controller");
    const claimed = claimEnvoyRole("ses_hb", "legion-controller", context).catch(() => undefined);
    await held.reached;
    const registering = nextRegistration("ses_hb_fork");
    const forked = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_hb_fork")
    );
    await registering;
    const forkedAgain = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_hb_fork2")
    );
    // A heartbeat tick lands in the gap, while the plugin's claim still holds the role moves.
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    await drain();
    tick();
    await drain();
    held.release();
    await claimed;
    await forked;
    await forkedAgain;
    for (let step = 0; step < 10; step += 1) await drain();
    // The tool's move runs after all of it, so its answer is the end state: the role is still
    // held, under the second fork, whose hold the move hands back.
    const next = await roleTool.execute("", { role: "reviewer" });
    expect(next.details).toEqual({ role: "reviewer", released: "legion-controller" });
    expect(handedBack).toContain("notifications.role.legion-controller@ses_hb_fork2");
    expect(registrations).toContain("ses_hb_fork2");
  });

  test("an establishment's reclaim belongs to the session it was queued for, though the host's one session manager moves on", async () => {
    const { fixture, live, context, claimSessions, gate, rows } = await gatedRoleFixture(
      "reclaim-queued-id",
      "ses_q"
    );
    // The listener's rows for the session the `/new` leaves still list its role, so a reclaim
    // that took the `/new` session for its own would carry the role into it from there.
    rows.set("ses_q", ["notifications.role.legion-controller"]);
    // The host moves its manager only once a handler has run as far as it can: it awaits each
    // handler (up to its 30 s budget). No real I/O runs on these paths, so draining the pending
    // callbacks is that point.
    const settled = () => new Promise<void>((resolve) => setImmediate(resolve));
    const held = gate("legion-controller");
    const claimed = claimEnvoyRole("ses_q", "legion-controller", context).catch(() => undefined);
    await held.reached;
    // A tree navigation's reclaim waits behind the held claim; a fork queues behind that
    // establishment; then a `/new` moves the manager to a fresh, empty transcript.
    const rebound = fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_q"));
    await settled();
    const forked = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_q_fork"));
    await settled();
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_q_new", { fresh: true })
    );
    await settled();
    held.release();
    await claimed;
    await rebound;
    await forked;
    await switched;
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_q_new"))).toEqual([]);
  });

  test("a plugin's claim at the listener when the session switches is handed back, not kept for the new session", async () => {
    const {
      fixture,
      live,
      context,
      claimSessions,
      handedBack,
      gate,
      nextRegistration,
      notifications,
    } = await gatedRoleFixture("claim-in-flight-across-new", "ses_flight_old");
    const held = gate("reviewer");
    const claimed = claimEnvoyRole("ses_flight_old", "reviewer", context);
    await held.reached;
    const registering = nextRegistration("ses_flight_new");
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_flight_new", { fresh: true })
    );
    await registering;
    held.release();
    await expect(claimed).rejects.toThrow("the session moved from ses_flight_old");
    await switched;
    expect(claimSessions.at(-1)).toBe("reviewer@ses_flight_old");
    expect(handedBack).toContain("notifications.role.reviewer@ses_flight_old");
    expect(claimEntryRoles(fixture.entries)).not.toContain("reviewer");
    // The `/new` was heard, so the plugin that claimed knows; nothing warns.
    expect(notifications.filter((message) => message.includes("ended:"))).toEqual([]);
  });

  test("a resume whose listener read a /new outlasts claims nothing, for either session", async () => {
    const { fixture, live, claimSessions, gateInterest } = await gatedRoleFixture(
      "reclaim-read-across-new",
      "ses_ri",
      { claimAtStart: false }
    );
    // The resumed session records no role, so its pass asks the listener which role its rows
    // hold; that read is held while a `/new` lands.
    const read = gateInterest("ses_ri_r", ["notifications.role.release-captain"]);
    const resumed = fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      live("ses_ri_r", { fresh: true })
    );
    await read.reached;
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_ri_new", { fresh: true })
    );
    await new Promise<void>((resolve) => setImmediate(resolve));
    read.release();
    await resumed;
    await switched;
    expect(claimSessions.filter((claim) => claim.startsWith("release-captain@"))).toEqual([]);
    expect(claimEntryRoles(fixture.entries)).not.toContain("release-captain");
  });

  test("a heartbeat's role check waits for the model's move in flight and checks the role it took", async () => {
    const { roleTool, claims, holders, tick, gate, nextRegistration } = await gatedRoleFixture(
      "heartbeat-during-move",
      "ses_hm"
    );
    const regained: string[] = [];
    onEnvoyRoleRegained(async (role: string, reason: string) => {
      regained.push(`${role}:${reason}`);
    });
    // The listener lost the session's claim, so a check of the role it is leaving would claim it
    // again.
    holders.delete("legion-controller");
    const before = claims.length;
    const held = gate("release-captain");
    const moved = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    const tickRegistered = nextRegistration("ses_hm");
    tick();
    await tickRegistered;
    await new Promise<void>((resolve) => setImmediate(resolve));
    held.release();
    expect((await moved).details).toEqual({
      role: "release-captain",
      released: "legion-controller",
    });
    // A re-claim of the held role queues behind the check, so the check has run when it returns.
    await roleTool.execute("", { role: "release-captain" });
    expect(claims.slice(before)).toEqual(["release-captain", "release-captain"]);
    expect(regained).toEqual([]);
  });

  test("a regain an outage owes is made once, by the first pass that decides after it", async () => {
    const { fixture, live, roleTool, refuseRegistrations, tick, gate } = await gatedRoleFixture(
      "heartbeat-owed-regain",
      "ses_or"
    );
    const regained: string[] = [];
    onEnvoyRoleRegained(async (role: string, reason: string) => {
      regained.push(`${role}:${reason}`);
    });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // A tick whose registration the listener refuses, retry included: the next pass that decides
    // owes a regain.
    const refused = refuseRegistrations(2);
    tick();
    await refused;
    await drain();
    // A re-claim of the held role holds the role moves; a tick's pass and a tree navigation's
    // pass queue behind it.
    const held = gate("legion-controller");
    const reclaimed = roleTool.execute("", { role: "legion-controller" });
    await held.reached;
    tick();
    await drain();
    const rebound = fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_or"));
    await drain();
    held.release();
    await reclaimed;
    await rebound;
    for (let step = 0; step < 10; step += 1) await drain();
    // Later ticks owe nothing more.
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    await roleTool.execute("", { role: "legion-controller" });
    await drain();
    expect(regained).toEqual(["legion-controller:reregistered"]);
  });

  test("a heartbeat's role check whose read a /new outlasts fires no regain for the role the switch ended", async () => {
    const { fixture, live, claimSessions, refuseRegistrations, tick, gateRoleRead } =
      await gatedRoleFixture("heartbeat-read-across-new", "ses_rr");
    const regained: string[] = [];
    onEnvoyRoleRegained(async (role: string, reason: string) => {
      regained.push(`${role}:${reason}`);
    });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // An outage owes a regain to the next check that runs.
    const refused = refuseRegistrations(2);
    tick();
    await refused;
    await drain();
    // That check's read of the role, which still names this session, is held while a `/new`
    // lands.
    const read = gateRoleRead("legion-controller");
    tick();
    await read.reached;
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_rr_new", { fresh: true })
    );
    await drain();
    read.release();
    await switched;
    await drain();
    expect(regained).toEqual([]);
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_rr_new"))).toEqual([]);
  });

  test("a /new ends the outgoing role as it lands, while an earlier establishment still waits on a move", async () => {
    const notice = "notifications.legion.ACME.controller";
    const { fixture, live, context, claimSessions, gate, nextRegistration, notifications } =
      await gatedRoleFixture("new-ends-role-at-once", "ses_ne");
    await subscribeLegionNotice("ses_ne", notice, context, "legion-controller");
    const bound = natsState.controls.get(notice);
    if (bound === undefined) throw new Error("the role-bound subject was not subscribed");
    // A plugin's re-claim of the held role holds the role-move queue, and a tree navigation's
    // establishment waits behind it.
    const held = gate("legion-controller");
    const claimed = claimEnvoyRole("ses_ne", "legion-controller", context).catch(() => undefined);
    await held.reached;
    const registering = nextRegistration("ses_ne");
    const rebound = fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_ne"));
    await registering;
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_ne_new", { fresh: true })
    );
    await new Promise<void>((resolve) => setImmediate(resolve));
    // Nothing queued has run, and the new session already takes none of the old role's notices.
    expect(bound.active()).toBe(false);
    held.release();
    await claimed;
    await rebound;
    await switched;
    expect(bound.active()).toBe(false);
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_ne_new"))).toEqual([]);
    // The switch's event ended the role, so pi-legion's handler for it can claim again: no warning.
    expect(notifications.filter((message) => message.includes("sent no event"))).toEqual([]);
  });

  test("a heartbeat's role check after a fork whose reclaim failed claims for the fork over the parent's hold", async () => {
    const notice = "notifications.legion.ACME.controller";
    const { fixture, live, context, holders, failClaims, tick, nextRegistration } =
      await gatedRoleFixture("heartbeat-after-failed-fork", "ses_ft");
    await subscribeLegionNotice("ses_ft", notice, context, "legion-controller");
    const bound = natsState.controls.get(notice);
    if (bound === undefined) throw new Error("the role-bound subject was not subscribed");
    // The fork's reclaim fails, retry included, so the hold stays under the parent's id, which
    // the listener still names as the live holder.
    failClaims(2);
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_ft_fork"));
    expect(holders.get("legion-controller")).toBe("ses_ft");
    const tickRegistered = nextRegistration("ses_ft_fork");
    tick();
    await tickRegistered;
    // No real I/O runs on the check's path, so draining the pending callbacks lets it finish.
    await new Promise<void>((resolve) => setImmediate(resolve));
    // The check claimed for the fork over the parent's hold, rather than drop the role on a 409.
    expect(holders.get("legion-controller")).toBe("ses_ft_fork");
    expect(bound.active()).toBe(true);
    // The hold is under the fork now, so a later fork carries it from there.
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_ft_fork2"));
    expect(holders.get("legion-controller")).toBe("ses_ft_fork2");
    expect(bound.active()).toBe(true);
  });

  test("a fork that overtakes a resumed start's reclaim carries the role the start would have reclaimed", async () => {
    const { fixture, live, claimSessions, holders, started, startReached, releaseStart } =
      await gatedRoleFixture("fork-over-resumed-start", "ses_rs", {
        claimAtStart: false,
        transcriptRole: "legion-controller",
        holdStart: true,
      });
    // `omp --resume` starts on a transcript recording the role, which the listener still names
    // this session for; a fork lands while the start is still registering.
    await startReached;
    const forked = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_rs_fork")
    );
    await new Promise<void>((resolve) => setImmediate(resolve));
    releaseStart();
    await started;
    await forked;
    // The start's reclaim was overtaken and continued no one, so the fork names the start itself.
    expect(claimSessions).toEqual(["legion-controller@ses_rs_fork"]);
    expect(holders.get("legion-controller")).toBe("ses_rs_fork");
  });

  // A session can be overtaken at any point of its establishment: before it begins, at its
  // reclaim's turn, during the reclaim's listener reads, or once its claim returns. Each hands the
  // session that overtook it what it continued, so a fork keeps the role.
  test("an establishment that begins in a later fork's window hands the fork its role, told once", async () => {
    const { fixture, live, context, claimSessions, holders, started, startReached, releaseStart } =
      await gatedRoleFixture("establishment-in-fork-window", "ses_ew", {
        claimAtStart: false,
        transcriptRole: "legion-controller",
        holdStart: true,
      });
    // A resumed start registers, held; a tree navigation queues an establishment behind it; then
    // a fork is announced and the host moves to it, and only then does the start finish.
    await startReached;
    const navigated = fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, context);
    await new Promise<void>((resolve) => setImmediate(resolve));
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    const moved = live("ses_ew_fork");
    releaseStart();
    await started;
    await navigated;
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, moved);
    expect(claimSessions).toEqual(["legion-controller@ses_ew_fork"]);
    expect(holders.get("legion-controller")).toBe("ses_ew_fork");
    // The fork's event still finds the id it changed from, and tells the agent once.
    const idNotices = fixture.deliveries.filter((delivery) =>
      String(delivery.content).includes("session id changed")
    );
    expect(idNotices).toHaveLength(1);
  });

  test("a fork's establishment that begins in a second fork's window passes the role on and closes the subject it left", async () => {
    const { fixture, live, context, claimSessions, holders, started, startReached, releaseStart } =
      await gatedRoleFixture("establishment-in-second-fork-window", "ses_ef", {
        claimAtStart: false,
        transcriptRole: "legion-controller",
        holdStart: true,
      });
    await startReached;
    const parent = natsState.controls.get("notifications.agent.ses_ef");
    if (parent === undefined) throw new Error("the parent's agent subject was not subscribed");
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    const first = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_ef_f1"));
    await new Promise<void>((resolve) => setImmediate(resolve));
    // The second fork moves the host while the first fork's establishment still waits.
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    const second = live("ses_ef_f2");
    releaseStart();
    await started;
    await first;
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, second);
    expect(claimSessions).toEqual(["legion-controller@ses_ef_f2"]);
    expect(holders.get("legion-controller")).toBe("ses_ef_f2");
    expect(parent.active()).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_ef_f1")).toBeUndefined();
  });

  for (const phase of ["listener read", "claim"] as const) {
    test(`a fork whose reclaim's ${phase} a second fork outlasts hands that fork the role it found`, async () => {
      const slug = phase.replace(" ", "-");
      const { fixture, live, claimSessions, holders, rows, gate, gateInterest } =
        await gatedRoleFixture(`reclaim-${slug}-overtaken`, `ses_${slug}`, {
          claimAtStart: false,
        });
      // Only the listener's rows for the parent name the role: no transcript entry, no hold.
      rows.set(`ses_${slug}`, ["notifications.role.legion-controller"]);
      holders.set("legion-controller", `ses_${slug}`);
      const held =
        phase === "claim"
          ? gate("legion-controller")
          : gateInterest(`ses_${slug}`, ["notifications.role.legion-controller"]);
      const forked = fixture.handlers.get("session_switch")?.(
        { reason: "fork" },
        live(`ses_${slug}_f1`)
      );
      await held.reached;
      const forkedAgain = fixture.handlers.get("session_switch")?.(
        { reason: "fork" },
        live(`ses_${slug}_f2`)
      );
      await new Promise<void>((resolve) => setImmediate(resolve));
      held.release();
      await forked;
      await forkedAgain;
      // A claim at the listener when the second fork landed is handed back; either way the second
      // fork claims the role the first one found.
      expect(claimSessions.at(-1)).toBe(`legion-controller@ses_${slug}_f2`);
      expect(holders.get("legion-controller")).toBe(`ses_${slug}_f2`);
    });
  }

  // Two establishments can be queued from one previous session: a drift heal's and a switch's, or
  // a plugin's subscribe and the switch's own handler. Each continues what that session did.
  for (const { name, phase, between } of [
    { name: "an unannounced move during its listener read", phase: "read", between: "drift" },
    { name: "an unannounced move during its claim", phase: "claim", between: "drift" },
    { name: "a fork whose plugin subscribes before its event", phase: "read", between: "plugin" },
  ] as const) {
    test(`a fork's reclaim overtaken by ${name} still hands a third fork the role it found`, async () => {
      const id = `ses_${between}_${phase}`;
      const role = "notifications.role.legion-controller";
      const { fixture, live, context, claimSessions, holders, rows, gate, gateInterest, tick } =
        await gatedRoleFixture(`chain-${between}-${phase}`, id, { claimAtStart: false });
      rows.set(id, [role]);
      holders.set("legion-controller", id);
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      const held = phase === "claim" ? gate("legion-controller") : gateInterest(id, [role]);
      const forked = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live(`${id}_f1`));
      await held.reached;
      let second: Promise<unknown> | undefined;
      if (between === "drift") {
        // The host moves off the first fork with no announcement; a tick queues a drift heal from
        // it.
        live(`${id}_f2`);
        tick();
        await drain();
      } else {
        // A second fork, whose plugin subscribes a notice before pi-envoy's handler runs: both
        // queue an establishment from the first fork.
        await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
        const toSecond = live(`${id}_f2`);
        const subscribed = subscribeLegionNotice(
          `${id}_f2`,
          "notifications.legion.ACME.other",
          toSecond
        ).catch(() => undefined);
        second = Promise.all([
          subscribed,
          fixture.handlers.get("session_switch")?.({ reason: "fork" }, toSecond),
        ]);
        await drain();
      }
      // A third fork lands before any of them runs.
      await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
      const third = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live(`${id}_f3`));
      await drain();
      held.release();
      await forked;
      await second;
      await third;
      for (let step = 0; step < 10; step += 1) await drain();
      expect(claimSessions.at(-1)).toBe(`legion-controller@${id}_f3`);
      expect(holders.get("legion-controller")).toBe(`${id}_f3`);
    });
  }

  test("a branch to before the last role move whose reclaim fails keeps no role its transcript does not record", async () => {
    const { fixture, live, manager, roleTool, claimSessions, failClaims, tick, nextRegistration } =
      await gatedRoleFixture("branch-before-move", "ses_bm");
    // The transcript records legion-controller, then the model's move to release-captain.
    const beforeMove = fixture.entries.length;
    await roleTool.execute("", { role: "release-captain" });
    // A branch to the point before the move: its transcript records legion-controller, and its
    // reclaim of that role fails at the listener, retry included.
    failClaims(2);
    const branched = live("ses_bm_branch");
    manager.branch = () => fixture.entries.slice(0, beforeMove);
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      branched
    );
    const tickRegistered = nextRegistration("ses_bm_branch");
    tick();
    await tickRegistered;
    await new Promise<void>((resolve) => setImmediate(resolve));
    // Neither the reclaim nor the heartbeat's check took release-captain for the branch.
    expect(claimSessions.filter((claim) => claim === "release-captain@ses_bm_branch")).toEqual([]);
  });

  test("a fork after a fork whose reclaim failed carries the role from the hold the parent still has", async () => {
    const { fixture, live, holders, failClaims } = await gatedRoleFixture(
      "fork-after-failed-fork",
      "ses_fa"
    );
    failClaims(2);
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_fa_fork"));
    expect(holders.get("legion-controller")).toBe("ses_fa");
    // The second fork names the id the hold is under, not the first fork it continues.
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_fa_fork2"));
    expect(holders.get("legion-controller")).toBe("ses_fa_fork2");
  });

  for (const record of ["transcript", "listener"] as const) {
    test(`a fork that overtakes another fork's reclaim carries the role the ${record} records, though this process holds none`, async () => {
      const { fixture, live, claimSessions, holders, rows, holdRegistration } =
        await gatedRoleFixture(`fork-over-fork-no-hold-${record}`, "ses_tf", {
          claimAtStart: false,
        });
      const warnings: string[] = [];
      const stopSink = logger.registerLogSink((entry) => {
        if (entry.level === "warn") warnings.push(entry.message);
      });
      try {
        // An earlier process claimed the role under this session's id, and the listener still
        // names that id, while this process holds nothing. The claim is in the transcript, or,
        // when that entry was lost, only in the listener's rows for this session.
        if (record === "transcript") {
          fixture.pi.appendEntry("envoy-role-claim", { role: "legion-controller" });
        } else {
          rows.set("ses_tf", ["notifications.role.legion-controller"]);
        }
        holders.set("legion-controller", "ses_tf");
        const held = holdRegistration("ses_tf_fork");
        const forked = fixture.handlers.get("session_switch")?.(
          { reason: "fork" },
          live("ses_tf_fork")
        );
        await held.reached;
        // A second fork sets its id while the first fork's establishment is still registering.
        const forkedAgain = fixture.handlers.get("session_switch")?.(
          { reason: "fork" },
          live("ses_tf_fork2")
        );
        await new Promise<void>((resolve) => setImmediate(resolve));
        held.release();
        await forked;
        await forkedAgain;
        // The first fork's reclaim did nothing; the second names the id the first would have,
        // and the listener lets it supersede that hold.
        expect(claimSessions).toEqual(["legion-controller@ses_tf_fork2"]);
        expect(holders.get("legion-controller")).toBe("ses_tf_fork2");
        expect(warnings).not.toContain("envoy: role reclaim failed");
      } finally {
        stopSink();
      }
    });
  }

  for (const phase of ["read", "claim"] as const) {
    test(`a heartbeat's role check whose ${phase} a /fork outlasts leaves the regain an outage owes to the fork`, async () => {
      const {
        fixture,
        live,
        roleTool,
        holders,
        refuseRegistrations,
        tick,
        gate,
        gateRoleRead,
        nextRegistration,
      } = await gatedRoleFixture(`heartbeat-${phase}-across-fork`, `ses_${phase}f`);
      const regained: string[] = [];
      onEnvoyRoleRegained(async (role: string, reason: string) => {
        regained.push(`${role}:${reason}`);
      });
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      const refused = refuseRegistrations(2);
      tick();
      await refused;
      await drain();
      // The owed check is held at its read of the role, which still names the parent, or, once
      // the listener has lost the claim, at the claim it sends; a `/fork` lands meanwhile.
      if (phase === "claim") holders.delete("legion-controller");
      const held = phase === "read" ? gateRoleRead("legion-controller") : gate("legion-controller");
      tick();
      await held.reached;
      const forked = fixture.handlers.get("session_switch")?.(
        { reason: "fork" },
        live(`ses_${phase}f_fork`)
      );
      await drain();
      held.release();
      await forked;
      for (let step = 0; step < 10; step += 1) await drain();
      // Nothing fired for the parent, and the fork's pass carried the role.
      expect(regained).toEqual([]);
      expect(holders.get("legion-controller")).toBe(`ses_${phase}f_fork`);
      // The next tick checks the fork and makes the regain it owes.
      const nextTick = nextRegistration(`ses_${phase}f_fork`);
      tick();
      await nextTick;
      await roleTool.execute("", { role: "legion-controller" });
      await drain();
      expect(regained).toEqual(["legion-controller:reregistered"]);
    });
  }

  test("a fork that overtakes a /new's reclaim reads no role from the session the /new left", async () => {
    const { fixture, live, claimSessions, rows, holdRegistration } = await gatedRoleFixture(
      "fork-over-new",
      "ses_fn"
    );
    // The listener's rows for the session the `/new` left still list its role.
    rows.set("ses_fn", ["notifications.role.legion-controller"]);
    const held = holdRegistration("ses_fn_new");
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_fn_new", { fresh: true })
    );
    await held.reached;
    const forked = fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      live("ses_fn_fork")
    );
    await new Promise<void>((resolve) => setImmediate(resolve));
    held.release();
    await switched;
    await forked;
    expect(claimSessions.filter((claim) => !claim.endsWith("@ses_fn"))).toEqual([]);
  });

  test("with inbound messages off, a /new still ends the outgoing role, so the heartbeat never carries it", async () => {
    delete process.env.ENVOY_NATS_URL;
    const { fixture, live, roleTool, claimSessions, tick, nextRegistration } =
      await gatedRoleFixture("inbound-off-new", "ses_io", { claimAtStart: false });
    expect((await roleTool.execute("", { role: "release-captain" })).isError).not.toBe(true);
    await fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_io_new", { fresh: true })
    );
    const tickRegistered = nextRegistration("ses_io_new");
    tick();
    await tickRegistered;
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(claimSessions).toEqual(["release-captain@ses_io"]);
  });

  // Oh My Pi announces a switch (`session_before_switch`), moves its one session manager, and
  // only then dispatches the switch event. `hostMoves` stops in that window: the announcement
  // has run and the manager names the new session, and the test fires the event itself later.
  const hostMoves = async (
    fixture: {
      readonly handlers: Map<string, (event: unknown, context: SessionContext) => unknown>;
    },
    live: (id: string, options?: { fresh?: boolean }) => SessionContext,
    context: SessionContext,
    id: string
  ): Promise<SessionContext> => {
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    return live(id, { fresh: true });
  };

  test("a heartbeat tick between the host's move to a /new session and its switch event carries no role into it", async () => {
    const notice = "notifications.legion.ACME.controller";
    const { fixture, live, context, claimSessions, tick } = await gatedRoleFixture(
      "tick-in-host-window",
      "ses_hw"
    );
    await subscribeLegionNotice("ses_hw", notice, context, "legion-controller");
    const bound = natsState.controls.get(notice);
    if (bound === undefined) throw new Error("the role-bound subject was not subscribed");
    const moved = await hostMoves(fixture, live, context, "ses_hw_new");
    tick();
    await new Promise<void>((resolve) => setImmediate(resolve));
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_hw_new"))).toEqual([]);
    expect(bound.active()).toBe(false);
  });

  for (const reason of ["new", "fork"] as const) {
    for (const event of ["never comes", "comes late"] as const) {
      test(`a ${reason === "new" ? "/new" : "fork"} whose event ${event} after the host's move is run by the next tick by its own rule`, async () => {
        const notice = "notifications.legion.ACME.controller";
        const { fixture, live, context, claimSessions, registrations, tick, notifications } =
          await gatedRoleFixture(
            `switch-without-event-${reason}-${event.replace(" ", "-")}`,
            "ses_se"
          );
        await subscribeLegionNotice("ses_se", notice, context, "legion-controller");
        const bound = natsState.controls.get(notice);
        if (bound === undefined) throw new Error("the role-bound subject was not subscribed");
        const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
        // The switch is announced and the manager moves; its event then never comes (the switch
        // threw) or comes only after a tick.
        await fixture.handlers.get("session_before_switch")?.({ reason }, context);
        const moved = live("ses_se_next", { fresh: reason === "new" });
        // The tick converges to the session the host is on, by the announced switch's rule.
        tick();
        for (let step = 0; step < 5; step += 1) await drain();
        if (event === "comes late") {
          await fixture.handlers.get("session_switch")?.({ reason }, moved);
        }
        expect(registrations).toContain("ses_se_next");
        expect(process.env.DISPATCH_SESSION_ID).toBe("ses_se_next");
        const idNotices = fixture.deliveries.filter(
          (delivery) =>
            String(delivery.content).includes("session id changed") &&
            String(delivery.content).includes("ses_se_next")
        );
        // A role end no session event announced is one no plugin heard, so it is said; a role
        // carried needs no word.
        const warned = notifications.filter((message) => message.includes("with no session event"));
        if (reason === "new") {
          expect(claimSessions.filter((claim) => claim.endsWith("@ses_se_next"))).toEqual([]);
          expect(bound.active()).toBe(false);
          expect(idNotices).toHaveLength(0);
          expect(warned).toHaveLength(1);
          expect(warned[0]).toContain("legion-controller");
          // The role's notices were a plugin's, which only that plugin's next claim reopens.
          expect(warned[0]).toContain("the plugin that opened its notices");
          expect(warned[0]).not.toContain("envoy_role_set");
        } else {
          expect(claimSessions.at(-1)).toBe("legion-controller@ses_se_next");
          expect(bound.active()).toBe(true);
          // Told once, whether or not the event comes after.
          expect(idNotices).toHaveLength(1);
          expect(warned).toEqual([]);
        }
      });
    }
  }

  test("a /new whose event never comes, ending a role the model claimed, points at envoy_role_set", async () => {
    const { fixture, live, context, roleTool, tick, notifications } = await gatedRoleFixture(
      "switch-without-event-model-role",
      "ses_mw",
      { claimAtStart: false }
    );
    expect((await roleTool.execute("", { role: "release-captain" })).isError).not.toBe(true);
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    live("ses_mw_next", { fresh: true });
    tick();
    await new Promise<void>((resolve) => setImmediate(resolve));
    tick();
    for (let step = 0; step < 5; step += 1) await new Promise<void>((r) => setImmediate(r));
    const warned = notifications.filter((message) => message.includes("with no session event"));
    expect(warned).toHaveLength(1);
    expect(warned[0]).toContain("release-captain");
    expect(warned[0]).toContain("envoy_role_set");
  });

  for (const { name, newMoves, forkMoves } of [
    {
      name: "a /new that moved the host and sent no event, then a fork",
      newMoves: true,
      forkMoves: true,
    },
    { name: "a /new another extension cancelled, then a fork", newMoves: false, forkMoves: true },
    {
      name: "a /new that moved the host and sent no event, then a cancelled fork",
      newMoves: true,
      forkMoves: false,
    },
  ] as const) {
    test(`${name}: a switch that sent no event takes effect before the next one`, async () => {
      const notice = "notifications.legion.ACME.controller";
      const { fixture, live, context, claimSessions, registrations, tick, notifications } =
        await gatedRoleFixture(`missed-switch-${newMoves}-${forkMoves}`, "ses_nf");
      await subscribeLegionNotice("ses_nf", notice, context, "legion-controller");
      const bound = natsState.controls.get(notice);
      if (bound === undefined) throw new Error("the role-bound subject was not subscribed");
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      // The `/new` is announced; it moves the host and throws, or another extension cancels it.
      // A tick runs a pass meanwhile.
      await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
      if (newMoves) live("ses_nf_new", { fresh: true });
      tick();
      await drain();
      // A fork is announced before the next tick, and lands or is cancelled.
      await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
      if (forkMoves) {
        await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_nf_fork"));
      }
      for (let step = 0; step < 5; step += 1) await drain();
      const warned = notifications.filter((message) => message.includes("with no session event"));
      if (newMoves) {
        // The `/new` ran first: the role ended, said so, and nothing carried it on.
        expect(claimSessions.filter((claim) => !claim.endsWith("@ses_nf"))).toEqual([]);
        expect(bound.active()).toBe(false);
        expect(warned).toHaveLength(1);
        if (!forkMoves) {
          expect(registrations).toContain("ses_nf_new");
          expect(process.env.DISPATCH_SESSION_ID).toBe("ses_nf_new");
        }
      } else {
        // A cancelled `/new` changed nothing, so the fork carries the role.
        expect(claimSessions.at(-1)).toBe("legion-controller@ses_nf_fork");
        expect(bound.active()).toBe(true);
        expect(warned).toEqual([]);
      }
    });
  }

  test("a fork whose establishment gave way to a resume the host rolled back is settled by the next tick", async () => {
    const {
      fixture,
      live,
      context,
      holders,
      claimSessions,
      tick,
      started,
      startReached,
      releaseStart,
    } = await gatedRoleFixture("fork-under-rolled-back-resume", "ses_rb", {
      claimAtStart: false,
      transcriptRole: "legion-controller",
      holdStart: true,
    });
    await startReached;
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    const forked = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_rb_f1"));
    await new Promise<void>((resolve) => setImmediate(resolve));
    // A resume moves the host while the fork's establishment waits; the fork's turn comes then.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    live("ses_rb_x");
    releaseStart();
    await started;
    await forked;
    expect(natsState.controls.get("notifications.agent.ses_rb_f1")).toBeUndefined();
    // The resume fails and the host restores the fork, with no event.
    live("ses_rb_f1");
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(natsState.controls.get("notifications.agent.ses_rb_f1")?.active()).toBe(true);
    expect(holders.get("legion-controller")).toBe("ses_rb_f1");
    // Settled once: the next tick only re-registers.
    const claimed = claimSessions.length;
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(claimSessions.length).toBe(claimed);
  });

  test("a resume rolled back while this process hands back its claim is settled once the host is back", async () => {
    const role = "legion-controller";
    const { fixture, live, context, holders, manager, tick, claimSessions, gate, onHandBack } =
      await gatedRoleFixture("rollback-during-hand-back", "ses_hb", { claimAtStart: false });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // A resume to a session whose transcript records the role; its reclaim's claim is held.
    const held = gate(role);
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const target = live("ses_hb_r", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    holders.set(role, "ses_hb_r");
    const switched = fixture.handlers.get("session_switch")?.({ reason: "resume" }, target);
    await held.reached;
    // A second resume moves the host on; it rolls back while the claim is handed back.
    const branch = manager.branch;
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    live("ses_hb_x", { fresh: true });
    onHandBack(() => {
      manager.id = "ses_hb_r";
      manager.branch = branch;
    });
    held.release();
    await switched;
    for (let step = 0; step < 5; step += 1) await drain();
    expect(claimSessions).toEqual([`${role}@ses_hb_r`]);
    // A pass for the second resume hands the claim back, during which the host rolls back; the
    // pass after converges to the session the host is on again.
    for (let ticks = 0; ticks < 2; ticks += 1) {
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
    }
    expect(claimSessions).toEqual([`${role}@ses_hb_r`, `${role}@ses_hb_r`]);
    expect(holders.get(role)).toBe("ses_hb_r");
  });

  test("an undo back to an unsettled resume keeps the role a plugin claimed at the undone switch", async () => {
    const role = "legion-controller";
    const notice = "notifications.legion.ACME.controller";
    // The session the first resume leaves holds no role; the resumed session records one.
    const { fixture, live, context, manager, tick, holders, holdRegistration } =
      await gatedRoleFixture("undo-keeps-unsettled", "ses_uk", { claimAtStart: false });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const pending: Promise<unknown>[] = [];
    const held = holdRegistration("ses_uk_s");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const toS = live("ses_uk_s", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    const ownS = manager.branch();
    pending.push(
      Promise.resolve(fixture.handlers.get("session_switch")?.({ reason: "resume" }, toS))
    );
    await held.reached;
    // A second resume lands while the first one's establishment waits, and a plugin claims the
    // role at its event.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const toX = live("ses_uk_x", { fresh: true });
    pending.push(
      Promise.resolve(fixture.handlers.get("session_switch")?.({ reason: "resume" }, toX))
    );
    held.release();
    await Promise.all(pending);
    for (let step = 0; step < 10; step += 1) await drain();
    await claimEnvoyRole("ses_uk_x", role, toX);
    await subscribeLegionNotice("ses_uk_x", notice, toX, role);
    expect(holders.get(role)).toBe("ses_uk_x");
    // Oh My Pi rolls the second resume back to the first one's session.
    const returned = fixture.entries.length;
    manager.id = "ses_uk_s";
    manager.branch = () => [...ownS, ...fixture.entries.slice(returned)];
    for (let ticks = 0; ticks < 3; ticks += 1) {
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
    }
    // The returned session's records name the role, so it keeps it, its claim superseding the
    // undone session's hold, and the plugin's notice stays open.
    expect(holders.get(role)).toBe("ses_uk_s");
    expect(natsState.controls.get(notice)?.active()).toBe(true);
  });

  test("a resume rolled back before its reclaim ran carries none of its listener rows' role back", async () => {
    const other = "release-captain";
    // The session the resume leaves records no role, so its return reads the listener's rows.
    const {
      fixture,
      live,
      context,
      manager,
      tick,
      claimSessions,
      rows,
      holders,
      holdRegistration,
    } = await gatedRoleFixture("rollback-before-reclaim", "ses_rr0", { claimAtStart: false });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const own = manager.branch();
    // The resumed session's role is only in the listener's rows from its earlier life.
    rows.set("ses_rr0_x", [`notifications.role.${other}`]);
    holders.set(other, "ses_rr0_x");
    const held = holdRegistration("ses_rr0_x");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const switched = fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      live("ses_rr0_x", { fresh: true })
    );
    await held.reached;
    // Oh My Pi throws after the event and restores the session it left, while the resume's
    // establishment still registers.
    const returned = fixture.entries.length;
    manager.id = "ses_rr0";
    manager.branch = () => [...own, ...fixture.entries.slice(returned)];
    for (let ticks = 0; ticks < 2; ticks += 1) {
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
    }
    held.release();
    await switched;
    for (let step = 0; step < 10; step += 1) await drain();
    // A resume continues nothing, so its undo carries nothing back from it.
    expect(claimSessions.filter((claim) => claim.startsWith(`${other}@`))).toEqual([]);
    expect(holders.get(other)).toBe("ses_rr0_x");
  });

  test("a switch that lands while a rolled-back resume's role is handed back is established by its own rules", async () => {
    const role = "legion-controller";
    const other = "release-captain";
    const {
      fixture,
      live,
      context,
      manager,
      tick,
      claimSessions,
      holders,
      rows,
      notifications,
      holdRegistration,
      onHandBack,
    } = await gatedRoleFixture("undo-hand-back-race", "ses_ur");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const pending: Promise<unknown>[] = [];
    // The parent's listener rows name its role, as the real listener's do.
    rows.set("ses_ur", [`notifications.role.${role}`]);
    // A fork whose registration is held, then a resume that lands and takes a role of its own.
    const forkHeld = holdRegistration("ses_ur_f");
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    pending.push(
      Promise.resolve(
        fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_ur_f"))
      )
    );
    await forkHeld.reached;
    const forkBranch = manager.branch();
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const resumed = live("ses_ur_x", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role: other });
    holders.set(other, "ses_ur_x");
    pending.push(
      Promise.resolve(fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed))
    );
    forkHeld.release();
    await Promise.all(pending);
    for (let step = 0; step < 10; step += 1) await drain();
    expect(holders.get(other)).toBe("ses_ur_x");
    // Oh My Pi rolls the resume back to the fork; while the undo hands the resume's role back, a
    // `/new` lands.
    const returned = fixture.entries.length;
    manager.id = "ses_ur_f";
    manager.branch = () => [...forkBranch, ...fixture.entries.slice(returned)];
    onHandBack(() => {
      onHandBack(() => {});
      // Announced and moved before the hand-back answers; its event follows.
      void fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
      const toNew = live("ses_ur_y", { fresh: true });
      pending.push(
        Promise.resolve(fixture.handlers.get("session_switch")?.({ reason: "new" }, toNew))
      );
    });
    for (let ticks = 0; ticks < 2; ticks += 1) {
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
    }
    await Promise.all(pending);
    for (let step = 0; step < 10; step += 1) await drain();
    // The `/new` session takes no role, and the warning names the session the host came back to.
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_ur_y"))).toEqual([]);
    const undone = notifications.filter((message) =>
      message.includes("role release-captain ended")
    );
    expect(undone).toHaveLength(1);
    expect(undone[0]).toContain("the host moved to session ses_ur_f");
    // Only the session the host ends on keeps its agent subject open.
    expect(natsState.controls.get("notifications.agent.ses_ur_x")?.active() ?? false).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_ur_f")?.active() ?? false).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_ur_y")?.active()).toBe(true);
  });

  test("a release the session's records hold is final: a pass hands the role back and claims nothing", async () => {
    const { fixture, live, claimSessions, holders, handedBack } = await gatedRoleFixture(
      "release-is-final",
      "ses_rl"
    );
    const before = claimSessions.length;
    fixture.pi.appendEntry("envoy-role-claim", { role: null });
    await fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_rl"));
    expect(claimSessions.slice(before)).toEqual([]);
    expect(holders.has("legion-controller")).toBe(false);
    expect(handedBack).toContain("notifications.role.legion-controller@ses_rl");
  });

  test("a fork does not take its recorded role from a live session outside its lineage, nor once that session leaves, until a session event lands on it", async () => {
    const { fixture, live, holders, claimSessions, tick } = await gatedRoleFixture(
      "fork-foreign-holder",
      "ses_fh",
      { claimAtStart: false }
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // The transcript records the role, but another process took it over: the listener names a
    // session this process never was.
    fixture.pi.appendEntry("envoy-role-claim", { role: "legion-controller" });
    holders.set("legion-controller", "ses_elsewhere");
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_fh_fork"));
    expect(holders.get("legion-controller")).toBe("ses_elsewhere");
    // The listener's refusal stands: the heartbeat claims nothing, even once that holder is gone.
    const refused = claimSessions.length;
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    holders.delete("legion-controller");
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(claimSessions.length).toBe(refused);
    expect(holders.has("legion-controller")).toBe(false);
    // A session event on the fork decides afresh.
    await fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_fh_fork"));
    expect(holders.get("legion-controller")).toBe("ses_fh_fork");
  });

  test("a role claimed back after a move no event announced ended its plugin notices says they stay closed", async () => {
    const notice = "notifications.legion.ACME.controller";
    const { fixture, live, context, manager, tick, notifications, holders } =
      await gatedRoleFixture("notices-stay-closed", "ses_nc");
    await subscribeLegionNotice("ses_nc", notice, context, "legion-controller");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const own = manager.branch();
    // A resume moves the host and sends no event; a tick ends the role, whose notices close.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    live("ses_nc_x", { fresh: true });
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(natsState.controls.get(notice)?.active() ?? false).toBe(false);
    // The host rolls back to the session whose records name the role.
    const returned = fixture.entries.length;
    manager.id = "ses_nc";
    manager.branch = () => [...own, ...fixture.entries.slice(returned)];
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(holders.get("legion-controller")).toBe("ses_nc");
    expect(natsState.controls.get(notice)?.active() ?? false).toBe(false);
    expect(notifications.some((message) => message.includes("stay closed"))).toBe(true);
  });

  test("a heartbeat tick during a NATS outage still re-registers and re-asserts the role over HTTP", async () => {
    const { tick, registrations, holders, notifications } = await gatedRoleFixture(
      "nats-outage-heartbeat",
      "ses_no"
    );
    // NATS drops and refuses to reconnect; meanwhile the listener lost the session's claim.
    natsState.connectionsClosed = true;
    natsState.failConnects = 99;
    holders.delete("legion-controller");
    const before = registrations.length;
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(registrations.length).toBeGreaterThan(before);
    expect(holders.get("legion-controller")).toBe("ses_no");
    expect(notifications.filter((message) => message.includes("heartbeat failed"))).toEqual([]);
  });

  test("a pass between a /new's announcement and the host's move keeps the /new from carrying the role", async () => {
    const { fixture, live, context, claimSessions, tick } = await gatedRoleFixture(
      "pass-before-move",
      "ses_pm"
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    tick();
    for (let step = 0; step < 5; step += 1) await drain();
    live("ses_pm_new", { fresh: true });
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_pm_new"))).toEqual([]);
  });

  for (const [event, label] of [
    ["reload", "a /reload's event"],
    ["run", "a run under it that ends unaborted"],
  ] as const) {
    test(`${label} ends a switch's announcement, so a later move with no event carries the session`, async () => {
      const { fixture, live, context, claimSessions, landed, tick } = await gatedRoleFixture(
        `announcement-ends-${event}`,
        "ses_ae"
      );
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      if (event === "reload") {
        // Oh My Pi's reload switches to the session file it is on: announced, then its event
        // lands on the same id.
        await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
        await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_ae"));
      } else {
        // A `/new` another extension cancels moves nothing; the session runs on.
        await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
        await fixture.handlers.get("agent_start")?.({}, context);
        await fixture.handlers.get("agent_end")?.(
          { messages: [{ role: "assistant", stopReason: "stop" }] },
          context
        );
      }
      // The host later moves off its session file with no event.
      live("ses_ae_moved");
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
      expect(claimSessions.at(-1)).toBe("legion-controller@ses_ae_moved");
      const idNotices = landed.filter((message) => message.content.includes("session id changed"));
      expect(idNotices.map((message) => message.session)).toEqual(["ses_ae_moved"]);
    });
  }

  test("a run that starts while a /new is announced, which the /new aborts, keeps the announcement, so a tick before the switch's event carries no role into it", async () => {
    const { fixture, live, context, claimSessions, holders, tick } = await gatedRoleFixture(
      "run-under-new",
      "ses_ru"
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // An Envoy delivery wakes the session while another extension's announcement handler awaits.
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    await fixture.handlers.get("agent_start")?.({}, context);
    // The switch goes ahead: it aborts that run and moves the host; a tick lands before its event.
    await fixture.handlers.get("agent_end")?.(
      { messages: [{ role: "assistant", stopReason: "aborted" }] },
      context
    );
    const moved = live("ses_ru_new", { fresh: true });
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_ru_new"))).toEqual([]);
    expect(holders.has("legion-controller")).toBe(false);
  });

  for (const meanwhile of ["nothing else", "the model claiming another role there"] as const) {
    test(`a resume rolled back after its event ended the role, with ${meanwhile}, keeps the hold, never handing it back for another session to take`, async () => {
      const role = "legion-controller";
      const notice = "notifications.legion.ACME.controller";
      const {
        fixture,
        live,
        context,
        holders,
        handedBack,
        manager,
        notifications,
        roleTool,
        tick,
        holdRegistration,
        onHandBack,
      } = await gatedRoleFixture(`rollback-keeps-ended-hold-${meanwhile.length}`, "ses_rk");
      await subscribeLegionNotice("ses_rk", notice, context, role);
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      const own = [...manager.branch()];
      const held = holdRegistration("ses_rk_a");
      await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
      const switched = fixture.handlers.get("session_switch")?.(
        { reason: "resume" },
        live("ses_rk_a", { fresh: true })
      );
      await held.reached;
      if (meanwhile !== "nothing else") {
        expect((await roleTool.execute("", { role: "release-captain" })).isError).not.toBe(true);
      }
      // The resume fails after its event and Oh My Pi restores the session it left.
      manager.id = "ses_rk";
      manager.branch = () => own;
      // A hand-back of the role at the listener opens it to another live session's claim.
      onHandBack(() => {
        if (handedBack.at(-1) === `notifications.role.${role}@ses_rk`) {
          holders.set(role, "ses_rival");
        }
      });
      held.release();
      await switched;
      for (let ticks = 0; ticks < 3; ticks += 1) {
        tick();
        for (let step = 0; step < 10; step += 1) await drain();
      }
      expect(holders.get(role)).toBe("ses_rk");
      expect(holders.has("release-captain")).toBe(false);
      // The resume's event closed the plugin's notices, and only that plugin reopens them.
      expect(notifications.some((message) => message.includes("stay closed"))).toBe(true);
    });
  }

  test("two roles session events end before any pass are both handed back", async () => {
    const { fixture, live, context, holders, roleTool, handedBack, holdRegistration } =
      await gatedRoleFixture("two-ended-holds", "ses_te");
    const held = holdRegistration("ses_te_a");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const pending: unknown[] = [
      fixture.handlers.get("session_switch")?.(
        { reason: "resume" },
        live("ses_te_a", { fresh: true })
      ),
    ];
    await held.reached;
    // The resume's event ended legion-controller; the model claims another role meanwhile.
    expect((await roleTool.execute("", { role: "release-captain" })).isError).not.toBe(true);
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    pending.push(
      fixture.handlers.get("session_switch")?.({ reason: "new" }, live("ses_te_n", { fresh: true }))
    );
    held.release();
    await Promise.all(pending);
    expect(handedBack).toContain("notifications.role.legion-controller@ses_te");
    expect(handedBack).toContain("notifications.role.release-captain@ses_te_a");
    expect(holders.size).toBe(0);
  });

  for (const records of ["no role", "a role of its own"] as const) {
    test(`the pass after a role-ending event hands the hold back though its registration fails, the resumed session recording ${records}, so no later pass touches that id`, async () => {
      const role = "legion-controller";
      const { fixture, live, context, holders, handedBack, refuseRegistrations, tick } =
        await gatedRoleFixture(`hand-back-in-outage-${records.length}`, "ses_ho");
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      const refused = refuseRegistrations(2);
      await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
      const resumed = live("ses_ho_k", { fresh: true });
      if (records !== "no role") fixture.pi.appendEntry("envoy-role-claim", { role: "reviewer" });
      await fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed);
      await refused;
      expect(handedBack).toEqual([`notifications.role.${role}@ses_ho`]);
      // Another process resumes the session this one left and claims the role under its id.
      holders.set(role, "ses_ho");
      tick();
      for (let step = 0; step < 20; step += 1) await drain();
      expect(holders.get(role)).toBe("ses_ho");
      expect(handedBack).toEqual([`notifications.role.${role}@ses_ho`]);
    });
  }

  test("a hand-back the listener fails is not tried again, so a later pass leaves a hold another process makes under that id", async () => {
    const role = "legion-controller";
    const { fixture, live, context, holders, failHandBacks, tick } = await gatedRoleFixture(
      "failed-hand-back-once",
      "ses_fo"
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    failHandBacks(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      live("ses_fo_k", { fresh: true })
    );
    // Another process resumes the session this one left and claims the role under its id.
    holders.set(role, "ses_fo");
    tick();
    for (let step = 0; step < 20; step += 1) await drain();
    expect(holders.get(role)).toBe("ses_fo");
  });

  test("a claim the model makes after the listener refused the role to the session is kept by later passes", async () => {
    const role = "legion-controller";
    const { fixture, live, holders, roleTool, tick } = await gatedRoleFixture(
      "claim-after-refusal",
      "ses_ca",
      { claimAtStart: false }
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    fixture.pi.appendEntry("envoy-role-claim", { role });
    holders.set(role, "ses_elsewhere");
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_ca_fork"));
    expect(holders.get(role)).toBe("ses_elsewhere");
    expect((await roleTool.execute("", { role })).isError).not.toBe(true);
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(holders.get(role)).toBe("ses_ca_fork");
  });

  test("a refusal in another session does not clear this session's, so a rollback with no event takes nothing back", async () => {
    const role = "legion-controller";
    const { fixture, live, context, manager, holders, claimSessions, tick } =
      await gatedRoleFixture("two-refusals", "ses_tr2");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const own = [...manager.branch()];
    // A newer live session takes the role; the heartbeat's re-assertion is refused.
    holders.set(role, "ses_newer");
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    // A resume to a session whose records name a role a third session holds is refused too.
    holders.set("reviewer", "ses_third");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const resumed = live("ses_tr2_b", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role: "reviewer" });
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed);
    expect(claimSessions.at(-1)).toBe("reviewer@ses_tr2_b");
    // The resume rolls back with no event, and the newer holder leaves.
    manager.id = "ses_tr2";
    manager.branch = () => own;
    holders.delete(role);
    const before = claimSessions.length;
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(claimSessions.slice(before)).toEqual([]);
    expect(holders.has(role)).toBe(false);
  });

  test("a role whose hand-back an outage lost is claimed by a later session once the listener reaps the hold, not refused to it", async () => {
    const role = "legion-controller";
    const {
      fixture,
      live,
      context,
      holders,
      claimSessions,
      refuseRegistrations,
      failHandBacks,
      failRoleReads,
      tick,
    } = await gatedRoleFixture("abandoned-hold", "ses_ab");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // A /new lands during a listener outage: its registration and the hand-back both fail.
    const refused = refuseRegistrations(2);
    failHandBacks(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_ab_n", { fresh: true })
    );
    await refused;
    expect(holders.get(role)).toBe("ses_ab");
    // The listener is back; a resume opens a session whose records name the role, and the
    // listener still names the session this process left. Its first read of that holder fails,
    // which decides nothing.
    failRoleReads(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const resumed = live("ses_ab_c", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed);
    expect(holders.get(role)).toBe("ses_ab");
    // The listener reaps the abandoned hold; the next tick claims the role.
    holders.delete(role);
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(holders.get(role)).toBe("ses_ab_c");
    // From then on that id is no longer this process's to wait out: another process that resumes
    // it and takes the role is the newer holder, refused to the next session once.
    holders.set(role, "ses_ab");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const later = live("ses_ab_d", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, later);
    const refusedOnce = claimSessions.length;
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(claimSessions.length).toBe(refusedOnce);
    expect(holders.get(role)).toBe("ses_ab");
  });

  test("an abandoned hold whose session another process heartbeats again is the newer holder: refused once seen, never taken back", async () => {
    const role = "legion-controller";
    const {
      fixture,
      live,
      context,
      holders,
      lastSeen,
      claimSessions,
      refuseRegistrations,
      failHandBacks,
      tick,
    } = await gatedRoleFixture("abandoned-hold-heartbeated", "ses_ah");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const ticks = async (count: number) => {
      for (let each = 0; each < count; each += 1) {
        tick();
        for (let step = 0; step < 10; step += 1) await drain();
      }
    };
    // A /new lands during a listener outage, so the hand-back of ses_ah is abandoned.
    const refused = refuseRegistrations(2);
    failHandBacks(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_ah_n", { fresh: true })
    );
    await refused;
    // A resume opens a session whose records name the role; its claims wait on the listener.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const resumed = live("ses_ah_c", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed);
    await ticks(1);
    const waiting = claimSessions.filter((claim) => claim === `${role}@ses_ah_c`).length;
    expect(waiting).toBe(2);
    // Another process resumes ses_ah and heartbeats it: the next refusal sees it, and stands.
    lastSeen.set("ses_ah", (lastSeen.get("ses_ah") ?? 1) + 1);
    await ticks(3);
    expect(claimSessions.filter((claim) => claim === `${role}@ses_ah_c`).length).toBe(waiting + 1);
    // That process ends; nothing here takes the role back.
    holders.delete(role);
    await ticks(2);
    expect(holders.has(role)).toBe(false);
  });

  test("an abandoned hold's session this process registers again is its own hold again, not a newer holder", async () => {
    const role = "legion-controller";
    const {
      fixture,
      live,
      context,
      manager,
      holders,
      refuseRegistrations,
      failHandBacks,
      holdRegistration,
    } = await gatedRoleFixture("abandoned-hold-registered-here", "ses_sr");
    const own = [...manager.branch()];
    // A /new lands during a listener outage, so the hand-back of ses_sr is abandoned.
    const refused = refuseRegistrations(2);
    failHandBacks(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_sr_n", { fresh: true })
    );
    await refused;
    // A resume to a session whose records name the role waits on the abandoned hold.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    live("ses_sr_c", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    const resumedBranch = manager.branch;
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_sr_c"));
    expect(holders.get(role)).toBe("ses_sr");
    // A resume back to ses_sr registers it, which moves its `last_seen`, and the host moves on to
    // ses_sr_c again before that pass claims anything.
    const held = holdRegistration("ses_sr");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    manager.id = "ses_sr";
    manager.branch = () => own;
    const pending: unknown[] = [
      fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_sr")),
    ];
    await held.reached;
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    manager.id = "ses_sr_c";
    manager.branch = resumedBranch;
    pending.push(fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_sr_c")));
    held.release();
    await Promise.all(pending);
    expect(holders.get(role)).toBe("ses_sr_c");
  });

  for (const meanwhile of ["the listener reaped it", "a third session took the role"] as const) {
    test(`a hold taken back on returning to its abandoned session is claimed before it is trusted, when ${meanwhile}`, async () => {
      const role = "legion-controller";
      const notice = "notifications.legion.ACME.controller";
      const {
        fixture,
        live,
        context,
        manager,
        holders,
        claimSessions,
        refuseRegistrations,
        failHandBacks,
        tick,
      } = await gatedRoleFixture(`taken-back-claimed-${meanwhile.length}`, "ses_tb");
      const own = [...manager.branch()];
      // A /new lands during a listener outage, so the hand-back of ses_tb is abandoned.
      const refused = refuseRegistrations(2);
      failHandBacks(2);
      await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
      await fixture.handlers.get("session_switch")?.(
        { reason: "new" },
        live("ses_tb_n", { fresh: true })
      );
      await refused;
      if (meanwhile === "a third session took the role") holders.set(role, "ses_third");
      else holders.delete(role);
      // A resume back to ses_tb, whose records name the role.
      await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
      manager.id = "ses_tb";
      manager.branch = () => own;
      const before = claimSessions.length;
      await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_tb"));
      expect(claimSessions.slice(before)).toEqual([`${role}@ses_tb`]);
      if (meanwhile === "a third session took the role") {
        expect(holders.get(role)).toBe("ses_third");
        // The session does not hold the role, so a plugin opens no notices bound to it.
        await expect(subscribeLegionNotice("ses_tb", notice, live("ses_tb"), role)).rejects.toThrow(
          "does not hold role"
        );
      } else {
        expect(holders.get(role)).toBe("ses_tb");
        // The claim confirmed the hold, so later ticks only re-register.
        const confirmed = claimSessions.length;
        for (let ticks = 0; ticks < 2; ticks += 1) {
          tick();
          for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
        }
        expect(claimSessions.length).toBe(confirmed);
      }
    });
  }

  test("a registration outside a pass takes an abandoned hold back, so its moved last_seen is not read as a newer holder", async () => {
    const role = "legion-controller";
    const { fixture, live, context, manager, holders, refuseRegistrations, failHandBacks } =
      await gatedRoleFixture("abandoned-hold-registered-outside-pass", "ses_op");
    const own = [...manager.branch()];
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribeTool === undefined) throw new Error("envoy_subscribe was not registered");
    // A /new during a listener outage abandons the hold of ses_op.
    const outage = refuseRegistrations(2);
    failHandBacks(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "new" },
      live("ses_op_n", { fresh: true })
    );
    await outage;
    // A resume to a session whose records name the role waits on the abandoned hold.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    live("ses_op_c", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    const resumedBranch = manager.branch;
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_op_c"));
    // A resume back to ses_op meets another outage, so its pass registers nothing; the model's
    // envoy_subscribe then registers ses_op outside any pass.
    const second = refuseRegistrations(2);
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    manager.id = "ses_op";
    manager.branch = () => own;
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_op"));
    await second;
    expect((await subscribeTool.execute("", { topics: ["team.standup"] })).isError).not.toBe(true);
    // Back to the session whose records name the role: it takes the role.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    manager.id = "ses_op_c";
    manager.branch = resumedBranch;
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_op_c"));
    expect(holders.get(role)).toBe("ses_op_c");
  });

  test("a fork's run that ends after the host moved keeps the announcement, so the parent's frames arrive before any pass", async () => {
    const { fixture, live, context } = await gatedRoleFixture("fork-run-ends-after-move", "ses_fe");
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    await fixture.handlers.get("agent_start")?.({}, context);
    // A fork aborts no run: the host moves, and the run ends on the fork.
    const forked = live("ses_fe_f");
    await fixture.handlers.get("agent_end")?.(
      { messages: [{ role: "assistant", stopReason: "stop" }] },
      forked
    );
    const parent = natsState.controls.get("notifications.agent.ses_fe");
    if (parent === undefined) throw new Error("the parent's agent subject was not subscribed");
    parent.push(
      forwardedRoleEnvelope("legion-controller", "frame for the parent", "fe-parent"),
      "_INBOX.fe"
    );
    for (let step = 0; step < 5; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(fixture.messages.some((message) => message.includes("frame for the parent"))).toBe(true);
  });

  test("a hand-back the listener fails under the session's own id is made by a later pass", async () => {
    const { fixture, live, manager, holders, roleTool, failHandBacks, tick } =
      await gatedRoleFixture("own-id-hand-back-retried", "ses_oi");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const beforeMove = fixture.entries.length;
    expect((await roleTool.execute("", { role: "reviewer" })).isError).not.toBe(true);
    expect(holders.get("reviewer")).toBe("ses_oi");
    // A tree navigation back to before the move: its records name legion-controller, so its
    // event ends reviewer, whose hand-back the listener fails.
    failHandBacks(2);
    manager.branch = () => fixture.entries.slice(0, beforeMove);
    await fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_oi"));
    expect(holders.get("reviewer")).toBe("ses_oi");
    tick();
    for (let step = 0; step < 10; step += 1) await drain();
    expect(holders.has("reviewer")).toBe(false);
    expect(holders.get("legion-controller")).toBe("ses_oi");
  });

  test("a role the model claims again in a /new before its pass is carried by a fork that follows", async () => {
    const role = "legion-controller";
    const { fixture, live, context, holders, roleTool, holdRegistration } = await gatedRoleFixture(
      "reclaim-in-new-then-fork",
      "ses_rf"
    );
    const held = holdRegistration("ses_rf_n");
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    const pending: unknown[] = [
      fixture.handlers.get("session_switch")?.(
        { reason: "new" },
        live("ses_rf_n", { fresh: true })
      ),
    ];
    await held.reached;
    // The /new's event ended the role; the model claims it again in the new session.
    expect((await roleTool.execute("", { role })).isError).not.toBe(true);
    expect(holders.get(role)).toBe("ses_rf_n");
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    pending.push(fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_rf_f")));
    held.release();
    await Promise.all(pending);
    expect(holders.get(role)).toBe("ses_rf_f");
  });

  test("a session the host returns to while NATS was down gets its agent subject back once NATS is", async () => {
    const { fixture, live, context, manager, tick } = await gatedRoleFixture(
      "return-in-nats-outage",
      "ses_rn"
    );
    const own = [...manager.branch()];
    // NATS drops; a resume's pass closes the session's subject and cannot open the new one.
    natsState.connectionsClosed = true;
    natsState.failConnects = 99;
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      live("ses_rn_x", { fresh: true })
    );
    expect(natsState.controls.get("notifications.agent.ses_rn")?.active()).toBe(false);
    // The resume rolls back, and NATS comes back.
    manager.id = "ses_rn";
    manager.branch = () => own;
    natsState.connectionsClosed = false;
    natsState.failConnects = 0;
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(natsState.controls.get("notifications.agent.ses_rn")?.active()).toBe(true);
  });

  test("a heartbeat after a move with no event during a NATS outage registers the session and carries its role over HTTP, quietly", async () => {
    const { live, holders, registrations, notifications, tick } = await gatedRoleFixture(
      "move-in-nats-outage",
      "ses_mn"
    );
    natsState.connectionsClosed = true;
    natsState.failConnects = 99;
    live("ses_mn_moved");
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(registrations).toContain("ses_mn_moved");
    expect(holders.get("legion-controller")).toBe("ses_mn_moved");
    expect(notifications.filter((message) => message.includes("heartbeat failed"))).toEqual([]);
  });

  test("a role a session event ended and the model claims again before the pass stays held", async () => {
    const role = "legion-controller";
    const { fixture, live, holders, roleTool, handedBack, holdRegistration } =
      await gatedRoleFixture("ended-then-reclaimed", "ses_er");
    // A tree navigation to a point whose records release the role ends it at its event.
    fixture.pi.appendEntry("envoy-role-claim", { role: null });
    const held = holdRegistration("ses_er");
    const navigated = fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_er"));
    await held.reached;
    expect(holders.get(role)).toBe("ses_er");
    expect((await roleTool.execute("", { role })).isError).not.toBe(true);
    held.release();
    await navigated;
    expect(holders.get(role)).toBe("ses_er");
    expect(handedBack).not.toContain(`notifications.role.${role}@ses_er`);
  });

  test("the model's move off a role a fork holds under its parent hands that hold back under the parent", async () => {
    const { fixture, live, holders, roleTool, failClaims } = await gatedRoleFixture(
      "move-off-parent-hold",
      "ses_mp"
    );
    // The fork's carry fails at the listener, so the hold stays under the parent's id.
    failClaims(2);
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_mp_fork"));
    expect(holders.get("legion-controller")).toBe("ses_mp");
    const moved = await roleTool.execute("", { role: "reviewer" });
    expect(moved.details).toEqual({ role: "reviewer", released: "legion-controller" });
    expect(holders.has("legion-controller")).toBe(false);
    expect(holders.get("reviewer")).toBe("ses_mp_fork");
  });

  test("a plugin's claim while NATS is down registers the session and claims over HTTP, and the next pass opens its agent subject", async () => {
    natsState.failConnects = 99;
    const { live, holders, registrations, tick } = await gatedRoleFixture(
      "claim-nats-down",
      "ses_nd"
    );
    expect(registrations).toContain("ses_nd");
    expect(holders.get("legion-controller")).toBe("ses_nd");
    // A claim for a session the host switched to before pi-envoy's handler runs its own pass.
    const forked = live("ses_nd_fork");
    await claimEnvoyRole("ses_nd_fork", "legion-controller", forked);
    expect(registrations).toContain("ses_nd_fork");
    expect(holders.get("legion-controller")).toBe("ses_nd_fork");
    expect(natsState.controls.get("notifications.agent.ses_nd_fork")).toBeUndefined();
    natsState.failConnects = 0;
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(natsState.controls.get("notifications.agent.ses_nd_fork")?.active()).toBe(true);
  });

  for (const call of ["claim", "subscribe"] as const) {
    test(`a plugin's ${call} for a session the host switched to before pi-envoy's handler opens that session first`, async () => {
      const { live } = await gatedRoleFixture(`bridge-${call}-first`, "ses_bf");
      const forked = live("ses_bf_fork");
      if (call === "claim") await claimEnvoyRole("ses_bf_fork", "legion-controller", forked);
      else await subscribeLegionNotice("ses_bf_fork", "notifications.legion.ACME.other", forked);
      expect(natsState.controls.get("notifications.agent.ses_bf_fork")?.active()).toBe(true);
    });
  }

  test("a resume whose records name a role another process holds under an id this process gave up does not take it", async () => {
    const role = "legion-controller";
    const { fixture, live, context, holders } = await gatedRoleFixture("foreign-same-id", "ses_e");
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    // A resume to a session that records nothing: the role is handed back.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      live("ses_e_k", { fresh: true })
    );
    for (let step = 0; step < 5; step += 1) await drain();
    expect(holders.has(role)).toBe(false);
    // Another process resumes ses_e and holds the role under it.
    holders.set(role, "ses_e");
    // This process resumes a session whose records name the role.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const resumed = live("ses_e_q", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed);
    for (let step = 0; step < 5; step += 1) await drain();
    expect(holders.get(role)).toBe("ses_e");
  });

  test("a fork that overtakes a resume names the session the listener holds the role under", async () => {
    const role = "legion-controller";
    const { fixture, live, context, holders, holdRegistration } = await gatedRoleFixture(
      "fork-over-resume-holder",
      "ses_f"
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      live("ses_f_k", { fresh: true })
    );
    for (let step = 0; step < 5; step += 1) await drain();
    // The listener names P for the role from an earlier life; this process resumes P, whose
    // records name it, and a fork of P lands while P's registration is held.
    holders.set(role, "ses_f_p");
    const held = holdRegistration("ses_f_p");
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const toP = live("ses_f_p", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role });
    const resumed = fixture.handlers.get("session_switch")?.({ reason: "resume" }, toP);
    await held.reached;
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    const forked = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_f_f"));
    await drain();
    held.release();
    await resumed;
    await forked;
    for (let step = 0; step < 10; step += 1) await drain();
    expect(holders.get(role)).toBe("ses_f_f");
  });

  test("a fork rolled back after its event tells the agent of no id change it never saw", async () => {
    const { fixture, live, context, manager, tick, landed, holders } = await gatedRoleFixture(
      "fork-rolled-back-after-event",
      "ses_fr"
    );
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const own = manager.branch();
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_fr_b"));
    for (let step = 0; step < 5; step += 1) await drain();
    const notices = () =>
      landed.filter((message) => message.content.includes("session id changed"));
    expect(notices().map((message) => message.session)).toEqual(["ses_fr_b"]);
    // The host returns to the parent with no event.
    const returned = fixture.entries.length;
    manager.id = "ses_fr";
    manager.branch = () => [...own, ...fixture.entries.slice(returned)];
    for (let ticks = 0; ticks < 2; ticks += 1) {
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
    }
    // No notice lands in the parent, and the role comes back with the session.
    expect(notices().map((message) => message.session)).toEqual(["ses_fr_b"]);
    expect(holders.get("legion-controller")).toBe("ses_fr");
  });

  test("a resume rolled back after its event and its claim carries none of the resumed session's role back", async () => {
    const { fixture, live, context, claimSessions, manager, tick, notifications } =
      await gatedRoleFixture("resume-rolled-back-after-claim", "ses_ra", { claimAtStart: false });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    const own = [...manager.branch()];
    // A resume to a session whose transcript records a role; its event runs and it claims it.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    const resumed = live("ses_ra_x", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role: "release-captain" });
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, resumed);
    for (let step = 0; step < 5; step += 1) await drain();
    expect(claimSessions).toEqual(["release-captain@ses_ra_x"]);
    // Oh My Pi throws after the event and restores the session it left, with no event.
    manager.id = "ses_ra";
    manager.branch = () => own;
    for (let ticks = 0; ticks < 2; ticks += 1) {
      tick();
      for (let step = 0; step < 10; step += 1) await drain();
    }
    // The session returned to records no role, so it claims none, and the resumed session's
    // role it ended is warned of.
    expect(claimSessions).toEqual(["release-captain@ses_ra_x"]);
    expect(notifications.filter((message) => message.includes("release-captain"))).toHaveLength(1);
  });

  test("an establishment that fails before its reclaim leaves no handover for a later fork to inherit", async () => {
    const role = "legion-controller";
    const {
      fixture,
      live,
      context,
      claimSessions,
      holders,
      rows,
      gateInterest,
      refuseRegistrations,
    } = await gatedRoleFixture("handover-consumed", "ses_hc", { claimAtStart: false });
    const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
    rows.set("ses_hc", [`notifications.role.${role}`]);
    holders.set(role, "ses_hc");
    // A fork's reclaim finds the role in the parent's rows while a second fork lands, so it gives
    // way and hands the role on.
    const read = gateInterest("ses_hc", [`notifications.role.${role}`]);
    const first = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_hc_f1"));
    await read.reached;
    const second = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_hc_f2"));
    await drain();
    // The second fork's establishment fails at its registration, before its reclaim.
    const refused = refuseRegistrations(2);
    read.release();
    await first;
    await second;
    await refused;
    // The host resumes the first fork, which never held the role, and forks it.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, live("ses_hc_f1"));
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    await fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_hc_f3"));
    for (let step = 0; step < 10; step += 1) await drain();
    expect(claimSessions.filter((claim) => claim.endsWith("@ses_hc_f3"))).toEqual([]);
  });

  test("a reclaim left during its registration reads nothing of a resume target, and is settled after the rollback", async () => {
    const { fixture, live, context, holders, manager, tick, holdRegistration } =
      await gatedRoleFixture("reclaim-under-rolled-back-resume", "ses_q4");
    const forkTranscript = manager.branch;
    const held = holdRegistration("ses_q4_f");
    await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
    const forked = fixture.handlers.get("session_switch")?.({ reason: "fork" }, live("ses_q4_f"));
    await held.reached;
    // A resume moves the host to a session whose transcript records a release of the role.
    await fixture.handlers.get("session_before_switch")?.({ reason: "resume" }, context);
    live("ses_q4_y", { fresh: true });
    fixture.pi.appendEntry("envoy-role-claim", { role: null });
    held.release();
    await forked;
    // The resume fails and the host restores the fork, whose transcript records the role.
    manager.id = "ses_q4_f";
    manager.branch = () =>
      forkTranscript().filter(
        (entry) => (entry as { data?: { role?: unknown } }).data?.role !== null
      );
    tick();
    for (let step = 0; step < 10; step += 1) await new Promise<void>((r) => setImmediate(r));
    expect(holders.get("legion-controller")).toBe("ses_q4_f");
  });

  // The class every switch race above belongs to: pi-envoy's idea of the current session against
  // the host's. Each row holds a first switch's establishment at one stage while the host does
  // something else, with or without heartbeat ticks in that window, then lets everything run.
  // Whatever the interleaving, the end is the one the host's switches decide: the last session
  // holds the role only when every switch that took effect carried it, no session after a `/new`
  // or resume claims it, only the last session's agent subject is open, registered and bound for
  // Dispatch, the agent is told of each carried id change once, and a role a switch with no event
  // ended is warned of once.
  describe("every host switch against every establishment stage", () => {
    type Step = "fork" | "new" | "resume";
    const stages = ["starting", "queued", "registering", "reading", "claiming", "settled"] as const;
    const events = [
      { kind: "none", name: "nothing else happens", effective: [] },
      { kind: "fork", name: "a fork lands", effective: ["fork"] },
      { kind: "new", name: "a /new lands", effective: ["new"] },
      {
        kind: "fork-no-event",
        name: "a fork moves the host and sends no event",
        effective: ["fork"],
      },
      {
        kind: "new-no-event",
        name: "a /new moves the host and sends no event",
        effective: ["new"],
      },
      { kind: "resume", name: "a resume lands", effective: ["resume"] },
      { kind: "rollback", name: "a resume moves the host and rolls back", effective: [] },
      {
        kind: "landed-rollback",
        name: "a resume lands and rolls back",
        effective: [],
      },
      {
        kind: "plugin-rollback",
        name: "a resume lands, a plugin claims at its event, and it rolls back",
        effective: [],
      },
      {
        kind: "plugin-new-rollback",
        name: "a /new lands, a plugin claims at its event, and it rolls back",
        effective: [],
      },
      {
        kind: "new-then-fork",
        name: "a /new sends no event, then a fork lands",
        effective: ["new", "fork"],
      },
      {
        kind: "fork-then-fork",
        name: "a fork sends no event, then a fork lands",
        effective: ["fork", "fork"],
      },
      {
        kind: "fork-then-new",
        name: "a fork sends no event, then a /new lands",
        effective: ["fork", "new"],
      },
      { kind: "cancelled", name: "a fork is announced and cancelled", effective: [] },
    ] as const satisfies readonly { kind: string; name: string; effective: readonly Step[] }[];
    const role = "legion-controller";
    const roleTopic = `notifications.role.${role}`;
    const notice = "notifications.legion.ACME.controller";
    // A role of the resumed session's own.
    const other = "release-captain";
    const ids = ["ses_c", "ses_c_a", "ses_c_b", "ses_c_c", "ses_c_x"] as const;
    // A resume's target can record the role itself (a session that held it in an earlier life,
    // the way pi-legion's controller resumes its own), while the listener still names the session
    // the resume left, this process's own hold.
    for (const [first, firstRecords] of [
      ["fork", false],
      ["new", false],
      ["resume", false],
      ["resume", true],
    ] as const) {
      for (const stage of stages) {
        // Only a carried role is claimed, so only a fork's establishment reaches a claim.
        if (stage === "claiming" && first !== "fork") continue;
        // A target recording the role reads no listener rows, and a resumed start holds nothing.
        if (firstRecords && (stage === "reading" || stage === "starting")) continue;
        for (const event of events) {
          for (const ticksInWindow of [0, 2] as const) {
            // A start held at its registration has no heartbeat yet to tick.
            if (stage === "starting" && ticksInWindow > 0) continue;
            // A plugin that claims at session events exists only where the role is held with one.
            const pluginEvent =
              event.kind === "plugin-rollback" || event.kind === "plugin-new-rollback";
            if (pluginEvent && (stage === "reading" || stage === "starting")) continue;
            const ticking = ticksInWindow === 0 ? "" : ", the heartbeat ticking meanwhile";
            const recording = firstRecords ? " recording the role" : "";
            test(`a ${first}${recording} held ${stage} when ${event.name}${ticking}`, async () => {
              // Where the role is before the first switch: held by the session with a plugin's
              // role-bound notice; only in the listener's rows (the one source a reclaim reads over
              // the network); or, for a resumed start, in the transcript with nothing held yet.
              const rowsOnly = stage === "reading";
              const resumedStart = stage === "starting";
              const held = !rowsOnly && !resumedStart;
              const h = await gatedRoleFixture(
                `class-${first}${firstRecords ? "-own" : ""}-${stage}-${event.kind}-${ticksInWindow}`,
                "ses_c",
                resumedStart
                  ? { claimAtStart: false, transcriptRole: role, holdStart: true }
                  : { claimAtStart: held }
              );
              if (rowsOnly) {
                h.rows.set("ses_c", [roleTopic]);
                h.holders.set(role, "ses_c");
              }
              if (held) await subscribeLegionNotice("ses_c", notice, h.context, role);
              const drain = async (times = 5) => {
                for (let step = 0; step < times; step += 1) {
                  await new Promise<void>((resolve) => setImmediate(resolve));
                }
              };
              const pending: Promise<unknown>[] = [];
              // Oh My Pi announces a switch, moves its manager, then sends the switch's event (left
              // running here, since it can queue behind a held establishment).
              // A resumed session (`ownRole`) records a role of its own, which the listener still
              // names it for from its earlier life.
              const switchTo = async (
                reason: Step,
                id: string,
                withEvent = true,
                ownRole?: string,
                listenerNamesIt = true
              ) => {
                await h.fixture.handlers.get("session_before_switch")?.({ reason }, h.context);
                const moved = h.live(id, { fresh: reason !== "fork" });
                if (ownRole !== undefined) {
                  h.fixture.pi.appendEntry("envoy-role-claim", { role: ownRole });
                  if (listenerNamesIt) h.holders.set(ownRole, id);
                }
                if (withEvent) {
                  pending.push(
                    Promise.resolve(h.fixture.handlers.get("session_switch")?.({ reason }, moved))
                  );
                }
                return moved;
              };
              // The first switch, whose target may record the role while the listener names ses_c.
              const switchFirst = () =>
                switchTo(first, "ses_c_a", true, firstRecords ? role : undefined, false);
              let release = () => {};
              if (stage === "starting") {
                await h.startReached;
                pending.push(Promise.resolve(h.started));
                release = h.releaseStart;
                await switchTo(first, "ses_c_a");
                await drain();
              } else if (stage === "queued") {
                // A tree navigation's establishment holds the queue at its registration.
                const gate = h.holdRegistration("ses_c");
                pending.push(
                  Promise.resolve(
                    h.fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, h.context)
                  )
                );
                await gate.reached;
                release = gate.release;
                await switchFirst();
                await drain();
              } else if (stage === "settled") {
                await switchFirst();
                await Promise.all(pending);
                await drain();
              } else {
                const gate =
                  stage === "registering"
                    ? h.holdRegistration("ses_c_a")
                    : stage === "claiming"
                      ? h.gate(role)
                      : first === "fork"
                        ? h.gateInterest("ses_c", [roleTopic])
                        : h.gateInterest("ses_c_a", []);
                await switchFirst();
                await gate.reached;
                release = gate.release;
              }
              let afterRelease = () => {};
              let beforeRollback = async () => {};
              switch (event.kind) {
                case "none":
                  break;
                case "fork":
                case "new":
                  await switchTo(event.kind, "ses_c_b");
                  break;
                case "fork-no-event":
                  await switchTo("fork", "ses_c_b", false);
                  break;
                case "new-no-event":
                  await switchTo("new", "ses_c_b", false);
                  break;
                case "resume":
                  await switchTo("resume", "ses_c_b", true, other);
                  break;
                case "rollback":
                case "landed-rollback":
                case "plugin-rollback":
                case "plugin-new-rollback": {
                  // The session the host returns to has its own records: those before the switch,
                  // and whatever it writes after the return, never the switched-to session's.
                  const own = h.manager.branch();
                  const plugin =
                    event.kind === "plugin-rollback" || event.kind === "plugin-new-rollback";
                  const moved = await switchTo(
                    event.kind === "plugin-new-rollback" ? "new" : "resume",
                    "ses_c_x",
                    event.kind !== "rollback",
                    plugin ? undefined : other
                  );
                  if (plugin) {
                    // A plugin that claims at every session event (pi-legion's controller).
                    beforeRollback = async () => {
                      await claimEnvoyRole("ses_c_x", role, moved);
                      await subscribeLegionNotice("ses_c_x", notice, moved, role);
                    };
                  }
                  afterRelease = () => {
                    const returned = h.fixture.entries.length;
                    h.manager.id = "ses_c_a";
                    h.manager.branch = () => [...own, ...h.fixture.entries.slice(returned)];
                  };
                  break;
                }
                case "new-then-fork":
                case "fork-then-fork":
                case "fork-then-new": {
                  const [missed, landing] =
                    event.kind === "new-then-fork"
                      ? (["new", "fork"] as const)
                      : event.kind === "fork-then-fork"
                        ? (["fork", "fork"] as const)
                        : (["fork", "new"] as const);
                  await switchTo(missed, "ses_c_b", false);
                  await switchTo(landing, "ses_c_c");
                  break;
                }
                case "cancelled":
                  await h.fixture.handlers.get("session_before_switch")?.(
                    { reason: "fork" },
                    h.context
                  );
                  break;
              }
              await drain();
              for (let tick = 0; tick < ticksInWindow; tick += 1) {
                h.tick();
                await drain();
              }
              release();
              await Promise.all(pending);
              await drain(10);
              await beforeRollback();
              await drain(10);
              afterRelease();
              for (let tick = 0; tick < 3; tick += 1) {
                h.tick();
                await drain(10);
              }

              const chain: readonly Step[] = [first, ...event.effective];
              const chainIds = ["ses_c_a", "ses_c_b", "ses_c_c"].slice(0, chain.length);
              const last = chainIds.at(-1) ?? "ses_c_a";
              expect(h.registrations).toContain(last);
              expect(process.env.DISPATCH_SESSION_ID).toBe(last);
              expect(natsState.controls.get(`notifications.agent.${last}`)?.active()).toBe(true);
              for (const left of ids.filter((id) => id !== last)) {
                expect(
                  natsState.controls.get(`notifications.agent.${left}`)?.active() ?? false
                ).toBe(false);
              }
              // A step keeps the role when it carries it (a fork) or when its target records it
              // (the first resume, in the rows where it does); the session the host returns to in
              // a rollback records it after a fork or such a resume.
              const keeps = (step: Step, index: number) =>
                step === "fork" || (index === 0 && firstRecords);
              const replacedAt = chain.findIndex((step, index) => !keeps(step, index));
              const roleWarnings = h.notifications.filter((message) =>
                message.includes("with no session event")
              );
              const closedWarnings = h.notifications.filter((message) =>
                message.includes("stay closed")
              );
              if (replacedAt === -1) {
                expect(h.holders.get(role)).toBe(last);
                // The plugin's notices are open, unless a role end closed them on the way, which
                // pi-envoy cannot undo: then a warning says they stay closed until the plugin
                // claims again.
                if (held && natsState.controls.get(notice)?.active() !== true) {
                  expect(closedWarnings.length + roleWarnings.length).toBeGreaterThan(0);
                }
              } else {
                const after = chainIds.slice(replacedAt);
                expect(
                  h.claimSessions.filter((claim) => after.some((id) => claim === `${role}@${id}`))
                ).toEqual([]);
                if (held) expect(natsState.controls.get(notice)?.active() ?? false).toBe(false);
                // The hold this process had is handed back, a role a plugin claimed at a
                // rolled-back switch's event included: the listener names no session for it.
                if (held) expect(h.holders.has(role)).toBe(false);
              }
              // A carried switch changes the id the agent knows: each notice lands in the session
              // it names, once at most, and the last session's agent is told exactly when a
              // carried switch opened it.
              const idNotices = h.landed.filter((message) =>
                message.content.includes("session id changed")
              );
              const named = (content: string) =>
                /(?:^|[^_])session_id:\s*"?([A-Za-z0-9_]+)/.exec(content)?.[1];
              for (const message of idNotices) {
                expect(named(message.content)).toBe(message.session);
              }
              expect(new Set(idNotices.map((message) => message.session)).size).toBe(
                idNotices.length
              );
              expect(idNotices.some((message) => named(message.content) === last)).toBe(
                chain.at(-1) === "fork"
              );
              // A `/new` that sent no event, which no plugin heard, ends a role still held with a
              // warning naming it as the plugin's to claim again: the plugin's notices are still
              // open then, after a fork or a resume whose records name the role alike.
              const heldAtSecond = first === "fork" || (firstRecords && stage === "settled");
              if (held && event.kind === "new-no-event" && heldAtSecond) {
                const named = roleWarnings.filter((message) => message.includes(role));
                expect(named).toHaveLength(1);
                expect(named[0]).toContain("the plugin that opened its notices");
              }
              // Every such warning names a role and the session the host was on.
              for (const message of roleWarnings) {
                expect(message).toMatch(/role \S+ ended: the host moved to session ses_c_\w+/);
              }
              // The resumed session's own role is its own: a session the host lands on takes it, a
              // session the host left hands it back, and no other session claims it.
              if (event.kind === "resume") expect(h.holders.get(other)).toBe("ses_c_b");
              const resumed = event.kind === "resume" ? "ses_c_b" : "ses_c_x";
              expect(
                h.claimSessions.filter(
                  (claim) => claim.startsWith(`${other}@`) && claim !== `${other}@${resumed}`
                )
              ).toEqual([]);
              if (resumed === "ses_c_x" && h.claimSessions.includes(`${other}@ses_c_x`)) {
                expect(h.holders.has(other)).toBe(false);
              }
            });
          }
        }
      }
    }
  });

  for (const reason of ["new", "resume"] as const) {
    test(`after a ${reason === "new" ? "/new" : "resume"} completes, a frame on the new session's agent subject arrives and is acknowledged`, async () => {
      const { fixture, live, context } = await gatedRoleFixture(`frame-after-${reason}`, "ses_ac");
      await fixture.handlers.get("session_before_switch")?.({ reason }, context);
      const moved = live("ses_ac_next", { fresh: reason === "new" });
      await fixture.handlers.get("session_switch")?.({ reason }, moved);
      const subject = natsState.controls.get("notifications.agent.ses_ac_next");
      if (subject === undefined) throw new Error("the new agent subject was not subscribed");
      const inbox = `_INBOX.after-${reason}`;
      subject.push(
        forwardedRoleEnvelope("legion-controller", `frame after ${reason}`, `after-${reason}`),
        inbox
      );
      for (let drain = 0; drain < 5; drain += 1) {
        await new Promise<void>((resolve) => setImmediate(resolve));
      }
      expect(fixture.messages.some((message) => message.includes(`frame after ${reason}`))).toBe(
        true
      );
      expect(natsState.published.some((each) => each.subject === inbox)).toBe(true);
    });
  }

  for (const kind of ["fork", "branch"] as const) {
    test(`a heartbeat tick inside a ${kind}'s window leaves the ${kind}'s event to tell the agent its id changed`, async () => {
      const { fixture, live, context, tick } = await gatedRoleFixture(
        `tick-in-${kind}-window`,
        "ses_tw"
      );
      if (kind === "fork") {
        await fixture.handlers.get("session_before_switch")?.({ reason: "fork" }, context);
      } else {
        await fixture.handlers.get("session_before_branch")?.({ entryId: "entry" }, context);
      }
      const moved = live("ses_tw_next");
      tick();
      await new Promise<void>((resolve) => setImmediate(resolve));
      if (kind === "fork") {
        await fixture.handlers.get("session_switch")?.({ reason: "fork" }, moved);
      } else {
        await fixture.handlers.get("session_branch")?.(
          { previousSessionFile: "/tmp/old.jsonl" },
          moved
        );
      }
      const notices = fixture.deliveries.filter(
        (delivery) =>
          String(delivery.content).includes("session id changed") &&
          String(delivery.content).includes("ses_tw_next")
      );
      expect(notices).toHaveLength(1);
    });
  }

  test("a heartbeat tick after a switch another extension cancelled still checks the role", async () => {
    const { fixture, context, roleTool, claims, holders, tick, nextRegistration } =
      await gatedRoleFixture("cancelled-switch", "ses_cs");
    // The switch was announced, then cancelled: the manager never moved and no event follows.
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    // The listener lost the claim, so the check claims the role again.
    holders.delete("legion-controller");
    const before = claims.length;
    const tickRegistered = nextRegistration("ses_cs");
    tick();
    await tickRegistered;
    await new Promise<void>((resolve) => setImmediate(resolve));
    // A re-claim of the held role queues behind the check, so the check has run when it returns.
    await roleTool.execute("", { role: "legion-controller" });
    expect(claims.slice(before)).toEqual(["legion-controller", "legion-controller"]);
  });

  test("a fresh session's first id, minted after a switch another extension cancelled, is still healed", async () => {
    const { fixture, context, manager, registrations, tick } = await gatedRoleFixture(
      "cancelled-switch-fresh",
      "",
      { claimAtStart: false }
    );
    // A fresh terminal has no id yet; a switch is announced and cancelled before it gets one.
    await fixture.handlers.get("session_before_switch")?.({ reason: "new" }, context);
    // The host then mints the session's first id, with no session event.
    manager.id = "ses_minted";
    tick();
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(registrations).toContain("ses_minted");
  });

  test("a claim that returns between the host's move and its switch event is handed back by the next pass and recorded nowhere", async () => {
    const { fixture, live, context, handedBack, gate } = await gatedRoleFixture(
      "claim-in-host-window",
      "ses_cw"
    );
    const held = gate("reviewer");
    const claimed = claimEnvoyRole("ses_cw", "reviewer", context);
    await held.reached;
    const moved = await hostMoves(fixture, live, context, "ses_cw_new");
    held.release();
    await expect(claimed).rejects.toThrow("the session moved from ses_cw");
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
    expect(handedBack).toContain("notifications.role.reviewer@ses_cw");
    expect(claimEntryRoles(fixture.entries)).not.toContain("reviewer");
  });

  test("a claim whose turn comes between the host's move and its switch event sends nothing", async () => {
    const { fixture, live, context, roleTool, claimSessions, gate } = await gatedRoleFixture(
      "queued-claim-in-host-window",
      "ses_qw"
    );
    // The model's move holds the role-move queue, and a plugin's claim waits behind it.
    const held = gate("release-captain");
    const moving = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    const claimed = claimEnvoyRole("ses_qw", "reviewer", context);
    const moved = await hostMoves(fixture, live, context, "ses_qw_new");
    held.release();
    await moving;
    await expect(claimed).rejects.toThrow("nothing was claimed");
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
    expect(claimSessions.filter((claim) => claim.startsWith("reviewer@"))).toEqual([]);
  });

  for (const phase of ["read", "claim"] as const) {
    test(`a heartbeat's role check whose ${phase} the host's move to a /new outlasts fires no regain`, async () => {
      const { fixture, live, context, holders, refuseRegistrations, tick, gate, gateRoleRead } =
        await gatedRoleFixture(`heartbeat-${phase}-in-host-window`, `ses_${phase}w`);
      const regained: string[] = [];
      onEnvoyRoleRegained(async (role: string, reason: string) => {
        regained.push(`${role}:${reason}`);
      });
      const drain = () => new Promise<void>((resolve) => setImmediate(resolve));
      const refused = refuseRegistrations(2);
      tick();
      await refused;
      await drain();
      if (phase === "claim") holders.delete("legion-controller");
      const held = phase === "read" ? gateRoleRead("legion-controller") : gate("legion-controller");
      tick();
      await held.reached;
      const moved = await hostMoves(fixture, live, context, `ses_${phase}w_new`);
      held.release();
      await drain();
      await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
      await drain();
      expect(regained).toEqual([]);
    });
  }

  test("a reclaim whose turn comes between the host's move and its switch event does nothing", async () => {
    const { fixture, live, context, roleTool, claimSessions, gate, nextRegistration } =
      await gatedRoleFixture("reclaim-in-host-window", "ses_rw");
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    try {
      // A tree navigation's reclaim waits behind the model's held move.
      const held = gate("release-captain");
      const moving = roleTool.execute("", { role: "release-captain" });
      await held.reached;
      const registering = nextRegistration("ses_rw");
      const rebound = fixture.handlers.get("session_tree")?.({ newLeafId: "leaf" }, live("ses_rw"));
      await registering;
      const moved = await hostMoves(fixture, live, context, "ses_rw_new");
      const claimsBefore = claimSessions.length;
      const entriesBefore = claimEntryRoles(fixture.entries);
      held.release();
      await moving.catch(() => undefined);
      await rebound;
      // Neither the reclaim nor anything else sent a claim or recorded one in the window.
      expect(claimSessions.slice(claimsBefore)).toEqual([]);
      expect(claimEntryRoles(fixture.entries)).toEqual(entriesBefore);
      await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
      expect(warnings).not.toContain("envoy: role reclaim failed");
    } finally {
      stopSink();
    }
  });

  // A frame on the replaced session's agent subject is addressed to that session; one on a subject
  // its role holds is addressed to the role, and arrives until the switch ends the role.
  for (const { name, reason, moves, replaced } of [
    { name: "a /new replaces", reason: "new", moves: true, replaced: true },
    { name: "a resume replaces", reason: "resume", moves: true, replaced: true },
    { name: "a fork carries on", reason: "fork", moves: true, replaced: false },
    { name: "a cancelled /new keeps", reason: "new", moves: false, replaced: false },
  ] as const) {
    for (const channel of ["agent subject", "role-bound subject"] as const) {
      const arrives = !replaced || channel === "role-bound subject";
      test(`a frame on the ${channel} of the session ${name}, arriving before the switch's event, ${arrives ? "arrives" : "is left undelivered"}`, async () => {
        const slug = `${name}-${channel}`.replaceAll(/[^a-z]+/g, "-");
        const notice = "notifications.legion.ACME.controller";
        const { fixture, live, context } = await gatedRoleFixture(`inbound-${slug}`, "ses_in");
        await subscribeLegionNotice("ses_in", notice, context, "legion-controller");
        const subject = natsState.controls.get(
          channel === "agent subject" ? "notifications.agent.ses_in" : notice
        );
        if (subject === undefined) throw new Error(`the ${channel} was not subscribed`);
        await fixture.handlers.get("session_before_switch")?.({ reason }, context);
        if (moves) live("ses_in_next", { fresh: reason === "new" });
        const inbox = `_INBOX.${slug}`;
        subject.push(
          channel === "agent subject"
            ? forwardedRoleEnvelope("legion-controller", `frame ${slug}`, slug)
            : JSON.stringify({
                event_id: `evt-${slug}`,
                source: "legion",
                source_event_id: `source-${slug}`,
                topic: notice,
                dedupe_key: `legion.notice.${slug}`,
                issued_at: 1,
                payload_summary: `frame ${slug}`,
                trace_id: `trace-${slug}`,
              }),
          inbox
        );
        for (let drain = 0; drain < 5; drain += 1) {
          await new Promise<void>((resolve) => setImmediate(resolve));
        }
        expect(fixture.messages.some((message) => message.includes(`frame ${slug}`))).toBe(arrives);
        if (channel === "agent subject") {
          expect(natsState.published.some((each) => each.subject === inbox)).toBe(arrives);
        }
      });
    }
  }

  test("after a /new, a role the new session claims takes its role-bound frames", async () => {
    const notice = "notifications.legion.ACME.reviewer";
    const { fixture, live, context } = await gatedRoleFixture("inbound-after-new", "ses_an");
    const moved = await hostMoves(fixture, live, context, "ses_an_new");
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, moved);
    await claimEnvoyRole("ses_an_new", "legion-reviewer", moved);
    await subscribeLegionNotice("ses_an_new", notice, moved, "legion-reviewer");
    const subject = natsState.controls.get(notice);
    if (subject === undefined) throw new Error("the role-bound subject was not subscribed");
    subject.push(
      JSON.stringify({
        event_id: "evt-after-new",
        source: "legion",
        source_event_id: "source-after-new",
        topic: notice,
        dedupe_key: "legion.notice.after-new",
        issued_at: 1,
        payload_summary: "frame for the new session's role",
        trace_id: "trace-after-new",
      })
    );
    for (let drain = 0; drain < 5; drain += 1) {
      await new Promise<void>((resolve) => setImmediate(resolve));
    }
    expect(
      fixture.messages.some((message) => message.includes("frame for the new session's role"))
    ).toBe(true);
  });

  test("role moves run one at a time, so a move back and a plugin's subscription after it both stand", async () => {
    const notice = "notifications.legion.ACME.controller";
    const { context, roleTool, gate } = await gatedRoleFixture("role-moves-back", "ses_role_back");
    const held = gate("release-captain");
    const away = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    const back = roleTool.execute("", { role: "legion-controller" });
    const subscribed = subscribeLegionNotice("ses_role_back", notice, context, "legion-controller");
    held.release();
    expect((await away).details).toEqual({
      role: "release-captain",
      released: "legion-controller",
    });
    expect((await back).details).toEqual({
      role: "legion-controller",
      released: "release-captain",
    });
    await subscribed;
    expect(natsState.controls.get(notice)?.active()).toBe(true);
  });

  test("a second move reads the role the first left it in, and a subscription for that role is refused", async () => {
    const notice = "notifications.legion.ACME.release";
    const { context, roleTool, gate } = await gatedRoleFixture(
      "role-moves-twice",
      "ses_role_twice"
    );
    const held = gate("release-captain");
    const first = roleTool.execute("", { role: "release-captain" });
    await held.reached;
    const second = roleTool.execute("", { role: "reviewer" });
    const subscribed = subscribeLegionNotice("ses_role_twice", notice, context, "release-captain");
    held.release();
    expect((await first).details).toEqual({
      role: "release-captain",
      released: "legion-controller",
    });
    expect((await second).details).toEqual({ role: "reviewer", released: "release-captain" });
    await expect(subscribed).rejects.toThrow("does not hold role release-captain");
    expect(natsState.controls.get(notice)).toBeUndefined();
  });

  test("the model's role move waits for a plugin's claim in flight and names the role that claim took", async () => {
    const { context, roleTool, claims, gate } = await gatedRoleFixture(
      "role-move-after-claim",
      "ses_role_claim"
    );
    const held = gate("release-captain");
    const claimed = claimEnvoyRole("ses_role_claim", "release-captain", context);
    await held.reached;
    const moved = roleTool.execute("", { role: "reviewer" });
    held.release();
    await claimed;
    expect((await moved).details).toEqual({ role: "reviewer", released: "release-captain" });
    expect(claims).toEqual(["legion-controller", "release-captain", "reviewer"]);
  });

  test("a plugin's role-bound subscription that fails to open does not keep the role in place", async () => {
    const notice = "notifications.legion.ACME.controller";
    const { context, roleTool } = await gatedRoleFixture(
      "role-bound-subscribe-fails",
      "ses_role_fail"
    );
    natsState.failSubscribes = 1;
    await expect(
      subscribeLegionNotice("ses_role_fail", notice, context, "legion-controller")
    ).rejects.toThrow("SUBSCRIBE_FAILED");
    const moved = await roleTool.execute("", { role: "reviewer" });
    expect(moved.isError).toBeFalsy();
    expect(moved.details).toEqual({ role: "reviewer", released: "legion-controller" });
  });

  test("injects a post-takeover forwarded role event into B only", async () => {
    const role = "legion-controller";
    const roleTopic = `notifications.role.${role}`;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      if (url.pathname === "/v1/roles/set") {
        const holder = typeof body?.session_id === "string" ? body.session_id : "";
        return response({
          session_id: holder,
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [roleTopic],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtensionA } = await import("./envoy.ts?role-agent-takeover-a");
    const fixtureA = createPi();
    envoyExtensionA(fixtureA.pi);
    await fixtureA.handlers.get("session_start")?.({}, sessionContext("ses_role_a"));
    const roleToolA = fixtureA.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleToolA === undefined) throw new Error("role tool was not registered for session A");
    await roleToolA.execute("", { role });

    const { default: envoyExtensionB } = await import("./envoy.ts?role-agent-takeover-b");
    const fixtureB = createPi();
    const injectedB = Promise.withResolvers<void>();
    envoyExtensionB({
      ...fixtureB.pi,
      sendMessage: (message, options) => {
        fixtureB.pi.sendMessage(message, options);
        injectedB.resolve();
      },
    });
    await fixtureB.handlers.get("session_start")?.({}, sessionContext("ses_role_b"));
    const roleToolB = fixtureB.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleToolB === undefined) throw new Error("role tool was not registered for session B");
    await roleToolB.execute("", { role });

    expect(natsState.controls.get(roleTopic)).toBeUndefined();
    const agentB = natsState.controls.get("notifications.agent.ses_role_b");
    if (agentB === undefined) throw new Error("B agent subject was not subscribed");
    agentB.push(forwardedRoleEnvelope(role, "post-takeover role event", "role-agent-takeover"));
    await injectedB.promise;
    expect(fixtureA.messages.some((message) => message.includes("post-takeover role event"))).toBe(
      false
    );
    const takeoverDelivery = fixtureB.messages[0];
    if (takeoverDelivery === undefined) throw new Error("takeover delivery was not injected");
    expect(decode(takeoverDelivery)).toEqual({
      envoy: {
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-role-agent-takeover",
        summary: "post-takeover role event",
      },
    });
  });

  test("keeps forwarded role delivery live while the listener HTTP API is unavailable", async () => {
    const role = "legion-controller";
    let listenerAvailable = true;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/roles/set") {
        return response({
          session_id: "ses_role_a",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (!listenerAvailable) throw new Error("listener API unavailable");
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-agent-listener-down");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_role_a"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role });

    const agent = natsState.controls.get("notifications.agent.ses_role_a");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    listenerAvailable = false;
    agent.push(
      forwardedRoleEnvelope(role, "role event while listener is down", "role-agent-listener-down")
    );
    await injected.promise;
    expect(
      fixture.messages.some((message) => message.includes("role event while listener is down"))
    ).toBe(true);
  });

  test("a role claim keeps the existing registration fresh", async () => {
    const registrations: {
      readonly session_id: string;
      readonly self_subscribed: boolean;
      readonly topics: readonly string[];
    }[] = [];
    const heartbeatRegistration = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/roles/set") {
        return response({
          session_id: "ses_role_heartbeat",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: ["notifications.role.legion-controller"],
        });
      }
      if (url.pathname === "/v1/roles/legion-controller") {
        return response({ role: "legion-controller", holder: "ses_role_heartbeat", last_seen: 1 });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly self_subscribed: boolean;
          readonly topics: readonly string[];
        };
        registrations.push(body);
        if (registrations.length === 2) heartbeatRegistration.resolve();
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-claim-heartbeat");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_role_heartbeat"),
      setInterval: (callback) => intervals.push(callback),
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(intervals).toHaveLength(1);
    expect(registrations).toHaveLength(1);
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role: "legion-controller" });

    intervals[0]?.();
    await heartbeatRegistration.promise;
    expect(registrations).toHaveLength(2);
    expect(registrations[1]).toMatchObject({
      session_id: "ses_role_heartbeat",
      self_subscribed: true,
      topics: ["notifications.agent.ses_role_heartbeat"],
    });
  });

  test("unsubscribing all topics preserves a held role for heartbeat re-assertion", async () => {
    const role = "librarian";
    const roleTopic = `notifications.role.${role}`;
    let listenerHoldsClaim = true;
    const roleClaims: Record<string, unknown>[] = [];
    const reasserted = Promise.withResolvers<void>();
    const unsubscriptions: (readonly string[])[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_unsubscribe", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        if (body.soft === true) reasserted.resolve();
        listenerHoldsClaim = true;
        return response({
          session_id: "ses_unsubscribe",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [roleTopic],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as { readonly topics?: unknown };
        const topics = Array.isArray(body.topics)
          ? body.topics.filter((topic): topic is string => typeof topic === "string")
          : [];
        unsubscriptions.push(topics);
        if (topics.includes(roleTopic)) listenerHoldsClaim = false;
        return response({});
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?unsubscribe-preserves-role");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_unsubscribe"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    if (roleTool === undefined || subscribeTool === undefined || unsubscribeTool === undefined) {
      throw new Error("role tools were not registered");
    }
    const subscription = "notifications.dispatch.issue.LEGION-1.>";
    await roleTool.execute("", { role });
    await subscribeTool.execute("", { topics: [subscription] });
    await unsubscribeTool.execute("", {});

    expect(unsubscriptions).toEqual([["notifications.dispatch.issue.LEGION-1", subscription]]);
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await reasserted.promise;
    expect(roleClaims).toEqual([
      { session_id: "ses_unsubscribe", role },
      { session_id: "ses_unsubscribe", role, soft: true },
    ]);
  });

  test("unsubscribes role-shaped subscriptions locally without releasing the held role", async () => {
    const role = "librarian";
    const roleTopic = `notifications.role.${role}`;
    const agentTopic = "notifications.agent.ses_role_subscription";
    const unsubscriptions: (readonly string[])[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/roles/set") {
        return response({
          session_id: "ses_role_subscription",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [roleTopic],
        });
      }
      if (url.pathname === "/v1/interests/ses_role_subscription") {
        return response({
          session_id: "ses_role_subscription",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [agentTopic, roleTopic],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as { readonly topics?: unknown };
        unsubscriptions.push(
          Array.isArray(body.topics)
            ? body.topics.filter((topic): topic is string => typeof topic === "string")
            : []
        );
        return response({});
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-subscription-unsubscribe");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_role_subscription"));

    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const listTool = fixture.tools.find((tool) => tool.name === "envoy_list");
    if (
      roleTool === undefined ||
      subscribeTool === undefined ||
      unsubscribeTool === undefined ||
      listTool === undefined
    ) {
      throw new Error("role tools were not registered");
    }
    await roleTool.execute("", { role });
    await subscribeTool.execute("", { topics: [roleTopic] });
    const explicitlySubscribed = natsState.controls.get(roleTopic);
    if (explicitlySubscribed === undefined)
      throw new Error("role topic was not locally subscribed");

    const explicit = await unsubscribeTool.execute("", { topics: [roleTopic] });
    expect(explicit.content[0]?.text).toBe(`Unsubscribed: ${roleTopic}`);
    expect(explicitlySubscribed.active()).toBe(false);
    expect(unsubscriptions).toEqual([]);
    expect((await listTool.execute("", {})).details.interests).toEqual([
      { topic: agentTopic, source: "both" },
      { topic: roleTopic, source: "registry" },
    ]);
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);

    await subscribeTool.execute("", { topics: [roleTopic] });
    const subscribedAgain = natsState.controls.get(roleTopic);
    if (subscribedAgain === undefined) throw new Error("role topic was not re-subscribed");
    const all = await unsubscribeTool.execute("", {});
    expect(all.content[0]?.text).toBe(`Unsubscribed: ${roleTopic}`);
    expect(subscribedAgain.active()).toBe(false);
    expect(unsubscriptions).toEqual([]);
    expect((await listTool.execute("", {})).details.interests).toEqual([
      { topic: agentTopic, source: "both" },
      { topic: roleTopic, source: "registry" },
    ]);
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
  });

  test("a heartbeat tick re-asserts a held role the listener no longer holds, softly and without a transcript entry", async () => {
    const role = "legion-controller";
    let listenerHoldsClaim = true;
    const roleClaims: Record<string, unknown>[] = [];
    const roleReads: string[] = [];
    const reasserted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads.push(url.pathname);
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_reassert", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        listenerHoldsClaim = true;
        if (body.soft === true) reasserted.resolve();
        return response({
          session_id: "ses_reassert",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-reassert");
    const regained: { readonly role: string; readonly reason: string }[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_reassert"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role });
    expect(roleClaims).toEqual([{ session_id: "ses_reassert", role }]);

    // The listener drops the claim out from under the session (a reaped claim, NATS data
    // loss, an older listener build). Nothing in this process notices until the heartbeat.
    listenerHoldsClaim = false;
    intervals[0]?.();
    await reasserted.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(roleReads).toEqual([`/v1/roles/${role}`]);
    expect(roleClaims).toEqual([
      { session_id: "ses_reassert", role },
      { session_id: "ses_reassert", role, soft: true },
    ]);
    // The held role did not change, so the durable record is not rewritten.
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
    expect(regained).toEqual([{ role, reason: "reclaimed" }]);
  });

  test("heartbeat ticks while this session is the live holder issue no claim and append no transcript entry", async () => {
    const role = "legion-controller";
    const roleClaims: unknown[] = [];
    let roleReads = 0;
    let registrations = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads += 1;
        return response({ role, holder: "ses_quiet", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_quiet",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") registrations += 1;
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-quiet");
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_quiet"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    const claimEntries = () =>
      fixture.entries.filter((entry) => entry.customType === "envoy-role-claim");
    expect(claimEntries()).toHaveLength(1);
    expect(registrations).toBe(1);

    for (let tick = 0; tick < 3; tick += 1) {
      intervals[0]?.();
      await new Promise((resolve) => setTimeout(resolve, 10));
    }

    expect(registrations).toBe(4);
    expect(roleReads).toBe(3);
    expect(roleClaims).toEqual([{ session_id: "ses_quiet", role }]);
    expect(claimEntries()).toHaveLength(1);
    expect(regained).toEqual([]);
  });

  test("a 409 on re-assertion drops the local claim, warns once, and ends re-assertion for that role, even once the newer holder leaves", async () => {
    const role = "release-captain";
    const roleClaims: Record<string, unknown>[] = [];
    let newerHolderLive = true;
    let registrations = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        if (!newerHolderLive) {
          return Response.json({ error: "no live holder" }, { status: 404 });
        }
        // A newer live session took the role (a fork child, a second controller).
        return response({ role, holder: "ses_newer", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        if (body.soft === true && newerHolderLive) {
          return Response.json(
            { error: `role ${role} is held by ses_newer`, role, holder: "ses_newer" },
            { status: 409 }
          );
        }
        return response({
          session_id: "ses_refused",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") registrations += 1;
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-409");
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const refused = Promise.withResolvers<void>();
    const base = sessionContext("ses_refused");
    const context: SessionContext = {
      ...base,
      // The host reads the session's transcript, which records the claim.
      sessionManager: { ...base.sessionManager, getBranch: () => fixture.entries },
      setInterval: (callback) => intervals.push(callback),
      ui: {
        ...sessionContext().ui,
        notify: (message) => {
          notifications.push(message);
          if (message.includes("is now held by")) refused.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    intervals[0]?.();
    await refused.promise;
    const settle = async () => {
      for (let step = 0; step < 20; step += 1) await new Promise<void>((r) => setImmediate(r));
    };
    await settle();
    // More ticks, the last two after the newer holder has gone: each re-registers, and none
    // re-asserts this role.
    for (let tick = 0; tick < 4; tick += 1) {
      if (tick === 2) newerHolderLive = false;
      const before = registrations;
      intervals[0]?.();
      await settle();
      expect(registrations).toBe(before + 1);
    }

    expect(roleClaims).toEqual([
      { session_id: "ses_refused", role },
      { session_id: "ses_refused", role, soft: true },
    ]);
    expect(notifications.filter((message) => message.includes("is now held by"))).toHaveLength(1);
    expect(regained).toEqual([]);
    // A 409 clears only this process's transient claim. The durable record
    // remains for a future session to arbitrate with the listener.
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
  });

  test("the first healthy heartbeat after a registry outage fires the regain hook even though the claim survived", async () => {
    // A listener restart that outlasts the envoy_sessions TTL: the durable claim is intact, but
    // until this session re-registers the listener answers "no holder" for it, and the daemon's
    // publishes in that window went undelivered. The re-registration itself is the regain.
    const role = "legion-controller";
    let registryDown = false;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      if (registryDown) throw new Error("network unreachable");
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        return response({ role, holder: "ses_outage", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_outage",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-outage-regain");
    const regained: { readonly role: string; readonly reason: string }[] = [];
    const hooked = Promise.withResolvers<void>();
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
      hooked.resolve();
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_outage"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        ...sessionContext().ui,
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    registryDown = true;
    intervals[0]?.();
    await warned.promise;
    // Let the failed tick's own `.finally` release the healing guard before the next tick.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(regained).toEqual([]);

    registryDown = false;
    intervals[0]?.();
    await hooked.promise;
    expect(regained).toEqual([{ role, reason: "reregistered" }]);
    // The claim survived: only the explicit hard claim ever went out.
    expect(roleClaims).toEqual([{ session_id: "ses_outage", role }]);

    // The following healthy tick is quiet again.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(regained).toHaveLength(1);
    expect(
      notifications.filter((message) => message.includes("registry heartbeat failed"))
    ).toHaveLength(1);
  });

  test("a regain hook that never settles does not block the next heartbeat tick's registration", async () => {
    // The hook is legion.ts's daemon round-trip (`claims/ready`, retried on a 5xx or a transport
    // failure), which this side cannot bound. The heartbeat's healing latch must release once
    // registration and the claim are settled, or a stuck hook would stop re-registration and
    // let the session's registry entry lapse.
    const role = "legion-controller";
    let listenerHoldsClaim = true;
    let registrations = 0;
    const thirdRegistration = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_hung_hook", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        listenerHoldsClaim = true;
        return response({
          session_id: "ses_hung_hook",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        registrations += 1;
        if (registrations === 3) thirdRegistration.resolve();
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-hung-hook");
    let hookCalls = 0;
    onEnvoyRoleRegained(async () => {
      hookCalls += 1;
      await Promise.withResolvers<void>().promise;
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_hung_hook"),
      setInterval: (callback) => intervals.push(callback),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    expect(registrations).toBe(1);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(hookCalls).toBe(1);
    expect(registrations).toBe(2);

    // The hook is still pending. The next tick must still register.
    intervals[0]?.();
    await thirdRegistration.promise;
    expect(registrations).toBe(3);
    expect(hookCalls).toBe(1);
    expect(notifications).toEqual([]);
  });

  test("a failed role read does not make the next healthy heartbeat report a regain", async () => {
    // Only a lapsed registration can leave the listener answering "no holder" for a claim that
    // survived; a role read that fails after a successful registration is a plain heartbeat
    // error and must not trigger a redundant daemon ready call on the following tick.
    const role = "legion-controller";
    let roleReadBroken = false;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        // A body the client cannot parse fails immediately, without the transport's 5xx retry.
        if (roleReadBroken) return response({});
        return response({ role, holder: "ses_role_read", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_role_read",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-role-read-failure");
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_role_read"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        ...sessionContext().ui,
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    roleReadBroken = true;
    intervals[0]?.();
    await warned.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    roleReadBroken = false;
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(regained).toEqual([]);
    expect(roleClaims).toEqual([{ session_id: "ses_role_read", role }]);
    expect(
      notifications.filter((message) => message.includes("registry heartbeat failed"))
    ).toHaveLength(1);
  });

  test("a failed registration's regain survives a role-read failure on the recovery tick", async () => {
    // Tick N: registration fails (the listener may now answer "no holder" for this session).
    // Tick N+1: registration lands but the role read errors. Tick N+2: healthy. The regain owed
    // since tick N must still fire exactly once at N+2, or the daemon's held work stays stranded.
    const role = "legion-controller";
    let registryDown = false;
    let roleReadBroken = false;
    let registrations = 0;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        if (registryDown) throw new Error("network unreachable");
        registrations += 1;
      }
      if (url.pathname === `/v1/roles/${role}`) {
        // A body the client cannot parse fails immediately, without the transport's 5xx retry.
        if (roleReadBroken) return response({});
        return response({ role, holder: "ses_late_regain", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_late_regain",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-late-regain");
    const regained: { readonly role: string; readonly reason: string }[] = [];
    const hooked = Promise.withResolvers<void>();
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
      hooked.resolve();
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_late_regain"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        ...sessionContext().ui,
        notify: (message) => {
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    expect(registrations).toBe(1);

    registryDown = true;
    intervals[0]?.();
    await warned.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    registryDown = false;
    roleReadBroken = true;
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(registrations).toBe(2);
    expect(regained).toEqual([]);

    roleReadBroken = false;
    intervals[0]?.();
    await hooked.promise;
    // The hook fires detached; let the tick's own chain release the healing latch.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(regained).toEqual([{ role, reason: "reregistered" }]);
    // The claim survived throughout: only the explicit hard claim ever went out.
    expect(roleClaims).toEqual([{ session_id: "ses_late_regain", role }]);

    // A further healthy tick adds nothing.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(registrations).toBe(4);
    expect(regained).toHaveLength(1);
  });

  test("registers a self-subscribed interest on session start", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    globalThis.fetch = async (input, init) => {
      requests.push({
        path: new URL(input.toString()).pathname,
        body: init?.body === undefined ? undefined : JSON.parse(init.body.toString()),
      });
      return response({ session_id: "ses_register", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?register-session");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_register"));

    expect(requests).toContainEqual({
      path: "/v1/interests/subscribe",
      body: {
        session_id: "ses_register",
        dir: "/tmp/envoy-omp-test",
        topics: ["notifications.agent.ses_register"],
        port: 0,
        title: "",
        driving: false,
        self_subscribed: true,
        capabilities: ["aside", "steer"],
        host: machineID(),
      },
    });
  });
  test("answers a targeted BTW delivery ephemerally and registers the capability", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const registrations: unknown[] = [];
    const replies: unknown[] = [];
    const replyPosted = Promise.withResolvers<void>();
    const calls: string[] = [];
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/interests/subscribe") {
        registrations.push(JSON.parse(init?.body?.toString() ?? "{}"));
      }
      if (path === "/api/v1/messages/11111111-1111-4111-8111-111111111111/reply") {
        calls.push(`fetch ${path}`);
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        replyPosted.resolve();
      }
      return response({
        session_id: "ses_target",
        machine_id: "test",
        dir: "/tmp",
        topics: ["notifications.agent.ses_target"],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-btw");
    const fixture = createPi();
    const asked: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.(
      {},
      {
        ...sessionContext("ses_target"),
        runEphemeralTurn: async ({ promptText }) => {
          calls.push("runEphemeralTurn");
          asked.push(promptText);
          return { replyText: "Yes, ship it." };
        },
      }
    );
    const agent = natsState.controls.get("notifications.agent.ses_target");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    natsState.onPublish = (subject) => calls.push(`publish ${subject}`);
    agent.push(
      JSON.stringify({
        event_id: "dispatch-btw",
        source: "dispatch",
        source_event_id: "1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "dispatch-btw",
        issued_at: 1,
        payload_summary: "Can this ship?",
        payload: targetedDispatchPayload,
        trace_id: "dispatch-btw",
      }),
      "_INBOX.btw"
    );
    await replyPosted.promise;

    // The host sends the prompt as given, so the question goes out in the /btw wrapper.
    expect(asked).toEqual([expect.stringMatching(/^<btw>\n[\s\S]*\nCan this ship\?\n<\/btw>$/)]);
    expect(fixture.deliveries).toEqual([]);
    expect(replies).toEqual([
      {
        actor: { kind: "session", id: "ses_target" },
        attempt: 1,
        body: "Yes, ship it.",
      },
    ]);
    expect(registrations).toMatchObject([{ capabilities: ["aside", "btw", "steer"] }]);
    // A targeted Dispatch frame is a JetStream publish to the direct subject: its
    // reply inbox belongs to the server's PubAck, so no receipt is published —
    // the frame goes straight to the ephemeral question and the Dispatch reply.
    expect(calls).toEqual([
      "runEphemeralTurn",
      "fetch /api/v1/messages/11111111-1111-4111-8111-111111111111/reply",
    ]);
  });

  test("answers a targeted comment BTW through its supplied comment reply address", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: Array<{ readonly path: string; readonly body: unknown }> = [];
    const replyPosted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/interests/subscribe") {
        return response({
          session_id: "ses_delivery",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_delivery"],
        });
      }
      if (path === "/api/v1/comments/33333333-3333-4333-8333-333333333333/reply") {
        replies.push({
          path,
          body: JSON.parse(init?.body?.toString() ?? "{}"),
        });
        replyPosted.resolve();
      }
      return response({});
    };
    // Query-string isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?targeted-comment-btw");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.(
      {},
      {
        ...sessionContext("ses_delivery"),
        runEphemeralTurn: async () => ({ replyText: "Comment reply." }),
      }
    );
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedCommentEnvelope("btw", "comment-btw"));
    await replyPosted.promise;

    expect(fixture.deliveries).toEqual([]);
    expect(replies).toEqual([
      {
        path: "/api/v1/comments/33333333-3333-4333-8333-333333333333/reply",
        body: {
          actor: { id: "ses_delivery", kind: "session" },
          attempt: 1,
          target: "session:ses_delivery",
          body: "Comment reply.",
        },
      },
    ]);
  });

  test("delivers targeted aside and steer frames through their requested primary-turn modes", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?targeted-modes");
    const fixture = createPi();
    const delivered = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (fixture.deliveries.length === 2) delivered.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedDispatchEnvelope("aside", "targeted-aside"));
    agent.push(targetedDispatchEnvelope("steer", "targeted-steer"));
    await delivered.promise;

    expect(fixture.deliveries.map((delivery) => delivery.options)).toEqual([
      { deliverAs: "aside", triggerTurn: true },
      { deliverAs: "steer", triggerTurn: true },
    ]);
  });

  // A person's Send or Aside from the Agents page is the user's own turn, as if typed at the
  // terminal. The frame saying "a person wrote this" is not the proof - the listener takes its
  // source from the caller - so the session takes it only once Dispatch records the one
  // acceptance of that attempt, and injects what Dispatch's answer says was stored.
  describe("a person's direct message", () => {
    test("is accepted with Dispatch and arrives as the user's own turn, recorded first, with nothing posted", async () => {
      const { accepts, agent, delivered, fixture, posts, recordedWhenDelivered } =
        await bootDirectSession("direct-send");

      agent.push(directDispatchEnvelope("steer", "direct-send"));
      await delivered(1);

      expect(accepts).toEqual([
        {
          authorization: "Bearer dispatch-token",
          body: { actor: { id: "ses_delivery", kind: "session" } },
          path: `/api/v1/messages/${DIRECT_MESSAGE_ID}/deliveries/1/accept`,
        },
      ]);
      expect(fixture.userMessages).toEqual([{ content: DIRECT_MESSAGE_BODY, options: undefined }]);
      expect(fixture.deliveries).toEqual([]);
      expect(posts).toEqual([]);
      // The attempt is in the transcript before the turn goes out, so no crash in between leaves
      // a delivered attempt a forged frame could still have accepted.
      expect(fixture.entries).toEqual([
        {
          customType: "envoy-dispatch-handled-attempt",
          data: { attempt: 1, message_id: DIRECT_MESSAGE_ID },
          type: "custom",
        },
      ]);
      expect(recordedWhenDelivered).toEqual([1]);
      // Legion's phase-stall check reads this: the user message it starts is the person's, never
      // the daemon's assignment.
      const turn = {
        content: [{ text: DIRECT_MESSAGE_BODY, type: "text" }],
        role: "user",
        timestamp: 10,
      };
      expect(matchInjectedUserTurn("ses_delivery", turn)).toBe(DIRECT_MESSAGE_ID);
    });

    // Send is Enter at the terminal (no deliverAs); Aside lands at the next step. The frame's
    // mode and text are what any publisher wrote, so Dispatch's answer decides both.
    for (const [index, [frameMode, storedMode, options]] of (
      [
        ["steer", "steer", undefined],
        ["aside", "aside", { deliverAs: "aside" }],
        ["steer", "aside", { deliverAs: "aside" }],
        ["aside", "steer", undefined],
      ] as const
    ).entries()) {
      test(`a frame saying ${frameMode} whose stored attempt is ${storedMode} is delivered as ${storedMode}`, async () => {
        const { agent, delivered, fixture } = await bootDirectSession(`direct-mode-${index}`, {
          accept: () => ({ accepted: true, delivery: storedMode }),
        });

        agent.push(
          directDispatchEnvelope(frameMode, `direct-mode-${index}`, { body: "a forger's text" })
        );
        await delivered(1);

        expect(fixture.userMessages).toEqual([{ content: DIRECT_MESSAGE_BODY, options }]);
      });
    }

    // Only Dispatch's 200 makes a turn: whatever it refuses, and whatever keeps it from answering,
    // keeps the card, and the session posts nothing and injects nothing.
    for (const [name, answer] of [
      ["another frame already took it", { code: "ACCEPT_ALREADY_ACCEPTED", status: 409 }],
      ["a later attempt superseded it", { code: "ACCEPT_SUPERSEDED", status: 409 }],
      ["it names another session's attempt", { code: "ACCEPT_FORBIDDEN", status: 403 }],
      ["it is no direct message to this session", { code: "ACCEPT_NOT_DIRECT", status: 409 }],
      ["a session wrote it", { code: "ACCEPT_NOT_WRITTEN_BY_PERSON", status: 409 }],
      ["its attempt is a BTW", { code: "ACCEPT_NOT_ASIDE_OR_STEER", status: 409 }],
      ["Dispatch refuses the session's token", { code: "UNAUTHORIZED", status: 401 }],
      ["Dispatch predates the accept route", { status: 404 }],
      ["Dispatch fails", { status: 500 }],
      ["the accept never reaches Dispatch", { reject: "fetch failed" }],
      // A 200 that carries no stored body has nothing to inject.
      ["Dispatch accepts it without the stored body", { accepted: true, body: null }],
    ] as const satisfies readonly (readonly [string, AcceptAnswer])[]) {
      test(`stays a card when ${name}`, async () => {
        const { accepts, agent, delivered, fixture, posts, recordedWhenDelivered } =
          await bootDirectSession(`direct-accept-refused-${name}`, { accept: () => answer });

        agent.push(directDispatchEnvelope("steer", `direct-accept-refused-${name}`));
        await delivered(1);

        expect(accepts).toHaveLength(1);
        expect(fixture.userMessages).toEqual([]);
        expect(fixture.deliveries.map((delivery) => delivery.customType)).toEqual([
          "envoy-message",
        ]);
        expect(posts).toEqual([]);
        // The card is a delivery too: the attempt is recorded before it goes out.
        expect(recordedWhenDelivered).toEqual([1]);
      });
    }

    // The ways a session could otherwise make a person's message a fresh user turn (Deep's
    // reviews of #1592): Dispatch refuses the first three, and the session itself the rest.
    test("a person's earlier message a session re-sent through the retry route stays a card", async () => {
      const { accepts, agent, delivered, fixture } = await bootDirectSession(
        "direct-bearer-retry",
        {
          accept: () => ({ code: "ACCEPT_NOT_REQUESTED_BY_PERSON", status: 409 }),
        }
      );

      agent.push(directDispatchEnvelope("steer", "direct-bearer-retry", { attempt: 2 }));
      await delivered(1);

      expect(accepts.map((accept) => accept.path)).toEqual([
        `/api/v1/messages/${DIRECT_MESSAGE_ID}/deliveries/2/accept`,
      ]);
      expect(fixture.userMessages).toEqual([]);
      expect(fixture.deliveries).toHaveLength(1);
    });

    test("a frame forged after a restart naming a person's Send of long ago stays a card", async () => {
      const { accepts, agent, delivered, fixture } = await bootDirectSession(
        "direct-forged-after-restart",
        { accept: () => ({ code: "ACCEPT_STALE", status: 409 }) }
      );

      agent.push(
        directDispatchEnvelope("steer", "direct-forged-after-restart", { body: "forger text" })
      );
      await delivered(1);

      expect(accepts).toHaveLength(1);
      expect(fixture.userMessages).toEqual([]);
      expect(fixture.deliveries).toHaveLength(1);
    });

    test("a frame forged for a Send Dispatch recorded as failed stays a card", async () => {
      // The session was down when the person sent it, so no frame of it ever reached the session.
      const { accepts, agent, delivered, fixture } = await bootDirectSession(
        "direct-forged-failed",
        {
          accept: () => ({ code: "ACCEPT_FAILED", status: 409 }),
        }
      );

      agent.push(directDispatchEnvelope("steer", "direct-forged-failed", { body: "forger text" }));
      await delivered(1);

      expect(accepts).toHaveLength(1);
      expect(fixture.userMessages).toEqual([]);
      expect(fixture.deliveries).toHaveLength(1);
    });

    test("a Send that arrived as a card when Dispatch refused the token stays a card when a frame forged for it arrives once the token works", async () => {
      // The acceptance run's sequence at add7ac87: the accept is refused on the session's token, so
      // the Send arrives as a card; the token is restored, and a frame forged with the listener
      // token names that attempt inside its minute, which Dispatch would accept.
      const { accepts, agent, delivered, fixture } = await bootDirectSession(
        "direct-carded-forged",
        {
          accept: (count) =>
            count === 1 ? { code: "UNAUTHORIZED", status: 401 } : { accepted: true },
        }
      );

      agent.push(directDispatchEnvelope("steer", "direct-carded"));
      await delivered(1);
      agent.push(directDispatchEnvelope("steer", "direct-carded-forged", { body: "forger text" }));
      await delivered(2);

      expect(accepts).toHaveLength(1);
      expect(fixture.userMessages).toEqual([]);
      expect(fixture.deliveries.map((delivery) => delivery.customType)).toEqual([
        "envoy-message",
        "envoy-message",
      ]);
    });

    test("a person's retry of a Send that arrived as a card is a user turn", async () => {
      const { accepts, agent, delivered, fixture } = await bootDirectSession(
        "direct-carded-retry",
        {
          accept: (count) =>
            count === 1 ? { reject: "fetch failed" } : { accepted: true, delivery: "aside" },
        }
      );

      agent.push(directDispatchEnvelope("steer", "direct-carded-retry-1"));
      await delivered(1);
      // The person's retry is an attempt of its own, which the session has not delivered.
      agent.push(directDispatchEnvelope("aside", "direct-carded-retry-2", { attempt: 2 }));
      await delivered(2);

      expect(accepts.map((accept) => accept.path)).toEqual([
        `/api/v1/messages/${DIRECT_MESSAGE_ID}/deliveries/1/accept`,
        `/api/v1/messages/${DIRECT_MESSAGE_ID}/deliveries/2/accept`,
      ]);
      expect(fixture.deliveries).toHaveLength(1);
      expect(fixture.userMessages).toEqual([
        { content: DIRECT_MESSAGE_BODY, options: { deliverAs: "aside" } },
      ]);
    });

    test("is one user turn however often it is delivered, and no replay is accepted again", async () => {
      const { accepts, agent, delivered, fixture } = await bootDirectSession("direct-replay", {
        // Dispatch would take any of them: only the session keeps the replays cards.
        accept: () => ({ accepted: true }),
      });

      agent.push(directDispatchEnvelope("steer", "direct-replay"));
      await delivered(1);
      // The same attempt again under another envelope: the dedupe key does not catch it.
      agent.push(directDispatchEnvelope("steer", "direct-replay-again"));
      await delivered(2);

      expect(accepts).toHaveLength(1);
      expect(fixture.userMessages).toHaveLength(1);
      expect(fixture.deliveries.map((delivery) => delivery.customType)).toEqual(["envoy-message"]);
    });

    // What a restarted session knows is its file's entries, every branch of them: a tree switch
    // before the restart leaves the delivered attempt off the active branch, not out of the file.
    for (const [name, onActiveBranch] of [
      ["on its active branch", true],
      ["on another branch of its tree", false],
    ] as const) {
      test(`a frame naming an attempt a restarted session delivered, ${name}, is a card with no accept`, async () => {
        const first = await bootDirectSession(`direct-restart-first-${onActiveBranch}`, {
          accept: () => ({ code: "UNAUTHORIZED", status: 401 }),
        });
        first.agent.push(directDispatchEnvelope("steer", "direct-restart"));
        await first.delivered(1);
        expect(first.fixture.deliveries).toHaveLength(1);

        const restarted = await bootDirectSession(`direct-restart-second-${onActiveBranch}`, {
          accept: () => ({ accepted: true }),
          branch: onActiveBranch ? first.fixture.entries : [],
          entries: first.fixture.entries,
        });
        restarted.agent.push(
          directDispatchEnvelope("steer", "direct-restart-forged", { body: "forger text" })
        );
        await restarted.delivered(1);

        expect(restarted.accepts).toEqual([]);
        expect(restarted.fixture.userMessages).toEqual([]);
        expect(restarted.fixture.deliveries.map((delivery) => delivery.customType)).toEqual([
          "envoy-message",
        ]);
      });
    }

    // Every other frame keeps its envelope without costing a Dispatch call: an issue message and a
    // comment mention reply on the issue, a session's message is no person's, and a frame naming a
    // broadcast yields a card whether or not the claim is true.
    for (const [name, envelope] of [
      ["an issue message", targetedDispatchEnvelope("steer", "scope-issue")],
      ["a comment mention", targetedCommentEnvelope("aside", "scope-comment")],
      [
        "a direct message a session sent",
        directDispatchEnvelope("steer", "scope-session", {
          actor: { id: "ses_peer", kind: "session" },
        }),
      ],
      [
        "a frame naming a broadcast",
        directDispatchEnvelope("steer", "scope-broadcast", {
          broadcastID: "55555555-5555-4555-8555-555555555555",
        }),
      ],
    ] as const) {
      test(`${name} keeps its card and is never accepted`, async () => {
        const { accepts, agent, delivered, fixture } = await bootDirectSession(
          `direct-scope-${name}`
        );

        agent.push(envelope);
        await delivered(1);

        expect(accepts).toEqual([]);
        expect(fixture.userMessages).toEqual([]);
        expect(fixture.deliveries.map((delivery) => delivery.customType)).toEqual([
          "envoy-message",
        ]);
      });
    }

    // A Dispatch configuration that no longer resolves cannot accept anything, and must not lose
    // the message either: it arrives as the card it was before any of this.
    test("stays a card, accepted by nobody, when the Dispatch token file cannot be read", async () => {
      const { accepts, agent, delivered, fixture } =
        await bootDirectSession("direct-broken-config");
      process.env.DISPATCH_TOKEN_FILE = join(tmpdir(), "legion-394-no-such-token-file");

      agent.push(directDispatchEnvelope("steer", "direct-broken-config"));
      await delivered(1);

      expect(accepts).toEqual([]);
      expect(fixture.userMessages).toEqual([]);
      expect(fixture.deliveries.map((delivery) => delivery.customType)).toEqual(["envoy-message"]);
    });

    // The viewer shows Dispatch's stored copy until the turn's own frame names it. The run's end
    // clears the record under the id the turn was noted under, even while the host's own id still
    // differs from it, as a fresh terminal's does until the registration heartbeat heals it.
    for (const [name, runEndsFirst, liveID, tag] of [
      ["tags the user turn it became on the live stream", false, "ses_delivery", DIRECT_MESSAGE_ID],
      ["tags nothing once the run it was sent into has ended", true, "ses_delivery", undefined],
      [
        "tags nothing once the run it was sent into has ended under a host id not yet healed",
        true,
        "ses_host_drift",
        undefined,
      ],
    ] as const) {
      test(name, async () => {
        const { agent, context, delivered, fixture } = await bootDirectSession(
          `direct-stream-${runEndsFirst}-${liveID}`
        );
        natsState.controls
          .get("agentstream.ses_delivery.control")
          ?.push(JSON.stringify({ type: "watch", v: 1 }));

        agent.push(directDispatchEnvelope("steer", `direct-stream-${runEndsFirst}-${liveID}`));
        await delivered(1);
        if (runEndsFirst) {
          await fixture.handlers.get("agent_end")?.(
            { messages: [] },
            {
              ...context,
              sessionManager: { ...context.sessionManager, getSessionId: () => liveID },
            }
          );
        }
        const message = {
          content: [{ text: DIRECT_MESSAGE_BODY, type: "text" }],
          role: "user",
          timestamp: 10,
        };
        await fixture.handlers.get("message_start")?.({ message }, context);
        await fixture.handlers.get("message_end")?.({ message }, context);

        const frames = natsState.published
          .filter((published) => published.subject === "agentstream.ses_delivery.frames")
          .map((published) => JSON.parse(new TextDecoder().decode(published.data)));
        expect(frames.map((frame) => [frame.message.id, frame.message.dispatchMessageId])).toEqual([
          ["u10", tag],
          ["u10", tag],
        ]);
      });
    }

    // On a host with `pi.sendUserInput` (our fork, can1357/oh-my-pi#14323) a person's accepted
    // Send or Aside runs as typed at the terminal: a command, a skill, a template or a prompt.
    // Whatever a command printed, or why it did not run, is the session's reply on Dispatch, which
    // the conversation shows; a turn the model now works on is its own answer.
    describe("on a host that runs typed input", () => {
      for (const [name, frameMode, acceptedMode, inputOptions] of [
        ["a Send accepted as a Send", "steer", "steer", { tag: DIRECT_MESSAGE_ID }],
        [
          "an Aside accepted as an Aside",
          "aside",
          "aside",
          { deliverAs: "aside", tag: DIRECT_MESSAGE_ID },
        ],
        [
          "a frame claiming Send accepted as Aside",
          "steer",
          "aside",
          { deliverAs: "aside", tag: DIRECT_MESSAGE_ID },
        ],
        ["a frame claiming Aside accepted as Send", "aside", "steer", { tag: DIRECT_MESSAGE_ID }],
      ] as const) {
        test(`runs the stored body as typed in ${name}`, async () => {
          const { agent, delivered, fixture, inputs } = await bootDirectSession(
            `typed-input-mode-${name}`,
            {
              accept: () => ({ accepted: true, delivery: acceptedMode }),
              sendUserInput: async () => ({ handled: "prompt" }),
            }
          );

          agent.push(
            directDispatchEnvelope(frameMode, `typed-input-mode-${name}`, {
              body: "a forger's text",
            })
          );
          await delivered(1);

          expect(inputs).toEqual([{ options: inputOptions, text: DIRECT_MESSAGE_BODY }]);
          expect(fixture.userMessages).toEqual([]);
        });
      }

      for (const [name, result, reply] of [
        [
          "a built-in only the terminal runs",
          { handled: "terminal-only" },
          "/new runs only in the session's own terminal; nothing was sent.",
        ],
        [
          "a host mode that cannot run it",
          { handled: "unavailable" },
          "/new was not run: this session's host cannot run typed input from Dispatch; nothing was sent.",
        ],
        [
          "a built-in that printed its output",
          { handled: "command", output: "3 jobs running" },
          "3 jobs running",
        ],
        // `/force:<tool> <prompt>` prints its confirmation, then submits the prompt.
        [
          "a built-in that printed its output and then submitted a prompt",
          { handled: "prompt", output: "Next turn forced to use read." },
          "Next turn forced to use read.",
        ],
      ] as const satisfies readonly (readonly [string, UserInputResult, string])[]) {
        test(`answers ${name} with the session's reply`, async () => {
          const { agent, delivered, replied, replies } = await bootDirectSession(
            `typed-input-reply-${name}`,
            {
              accept: () => ({ accepted: true, body: "/new" }),
              sendUserInput: async () => result,
            }
          );

          agent.push(directDispatchEnvelope("steer", `typed-input-reply-${name}`));
          await delivered(1);
          await replied(1);

          expect(replies).toEqual([
            {
              body: { actor: { id: "ses_delivery", kind: "session" }, attempt: 1, body: reply },
              path: `/api/v1/messages/${DIRECT_MESSAGE_ID}/reply`,
            },
          ]);
        });
      }

      for (const [name, firstResult] of [
        ["a prompt", { handled: "prompt" }],
        ["a skill", { handled: "skill" }],
        ["a command with no output", { handled: "command" }],
        ["a command with whitespace output", { handled: "command", output: "  \n" }],
        ["a prompt with whitespace output", { handled: "prompt", output: "  \n" }],
      ] as const satisfies readonly (readonly [string, UserInputResult])[]) {
        test(`posts no reply for ${name}`, async () => {
          let inputs = 0;
          const { agent, delivered, replied, replies } = await bootDirectSession(
            `typed-input-no-reply-${name}`,
            {
              accept: (count) => ({
                accepted: true,
                body: count === 1 ? DIRECT_MESSAGE_BODY : "/new",
              }),
              sendUserInput: async () => {
                inputs++;
                return inputs === 1 ? firstResult : { handled: "terminal-only" };
              },
            }
          );

          agent.push(directDispatchEnvelope("steer", `typed-input-no-reply-${name}-1`));
          await delivered(1);
          agent.push(
            directDispatchEnvelope("steer", `typed-input-no-reply-${name}-2`, { attempt: 2 })
          );
          await delivered(2);
          await replied(1);

          expect(replies).toEqual([
            {
              body: {
                actor: { id: "ses_delivery", kind: "session" },
                attempt: 2,
                body: "/new runs only in the session's own terminal; nothing was sent.",
              },
              path: `/api/v1/messages/${DIRECT_MESSAGE_ID}/reply`,
            },
          ]);
        });
      }

      for (const [name, first] of [
        ["a command", async () => ({ handled: "command" as const, output: "done" })],
        [
          "a command that invoked the agent",
          async () => ({ agentInvoked: true, handled: "command" as const, output: "done" }),
        ],
        ["a terminal-only command", async () => ({ handled: "terminal-only" as const })],
        ["an unavailable host", async () => ({ handled: "unavailable" as const })],
        [
          "a failed host call",
          async () => {
            throw new Error("failed before submission");
          },
        ],
      ] as const) {
        test(`drops the typed note after ${name} submits no message`, async () => {
          const { agent, delivered, replied } = await bootDirectSession(`typed-note-drop-${name}`, {
            sendUserInput: first,
          });

          agent.push(directDispatchEnvelope("steer", `typed-note-drop-${name}`));
          await delivered(1);
          await replied(1);

          expect(
            envoyPluginInterface()
              .injectedUserTurns.get("ses_delivery")
              ?.sent.map((sent) => sent.messageId) ?? []
          ).not.toContain(DIRECT_MESSAGE_ID);
        });
      }

      for (const handled of ["prompt", "skill"] as const) {
        test(`keeps the typed note for ${handled} until its tagged message arrives`, async () => {
          const { agent, context, delivered, fixture } = await bootDirectSession(
            `typed-note-kept-${handled}`,
            {
              sendUserInput: async () => ({ handled }),
            }
          );

          agent.push(directDispatchEnvelope("steer", `typed-note-kept-${handled}`));
          await delivered(1);
          await Promise.resolve();
          expect(
            envoyPluginInterface()
              .injectedUserTurns.get("ses_delivery")
              ?.sent.map((sent) => sent.messageId)
          ).toContain(DIRECT_MESSAGE_ID);

          await fixture.handlers.get("message_start")?.(
            {
              message: {
                content: [{ text: DIRECT_MESSAGE_BODY, type: "text" }],
                role: handled === "skill" ? "custom" : "user",
                tag: DIRECT_MESSAGE_ID,
                timestamp: 10,
              },
            },
            context
          );
          expect(
            envoyPluginInterface()
              .injectedUserTurns.get("ses_delivery")
              ?.sent.map((sent) => sent.messageId) ?? []
          ).not.toContain(DIRECT_MESSAGE_ID);
        });
      }

      test("a host that fails running it answers with the failure", async () => {
        const { agent, delivered, replied, replies } = await bootDirectSession(
          "typed-input-fails",
          {
            accept: () => ({ accepted: true, body: "/compact" }),
            sendUserInput: async () => {
              throw new Error("compaction is already running");
            },
          }
        );

        agent.push(directDispatchEnvelope("steer", "typed-input-fails"));
        await delivered(1);
        await replied(1);

        expect(replies.map((reply) => reply.body)).toEqual([
          {
            actor: { id: "ses_delivery", kind: "session" },
            attempt: 1,
            body: "/compact was not run: compaction is already running",
          },
        ]);
      });

      test("answers a synchronous typed-input throw and drops its note", async () => {
        const { agent, delivered, replied, replies } = await bootDirectSession(
          "typed-input-sync-fails",
          {
            accept: () => ({ accepted: true, body: "/compact" }),
            sendUserInput: (() => {
              throw new Error("sync failure");
            }) as unknown as (text: string, options: unknown) => Promise<UserInputResult>,
          }
        );

        agent.push(directDispatchEnvelope("steer", "typed-input-sync-fails"));
        await delivered(1);
        await replied(1);

        expect(replies[0]?.body).toEqual({
          actor: { id: "ses_delivery", kind: "session" },
          attempt: 1,
          body: "/compact was not run: sync failure",
        });
        expect(
          envoyPluginInterface()
            .injectedUserTurns.get("ses_delivery")
            ?.sent.map((sent) => sent.messageId) ?? []
        ).not.toContain(DIRECT_MESSAGE_ID);
      });

      test("posts a typed command's reply under the session that accepted it after a switch", async () => {
        const answer = Promise.withResolvers<UserInputResult>();
        const { agent, delivered, fixture, replied, replies } = await bootDirectSession(
          "typed-input-reply-session",
          {
            accept: () => ({ accepted: true, body: "/new" }),
            sendUserInput: () => answer.promise,
          }
        );

        agent.push(directDispatchEnvelope("steer", "typed-input-reply-session"));
        await delivered(1);
        await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_reply"));
        answer.resolve({ handled: "terminal-only" });
        await replied(1);

        expect(replies[0]?.body).toEqual({
          actor: { id: "ses_delivery", kind: "session" },
          attempt: 1,
          body: "/new runs only in the session's own terminal; nothing was sent.",
        });
      });

      test("posts a typed picture command's reply under the session that accepted it after a switch during the picture read", async () => {
        const slug = "typed-input-picture-reply-session";
        const body = `Look:\n\n![shot.png](dispatch://LEGION-1/artifact/${slug}@v1)`;
        const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 1, 2]);
        const picture = Promise.withResolvers<Response>();
        const requested = Promise.withResolvers<void>();
        const { agent, delivered, fixture, replied, replies } = await bootDirectSession(
          "typed-input-picture-reply-session",
          {
            accept: () => ({ accepted: true, body }),
            dispatch: ((url: URL) => {
              if (url.pathname === `/api/v1/issues/LEGION-1/artifacts/${slug}`) {
                return Response.json({
                  id: `${slug}-id`,
                  kind: "image",
                  name: "shot.png",
                  versions: [{ mime: "image/png", number: 1, size: png.length }],
                });
              }
              if (url.pathname === `/api/v1/artifacts/${slug}-id/versions/1`) {
                requested.resolve();
                return picture.promise as unknown as Response;
              }
              return undefined;
            }) as (url: URL) => Response | undefined,
            sendUserInput: async () => ({ handled: "terminal-only" }),
          }
        );

        agent.push(directDispatchEnvelope("steer", "typed-input-picture-reply-session", { body }));
        await requested.promise;
        await fixture.handlers.get("session_switch")?.(
          {},
          sessionContext("ses_after_picture_read")
        );
        picture.resolve(new Response(png, { headers: { "Content-Type": "image/png" } }));
        await delivered(1);
        await replied(1);

        expect(replies[0]?.body).toEqual({
          actor: { id: "ses_delivery", kind: "session" },
          attempt: 1,
          body: "This message runs only in the session's own terminal; nothing was sent.",
        });
      });

      test("keeps accept and reply under the session that began a deferred accept after a switch", async () => {
        const accepted = Promise.withResolvers<AcceptAnswer>();
        const acceptRequested = Promise.withResolvers<void>();
        const { accepts, agent, delivered, fixture, replied, replies } = await bootDirectSession(
          "typed-input-accept-reply-session",
          {
            accept: () => {
              acceptRequested.resolve();
              return accepted.promise;
            },
            sendUserInput: async () => ({ handled: "terminal-only" }),
          }
        );

        agent.push(directDispatchEnvelope("steer", "typed-input-accept-reply-session"));
        await acceptRequested.promise;
        await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_accept"));
        accepted.resolve({ accepted: true, body: "/new" });
        await delivered(1);
        await replied(1);

        expect(accepts).toEqual([
          expect.objectContaining({
            body: expect.objectContaining({
              actor: { id: "ses_delivery", kind: "session" },
            }),
          }),
        ]);
        expect(replies[0]?.body).toEqual({
          actor: { id: "ses_delivery", kind: "session" },
          attempt: 1,
          body: "/new runs only in the session's own terminal; nothing was sent.",
        });
      });

      // On an idle session the host answers only once the run it started ends, which can be
      // minutes; the person's next message must still be taken meanwhile.
      test("is never awaited, so the next message arrives while the first still runs", async () => {
        const running = Promise.withResolvers<UserInputResult>();
        const { agent, delivered, inputs } = await bootDirectSession("typed-input-not-awaited", {
          accept: () => ({ accepted: true }),
          sendUserInput: () => running.promise,
        });

        agent.push(directDispatchEnvelope("steer", "typed-input-not-awaited-1"));
        agent.push(directDispatchEnvelope("steer", "typed-input-not-awaited-2", { attempt: 2 }));
        await delivered(2);

        expect(inputs).toHaveLength(2);
        running.resolve({ handled: "prompt" });
      });

      // `sendUserInput` takes text alone; the person's pictures reach the host through its input
      // handlers, as a picture pasted at the terminal does.
      test("hands the host the pictures the message embeds when it asks for its input's images", async () => {
        const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2]);
        const slug = "typed-input-png";
        const body = `Look:\n\n![shot.png](dispatch://LEGION-1/artifact/${slug}@v1)`;
        const images: unknown[] = [];
        const asked = Promise.withResolvers<void>();
        const handlers: { input?: (event: unknown, context: SessionContext) => Promise<unknown> } =
          {};
        const { agent, context, delivered, fixture } = await bootDirectSession(slug, {
          accept: () => ({ accepted: true, body }),
          dispatch: (url) => {
            if (url.pathname === `/api/v1/issues/LEGION-1/artifacts/${slug}`) {
              return Response.json({
                id: `${slug}-id`,
                kind: "image",
                name: "shot.png",
                versions: [{ mime: "image/png", number: 1, size: png.length }],
              });
            }
            if (url.pathname === `/api/v1/artifacts/${slug}-id/versions/1`) {
              return new Response(png, { headers: { "Content-Type": "image/png" } });
            }
            return undefined;
          },
          sendUserInput: async (text) => {
            const answer = (await handlers.input?.({ source: "extension", text }, context)) as
              | { readonly images?: readonly unknown[] }
              | undefined;
            images.push(...(answer?.images ?? []));
            asked.resolve();
            return { handled: "prompt" };
          },
        });
        handlers.input = fixture.handlers.get("input");

        agent.push(directDispatchEnvelope("steer", slug, { body }));
        await delivered(1);
        await asked.promise;

        expect(images).toEqual([
          { data: Buffer.from(png).toString("base64"), mimeType: "image/png", type: "image" },
        ]);
      });

      // A typed note is found only by the tag the host kept. The matching text on an unrelated
      // untagged user message is the daemon's own assignment, not the person's command.
      test("does not take an untagged message whose text matches a typed turn", async () => {
        const { agent, context, delivered, fixture, replied } = await bootDirectSession(
          "typed-input-tag-only",
          {
            accept: () => ({ accepted: true, body: "/compact" }),
            sendUserInput: async () => ({ handled: "command", output: "Compaction complete." }),
          }
        );
        natsState.controls
          .get("agentstream.ses_delivery.control")
          ?.push(JSON.stringify({ type: "watch", v: 1 }));

        agent.push(directDispatchEnvelope("steer", "typed-input-tag-only"));
        await delivered(1);
        await replied(1);
        const message = {
          content: [{ text: "/compact", type: "text" }],
          role: "user",
          timestamp: 10,
        };
        await fixture.handlers.get("message_start")?.({ message }, context);

        const frames = natsState.published
          .filter((published) => published.subject === "agentstream.ses_delivery.frames")
          .map((published) => JSON.parse(new TextDecoder().decode(published.data)))
          .filter((frame) => frame.kind === "message");
        expect(frames.map((frame) => [frame.message.id, frame.message.dispatchMessageId])).toEqual([
          ["u10", undefined],
        ]);
        expect(matchInjectedUserTurn("ses_delivery", message)).toBeUndefined();
      });

      // A Send that lands after the run's last steer poll is run by the host as a turn of its own,
      // after that run's `agent_end`. Its streamed user message must still name the Dispatch
      // message, or the page shows the person's Send twice; a prompt template's expansion is a text
      // nothing but the tag can tie back to it.
      test("tags the user message a Send became even when it starts after the run's end", async () => {
        const { agent, context, delivered, fixture } = await bootDirectSession(
          "typed-input-after-run-end",
          {
            accept: () => ({ accepted: true, body: "/sdd plan" }),
            sendUserInput: async () => ({ handled: "prompt" }),
          }
        );
        natsState.controls
          .get("agentstream.ses_delivery.control")
          ?.push(JSON.stringify({ type: "watch", v: 1 }));

        agent.push(directDispatchEnvelope("steer", "typed-input-after-run-end"));
        await delivered(1);
        await fixture.handlers.get("agent_end")?.({ messages: [] }, context);
        const message = {
          content: [{ text: "Run the plan for: plan", type: "text" }],
          role: "user",
          tag: DIRECT_MESSAGE_ID,
          timestamp: 10,
        };
        await fixture.handlers.get("message_start")?.({ message }, context);

        const frames = natsState.published
          .filter((published) => published.subject === "agentstream.ses_delivery.frames")
          .map((published) => JSON.parse(new TextDecoder().decode(published.data)))
          .filter((frame) => frame.kind === "message");
        expect(frames.map((frame) => [frame.message.id, frame.message.dispatchMessageId])).toEqual([
          [`u10:${DIRECT_MESSAGE_ID}`, DIRECT_MESSAGE_ID],
        ]);
      });
    });

    // A host without `pi.sendUserInput` would hand a command to the model as words, so the
    // session refuses it with a reply naming its own version; any other text is sent as before.
    test("refuses a slash command on a host that cannot run typed input, naming the host", async () => {
      const { agent, fixture, replied, replies } = await bootDirectSession("untyped-host-command", {
        accept: () => ({ accepted: true, body: "/compact" }),
      });

      agent.push(directDispatchEnvelope("steer", "untyped-host-command"));
      await replied(1);

      expect(fixture.userMessages).toEqual([]);
      expect(replies.map((reply) => reply.body)).toEqual([
        {
          actor: { id: "ses_delivery", kind: "session" },
          attempt: 1,
          body: expect.stringMatching(
            /^\/compact was not run: this session's Oh My Pi \S+ cannot run commands sent from Dispatch; nothing was sent\.$/
          ),
        },
      ]);
    });

    // `sendUserMessage` sends into whichever session is live once the pictures load, so the turn's
    // note and its shown picture belong to that session: the session the message lands in must tie
    // it to its Dispatch message, or Legion takes it for the daemon's assignment.
    test("notes a picture Send under the session it is sent into after a switch during the picture read", async () => {
      const slug = "untyped-host-picture-switch";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2]);
      const picture = Promise.withResolvers<Response>();
      const requested = Promise.withResolvers<void>();
      const { agent, delivered, fixture } = await bootDirectSession(slug, {
        accept: () => ({ accepted: true, body }),
        dispatch: ((url: URL) => {
          if (url.pathname === `/api/v1/issues/LEGION-1/artifacts/${slug}`) {
            return Response.json({
              id: `${slug}-id`,
              kind: "image",
              name: "shot.png",
              versions: [{ mime: "image/png", number: 1, size: png.length }],
            });
          }
          if (url.pathname === `/api/v1/artifacts/${slug}-id/versions/1`) {
            requested.resolve();
            return picture.promise as unknown as Response;
          }
          return undefined;
        }) as (url: URL) => Response | undefined,
      });

      agent.push(directDispatchEnvelope("steer", slug, { body }));
      await requested.promise;
      await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_picture_read"));
      picture.resolve(new Response(png, { headers: { "Content-Type": "image/png" } }));
      // The switch's own notice is the first delivery; the Send is the second.
      await delivered(2);
      expect(fixture.userMessages).toHaveLength(1);
      const message = { content: [{ text: body, type: "text" }], role: "user", timestamp: 10 };
      expect(matchInjectedUserTurn("ses_after_picture_read", message)).toBe(DIRECT_MESSAGE_ID);
      expect(shownPictures("ses_after_picture_read").has(address)).toBe(true);
      expect(shownPictures("ses_delivery").has(address)).toBe(false);
    });
  });

  // The composer on Dispatch's conversation page completes what the session's last `commands` frame
  // lists, the last frame of every replay. It lists what the session's own terminal picker offers,
  // in that picker's order (built-ins; extension, custom and skill commands; file commands; prompt
  // templates), each name once: a name a built-in answers to by name or alias runs the built-in, and
  // a name an earlier source has runs that source, so offering it again would offer what never runs.
  describe("the slash commands the session's stream lists", () => {
    /** The command list the session's stream ends its replay with, read as the Dispatch relay asks
     *  for it: a watch, then a replay answered on `inbox`. */
    const replayedCommands = async (inbox: string): Promise<unknown> => {
      const control = natsState.controls.get("agentstream.ses_delivery.control");
      if (control === undefined) throw new Error("the agent stream's control was not subscribed");
      const answered = Promise.withResolvers<void>();
      natsState.onPublish = (subject) => {
        if (subject === inbox) answered.resolve();
      };
      control.push(JSON.stringify({ type: "watch", v: 1 }));
      control.push(JSON.stringify({ type: "replay", v: 1 }), inbox);
      await answered.promise;
      const published = natsState.published.findLast((each) => each.subject === inbox);
      const replay: { readonly frames: readonly { readonly kind: string }[] } = JSON.parse(
        new TextDecoder().decode(published?.data)
      );
      const last = replay.frames.at(-1);
      return last?.kind === "commands" && "commands" in last ? last.commands : undefined;
    };
    const builtins: UserInputBuiltinCommand[] = [
      { aliases: [], description: "Compact the context", name: "compact", terminalOnly: false },
      { aliases: ["models"], description: "Pick a model", name: "model", terminalOnly: false },
      { aliases: [], description: "Start a new session", name: "new", terminalOnly: true },
    ];
    const extensionAndSkills: readonly HostSlashCommand[] = [
      { description: "Review with the extension", name: "review", source: "extension" },
      { description: "A custom command the model alias hides", name: "models", source: "prompt" },
      { description: "Dispatch", name: "skill:dispatch", source: "skill" },
    ];

    test("lists the live session's file commands and prompt templates after its other commands, each name once", async () => {
      hostBuiltins.push(...builtins);
      testAgentRoster().push({
        id: "Main",
        kind: "main",
        session: {
          sessionManager: { getSessionId: () => "ses_delivery" },
          slashCommands: [
            { description: "Deploy the branch", name: "deploy" },
            { description: "A file command the extension's review hides", name: "review" },
            { description: "A file command the model alias hides", name: "models" },
          ],
          promptTemplates: [
            { description: "Run the plan (user)", name: "sdd" },
            { description: "A template the deploy file command hides", name: "deploy" },
            { description: "A template the compact built-in hides", name: "compact" },
          ],
        },
        sessionFile: null,
      });
      await bootDirectSession("commands-frame-picker-order", {
        getCommands: () => extensionAndSkills,
        sendUserInput: async () => ({ handled: "prompt" }),
      });

      expect(await replayedCommands("inbox-picker-order")).toEqual([
        { description: "Compact the context", name: "compact", source: "builtin" },
        { description: "Pick a model", name: "model", source: "builtin" },
        { description: "Start a new session", name: "new", source: "builtin", terminalOnly: true },
        { description: "Review with the extension", name: "review", source: "extension" },
        { description: "Dispatch", name: "skill:dispatch", source: "skill" },
        { description: "Deploy the branch", name: "deploy", source: "prompt" },
        { description: "Run the plan (user)", name: "sdd", source: "prompt" },
      ]);
    });

    // The host keeps one `AgentSession` object and changes its id (`/new`, `/resume`, a fork), so
    // the live session is looked up by its id at every ping rather than kept.
    test("lists the rest while the host lists no session under this id, and the file commands once it does", async () => {
      hostBuiltins.push(...builtins);
      let liveID = "ses_before_the_switch";
      testAgentRoster().push({
        id: "Main",
        kind: "main",
        session: {
          sessionManager: { getSessionId: () => liveID },
          promptTemplates: [{ description: "Run the plan (user)", name: "sdd" }],
        },
        sessionFile: null,
      });
      await bootDirectSession("commands-frame-no-live-session", {
        getCommands: () => extensionAndSkills,
        sendUserInput: async () => ({ handled: "prompt" }),
      });
      const others = [
        { description: "Compact the context", name: "compact", source: "builtin" },
        { description: "Pick a model", name: "model", source: "builtin" },
        { description: "Start a new session", name: "new", source: "builtin", terminalOnly: true },
        { description: "Review with the extension", name: "review", source: "extension" },
        { description: "Dispatch", name: "skill:dispatch", source: "skill" },
      ];

      expect(await replayedCommands("inbox-unlisted")).toEqual(others);
      liveID = "ses_delivery";
      expect(await replayedCommands("inbox-listed")).toEqual([
        ...others,
        { description: "Run the plan (user)", name: "sdd", source: "prompt" },
      ]);
    });

    test("publishes no commands frame when the host cannot run typed input", async () => {
      hostBuiltins.push(...builtins);
      await bootDirectSession("commands-frame-untyped-host", {
        getCommands: () => extensionAndSkills,
      });

      expect(await replayedCommands("inbox-untyped-host")).toBeUndefined();
    });

    test("refreshes a changed getCommands result once for a burst of bare watches to an attached viewer", async () => {
      let reads = 0;
      let commands: readonly HostSlashCommand[] = [
        { description: "First", name: "first", source: "extension" },
      ];
      await bootDirectSession("commands-frame-watch-refresh", {
        getCommands: () => {
          reads++;
          return commands;
        },
        sendUserInput: async () => ({ handled: "prompt" }),
      });
      await replayedCommands("inbox-watch-refresh");
      reads = 0;
      commands = [{ description: "Second", name: "second", source: "extension" }];
      const control = natsState.controls.get("agentstream.ses_delivery.control");
      if (control === undefined) throw new Error("the agent stream's control was not subscribed");
      for (let index = 0; index < 3; index += 1) {
        control.push(JSON.stringify({ type: "watch", v: 1 }));
      }
      for (let index = 0; index < 10; index += 1) await Promise.resolve();

      const frame = natsState.published
        .filter((each) => each.subject === "agentstream.ses_delivery.frames")
        .map((each) => JSON.parse(new TextDecoder().decode(each.data)))
        .findLast((each) => each.kind === "commands");
      expect(reads).toBe(1);
      expect(frame?.commands).toEqual([
        { description: "Second", name: "second", source: "extension" },
      ]);
    });

    test("still answers replay when listing commands throws", async () => {
      await bootDirectSession("commands-frame-throws", {
        getCommands: () => {
          throw new Error("the host command list failed");
        },
        sendUserInput: async () => ({ handled: "prompt" }),
      });
      const control = natsState.controls.get("agentstream.ses_delivery.control");
      if (control === undefined) throw new Error("the agent stream's control was not subscribed");
      control.push(JSON.stringify({ type: "watch", v: 1 }));
      control.push(JSON.stringify({ type: "replay", v: 1 }), "inbox-throws");
      // The mock subscription yields queued controls on microtasks, not wall-clock time.
      for (let index = 0; index < 10; index += 1) await Promise.resolve();

      expect(
        natsState.published.findLast((published) => published.subject === "inbox-throws")
      ).toBeDefined();
    });

    test("omits a non-string command description and still replays the other commands", async () => {
      await bootDirectSession("commands-frame-numeric-description", {
        getCommands: () =>
          [
            { description: 42, name: "bad-description", source: "extension" },
            { description: "Good", name: "good-description", source: "extension" },
          ] as unknown as readonly HostSlashCommand[],
        sendUserInput: async () => ({ handled: "prompt" }),
      });

      expect(await replayedCommands("inbox-numeric-description")).toEqual([
        { name: "bad-description", source: "extension" },
        { description: "Good", name: "good-description", source: "extension" },
      ]);
    });
  });

  // A session is shown each picture once (`shownPictures`). A delivery's pictures count as shown
  // once the host took the message that carries them, a session that moves onto a transcript (a
  // fork, a resume, a restart) counts the pictures that transcript already shows, and the id a
  // session leaves is forgotten.
  describe("a session's pictures", () => {
    const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2]);
    const image = {
      type: "image",
      data: Buffer.from(png).toString("base64"),
      mimeType: "image/png",
    };

    /** Dispatch serving LEGION-1's upload `slug`, a 10-byte PNG named shot.png. */
    const pictureRoutes =
      (slug: string) =>
      (url: URL): Response | undefined => {
        if (url.pathname === `/api/v1/issues/LEGION-1/artifacts/${slug}`) {
          return Response.json({
            id: `${slug}-id`,
            name: "shot.png",
            kind: "image",
            versions: [{ number: 1, mime: "image/png", size: png.length }],
          });
        }
        if (url.pathname === `/api/v1/artifacts/${slug}-id/versions/1`) {
          return new Response(png, { headers: { "Content-Type": "image/png" } });
        }
        return undefined;
      };

    for (const [name, turn] of [
      ["a card", false],
      ["a person's own turn", true],
    ] as const) {
      test(`${name} whose send the host refused leaves its picture unshown, and Dispatch's retry shows it`, async () => {
        const slug = turn ? "refused-turn-png" : "refused-card-png";
        const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
        const body = `Look:\n\n![shot.png](${address})`;
        let refusing = true;
        const refused = Promise.withResolvers<void>();
        const { agent, delivered, fixture } = await bootDirectSession(`pictures-${slug}`, {
          // Dispatch's 200 makes the person's turn; a refused accept keeps the card.
          accept: () => (turn ? { accepted: true, body } : { code: "UNAUTHORIZED", status: 401 }),
          dispatch: pictureRoutes(slug),
          beforeSend: () => {
            if (!refusing) return;
            refusing = false;
            refused.resolve();
            throw new Error("the host refused the message");
          },
        });
        const warn = spyOn(console, "warn").mockImplementation(() => undefined);
        try {
          agent.push(directDispatchEnvelope("steer", `${slug}-1`, { body }));
          await refused.promise;
          // The model never saw it, so Dispatch's retry must carry it again.
          expect(shownPictures("ses_delivery").has(address)).toBe(false);

          agent.push(directDispatchEnvelope("steer", `${slug}-2`, { attempt: 2, body }));
          await delivered(1);
        } finally {
          warn.mockRestore();
        }

        const sent: readonly { readonly content: unknown }[] = turn
          ? fixture.userMessages
          : fixture.deliveries;
        expect(sent).toHaveLength(1);
        expect(sent[0]?.content).toEqual([
          {
            type: "text",
            text: turn
              ? body
              : expect.stringContaining(`- image 1: ${address} (shot.png, image/png, 10 bytes)`),
          },
          image,
        ]);
        expect(shownPictures("ses_delivery").has(address)).toBe(true);
      });
    }

    // A card goes into whichever session is live once the accept answers, so its pictures count
    // as shown there; the id the session left was forgotten at the switch and stays forgotten.
    test("a card refused after a switch during the accept counts its picture shown in the session it went into", async () => {
      const slug = "card-after-accept-switch";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const answer = Promise.withResolvers<AcceptAnswer>();
      const acceptRequested = Promise.withResolvers<void>();
      const { agent, delivered, fixture } = await bootDirectSession(slug, {
        accept: () => {
          acceptRequested.resolve();
          return answer.promise;
        },
        dispatch: pictureRoutes(slug),
      });

      agent.push(directDispatchEnvelope("steer", slug, { body }));
      await acceptRequested.promise;
      await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_accept"));
      answer.resolve({ code: "UNAUTHORIZED", status: 401 });
      // The switch's own notice is the first delivery; the card is the second.
      await delivered(2);

      const card = fixture.deliveries.at(-1);
      expect(card?.content).toEqual([
        {
          type: "text",
          text: expect.stringContaining(`- image 1: ${address} (shot.png, image/png, 10 bytes)`),
        },
        image,
      ]);
      expect(shownPictures("ses_after_accept").has(address)).toBe(true);
      expect(shownPictures("ses_delivery").size).toBe(0);
    });

    // The card goes into the session live once its pictures load, which a switch during the
    // load changes again, so that session's set is the one the card's pictures join.
    test("a card whose picture load overlaps a switch counts its picture shown in the session it went into", async () => {
      const slug = "card-load-switch";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const picture = Promise.withResolvers<Response>();
      const requested = Promise.withResolvers<void>();
      const routes = pictureRoutes(slug);
      const { agent, delivered, fixture } = await bootDirectSession(slug, {
        accept: () => ({ code: "UNAUTHORIZED", status: 401 }),
        dispatch: ((url: URL) => {
          if (url.pathname === `/api/v1/artifacts/${slug}-id/versions/1`) {
            requested.resolve();
            return picture.promise as unknown as Response;
          }
          return routes(url);
        }) as (url: URL) => Response | undefined,
      });

      agent.push(directDispatchEnvelope("steer", slug, { body }));
      await requested.promise;
      await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_load"));
      picture.resolve(new Response(png, { headers: { "Content-Type": "image/png" } }));
      // The switch's own notice is the first delivery; the card is the second.
      await delivered(2);

      expect(fixture.deliveries.at(-1)?.content).toEqual([
        {
          type: "text",
          text: expect.stringContaining(`- image 1: ${address} (shot.png, image/png, 10 bytes)`),
        },
        image,
      ]);
      expect(shownPictures("ses_after_load").has(address)).toBe(true);
      expect(shownPictures("ses_delivery").size).toBe(0);
    });

    // An admission still holding its pictures belongs to a `sendUserInput` the host has not yet
    // asked pi-envoy's input handler about: a session change (a branch, a move-off) must leave it,
    // or the accepted turn goes out without its pictures and Dispatch never offers it again.
    test("a typed turn's pictures not yet handed over survive a session change, and count shown in the live session", async () => {
      const slug = "typed-handover-across-switch";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const handlers: { input?: (event: unknown, context: SessionContext) => Promise<unknown> } =
        {};
      const called = Promise.withResolvers<void>();
      const go = Promise.withResolvers<void>();
      const images: unknown[] = [];
      const { agent, context, fixture } = await bootDirectSession(slug, {
        accept: () => ({ accepted: true, body }),
        dispatch: pictureRoutes(slug),
        sendUserInput: async (text) => {
          called.resolve();
          await go.promise;
          const answer = (await handlers.input?.({ source: "extension", text }, context)) as
            | { readonly images?: readonly unknown[] }
            | undefined;
          images.push(...(answer?.images ?? []));
          return { handled: "prompt" };
        },
      });
      handlers.input = fixture.handlers.get("input");

      agent.push(directDispatchEnvelope("steer", slug, { body }));
      await called.promise;
      await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_branched"));
      go.resolve();
      for (let index = 0; index < 10 && images.length === 0; index += 1) {
        await Promise.resolve();
      }

      expect(images).toEqual([image]);
      await fixture.handlers.get("message_start")?.(
        {
          message: {
            content: [{ text: body, type: "text" }],
            role: "user",
            tag: DIRECT_MESSAGE_ID,
            timestamp: 10,
          },
        },
        context
      );
      expect(shownPictures("ses_branched").has(address)).toBe(true);
      expect(shownPictures("ses_delivery").size).toBe(0);
    });

    // Matching asks under the live session id, so the typed turns noted under the id the session
    // left can never be found again: a session change forgets them.
    test("a typed turn's note under the session left is forgotten when the session changes", async () => {
      const running = Promise.withResolvers<UserInputResult>();
      const { agent, delivered, fixture } = await bootDirectSession("typed-note-across-switch", {
        accept: () => ({ accepted: true }),
        sendUserInput: () => running.promise,
      });

      agent.push(directDispatchEnvelope("steer", "typed-note-across-switch"));
      await delivered(1);
      expect(envoyPluginInterface().injectedUserTurns.get("ses_delivery")?.sent).toEqual([
        { messageId: DIRECT_MESSAGE_ID, via: "typed" },
      ]);
      await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_note"));

      expect(envoyPluginInterface().injectedUserTurns.has("ses_delivery")).toBe(false);
      running.resolve({ handled: "prompt" });
    });

    // An ACP host serves several top-level sessions in one process, each with its own pi-envoy,
    // and its fork opens the source session in a new one before forking it (acp-agent.ts:1290-1310
    // at the pin). The fork's instance leaving the source id must not forget the turns the source
    // session's own instance sent in: that instance still serves the id and still asks under it.
    test("a typed turn's note survives another instance in the process passing through its session", async () => {
      const running = Promise.withResolvers<UserInputResult>();
      const { agent, delivered } = await bootDirectSession("typed-note-acp-fork", {
        accept: () => ({ accepted: true }),
        sendUserInput: () => running.promise,
      });
      agent.push(directDispatchEnvelope("steer", "typed-note-acp-fork"));
      await delivered(1);

      const forkQuery = "typed-note-acp-fork-instance";
      const { default: forkExtension } = await import(`./envoy.ts?${forkQuery}`);
      const fork = createPi();
      forkExtension(fork.pi);
      await fork.handlers.get("session_start")?.({}, sessionContext("ses_fork_fresh"));
      await fork.handlers.get("session_switch")?.({}, sessionContext("ses_delivery"));
      await fork.handlers.get("session_switch")?.({}, sessionContext("ses_forked"));

      const message = {
        content: [{ text: DIRECT_MESSAGE_BODY, type: "text" }],
        role: "user",
        tag: DIRECT_MESSAGE_ID,
        timestamp: 10,
      };
      expect(matchInjectedUserTurn("ses_delivery", message)).toBe(DIRECT_MESSAGE_ID);
      running.resolve({ handled: "prompt" });
    });

    for (const [name, first] of [
      ["the host's terminal-only answer", async () => ({ handled: "terminal-only" as const })],
      [
        "a rejected typed input",
        async () => {
          throw new Error("the host rejected it");
        },
      ],
    ] as const) {
      test(`${name} leaves a typed turn's picture unshown, and the person's next Send with it hands it to the input handler`, async () => {
        const slug = `typed-retry-${name.replaceAll(/\W+/g, "-")}`;
        const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
        const body = `Look:\n\n![shot.png](${address})`;
        const handlers: { input?: (event: unknown, context: SessionContext) => Promise<unknown> } =
          {};
        const images: unknown[] = [];
        const asked = Promise.withResolvers<void>();
        let attempts = 0;
        const { agent, context, delivered, fixture, replied } = await bootDirectSession(slug, {
          accept: () => ({ accepted: true, body }),
          dispatch: pictureRoutes(slug),
          sendUserInput: async (text) => {
            attempts++;
            if (attempts === 1) return first();
            const answer = (await handlers.input?.({ source: "extension", text }, context)) as
              | { readonly images?: readonly unknown[] }
              | undefined;
            images.push(...(answer?.images ?? []));
            asked.resolve();
            return { handled: "prompt" };
          },
        });
        handlers.input = fixture.handlers.get("input");

        agent.push(directDispatchEnvelope("steer", `${slug}-1`, { body }));
        await delivered(1);
        await replied(1);
        expect(shownPictures("ses_delivery").has(address)).toBe(false);

        // Dispatch accepts each message once, so the picture's next chance is a new Send of it.
        agent.push(
          directDispatchEnvelope("steer", `${slug}-2`, { body, messageID: NEXT_MESSAGE_ID })
        );
        await delivered(2);
        await asked.promise;
        expect(images).toEqual([
          { data: Buffer.from(png).toString("base64"), mimeType: "image/png", type: "image" },
        ]);
        expect(shownPictures("ses_delivery").has(address)).toBe(false);
        await fixture.handlers.get("message_start")?.(
          {
            message: {
              content: [{ text: body, type: "text" }],
              role: "user",
              tag: NEXT_MESSAGE_ID,
              timestamp: 10,
            },
          },
          context
        );
        expect(shownPictures("ses_delivery").has(address)).toBe(true);
      });
    }

    // Dispatch accepts a message once (`ACCEPT_ALREADY_ACCEPTED` for every later attempt), so the
    // retry of a typed turn whose message went without its picture arrives as a card: the card
    // must carry the picture, since it was never marked shown.
    test("does not mark a picture shown when an earlier input extension rewrites the text, and the retry's card carries it", async () => {
      const slug = "typed-rewrite-picture";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const handlers: { input?: (event: unknown, context: SessionContext) => Promise<unknown> } =
        {};
      const asked = Promise.withResolvers<void>();
      const { agent, context, delivered, fixture } = await bootDirectSession(slug, {
        accept: acceptOnce(body),
        dispatch: pictureRoutes(slug),
        sendUserInput: async () => {
          await handlers.input?.({ source: "extension", text: "rewritten before Envoy" }, context);
          asked.resolve();
          return { handled: "prompt" };
        },
      });
      handlers.input = fixture.handlers.get("input");

      agent.push(directDispatchEnvelope("steer", `${slug}-1`, { body }));
      await delivered(1);
      await asked.promise;
      // The host answered `prompt` after every input handler ran, so this turn will never be
      // handed its pictures: its admission is released, and a later input with the original text
      // is handed nothing.
      for (let index = 0; index < 10; index += 1) await Promise.resolve();
      expect(await handlers.input?.({ source: "extension", text: body }, context)).toBeUndefined();
      await fixture.handlers.get("message_start")?.(
        {
          message: {
            content: [{ text: "rewritten before Envoy", type: "text" }],
            role: "user",
            tag: DIRECT_MESSAGE_ID,
            timestamp: 10,
          },
        },
        context
      );
      expect(shownPictures("ses_delivery").has(address)).toBe(false);

      agent.push(directDispatchEnvelope("steer", `${slug}-2`, { attempt: 2, body }));
      await delivered(2);
      expect(fixture.deliveries.at(-1)?.content).toEqual([
        {
          type: "text",
          text: expect.stringContaining(`- image 1: ${address} (shot.png, image/png, 10 bytes)`),
        },
        image,
      ]);
      expect(shownPictures("ses_delivery").has(address)).toBe(true);
    });

    test("does not mark a picture shown after prompt admission aborts before tagged message_start, and the retry's card carries it", async () => {
      const slug = "typed-abort-picture";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const handlers: { input?: (event: unknown, context: SessionContext) => Promise<unknown> } =
        {};
      const asked = Promise.withResolvers<void>();
      const images: unknown[] = [];
      const { agent, context, delivered, fixture } = await bootDirectSession(slug, {
        accept: acceptOnce(body),
        dispatch: pictureRoutes(slug),
        sendUserInput: async (text) => {
          const answer = (await handlers.input?.({ source: "extension", text }, context)) as
            | { readonly images?: readonly unknown[] }
            | undefined;
          images.push(...(answer?.images ?? []));
          asked.resolve();
          return { handled: "prompt" };
        },
      });
      handlers.input = fixture.handlers.get("input");

      agent.push(directDispatchEnvelope("steer", `${slug}-1`, { body }));
      await delivered(1);
      await asked.promise;
      // The host took the picture and reported prompt, but dropped the prompt before it emitted a
      // tagged message_start.
      expect(images).toEqual([image]);
      expect(shownPictures("ses_delivery").has(address)).toBe(false);

      agent.push(directDispatchEnvelope("steer", `${slug}-2`, { attempt: 2, body }));
      await delivered(2);
      expect(fixture.deliveries.at(-1)?.content).toEqual([
        {
          type: "text",
          text: expect.stringContaining(`- image 1: ${address} (shot.png, image/png, 10 bytes)`),
        },
        image,
      ]);
      expect(shownPictures("ses_delivery").has(address)).toBe(true);
    });

    test("hands both identical in-flight typed turns their own pictures", async () => {
      const slug = "typed-same-body-pictures";
      const address = `dispatch://LEGION-1/artifact/${slug}@v1`;
      const body = `Look:\n\n![shot.png](${address})`;
      const handlers: { input?: (event: unknown, context: SessionContext) => Promise<unknown> } =
        {};
      const inputs: unknown[][] = [];
      const requests: {
        readonly text: string;
        readonly answer: PromiseWithResolvers<UserInputResult>;
      }[] = [];
      const { agent, context, delivered, fixture } = await bootDirectSession(slug, {
        accept: () => ({ accepted: true, body }),
        dispatch: pictureRoutes(slug),
        sendUserInput: (text) => {
          const answer = Promise.withResolvers<UserInputResult>();
          requests.push({ answer, text });
          return answer.promise;
        },
      });
      handlers.input = fixture.handlers.get("input");

      agent.push(
        directDispatchEnvelope("steer", `${slug}-1`, {
          body,
          messageID: "11111111-1111-4111-8111-111111111111",
        })
      );
      agent.push(
        directDispatchEnvelope("steer", `${slug}-2`, {
          body,
          messageID: "22222222-2222-4222-8222-222222222222",
        })
      );
      await delivered(2);
      expect(requests).toHaveLength(2);
      for (const request of requests) {
        const answer = (await handlers.input?.(
          { source: "extension", text: request.text },
          context
        )) as { readonly images?: readonly unknown[] } | undefined;
        inputs.push([...(answer?.images ?? [])]);
        request.answer.resolve({ handled: "prompt" });
      }

      expect(inputs).toEqual([
        [{ data: Buffer.from(png).toString("base64"), mimeType: "image/png", type: "image" }],
        [{ data: Buffer.from(png).toString("base64"), mimeType: "image/png", type: "image" }],
      ]);
    });

    /**
     * A transcript as Oh My Pi keeps it, showing a picture each way a session is shown one: a
     * `dispatch_read` result (`read`), a delivered card (`card`) and a person's own turn (`turn`),
     * beside one the read named without showing (`named`).
     */
    const transcript = (addresses: {
      readonly read: string;
      readonly card: string;
      readonly turn: string;
      readonly named: string;
    }) => [
      {
        type: "message",
        message: {
          role: "toolResult",
          toolCallId: "call-1",
          toolName: "dispatch_read",
          content: [
            {
              type: "text",
              text: [
                "LEGION-1: Pictures",
                "Pictures:",
                `- image 1: ${addresses.read} (a.png, image/png, 10 bytes)`,
                `- not shown: ${addresses.named} (unavailable: Dispatch answered 404)`,
              ].join("\n"),
            },
            image,
          ],
        },
      },
      {
        type: "custom_message",
        customType: "envoy-message",
        content: [
          {
            type: "text",
            text: `envoy: card\n\nPictures:\n- image 1: ${addresses.card} (b.png, image/png, 10 bytes)`,
          },
          image,
        ],
        display: true,
      },
      {
        type: "message",
        message: {
          role: "user",
          content: [{ type: "text", text: `Look:\n\n![c.png](${addresses.turn})` }, image],
        },
      },
    ];

    const transcriptAddresses = (prefix: string) => {
      const address = (slug: string) => `dispatch://LEGION-1/artifact/${prefix}-${slug}-png@v1`;
      return {
        read: address("read"),
        card: address("card"),
        turn: address("turn"),
        named: address("named"),
      };
    };

    for (const reason of ["fork", "resume"] as const) {
      test(`a ${reason} counts the pictures its transcript shows as shown and forgets the id it left`, async () => {
        const [from, to] = [`ses_pictures_${reason}_from`, `ses_pictures_${reason}_to`];
        const addresses = transcriptAddresses(reason);
        globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
        const { default: envoyExtension } = await import(`./envoy.ts?pictures-${reason}`);
        const fixture = createPi();
        envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, sessionContext(from));
        shownPictures(from).add(`dispatch://LEGION-1/artifact/${reason}-earlier-png@v1`);

        const moved = sessionContext(to);
        await fixture.handlers.get("session_switch")?.(
          { reason },
          {
            ...moved,
            sessionManager: { ...moved.sessionManager, getBranch: () => transcript(addresses) },
          }
        );

        expect([...shownPictures(to)].sort()).toEqual(
          [addresses.card, addresses.read, addresses.turn].sort()
        );
        expect(shownPictures(from).size).toBe(0);
      });
    }

    test("a session started on its transcript counts the pictures that transcript shows as shown", async () => {
      const addresses = transcriptAddresses("restart");
      globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
      const { default: envoyExtension } = await import("./envoy.ts?pictures-restart");
      const fixture = createPi();
      envoyExtension(fixture.pi);

      const started = sessionContext("ses_pictures_restarted");
      await fixture.handlers.get("session_start")?.(
        {},
        {
          ...started,
          sessionManager: { ...started.sessionManager, getBranch: () => transcript(addresses) },
        }
      );

      expect([...shownPictures("ses_pictures_restarted")].sort()).toEqual(
        [addresses.card, addresses.read, addresses.turn].sort()
      );
    });

    test("a session that shuts down forgets the pictures it was shown", async () => {
      globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
      const { default: envoyExtension } = await import("./envoy.ts?pictures-shutdown");
      const fixture = createPi();
      envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_pictures_shutdown"));
      shownPictures("ses_pictures_shutdown").add("dispatch://LEGION-1/artifact/shut-png@v1");

      await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_pictures_shutdown"));

      expect(shownPictures("ses_pictures_shutdown").size).toBe(0);
    });

    // A task subagent's instance never runs restoreLocalSessionState (its session_start returns
    // early), so its closure's id stays empty while its tool calls carry the host's live id into
    // the registry; its shutdown forgets that id, not the empty one.
    test("a subagent instance that shuts down forgets the id its tool calls carried", async () => {
      globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
      const { default: envoyExtension } = await import("./envoy.ts?pictures-subagent-shutdown");
      const fixture = createPi();
      envoyExtension(fixture.pi);
      shownPictures("ses_pictures_subagent").add("dispatch://LEGION-1/artifact/sub-png@v1");

      await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_pictures_subagent"));

      expect(shownPictures("ses_pictures_subagent").size).toBe(0);
    });
  });

  test("deduplicates targeted Dispatch frames by dedupe key", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?targeted-dedupe");
    const fixture = createPi();
    const distinctDelivery = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (fixture.deliveries.length === 2) distinctDelivery.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedDispatchEnvelope("steer", "targeted-duplicate"));
    agent.push(targetedDispatchEnvelope("steer", "targeted-duplicate"));
    agent.push(targetedDispatchEnvelope("steer", "targeted-distinct"));
    await distinctDelivery.promise;

    expect(fixture.deliveries).toHaveLength(2);
    expect(fixture.deliveries[0]?.content).toContain("targeted-duplicate");
    expect(fixture.deliveries[1]?.content).toContain("targeted-distinct");
  });

  test("reports an ephemeral delivery rejection to Dispatch without steering it", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: unknown[] = [];
    const posted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      if (
        new URL(input.toString()).pathname ===
        "/api/v1/messages/11111111-1111-4111-8111-111111111111/reply"
      ) {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        posted.resolve();
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-btw-rejection");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.(
      {},
      {
        ...sessionContext("ses_delivery"),
        runEphemeralTurn: async () => {
          throw new Error("No active model on session");
        },
      }
    );
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedDispatchEnvelope("btw", "targeted-rejection"));
    await posted.promise;

    expect(replies).toEqual([
      {
        actor: { id: "ses_delivery", kind: "session" },
        attempt: 1,
        error: "No active model on session",
      },
    ]);
    expect(fixture.deliveries).toEqual([]);
  });

  // Dispatch records a BTW answered with an error as failed, and a same-mode re-send of it
  // repeats the attempt's dedupe key. The agent never answered the first, so the re-send has to
  // run the side turn again; a repeat of one that was answered is still dropped.
  test("runs a BTW side turn again for a Retry of one whose side turn failed", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: unknown[] = [];
    let posted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      if (
        new URL(input.toString()).pathname ===
        "/api/v1/messages/11111111-1111-4111-8111-111111111111/reply"
      ) {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        posted.resolve();
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-btw-retry");
    const fixture = createPi();
    let sideTurns = 0;
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.(
      {},
      {
        ...sessionContext("ses_delivery"),
        runEphemeralTurn: async () => {
          sideTurns += 1;
          if (sideTurns === 1) throw new Error("No API key for provider: openai");
          return { replyText: `Answer ${sideTurns}` };
        },
      }
    );
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    const send = async (dedupeKey: string): Promise<void> => {
      posted = Promise.withResolvers<void>();
      agent.push(targetedDispatchEnvelope("btw", dedupeKey));
      await posted.promise;
    };

    await send("targeted-btw-retry");
    await send("targeted-btw-retry");
    // Answered now: a third copy is a repeat and runs nothing, so the next reply is the sentinel's.
    agent.push(targetedDispatchEnvelope("btw", "targeted-btw-retry"));
    await send("targeted-btw-sentinel");

    expect(sideTurns).toBe(3);
    const actor = { id: "ses_delivery", kind: "session" };
    expect(replies).toEqual([
      { actor, attempt: 1, error: "No API key for provider: openai" },
      { actor, attempt: 1, body: "Answer 2" },
      { actor, attempt: 1, body: "Answer 3" },
    ]);
  });

  test("refuses a BTW that drains in during session_shutdown without starting a side turn", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: unknown[] = [];
    const posted = Promise.withResolvers<void>();
    // Deregistration hangs, so the shutdown handler is still running when the frame arrives.
    const deregistration = Promise.withResolvers<Response>();
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/sessions/ses_delivery") return deregistration.promise;
      if (path === "/api/v1/messages/11111111-1111-4111-8111-111111111111/reply") {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        posted.resolve();
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-btw-shutdown");
    const fixture = createPi();
    const calls: string[] = [];
    envoyExtension(fixture.pi);
    const context: SessionContext = {
      ...sessionContext("ses_delivery"),
      runEphemeralTurn: async () => {
        calls.push("runEphemeralTurn");
        return { replyText: "too late" };
      },
    };
    await fixture.handlers.get("session_start")?.({}, context);
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    const shutdown = fixture.handlers.get("session_shutdown")?.({}, context);
    agent.push(targetedDispatchEnvelope("btw", "targeted-shutdown"));
    await posted.promise;
    deregistration.resolve(response({}));
    await shutdown;

    expect(replies).toEqual([
      {
        actor: { id: "ses_delivery", kind: "session" },
        attempt: 1,
        error: "This OMP session is shutting down",
      },
    ]);
    expect(calls).toEqual([]);
  });

  test("fails closed for a malformed targeted frame and reports the error to Dispatch", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: unknown[] = [];
    const posted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      if (
        new URL(input.toString()).pathname ===
        "/api/v1/messages/44444444-4444-4444-8444-444444444444/reply"
      ) {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        posted.resolve();
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-malformed");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(
      JSON.stringify({
        event_id: "dispatch-malformed",
        source: "dispatch",
        source_event_id: "1",
        topic: "notifications.agent.ses_delivery",
        dedupe_key: "dispatch-malformed",
        issued_at: 1,
        payload_summary: "Can this ship?",
        payload: JSON.stringify({
          event: {
            actor: { id: "alice", kind: "user" },
            issue_key: "CORE-1",
            payload: { body: "Can this ship?", id: "44444444-4444-4444-8444-444444444444" },
            type: "message.created",
          },
          delivery: { attempt: 1, mode: "btw" },
        }),
        trace_id: "dispatch-malformed",
      })
    );

    await posted.promise;
    expect(fixture.deliveries).toEqual([]);
    expect(replies).toEqual([
      {
        actor: { id: "ses_delivery", kind: "session" },
        attempt: 1,
        error: "Invalid Dispatch targeted delivery frame",
      },
    ]);
  });
  test("registers every session and starts its heartbeat on session start", async () => {
    const registrations: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        registrations.push(body);
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
        });
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?always-register");
    const fixture = createPi();
    const heartbeats: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_every_session"),
      setInterval: (callback) => heartbeats.push(callback),
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(registrations).toHaveLength(1);
    expect(heartbeats).toHaveLength(1);
  });

  // OMP's file layout: a parent's transcript sits beside the directory holding its subagents'
  // transcripts (`<parent>.jsonl` next to `<parent>/<Agent>.jsonl`); both exist on disk once
  // `ensureOnDisk` has run. Transcripts live in a fresh directory that each test removes.
  function transcriptFixture() {
    const sessionsDir = mkdtempSync(join(tmpdir(), "envoy-subagent-"));
    const transcript = (name: string): string => {
      const file = join(sessionsDir, name);
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(file, "");
      return file;
    };
    return { transcript, remove: () => rmSync(sessionsDir, { recursive: true, force: true }) };
  }

  function recordingRegistrations(): string[] {
    const registrations: string[] = [];
    globalThis.fetch = async (input, init) => {
      if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
        registrations.push(JSON.parse(init?.body?.toString() ?? "{}").session_id);
      }
      return responseWithRegistration(input, init, {});
    };
    return registrations;
  }

  function liveSession(id: string) {
    return { sessionManager: { getSessionId: () => id } };
  }

  function sessionWithTranscript(
    id: string,
    file: string,
    heartbeats: (() => void)[]
  ): SessionContext {
    return {
      ...sessionContext(id),
      sessionManager: { ...topLevelSession, getSessionId: () => id, getSessionFile: () => file },
      setInterval: (callback) => heartbeats.push(callback),
    };
  }

  test("a task subagent's session_start registers nothing and shares the parent's identity", async () => {
    const fixture = transcriptFixture();
    try {
      const parentFile = fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent.jsonl");
      const childFile = fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl");
      const registrations = recordingRegistrations();
      const { default: envoyExtension } = await import("./envoy.ts?subagent-session");
      const connectsBefore = natsState.connectedNames.length;
      const heartbeats: (() => void)[] = [];

      // The parent and the subagent each load their own instance of the extension.
      const parent = createPi();
      envoyExtension(parent.pi);
      await parent.handlers.get("session_start")?.(
        {},
        sessionWithTranscript("ses_parent", parentFile, heartbeats)
      );
      const child = createPi();
      envoyExtension(child.pi);
      await child.handlers.get("session_start")?.(
        {},
        sessionWithTranscript("ses_child", childFile, heartbeats)
      );
      await child.handlers.get("session_switch")?.(
        { reason: "new" },
        sessionWithTranscript("ses_child_next", childFile, heartbeats)
      );

      expect(registrations).toEqual(["ses_parent"]);
      expect(heartbeats).toHaveLength(1);
      expect(natsState.connectedNames.length - connectsBefore).toBe(1);
      expect(natsState.subscriptions.has("notifications.agent.ses_child")).toBe(false);
      // No identity of its own, so whoami names the reachable one — the parent that spawned
      // it — and reports the subagent's own host session id beside it.
      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      if (whoami === undefined) throw new Error("envoy_whoami was not registered");
      const childContext = sessionWithTranscript("ses_child", childFile, heartbeats);
      const result = await whoami.execute("call-1", {}, undefined, undefined, childContext);
      expect(result.details).toMatchObject({
        sessionID: "ses_parent",
        subagent: { session_id: "ses_child" },
      });
      expect(JSON.parse(result.content[0]?.text ?? "{}")).toMatchObject({
        session_id: "ses_parent",
        // Its instance stored no directory either, so the live one answers for it.
        dir: childContext.cwd,
        subagent: { session_id: "ses_child" },
      });
    } finally {
      fixture.remove();
    }
  });

  type MessageBody = Record<string, unknown> & { readonly source_session?: string };

  /** The message bodies a fixture's tools posted, by listener path. */
  function recordingMessages(): { readonly path: string; readonly body: MessageBody }[] {
    const posted: { readonly path: string; readonly body: MessageBody }[] = [];
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/messages/send" || path === "/v1/messages/publish") {
        posted.push({ path, body: JSON.parse(init?.body?.toString() ?? "{}") });
        return response({
          event_id: "evt_subagent",
          source: "agent",
          source_event_id: "evt_subagent",
          topic: "notifications.agent.ses_target",
          dedupe_key: "dedupe_subagent",
          issued_at: 1,
          payload_summary: "message",
          trace_id: "trace_subagent",
        });
      }
      return responseWithRegistration(input, init, {});
    };
    return posted;
  }

  /**
   * A process whose first top-level session is `parentID`, and the instances that join it: more
   * top-level sessions (an ACP host runs several in one process) and subagents, each nesting
   * under the transcript it is given. Every instance comes from one module import, as they do
   * in a real process.
   */
  async function subagentProcess(
    query: string,
    parentID: string,
    transcript: (name: string) => string
  ) {
    const { default: envoyExtension } = await import(`./envoy.ts?${query}`);
    const heartbeats: (() => void)[] = [];
    const boot = async (id: string, file: string) => {
      const fixture = createPi();
      envoyExtension(fixture.pi);
      const context = sessionWithTranscript(id, file, heartbeats);
      await fixture.handlers.get("session_start")?.({}, context);
      return { ...fixture, context };
    };
    const parentFile = transcript(`2026-09-23T00-00-00-000Z_${parentID}.jsonl`);
    const parent = await boot(parentID, parentFile);
    return { boot, heartbeats, parent, parentFile };
  }

  test("a task subagent's sends name the parent as their source session", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      const host = await subagentProcess("subagent-source", "ses_parent", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl")
      );

      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "from the subagent" },
          undefined,
          undefined,
          child.context
        );
      await child.tools
        .find((tool) => tool.name === "envoy_publish")
        ?.execute(
          "call-publish",
          { topic: "notifications.role.controller", message: "broadcast" },
          undefined,
          undefined,
          child.context
        );

      expect(posted).toEqual([
        {
          path: "/v1/messages/send",
          body: {
            source: "agent",
            source_session: "ses_parent",
            target_session: "ses_target",
            message: "from the subagent",
            idempotency_key: expect.any(String),
          },
        },
        {
          path: "/v1/messages/publish",
          body: {
            source: "agent",
            source_session: "ses_parent",
            topic: "notifications.role.controller",
            message: "broadcast",
            idempotency_key: expect.any(String),
          },
        },
      ]);
    } finally {
      fixture.remove();
    }
  });

  test("a subagent of a subagent names the process's top-level session", async () => {
    const fixture = transcriptFixture();
    try {
      recordingRegistrations();
      const host = await subagentProcess("nested-subagent", "ses_parent", fixture.transcript);
      await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl")
      );
      // OMP nests a subagent's own subagent one directory deeper.
      const nested = await host.boot(
        "ses_nested",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout/Oracle.jsonl")
      );

      const whoami = nested.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, nested.context);
      expect(result?.details).toMatchObject({
        sessionID: "ses_parent",
        subagent: { session_id: "ses_nested" },
      });
    } finally {
      fixture.remove();
    }
  });

  test("a subagent follows the parent to the session id a switch minted", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      const host = await subagentProcess("switched-parent", "ses_parent", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl")
      );
      // A /fork or /handoff mints a new id for the same conversation; the parent's instance
      // republishes it, and the subagent's reply address must move with it.
      await host.parent.handlers.get("session_switch")?.(
        { reason: "fork" },
        sessionWithTranscript(
          "ses_parent_next",
          fixture.transcript("2026-09-23T00-01-00-000Z_ses_parent_next.jsonl"),
          host.heartbeats
        )
      );

      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, child.context);
      expect(result?.details).toMatchObject({ sessionID: "ses_parent_next" });
      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "after the switch" },
          undefined,
          undefined,
          child.context
        );
      expect(posted.map((request) => request.body.source_session)).toEqual(["ses_parent_next"]);
    } finally {
      fixture.remove();
    }
  });

  test("a subagent of a process with no Envoy identity reports no reply address", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      // Nothing ever ran a top-level session_start here: no Envoy config, so no identity to
      // inherit. The tool must say so rather than name the subagent's unreachable host id.
      const { default: envoyExtension } = await import("./envoy.ts?identityless-subagent");
      fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent.jsonl");
      const child = createPi();
      envoyExtension(child.pi);
      const context = sessionWithTranscript(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl"),
        []
      );
      await child.handlers.get("session_start")?.({}, context);

      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, context);
      expect(result?.details).toMatchObject({
        sessionID: "",
        subagent: {
          session_id: "ses_child",
          note: expect.stringContaining("no reply address"),
        },
      });
      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "orphaned" },
          undefined,
          undefined,
          context
        );
      // An empty source session is dropped by the listener's own JSON on the way out, which
      // leaves the recipient no sender and no reply hint: the field must be absent instead.
      expect(posted[0]?.body).not.toHaveProperty("source_session");
    } finally {
      fixture.remove();
    }
  });

  test("a subagent of one of several top-level sessions names the one that spawned it", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      // An ACP host runs several top-level sessions in one process, each firing its own
      // session_start. The subagent belongs to the first; the second must not answer for it.
      const host = await subagentProcess("two-top-level", "ses_a", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_a/Scout.jsonl")
      );
      await host.boot("ses_b", fixture.transcript("2026-09-23T00-02-00-000Z_ses_b.jsonl"));

      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, child.context);
      expect(result?.details).toMatchObject({
        sessionID: "ses_a",
        subagent: { session_id: "ses_child" },
      });
      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "from ses_a's subagent" },
          undefined,
          undefined,
          child.context
        );
      expect(posted.map((request) => request.body.source_session)).toEqual(["ses_a"]);
    } finally {
      fixture.remove();
    }
  });

  test("a nested subagent walks its own transcript up past a second top-level session", async () => {
    const fixture = transcriptFixture();
    try {
      recordingRegistrations();
      const host = await subagentProcess("nested-two-top-level", "ses_a", fixture.transcript);
      await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_a/Scout.jsonl")
      );
      await host.boot("ses_b", fixture.transcript("2026-09-23T00-02-00-000Z_ses_b.jsonl"));
      await host.boot(
        "ses_b_child",
        fixture.transcript("2026-09-23T00-02-00-000Z_ses_b/Scout.jsonl")
      );
      const nested = await host.boot(
        "ses_nested",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_a/Scout/Oracle.jsonl")
      );

      const whoami = nested.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, nested.context);
      expect(result?.details).toMatchObject({
        sessionID: "ses_a",
        subagent: { session_id: "ses_nested" },
      });
    } finally {
      fixture.remove();
    }
  });

  test("a session that switches transcripts leaves no record for a subagent of the old one", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      const host = await subagentProcess("switched-away", "ses_a", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_a/Scout.jsonl")
      );
      // ses_a forks into a new id under a new transcript, and a second top-level session runs
      // beside it, so nothing is unambiguous any more: the entry under ses_a's old transcript
      // must be gone, or this subagent would be handed an id its session has retired.
      await host.parent.handlers.get("session_switch")?.(
        { reason: "fork" },
        sessionWithTranscript(
          "ses_a_next",
          fixture.transcript("2026-09-23T00-01-00-000Z_ses_a_next.jsonl"),
          host.heartbeats
        )
      );
      await host.boot("ses_b", fixture.transcript("2026-09-23T00-02-00-000Z_ses_b.jsonl"));

      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, child.context);
      expect(result?.details).toMatchObject({ sessionID: "" });
      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "orphaned by the fork" },
          undefined,
          undefined,
          child.context
        );
      expect(posted[0]?.body).not.toHaveProperty("source_session");
    } finally {
      fixture.remove();
    }
  });

  test("a subagent of a session whose host mints its id later reports no address", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      // A fresh TUI fires session_start before its id exists and heals by drift up to a
      // heartbeat later. Its subagent must resolve that session and find no id yet, never fall
      // through to whichever other top-level session this process is running.
      const host = await subagentProcess("lazy-id", "", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_/Scout.jsonl")
      );
      await host.boot("ses_b", fixture.transcript("2026-09-23T00-02-00-000Z_ses_b.jsonl"));

      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, child.context);
      expect(result?.details).toMatchObject({ sessionID: "" });
      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "before the id exists" },
          undefined,
          undefined,
          child.context
        );
      expect(posted[0]?.body).not.toHaveProperty("source_session");
    } finally {
      fixture.remove();
    }
  });

  test("a subagent that outlives its session's shutdown reports no address", async () => {
    const fixture = transcriptFixture();
    try {
      const posted = recordingMessages();
      const host = await subagentProcess("shutdown-parent", "ses_parent", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl")
      );
      // The session deregisters with the listener here, so it is no longer an address a reply
      // reaches — the `no live session` failure this whole record exists to stop.
      await host.parent.handlers.get("session_shutdown")?.({}, host.parent.context);

      const whoami = child.tools.find((tool) => tool.name === "envoy_whoami");
      const result = await whoami?.execute("call-1", {}, undefined, undefined, child.context);
      expect(result?.details).toMatchObject({ sessionID: "" });
      await child.tools
        .find((tool) => tool.name === "envoy_send")
        ?.execute(
          "call-send",
          { session_id: "ses_target", message: "after the parent went away" },
          undefined,
          undefined,
          child.context
        );
      expect(posted[0]?.body).not.toHaveProperty("source_session");
    } finally {
      fixture.remove();
    }
  });

  test("a subagent's publish to a role its own parent holds says it was not delivered", async () => {
    const fixture = transcriptFixture();
    try {
      // The listener never delivers a message to the session it names as its source, so a
      // subagent publishing to a role its parent holds gets a 200 with that holder and nothing
      // else. The tool result has to say so; only the role case is visible in the answer.
      globalThis.fetch = async (input, init) => {
        const url = new URL(input.toString());
        if (url.pathname !== "/v1/messages/publish") {
          return responseWithRegistration(input, init, {});
        }
        const topic = String(JSON.parse(init?.body?.toString() ?? "{}").topic);
        return response({
          event_id: "evt_echo",
          source: "agent",
          source_event_id: "evt_echo",
          topic,
          dedupe_key: "dedupe_echo",
          issued_at: 1,
          payload_summary: "broadcast",
          trace_id: "trace_echo",
          holder: topic.endsWith("controller") ? "ses_parent" : "ses_reviewer",
        });
      };
      const host = await subagentProcess("publish-echo", "ses_parent", fixture.transcript);
      const child = await host.boot(
        "ses_child",
        fixture.transcript("2026-09-23T00-00-00-000Z_ses_parent/Scout.jsonl")
      );

      const publish = child.tools.find((tool) => tool.name === "envoy_publish");
      const echoed = await publish?.execute(
        "call-echo",
        { topic: "notifications.role.controller", message: "broadcast" },
        undefined,
        undefined,
        child.context
      );
      expect(echoed?.details).toMatchObject({ holder: "ses_parent", undelivered_echo: true });
      expect(echoed?.content[0]?.text).toContain("Not delivered");

      // The same tool, the same source session, a role someone else holds: delivered as usual,
      // and nothing of the sort said.
      const other = await publish?.execute(
        "call-other",
        { topic: "notifications.role.reviewer", message: "broadcast" },
        undefined,
        undefined,
        child.context
      );
      expect(other?.details).not.toHaveProperty("undelivered_echo");
    } finally {
      fixture.remove();
    }
  });

  test("a top-level session that rotated its transcript still registers in a Legion process", async () => {
    const fixture = transcriptFixture();
    try {
      // legion.ts recorded the pane's transcript at bootstrap; the pane then ran /new, so its
      // live transcript is a different top-level file. A later instance of this extension for
      // that session must read the layout, not the stale record.
      recordBootstrappedSession(fixture.transcript("2026-09-23T00-00-00-000Z_ses_first.jsonl"));
      const rotatedFile = fixture.transcript("2026-09-23T00-01-00-000Z_ses_second.jsonl");
      const registrations = recordingRegistrations();
      const { default: envoyExtension } = await import("./envoy.ts?rotated-top-level");
      const heartbeats: (() => void)[] = [];
      const reloaded = createPi();
      envoyExtension(reloaded.pi);
      await reloaded.handlers.get("session_start")?.(
        {},
        sessionWithTranscript("ses_second", rotatedFile, heartbeats)
      );

      expect(registrations).toEqual(["ses_second"]);
      expect(heartbeats).toHaveLength(1);
    } finally {
      fixture.remove();
    }
  });

  test("a task subagent under SQL session storage registers nothing", async () => {
    // With OMP_SESSION_STORAGE=sql the transcript is a database row: getSessionFile() still
    // returns the .jsonl-shaped logical path, but nothing exists on disk for the layout check
    // to read. The host's roster still says which session is the subagent's.
    const sessionsDir = join(tmpdir(), "envoy-sql-never-created");
    const parentId = "01a0cbf4-6e4e-709b-85d8-70b771f73712";
    const childFile = join(sessionsDir, `2026-09-23T01-49-49-006Z_${parentId}`, "Scout.jsonl");
    const parentFile = join(sessionsDir, `2026-09-23T01-49-49-006Z_${parentId}.jsonl`);
    testAgentRoster().push(
      { id: "Main", kind: "main", session: liveSession("ses_sql_parent"), sessionFile: parentFile },
      { id: "Scout", kind: "sub", session: liveSession("ses_sql_child"), sessionFile: childFile }
    );
    const registrations = recordingRegistrations();
    const { default: envoyExtension } = await import("./envoy.ts?sql-subagent");
    const heartbeats: (() => void)[] = [];
    const parent = createPi();
    envoyExtension(parent.pi);
    await parent.handlers.get("session_start")?.(
      {},
      sessionWithTranscript("ses_sql_parent", parentFile, heartbeats)
    );
    const child = createPi();
    envoyExtension(child.pi);
    await child.handlers.get("session_start")?.(
      {},
      sessionWithTranscript("ses_sql_child", childFile, heartbeats)
    );

    expect(registrations).toEqual(["ses_sql_parent"]);
    expect(heartbeats).toHaveLength(1);
  });

  test("a task subagent of a --no-session parent registers nothing", async () => {
    // An ephemeral parent has no transcript, and OMP puts its subagents' transcripts in a
    // temporary omp-task-* directory with no parent file beside them: the layout says nothing,
    // the host's roster decides.
    const childFile = join(tmpdir(), "omp-task-never-created", "Scout.jsonl");
    testAgentRoster().push(
      { id: "Main", kind: "main", session: liveSession("ses_ephemeral"), sessionFile: null },
      {
        id: "Scout",
        kind: "sub",
        session: liveSession("ses_ephemeral_child"),
        sessionFile: childFile,
      }
    );
    const registrations = recordingRegistrations();
    const { default: envoyExtension } = await import("./envoy.ts?no-session-parent");
    const heartbeats: (() => void)[] = [];
    const parent = createPi();
    envoyExtension(parent.pi);
    await parent.handlers.get("session_start")?.(
      {},
      {
        ...sessionContext("ses_ephemeral"),
        setInterval: (callback) => heartbeats.push(callback),
      }
    );
    const child = createPi();
    envoyExtension(child.pi);
    await child.handlers.get("session_start")?.(
      {},
      sessionWithTranscript("ses_ephemeral_child", childFile, heartbeats)
    );

    expect(registrations).toEqual(["ses_ephemeral"]);
    expect(heartbeats).toHaveLength(1);
  });

  test("registers the session title from the host and refreshes it on heartbeat", async () => {
    const subscribeTitles: unknown[] = [];
    const heartbeatSubscribe = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe" && init?.body !== undefined) {
        subscribeTitles.push(JSON.parse(init.body.toString()).title);
        if (subscribeTitles.length === 2) heartbeatSubscribe.resolve();
      }
      return response({ session_id: "ses_titled", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?register-title");
    const fixture = createPi();
    // Titles are assigned by omp after the first turn, so session_start
    // registers before one exists; the heartbeat must pick it up later.
    let sessionName: string | undefined;
    const heartbeats: (() => void)[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: {
        ...topLevelSession,
        getSessionId: () => "ses_titled",
        getSessionName: () => sessionName,
      },
      setInterval: (callback) => {
        heartbeats.push(callback);
      },
      ui: { ...sessionContext().ui, notify: () => undefined },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(subscribeTitles).toEqual([""]);

    sessionName = "fix envoy ps titles";
    for (const tick of heartbeats) tick();
    await heartbeatSubscribe.promise;
    expect(subscribeTitles).toEqual(["", "fix envoy ps titles"]);
  });

  test("keeps registry registration aligned with live subscriptions", async () => {
    const registrations: { readonly topics: readonly string[] }[] = [];
    const unregistrations: (readonly string[])[] = [];
    const registryTopics = new Set<string>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        registrations.push(body);
        for (const topic of body.topics) registryTopics.add(topic);
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: [...registryTopics],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly topics: readonly string[];
        };
        unregistrations.push(body.topics);
        for (const topic of body.topics) registryTopics.delete(topic);
        return response({});
      }
      if (url.pathname === "/v1/interests/ses_registry") {
        return response({
          session_id: "ses_registry",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [...registryTopics],
        });
      }
      return response({ session_id: "ses_registry", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?live-registration");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_registry"),
      setInterval: (callback) => intervals.push(callback),
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const listTool = fixture.tools.find((tool) => tool.name === "envoy_list");
    if (subscribeTool === undefined || unsubscribeTool === undefined || listTool === undefined) {
      throw new Error("subscription tools were not registered");
    }

    const topic = "notifications.github.o.r.pr.42.>";
    const base = "notifications.github.o.r.pr.42";
    await subscribeTool.execute("", { topics: [topic] });
    expect(registrations.at(-1)?.topics).toEqual(["notifications.agent.ses_registry", base, topic]);
    expect(natsState.controls.get(base)?.active()).toBe(true);
    expect(natsState.controls.get(topic)?.active()).toBe(true);

    for (const tick of intervals) tick();
    expect(registrations.at(-1)?.topics).toEqual(["notifications.agent.ses_registry", base, topic]);
    const subscribed = await listTool.execute("", {});
    expect(subscribed.details.interests).toEqual([
      { topic: "notifications.agent.ses_registry", source: "both" },
      { topic: base, source: "both" },
      { topic, source: "both" },
    ]);

    await unsubscribeTool.execute("", { topics: [topic] });
    expect(unregistrations).toEqual([[base, topic]]);
    expect(registrations.at(-1)?.topics).toEqual(["notifications.agent.ses_registry"]);
    const result = await listTool.execute("", {});
    expect(JSON.parse(result.content[0]?.text ?? "")).toMatchObject({
      topics: ["notifications.agent.ses_registry"],
    });
    expect(result.details.interests).toEqual([
      { topic: "notifications.agent.ses_registry", source: "both" },
    ]);

    // The session's own inbox survives "remove all" and an explicit request
    // alike: it is how direct messages reach the session, not a subscription
    // the tool manages.
    await subscribeTool.execute("", { topics: [topic] });
    const inbox = "notifications.agent.ses_registry";
    const removeAll = await unsubscribeTool.execute("", {});
    expect(removeAll.details.removed).toEqual([base, topic]);
    expect(natsState.controls.get(inbox)?.active()).toBe(true);
    const explicit = await unsubscribeTool.execute("", { topics: [inbox] });
    expect(explicit.details.removed).toEqual([]);
    expect(natsState.controls.get(inbox)?.active()).toBe(true);
    expect(registrations.at(-1)?.topics).toEqual([inbox]);
  });

  test("merges locally live subscriptions into envoy_list before the next heartbeat", async () => {
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
        });
      }
      if (url.pathname === "/v1/interests/ses_list") {
        return response({
          session_id: "ses_list",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: ["notifications.agent.ses_list", "notifications.registry-only"],
        });
      }
      return response({ session_id: "ses_list", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?list-live-subscriptions");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_list"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const listTool = fixture.tools.find((tool) => tool.name === "envoy_list");
    if (subscribeTool === undefined || listTool === undefined)
      throw new Error("subscription tools were not registered");

    await subscribeTool.execute("", { topics: ["notifications.live-only"] });
    const result = await listTool.execute("", {});

    expect(JSON.parse(result.content[0]?.text ?? "")).toMatchObject({
      topics: [
        "notifications.agent.ses_list",
        "notifications.registry-only",
        "notifications.live-only",
      ],
    });
    expect(result.details.interests).toEqual([
      { topic: "notifications.agent.ses_list", source: "both" },
      { topic: "notifications.registry-only", source: "registry" },
      { topic: "notifications.live-only", source: "live" },
    ]);
  });

  test("reports the active session directory through envoy_whoami", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-directory");
    const fixture = createPi();
    const context = sessionContext("ses_whoami");

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const whoami = fixture.tools.find((tool) => tool.name === "envoy_whoami");
    if (whoami === undefined) throw new Error("envoy_whoami was not registered");

    const result = await whoami.execute("", {}, undefined, undefined, context);

    expect(JSON.parse(result.content[0]?.text ?? "")).toMatchObject({
      session_id: "ses_whoami",
      machine_id: hostname(),
      dir: "/tmp/envoy-omp-test",
    });
  });

  test("registers /whoami and copies the current session ID to the clipboard", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-command");
    const fixture = createPi();
    const notifications: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_command"));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    await whoami.handler("", commandContext(notifications));

    expect(fixture.copiedSessionIDs).toEqual(["ses_command"]);
    expect(notifications[0]).toContain("ses_command");
    expect(notifications[0]).toContain("Session ID copied");
  });

  test("copies the new session identity from /whoami after an in-process switch", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-command-switch");
    const fixture = createPi();
    const notifications: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_command"));
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_command"));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    await whoami.handler("", commandContext(notifications));

    expect(fixture.copiedSessionIDs).toEqual(["ses_after_command"]);
    expect(notifications[0]).toContain("ses_after_command");
    expect(notifications[0]).not.toContain("ses_before_command");
  });

  test("still displays the current session ID when clipboard copy fails", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-command-copy-failure");
    const fixture = createPi({ clipboardError: new Error("clipboard unavailable") });
    const notifications: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_clipboard_unavailable"));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    await whoami.handler("", commandContext(notifications));

    expect(fixture.copiedSessionIDs).toEqual([]);
    expect(notifications[0]).toContain("ses_clipboard_unavailable");
    expect(notifications[0]).toContain("Could not copy");
  });

  test("an Envoy with no NATS server says once that inbound messages are off, and keeps its tools", async () => {
    delete process.env.ENVOY_NATS_URL;
    const notifications: string[] = [];
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_unconfigured" },
      setInterval: (callback) => intervals.push(callback),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension } = await import("./envoy.ts?unconfigured");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(notifications).toEqual([
      "envoy: Envoy's inbound messages are off: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL); the Envoy tools still work",
    ]);
    expect(natsState.connectedNames.length).toBe(0);
    expect(intervals.length).toBe(0);
    expect(fixture.tools.map((tool) => tool.name)).toContain("envoy_send");
  });

  // A seed is for NATS alone: its refusal leaves the listener, its tools and /whoami.
  test("a refused NATS seed turns off inbound messages alone, saying why once, and keeps the tools and /whoami", async () => {
    const missing = join(configDirectory, "missing-seed");
    process.env.NATS_NKEY_SEED_FILE = missing;
    const notifications: string[] = [];
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_seed_refused" },
      setInterval: (callback) => intervals.push(callback),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension } = await import("./envoy.ts?seed-refused");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(notifications).toEqual([
      `envoy: Envoy's inbound messages are off: NATS_NKEY_SEED_FILE names ${missing}, which could not be read: ENOENT: no such file or directory, open '${missing}'; the Envoy tools still work`,
    ]);
    expect(natsState.connectedNames.length).toBe(0);
    expect(intervals.length).toBe(0);
    expect(fixture.tools.map((tool) => tool.name)).toContain("envoy_send");
    expect(fixture.commands.map((command) => command.name)).toContain("whoami");
  });

  // nats.js dials 127.0.0.1:4222 when handed no server, so a subscription with inbound off must
  // refuse before any connect: one that reached it would attach to whatever NATS this machine runs.
  for (const { slug, name, configure, why } of [
    {
      slug: "seed-refused",
      name: "its NATS seed refused (envoy.natsUrls other than ENVOY_NATS_URL's)",
      configure: (file: string) => {
        const seed = new TextDecoder().decode(nkeys.createUser().getSeed());
        writeFileSync(
          file,
          JSON.stringify({ envoy: { natsUrls: ["nats://nats-from-file:4222"], natsSeed: seed } })
        );
      },
      why: (file: string) =>
        `ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`,
    },
    {
      slug: "no-nats",
      name: "no NATS server",
      configure: () => {
        delete process.env.ENVOY_NATS_URL;
      },
      why: () => "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)",
    },
  ]) {
    test(`with ${name}, envoy_subscribe and the Legion subscribe refuse naming why, and no NATS is dialed`, async () => {
      const file = process.env.ENVOY_CONFIG_FILE ?? "";
      configure(file);
      // A listener of the row's own on 127.0.0.1:4222 counts what dials it. Where something else
      // already listens there (a local NATS, a CI service), only that count goes unchecked: every
      // other assertion stands, and the mock records a connect without handing it to that server.
      let dialed = 0;
      const loopback = createServer((socket) => {
        dialed += 1;
        socket.destroy();
      });
      const listening = Promise.withResolvers<boolean>();
      loopback.once("error", (error: Error & { readonly code?: string }) => {
        if (error.code === "EADDRINUSE") listening.resolve(false);
        else listening.reject(error);
      });
      loopback.listen(4222, "127.0.0.1", () => listening.resolve(true));
      const watching = await listening.promise;
      if (!watching) {
        console.warn(
          "127.0.0.1:4222 is already in use: this row leaves unchecked that nothing dialed it"
        );
      }
      natsState.realConnect = watching;
      try {
        const { default: envoyExtension } = await import(`./envoy.ts?inbound-off-${slug}`);
        const fixture = createPi();
        envoyExtension(fixture.pi);
        const context = sessionContext("ses_inbound_off");
        await fixture.handlers.get("session_start")?.({}, context);
        const refusal = `inbound messages are off for this session: ${why(file)}`;

        const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
        if (subscribeTool === undefined) throw new Error("envoy_subscribe was not registered");
        const result = await subscribeTool.execute("", { topics: ["team.standup"] });
        expect(result.isError).toBe(true);
        expect(result.content[0]?.text).toBe(refusal);
        await expect(
          subscribeLegionNotice("ses_inbound_off", "notifications.dispatch.ACME", context)
        ).rejects.toThrow(refusal);

        // Each call above awaited its connect, so a dial would already have reached the listener.
        if (watching) expect(dialed).toBe(0);
        expect(natsState.connects).toEqual([]);
      } finally {
        if (watching) {
          const closed = Promise.withResolvers<void>();
          loopback.close(() => closed.resolve());
          await closed.promise;
        }
      }
    });
  }

  // The daemon sends a role's every task and notice to its role topic, which reaches a session only
  // over NATS: a claim with inbound off would hold the role and report ready while every delivery
  // to it goes nowhere. It refuses before any listener call, so nothing is registered or claimed.
  for (const { slug, name, configure } of [
    {
      slug: "seed-refused",
      name: "its NATS seed refused (NATS_NKEY_SEED_FILE names a missing file)",
      configure: () => {
        const missing = join(configDirectory, "missing-seed");
        process.env.NATS_NKEY_SEED_FILE = missing;
        return `NATS_NKEY_SEED_FILE names ${missing}, which could not be read: ENOENT: no such file or directory, open '${missing}'`;
      },
    },
    {
      slug: "no-nats",
      name: "no NATS server",
      configure: () => {
        delete process.env.ENVOY_NATS_URL;
        return "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)";
      },
    },
  ]) {
    test(`with ${name}, the Legion claim refuses naming why, before any listener call`, async () => {
      const why = configure();
      const requests: string[] = [];
      globalThis.fetch = async (input, init) => {
        const { pathname } = new URL(input.toString());
        requests.push(pathname);
        if (pathname !== "/v1/roles/set") return responseWithRegistration(input, init, {});
        const body = JSON.parse(init?.body?.toString() ?? "{}") as { readonly role?: string };
        return response({
          session_id: "ses_claim_inbound_off",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${body.role ?? ""}`],
        });
      };
      const { default: envoyExtension } = await import(`./envoy.ts?claim-inbound-off-${slug}`);
      const fixture = createPi();
      envoyExtension(fixture.pi);
      const context = sessionContext("ses_claim_inbound_off");
      await fixture.handlers.get("session_start")?.({}, context);

      await expect(
        claimEnvoyRole("ses_claim_inbound_off", "legion-omp-repo-1-implementer", context)
      ).rejects.toThrow(`inbound messages are off for this session: ${why}`);
      expect(requests).toEqual([]);
      expect(natsState.connects).toEqual([]);
    });
  }

  test("retries NATS in the background when the initial connection fails", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    natsState.failConnects = 1;
    const notifications: string[] = [];
    const intervals: { callback: () => void; intervalMs: number }[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_retry" },
      setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension } = await import("./envoy.ts?retry");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(notifications[0]).toContain("retrying in the background");
    expect(natsState.subscriptions.has("notifications.agent.ses_retry")).toBe(false);
    // The registration needs no NATS, so the heartbeat registers first and the retry after it.
    const retry = intervals.at(-1);
    if (retry === undefined) throw new Error("retry interval was not registered");

    retry.callback();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(natsState.subscriptions.has("notifications.agent.ses_retry")).toBe(true);
    expect(notifications[1]).toContain("NATS connection established");
    const connectionsAfterRecovery = natsState.connectedNames.length;

    retry.callback();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(natsState.connectedNames.length).toBe(connectionsAfterRecovery);
  });

  test("rebinds the direct subscription after an in-process session switch", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?session-switch");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_switch"));
    const beforeSwitch = natsState.controls.get("notifications.agent.ses_before_switch");
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_switch"));

    expect(beforeSwitch?.active()).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_after_switch")?.active()).toBe(true);
  });

  test("tells the agent its identity changed when a branch re-mints the session id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?branch-identity-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_branch"));
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      sessionContext("ses_after_branch")
    );

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_before_branch");
    expect(fixture.deliveries[0]?.content).toContain("ses_after_branch");
    // The human just rewound; an informational notice must wait for the next
    // turn rather than starting one.
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });
  });

  test("tells the agent its identity changed when a fork re-mints the session id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?fork-identity-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_fork"));
    await fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      sessionContext("ses_after_fork")
    );

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_before_fork");
    expect(fixture.deliveries[0]?.content).toContain("ses_after_fork");
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });
  });

  test("tells the agent its identity changed when a handoff re-mints the session id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?handoff-identity-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    // /handoff carries the agent's own words (and any identity it named)
    // into the new session, so the stale-identity notice must fire.
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_handoff"));
    await fixture.handlers.get("session_switch")?.(
      { reason: "handoff" },
      sessionContext("ses_after_handoff")
    );

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_before_handoff");
    expect(fixture.deliveries[0]?.content).toContain("ses_after_handoff");
  });

  test("stays silent about identity when a switch replaces the transcript or keeps the id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?switch-identity-silent");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_original"));
    // A fresh conversation ("new") and a loaded one ("resume") both carry a
    // transcript that already matches its own id — no stale identity to flag.
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, sessionContext("ses_fresh"));
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      sessionContext("ses_loaded")
    );
    // Tree navigation rebinds without changing the session id.
    await fixture.handlers.get("session_tree")?.(
      { newLeafId: "leaf" },
      sessionContext("ses_loaded")
    );

    expect(fixture.deliveries.length).toBe(0);
  });

  test("stays silent about identity when the session gains its first id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?first-identity-silent");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    // A fresh TUI: session_start fires before any session exists, so the
    // transcript never carried an identity that could go stale.
    await fixture.handlers.get("session_start")?.({}, sessionContext(""));
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      sessionContext("ses_first")
    );

    expect(fixture.deliveries.length).toBe(0);
  });

  test("a network outage during rebind does not swallow the identity notice", async () => {
    let registryDown = false;
    globalThis.fetch = async () => {
      if (registryDown) throw new Error("network unreachable");
      return response({
        session_id: "ses_outage_before",
        machine_id: "test",
        dir: "/tmp",
        topics: [],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?identity-notice-outage");
    const fixture = createPi();
    const notifications: string[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_outage_before"),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    registryDown = true;
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      { ...context, sessionManager: { ...topLevelSession, getSessionId: () => "ses_outage_after" } }
    );

    // The id change is a fact about the transcript, not the network: the
    // degraded-rebind warning and the identity notice must both land.
    expect(notifications.some((message) => message.includes("rebind failed"))).toBe(true);
    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_outage_before");
    expect(fixture.deliveries[0]?.content).toContain("ses_outage_after");
  });

  test("an in-process session switch does not resurrect the previous session's topic", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?switch-no-resubscribe");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_left_behind"));
    const abandoned = natsState.controls.get("notifications.agent.ses_left_behind");
    expect(abandoned).toBeDefined();

    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_current"));
    // Well past ENVOY_RESUBSCRIBE_DELAY_MS, so the pump has had its chance to
    // treat this deliberate close as an outage and recover from it.
    await new Promise((resolve) => setTimeout(resolve, 60));

    // A resubscribe installs a new controls object for the topic; the original
    // surviving means no second subscription was ever opened.
    expect(natsState.controls.get("notifications.agent.ses_left_behind")).toBe(abandoned);
    expect(abandoned?.active()).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_current")?.active()).toBe(true);

    abandoned?.push("addressed to the session we switched away from");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(
      fixture.messages.some((m) => m.includes("addressed to the session we switched away from"))
    ).toBe(false);
  });

  test("envoy_unsubscribe is not undone by the resubscribe path", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?unsubscribe-stays");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_unsub"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    if (subscribeTool === undefined || unsubscribeTool === undefined)
      throw new Error("subscription tools were not registered");

    await subscribeTool.execute("", { topics: ["team.standup"] });
    const dropped = natsState.controls.get("team.standup");
    expect(dropped).toBeDefined();

    const result = await unsubscribeTool.execute("", { topics: ["team.standup"] });
    expect(result.details.removed).toEqual(["team.standup"]);
    await new Promise((resolve) => setTimeout(resolve, 60));

    expect(natsState.controls.get("team.standup")).toBe(dropped);

    dropped?.push("published after the tool said it was unsubscribed");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(
      fixture.messages.some((m) => m.includes("published after the tool said it was unsubscribed"))
    ).toBe(false);
  });

  test("a subscription.removed notice drops the local subscription so dead-connection recovery does not resurrect it", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?subscription-removed-drops-local");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_dropped"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribeTool === undefined) throw new Error("subscription tool was not registered");

    const topic = dispatchIssueSubject("LEGION-1", ">");
    await subscribeTool.execute("", { topics: [topic] });
    const dropped = natsState.controls.get(topic);
    expect(dropped).toBeDefined();

    const inbox = natsState.controls.get("notifications.agent.ses_dropped");
    inbox?.push(
      JSON.stringify({
        source: "dispatch",
        topic: "notifications.agent.ses_dropped",
        payload: JSON.stringify({
          issue_key: "LEGION-1",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          notify: true,
          payload: {
            session_id: "ses_dropped",
            by: { kind: "user", id: "alice" },
            topics: [topic],
          },
        }),
      })
    );
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(natsState.controls.get(topic)).toBe(dropped);
    expect(dropped?.active()).toBe(false);

    // Well past ENVOY_RESUBSCRIBE_DELAY_MS: the dead-connection recovery path
    // had its chance to treat this deliberate close as an outage. It must not.
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(natsState.controls.get(topic)).toBe(dropped);
  });

  test("a subscription.removed notice naming a different session leaves this session's subscription and emits no notice", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import(
      "./envoy.ts?subscription-removed-other-session"
    );
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_other"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribeTool === undefined) throw new Error("subscription tool was not registered");

    const topic = dispatchIssueSubject("LEGION-1", ">");
    await subscribeTool.execute("", { topics: [topic] });
    const kept = natsState.controls.get(topic);
    expect(kept).toBeDefined();

    // This issue's own topic reaches every subscriber, not just the session
    // named in the payload — ses_other must not act on a removal that names
    // a different session (ses_target).
    const inbox = natsState.controls.get("notifications.agent.ses_other");
    inbox?.push(
      JSON.stringify({
        source: "dispatch",
        topic,
        payload: JSON.stringify({
          issue_key: "LEGION-1",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          notify: true,
          payload: { session_id: "ses_target", by: { kind: "user", id: "alice" }, topics: [topic] },
        }),
      })
    );
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(natsState.controls.get(topic)).toBe(kept);
    expect(kept?.active()).toBe(true);
    expect(fixture.messages.some((m) => m.includes("Unsubscribed"))).toBe(false);
  });

  test("envoy_unsubscribe deregisters a topic while its pump waits to retry", async () => {
    const unregistrations: (readonly string[])[] = [];
    const retryScheduled = Promise.withResolvers<void>();
    const originalSetTimeout = globalThis.setTimeout;
    globalThis.setTimeout = ((_: () => void) => {
      retryScheduled.resolve();
      return undefined as never;
    }) as typeof setTimeout;
    try {
      globalThis.fetch = async (input, init) => {
        const url = new URL(input.toString());
        const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
        if (url.pathname === "/v1/interests/unsubscribe") {
          const topics = Array.isArray(body?.topics)
            ? body.topics.filter((topic): topic is string => typeof topic === "string")
            : [];
          unregistrations.push(topics);
          return response({});
        }
        if (url.pathname === "/v1/interests/subscribe") {
          return response({
            session_id: "ses_awaiting_retry",
            machine_id: "test",
            dir: "/tmp/envoy-omp-test",
            topics: body?.topics ?? [],
          });
        }
        return response({});
      };
      const { default: envoyExtension } = await import("./envoy.ts?unsubscribe-awaiting-retry");
      const fixture = createPi();

      envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_awaiting_retry"));
      const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
      const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
      if (subscribeTool === undefined || unsubscribeTool === undefined)
        throw new Error("subscription tools were not registered");

      const topic = "notifications.retrying";
      await subscribeTool.execute("", { topics: [topic] });
      const controls = natsState.controls.get(topic);
      if (controls === undefined) throw new Error("subscription was not created");
      controls.end();
      await retryScheduled.promise;

      const result = await unsubscribeTool.execute("", { topics: [topic] });
      expect(result.details.removed).toEqual([topic]);
      expect(unregistrations).toEqual([[topic]]);
    } finally {
      globalThis.setTimeout = originalSetTimeout;
    }
  });

  test("a deliberately closed topic still recovers from a genuine death once it is back", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?marker-is-consumed");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_home"));
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_away"));
    await new Promise((resolve) => setTimeout(resolve, 60));

    // Switch back: the topic we closed on purpose is legitimately wanted again.
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_home"));
    const reopened = natsState.controls.get("notifications.agent.ses_home");
    expect(reopened?.active()).toBe(true);

    // Now kill it the way a dropped connection does. The close marker from the
    // first switch must have been consumed, so recovery still happens.
    reopened?.end();
    await new Promise((resolve) => setTimeout(resolve, 60));

    const recovered = natsState.controls.get("notifications.agent.ses_home");
    expect(recovered).toBeDefined();
    expect(recovered).not.toBe(reopened);

    recovered?.push(
      forwardedRoleEnvelope(
        "legion-controller",
        "delivered after a genuine iterator death",
        "genuine-death"
      )
    );
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(
      fixture.messages.some((m) => m.includes("delivered after a genuine iterator death"))
    ).toBe(true);
  });

  test("permits redelivery after a failed injection and warns with the envelope id", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?deliver-retry");
    const fixture = createPi();
    let throwNext = true;
    const failedInjection = Promise.withResolvers<void>();
    const followingDelivery = Promise.withResolvers<void>();
    const delivered: string[] = [];
    const warning = spyOn(console, "warn").mockImplementation(() => {});
    const pi: TestPi = {
      ...fixture.pi,
      sendMessage: (message) => {
        if (throwNext) {
          throwNext = false;
          failedInjection.resolve();
          throw new Error("injection rejected mid-compaction");
        }
        delivered.push(message.content);
        if (message.content.includes("the following message proves the pump continued")) {
          followingDelivery.resolve();
        }
      },
    };

    try {
      envoyExtension(pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_pump"));
      const controls = natsState.controls.get("notifications.agent.ses_pump");
      expect(controls).toBeDefined();

      const envelope = forwardedRoleEnvelope(
        "legion-controller",
        "the redelivery repairs the failed injection",
        "retry-after-failure"
      );
      controls?.push(envelope, "_INBOX.failed");
      await failedInjection.promise;
      controls?.push(envelope, "_INBOX.retried");
      controls?.push(
        forwardedRoleEnvelope(
          "legion-controller",
          "the following message proves the pump continued",
          "after-retry"
        ),
        "_INBOX.after"
      );
      await followingDelivery.promise;

      expect(delivered).toHaveLength(2);
      expect(delivered[0]).toContain("the redelivery repairs the failed injection");
      expect(warning).toHaveBeenCalledTimes(1);
      expect(warning).toHaveBeenCalledWith(
        expect.stringContaining("evt-retry-after-failure"),
        expect.any(Error)
      );
      // The failed attempt was acknowledged before its injection threw: the receipt does
      // not depend on injection, and the unrecorded dedupe key lets the re-send inject.
      expect(natsState.published.map((message) => message.subject)).toEqual([
        "_INBOX.failed",
        "_INBOX.retried",
        "_INBOX.after",
      ]);
    } finally {
      warning.mockRestore();
    }
  });

  test("deduplicates a redelivery after successful injection", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?deliver-dedupe");
    const fixture = createPi();
    const deliveryAfterDuplicate = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("the next unique delivery proves the duplicate was skipped")) {
          deliveryAfterDuplicate.resolve();
        }
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_dedupe"));
    const controls = natsState.controls.get("notifications.agent.ses_dedupe");
    if (controls === undefined) throw new Error("agent subject was not subscribed");

    const envelope = forwardedRoleEnvelope(
      "legion-controller",
      "only the first delivery injects",
      "dedupe-after-success"
    );
    controls.push(envelope, "_INBOX.first");
    controls.push(envelope, "_INBOX.duplicate");
    controls.push(
      forwardedRoleEnvelope(
        "legion-controller",
        "the next unique delivery proves the duplicate was skipped",
        "after-dedupe"
      ),
      "_INBOX.next"
    );
    await deliveryAfterDuplicate.promise;

    expect(fixture.messages).toHaveLength(2);
    expect(fixture.messages[0]).toContain("only the first delivery injects");
    // The duplicate is acknowledged — the listener must not report it undelivered —
    // without a second injection.
    expect(natsState.published.map((message) => message.subject)).toEqual([
      "_INBOX.first",
      "_INBOX.duplicate",
      "_INBOX.next",
    ]);
  });

  // Following a pull request's thread and its checks holds two subscriptions for the checks
  // subject, so one CI settlement arrives on both. Its key does not name its event, so only the
  // event id the two copies share makes the second one the first again.
  test("injects one publish once when two followed subscriptions both carry it", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?deliver-overlapping-copies");
    const fixture = createPi();
    const following = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("the next publish proves the copy was skipped")) {
          following.resolve();
        }
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_overlap"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribeTool === undefined) throw new Error("subscription tool was not registered");
    const pr = "notifications.github.acme.widgets.pr.7.>";
    const checks = "notifications.github.acme.widgets.pr.7.checks";
    await subscribeTool.execute("", { topics: [pr, checks] });
    const wildcard = natsState.controls.get(pr);
    const exact = natsState.controls.get(checks);
    if (wildcard === undefined || exact === undefined) throw new Error("topics were not followed");

    const settlement = (eventID: string, summary: string) =>
      JSON.stringify({
        event_id: eventID,
        source: "github",
        source_event_id: `ci-${eventID}`,
        topic: checks,
        dedupe_key: "github.checks.acme.widgets.pr.7.0a1b2c3.g1",
        issued_at: 1,
        payload_summary: summary,
        trace_id: `trace-${eventID}`,
      });
    wildcard.push(settlement("evt-checks-1", "checks settled once"));
    exact.push(settlement("evt-checks-1", "checks settled once"));
    exact.push(settlement("evt-checks-2", "the next publish proves the copy was skipped"));
    await following.promise;

    expect(
      fixture.messages.filter((message) => message.includes("checks settled once"))
    ).toHaveLength(1);
  });

  test("inbound envoy messages deliver as steering so they interrupt an in-flight turn", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?deliver-as-steer");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_steer"));
    const controls = natsState.controls.get("notifications.agent.ses_steer");
    expect(controls).toBeDefined();

    controls?.push(forwardedRoleEnvelope("legion-controller", "steer me mid-turn", "steer"));
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("steer me mid-turn");
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: true });
  });

  test("acknowledges an agent-subject request before any Dispatch call or the injection", async () => {
    // The listener waits two seconds for this receipt and reports a miss as a failed
    // delivery, which the daemon answers by re-sending the message (LEGION-101). The
    // receipt therefore precedes every other step of deliver(); the recorded call
    // sequence is the proof — a fetch recorded here would be a Dispatch call made
    // before the acknowledgement.
    const calls: string[] = [];
    let recording = false;
    globalThis.fetch = async (input, init) => {
      if (recording) calls.push(`fetch ${new URL(input.toString()).pathname}`);
      return responseWithRegistration(input, init, []);
    };
    const { default: envoyExtension } = await import("./envoy.ts?agent-receipt");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        calls.push("sendMessage");
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_receipt"));
    const controls = natsState.controls.get("notifications.agent.ses_receipt");
    if (controls === undefined) throw new Error("agent subject was not subscribed");
    natsState.onPublish = (subject) => calls.push(`publish ${subject}`);
    recording = true;

    controls.push(
      forwardedRoleEnvelope("legion-controller", "receipt event", "agent-receipt"),
      "_INBOX.receipt"
    );
    await injected.promise;

    expect(calls).toEqual(["publish _INBOX.receipt", "sendMessage"]);
    expect(fixture.messages[0]).toContain("receipt event");
  });

  test("a JetStream publish to the direct subject gets no receipt in its reply inbox", async () => {
    // Only the listener's role lane makes a receipt-backed core request: a role
    // frame forwarded to the holder keeps the role topic in its envelope. Every
    // other frame on the direct subject — a Dispatch author route, a peer
    // envoy_send — is a JetStream publish whose reply inbox expects the server's
    // PubAck; an empty receipt there makes the publisher fail with
    // `nats: invalid jetstream publish response`.
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?jetstream-no-receipt");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (fixture.messages.length === 2) injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_jetstream"));
    const controls = natsState.controls.get("notifications.agent.ses_jetstream");
    if (controls === undefined) throw new Error("agent subject was not subscribed");

    controls.push(
      JSON.stringify({
        event_id: "dispatch-author-route",
        source: "dispatch",
        source_event_id: "8341",
        topic: "notifications.agent.ses_jetstream",
        dedupe_key: "dispatch-author-route",
        issued_at: 1,
        payload_summary: "CORE-1 comment created",
        trace_id: "dispatch-author-route",
      }),
      "_INBOX.jetstream-puback"
    );
    controls.push(
      forwardedRoleEnvelope("legion-controller", "role frame after", "role-after-jetstream"),
      "_INBOX.role-receipt"
    );
    await injected.promise;

    expect(fixture.messages[0]).toContain("CORE-1 comment created");
    expect(fixture.messages[1]).toContain("role frame after");
    expect(natsState.published.map((message) => message.subject)).toEqual(["_INBOX.role-receipt"]);
  });

  test("a failed receipt publish is logged with the event id and the message is still injected", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?receipt-publish-failure");
    const fixture = createPi();
    const secondDelivery = Promise.withResolvers<void>();
    const warning = spyOn(console, "warn").mockImplementation(() => {});
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (fixture.messages.length === 2) secondDelivery.resolve();
      },
    });
    try {
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_receipt_fail"));
      const controls = natsState.controls.get("notifications.agent.ses_receipt_fail");
      if (controls === undefined) throw new Error("agent subject was not subscribed");

      natsState.failPublishes = 1;
      controls.push(
        forwardedRoleEnvelope("legion-controller", "receipt lost", "receipt-lost"),
        "_INBOX.lost"
      );
      controls.push(
        forwardedRoleEnvelope("legion-controller", "receipt kept", "receipt-kept"),
        "_INBOX.kept"
      );
      await secondDelivery.promise;

      // The message whose receipt could not be published is still injected — the
      // acknowledgement is best-effort, the local delivery is not — and the pump
      // acknowledges the next one normally.
      expect(fixture.messages[0]).toContain("receipt lost");
      expect(fixture.messages[1]).toContain("receipt kept");
      expect(natsState.published.map((message) => message.subject)).toEqual(["_INBOX.kept"]);
      expect(warning).toHaveBeenCalledTimes(1);
      expect(warning).toHaveBeenCalledWith(
        expect.stringContaining("evt-receipt-lost"),
        expect.any(Error)
      );
    } finally {
      warning.mockRestore();
    }
  });

  test("an ended subscription iterator resubscribes instead of going deaf", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?resubscribe");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_deaf"));
    const first = natsState.controls.get("notifications.agent.ses_deaf");
    expect(first).toBeDefined();

    // Kill the iterator the way a closed/errored connection does.
    first?.end();
    await new Promise((resolve) => setTimeout(resolve, 60));

    const second = natsState.controls.get("notifications.agent.ses_deaf");
    expect(second).toBeDefined();
    expect(second).not.toBe(first);

    second?.push(forwardedRoleEnvelope("legion-controller", "post-recovery message", "recovery"));
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(fixture.messages.some((m) => m.includes("post-recovery message"))).toBe(true);
  });

  test("a heartbeat tick during a registry outage warns once instead of rejecting unhandled", async () => {
    // The extension captures fetch by value at creation, so the blip must be
    // flipped inside the same function object rather than by reassigning
    // globalThis.fetch afterwards.
    let registryDown = false;
    globalThis.fetch = async () => {
      if (registryDown) throw new Error("network unreachable");
      return response({ session_id: "ses_heartbeat", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-blip");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_heartbeat" },
      setInterval: (callback) => intervals.push(callback),
      ui: {
        ...sessionContext().ui,
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    // The healthy start must land on the heartbeat branch, not the NATS-retry
    // branch; a failed registration here would register the wrong interval.
    expect(notifications).toEqual([]);
    expect(intervals.length).toBe(1);

    // The internet blip: every registry call now rejects. bun:test swallows
    // unhandled rejections before user listeners, so the observable contract
    // is the owned rejection's warning: exactly one per outage, however many
    // ticks elapse. On the unfixed code the tick rejects unhandled (fatal in
    // OMP via postmortem exitAfterFatal) and no warning ever appears.
    registryDown = true;
    for (const tick of intervals) tick();
    await warned.promise;
    for (const tick of intervals) tick();
    await Promise.resolve();

    const warnings = notifications.filter((message) =>
      message.includes("registry heartbeat failed")
    );
    expect(warnings).toHaveLength(1);
  });

  test("a rebind during a network outage notifies instead of failing the handler", async () => {
    let registryDown = false;
    globalThis.fetch = async () => {
      if (registryDown) throw new Error("network unreachable");
      return response({ session_id: "ses_rebind", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?rebind-blip");
    const fixture = createPi();
    const notifications: string[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_rebind" },
      setInterval: () => undefined,
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    registryDown = true;
    const switched: SessionContext = {
      ...context,
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_rebind_next" },
    };

    // The handler must resolve; a rejection here surfaces as an extension
    // handler failure on a live session.
    await fixture.handlers.get("session_switch")?.({}, switched);

    expect(notifications.some((message) => message.includes("rebind failed"))).toBe(true);
  });

  test("deregisters a live session before draining its NATS connection", async () => {
    const requests: { readonly method: string; readonly path: string }[] = [];
    let drainHadStartedAtDeregistration: boolean | undefined;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      requests.push({ method: init?.method ?? "GET", path: url.pathname });
      if (url.pathname === "/v1/sessions/ses_shutdown") {
        drainHadStartedAtDeregistration = natsState.drainStarted;
      }
      return response({
        session_id: "ses_shutdown",
        machine_id: "test",
        dir: "/tmp",
        topics: ["notifications.agent.ses_shutdown"],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?shutdown-deregister");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_shutdown"));
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_shutdown"));

    expect(drainHadStartedAtDeregistration).toBe(false);
    expect(requests).toEqual([
      { method: "POST", path: "/v1/interests/subscribe" },
      { method: "DELETE", path: "/v1/sessions/ses_shutdown" },
    ]);
  });

  test("session_shutdown resolves within its budget when drain hangs on a dead connection", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?shutdown-hang");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_shutdown"));

    natsState.drainHangs = true;
    const startedAt = Date.now();
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_shutdown"));

    // OMP kills shutdown handlers at 2s; the bounded drain must finish first.
    expect(Date.now() - startedAt).toBeLessThan(1_900);
  });

  test("starts NATS drain while listener deregistration is still pending", async () => {
    const deletion = Promise.withResolvers<Response>();
    const deleteStarted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/interests/subscribe") return responseWithRegistration(input, init, []);
      if (path === "/v1/sessions/ses_shutdown") {
        deleteStarted.resolve();
        return deletion.promise;
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?shutdown-parallel-drain");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_shutdown"));
    const shutdown = fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_shutdown"));
    if (shutdown === undefined) throw new Error("session_shutdown handler was not registered");

    await deleteStarted.promise;
    await Promise.resolve();
    expect(natsState.drainStarted).toBe(true);

    deletion.reject(new Error("listener cleanup failed"));
    await shutdown;
  });

  test("/whoami reads the live session ID when the session was created after session_start", async () => {
    globalThis.fetch = async () =>
      response({ session_id: "ses_live", machine_id: "test", dir: "/tmp", topics: [] });
    const { default: envoyExtension } = await import("./envoy.ts?whoami-live-id");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    // A fresh TUI: session_start fires before any session exists.
    await fixture.handlers.get("session_start")?.({}, sessionContext(""));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    // The session materialized later; the command context carries the live
    // session manager and must win over the extension's cached empty ID.
    const notifications: string[] = [];
    await whoami.handler("", {
      ui: { notify: (message) => notifications.push(message) },
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_live" },
    });

    expect(fixture.copiedSessionIDs).toEqual(["ses_live"]);
    expect(notifications[0]).toContain("ses_live");
  });

  test("the heartbeat heals a stale registration when the session ID drifts", async () => {
    const registered: string[] = [];
    globalThis.fetch = async (_input: string | URL | Request, init?: RequestInit) => {
      if (init?.body !== undefined) {
        const body = JSON.parse(init.body.toString());
        if (typeof body.session_id === "string") registered.push(body.session_id);
      }
      return response({ session_id: "ses_created", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-drift");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    let liveSessionID = "";
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => liveSessionID },
      setInterval: (callback) => intervals.push(callback),
      ui: { ...sessionContext().ui, notify: () => undefined },
    };

    envoyExtension(fixture.pi);
    // A fresh TUI has no direct subject and no registry row until the host
    // assigns an id, but it still starts the heartbeat that observes that id.
    await fixture.handlers.get("session_start")?.({}, context);
    expect(registered).toEqual([]);
    expect(natsState.controls.has("notifications.agent.")).toBe(false);
    expect(intervals).toHaveLength(1);

    // A tick while the host still has no id registers nothing: the listener
    // rejects an empty session id, and there is no identity to advertise yet.
    // The guard returns before any request is issued, so no wait is needed.
    for (const tick of intervals) tick();
    expect(registered).toEqual([]);

    // The session materializes later; the next heartbeat must rebind the
    // NATS topic and re-register under the live ID instead of heartbeating
    // the dead identity forever.
    liveSessionID = "ses_created";
    for (const tick of intervals) tick();
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(registered).toEqual(["ses_created"]);
    expect(natsState.controls.has("notifications.agent.ses_created")).toBe(true);
  });

  test("surfaces listener subscribe warnings in the envoy_subscribe result", async () => {
    globalThis.fetch = async (input, init) => {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly session_id?: string;
        readonly dir?: string;
        readonly topics?: readonly string[];
      };
      if (new URL(input.toString()).pathname !== "/v1/interests/subscribe") return response({});
      return response({
        session_id: body.session_id ?? "ses_omp",
        machine_id: "example-host",
        dir: body.dir ?? "/tmp",
        topics: body.topics ?? [],
        warnings: ["no matching event in stream"],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?subscribe-warnings");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const subscribe = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    expect(subscribe).toBeDefined();
    if (subscribe === undefined) return;

    const result = await subscribe.execute("", {
      topics: ["notifications.github.example-org.example-repo.pr.7"],
    });

    expect(result.content[0]?.text).toContain("Warnings: no matching event in stream");
    expect(result.details).toMatchObject({ warnings: ["no matching event in stream"] });
  });

  test("returns the 50 newest rendered deliveries from envoy_inbox", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?inbox");
    const fixture = createPi();
    const delivered = Promise.withResolvers<void>();
    let deliveryCount = 0;
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        deliveryCount += 1;
        if (deliveryCount === 51) delivered.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    for (let index = 1; index <= 51; index += 1) {
      agent.push(
        JSON.stringify({
          event_id: `event-${index}`,
          source: "agent",
          source_session: "01a00000-0000-7000-0000-000000000001",
          topic: "notifications.agent.ses_omp",
          dedupe_key: `dedupe-${index}`,
          issued_at: Date.parse("2026-09-07T04:41:12Z"),
          payload_summary: `summary-${index}`,
        })
      );
    }
    await delivered.promise;

    const inbox = fixture.tools.find((tool) => tool.name === "envoy_inbox");
    expect(inbox).toBeDefined();
    if (inbox === undefined) return;
    const result = await inbox.execute("", {});
    const entries = JSON.parse(result.content[0]?.text ?? "[]") as Array<{ event_id: string }>;

    expect(entries).toHaveLength(50);
    expect(entries[0]).toMatchObject({
      event_id: "event-51",
      at: "2026-09-07T04:41:12Z",
      from: "01a00000-0000-7000-0000-000000000001",
      summary: "summary-51",
    });
    expect(entries.at(-1)?.event_id).toBe("event-2");
  });

  test("registers a plain-text renderer for envoy-message so inbound content never renders as a markdown code block", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?message-renderer");
    const fixture = createPi();
    envoyExtension(fixture.pi);

    const renderer = fixture.renderers.get("envoy-message");
    if (renderer === undefined) throw new Error("envoy-message renderer was not registered");

    // TOON nests a structured `message` value under a `message:` key at
    // 4-space indent — the exact shape CommonMark reads as an indented code
    // block. The renderer must show the human body verbatim regardless,
    // since it displays raw text and never runs content through Markdown.
    const content = [
      "envoy:",
      "  from: human",
      "  summary: dispatch answer",
      "  message:",
      "    **bold** answer",
      "",
      "    - one",
      "    - two",
    ].join("\n");
    const theme: MessageRendererTheme = {
      fg: (_color, text) => text,
      bold: (text) => text,
      boxRound: {
        topLeft: "+",
        topRight: "+",
        bottomLeft: "+",
        bottomRight: "+",
        horizontal: "-",
        vertical: "|",
      },
    };

    const component = renderer({ customType: "envoy-message", content }, { expanded: true }, theme);
    if (component === undefined) throw new Error("renderer returned no component");

    const lines = component.render(80).map((line) => Bun.stripANSI(line));
    const rendered = lines.join("\n");
    expect(rendered).toContain("**bold** answer");
    const oneIndex = lines.findIndex((line) => line.includes("- one"));
    const twoIndex = lines.findIndex((line) => line.includes("- two"));
    expect(oneIndex).toBeGreaterThan(-1);
    expect(twoIndex).toBeGreaterThan(oneIndex);
    // A markdown paragraph merge (the bug's actual symptom) would join
    // adjacent lines onto one row; plain-text rendering keeps them apart.
    expect(rendered).not.toContain("- one - two");
  });
});

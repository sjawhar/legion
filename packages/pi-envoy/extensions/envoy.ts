import { existsSync } from "node:fs";
import { delimiter, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  AGENT_STREAM_WATCH_INTERVAL_MS,
  type AgentStreamCommand,
  type AgentStreamControlMessage,
  agentStreamControlSubject,
  agentStreamFramesSubject,
  agentSubject,
  DELIVERY_CAPABILITIES,
  type DeliveryCapability,
  type OpenAsk,
  type OpenAsksResponse,
  ROLE_TOPIC_PREFIX,
  slackTopicChannel,
  zodSchemaApi,
} from "@legion/contracts";
import {
  type AskgateOwner,
  closeStore,
  dropAskgateRecord,
  openStore,
  recordedOwnerMode,
  selfOwner,
  writeAskgateRecord,
} from "@legion/envoy-client/askgate-marker";
import {
  type ConsumerDelivery,
  type ConsumerFrame,
  createConsumerDelivery,
  registeredWithoutRelay,
} from "@legion/envoy-client/consumer-delivery";
import {
  createDeliveryDedupe,
  type DedupeIdentity,
  DedupeIdentitySchema,
  type DeliveryClaim,
  type DispatchDelivery,
  expectsLaneReceipt,
  inboundTimestamp,
  postDeliveryReply,
  type RenderInboundResult,
  readArrivals,
  renderInbound,
  senderLabel,
} from "@legion/envoy-client/delivery";
import { commandName } from "@legion/envoy-client/dispatch-command";
import {
  type ActiveDispatchConfig,
  activeDispatchSettings,
  dispatchSettings,
} from "@legion/envoy-client/dispatch-config";
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import { forgetShownPictures, shownPictures } from "@legion/envoy-client/dispatch-picture-tools";
import { pictureAddresses } from "@legion/envoy-client/dispatch-pictures";
import {
  sessionDirectory as dispatchSessionDirectory,
  sessionStateRoot,
} from "@legion/envoy-client/dispatch-session-directory";
import {
  readResultsSince,
  resultsEnd,
  writeSessionTitle,
} from "@legion/envoy-client/dispatch-session-state";
import { subscriptionRemovedTopics } from "@legion/envoy-client/dispatch-subscribe";
import { askgateSetting, readEnvoyConfig } from "@legion/envoy-client/envoy-config";
import { MIN_PASS_DEADLINE_MS } from "@legion/envoy-client/envoy-settings";
import { messageFor } from "@legion/envoy-client/errors";
import { machineID } from "@legion/envoy-client/machine";
import { natsConnectionOptions } from "@legion/envoy-client/nats-connection";
import {
  closeReplayedRemoval,
  createReplayEngine,
  envoyNoticeText,
  failureNoticeFields,
  type ReplayDeliveryOutcome,
  type ReplayEngine,
  type ReplayFrame,
  type ReplayNotice,
  reachesPrivateStreamSubject,
  replayedText,
  replayFollowedSubjects,
  replayLedgerIdentity,
  replaySuppressed,
  routeReplayNotice,
} from "@legion/envoy-client/replay";
import {
  readSessionAnswers,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import {
  type SessionConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
  sessionSettings,
} from "@legion/envoy-client/session-settings";
import { postSlackReply } from "@legion/envoy-client/slack-post";
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  type MessageMetadataArguments,
  parseEnvoyToolArguments,
  publishConfirmationText,
  sendConfirmationText,
  sessionsOnMachine,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract";
import {
  createEnvoyClient,
  expandSubscriptionTopics,
  mergeInterestSources,
  type SubscribeResult,
} from "@legion/envoy-client/transport";
import {
  dropInjectedUserTurn,
  endInjectedUserTurns,
  forgetInjectedUserTurns,
  matchInjectedUserTurn,
  noteInjectedUserTurn,
  noteTypedUserTurn,
} from "@legion/pi-shared/injected-user-turns";
import { LOCAL_ENVOY_NOTICE, publishEnvoyPluginInterface } from "@legion/pi-shared/interface";
import type {
  ContentBlock,
  ImageContent,
  PiApi,
  SessionContext,
  SessionSwitchReason,
  SideTurn,
  ToolResult,
} from "@legion/pi-shared/pi-types";
import {
  type LegionNoticeSubscription,
  type LegionRoleClaim,
  type LegionRoleClaimInstance,
  legionRoleClaimBridge,
} from "@legion/pi-shared/role-claim-bridge";
import { dispatchCommandHead } from "@legion/pi-shared/shell-command";
import {
  type SessionIdentityContext,
  subagentSessionCheck,
} from "@legion/pi-shared/subagent-session";
import { toolFailure, toolSuccess } from "@legion/pi-shared/tool-result";
import * as host from "@oh-my-pi/pi-coding-agent";
import type { UserInputBuiltinCommand } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler";
import { logger, procmgr, VERSION } from "@oh-my-pi/pi-utils";
import { encode } from "@toon-format/toon";
import { connect, Events, type NatsConnection, StringCodec, type Subscription } from "nats";
import { name as packageName, version as packageVersion } from "../package.json" with {
  type: "json",
};
import {
  AgentStreamPublisher,
  type AgentStreamSettingsState,
  modelReference,
} from "../src/agent-stream";
import { CardWake } from "../src/card-wake";
import { transcriptPictures, withDeliveredPictures } from "../src/delivery-pictures";
import { withDispatchFirst } from "../src/dispatch-first";
import { sessionDispatchClient } from "../src/dispatch-session";
import {
  type AcceptedUserTurn,
  HANDLED_ATTEMPT_ENTRY,
  handledAttemptKey,
  handledAttempts,
  isUserTurnCandidate,
  turnFromAccept,
  typedInputReply,
} from "../src/dispatch-user-turn";
import { recordEnvoySession, resolveEnvoySession } from "../src/envoy-session";
import {
  CONSUMER_STATE_ENTRY,
  consumerSnapshot,
  heldReplayNotices,
  REPLAY_DELIVERED_ENTRY,
  REPLAY_NOTICE_ENTRY,
  REPLAY_STATE_ENTRY,
  rememberedSubjects,
  replaySnapshot,
} from "../src/missed-messages";
import { opensAsk } from "../src/opens-ask";
import {
  createRegistrationState,
  type RegistrationInput,
  type RoleSnapshot,
  type TaskHandle,
  taskWasAbandoned,
} from "../src/registration-state";
import { withSessionAnswers } from "../src/session-answers-context";
import { sideTurn } from "../src/side-turn";
import { TypedPictures } from "../src/typed-pictures";
import { type CardDelivery, UnwrittenFrames, unwrittenCardKeys } from "../src/unwritten-frames";
import { type AskGateRegistration, registerAskGate } from "./askgate/askgate";
import {
  ADVISOR_GATE_ENTRY_TYPE,
  ADVISOR_GATE_RECORD_ENTRY_TYPE,
  type GateEntry,
  type GateRecordEntry,
} from "./askgate/askgate-core";
import { registerEnvoyMessageRenderer } from "./envoy-message-renderer";
import { registerEnvoyWhoamiCommand } from "./envoy-whoami-command";

// The host's built-in slash commands, each marked when only its terminal runs it, which our fork
// exports beside `sendUserInput` (can1357/oh-my-pi#14323). Loaded once and never awaited, through
// a dynamic import because a static one would fail the whole extension's load on a host without
// the module; such a host lists no built-ins.
let hostBuiltinCommands: (() => readonly UserInputBuiltinCommand[]) | undefined;
void import("@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler").then(
  (module) => {
    hostBuiltinCommands = module.listUserInputBuiltinCommands;
  },
  () => undefined
);

/** The host's live `AgentSession`, as far as this extension reads it. */
type LiveAgentSession = NonNullable<host.AgentRef["session"]>;

// The live `AgentSession` under `session`, its live id, found in the host's roster
// (`AgentRegistry`) at every call, since the host keeps one `AgentSession` across `/new`, `/resume`
// and a fork and changes its id. Undefined on a host without the registry export, or listing no
// live session under that id.
function liveAgentSession(session: string): LiveAgentSession | undefined {
  return (
    host.AgentRegistry?.global()
      .list()
      .find((ref) => ref.session?.sessionManager.getSessionId() === session)?.session ?? undefined
  );
}

// The model and thinking level the live session reports, read through the same getters the host's
// `/model` and `/effort` read: `model`, `getAvailableModels()`, `configuredThinkingLevel()` (what
// `/effort` reports, undefined for the model's default) and `getAvailableEffortSelectors()` (what
// `/effort` offers, empty for a model with no adjustable thinking). Unreported with no live session,
// or on a build that lacks any of the four. A model whose provider or id is empty is skipped.
function liveSessionSettings(live: LiveAgentSession | undefined): AgentStreamSettingsState {
  if (
    live === undefined ||
    !("model" in live) ||
    typeof live.configuredThinkingLevel !== "function" ||
    typeof live.getAvailableModels !== "function" ||
    typeof live.getAvailableEffortSelectors !== "function"
  ) {
    return { reported: false };
  }
  const model = modelReference(live.model?.provider, live.model?.id);
  const levels = live.getAvailableEffortSelectors();
  return {
    ...(model !== undefined && { model }),
    models: live.getAvailableModels().flatMap((available) => {
      const listed = modelReference(available.provider, available.id);
      return listed === undefined ? [] : [listed];
    }),
    reported: true,
    thinking:
      levels.length === 0 ? null : { level: live.configuredThinkingLevel() ?? null, levels },
  };
}

const codec = StringCodec();
const NATS_RETRY_INTERVAL_MS = 15_000;
/** How long a delivery waits for the pictures it embeds before it goes out as text alone. */
const DELIVERY_PICTURES_TIMEOUT_MS = 20_000;
/** The most BTW event ids an instance remembers, so a failure notice naming one stays inert. */
const BTW_EVENTS_LIMIT = 1_000;
/** The line a replayed BTW's notice ends with: it runs nothing and asks for nothing. */
const REPLAYED_BTW_NOTE =
  "This BTW was asked while this session was not taking messages. It is shown for your " +
  "information only: it runs no side turn and needs no answer. Dispatch shows whether it was " +
  "answered.";

/**
 * Aside and Steer go through `pi.sendMessage` on every OMP build; BTW only where the host has a
 * side turn (`sideTurn` in src/side-turn.ts), so a host without one advertises every capability
 * but that one.
 */
const CAPABILITIES_WITHOUT_BTW: readonly DeliveryCapability[] = DELIVERY_CAPABILITIES.filter(
  (capability) => capability !== "btw"
);

/**
 * The identity a delivery is claimed under, each field read on its own by the shared rule for it
 * (`DedupeIdentitySchema`): a field the rule refuses identifies nothing, and a valid sibling still
 * does. `renderInbound` renders a frame whose `dedupe_key` is empty, and that frame's `event_id`
 * must still recognise its second copy.
 */
const ClaimIdentitySchema = DedupeIdentitySchema.extend({
  event_id: DedupeIdentitySchema.shape.event_id.catch(undefined),
  dedupe_key: DedupeIdentitySchema.shape.dedupe_key.catch(undefined),
  source: DedupeIdentitySchema.shape.source.catch(undefined),
  source_event_id: DedupeIdentitySchema.shape.source_event_id.catch(undefined),
});

/**
 * Transcript entry recording the role this session holds. Successful claims
 * write `{ role }`; legacy `{ role: null }` release records remain authoritative
 * when a resumed session decides whether to reclaim.
 */
const ROLE_CLAIM_ENTRY = "envoy-role-claim";

const OPEN_ASKS_TIMEOUT_MS = 3_000;

/**
 * How long a person's direct message waits on Dispatch's acceptance before it arrives as a card
 * instead: the frames on the agent subject are delivered one at a time, so a hung Dispatch must
 * not hold every message behind this one for the client's own minute.
 */
const USER_TURN_ACCEPT_TIMEOUT_MS = 10_000;

/**
 * Transcript entry marking a session Legion drives. The process-wide bridge knows the same
 * thing until the process ends; this is what a resumed session reads.
 */
const LEGION_MANAGED_ENTRY = "legion-managed-session";

/** Custom-message type of the run-end nudge itself; never displayed. */
const ASK_REMINDER_MESSAGE = "dispatch-ask-reminder";
/** Unicode line separators that terminate a displayed ask question. */
const ASK_PROMPT_LINE_SEPARATOR = /\r\n|[\r\n\u0085\u2028\u2029]/;

/**
 * Return the first display line while keeping all other C0/C1 controls visibly inert. The
 * separator search handles CRLF, bare CR/LF, NEL, and Unicode line separators; every other
 * control becomes one space before the bounded prompt line is formed.
 */
function askPromptQuestionHead(question: string): string {
  const firstLineEnd = question.search(ASK_PROMPT_LINE_SEPARATOR);
  const firstLine = firstLineEnd === -1 ? question : question.slice(0, firstLineEnd);
  let normalized: string[] | undefined;
  let segmentStart = 0;
  for (let index = 0; index < firstLine.length; index += 1) {
    const codePoint = firstLine.charCodeAt(index);
    if (codePoint > 0x1f && (codePoint < 0x7f || codePoint > 0x9f)) continue;
    normalized ??= [];
    normalized.push(firstLine.slice(segmentStart, index), " ");
    segmentStart = index + 1;
  }
  return (
    normalized === undefined ? firstLine : `${normalized.join("")}${firstLine.slice(segmentStart)}`
  ).trim();
}
/**
 * What the hidden self-check asks the agent, over a snapshot of its own conversation. It names
 * the session's own open asks, then asks whether the agent is waiting on a human for something
 * those asks do not cover. With none it says the session has none, not that Dispatch has none:
 * another session's open ask can cover the wait, and a line denying that ask exists drew WAITING.
 * One word comes back; only WAITING buys the visible turn.
 * A reply a human already said they will send and that is on its way, such as a go-ahead message
 * the user promised, is an event, so PROCEEDING: WAITING would buy a steer to ask for it.
 * Something a human put off for later ("I'll take a look later") or promised and then did not
 * send is not on its way, so it stays WAITING: an agent waits on a human only through an open
 * ask. WAITING excludes a wait the listed asks cover, and PROCEEDING counts a wait any open ask
 * covers, another session's included.
 */
const ASK_SELF_CHECK_PROMPT = (asks: readonly Pick<OpenAsk, "question">[]): string => {
  const askLimit = 5;
  const questionLimit = 120;
  const promptAsks =
    asks.length === 0
      ? "You have no open asks in Dispatch."
      : [
          "Your open asks in Dispatch:",
          ...asks.slice(0, askLimit).map((ask) => {
            const firstLine = askPromptQuestionHead(ask.question);
            const question =
              firstLine.length <= questionLimit
                ? firstLine
                : `${firstLine.slice(0, questionLimit - 1)}…`;
            return `- ${question}`;
          }),
          ...(asks.length > askLimit ? [`+${asks.length - askLimit} more`] : []),
        ].join("\n");

  return (
    "Your run has just ended. " +
    `${promptAsks}\n\n` +
    "Are you right now waiting on a human for anything none of those asks covers? " +
    "Answer with exactly one word and nothing else: WAITING or PROCEEDING. " +
    "WAITING — you stopped because you need a decision, an approval, or information from a human " +
    "that none of those asks covers and that is not already on its way; something they put off " +
    "for later, or promised and then did not send, is not on its way. " +
    "PROCEEDING — you finished, you will carry on by yourself, or you are waiting only on open " +
    "asks, tools, subagents, or events, including a message, an answer, or an event a human " +
    "already said they will send and that is on its way: a reply on its way is an event, not an " +
    "open question."
  );
};

/**
 * A verdict whose first word is WAITING, after any markdown emphasis or quoting — the reply
 * convention the Legion phase-stall follow-up already uses (`packages/pi-legion/src/phase-stall.ts`).
 * Case-sensitive and first-word-only on purpose: "NOT WAITING", "I am WAITING on Sami" and
 * "Waiting." are all PROCEEDING here, because a false WAITING is the expensive error — the
 * steer it buys asserts the agent said it is waiting, which sends it to page a human with a
 * question nobody had, while a false PROCEEDING is only the silence of the status quo.
 */
const WAITING_VERDICT = /^\W*WAITING\b/;

/** The other choice, named anywhere in the reply: the model echoing the question, not answering it. */
const ECHOED_CHOICE = /\bPROCEEDING\b/;

const isWaitingVerdict = (reply: string): boolean =>
  WAITING_VERDICT.test(reply) && !ECHOED_CHOICE.test(reply);

/**
 * Bound on the self-check, imposed by the extension's own clock rather than the host's: it is a
 * whole-context model call on the session's own model, so it costs what a short turn costs; a
 * minute is long enough for a large context on a slow provider and short enough that a wedged
 * one does not hold the stop window open all night. `ENVOY_SELF_CHECK_TIMEOUT_MS` moves it for a
 * deployment whose provider is slower than that — past the bound the check is simply silent, so
 * one set too low turns the nudge off rather than making it wrong.
 */
const ASK_SELF_CHECK_TIMEOUT_MS = 60_000;

/**
 * Most self-checks one armed period pays for. Work re-arms the check, so an unattended run that
 * keeps working would otherwise buy a hidden model call after every settle; five bounds that.
 */
const ASK_CHECKS_PER_PERIOD = 5;

const UNASKED_WAIT_REMINDER =
  "You just said you are waiting on a human for something no open ask in Dispatch covers. " +
  "Open it now: a decision block in the document it concerns (dispatch doc-edit with an ask block), or " +
  "dispatch ask for a to-do only a human can do, naming exactly what you need and from whom.";

/**
 * One arming period of the run-end nudge. A genuine user turn arms a period. `check_due` is the
 * outstanding check, in the shape of the host's own todo reminder: arming owes one, a completed
 * check spends it, the agent's next real work — a successful tool call that ran anything but
 * `dispatch` commands — owes another, and opening the ask itself spends it, since the nudge has
 * nothing left to say about that stop. A turn that only replies and stops does no work, so the
 * nudge's own continuation can never owe one, which is what keeps it from nudging itself forever.
 * `checks` counts the ones spent, capped at `ASK_CHECKS_PER_PERIOD`.
 *
 * `baseline_as_of` is the server clock the period's next Dispatch read asks from, and it moves
 * after every self-check to that snapshot's `as_of`. This keeps the check's view current while
 * the session works between settles.
 *
 * In-memory only, for the life of this process's session. A cold start or a session change
 * begins at period 0, which the stop guard refuses, so nothing nudges before the next genuine
 * user turn arms a period — no transcript entry buys anything beyond that.
 */
interface AskAwarenessState {
  readonly session_id: string;
  readonly period: number;
  readonly baseline_as_of: string | null;
  readonly check_due: boolean;
  readonly checks: number;
}

/**
 * A settle the stop-time check is about: its context and the side turn that context offers, how
 * many runs had started by then, and the awareness generation then, so a user turn between the
 * settle and its check is caught.
 */
interface AskSettle {
  readonly context: SessionContext;
  readonly ask: SideTurn;
  readonly seenRun: number;
  readonly generation: number;
}

interface LegionManagedEntry {
  readonly type: "custom";
  readonly customType: typeof LEGION_MANAGED_ENTRY;
  readonly data: { readonly session_id: string };
}

function isLegionManagedEntry(entry: unknown): entry is LegionManagedEntry {
  if (typeof entry !== "object" || entry === null) return false;
  if (!("type" in entry) || entry.type !== "custom") return false;
  if (!("customType" in entry) || entry.customType !== LEGION_MANAGED_ENTRY) return false;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return false;
  return "session_id" in entry.data && typeof entry.data.session_id === "string";
}

interface RoleClaimEntry {
  readonly type: "custom";
  readonly customType: typeof ROLE_CLAIM_ENTRY;
  readonly data: { readonly role: string | null };
}

// A claim refused, or handed back, because the session it was decided for was no longer open:
// told apart from a claim that failed, since the session may be open again by the time its caller
// reads (a resume Oh My Pi rolled back during the hand-back).
class SessionLeftError extends Error {}

// A registration failure inside a reconcile pass, told apart from a failed role read: only the
// former warrants a `"reregistered"` regain on the next healthy tick.
class RegistrationError extends Error {}

// The agent subject would not open (NATS unavailable) in a pass that still registered the session
// and settled its role over HTTP, as `decided` says: a bridge call goes on with its claim, a
// heartbeat counts the registration, and the next pass opens the subject.
class InboundError extends Error {
  readonly decided: boolean;
  constructor(message: string, decided: boolean, options: ErrorOptions) {
    super(message, options);
    this.decided = decided;
  }
}

function isRoleClaimEntry(entry: unknown): entry is RoleClaimEntry {
  if (typeof entry !== "object" || entry === null) return false;
  if (!("type" in entry) || entry.type !== "custom") return false;
  if (!("customType" in entry) || entry.customType !== ROLE_CLAIM_ENTRY) return false;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return false;
  if (!("role" in entry.data)) return false;
  return entry.data.role === null || typeof entry.data.role === "string";
}

function resolveSkillsDirectory(): string {
  const moduleDirectory = dirname(fileURLToPath(import.meta.url));
  const candidates = [
    // Packed layout: this file is bundled to dist/envoy.js and prepack stages
    // the repo skills/ directory beside it at dist/skills.
    resolve(moduleDirectory, "skills"),
    // Repo layout: this file runs from packages/pi-envoy/extensions/ and the
    // skills live at the repo root.
    resolve(moduleDirectory, "../../../skills"),
  ];
  const found = candidates.find((directory) => existsSync(directory));
  if (!found) {
    throw new Error(`legion skills directory not found; tried: ${candidates.join(", ")}`);
  }
  return found;
}
const SKILLS_DIRECTORY = resolveSkillsDirectory();
// Read once at load, so a plugin packed without the skill fails to load naming the file. A throw
// inside the `context` handler would not do that: Oh My Pi catches it and sends the request anyway.
const DISPATCH_FIRST_CONTEXT = readDispatchFirstContext(dispatchFirstSkillFile(SKILLS_DIRECTORY));

// The `dispatch` command this plugin ships: `bin/` is one directory above this module in both
// layouts (`<pkg>/dist/envoy.js` packed, `<pkg>/extensions/envoy.ts` in the repository). Oh My Pi
// snapshots `process.env` for its shell when it first runs one, after every extension has loaded,
// so `PATH`, `DISPATCH_HOST` and `ENVOY_CONFIG_FILE` below reach every shell command.
const PLUGIN_BIN = resolve(dirname(fileURLToPath(import.meta.url)), "..", "bin");
if (!(process.env.PATH ?? "").split(delimiter).includes(PLUGIN_BIN)) {
  process.env.PATH = `${PLUGIN_BIN}${delimiter}${process.env.PATH ?? ""}`;
}
process.env.DISPATCH_HOST = "omp";
// The session's envoy.json, by Oh My Pi's rule: a named profile's `$PI_CODING_AGENT_DIR/envoy.json`,
// else the file `ENVOY_CONFIG_FILE` names, else the neutral file. The commands the session runs
// are handed the same file, so a `dispatch` command reads what this session reads. A location that
// cannot be worked out (a relative XDG_CONFIG_HOME, ENVOY_CONFIG_FILE or PI_CODING_AGENT_DIR) hands
// nothing on and turns both sections off.
const CONFIG_FILE = sessionConfigFile("omp", process.env);
if (CONFIG_FILE.path !== null) process.env.ENVOY_CONFIG_FILE = CONFIG_FILE.path;
/**
 * The session's Dispatch for one request of this extension's own, which has `ms` to answer: the
 * one builder (`sessionDispatchClient`), resolved again per call, so a Dispatch that moved takes
 * effect without a reload. Undefined when the session has no Dispatch configuration.
 */
const timedDispatchClient = (ms: number) =>
  sessionDispatchClient(CONFIG_FILE, process.cwd(), AbortSignal.timeout(ms))?.client;

export default function envoyExtension(pi: PiApi): void {
  logger.debug("extension instance loaded", { extension: import.meta.url });
  // The interface the Legion plugin reads (`@legion/pi-shared/interface`). `undefined` when an
  // Envoy entry at another interface version published first: that entry's object stays the one
  // Legion finds, and this instance pushes no claim instance onto it (first wins).
  const publishedInterface = publishEnvoyPluginInterface(import.meta.url);
  // Dispatch and Envoy as this session starts with them, each resolved on its own from the
  // session's envoy.json (`CONFIG_FILE`) and the variables over it. The dispatch-first context is
  // injected only where Dispatch is on at load. Dispatch's URL and token are re-read on every use
  // (`currentDispatchConfig`), so a Dispatch that moved - a new dispatch.serverUrl in envoy.json -
  // takes effect without /reload-plugins, and a file that has since broken fails with its own
  // error instead of quietly using the stale endpoint.
  const atStart = sessionSettings(CONFIG_FILE, process.env, { cwd: process.cwd() });
  const { envoy } = atStart;
  const currentDispatchConfig = (): ActiveDispatchConfig => {
    const resolution = sessionDispatchSettings(CONFIG_FILE, process.env, {
      cwd: process.cwd(),
    }).dispatch;
    const config = activeDispatchSettings(resolution);
    if (config === null) {
      throw new Error(
        `Dispatch is no longer configured: ${resolution.enabled ? "" : resolution.notice}`
      );
    }
    return config;
  };
  // Inbound messages arrive over NATS: an Envoy with no NATS server, or whose seed was refused,
  // takes none, and its tools, which call the listener, still work. The start notice says so
  // (`sessionSettings`), with why a section is off, once, and `envoy_subscribe` and the Legion
  // interface's `claim` and `subscribe` refuse naming why (`requireInbound`).
  const inbound = envoy.enabled && !("off" in envoy.inbound);
  const startNotice = atStart.notice ?? "";
  // AskGate (extensions/askgate/) reviews each Dispatch write before it posts, in a session that
  // has Dispatch: its own envoy.json and the variables over it name one. A session or pod with no
  // Dispatch has no write to review, so the gate binds nothing there and says nothing. A
  // checkout's file is left out of that test: it can turn off the Dispatch of its own checkout,
  // but not the gate, which a later shell command can still pass with DISPATCH_URL. The session's
  // own envoy.json is the gate's only switch. Its one `tool_call` handler registers with this
  // factory's own handlers (after this entry's handler, below) and waits for the Oh My Pi modules
  // the gate loads. A switch that cannot be read, and a gate that did not load, are said at every
  // session start; a gate that did not load refuses each write it would review.
  const askgateNotices: string[] = [];
  let askgateLoad: Promise<void> = Promise.resolve();
  // The gate's registration, while it is bound: the load, which the start notice reads, and what
  // publishes the Dispatch record of a write that reached the CLI outside the form the gate reads
  // (`recordUnmarked`). The records' shutdown is the recorder's own, registered where the gate
  // binds (`askgate.ts`), so this entry holds no cut of its own.
  let askgateRecorder: AskGateRegistration | undefined;
  // The session's envoy.json as the gate's own reads go through it, set only where the switch is
  // on: the file object the session resolved, not a path rebuilt from it.
  let askgateFile: Extract<SessionConfigFile, { error: null }> | null = null;
  const configFile = CONFIG_FILE.error === null ? CONFIG_FILE : null;
  const ownDispatch = (file: string): boolean => {
    const own = readEnvoyConfig(file, process.env, { cwd: process.cwd() });
    return dispatchSettings(process.env, own.kind === "read" ? { ...own, checkout: null } : own)
      .enabled;
  };
  if (configFile !== null && (atStart.dispatch.enabled || ownDispatch(configFile.path))) {
    const setting = askgateSetting(configFile.path);
    if (setting.error !== null) askgateNotices.push(setting.error);
    if (setting.enabled) askgateFile = configFile;
  }
  // The record that makes the `dispatch` CLI require AskGate's marker for a gated write
  // (`@legion/envoy-client/askgate-marker`): written, while the gate is bound, for each session id
  // this top-level instance binds (`bindDispatchSession`; a subagent's instance binds none), and
  // kept for every one of them until shutdown, since a shell or kernel started before a switch
  // still acts as the session it started in. A gate that did not load refuses each literal call at
  // the hook (`loadFailureGate`), so those never reach the CLI; it mints no marker and records no
  // mode, so every gated write that does reach the CLI — one outside the literal form, and a
  // subagent's literal one — is refused where the session chose block (`OMP_ASKGATE=block`, which
  // `bindDispatchSession` writes beside the record), and at the warn default posts with a warning
  // and an `unmarked` entry.
  const askgateOwner: AskgateOwner | undefined =
    askgateFile === null ? undefined : selfOwner(sessionStateRoot(process.env));
  const askgateRecords = new Set<string>();
  // The listener client exists only while Envoy is on. The listener calls and the NATS connection
  // reach it through `requireEnvoy`, which refuses, saying why Envoy is off, so this session
  // reaches no listener and no NATS server its configuration does not name. Tools, `/whoami`,
  // inbound messages and the heartbeat exist only with Envoy on, so only the Legion interface's
  // `claim` and `subscribe` can meet that refusal.
  const active = envoy.enabled
    ? {
        settings: envoy,
        client: createEnvoyClient({ baseUrl: envoy.url, fetch, token: envoy.token }),
      }
    : undefined;
  const requireEnvoy = (): NonNullable<typeof active> => {
    if (active === undefined) {
      throw new Error(
        `Envoy is off for this session: ${envoy.enabled ? "" : (envoy.error ?? envoy.notice)}`
      );
    }
    return active;
  };
  // The deadline each queued task gets, read afresh at every task start. Tolerant of Envoy being
  // off, unlike `requireEnvoy`, which throws: the queues exist whatever the session resolved, and
  // a queue cannot answer a throw with a deadline.
  const passDeadlineMs = (): number => active?.settings.passDeadlineMs ?? MIN_PASS_DEADLINE_MS;
  const subscriptions = new Map<string, Subscription>();
  const delivered = createDeliveryDedupe();
  let connection: NatsConnection | undefined;
  let sessionDirectory = "";
  let sessionID = "";
  // The key this instance last published its session under, so a switch that changes the id or
  // the transcript path leaves no entry behind for a subagent to resolve (`src/envoy-session.ts`).
  let recordedSessionKey: string | undefined;
  let heartbeatRegistered = false;
  // The session the host last announced it was leaving (`session_before_switch`,
  // `session_before_branch`) and why: read until the next pass sees where the host went, to learn
  // whether the session it went to continues the one it left, and by the frame drop meanwhile. A
  // session event ends it wherever it lands (a `/reload` lands on the session it announced
  // leaving), and so does a run that started on that session under it and ended unaborted there:
  // a switch that goes ahead aborts a run still going, so that switch was cancelled. (A run that
  // starts and ends while another extension's announcement handler still runs reads the same, and
  // no host event tells the two apart.)
  let announced:
    | { readonly from: string; readonly reason: SessionSwitchReason | undefined }
    | undefined;
  // The announcement the current run started under, on the session it announced leaving.
  let runUnderAnnouncement: typeof announced;
  // The session each host session continues, for a session a fork or a branch opened from another:
  // its records are a copy of that session's, so that session's role and listener rows are its
  // own when its own records name none. A session opened any other way (`/new`, a resume, or a
  // move the host made with no word) continues nothing.
  const continues = new Map<string, string>();
  // `id`, then the sessions it continues (a fork's or a branch's parent, and so on).
  const continuedBy = (id: string): readonly string[] => {
    const lineage = [id];
    for (let at = continues.get(id); at !== undefined && !lineage.includes(at); ) {
      lineage.push(at);
      at = continues.get(at);
    }
    return lineage;
  };
  // The sessions the agent was told it now lives under (`session id changed`), each once.
  const notifiedSessions = new Set<string>();
  let activeSessionContext: SessionContext | undefined;
  const inbox: {
    event_id: string;
    at: string;
    from: string;
    summary: string;
  }[] = [];

  let askAwareness: AskAwarenessState = {
    session_id: "",
    period: 0,
    baseline_as_of: null,
    check_due: false,
    checks: 0,
  };
  // Bumped by everything that invalidates a stop-time check already in flight: a new user turn,
  // and a session change. The period alone cannot carry that — a rebound session's restored
  // period may equal the one the pending check read.
  let awarenessGeneration = 0;
  // One stop-time check at a time. `agent_end` handlers are not awaited by the host, so a second
  // stop can arrive while the first one's Dispatch round trip or self-check is open — it clears
  // every guard below, because `check_due` is not spent until both answers are back. The
  // re-checks after each await then make it silent, but only after it has spent a round trip and
  // a model call on an answer that can change nothing. This makes "one check per stop window"
  // the structure rather than an ordering a re-check happens to win: held from before the query
  // until after `check_due` is spent.
  let askCheckInFlight = false;
  // A settle that arrived while a check was open, and passed every guard but that one: its run
  // started after the check's settle, so the open check will not steer, and it runs again for
  // this settle once it is done. Dropped, the newer settle's check would be lost whenever that
  // run finished inside the check's window — the usual case for an agent that acts on a reply
  // and stops within seconds. It carries the run count read at its own settle: the loop drops
  // it when a run has started since, so the re-check pays no Dispatch read for a settle the
  // session has moved past, and `checkAtSettle` judges staleness against that settle rather
  // than trusting its caller to have done so.
  let settledDuringCheck: AskSettle | undefined;
  const takeSettledDuringCheck = (): AskSettle | undefined => {
    const settle = settledDuringCheck;
    settledDuringCheck = undefined;
    return settle;
  };
  // The self-check in flight, so whatever invalidates it can stop it. Dropping the verdict is
  // not enough: the call is a whole-context request on the session's own model, and one the
  // user superseded by typing competes with the turn they are waiting on for the same provider.
  let askCheckAbort: AbortController | undefined;
  const abortSelfCheck = (reason: string): void => {
    askCheckAbort?.abort(new Error(reason));
    askCheckAbort = undefined;
  };
  // Every run the session has started, which is what a stop-time check compares with the value
  // it read at its settle. A run that starts while the check is open — an Envoy delivery woke
  // the session, or another extension continued it — means the verdict no longer describes
  // where the session is, and any work that run does owes the period a check of its own.
  let runSeq = 0;
  // Whether this session's transcript records Legion driving it. Not matched against the id the
  // guard runs under: `/fork` and `/handoff` mint a new id and carry the transcript, and the
  // session stays Legion-driven across one — the role-claim reader beside it is id-agnostic for
  // the same reason. `/new` and `/resume` install a transcript that is their own, so an empty or
  // replaced branch still reads as not-managed.
  let legionManagedTranscript = false;
  /** The bridge set is the process-local record; the transcript is what a fresh process reads. */
  const legionManaged = (id: string): boolean =>
    legionManagedTranscript || legionRoleClaimBridge().managedSessions.has(id);
  // The attempts of a person's direct message this session delivered, as a card or as a turn
  // (`HANDLED_ATTEMPT_ENTRY`): read from every entry of the session file, so a resume, a switch to
  // another branch of the tree, or both keep a delivered attempt from being accepted again.
  let handledDispatchAttempts = new Set<string>();
  // The session ids this instance noted a person's turn under (`noteTypedUserTurn`,
  // `noteInjectedUserTurn`). The record is process-wide, and an ACP host serves several top-level
  // sessions in one process, each with its own instance: leaving an id forgets only the turns
  // this instance sent in there, never another instance's.
  const notedSessionIDs = new Set<string>();

  const availabilityWarningSessionIDs = new Set<string>();

  const warnAskAvailability = (context: SessionContext, error: unknown): void => {
    const sessionID = context.sessionManager.getSessionId();
    if (availabilityWarningSessionIDs.has(sessionID)) return;
    availabilityWarningSessionIDs.add(sessionID);
    context.ui.notify(
      `envoy: Dispatch open-ask check unavailable (${messageFor(error)}); the stop-time ask reminder is off until it recovers`,
      "warning"
    );
  };

  const overriddenTimeout = Number(process.env.ENVOY_SELF_CHECK_TIMEOUT_MS);
  const selfCheckTimeoutMs =
    Number.isFinite(overriddenTimeout) && overriddenTimeout > 0
      ? overriddenTimeout
      : ASK_SELF_CHECK_TIMEOUT_MS;
  const selfCheckWarningSessionIDs = new Set<string>();

  /** A self-check that failed is silent by design; only the log says it happened. */
  const logSelfCheckFailure = (failedSessionID: string, error: unknown): void => {
    if (selfCheckWarningSessionIDs.has(failedSessionID)) return;
    selfCheckWarningSessionIDs.add(failedSessionID);
    logger.warn("envoy: run-end waiting self-check failed; no reminder was sent", {
      sessionID: failedSessionID,
      error: messageFor(error),
    });
  };

  const queryOpenAsks = async (
    requestedSessionID: string,
    since?: string
  ): Promise<OpenAsksResponse | null> => {
    const dispatch = timedDispatchClient(OPEN_ASKS_TIMEOUT_MS);
    if (dispatch === undefined) return null;
    const snapshot = await dispatch.openAsks(requestedSessionID, since);
    availabilityWarningSessionIDs.delete(requestedSessionID);
    return snapshot;
  };

  const markLegionManagedSession = (targetSessionID: string): void => {
    legionRoleClaimBridge().managedSessions.add(targetSessionID);
    if (legionManagedTranscript) return;
    legionManagedTranscript = true;
    pi.appendEntry(LEGION_MANAGED_ENTRY, { session_id: targetSessionID });
  };

  // Adds `addresses` to `shown`, a session's set of pictures it was shown (`shownPictures`),
  // which later reads and deliveries name instead of sending again: the pictures a delivery
  // showed once the host took it, and those the transcript the session moved onto already shows.
  const markPicturesShown = (shown: Set<string>, addresses: readonly string[]): void => {
    for (const address of addresses) shown.add(address);
  };

  const restoreLocalSessionState = (context: SessionContext): void => {
    const previousSessionID = sessionID;
    sessionDirectory = context.cwd;
    sessionID = context.sessionManager.getSessionId();
    // Only a top-level instance reaches here — a subagent's session_start and switch events
    // return early — so this publishes the session this process registered, whatever moved the
    // id: a start, a switch, or the heartbeat's drift heal. The subagent instances in this
    // process resolve it from their own transcript path as their reply address.
    recordedSessionKey = recordEnvoySession({
      previousKey: recordedSessionKey,
      sessionFile: context.sessionManager.getSessionFile(),
      sessionID,
    });
    activeSessionContext = context;
    const branch = context.sessionManager.getBranch?.() ?? [];
    // Only a genuine session change clears the armed period — a `/fork`, `/handoff`, resume or
    // switch, each of which mints a different id and leaves the period describing a
    // conversation this session is no longer in. Re-establishing the *same* session is not one:
    // the heartbeat's drift heal runs this for every fresh TUI once the host mints its id, and
    // the `session_start` NATS retry runs it every 15 s for the length of an Envoy outage.
    // Clearing on those disabled the nudge for exactly the sessions it was opened up for.
    if (askAwareness.session_id !== sessionID) {
      abortSelfCheck(`session changed from ${askAwareness.session_id || "none"} to ${sessionID}`);
      askAwareness = {
        session_id: sessionID,
        period: 0,
        baseline_as_of: null,
        check_due: false,
        checks: 0,
      };
      awarenessGeneration++;
    }
    legionManagedTranscript = branch.some(isLegionManagedEntry);
    const entries = context.sessionManager.getEntries();
    handledDispatchAttempts = handledAttempts(entries);
    // The replay ledger and cursors, from every entry of the session file as handled attempts are:
    // what this session was shown, live or replayed, on any branch, with the cards the host wrote,
    // and where it read the stream to. The replay notices it records held and not handed, which a
    // restart or a session change cut short, are held again for the first settled heartbeat.
    replay?.restore(replaySnapshot(entries));
    unwritten.requeueNotices(heldReplayNotices(entries));
    // The pictures the transcript the session is now on already shows (a fork's, a handoff's, a
    // resume's, a restart's) are in the history every request sends, so they count as shown. The
    // id the session left is no longer served: what it was shown is forgotten, and so are the
    // turns this instance sent into it (every match asks under the live id; another instance in
    // the process may still serve the id) and its typed turns already handed their pictures
    // (`TypedPictures.dropOtherSessions` says which survive).
    markPicturesShown(shownPictures(sessionID), transcriptPictures(branch));
    if (previousSessionID !== sessionID) {
      forgetShownPictures(previousSessionID);
      if (notedSessionIDs.delete(previousSessionID)) forgetInjectedUserTurns(previousSessionID);
      typedPictures.dropOtherSessions(sessionID);
      // What the session left was handed and has not written is no longer waited on.
      unwritten.releaseOtherSessions(sessionID);
    }
  };

  // Where this instance has read the `dispatch` command's ledger to (`results.jsonl` in the
  // session's state directory, a line per call), so the run-end check sees each call once.
  let dispatchLedger: { readonly sessionID: string; offset: number } | undefined;

  // Names the top-level session in the environment every later shell command takes, and starts
  // its ledger at the end, so a resumed or switched session never reads an earlier run's calls.
  // It also names the CLI state root (`DISPATCH_STATE_DIR`, unless it is already set), so a
  // command that changes HOME or XDG_STATE_HOME still finds the session's ledger and AskGate
  // record. Oh My Pi refreshes the shell's environment after `session_start`, `session_switch`
  // and `session_branch`; any other caller refreshes it itself. A caller after `session_start`
  // passes `ui`, so a record it cannot write is a notice at once: the start notices are shown once.
  const bindDispatchSession = (id: string, ui?: SessionContext["ui"]): void => {
    process.env.DISPATCH_STATE_DIR ||= sessionStateRoot(process.env);
    if (id === "") {
      delete process.env.DISPATCH_SESSION_ID;
      dispatchLedger = undefined;
      return;
    }
    process.env.DISPATCH_SESSION_ID = id;
    if (askgateOwner !== undefined) {
      const dir = dispatchSessionDirectory(process.env, id);
      try {
        openStore(askgateOwner.store);
        // The session's own check mode at the bind: block when it chose block through OMP_ASKGATE,
        // which survives a gate that does not load, or when this process already recorded block for
        // this session id. The gate reads an arm per call and records it in the owner file, so a
        // rebind (a start, a switch, a branch, a host move) keeps a recorded block rather than turn
        // it back to warn before the gate's next call; the gate's call still sets warn when the arm
        // is off. Kept per session in its owner file, so a switch never changes another session's.
        const recorded = recordedOwnerMode(dir, askgateOwner);
        writeAskgateRecord(
          dir,
          askgateOwner,
          process.env.OMP_ASKGATE?.trim() === "block" || recorded === "block" ? "block" : "warn"
        );
        askgateRecords.add(dir);
      } catch (error) {
        const notice = `AskGate could not write its record for session ${id} (${messageFor(error)}), so the dispatch CLI does not ask for AskGate's marker there: a Dispatch write AskGate cannot read posts unreviewed.`;
        logger.warn(notice);
        if (ui === undefined) askgateNotices.push(notice);
        else ui.notify(`envoy: ${notice}`, "warning");
      }
    }
    dispatchLedger = {
      sessionID: id,
      offset: resultsEnd(dispatchSessionDirectory(process.env, id)),
    };
  };

  // The ledger lines appended since the last read, under the id the `dispatch` command writes
  // under, which is the environment's and not a fresh `getSessionId()`. A line that is not JSON is
  // logged with its file and line number and skipped, so it never stalls the run-end check.
  const readNewDispatchResults = () => {
    const id = process.env.DISPATCH_SESSION_ID;
    if (id === undefined || id === "") return [];
    const dir = dispatchSessionDirectory(process.env, id);
    if (dispatchLedger?.sessionID !== id) {
      dispatchLedger = { sessionID: id, offset: resultsEnd(dir) };
      return [];
    }
    const read = readResultsSince(dir, dispatchLedger.offset, (problem) =>
      logger.warn(`envoy: ${problem}`)
    );
    dispatchLedger.offset = read.offset;
    return read.entries;
  };

  pi.on("resources_discover", async () => ({ skillPaths: [SKILLS_DIRECTORY] }));
  registerEnvoyMessageRenderer(pi);

  // Refuses, saying why inbound messages are off (`inbound.off`: no NATS server, or the seed's
  // refusal). A subscription and a Legion claim both ask it first: nats.js dials 127.0.0.1:4222
  // when handed no server, which would attach this session to whatever NATS runs there, and a
  // role's every task and notice reaches its holder only over NATS.
  const requireInbound = () => {
    const { inbound } = requireEnvoy().settings;
    if ("off" in inbound) {
      throw new Error(`inbound messages are off for this session: ${inbound.off}`);
    }
    return inbound;
  };

  // Opens a NATS connection for this session and stores nothing: `ensureConnection` keeps the one
  // this session uses, and a queued task dials through its handle, which closes a connection the
  // task was refused while the dial was in flight rather than leave it dangling.
  const dialNats = async (): Promise<NatsConnection> => {
    const { natsUrls, natsSeed } = requireInbound();
    // nats.js re-subscribes existing subscriptions on its own once reconnected.
    return connect(
      natsConnectionOptions({
        servers: natsUrls,
        name: `omp-${sessionID || "unknown"}`,
        seed: natsSeed,
      })
    );
  };

  // The session's one NATS connection, dialled if it has none. A queued task reaches it through
  // its handle's `connect`, which hands in the task's own fenced dial and checks the task again
  // once this whole call has answered: the wait below, after a dial another caller's connection
  // beat, is inside the step rather than after it.
  const ensureConnection = async (
    dial: () => Promise<NatsConnection> = dialNats
  ): Promise<NatsConnection> => {
    if (connection?.isClosed() === false) return connection;
    const dialled = await dial();
    // Another pass can establish the connection, and the session's shutdown drop it, while this
    // dial is in flight: cleanup drains `connection` alone, and `maxReconnectAttempts: -1` keeps
    // anything else alive for the process's life. The loser of either race is this dial, closed
    // here rather than left dangling.
    if (shuttingDown || connection?.isClosed() === false) {
      await dialled.close().catch(() => {});
      if (!shuttingDown && connection?.isClosed() === false) return connection;
      throw new Error(
        "the session shut down while its NATS dial was in flight; the connection it opened was closed"
      );
    }
    connection = dialled;
    void watchReconnects(dialled);
    return connection;
  };

  // nats.js resubscribes every subscription once it reconnects, but nothing published while it was
  // away is kept for it: each reconnect sweeps the stream for what this session missed, and, while
  // the relay subscription is open, begins a new epoch and registers it at once, so the listener
  // restarts the session's relay at its ack floor (`renewRelayEpoch`). The reader ends with the
  // connection, and stops acting once another connection replaced it.
  const watchReconnects = async (watched: NatsConnection): Promise<void> => {
    try {
      for await (const status of watched.status()) {
        if (connection !== watched) return;
        if (status.type !== Events.Reconnect) continue;
        renewRelayEpoch();
        triggerReplay();
      }
    } catch (error) {
      logger.warn("envoy: the NATS connection's status reader failed", {
        error: messageFor(error),
      });
    }
  };

  const postDispatchReply = async (
    delivery: DispatchDelivery,
    result: { readonly body?: string; readonly error?: string },
    replySessionID = sessionID
  ): Promise<void> => {
    await postDeliveryReply(currentDispatchConfig(), replySessionID, delivery, result);
  };

  // A typed input's pictures, until the host's message carrying its Dispatch id starts
  // (`src/typed-pictures.ts`).
  const typedPictures = new TypedPictures();
  pi.on("input", async (event, context) => {
    // An input that finds the host idle is a prompt sent from idle: any run, one the host dropped
    // with no `agent_end` included, is over (`../src/card-wake`).
    cardWake.noteIfIdle(context);
    if (event.source !== "extension") return undefined;
    const images = typedPictures.handOver(event.text);
    return images === undefined ? undefined : { images: [...(event.images ?? []), ...images] };
  });

  // How a card that starts a turn goes, and the wake for one the host folds into the context with
  // no turn (`../src/card-wake`). A card the host's dispose flushes at shutdown wakes nothing. The
  // start-turn mark the Legion entry keeps, as published at this build's interface version, holds
  // the startup phase while it names the session open here (`sessionOpen`: this instance's own and
  // the host's live one), never a `task` subagent's.
  const cardWake = new CardWake({
    now: () => Date.now(),
    sendMessage: (message, options) => pi.sendMessage(message, options),
    startupTurn: () => publishedInterface?.startupTurn,
    isOwnSession: (session) => sessionOpen(session),
  });

  // A card's message: its `details` name the frame (`replayLedgerIdentity`), which the host keeps
  // on the message it writes, beside the wake's card marker. An `inert` card is a steer that starts
  // no turn on an idle host; a turn streaming when it is handed takes it and runs another model
  // turn for it, so `UnwrittenFrames` hands an inert card only to a settled host (`hostSettled`).
  // Every other card goes as the wake says (`CardWake.deliveryFor`), which records a startup card
  // before the host has it; a targeted Aside stays an aside.
  const handCard = (
    content: string | readonly ContentBlock[],
    deliverAs: CardDelivery,
    identity: DedupeIdentity | undefined
  ): void => {
    const known = identity === undefined ? {} : replayLedgerIdentity(identity);
    const frame = unwrittenCardKeys(known).length === 0 ? undefined : { ...known };
    if (deliverAs === "inert") {
      pi.sendMessage(
        {
          customType: "envoy-message",
          content,
          display: true,
          ...(frame === undefined ? {} : { details: frame }),
        },
        { deliverAs: "steer", triggerTurn: false }
      );
      return;
    }
    const { details, ...options } = cardWake.deliveryFor(
      deliverAs === "aside",
      activeSessionContext
    );
    pi.sendMessage(
      { customType: "envoy-message", content, display: true, details: { ...frame, ...details } },
      options
    );
  };

  // The host is idle, holds no message, is not shutting down, and is on the session this instance
  // serves: only then is an inert card handed, or a withdrawn card handed back (`UnwrittenFrames`).
  const hostSettled = (): boolean => {
    const hostContext = activeSessionContext;
    return (
      hostContext?.isIdle?.() === true &&
      hostContext.hasPendingMessages?.() === false &&
      !shuttingDown &&
      hostSessionID() === sessionID
    );
  };

  const unwritten = new UnwrittenFrames({
    sessionID: () => sessionID,
    settled: hostSettled,
    recordDelivered: async (identity) => {
      await replay?.recordDelivered(identity);
    },
    handCard,
    handNotice: (content) => {
      pi.sendMessage(
        { customType: "envoy-message", content, display: true, details: LOCAL_ENVOY_NOTICE },
        { deliverAs: "steer", triggerTurn: false }
      );
    },
    matchTurn: (message) => matchInjectedUserTurn(sessionID, message),
    turnWithdrawn: (turnSessionID, messageID) => {
      dropInjectedUserTurn(turnSessionID, messageID);
      typedPictures.withdraw(messageID);
    },
    handBackFailed: (identity, error) => {
      logger.warn("envoy: a card waiting for a settled host could not be handed", {
        eventID: identity.event_id,
        error: messageFor(error),
      });
    },
    handNoticeFailed: (error) => {
      logger.warn("envoy: a replay notice waiting for a settled host could not be handed", {
        error: messageFor(error),
      });
    },
    // A held notice goes to the transcript of the session it was raised for, which the host is on
    // while this instance serves it.
    recordNotice: (id, content) => {
      if (hostSessionID() !== sessionID) throw new Error("the host is on another session");
      pi.appendEntry(REPLAY_NOTICE_ENTRY, { id, content });
    },
    noticeHanded: (id) => {
      pi.appendEntry(REPLAY_NOTICE_ENTRY, { id, handed: true });
    },
    recordNoticeFailed: (error) => {
      logger.warn("envoy: a held replay notice could not be recorded in the session file", {
        error: messageFor(error),
      });
    },
  });

  // Shows a card the replay engine asked for (a stored frame, or a failure notice naming one)
  // under a claim of that frame, so a live copy arriving meanwhile is a repeat: `queued` while it
  // waits to be written (`UnwrittenFrames.sendCard`), `shown` when it does not wait. A frame the
  // live path is delivering now is that path's to show and record, and one whose card waits
  // already is that card's. A frame the dedupe let go of that the open session's ledger does not
  // hold (one shown before the host moved to this session, say) is handed with no claim, and waits
  // on its card's write all the same: nothing records it until the host writes it.
  const showReplayCard = (
    content: string,
    envelope: unknown,
    deliverAs: CardDelivery = "steer"
  ): "shown" | "queued" => {
    const parsed = ClaimIdentitySchema.safeParse(envelope);
    const identity = parsed.success ? parsed.data : undefined;
    const claim = delivered.claim(identity);
    if (
      claim === undefined &&
      identity !== undefined &&
      (delivered.pending(identity) || unwritten.waitsOnCard(identity))
    ) {
      return "queued";
    }
    return unwritten.sendCard(content, deliverAs, identity, claim);
  };

  // The event ids of BTW frames this process met, live or replayed, so a failure notice naming one
  // is inert too: a BTW never starts a turn. Bounded: the oldest is forgotten first.
  const btwEvents = new Set<string>();
  const noteBtw = (eventID: string | undefined): void => {
    if (eventID === undefined) return;
    btwEvents.delete(eventID);
    btwEvents.add(eventID);
    if (btwEvents.size > BTW_EVENTS_LIMIT) {
      const oldest = btwEvents.values().next();
      if (oldest.done !== true) btwEvents.delete(oldest.value);
    }
  };

  // Runs a person's accepted Send or Aside as if typed at the terminal (`pi.sendUserInput`): a
  // command, a skill, a template, or a prompt. Never awaited here: on an idle session the host
  // answers only once the run it starts ends, and the pump that called `deliver` must keep taking
  // frames meanwhile, the person's next Send included. What a command printed, or why it did not
  // run, is the session's reply on Dispatch, which the conversation shows. The turn is noted under
  // its tag before the send, and dropped once the host answers that the input submitted no message
  // (a command, a terminal-only built-in, a refusal, a failure): no message will carry its tag, so
  // the frame is done with then, and otherwise once the host writes the message that carries it.
  const runTypedInput = (
    sendUserInput: NonNullable<PiApi["sendUserInput"]>,
    delivery: DispatchDelivery,
    replySessionID: string,
    turn: AcceptedUserTurn,
    content: string | readonly ContentBlock[],
    pictureAddresses: readonly string[]
  ): void => {
    const images =
      typeof content === "string"
        ? []
        : content.filter((block): block is ImageContent => block.type === "image");
    const noted = sessionID;
    if (images.length > 0) {
      typedPictures.admit(turn.messageId, {
        addresses: pictureAddresses,
        body: turn.body,
        images,
        sessionID: noted,
      });
    }
    noteTypedUserTurn(noted, turn.messageId);
    notedSessionIDs.add(noted);
    void Promise.resolve()
      .then(() =>
        sendUserInput(turn.body, {
          ...(turn.mode === "aside" && { deliverAs: "aside" as const }),
          tag: turn.messageId,
        })
      )
      .then(
        (result) => {
          const submitted = result.handled === "prompt" || result.handled === "skill";
          if (submitted) {
            // Every input handler has run: pictures never handed over never will be.
            typedPictures.inputPassed(turn.messageId);
            // In the host's queue from now on, unless its message was written already.
            unwritten.turnSubmitted(turn.messageId);
          } else {
            dropInjectedUserTurn(noted, turn.messageId);
            typedPictures.withdraw(turn.messageId);
            void unwritten.turnHandled(turn.messageId);
          }
          return typedInputReply(turn.body, result);
        },
        (error: unknown) => {
          dropInjectedUserTurn(noted, turn.messageId);
          typedPictures.withdraw(turn.messageId);
          void unwritten.turnHandled(turn.messageId);
          return typedInputReply(turn.body, { error: messageFor(error) });
        }
      )
      .then(async (reply) => {
        // A `/model` or `/effort` has run by now: the viewer's header shows its result before the
        // reply that names it reaches the page.
        refreshAgentStreamSettings(sessionID);
        if (reply !== undefined) await postDispatchReply(delivery, { body: reply }, replySessionID);
      })
      .catch((error: unknown) => {
        logger.warn("envoy: the reply to a person's typed input did not reach Dispatch", {
          attempt: delivery.attempt,
          error: messageFor(error),
          messageID: delivery.id,
        });
      });
  };

  // The user turn a person's direct Send or Aside becomes (`src/dispatch-user-turn.ts`): only one
  // whose acceptance by this session Dispatch records, as the body and mode Dispatch answers. The
  // attempt is recorded as handled before Dispatch is asked, so whatever this frame becomes - a
  // turn, or the card every refusal, error and timeout keeps, a broken Dispatch configuration
  // included - no later frame naming the attempt is accepted: a replay, or one forged inside the
  // accept's minute for a Send that arrived as a card. Undefined for every frame that is not a
  // turn; the caller then delivers today's card, which is the delivery, and posts nothing.
  //
  // A forged frame can name an attempt before Dispatch's frame does and so spend it; what that
  // costs is `packages/pi-envoy/AGENTS.md`'s (the record's limit). Do not skip the record when
  // the accept answers 404: the route answers 404 for a transient database error too, and a frame
  // another subscription's pump carded while that accept was in flight would be forgotten, so a
  // Send that arrived as a card could become a turn on a later forged frame.
  const acceptedUserTurn = async (
    rendered: RenderInboundResult,
    acceptedSessionID: string
  ): Promise<
    { readonly turn: AcceptedUserTurn; readonly delivery: DispatchDelivery } | undefined
  > => {
    if (!isUserTurnCandidate(rendered)) return undefined;
    const { delivery } = rendered;
    const key = handledAttemptKey(delivery.id, delivery.attempt);
    if (handledDispatchAttempts.has(key)) {
      logger.warn(
        "envoy: a frame names a Dispatch attempt this session already delivered; it arrives as a card",
        { attempt: delivery.attempt, messageID: delivery.id }
      );
      return undefined;
    }
    handledDispatchAttempts.add(key);
    pi.appendEntry(HANDLED_ATTEMPT_ENTRY, { attempt: delivery.attempt, message_id: delivery.id });
    try {
      const dispatch = timedDispatchClient(USER_TURN_ACCEPT_TIMEOUT_MS);
      if (dispatch === undefined) return undefined;
      const accepted = await dispatch.acceptMessageDelivery(delivery.id, delivery.attempt, {
        actor: { id: acceptedSessionID, kind: "session" },
      });
      const turn = turnFromAccept(accepted);
      if (turn === undefined) {
        logger.warn(
          "envoy: Dispatch accepted a direct message without the stored body and mode to inject; it arrives as a card",
          { attempt: delivery.attempt, messageID: delivery.id }
        );
        return undefined;
      }
      return { delivery, turn };
    } catch (error) {
      logger.warn(
        "envoy: Dispatch did not accept a direct message as this session's turn; it arrives as a card",
        { attempt: delivery.attempt, messageID: delivery.id, error: messageFor(error) }
      );
      return undefined;
    }
  };

  // `topic` is the subscription the frame came on, `subject` the subject it was published to, and
  // `receivedAt` when the subscription yielded it (`readArrivals`): a cursor that started after
  // that may stand past it (`liveFailed`), however long the frames before it took to deliver.
  // `relayed` is the relay frame (consumer delivery) a frame came as; a core frame has none. A
  // relay frame is answered `done` once it is written, or skipped, suppressed, a repeat, or
  // rejected (after the rejection reply is posted), and `nak` once the host dropped it unwritten.
  // A BTW's side turn and every Dispatch reply run off the caller (`offWorker`), so the frames
  // after one never wait on a model call or a reply.
  const deliver = async (
    topic: string,
    subject: string,
    raw: string,
    reply: string,
    receivedAt: number,
    relayed?: ConsumerFrame
  ): Promise<void> => {
    const rendered = renderInbound(raw, sessionID, subject);
    // Acknowledge first. The listener waits two seconds for this receipt: its
    // publish route, which forwards a role message or an ad-hoc topic's copy
    // here, forwards again until a receipt arrives within its four-second window
    // and answers a miss to the sender as an error, which the daemon answers by
    // re-sending the message; its role lane, for a role topic published straight
    // onto NATS, reports a miss as a failed delivery (LEGION-101). Everything
    // below — the inbox update, a Dispatch round trip, the injection — can
    // outlast that window under load. A frame renderInbound throws on never
    // reaches this line, so an undecodable frame is still not acknowledged. A
    // receipt that cannot be published is logged and delivery goes on: the
    // message must not be lost locally because the acknowledgement was.
    const directSubject = agentSubject(sessionID);
    if (
      expectsLaneReceipt({
        subject,
        directSubject,
        envelopeTopic: rendered.envelope?.topic,
        reply,
      })
    ) {
      try {
        (await ensureConnection()).publish(reply);
      } catch (error) {
        console.warn(
          `[envoy] failed to acknowledge envelope ${rendered.envelope?.event_id ?? "unknown"}`,
          error
        );
      }
    }
    // The listener relays this subscription's subject from the session's consumer
    // (`relaysCoreTopic`): the relay's frame is the one shown, and this core copy answered only its
    // lane receipt above.
    if (relayed === undefined && relaysCoreTopic(topic)) return;
    // A human unsubscribed one of our topics: drop it locally too, the same
    // way envoy_unsubscribe does, so the resubscribe-on-drop recovery path
    // below does not undo the human's action a few seconds later. The event
    // also reaches the issue's own topic (every subscriber, not just the
    // removed session), so this only fires for a removal naming us.
    let removed = false;
    for (const topic of subscriptionRemovedTopics(raw, sessionID) ?? []) {
      if (closeIntentionally(topic)) removed = true;
    }
    if (removed) {
      void replayFollowedChanged();
      // The listener's consumer still relays the session's topics: registered again without this
      // one, the relay stops taking it at once (`setFollowed`) and the listener's filter drops it.
      if (relay?.sessionID === sessionID && relay.delivery.relaying()) {
        void Promise.resolve()
          .then(registerSession)
          .catch(() => undefined);
      }
    }
    const dedupeIdentity = ClaimIdentitySchema.safeParse(rendered.envelope);
    const identity = dedupeIdentity.success ? dedupeIdentity.data : undefined;
    const held = rendered.skip ? undefined : delivered.claim(identity);
    if (held === undefined) {
      // Skipped for this session, or a copy of a frame this process shows or has shown: nothing
      // waits on a relay frame's write.
      relayed?.done();
      return;
    }
    // A relay frame's claim answers the listener as the claim ends: `done` once it settles (the
    // host wrote the frame, or handled it with nothing to write, the ledger recording it first),
    // `nak` once it is released with nothing recorded (the host dropped the frame unwritten: a
    // session change, an interrupt that withdrew a person's turn), so the listener sends it again.
    // A release while this call still runs answers only once the call returns: a throw is the
    // relay's own to count (its failure notice comes from `createConsumerDelivery`).
    let handing = true;
    let releasedWhileHanding = false;
    const claim: DeliveryClaim =
      relayed === undefined
        ? held
        : {
            settle: () => {
              held.settle();
              relayed.done();
              relay?.delivery.persist();
            },
            release: () => {
              held.release();
              if (handing) releasedWhileHanding = true;
              else relayed.nak();
            },
          };
    // A repeat of a frame this session was shown before a restart, which the in-memory claim
    // cannot know of: the replay ledger, rebuilt from the transcript, holds it.
    if (identity !== undefined && replay?.delivered(identity) === true) {
      claim.settle();
      return;
    }
    const envelope = rendered.envelope;
    if (envelope !== undefined) noteInbox(envelope);
    // The session the frame is delivered in, whose transcript records what became of it.
    const deliveredIn = sessionID;
    // The host wrote the frame, or handled it without writing anything: the ledger records it,
    // so no sweep shows the stream's copy again, here or after a restart, and the claim is
    // settled, so a sweep stops waiting on it. The record reaches the transcript of the session
    // it was delivered in even while that session shuts down (`recordDeliveredFor`): a BTW
    // answered as the session stops must not come back from a sweep after its resume.
    const taken = async (): Promise<void> => {
      if (identity !== undefined) await replay?.recordDeliveredFor(deliveredIn, identity);
      claim.settle();
    };
    // An error reply means the agent was not handed the frame: Dispatch records the attempt
    // failed, and a same-mode re-send of it must reach this session again, so the claim is
    // released before the reply goes out. A BTW side turn adds nothing to the transcript, so
    // running it again for that re-send repeats no work the session kept. A relay frame is done
    // once the reply is posted: the re-send is a new frame.
    const refuse = async (delivery: DispatchDelivery, error: string): Promise<void> => {
      held.release();
      await postDispatchReply(delivery, { error });
      relayed?.done();
    };
    // Runs a side turn or a Dispatch reply off the caller (a pump or the relay's queue), so the
    // next frame is handed without waiting on it. One that throws is handled as a delivery that
    // throws: its claim is released, and a core frame no sweep will show again gets its notice; a
    // relay frame is done (a BTW is in the ledger already, so its redelivery would be dropped).
    const offWorker = (work: () => Promise<void>): void => {
      relayed?.release();
      void work().catch((error: unknown) => {
        held.release();
        console.warn(
          `[envoy] failed to deliver envelope ${envelope?.event_id ?? "unknown"}`,
          error
        );
        if (relayed !== undefined) relayed.done();
        else if (envelope !== undefined)
          void replay?.liveFailed(topic, envelope, error, receivedAt);
      });
    };
    try {
      if (rendered.rejectedDelivery !== undefined) {
        console.warn(
          `[envoy] rejecting malformed Dispatch targeted delivery ${rendered.rejectedDelivery.id}`
        );
        const rejected = rendered.rejectedDelivery;
        offWorker(() => refuse(rejected, "Invalid Dispatch targeted delivery frame"));
      } else if (rendered.malformedDelivery === true) {
        console.warn(
          "[envoy] dropping malformed Dispatch targeted delivery without a reply address"
        );
        claim.settle();
      } else if (rendered.delivery?.mode === "btw" && relayed?.late === true) {
        // A BTW that arrives late (delivered before, or stored before this connection's epoch
        // began) never starts a side turn: it is one inert card, as a replayed BTW is, written to
        // the transcript and done then. Its answer, if any, is on Dispatch.
        noteBtw(identity?.event_id);
        unwritten.sendCard(
          `${replayedText(rendered.content, relayed.storedAt)}\n${REPLAYED_BTW_NOTE}`,
          "inert",
          identity,
          claim
        );
      } else if (rendered.delivery?.mode === "btw") {
        // A BTW is answered on Dispatch or refused there, never shown as a card: the stream's
        // copy must never come back from a sweep as a card that starts a turn. The ledger
        // records it by its event id alone before the side turn and before any reply is posted,
        // so no sweep finds it unrecorded whatever happens after (a reply that fails, a sweep
        // while the POST is in flight, the process ending mid-turn or mid-POST), and the record
        // reaches the transcript of the session it was delivered in even while that session
        // shuts down (`recordDeliveredFor`), so a resume of it passes the stream's copy over too.
        // A same-mode Retry carries a new event id under the same dedupe key, and still reaches
        // the session once a refusal released the claim. The side turn and the reply run off the
        // caller: the next frame does not wait for the model.
        noteBtw(identity?.event_id);
        if (identity?.event_id !== undefined) {
          await replay?.recordDeliveredFor(deliveredIn, { event_id: identity.event_id });
        }
        const delivery = rendered.delivery;
        const answer = sideTurn(activeSessionContext);
        if (shuttingDown) {
          // A session that is shutting down starts no model call, but a frame can still drain in.
          offWorker(() => refuse(delivery, "This OMP session is shutting down"));
        } else if (answer === undefined) {
          offWorker(() => refuse(delivery, "This OMP host does not support BTW delivery"));
        } else {
          offWorker(async () => {
            try {
              const reply = await answer({ prompt: delivery.body });
              await postDispatchReply(delivery, { body: reply.replyText });
              await taken();
            } catch (error) {
              await refuse(delivery, messageFor(error));
            }
          });
        }
      } else {
        const acceptedSessionID = sessionID;
        const accepted = await acceptedUserTurn(rendered, acceptedSessionID);
        // The pictures the delivered text embeds reach the model beside it, read with this
        // session's own Dispatch bearer, each picture once in this session. They count as shown
        // only once the host took the send: one that throws releases the claim, and the delivery
        // Dispatch retries must carry them again. `acceptedSessionID` is only the accept's and
        // its replies': what to name is read from the set of the session live once the accept
        // answers, which a switch during it changes, and what was shown joins the set of the
        // session live once the pictures load, which the send goes into.
        const shown = shownPictures(sessionID);
        if (accepted === undefined) {
          const card = await withDeliveredPictures(
            rendered.content,
            rendered.pictures ?? [],
            () => timedDispatchClient(DELIVERY_PICTURES_TIMEOUT_MS),
            true,
            shown
          );
          // Settled once the host writes the card, or at once when nothing waits on that. How
          // the card goes, and why, is the wake's (`handCard`, `../src/card-wake`).
          unwritten.sendCard(
            card.content,
            rendered.delivery?.mode === "aside" ? "aside" : "steer",
            identity,
            claim
          );
          markPicturesShown(shownPictures(sessionID), card.shown);
        } else {
          const { turn } = accepted;
          // Sent exactly as Enter, or an aside, at the terminal sends it, the person's pictures
          // beside their text.
          const delivery = await withDeliveredPictures(
            turn.body,
            pictureAddresses(turn.body),
            () => timedDispatchClient(DELIVERY_PICTURES_TIMEOUT_MS),
            false,
            shown
          );
          const sendUserInput = pi.sendUserInput;
          if (sendUserInput === undefined) {
            const refusal = typedInputReply(turn.body, { hostVersion: VERSION });
            if (refusal !== undefined) {
              // A host that cannot run typed input would hand the command to the model as words.
              offWorker(async () => {
                await postDispatchReply(accepted.delivery, { body: refusal }, acceptedSessionID);
                await taken();
              });
            } else {
              // Noted right before the send, after the pictures load: a run that ends while they
              // load clears every text note (endInjectedUserTurns), and this turn must still be found.
              const injectionSessionID = sessionID;
              noteInjectedUserTurn(injectionSessionID, turn.body, turn.messageId);
              notedSessionIDs.add(injectionSessionID);
              pi.sendUserMessage(
                delivery.content,
                turn.mode === "aside" ? { deliverAs: "aside" } : undefined
              );
              markPicturesShown(shownPictures(injectionSessionID), delivery.shown);
              // Done once the host writes the user message its text is found as, or at the
              // run's end, which stops that search (`endInjectedUserTurns`).
              unwritten.awaitTurn(turn.messageId, {
                sessionID: injectionSessionID,
                identity,
                claim,
                via: "text",
              });
            }
          } else {
            unwritten.awaitTurn(turn.messageId, { sessionID, identity, claim, via: "typed" });
            runTypedInput(
              sendUserInput,
              accepted.delivery,
              acceptedSessionID,
              turn,
              delivery.content,
              delivery.shown
            );
          }
        }
      }
    } catch (error) {
      handing = false;
      held.release();
      console.warn(`[envoy] failed to deliver envelope ${envelope?.event_id ?? "unknown"}`, error);
      // A frame no sweep will show again (a role forward, already acknowledged; one on a
      // subscription the replay does not read: refused, not started, or the listener's replay
      // failing) gets its notice at once. The stream's copy of any other waits for a sweep. A
      // relay frame's failure is the relay's to count: it comes again, or gets its notice.
      if (relayed === undefined && envelope !== undefined) {
        void replay?.liveFailed(topic, envelope, error, receivedAt);
      }
      throw error;
    }
    handing = false;
    if (releasedWhileHanding) relayed?.nak();
  };

  const RESUBSCRIBE_DELAY_MS = Number(process.env.ENVOY_RESUBSCRIBE_DELAY_MS ?? "") || 5_000;
  let shuttingDown = false;

  // Topics this session closed on purpose: the session-switch rebind and the
  // envoy_unsubscribe tool. The pump's recovery path below exists for
  // connections that died under us, and it cannot tell those two cases apart on
  // its own, because an explicit unsubscribe() ends the async iterator exactly
  // the way a dropped connection does. Without this marker the recovery path
  // resurrects the topic seconds later, so a switched-away session keeps
  // receiving the previous session's traffic and envoy_unsubscribe silently
  // undoes itself.
  const intentionallyClosed = new Set<string>();

  // Topics whose pump is sitting in the resubscribe delay. Such a topic has no
  // entry in `subscriptions`, so this is the only way to know a deliberate close
  // still has something to suppress.
  const awaitingRetry = new Set<string>();

  // Drop a topic we no longer want. Every marker set here is consumed again by
  // the pump end-path, the retry callback, or the next subscribe(), so the set
  // cannot accumulate topics that were never live in the first place.
  const closeIntentionally = (topic: string): boolean => {
    const subscription = subscriptions.get(topic);
    if (subscription === undefined) {
      if (!awaitingRetry.has(topic)) return false;
      intentionallyClosed.add(topic);
      return true;
    }
    intentionallyClosed.add(topic);
    subscription.unsubscribe();
    subscriptions.delete(topic);
    return true;
  };

  // The sessions whose agent subject still addresses the host's current session: that session and
  // the ones it continues (a fork's or branch's parent), the announced one included while the host
  // has moved on and no pass has yet seen where.
  const lineageOf = (id: string): readonly string[] => {
    const lineage = continuedBy(id);
    return announced !== undefined &&
      announced.from !== id &&
      carriesSession(announced.reason) &&
      !lineage.includes(announced.from)
      ? [...lineage, announced.from]
      : lineage;
  };

  // Whether a frame on the subscription `topic` is addressed to a session the host has replaced
  // (a `/new` or resume), arriving before a pass closes that session's agent subject. Injected
  // now, the frame would land in the new session's transcript; it stays undelivered and
  // unacknowledged, as on the closed subject (a role lane counts a failed delivery and the daemon
  // re-sends to the role's holder, a BTW's sender sees its wait time out). A fork or branch carries
  // the session on, so its parent's frames arrive. A subject the role holds is addressed to the
  // role, not a session: its notices arrive until a pass ends the role, since the controller, the
  // one plugin that opens such subjects, claims its role again for the session its pane is left on.
  const replacedSessionTopic = (topic: string): boolean => {
    if (!topic.startsWith(agentSubject(""))) return false;
    const host = hostSessionID();
    return host !== "" && !lineageOf(host).some((id) => topic === agentSubject(id));
  };

  const pump = async (topic: string, subscription: Subscription): Promise<void> => {
    try {
      for await (const { message, receivedAt } of readArrivals(subscription)) {
        // Left undelivered here, the frame stays in the stream: when this session id is resumed,
        // its first sweep shows it.
        if (replacedSessionTopic(topic)) {
          logger.warn("envoy: frame for a session a switch replaced left undelivered", {
            subject: message.subject,
          });
          continue;
        }
        try {
          await deliver(
            topic,
            message.subject,
            codec.decode(message.data),
            message.reply ?? "",
            receivedAt
          );
        } catch {
          // A failed injection is logged by deliver and must not tear down the subscription; the
          // next sweep shows the stream's copy, or deliver reported one no sweep can show. A BTW
          // is neither: it is recorded before its side turn, so no sweep shows it, and its outcome
          // is Dispatch's record (a reply that failed leaves the attempt unanswered there, and a
          // Retry runs again).
        }
      }
    } catch {
      // Iterator failure falls through to the resubscribe path below.
    }
    // Only retire our own generation: a later subscribe() for this topic may
    // already own the map entry.
    if (subscriptions.get(topic) === subscription) subscriptions.delete(topic);
    if (shuttingDown) return;
    // A close we asked for is not an outage. Consume the marker so a later,
    // genuine death of this same topic still recovers.
    if (intentionallyClosed.delete(topic)) return;
    // Otherwise the iterator only ended because the connection was closed or
    // errored out from under nats.js's own reconnect handling. Re-establish
    // rather than staying silently deaf while the HTTP registration heartbeat
    // keeps the session looking healthy in the registry. Whether the topic served a role, and how
    // many roles had ended, are taken once, here: every retry of the chain, a failed reconnect's
    // included, compares against them, so a role that ends mid-chain keeps the topic closed.
    const snapshot = registrationState.roleSnapshot(topic);
    const retry = (delayMs: number): void => {
      awaitingRetry.add(topic);
      setTimeout(() => {
        awaitingRetry.delete(topic);
        // A subscribe() during the delay already cleared the marker and owns the
        // topic, so there is nothing to do here.
        if (shuttingDown || subscriptions.has(topic)) return;
        // A close that landed during the delay leaves the marker for us instead
        // of the pump end-path. Consume it here so it cannot outlive the timer.
        if (intentionallyClosed.delete(topic)) return;
        // Reopened, the topic is live again, but what was published to it while it was down is
        // only in the stream: a sweep shows it.
        void subscribeUnlessRoleEnds(topic, snapshot).then(
          (wanted) => {
            if (wanted) triggerReplay();
          },
          () => retry(NATS_RETRY_INTERVAL_MS)
        );
      }, delayMs);
    };
    retry(RESUBSCRIBE_DELAY_MS);
  };

  // Opens every subject of `topic` not already open; answers whether any were new. A queued task
  // subscribes through its handle, which hands in its own `connect`: a connection its task
  // outran refuses there, before the first subject opens, so nothing a newer pass has since
  // settled is reopened behind its back.
  const subscribe = async (
    topic: string,
    connect: () => Promise<NatsConnection> = ensureConnection
  ): Promise<boolean> => {
    const subjects = expandSubscriptionTopics([topic]).filter(
      (candidate) => !subscriptions.has(candidate)
    );
    if (subjects.length === 0) return false;
    const activeConnection = await connect();
    for (const subject of subjects) {
      const subscription = activeConnection.subscribe(subject);
      // A fresh subscription supersedes any earlier deliberate close of this topic.
      intentionallyClosed.delete(subject);
      subscriptions.set(subject, subscription);
      void pump(subject, subscription);
    }
    return true;
  };

  const noteInbox = (envelope: NonNullable<RenderInboundResult["envelope"]>): void => {
    inbox.unshift({
      event_id: envelope.event_id ?? "unknown",
      at: inboundTimestamp(envelope.issued_at),
      from: senderLabel(envelope),
      summary: envelope.payload_summary ?? "unknown",
    });
    if (inbox.length > 50) inbox.pop();
  };

  // What the session follows for replay (`replayFollowedSubjects`): what `registerSession`
  // registers, its own agent subject and every open subscription not held for a role, with a
  // subscription still in its resubscribe delay, which the session still follows (dropped, its
  // cursor would restart past what the outage cost).
  const followedSubjects = (): string[] => {
    if (sessionID === "") return [];
    const open = [...subscriptions.keys(), ...awaitingRetry].filter(
      (subject) => !intentionallyClosed.has(subject)
    );
    return replayFollowedSubjects(agentSubject(sessionID), open, registrationState.isRoleBound);
  };

  // Replays a stored frame: a card and nothing else. A targeted frame (BTW, Aside, Steer, a
  // person's Send) runs no side turn, asks Dispatch to accept no user turn and posts no reply: a
  // person's input run late could act on an instruction they no longer mean, so the session is
  // shown it and decides. A removal closes a topic and takes it out of the listener's registry
  // only when it was stored after the session started following that topic
  // (`closeReplayedRemoval`). The card is `queued` until the host writes it (`showReplayCard`).
  const showReplayed = async (frame: ReplayFrame): Promise<ReplayDeliveryOutcome> => {
    const rendered = renderInbound(frame.data, sessionID, frame.subject);
    if (replay !== undefined) {
      await closeReplayedRemoval(frame, {
        sessionID,
        engine: replay,
        close: closeIntentionally,
        unregister: async (topics) => {
          await active?.client
            .unsubscribe({ sessionID, topics })
            .catch((error: unknown) =>
              logger.warn(
                "envoy: a replayed subscription removal was not taken out of the registry",
                { topics, error: messageFor(error) }
              )
            );
        },
        followedChanged: replayFollowedChanged,
      });
    }
    if (replaySuppressed(rendered)) return "suppressed";
    if (rendered.envelope !== undefined) noteInbox(rendered.envelope);
    const shown = replayedText(rendered.content, frame.time);
    // A BTW that reaches replay, whatever became of it live (answered, refused, or never received),
    // is one inert notice: a card that starts no turn, so a stale side question never runs as a
    // turn. Its answer, if any, is on Dispatch.
    if (rendered.delivery?.mode === "btw") {
      noteBtw(rendered.envelope?.event_id);
      return showReplayCard(`${shown}\n${REPLAYED_BTW_NOTE}`, frame.envelope, "inert");
    }
    return showReplayCard(shown, frame.envelope);
  };

  // The session this instance and the host are both on: an entry appended now lands in its
  // transcript.
  const transcriptSessionID = (): string | undefined => {
    const host = hostSessionID();
    return host !== "" && host === sessionID ? host : undefined;
  };

  // Tells the session a replay or relay notice: one about the listener as a warning, one about the
  // stream as a card held until the host is settled (`UnwrittenFrames.sendNotice`), which the
  // transcript records held, so a restart holds it again.
  const tellReplayNotice = (notice: ReplayNotice): void => {
    logger.warn(`envoy: ${notice.message}`);
    routeReplayNotice(notice, {
      warn: (message) => activeSessionContext?.ui.notify(`envoy: ${message}`, "warning"),
      card: (content) => unwritten.sendNotice(content),
    });
  };

  // Shows the session each message the notification stream stored for it while its subscriptions
  // were down, once (`@legion/envoy-client/replay`). Its sessions are this instance's: work started
  // for one session stops once the host, or this instance, is on another.
  const replay: ReplayEngine | undefined =
    active === undefined || !inbound
      ? undefined
      : createReplayEngine({
          host: {
            sessionID: () => (shuttingDown ? undefined : transcriptSessionID()),
            // A session shutting down starts no replay work, but the host still writes its
            // transcript, so a refused BTW's record still reaches it (`recordDeliveredFor`).
            writeSessionID: transcriptSessionID,
            followedSubjects,
            deliver: showReplayed,
            showFailureNotice: async (notice) =>
              showReplayCard(
                envoyNoticeText(failureNoticeFields(notice)),
                notice.envelope,
                btwEvents.has(notice.eventId) ? "inert" : "steer"
              ),
            failureNoticeFailed: (notice, error) => {
              logger.warn("envoy: a message this session was not shown could not be reported", {
                eventID: notice.eventId,
                error: messageFor(error),
              });
              activeSessionContext?.ui.notify(
                `envoy: message ${notice.eventId} from ${notice.sender} was not shown, nor was the notice naming it (${messageFor(error)})`,
                "warning"
              );
            },
            notify: tellReplayNotice,
            log: (message, error) =>
              logger.warn(
                `envoy: ${message}`,
                error === undefined ? undefined : { error: messageFor(error) }
              ),
            write: (change) => {
              for (const entry of change.delivered) pi.appendEntry(REPLAY_DELIVERED_ENTRY, entry);
              if (change.state !== undefined) pi.appendEntry(REPLAY_STATE_ENTRY, change.state);
            },
          },
          transport: active.client,
          dedupe: delivered,
        });

  // A sweep reads the cursors of a session whose agent subject and recovered interests a pass
  // opened: before that, a topic restored from the transcript is not yet open
  // again, and a sweep would drop its cursor. Each sweep runs on the engine's own chain, never in
  // the passes' or the role moves' queues.
  const replayOpen = (): boolean =>
    !shuttingDown && sessionID !== "" && registrationState.inboundOpenSession() === sessionID;
  const triggerReplay = (): void => {
    if (replay !== undefined && replayOpen()) void replay.sweep();
  };
  const replayFollowedChanged = async (): Promise<void> => {
    if (replay !== undefined && replayOpen()) await replay.followedChanged();
  };

  // Consumer delivery (`@legion/envoy-client/consumer-delivery`). While a pass holds the session's
  // agent subject open, it holds the session's relay subject open too, subscribed before any
  // registration, and every registration then asks the listener for consumer delivery. Answered
  // `consumer`, the listener relays the session's durable consumer on that subject: its agent
  // subject and every followed topic the stream stores, each frame handed to `deliver` in stream
  // order and answered `done` once written. The core copies of those subjects are then shown
  // nothing (`relays`), the agent subject's lane receipt still answered, and the replay
  // engine makes no `/v1/replay` call. Answered `core`, or not at all, the core path runs as before.
  interface Relay {
    readonly sessionID: string;
    readonly delivery: ConsumerDelivery;
    subscription: Subscription | undefined;
  }
  let relay: Relay | undefined;

  const relaysCoreTopic = (topic: string): boolean =>
    relay?.sessionID === sessionID && relay.delivery.relays(topic);

  // A relay frame for a session the host is no longer on (a switch, before the next pass closes
  // its relay), or one that drains in while the session shuts down, is one the session cannot
  // take: `nak`, so the listener sends it again to whoever takes the session next.
  const deliverRelayed = async (delivery: ConsumerDelivery, frame: ConsumerFrame) => {
    const owner = relay;
    if (shuttingDown || owner?.delivery !== delivery || !sessionOpen(owner.sessionID)) {
      frame.nak();
      return;
    }
    await deliver(frame.subject, frame.subject, frame.data, "", Date.now(), frame);
  };

  // The relay of session `id`, its snapshot restored from `entries` (before any `setFollowed`),
  // and every later change of it appended to the transcript of the session it is for
  // (`CONSUMER_STATE_ENTRY`): after a frame is written, after the followed topics change, and
  // after a registration answer moves the stream. The follows map goes with an entry only when it
  // changed, so a written frame costs one resume point beside the frame's own entries; what the
  // relay restored is stored already and is not appended again.
  const createRelay = (id: string, entries: readonly unknown[], ledger: ReplayEngine): Relay => {
    let persistedFollows = "";
    const delivery: ConsumerDelivery = createConsumerDelivery({
      sessionID: id,
      ledger,
      host: {
        deliver: (frame) => deliverRelayed(delivery, frame),
        gap: tellReplayNotice,
        log: (message, error) =>
          logger.warn(
            `envoy: ${message}`,
            error === undefined ? undefined : { error: messageFor(error) }
          ),
      },
      publish: (reply, body) => {
        if (connection === undefined) throw new Error("no NATS connection");
        connection.publish(reply, codec.encode(body));
      },
      persist: (snapshot) => {
        // Another session's transcript is not this relay's to write to: the change is appended
        // once the host records in this session again.
        if (relay?.delivery !== delivery || transcriptSessionID() !== id) return false;
        const follows = JSON.stringify(snapshot.follows);
        pi.appendEntry(CONSUMER_STATE_ENTRY, {
          session_id: id,
          ...(snapshot.resume === undefined ? {} : { resume: snapshot.resume }),
          ...(follows === persistedFollows ? {} : { follows: snapshot.follows }),
        });
        persistedFollows = follows;
        return true;
      },
    });
    const restored = consumerSnapshot(entries, id);
    if (restored !== undefined) delivery.restore(restored);
    persistedFollows = JSON.stringify(delivery.snapshot().follows);
    return { sessionID: id, delivery, subscription: undefined };
  };

  // Subscribes the relay subject, unless it is open: a subscription on a connection the listener
  // has not been told of begins a new epoch, which the next registration carries. A queued task
  // hands in its own fenced `connect`, so a task the host outran opens no relay subject: the
  // acquisition refuses and this call throws out of the task.
  const subscribeRelay = async (
    owner: Relay,
    connect: () => Promise<NatsConnection> = ensureConnection
  ): Promise<void> => {
    if (owner.subscription !== undefined) return;
    const connected = await connect();
    if (relay !== owner || owner.subscription !== undefined) return;
    const subscription = connected.subscribe(owner.delivery.subject);
    owner.subscription = subscription;
    owner.delivery.beginEpoch();
    void pumpRelay(owner, subscription);
  };

  // Opens session `id`'s relay subject, closing another session's relay first.
  const openRelay = async (
    context: SessionContext,
    id: string,
    connect: () => Promise<NatsConnection>
  ): Promise<void> => {
    if (replay === undefined) return;
    if (relay?.sessionID !== id) {
      closeRelay();
      relay = createRelay(id, context.sessionManager.getEntries(), replay);
    }
    await subscribeRelay(relay, connect);
  };

  // The session left (or shut down): its unsettled frames are answered `nak`, the replay engine
  // leaves consumer mode, and its relay subject is closed. The next registration answers the new
  // session's mode.
  const closeRelay = (): void => {
    const owner = relay;
    if (owner === undefined) return;
    relay = undefined;
    owner.delivery.close();
    const subscription = owner.subscription;
    owner.subscription = undefined;
    subscription?.unsubscribe();
  };

  // Hands each relay message to the session's consumer delivery, which answers `received` at once
  // and queues the frame in stream order. A subscription that ends under the session (its
  // connection closed) is opened again after the resubscribe delay, on a new epoch, and
  // registered, so the listener restarts the relay at its ack floor.
  const pumpRelay = async (owner: Relay, subscription: Subscription): Promise<void> => {
    try {
      for await (const message of subscription) {
        owner.delivery.receive(message);
      }
    } catch {
      // Iterator failure falls through to the resubscribe path below.
    }
    if (owner.subscription !== subscription) return;
    owner.subscription = undefined;
    if (!shuttingDown && relay === owner) retryRelay(owner, RESUBSCRIBE_DELAY_MS);
  };

  const retryRelay = (owner: Relay, delayMs: number): void => {
    setTimeout(() => {
      if (shuttingDown || relay !== owner || owner.subscription !== undefined) return;
      subscribeRelay(owner).then(
        () =>
          void Promise.resolve()
            .then(registerSession)
            .catch(() => undefined),
        () => retryRelay(owner, NATS_RETRY_INTERVAL_MS)
      );
    }, delayMs);
  };

  // nats.js reconnected: the relay subscription is the same object, but the listener must restart
  // the relay at its ack floor, so a new epoch is registered at once.
  const renewRelayEpoch = (): void => {
    if (shuttingDown || relay?.subscription === undefined || relay.sessionID !== sessionID) return;
    relay.delivery.beginEpoch();
    void Promise.resolve()
      .then(registerSession)
      .catch(() => undefined);
  };

  // What a registration's answer says of consumer delivery, for the session it registered with
  // the topics it carried, and the relay it asked for. One that asked nothing (no relay subject
  // open) relays nothing, whatever the listener answered, so the relay takes none of the topics
  // and the replay engine sweeps again (`registeredWithoutRelay`, the module that owns the
  // ledger's consumer mode). An answer for a session this instance has left, or for a relay since
  // closed, changes nothing.
  const takeDeliveryAnswer = (
    registeredSession: string,
    topics: readonly string[],
    asked: Relay | undefined,
    result: SubscribeResult
  ): void => {
    if (sessionID !== registeredSession || (asked !== undefined && relay !== asked)) return;
    if (asked !== undefined) {
      asked.delivery.answered(result.delivery, topics);
      return;
    }
    if (replay === undefined) return;
    const open = relay?.sessionID === registeredSession ? relay : undefined;
    registeredWithoutRelay(replay, open?.delivery, topics);
  };

  // The live agent conversation stream (LEGION-232). This session's own turns, tool calls and
  // streamed output become frames on a core-NATS subject the notification stream does not
  // capture, so nothing is retained by the bus; the Dispatch relay forwards them to a human
  // watching the session and writes none of it down. The publisher stays silent unless a viewer
  // is attached, so a session nobody has open puts no conversation on the wire at all.
  const agentStream = new AgentStreamPublisher({
    now: () => Date.now(),
    publish: (subject, payload) => connection?.publish(subject, codec.encode(payload)),
  });
  // The control subscription this session currently holds, and the id it was opened for.
  let agentStreamControl: Subscription | undefined;
  let agentStreamControlSession = "";
  let agentStreamCommandsRefreshedOnWatchAt = -Infinity;
  let agentStreamViewerAttached = false;

  // The slash commands a viewer can send this session, in its terminal picker's order
  // (`#rebuildSlashCommandAutocomplete` in the host's `modes/interactive-mode.ts`): the host's
  // built-ins, each marked when only the terminal runs it; its extension, custom and skill commands
  // (`pi.getCommands`); then the file commands and prompt templates the live session expands in
  // `prompt()`, read from the host's own `AgentSession` (`slashCommands`, `promptTemplates`); a
  // host that lists no live session under this id (`liveAgentSession`) lists none of the two.
  // The host runs a built-in by its name or any alias before anything else, so those names
  // are reserved first; a name repeated among the rest runs its first source, and the publisher
  // keeps the first. Only a host that can run typed input from an extension offers any; one that
  // cannot sends no list, and its viewer offers no completion.
  const refreshAgentStreamCommands = (session: string): void => {
    if (typeof pi.sendUserInput !== "function") return;
    const builtins = hostBuiltinCommands?.() ?? [];
    const reserved = new Set(builtins.flatMap((builtin) => [builtin.name, ...builtin.aliases]));
    const live = liveAgentSession(session);
    const commands: AgentStreamCommand[] = builtins.map((builtin) => ({
      description: builtin.description,
      name: builtin.name,
      source: "builtin",
      ...(builtin.terminalOnly && { terminalOnly: true as const }),
    }));
    for (const command of [
      ...(pi.getCommands?.() ?? []),
      ...[...(live?.slashCommands ?? []), ...(live?.promptTemplates ?? [])].map((file) => ({
        description: file.description,
        name: file.name,
        source: "prompt" as const,
      })),
    ]) {
      if (reserved.has(command.name)) continue;
      const description =
        typeof command.description === "string" ? { description: command.description } : {};
      commands.push({
        name: command.name,
        source: command.source,
        ...description,
      });
    }
    agentStream.setCommands(agentStreamFramesSubject(session), commands);
  };

  // The model and thinking level a viewer's header shows (`liveSessionSettings`). The host fires no
  // event when either is set (`/model`, `/effort`, its terminal's selector), so they are re-read on
  // every control message, on each turn's start, on a retry's fallback to another model, and after
  // a person's typed input runs. The reads are in-memory getters and the publisher sends only a
  // change, so nothing throttles them. A getter that throws must not suppress the replay answer, a
  // turn or a typed input's reply, so it is logged and the last frame stands.
  const refreshAgentStreamSettings = (session: string): void => {
    if (session === "") return;
    try {
      agentStream.setSettings(
        agentStreamFramesSubject(session),
        liveSessionSettings(liveAgentSession(session))
      );
    } catch (error) {
      logger.warn("envoy: could not refresh agent stream settings", {
        error: messageFor(error),
        sessionID: session,
      });
    }
  };

  const pumpAgentStreamControl = async (
    subscription: Subscription,
    session: string
  ): Promise<void> => {
    try {
      for await (const message of subscription) {
        // Everything a control message can do runs inside this guard. Any client on the bus can
        // publish here, and an unhandled throw ends the `for await` — nats.js unsubscribes, and
        // the session goes deaf to watch and replay until the resubscribe delay below. A `null`
        // payload parses cleanly and then throws on `.type`; a reply over the server's
        // max_payload throws out of the publish.
        try {
          const parsed: unknown = JSON.parse(codec.decode(message.data));
          if (typeof parsed !== "object" || parsed === null) continue;
          const request = parsed as AgentStreamControlMessage;
          if (request.type !== "replay" && request.type !== "watch") continue;
          refreshAgentStreamSettings(session);
          const attached = agentStreamViewerAttached;
          if (
            request.type === "replay" ||
            !attached ||
            Date.now() - agentStreamCommandsRefreshedOnWatchAt >= AGENT_STREAM_WATCH_INTERVAL_MS
          ) {
            // A list is optional. A host getter or a third-party command with an invalid description
            // must not suppress the replay answer or leave this viewer unarmed.
            try {
              refreshAgentStreamCommands(session);
            } catch (error) {
              logger.warn("envoy: could not refresh agent stream commands", {
                error: messageFor(error),
                sessionID: session,
              });
            }
            if (request.type === "watch" && attached) {
              agentStreamCommandsRefreshedOnWatchAt = Date.now();
            }
          }
          // The answer is taken before this message arms anything, so a bare replay request
          // from a client that never attached a viewer gets an empty history, not the ring.
          const reply = message.reply;
          const answer =
            request.type === "replay" && reply !== undefined
              ? JSON.stringify(agentStream.replay(session))
              : undefined;
          // A viewer arming a session nobody was watching is sent the settings and commands as
          // they stand: what was read before it arrived was kept unpublished, and a page held open
          // while this session was relaunched gets no replay from this process (`publishState`).
          const arming = !agentStream.watched;
          agentStream.noteViewer();
          agentStreamViewerAttached = true;
          if (arming) agentStream.publishState(agentStreamFramesSubject(session));
          if (answer === undefined || reply === undefined) continue;
          connection?.publish(reply, codec.encode(answer));
        } catch (error) {
          logger.warn("envoy: agent stream control message failed", {
            error: messageFor(error),
            sessionID: session,
          });
        }
      }
    } catch {
      // A dead iterator is a dropped connection, handled by the resubscribe below.
    }
    if (agentStreamControl === subscription) agentStreamControl = undefined;
    if (shuttingDown || agentStreamControlSession !== session) return;
    setTimeout(() => {
      if (shuttingDown || agentStreamControlSession !== session) return;
      void watchAgentStream(session).catch(() => undefined);
    }, RESUBSCRIBE_DELAY_MS);
  };

  const watchAgentStream = async (
    session: string,
    connect: () => Promise<NatsConnection> = ensureConnection
  ): Promise<void> => {
    if (agentStreamControlSession === session && agentStreamControl !== undefined) return;
    agentStreamControl?.unsubscribe();
    agentStreamControl = undefined;
    // A different session under the same extension instance is a different conversation: the
    // ring still holds the previous one, and a viewer opening the new id must not be served it.
    if (agentStreamControlSession !== session) {
      agentStream.reset();
      agentStreamViewerAttached = false;
      agentStreamCommandsRefreshedOnWatchAt = -Infinity;
    }
    agentStreamControlSession = session;
    // A queued task connects through its handle: an acquisition its task outran refuses there,
    // and the task ends, so a newer pass's control subscription is never overwritten behind its
    // back.
    const connected = await connect();
    const subscription = connected.subscribe(agentStreamControlSubject(session));
    agentStreamControl = subscription;
    void pumpAgentStreamControl(subscription, session);
  };

  /** One of this session's messages, on its way to whoever has the session open, tagged with the
   *  Dispatch message a user message delivered when it matches a person's turn this extension sent
   *  in. Only the top-level instance publishes: a `task` subagent's instance never learns a session
   *  id, and its work reaches the viewer as the parent's tool call anyway. */
  const recordAgentStreamMessage = (message: unknown, streaming: boolean): void => {
    if (sessionID === "") return;
    agentStream.record(
      agentStreamFramesSubject(sessionID),
      message,
      streaming,
      matchInjectedUserTurn(sessionID, message)
    );
  };

  pi.on("message_start", async (event) => {
    // A tagged message starts in the live session's transcript, so its pictures join that set.
    const shown = typedPictures.started(event.message);
    if (shown !== undefined) markPicturesShown(shownPictures(sessionID), shown);
    recordAgentStreamMessage(event.message, true);
  });
  pi.on("message_update", async (event) => {
    recordAgentStreamMessage(event.message, true);
  });

  // The host writes a message to the transcript as it ends it: a card or a person's turn handed to
  // it, idle or taken by a turn after it streamed, is shown from here on (`UnwrittenFrames`). A
  // card the host appended also arms the wake's check (`../src/card-wake`).
  pi.on("message_end", async (event, context) => {
    const { message } = event;
    if (!shuttingDown) cardWake.messageEnd(message, context);
    recordAgentStreamMessage(message, false);
    await unwritten.written(message);
  });
  // The host asks whether any extension handles `session_before_compact`, which turns its
  // background compaction off, but never these two: handling them changes nothing it does.
  pi.on("session_compact", async (_event, context) => cardWake.noteIfIdle(context));
  pi.on("auto_compaction_end", async (event) => cardWake.autoCompactionEnd(event));
  pi.on("auto_retry_end", async (event) => cardWake.autoRetryEnd(event));

  // A turn runs on the model and level set when it starts, and a retry's fallback moves the session
  // onto another model: either can follow a change no event announced. A subagent's instance knows
  // no session id and publishes nothing.
  pi.on("turn_start", async () => {
    refreshAgentStreamSettings(sessionID);
  });
  pi.on("retry_fallback_applied", async () => {
    refreshAgentStreamSettings(sessionID);
  });

  // The host's own session id. Oh My Pi moves its one session manager before it dispatches the
  // session event that moves `sessionID`, so between the two only this names the session open.
  const hostSessionID = (): string =>
    activeSessionContext?.sessionManager.getSessionId() ?? sessionID;
  // Whether `id` is the session open here, for this instance and the host alike: a role move
  // decided for `id` acts only while it is.
  const sessionOpen = (id: string): boolean => sessionID === id && hostSessionID() === id;
  // What a role move that decided nothing tells the plugin or the model: the session it was
  // decided for stopped being the open one. A claim the listener granted all the same is held
  // here under that session, and the next pass carries it or hands it back; one that never
  // landed claimed nothing. Both the plugin bridge's claim and the tool say the same sentence.
  const sessionLeftDuringClaim = (
    from: string,
    role: string,
    claimAdopted: boolean
  ): SessionLeftError =>
    new SessionLeftError(
      claimAdopted
        ? `the session moved from ${from} to ${hostSessionID()} while its claim of role ${role} was in flight; the next pass keeps the hold for the session the host is on, or hands it back`
        : `the session moved from ${from} to ${hostSessionID()} while its claim of role ${role} ran; nothing was claimed`
    );

  // The registration and role state the two serial queues write, the queues themselves, each
  // task's deadline and this extension instance's client instance id, all private to
  // `../src/registration-state`. A queued task writes none of it by hand: `run` hands it a handle
  // whose every step — a state write, a listener request, a NATS connection, a subscribe —
  // refuses once that task was abandoned at its deadline or the session it acts for stopped being
  // the open one, and the first refusal latches. Nothing here re-checks a fence before a step.
  // `handle.check()` is left at the two places that are not steps: where a catch-all would
  // otherwise swallow the refusal, and before an action outside the handle (a replay trigger, a
  // throw this pass reports) that a refused task must not take.
  const registrationState = createRegistrationState({
    client: () => requireEnvoy().client,
    sessionOpen,
    deadlineMs: passDeadlineMs,
    notify: (message, level) => activeSessionContext?.ui.notify(message, level),
    log: logger,
    appendRoleClaimEntry: (role) => {
      // The transcript is the one thing `omp --resume` guarantees, so it is the durable record
      // of the claim: the listener reaps a dead session's interest row 15 minutes after the
      // row's last write and releases the role claim separately, one session TTL after
      // heartbeats stop (a role claim is not an interest: see `ReapRoleClaims` in
      // packages/envoy/internal/store/kv.go), and this process's memory dies with it. A session
      // that holds a role heartbeats from here on, whichever claim took it.
      pi.appendEntry(ROLE_CLAIM_ENTRY, { role });
      if (activeSessionContext !== undefined) ensureHeartbeat(activeSessionContext);
    },
    dial: dialNats,
    connect: ensureConnection,
    subscribeTopic: subscribe,
    closeSubject: (subject) => {
      closeIntentionally(subject);
    },
  });

  // Opens `topic` unless it served a role that has ended since `snapshot` was taken (a first
  // subscribe, or any retry of a dropped topic's chain): a role-bound subject is not reopened, and
  // one whose role ends while its connection is made is closed as soon as it opens, before any
  // registration can carry it. Answers whether the topic is still wanted.
  const subscribeUnlessRoleEnds = async (
    topic: string,
    snapshot: RoleSnapshot
  ): Promise<boolean> => {
    const roleEnded = () =>
      snapshot.roleBound && registrationState.roleEndCount() !== snapshot.endsBefore;
    if (roleEnded()) return false;
    await subscribe(topic);
    if (!roleEnded()) return true;
    for (const subject of expandSubscriptionTopics([topic])) closeIntentionally(subject);
    return false;
  };

  // What this session registers with the listener, read afresh at every registration. The
  // listener delivers nothing to a self-subscribed session (it takes its topics over its own
  // NATS subscriptions), so the registered topics serve recovery on resume; a role-bound subject
  // stays out of them, and so does one reaching another session's agent subject (a fork parent's,
  // until a pass closes it, or a wildcard over them), which the listener refuses to store.
  // While the session's relay subject is open, the registration asks for consumer delivery
  // (`delivery`: the relay's epoch and resume point), the relay taking the topics it carries as
  // followed first; its answer says whether the listener relays them.
  const registrationInput = (asking: Relay | undefined): RegistrationInput => {
    const own = agentSubject(sessionID);
    const topics = [
      ...new Set([
        own,
        ...[...subscriptions.keys()].filter(
          (subject) =>
            !registrationState.isRoleBound(subject) && !reachesPrivateStreamSubject(own, subject)
        ),
      ]),
    ];
    if (asking !== undefined) asking.delivery.setFollowed(topics);
    return {
      sessionID,
      directory: sessionDirectory,
      topics,
      port: 0,
      // Read at every registration: the heartbeat re-registers, which picks up
      // titles assigned after session_start and later renames.
      title: activeSessionContext?.sessionManager.getSessionName?.() ?? "",
      capabilities:
        sideTurn(activeSessionContext) === undefined
          ? CAPABILITIES_WITHOUT_BTW
          : DELIVERY_CAPABILITIES,
      driving: false,
      selfSubscribed: true,
      plugin: { name: packageName, version: packageVersion },
      ...(asking === undefined ? {} : { delivery: asking.delivery.registration() }),
    };
  };

  // One registration, whichever path sends it: `registrationState.refreshRegistration` outside a
  // queue, a queued task's own fenced `handle.register` inside one. While this session's relay
  // subject is open the registration asks for consumer delivery, and the answer decides whether
  // the listener relays the topics it carried (`takeDeliveryAnswer`).
  const sendRegistration = (
    send: (input: RegistrationInput) => Promise<SubscribeResult>
  ): Promise<SubscribeResult> => {
    const registeringFor = sessionID;
    const asking =
      relay?.sessionID === registeringFor && relay.subscription !== undefined ? relay : undefined;
    const input = registrationInput(asking);
    return send(input).then((result) => {
      takeDeliveryAnswer(registeringFor, input.topics, asking, result);
      return result;
    });
  };

  // Registers the session outside any queue, carrying this instance's client instance id: a
  // registration moves the session's `last_seen` at the listener even when its answer is lost, so
  // the module first takes back every hold this process abandoned under that session, and times
  // the answer on the one clock the heartbeat's stall test reads.
  const registerSession = () => sendRegistration(registrationState.refreshRegistration);

  const ensureHeartbeat = (context: SessionContext): void => {
    if (heartbeatRegistered) return;
    // Never let a heartbeat tick reject unhandled: OMP treats unhandled
    // rejections as fatal (postmortem exitAfterFatal), so a registry blip
    // would kill a live session. Warn once per outage; registration
    // self-heals on the next successful tick.
    let heartbeatOutageNotified = false;
    // Said once per stall episode, by whichever symptom shows first: a tick that had to refresh
    // the registration itself because a pass was still running, or a pass abandoned at its
    // deadline. One episode, one notification and one log line; cleared when a pass next settles.
    let stallNotified = false;
    // Set only by a failed registration, never by a failed role check afterwards: the listener
    // answers "no holder" for a session whose registry entry lapsed, not for a role read that
    // failed, and only the former warrants a `"reregistered"` regain on the next healthy tick.
    // Cleared only once a whole pass — registration and role — has decided, so a recovery tick
    // whose role read errors, or whose pass the host overtook, still owes the regain.
    let registrationFailed = false;
    let healing = false;
    // The pane hears about a stall as it hears about a registry outage, and for the same reason:
    // the session is at risk of being reaped by the listener while the pane runs on.
    const noteStall = (detail: string): void => {
      if (stallNotified) return;
      stallNotified = true;
      logger.warn("envoy: reconcile pass stalled; registration refreshed on its own", {
        detail,
        sessionID: context.sessionManager.getSessionId(),
      });
      context.ui.notify(
        `envoy: a reconcile pass has stalled (${detail}); the session registration is being refreshed on its own, and recovers once the pass settles`,
        "warning"
      );
    };
    context.setInterval(() => {
      // Sessions can be created lazily after session_start (a fresh TUI has no session yet); until
      // the host mints an id there is nothing to register.
      if (context.sessionManager.getSessionId() === "") return;
      // A held inert card or replay notice, or a card the host withdrew unwritten, is handed first,
      // while the host is settled (`hostSettled`), so the sweep finds its frame still claimed.
      // Every tick also sweeps the stream, outside the pass, so a sweep never waits on one: it
      // catches up on what a missed trigger left, and keeps probing a listener without replay.
      unwritten.handBackWithdrawn();
      triggerReplay();
      if (healing) {
        // A pass that has not settled holds the queue, and with it the registration refresh that
        // is its last step. The age is measured from the last registration accepted for the
        // session running now: one accepted for an earlier session, which is what a `/new` during
        // a wedged pass leaves behind, counts as none at all, or the new session would sit out of
        // the listener's sessions for a whole interval while its pane runs. A session with no
        // accepted registration of its own counts as older than any age. A pass still running a
        // whole refresh interval later has already spent 90 s of the record's 300 s lifetime, so
        // the refresh goes out on its own, outside the queues, exactly as `envoy_subscribe` posts
        // it. A failure is ignored: the next tick tries again.
        const refreshMs = requireEnvoy().settings.heartbeatMs;
        if (registrationState.registrationFresherThan(sessionID, refreshMs)) return;
        noteStall(
          "a pass has not settled a whole refresh interval after the last accepted registration"
        );
        void registerSession().catch(() => undefined);
        return;
      }
      healing = true;
      const afterOutage = registrationFailed;
      const settled = (decided: boolean) => {
        if (decided) registrationFailed = false;
        heartbeatOutageNotified = false;
        stallNotified = false;
      };
      void reconcile(context, { heartbeat: true, afterOutage })
        .then(settled, (error: unknown) => {
          // NATS is down but the registration and the role went through: the session's start or
          // its session event warned of the outage, and every tick tries the agent subject again.
          if (error instanceof InboundError) return settled(error.decided);
          // The pass, or the role move inside it, was abandoned at its deadline. That is an
          // outage like a failed registration: the listener reaps a role claim whose holder left
          // the session registry after one record lifetime, so the next healthy tick re-asserts
          // the role and fires the regain hook (`"reregistered"`), which makes a Legion worker
          // known to its daemon again. One notification per episode, so it stops here rather
          // than becoming a second `registry heartbeat failed` line.
          if (taskWasAbandoned(error)) {
            registrationFailed = true;
            noteStall(messageFor(error));
            return;
          }
          if (error instanceof RegistrationError) registrationFailed = true;
          throw error instanceof RegistrationError ? error.cause : error;
        })
        .catch((error) => {
          if (heartbeatOutageNotified) return;
          heartbeatOutageNotified = true;
          context.ui.notify(
            `envoy: registry heartbeat failed (${messageFor(error)}); retrying every heartbeat`,
            "warning"
          );
        })
        .finally(() => {
          healing = false;
        });
    }, requireEnvoy().settings.heartbeatMs);
    heartbeatRegistered = true;
  };

  const recoverRegisteredInterests = async (handle: TaskHandle): Promise<void> => {
    // Registered interests deliver only through this session's own NATS
    // subscriptions — the listener skips push delivery for self-subscribed
    // sessions — so a resumed process must rebuild them or stay deaf to
    // topics it registered before it died (e.g. dispatch thread replies).
    // Quiet on failure like rebind: a brand-new session has no registry
    // entry, and a listener outage must not fail session start.
    // Role topics are not subscriptions (role deliveries arrive on the agent
    // subject); a pass claims them back once registered (`settleRole`).
    // In the daemon-launched controller (the bridge's `slackRoleOwned`), every registered Slack
    // topic is unsubscribed from the listener and never restored: a restored topic would be a plain
    // subscription, which outlives the controller role, and the controller hears its listed
    // channels through the subscriptions its role owns (pi-legion's controller-session.ts).
    // The topics the newest replay state names come back too: the listener reaps a dead session's
    // interests after its grace, and the transcript still says what the session followed. A topic
    // it unfollowed is absent from that state. Agent and role subjects are never among them.
    const registry = await handle.readInterest(sessionID);
    const slackRetired = publishedInterface?.roleClaim.slackRoleOwned === true;
    const registered = registry?.topics ?? [];
    const retired = slackRetired
      ? registered.filter((topic) => slackTopicChannel(topic) !== undefined)
      : [];
    // A step of this pass, so a pass the host left behind unregisters nothing: the listener keeps
    // the retired topics until the pass the move queued reads the registry itself. The failure is
    // this branch's to name: the step warns under the sentence it is handed.
    if (retired.length > 0)
      await handle.unregisterTopics(
        sessionID,
        retired,
        "envoy: could not unregister a resumed controller's retired Slack topics"
      );
    // The newest replay state's topics (`rememberedTopics`), but a retired Slack topic.
    const remembered = rememberedSubjects(
      activeSessionContext?.sessionManager.getEntries() ?? []
    ).filter((topic) => !(slackRetired && slackTopicChannel(topic) !== undefined));
    for (const topic of new Set([...registered, ...remembered])) {
      if (topic === agentSubject(sessionID) || topic.startsWith(ROLE_TOPIC_PREFIX)) continue;
      if (retired.includes(topic)) continue;
      await handle.subscribe(topic);
    }
  };

  const transcriptClaimedRole = (branch: readonly unknown[]): string | null | undefined => {
    // The last claim entry wins: a claim followed by a release is no claim.
    let role: string | null | undefined;
    for (const entry of branch) {
      if (!isRoleClaimEntry(entry)) continue;
      role = entry.data.role;
    }
    return role;
  };

  // The role the host session `id` should hold:
  //  - The role its own branch records: the transcript entry written at claim time is the durable
  //    record (it survives `omp --resume` after the listener has reaped a dead session's rows),
  //    and a recorded release is final. A fork's branch is a copy of its parent's, so it records
  //    its parent's role; a `/new` records none; a resume or branch records its own.
  //  - Otherwise the role this process holds, when its hold is under `id` or a session `id`
  //    continues (a tree navigation to before the claim, or a fork from before it).
  //  - Otherwise, once when a pass establishes a resumed or switched session (`readRows`), the
  //    listener's rows for `id` (a same-id resume inside the reap window) and for the sessions it
  //    continues.
  // None of them while the listener has refused that role to `id`. The pass
  // claims it softly (`settleRole`): a session's records may be stale evidence (the parent of a
  // fork whose role moved to the child, a second process on the same transcript), and it must not
  // take a role another live session holds. The listener decides that atomically: a soft claim
  // naming a predecessor takes the role from that predecessor only when the predecessor has no
  // live registry entry, when its live entry carries this client instance's own instance id, or
  // when either side carries no instance id. A predecessor still live under another client
  // instance keeps the role, and the claim is refused naming it.
  const desiredRole = async (
    handle: TaskHandle,
    id: string,
    branch: readonly unknown[],
    readRows: boolean
  ): Promise<string | undefined> => {
    const want = (role: string) => (registrationState.refusalFor(id) === role ? undefined : role);
    const recorded = transcriptClaimedRole(branch);
    if (recorded === null) return undefined;
    if (recorded !== undefined) return want(recorded);
    const held = registrationState.heldRole();
    const holdID = registrationState.holdSessionID();
    const lineage = continuedBy(id);
    if (held !== undefined && holdID !== undefined && lineage.includes(holdID)) return want(held);
    if (!readRows) return undefined;
    for (const source of lineage) {
      const registry = await handle.readInterest(source);
      const topic = registry?.topics.find((candidate) => candidate.startsWith(ROLE_TOPIC_PREFIX));
      if (topic !== undefined) return want(topic.slice(ROLE_TOPIC_PREFIX.length));
    }
    return undefined;
  };

  // Says, once per session, that the agent now lives under a new id when a fork or a branch
  // carried its conversation over from another session: any identity already baked into that
  // carried context — envoy_whoami output, ids the agent named in sent messages — silently rots,
  // and peers who saved the old id can no longer reach this session. Told without starting a
  // turn, by the pass that converges to the session, while the host is on it, naming the session
  // it continues. A session a `/new` or resume opened, or one the host came back to, has a
  // transcript that matches its id, so it is told nothing.
  const tellIdChanged = (current: string): void => {
    const previous = continues.get(current);
    if (previous === undefined || previous === current || notifiedSessions.has(current)) return;
    notifiedSessions.add(current);
    pi.sendMessage(
      {
        customType: "envoy-message",
        content: encode({
          envoy: {
            notice: "session id changed",
            previous_session_id: previous,
            session_id: current,
            detail:
              "This conversation now lives under a new session id. Any identity you shared earlier (envoy_whoami output, session ids named in messages) is stale, and peers using the old id cannot reach you. Re-run envoy_whoami before identifying yourself and re-announce the new id to peers you already introduced yourself to.",
          },
        }),
        display: true,
        details: LOCAL_ENVOY_NOTICE,
      },
      { deliverAs: "steer", triggerTurn: false }
    );
  };

  // Learns where the host went. An announcement made before the move (or, without one, the
  // switch's own event) names the reason: a session a fork or a branch opened continues the one
  // it left; one a `/new` or resume opened continues nothing. A move with no word to an id never
  // seen here (a fresh TUI's first id after another, a session file the host moved off) carries
  // the session on, as the heartbeat's drift heal always has; one back to a session seen before
  // (a rollback) teaches nothing, since that session's lineage is how it was first opened. A pass
  // that finds the host still on the session it announced leaving keeps the announcement for the
  // move; a session event there ends it (a `/reload`, which lands back on its own session).
  const noteMove = (host: string, reason?: { readonly kind: SessionSwitchReason | undefined }) => {
    if (host === "") return;
    if (announced !== undefined && host === announced.from) {
      if (reason !== undefined) announced = undefined;
      return;
    }
    const from = announced?.from ?? sessionID;
    const told = announced !== undefined || reason !== undefined;
    const kind = announced !== undefined ? announced.reason : reason?.kind;
    announced = undefined;
    if (from !== "" && from !== host) {
      if (told) {
        if (carriesSession(kind)) continues.set(host, from);
        else continues.delete(host);
      } else if (!knownSessions.has(host)) {
        continues.set(host, from);
      }
      knownSessions.add(from);
    }
    knownSessions.add(host);
  };

  // Every session id this process has been on: a move with no word back to one is a rollback.
  const knownSessions = new Set<string>();

  interface PassCause {
    // A heartbeat tick, which also checks the listener still resolves a held role.
    readonly heartbeat?: boolean;
    readonly afterOutage?: boolean;
  }

  // Converges this process to the host's session as it is now: that session's agent subject open
  // and registered and Dispatch bound to it, every other agent subject this process opened
  // closed, and the role that session's records name held for it (`settleRole`). Every session
  // event, every heartbeat tick and every bridge call runs one, one at a time; a pass that finds
  // the host moved after any await stops, and the pass the move queues converges. Answers whether
  // it decided the role: a pass that decided nothing, because it was abandoned at its deadline or
  // the host left it mid-pass, answers false.
  const reconcile = (context: SessionContext, cause: PassCause = {}): Promise<boolean> =>
    registrationState.passes
      .run((handle) => reconcileNow(context, cause, handle))
      .then((decided) => decided ?? false);

  const reconcileNow = async (
    context: SessionContext,
    cause: PassCause,
    handle: TaskHandle
  ): Promise<boolean> => {
    const host = context.sessionManager.getSessionId();
    // Every write, listener call, dial and subscribe below is a step of this pass's handle, which
    // refuses it once the pass was abandoned at its deadline or the host moved out from under it:
    // a pass that no longer decides records nothing of what it opened, and the pass the move
    // queues converges from there.
    handle.actFor(host);
    noteMove(host);
    if (sessionID !== host) restoreLocalSessionState(context);
    if (registrationState.establishedFor() !== host) {
      // A move with no session event refreshed no shell environment: name the live id, and
      // refresh the snapshot the shell takes, as the host does after a session event.
      if ((process.env.DISPATCH_SESSION_ID ?? "") !== host) {
        bindDispatchSession(host, context.ui);
        procmgr.refreshShellConfigCache();
      }
      tellIdChanged(host);
      // A session the host reaches with no event is one no plugin heard, the session last heard
      // included once the host has been elsewhere (a rollback to it).
      if (host !== lastHeard) lastHeard = "";
    }
    for (const other of handle.forgetOtherAgentSessions(host)) {
      closeIntentionally(agentSubject(other));
    }
    // Another session's relay: its unsettled frames go back to the listener (`nak`).
    if (relay !== undefined && relay.sessionID !== host) closeRelay();
    if (host === "") {
      handle.markEstablished(host);
      ensureHeartbeat(context);
      return true;
    }
    // Only opening the session's agent subject needs NATS. The registration and the role go over
    // HTTP, so a NATS outage stops neither (a Legion claim through the bridge included): the pass
    // reports it once they are settled, and the next pass opens the subject. A steady pass touches
    // no NATS, and the subscriptions' own retry resubscribes an open subject once NATS is back.
    let inboundFailure: unknown;
    let openedInbound = false;
    if (inbound && registrationState.inboundOpenSession() !== host) {
      try {
        handle.markAgentSessionOpen(host);
        await handle.connect();
        await handle.subscribe(agentSubject(host));
        // The relay subject opens before any registration asks for consumer delivery, so the
        // listener's first relayed frame has a subscriber.
        await openRelay(context, host, handle.connect);
        // A viewer's replay request and its watch pings reach the session here. Opening it costs
        // one core subscription and publishes nothing: a session nobody opens stays silent.
        await watchAgentStream(host, handle.connect);
        if ((context.sessionManager.getBranch?.() ?? []).length > 0) {
          await recoverRegisteredInterests(handle);
        }
        handle.markInboundOpen(host);
        openedInbound = true;
      } catch (error) {
        inboundFailure = error;
      }
    }
    // A step this pass was refused ends it here: what it was refused is no NATS outage to
    // report, and the pass the move queued opens the subject.
    handle.check();
    // Every session subscribes to its own agent subject, so every session is registered;
    // envoy_send treats an unregistered id as dead. The listener rejects a claim from a session it
    // does not currently know, so this comes before the role.
    const registration = await sendRegistration(handle.register).then(
      () => undefined,
      (error: unknown) => ({ error })
    );
    // The catch-all above turned whatever the registration threw into `registration.error`, a
    // refused step included: this says so rather than report a registration failure.
    handle.check();
    if (registration === undefined) {
      handle.markEstablished(host);
      ensureHeartbeat(context);
    }
    // The agent subject and the recovered topics are open, and registered as this session's, which
    // the listener's replay asks: what the stream stored while they were closed is swept now, on
    // its own chain, and the pass goes on.
    if (openedInbound) triggerReplay();
    // The nested move has its own queue and its own deadline: it is fenced by `roleMoves`, not
    // by this pass, and one that decided nothing leaves the role to the next pass.
    const decided =
      (await registrationState.roleMoves.run((move) =>
        settleRole(context, host, cause, registration === undefined, move)
      )) ?? false;
    handle.check();
    if (registration !== undefined) {
      throw new RegistrationError(messageFor(registration.error), { cause: registration.error });
    }
    if (inboundFailure !== undefined) {
      throw new InboundError(messageFor(inboundFailure), decided, { cause: inboundFailure });
    }
    return decided;
  };

  // The session the latest session event moved the host to: a pass that decides for a session no
  // event announced (a move with no event, a rollback) is the only one a plugin did not hear, so
  // only its role ends carry a warning.
  let lastHeard = "";

  // The pass's role move: converge the role this process holds to the one the host session
  // should hold, for that session. It runs in its turn of the role moves, so a move the model or
  // a plugin made meanwhile is in the transcript by then, and it reads nothing until that turn. A
  // pass whose registration failed hands back what the session does not want and claims nothing,
  // since the listener takes no claim from a session it does not know.
  const settleRole = async (
    context: SessionContext,
    host: string,
    cause: PassCause,
    registered: boolean,
    handle: TaskHandle
  ): Promise<boolean> => {
    // Every write and listener call below is a step of this move's handle, which refuses it once
    // the move was abandoned at its deadline or the host moved: the move then decides nothing,
    // and the move the next pass queues converges from the state this one left. A step is never
    // preceded by a check here, because it checks itself.
    handle.actFor(host);
    const branch = context.sessionManager.getBranch?.() ?? [];
    // The rows are read until a pass has decided for this session, except for a brand-new first
    // session, which has none.
    const settled = registrationState.roleSettledSession();
    const readRows = settled !== host && (branch.length > 0 || settled !== "");
    const desired = await desiredRole(handle, host, branch, readRows);
    const unheard = lastHeard !== host;
    const held = registrationState.heldRole();
    if (held !== undefined && held !== desired) {
      // A role end after a move no session event announced is one no plugin heard: the hand-back
      // says so in the pane, and who claims it again.
      await handle.handBackHeldRole(unheard);
    }
    if ((await handle.settleEndedHolds(desired)) && unheard && desired !== undefined) {
      handle.noteNoticesClosed(desired);
    }
    if (!registered) return false;
    if (desired === undefined) {
      handle.markRoleSettled(host);
      return true;
    }
    if (
      !registrationState.holdIsUnconfirmed() &&
      registrationState.heldRole() === desired &&
      registrationState.holdSessionID() === host
    ) {
      handle.markRoleSettled(host);
      if (cause.heartbeat !== true) return true;
      // A heartbeat pass makes sure the listener still resolves this session as the role's live
      // holder, and fires Legion's regain hook for a role it had to take back.
      const checked = await handle.reassertRole(desired, cause.afterOutage === true);
      const { regain } = checked;
      const regained = regain === undefined ? undefined : legionRoleClaimBridge().regained;
      if (regain !== undefined && regained !== undefined) {
        // Detached from the heartbeat chain: the hook is a daemon round-trip this side cannot
        // bound (`claims/ready`, retried on a 5xx or a transport failure), and the chain's
        // `healing` latch must release as soon as registration and the claim are settled, or the
        // next tick could never register. `Promise.resolve().then` also catches a synchronous
        // throw.
        void Promise.resolve()
          .then(() => regained(desired, regain))
          .catch((error: unknown) => {
            logger.warn("envoy: role regain hook failed", {
              role: desired,
              reason: regain,
              error: messageFor(error),
            });
          });
      }
      return checked.decided;
    }
    // A role a session holds by its records, with inbound messages off, is never claimed: every
    // task and notice reaches its holder over NATS.
    if (!inbound) {
      handle.markRoleSettled(host);
      return true;
    }
    // The claim may supersede this process's own hold of the role, wherever the listener names
    // it, or else whichever session the host session continues the listener names (a fork's hold
    // from this process or an earlier one).
    const holdID = registrationState.holdSessionID();
    let supersedes =
      registrationState.heldRole() === desired && holdID !== undefined && holdID !== ""
        ? holdID
        : undefined;
    const lineage = continuedBy(host);
    if (supersedes === undefined && lineage.length > 1) {
      const holder = await handle.readRoleHolder(desired);
      if (holder !== undefined && holder !== host && lineage.includes(holder)) supersedes = holder;
    }
    try {
      const claimed = await handle.claimRole(desired, {
        soft: true,
        previousSessionID: supersedes,
      });
      if (claimed && unheard) handle.noteNoticesClosed(desired);
    } catch (error) {
      // This catch would otherwise swallow a step the handle refused: the check says so instead.
      // A move that was refused decided nothing, and the move or pass that replaced it does.
      // That is not a failure to report.
      handle.check();
      logger.warn("envoy: role reclaim failed", {
        role: desired,
        sessionID: host,
        error: messageFor(error),
      });
    }
    handle.markRoleSettled(host);
    return true;
  };

  // A bridge call for a session no pass has established yet runs one first. NATS being down does
  // not stop the call: a claim goes over HTTP, as the pass's registration did, and a subscription
  // fails on its own connect.
  const establishForBridge = (context: SessionContext): Promise<void> =>
    reconcile(context).then(
      () => undefined,
      (error: unknown) => {
        if (!(error instanceof InboundError)) throw error;
      }
    );

  // The bridge's state is read through `legionRoleClaimBridge()` at each use; only the instance
  // list is this instance's own entry on the object it published into.
  const claim: LegionRoleClaim = async (targetSessionID, role, callerContext) => {
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(`Envoy has no active session for Legion role claim: ${targetSessionID}`);
    }
    // Legion drives this session from here on, and the run-end ask nudge stays out of it.
    // Recorded before the claim runs: a claim that fails midway leaves the session just as
    // Legion-driven as one that succeeds.
    markLegionManagedSession(targetSessionID);
    // The role's every task and notice reaches its holder over NATS: with inbound messages off,
    // a held role would take them all and deliver none, so the claim refuses before any listener
    // call registers the session or sets the role.
    requireInbound();
    if (registrationState.establishedFor() !== targetSessionID) {
      await establishForBridge(context);
    }
    // The listener rejects a claim from a session it does not currently know
    // (its registration may have expired), so register before claiming.
    await registerSession();
    let claimAdopted = false;
    const claimed = await registrationState.roleMoves.run(
      async (handle) => {
        handle.actFor(targetSessionID);
        await handle.claimRole(role);
        return true;
      },
      (refusal) => {
        claimAdopted = refusal.claimAdopted;
      }
    );
    // The move decided nothing, because the session it was decided for stopped being the open
    // one: the plugin that asked hears that, as it hears a claim that failed, and hears whether
    // the listener granted the claim all the same.
    if (claimed === undefined) throw sessionLeftDuringClaim(targetSessionID, role, claimAdopted);
  };

  const subscribeNotice: LegionNoticeSubscription = async (
    targetSessionID,
    topic,
    callerContext,
    whileHolding
  ) => {
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(
        `Envoy has no active session for Legion notice subscription: ${targetSessionID}`
      );
    }
    if (registrationState.establishedFor() !== targetSessionID) {
      await establishForBridge(context);
    }
    let marked: readonly string[] = [];
    if (whileHolding !== undefined) {
      // A role move still in flight could end the role just after the subscription opens, so the
      // role is read once every move started before this call has landed.
      await registrationState.roleMoves.settled();
      if (!registrationState.holdsRoleTopic(ROLE_TOPIC_PREFIX + whileHolding)) {
        throw new Error(
          `Envoy session ${targetSessionID} does not hold role ${whileHolding} for a notice subscription`
        );
      }
      // Marked before the subscription opens, so no registration ever carries it, and a move the
      // model starts from here on sees it.
      marked = registrationState.markRoleBoundSubjects(
        expandSubscriptionTopics([topic]),
        whileHolding
      );
    }
    // Taken once the subjects are marked: nothing awaits between the two.
    const snapshot = registrationState.roleSnapshot(topic);
    try {
      if (!(await subscribeUnlessRoleEnds(topic, snapshot))) return;
    } catch (error) {
      // Nothing opened, so nothing may keep the session's role in place on its behalf. A role that
      // ended meanwhile already dropped these marks, and a newer subscription may hold them now.
      if (registrationState.roleEndCount() === snapshot.endsBefore) {
        registrationState.unmarkRoleBoundSubjects(marked);
      }
      throw error;
    }
    await registerSession();
  };

  const claimInstance: LegionRoleClaimInstance = {
    claim,
    subscribe: subscribeNotice,
    // The host's id, already moved by the time any session event is dispatched; the module
    // `sessionID` follows only once this instance's own rebind has run.
    sessionID: hostSessionID,
  };
  if (publishedInterface !== undefined) publishedInterface.roleClaim.instances.push(claimInstance);

  // A `task` subagent loads its own instance of this module in the parent's process and fires
  // its own session_start. It shares the parent's Envoy identity: registering it would list an
  // untitled session per subagent, heartbeated for as long as the parent process lives. The
  // host's own roster decides (any storage, any transcript or none), and the transcript layout
  // only where the roster gives no opinion.
  const isSubagent = subagentSessionCheck();

  /**
   * The Envoy address a reply to this instance reaches, and the source session its sends carry.
   *
   * An instance that took a session of its own answers with it — `liveSessionID` first, which
   * the slash command passes because a session created lazily after `session_start` has moved
   * past the module copy. A `task` subagent took none: it registers nothing, so its own host
   * session id is an address no message can be delivered to, and the reachable session is the
   * top-level one that spawned it, resolved from its transcript path
   * (`resolveEnvoySession`). That resolves to `""` when this process cannot say which
   * top-level session that is, and an empty address is what the caller reports and sends —
   * naming an unrelated live session would send peers to a session that never spawned it. A hook
   * that already asked the subagent check passes its answer as `subagent`, so it asks once.
   */
  const replyAddress = async (
    context: SessionIdentityContext,
    liveSessionID = "",
    subagent?: boolean
  ): Promise<string> => {
    if (sessionID !== "") return liveSessionID || sessionID;
    if (!(subagent ?? (await isSubagent(context)))) return liveSessionID;
    return resolveEnvoySession(context.sessionManager.getSessionFile());
  };

  pi.on("session_start", async (_event, context) => {
    if (await isSubagent(context)) return;
    bindDispatchSession(context.sessionManager.getSessionId());
    restoreLocalSessionState(context);
    lastHeard = context.sessionManager.getSessionId();
    if (startNotice !== "") context.ui.notify(`envoy: ${startNotice}`, "warning");
    await askgateLoad;
    for (const notice of askgateNotices) context.ui.notify(`envoy: ${notice}`, "warning");
    if (!inbound) return;
    let established = false;
    const establish = async (): Promise<void> => {
      await reconcile(context);
      established = true;
    };
    try {
      await establish();
    } catch (error) {
      context.ui.notify(
        `envoy: NATS unavailable (${messageFor(error)}); retrying in the background`,
        "warning"
      );
      let attempting = false;
      context.setInterval(() => {
        if (established || attempting) return;
        attempting = true;
        void establish()
          .then(() => context.ui.notify("envoy: NATS connection established", "warning"))
          // Quiet on purpose: the initial warning disclosed the outage, and a
          // fresh warning every retry tick would spam the session.
          .catch(() => undefined)
          .finally(() => {
            attempting = false;
          });
      }, NATS_RETRY_INTERVAL_MS);
    }
  });

  // Whether a switch carries the session on (a fork, a branch) rather than replace it (`/new`, a
  // resume): what the session it opens continues, and so what the frame drop delivers.
  const carriesSession = (reason: SessionSwitchReason | undefined): boolean =>
    reason !== "new" && reason !== "resume";

  // A session event: learn what the session the host is on continues, take its id at once (its
  // tools and messages name it from here on), end at once a role its records rule out, then
  // converge in a pass, which may claim again a role the listener refused that session before.
  const rebind = async (
    reason: SessionSwitchReason | undefined,
    context: SessionContext
  ): Promise<void> => {
    // The cards a check was armed for belong to the session the host left.
    cardWake.cancel();
    if (await isSubagent(context)) return;
    const host = context.sessionManager.getSessionId();
    noteMove(host, { kind: reason });
    lastHeard = host;
    registrationState.clearRefusal(host);
    if (sessionID !== host) restoreLocalSessionState(context);
    // Ends at once a role the host session's own records rule out, so the new session never
    // takes the old role's notices while an earlier pass finishes; the pass hands the hold back.
    registrationState.endUnwantedRole(
      host,
      transcriptClaimedRole(context.sessionManager.getBranch?.() ?? []),
      continuedBy(host)
    );
    // With inbound messages off the session opens no agent subject; a heartbeat's pass, when a
    // claimed role runs one, registers it and hands back what the event ended.
    if (!inbound) return;
    try {
      await reconcile(context);
    } catch (error) {
      // A switch during a network outage must degrade, not fail the handler; the heartbeat's next
      // pass or the NATS client's own reconnect converges.
      context.ui.notify(
        `envoy: rebind failed (${messageFor(error)}); will recover on reconnect`,
        "warning"
      );
    }
  };

  // Only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason at all. A switch and a branch change the id the `dispatch`
  // command names; Oh My Pi refreshes the shell's environment after those two, and never after
  // a tree navigation, which keeps the session's id.
  const rebindDispatch = async (
    reason: SessionSwitchReason | undefined,
    context: SessionContext
  ): Promise<void> => {
    if (!(await isSubagent(context))) {
      bindDispatchSession(context.sessionManager.getSessionId(), context.ui);
    }
    await rebind(reason, context);
  };
  pi.on("session_switch", (event, context) => {
    cardWake.noteIfIdle(context);
    return rebindDispatch(event.reason, context);
  });
  pi.on("session_branch", (_event, context) => rebindDispatch(undefined, context));
  pi.on("session_tree", (_event, context) => rebind(undefined, context));
  // Oh My Pi emits these before it moves its session manager: the session the host leaves and
  // why, which the next pass reads once the host has moved (`noteMove`). A subagent's instance and
  // a session the host has yet to give an id have nothing to leave. A switch another extension
  // cancels changes nothing: the host's id never moves, so the announcement teaches nothing.
  const announce = (reason: SessionSwitchReason | undefined, context: SessionContext): void => {
    cardWake.cancel();
    const from = context.sessionManager.getSessionId();
    // An earlier announcement whose move came with no event and no pass since is learned from now,
    // before this one replaces it.
    noteMove(from);
    if (from === "" || sessionID === "") return;
    announced = { from, reason };
  };
  pi.on("session_before_switch", async (event, context) => announce(event.reason, context));
  pi.on("session_before_branch", async (_event, context) => announce(undefined, context));

  pi.on("session_shutdown", async (_event, context) => {
    shuttingDown = true;
    cardWake.cancel();
    if (publishedInterface !== undefined) {
      const bound = publishedInterface.roleClaim.instances.indexOf(claimInstance);
      if (bound !== -1) publishedInterface.roleClaim.instances.splice(bound, 1);
    }
    // This session is about to deregister with the listener, so it stops being an address a
    // reply can reach: a subagent still running in this process must report none rather than
    // name it, and a dead entry must not be what makes some other session ambiguous.
    recordedSessionKey = recordEnvoySession({
      previousKey: recordedSessionKey,
      sessionFile: undefined,
      sessionID: "",
    });
    // The relay's unsettled frames go back to the listener before the connection drains.
    closeRelay();
    // The process ends, and with it every record it keeps live; removing them, and its store, is
    // tidiness (a dead process's record is inert). Only the instance that wrote records has any.
    if (askgateOwner !== undefined && askgateRecords.size > 0) {
      for (const dir of askgateRecords) {
        try {
          dropAskgateRecord(dir, askgateOwner);
        } catch {
          // A record left behind names a process that is gone.
        }
      }
      askgateRecords.clear();
      closeStore(askgateOwner.store);
    }
    const deadline = Promise.withResolvers<void>();
    const timer = setTimeout(deadline.resolve, 1_000);
    try {
      const deregistration =
        sessionID === "" || active === undefined
          ? Promise.resolve()
          : registrationState.deregister(sessionID).catch(() => undefined);
      const draining = connection?.drain().catch(() => undefined) ?? Promise.resolve();
      await Promise.race([
        Promise.allSettled([deregistration, draining]).then(() => undefined),
        deadline.promise,
      ]);
    } finally {
      clearTimeout(timer);
      connection = undefined;
      activeSessionContext = undefined;
      subscriptions.clear();
      intentionallyClosed.clear();
      awaitingRetry.clear();
      agentStreamControl = undefined;
      agentStreamControlSession = "";
      // This process serves the session no longer, so what it was shown goes with it: the id this
      // instance registered, and the host's live one, which a task subagent's tool calls carried
      // (its instance never ran restoreLocalSessionState, so its `sessionID` stayed empty).
      const liveSessionID = context.sessionManager.getSessionId();
      forgetShownPictures(sessionID);
      if (liveSessionID !== sessionID) forgetShownPictures(liveSessionID);
    }
  });

  // An Envoy that is off registers no tool and no `/whoami`: there is no listener to reach. Every
  // tool validates its own arguments inside execute (one refusal naming every problem); the host's
  // schema check is bypassed so it cannot pre-empt that with its own wording.
  if (envoy.enabled) {
    for (const spec of envoyToolSpecs) {
      pi.registerTool({
        name: spec.name,
        label: spec.name,
        description: spec.description,
        parameters: schemaFor(pi, spec.operation),
        lenientArgValidation: true,
        execute: async (_id, parameters, _signal, _onUpdate, context) =>
          execute(spec.operation, parameters, context),
      });
    }
    registerEnvoyWhoamiCommand(pi, replyAddress);
  }

  // What people wrote to the session in the last 72 hours, as the list of
  // `@legion/envoy-client/session-answers` renders it: the session `replyAddress` names, which is
  // the top-level session's own and for a `task` subagent its parent's, the session it writes to
  // Dispatch as (`DISPATCH_SESSION_ID`). Dispatch is resolved with `commands: "kept"`, so no `!`
  // token command runs on the request path, and the read waits at most
  // `SESSION_ANSWERS_DEADLINE_MS` before the last list read stands in. A subagent this process
  // cannot place carries no list rather than another session's. A Dispatch with no answers route
  // carries nothing to the model; the read's `notice`, which it gives once per session and
  // Dispatch URL, goes to the log instead.
  const sessionAnswersText = async (context: SessionContext): Promise<string | undefined> => {
    const session = await replyAddress(context, context.sessionManager.getSessionId());
    if (session === "") return undefined;
    try {
      const read = await readSessionAnswers({
        session,
        dispatch: sessionDispatchSettings(CONFIG_FILE, process.env, {
          cwd: process.cwd(),
          commands: "kept",
        }).dispatch,
        env: process.env,
      });
      if (read.notice !== undefined) logger.warn(`envoy: ${read.notice}`);
      return read.text;
    } catch (error) {
      // Oh My Pi drops all a `context` handler returns when it throws, the dispatch-first skill
      // with it, so a read that throws instead of answering with its failure line still costs
      // only the list: the request says the list could not be read, and why.
      logger.warn(`envoy: people's answers could not be read: ${messageFor(error)}`);
      return sessionAnswersFailureLine(messageFor(error));
    }
  };

  if (atStart.dispatch.enabled) {
    // Every session that reaches Dispatch, Legion panes and `task` subagents included, carries
    // the dispatch-first skill on every request at the head, where its bytes never change, and
    // on every request of its main loop what people wrote to it at the tail, where a change in
    // the list re-reads no cached prefix. The host runs this handler on its side turns, its
    // handoff prompt and a remote compaction's conversions too; `withSessionAnswers` leaves the
    // list off those. Oh My Pi hands this handler no abort signal: it races the handler against
    // the run's own and drops what an aborted handler returns, and the read it shares goes on to
    // fill the cache.
    pi.on("context", async (event, context) => {
      const withSkill = withDispatchFirst(event.messages, DISPATCH_FIRST_CONTEXT);
      const messages = await withSessionAnswers(withSkill ?? event.messages, {
        branch: () => context.sessionManager.getBranch?.() ?? [],
        session: context.sessionManager,
        // Read off the live `AgentSession` (18.12.2), whose getters the declared host surface
        // leaves out: `get isCompacting() { return this.#C.isCompacting; }` and
        // `get compactionSpeculation() { return this.#C.speculationState; }`, the compaction
        // controller's `this.#i !== undefined || this.#u !== undefined || this.#l !== undefined`
        // (an automatic or manual compaction, or a handoff) and `e.armed ? "armed" : "running"`
        // for a speculative one. A session the registry does not list (a `task` subagent's)
        // reads as not compacting.
        compacting: () => {
          const live = liveAgentSession(context.sessionManager.getSessionId());
          return (
            live !== undefined &&
            (("isCompacting" in live && live.isCompacting === true) ||
              ("compactionSpeculation" in live && live.compactionSpeculation === "running"))
          );
        },
        read: () => sessionAnswersText(context),
      });
      const result = messages ?? withSkill;
      return result === undefined ? undefined : { messages: result };
    });
  }

  // Turn start injects nothing into the conversation; its open-asks query only arms the run-end
  // nudge below, the snapshot's `as_of` becoming the period's baseline. A session the stop can
  // never nudge does not pay for it: the host awaits this handler, so a slow or unreachable
  // Dispatch would add up to `OPEN_ASKS_TIMEOUT_MS` to the head of each of its turns and then
  // warn it about a reminder it never gets — and a host with no side turn cannot run the
  // self-check the nudge now turns on, so it is one of those sessions. `id === sessionID` is
  // deliberately not one of the conditions — a fresh TUI mints its id lazily and heals by drift,
  // so it would drop the first turn's arming.
  pi.on("before_agent_start", async (event, context) => {
    const id = context.sessionManager.getSessionId();
    if (
      id === "" ||
      event.prompt.trim() === "" ||
      !context.hasUI ||
      sideTurn(context) === undefined ||
      legionManaged(id)
    ) {
      return undefined;
    }
    // Memoized per instance once settled: one answer for the life of the session.
    if (await isSubagent(context)) return undefined;
    // The user typed, so a check still in flight is about to answer for a run they have moved
    // past. This runs before the arming query rather than in `armAskAwareness`: a slow Dispatch
    // would otherwise hold the old check live for up to `OPEN_ASKS_TIMEOUT_MS`, and one that
    // fails would never arm at all, and either way its steer would land on the turn just typed.
    awarenessGeneration++;
    abortSelfCheck("a new user turn superseded the self-check");
    try {
      const open = await queryOpenAsks(id);
      if (open !== null) armAskAwareness(id, open.as_of);
    } catch (error) {
      warnAskAvailability(context, error);
    }
    return undefined;
  });

  // Every run the host starts, typed or not: the user's prompt, an Envoy delivery waking the
  // session, another extension's continuation. `agent_end` compares it with what it read at the
  // settle, so a verdict that arrives after the session has moved on steers nothing. A run that
  // starts on the session a switch announced leaving is marked, and its end tells whether that
  // switch went ahead (`runUnderAnnouncement`).
  pi.on("agent_start", async (_event, context) => {
    runSeq += 1;
    cardWake.agentStart();
    runUnderAnnouncement =
      announced?.from === context.sessionManager.getSessionId() ? announced : undefined;
  });

  // Arms one nudge period. A genuine user turn is the only thing that arms one: its caller,
  // `before_agent_start`, runs for an ordinary prompt and for a steering batch carrying the
  // user's own text, and returns before this for an empty one. A `triggerTurn` continuation of
  // an agent-attributed custom message — which is what the nudge below is — never reaches that
  // handler at all (measured on OMP 18.2.9: one before_agent_start for the user's prompt, none
  // for the continuation), so the nudge cannot re-arm itself, and one stop can produce at most
  // one of them.
  function armAskAwareness(id: string, asOf: string): void {
    askAwareness = {
      session_id: id,
      period: (askAwareness.session_id === id ? askAwareness.period : 0) + 1,
      baseline_as_of: asOf,
      check_due: true,
      checks: 0,
    };
  }

  // The run-end nudge. `agent_end` is the bare stop signal — no message, no shape to read — so
  // Dispatch supplies the open asks the self-check names. The hidden self-check decides whether
  // the agent is waiting for something outside that list; only its WAITING verdict buys the
  // steer. The check itself is owed and spent like the host's own todo reminder: the arming turn
  // owes one, the check spends it whatever the verdict, opening the ask spends it too, and the
  // agent's next real work owes another (`tool_result` below), so a run that keeps working keeps
  // being checked without a new user turn. A settled turn that only replies does no work, so the
  // nudge's own continuation never owes a check and cannot nudge itself;
  // `ASK_CHECKS_PER_PERIOD` bounds the rest.
  //
  // A run with no UI (`omp -p`, and any other headless launch) never gets it: the host disposes
  // the session at the end of that one run, and its output is already printed, so the steered
  // continuation either races dispose and is recorded aborted with no provider reply, or wins
  // and bills a whole turn whose text nobody reads. Both were measured on OMP 18.2.9. An RPC
  // host — every Legion pane — carries a UI context and is nudged as a terminal is. So does an
  // ACP host, but a client that defers agent-initiated turns gets the steer queued as hidden
  // next-turn context instead of a turn of its own, consumed when the user next prompts.
  pi.on("agent_end", async (event, context) => {
    cardWake.agentEnd(context);
    const id = context.sessionManager.getSessionId();
    const lastReply = event.messages?.findLast((message) => message.role === "assistant");
    // An interrupted run may have taken back the cards it held (`UnwrittenFrames`): those handed
    // before its end are looked at by the next heartbeat.
    if (lastReply?.stopReason === "aborted") unwritten.runAborted();
    // A person's direct message sent through `pi.sendUserMessage` and not yet seen as a user
    // message by the end of the run is not looked for again; one sent as typed input is, by its tag,
    // since the host can run it as a turn of its own after this run
    // (`@legion/pi-shared/injected-user-turns`). The record is kept under this instance's own
    // `sessionID`, where `deliver` notes a turn and the stream recorder matches it, and not under
    // the host's live id, which a fresh terminal's differs from until the heartbeat heals it.
    endInjectedUserTurns(sessionID);
    // A `pi.sendUserMessage` turn no longer looked for is done with now, found or not.
    await unwritten.textTurnsEnded();
    // A run that started under a switch's announcement and ended with a reply it was not aborted
    // in, the host still on the session it announced leaving: that switch was cancelled. A fork's
    // run ends after the host moved, which keeps the announcement, and with it the parent's frames.
    if (runUnderAnnouncement !== undefined && event.willContinue !== true) {
      if (
        announced === runUnderAnnouncement &&
        announced.from === id &&
        lastReply !== undefined &&
        lastReply.stopReason !== "aborted"
      ) {
        announced = undefined;
      }
      runUnderAnnouncement = undefined;
    }
    // Only a run that settled normally is nudged: steering an interrupt (`aborted`), a provider
    // failure (`error`), a truncation, or a run with no reply of its own answers the user's cancel,
    // or a failure, with a turn nobody asked for.
    const ask = sideTurn(context);
    if (
      event.willContinue === true ||
      lastReply?.stopReason !== "stop" ||
      !context.hasUI ||
      id === "" ||
      ask === undefined ||
      !checkOwed(id)
    ) {
      return;
    }
    const settle: AskSettle = { context, ask, seenRun: runSeq, generation: awarenessGeneration };
    if (askCheckInFlight) {
      settledDuringCheck = settle;
      return;
    }
    askCheckInFlight = true;
    // The check runs after this handler returns, on the host's managed timer: a side turn
    // started while an event handler is still running inherits that handler's abort signal
    // (Oh My Pi 18.3), which the host fires at its 30 s handler budget, well inside the check's
    // own `selfCheckTimeoutMs`. The host does not await `agent_end`, so nothing waits on it.
    context.setTimeout(() => checkFromSettle(id, settle), 0);
  });

  // Every stop-time check owed from `first` on, with `askCheckInFlight` held throughout. Each
  // pass after the first needs another real settle during the last, and every check that
  // reaches the model counts against the period's cap, so this ends.
  async function checkFromSettle(id: string, first: AskSettle): Promise<void> {
    try {
      let settle: AskSettle | undefined = first;
      while (settle !== undefined) {
        await checkAtSettle(id, settle);
        settle = takeSettledDuringCheck();
      }
    } finally {
      askCheckInFlight = false;
      settledDuringCheck = undefined;
    }
  }

  // Whether this session's current period still owes a stop-time check.
  function checkOwed(id: string): boolean {
    return !(
      shuttingDown ||
      legionManaged(id) ||
      // The host's live id, never the module's `sessionID`: a fresh TUI mints its id after
      // `session_start`, so the two disagree until the registration heartbeat heals the drift
      // (up to `ENVOY_HEARTBEAT_MS`, 90 s by default) and a brand-new terminal went unchecked
      // for that whole window. The period already carries the id that armed it, and a session
      // change resets the period to 0 and bumps the generation, so this is the discriminator —
      // `before_agent_start` and `tool_result` read the live id for the same reason. A `task`
      // subagent arms no period at all (it returns there), so it never reaches the check either.
      askAwareness.session_id !== id ||
      askAwareness.period === 0 ||
      !askAwareness.check_due ||
      askAwareness.checks >= ASK_CHECKS_PER_PERIOD ||
      askAwareness.baseline_as_of === null
    );
  }

  // One stop-time check for `settle`, run with `askCheckInFlight` held. Staleness is judged
  // against the run count and generation read at that settle, never at the start of this call.
  async function checkAtSettle(id: string, settle: AskSettle): Promise<void> {
    const { context, ask, seenRun, generation } = settle;
    const period = askAwareness.period;
    const baseline = askAwareness.baseline_as_of;
    // Both awaits below are windows in which a new user turn can arm another period, a
    // session change can move the session, or the agent's own next run can open the ask this
    // check is about — which clears the check it owed. The re-checks read one list, so they
    // cannot drift.
    const stale = (): boolean =>
      shuttingDown ||
      generation !== awarenessGeneration ||
      // Only the host's live id: see the guard above on the module's `sessionID`.
      context.sessionManager.getSessionId() !== id ||
      legionManaged(id) ||
      askAwareness.session_id !== id ||
      askAwareness.period !== period ||
      !askAwareness.check_due;
    // A settle is checked only while it is still current. The timer runs the first one a
    // macrotask after its stop, and one recorded while the last check was open waits for that
    // check: a run, a user turn, or a session change can have overtaken either meanwhile.
    // `baseline` is restated so the compiler sees it here.
    if (baseline === null || !checkOwed(id) || stale() || runSeq !== seenRun) return;
    let open: OpenAsksResponse | null;
    try {
      open = await queryOpenAsks(id, baseline);
    } catch (error) {
      if (generation === awarenessGeneration) warnAskAvailability(context, error);
      return;
    }
    if (open === null || stale()) return;
    // A run started while Dispatch answered: the conversation a self-check would read is past
    // this settle. Nothing is spent, so the newer run's own settle is where the check happens.
    if (runSeq !== seenRun) return;
    // Every normal settle takes the one path: Dispatch supplies the current open asks for the
    // prompt, not a reason to suppress it. `opened_since` remains part of the snapshot's
    // contract, but does not alter the self-check.
    // The self-check: one hidden, tool-free model call over a snapshot of this conversation
    // (`sideTurn`, the channel a targeted Dispatch BTW already uses), which adds nothing
    // to the transcript and which the user never sees. Only a WAITING verdict — the agent
    // saying it is waiting on a human for something its open asks do not cover — buys the visible
    // turn. PROCEEDING, any other answer, a timeout, and a failure are all silent, so an ordinary
    // settle costs one hidden call and shows nothing.
    //
    // The bound is the check's own clock, not the host's: the signal is still passed, so a
    // host that honours it stops paying for an answer nobody will read, but the await always
    // settles at `selfCheckTimeoutMs` whatever the host does. A host that ignored the signal
    // would otherwise hold `askCheckInFlight` — and with it every later check — for as long as
    // its call hung. A late answer is dropped; its rejection is already handled, so it can
    // never surface as an unhandled one.
    const abort = new AbortController();
    askCheckAbort = abort;
    const answered = ask({
      prompt: ASK_SELF_CHECK_PROMPT(open.asks),
      signal: abort.signal,
    }).then(
      (reply): string | undefined => reply.replyText,
      (error: unknown): string | undefined => {
        // An abort this extension made — a superseded check, or its own timeout, which logs
        // itself — is not a failure. Logging it would spend the once-per-session warning on
        // an ordinary event, such as the user typing, and silence the real failure after it.
        if (!abort.signal.aborted) logSelfCheckFailure(id, error);
        return undefined;
      }
    );
    const expiry = Promise.withResolvers<undefined>();
    const expire = setTimeout(() => {
      const timedOut = new Error(`self-check timed out after ${selfCheckTimeoutMs} ms`);
      abort.abort(timedOut);
      logSelfCheckFailure(id, timedOut);
      expiry.resolve(undefined);
    }, selfCheckTimeoutMs);
    let verdict: string | undefined;
    try {
      verdict = await Promise.race([answered, expiry.promise]);
    } finally {
      clearTimeout(expire);
      if (askCheckAbort === abort) askCheckAbort = undefined;
    }
    if (stale()) {
      // Whatever invalidated this check aborted it already if it could reach it; a host that
      // is still working on an answer nobody will read stops paying here.
      abort.abort(new Error("the self-check was superseded before its verdict arrived"));
      return;
    }
    // The check ran: it counts against the cap and moves the window on, whatever came back,
    // and it spends the period's outstanding check unless the session started another run
    // meanwhile — that run's own settle is where the check now belongs, and any work it did
    // owes one anyway.
    const superseded = runSeq !== seenRun;
    askAwareness = {
      ...askAwareness,
      check_due: superseded,
      checks: askAwareness.checks + 1,
      baseline_as_of: open.as_of,
    };
    // A verdict about a run the session has moved past is not steered into the one it is in:
    // "you just said you are waiting on a human" would describe a reply it has left behind.
    if (superseded) return;
    if (verdict === undefined || !isWaitingVerdict(verdict.trim())) return;
    // `deliverAs: "nextTurn"` with `triggerTurn` is the host's documented form for a message
    // sent during prompt teardown; on the pin this steer produced exactly one continuation in
    // every interactive and RPC run, so it stays the channel `deliver` already uses.
    pi.sendMessage(
      { customType: ASK_REMINDER_MESSAGE, content: UNASKED_WAIT_REMINDER, display: false },
      { deliverAs: "steer", triggerTurn: true }
    );
  }

  // The session title the `dispatch` command reads from the session's `title` file. A shell
  // command takes the environment Oh My Pi captured at the last session event, and a session is
  // named after it starts (the host's automatic title, a Legion pane's), so the name goes
  // through a file, rewritten before a main-agent shell command whenever it changed.
  let writtenTitle: { readonly sessionID: string; readonly title: string } | undefined;
  pi.on("tool_call", async (event, context) => {
    if (event.toolName !== "bash" && event.toolName !== "eval") return undefined;
    if (context.agent?.kind !== "main") return undefined;
    const id = process.env.DISPATCH_SESSION_ID;
    if (id === undefined || id === "") return undefined;
    const title = context.sessionManager.getSessionName?.() ?? "";
    if (writtenTitle?.sessionID === id && writtenTitle.title === title) return undefined;
    writeSessionTitle(dispatchSessionDirectory(process.env, id), title);
    writtenTitle = { sessionID: id, title };
    return undefined;
  });

  // AskGate's `tool_call` handler, registered now, in the factory, so it is there for the first
  // call; it waits for the modules the gate loads (`registerAskGate`). Only a session with
  // Dispatch and the switch on has one (`askgateFile`, above).
  if (askgateFile !== null) {
    const gateFile = askgateFile;
    // What the gate logs at bind is also a notice at session start: the one it logs there says
    // the review deadline is under 30 s, which at the host's default handler limit is every
    // profile that sets nothing, and a review that times out lets the write post. A warning a
    // gated call raises later goes to the plain log alone (`runtimeLogger`): it is about that
    // session's calls, and a notice here would show again at every later session start.
    const gateLogger = {
      warn: (message: string) => {
        logger.warn(message);
        askgateNotices.push(message);
      },
    };
    const registration = registerAskGate(pi, {
      configFile: gateFile,
      logger: gateLogger,
      runtimeLogger: logger,
    });
    askgateRecorder = registration;
    askgateLoad = registration.loaded.catch((error: unknown) => {
      const notice = `AskGate did not load, so it refuses each Dispatch write it would review: ${messageFor(error)}. To turn AskGate off for this session, set askgate.enabled to false in ${gateFile.path}.`;
      logger.warn(notice);
      askgateNotices.push(notice);
    });
  }

  pi.on("tool_result", async (event, context) => {
    // Read, and the offset advanced, on every result, failed or not and armed period or not, so a
    // later result never counts the calls this one ran.
    const entries = readNewDispatchResults();
    // A gated write the CLI found no AskGate marker for (refused in block mode, posted with a
    // warning in warn mode) becomes an `advisor-gate` entry, so AskGate's report counts it. Only
    // the top-level instance records it: it reads each line of the session's ledger once, the
    // lines of a subagent's calls included.
    if (
      askgateOwner !== undefined &&
      entries.some((entry) => entry.askgate !== undefined) &&
      !(await isSubagent(context))
    ) {
      for (const [line, { tool, askgate }] of entries.entries()) {
        if (askgate === undefined) continue;
        const entry: GateEntry = {
          advisor: "AskGate",
          tool,
          path: `dispatch ${commandName(tool)}`,
          toolCallId: event.toolCallId,
          decision: askgate.mode === "block" ? "revise" : "allow",
          outcome: "unmarked",
          verdictMode: askgate.mode,
          reason: askgate.why,
          latencyMs: 0,
          revisesForKey: 0,
          argsDigest: "",
        };
        // The entry above reaches only this machine's disk. A write that would have opened an
        // ask for a person also goes to Dispatch, where a session with no access to this machine
        // counts the asks that went out, or were refused, unreviewed: the record carries the
        // tool, the mode and how many asks the write carried, and no ask text. The entry is
        // appended first and the record started behind it, awaited by nothing of this call: a
        // session's records are written one at a time and each read and write has the client's
        // own deadline, so awaiting one here held this tool result for as long as every record
        // queued before it took, past the host's handler limit (#2161, reviewer round 17). A
        // record that fails or that the shutdown cuts rides on an `advisor-gate-record` follow-up
        // of its own, as a gated call's does (`askgate-core.ts`), appended while the session is
        // still live; the recorder's cut at shutdown is what bounds it.
        const asksCarried = askgate.asksCarried ?? 0;
        // One tool call can log several gated writes, so the record's id names the ledger line's
        // own place in it: what the reader de-duplicates a replayed write by, and what ties the
        // follow-up of a record that failed to this entry alone — the gated calls of that same
        // tool call carry its id too.
        const recordId = `${event.toolCallId}#${line}`;
        const recorder = asksCarried > 0 ? askgateRecorder : undefined;
        pi.appendEntry(ADVISOR_GATE_ENTRY_TYPE, {
          ...entry,
          ...(recorder === undefined ? {} : { recordId }),
        } satisfies GateEntry);
        if (recorder === undefined) continue;
        void (async () => {
          let recordError: string | undefined;
          try {
            recordError = await recorder.recordUnmarked({
              entry: { ...entry, asksCarried },
              sessionId: context.sessionManager.getSessionId(),
              directory: context.cwd,
              id: recordId,
            });
          } catch (error) {
            recordError = messageFor(error);
          }
          if (recordError === undefined) return;
          logger.warn(
            `envoy: AskGate did not record the unmarked ${entry.path} on Dispatch: ${recordError}`
          );
          try {
            pi.appendEntry(ADVISOR_GATE_RECORD_ENTRY_TYPE, {
              toolCallId: entry.toolCallId,
              tool: entry.tool,
              recordId,
              recordError,
            } satisfies GateRecordEntry);
          } catch {
            // The session is sealed or its file is gone: there is nothing left to tell, and the
            // tool result this record belongs to was answered long before it ran.
          }
        })();
      }
    }
    if (event.isError) return;
    // The host's live id, never the module's `sessionID`, for the reason the stop guard reads
    // it: in a fresh TUI's drift window the two disagree, and the period is armed against the
    // live one, so a comparison against the module copy matched nothing for up to a heartbeat —
    // the window this nudge was opened up for.
    if (
      askAwareness.session_id !== context.sessionManager.getSessionId() ||
      askAwareness.period === 0
    ) {
      return;
    }
    const openedAsk = entries.some(
      (entry) =>
        entry.details !== undefined && opensAsk({ toolName: entry.tool, details: entry.details })
    );
    // A call that owes no check: a bash command that is one `dispatch` command and wrote to the
    // ledger, or any eval cell during which the ledger grew. The two differ on purpose: a bash
    // command's text is scanned, while an eval cell runs `dispatch` only through a shell its code
    // starts, which no scan reads, so for it the ledger growing is the only signal.
    const bashRanDispatchAlone =
      event.toolName === "bash" && dispatchCommandHead(event.input.command) !== undefined;
    const owesNoCheck = entries.length > 0 && (bashRanDispatchAlone || event.toolName === "eval");
    if (openedAsk) {
      // The agent asked the humans itself, so this stop has nothing left for the nudge to
      // say: it spends the check the period owed rather than ending the period, and aborts a
      // check in flight before its verdict can be used. Later real work can re-arm a fresh
      // check, whose prompt names the ask.
      askAwareness = { ...askAwareness, check_due: false };
      abortSelfCheck("the agent opened the ask itself");
    } else if (!owesNoCheck) {
      // Real work: it owes the period another check, the way finishing a step re-arms the
      // host's todo reminder. A `dispatch` command is the agent talking to the humans this
      // nudge is about, not work, so it owes nothing — and a turn that only replies calls no
      // tool at all, which is what stops the nudge's own continuation from re-arming itself.
      askAwareness = { ...askAwareness, check_due: true };
    }
  });

  async function execute(
    operation: EnvoyToolOperation,
    rawParameters: Record<string, unknown>,
    context: SessionContext
  ): Promise<ToolResult> {
    try {
      // The host hands raw arguments through (lenientArgValidation); this is the one
      // refusal, naming every problem, before anything reaches the listener.
      const parameters: Record<string, unknown> = parseEnvoyToolArguments(operation, rawParameters);
      const { client } = requireEnvoy();
      switch (operation) {
        case EnvoyToolOperation.subscribe: {
          const added: string[] = [];
          const already: string[] = [];
          for (const topic of topicsFor(parameters)) {
            ((await subscribe(topic)) ? added : already).push(topic);
          }
          let warnings: readonly string[] | undefined;
          let registrationError: string | undefined;
          if (added.length > 0) {
            try {
              warnings = (await registerSession()).warnings;
            } catch (error) {
              registrationError = messageFor(error);
            }
            // A new follow starts its cursor at the stream's end now, with no older history, so
            // what is published from here on is recovered if the subscription drops.
            await replayFollowedChanged();
            triggerReplay();
          }
          const subscribed = `Subscribed: ${added.join(", ") || "(none new)"}`;
          return toolSuccess(
            warnings === undefined || warnings.length === 0
              ? subscribed
              : `${subscribed}\nWarnings: ${warnings.join("; ")}`,
            {
              added,
              already,
              ...(warnings === undefined || warnings.length === 0 ? {} : { warnings }),
              ...(registrationError === undefined ? {} : { registrationError }),
            }
          );
        }
        case EnvoyToolOperation.unsubscribe: {
          // The session's own inbox is not a subscription the tool manages.
          // Role topics can be ordinary local NATS subscriptions too, but their
          // listener interest represents the independently-held role claim: close
          // the local subscription without unregistering that interest.
          const inbox = agentSubject(sessionID);
          // Concrete subjects, a named `X.>` already expanded to `X.>` and `X`.
          const candidates = topicsFor(parameters, [...subscriptions.keys()]).filter(
            (subject) => subject !== inbox
          );
          // A subject another plugin opened for the role this session holds lasts as long as the
          // role does, so the tool keeps it and says so rather than leave the session deaf to it.
          const kept = candidates.filter(registrationState.isRoleBound);
          const removed = candidates
            .filter((subject) => !registrationState.isRoleBound(subject))
            .filter(closeIntentionally);
          // Persisted at once, so the topic stays unfollowed across a restart.
          if (removed.length > 0) await replayFollowedChanged();
          const registryRemoved = removed.filter((topic) => !topic.startsWith(ROLE_TOPIC_PREFIX));
          const registrationError =
            registryRemoved.length === 0
              ? undefined
              : await client
                  .unsubscribe({ sessionID, topics: registryRemoved })
                  .then(registerSession)
                  .then(() => undefined, messageFor);
          const unsubscribed = `Unsubscribed: ${removed.join(", ") || "(none)"}`;
          return toolSuccess(
            kept.length === 0
              ? unsubscribed
              : `${unsubscribed}\nKept (they last while this session holds role ${registrationState.heldRole()}): ${kept.join(", ")}`,
            {
              removed,
              ...(kept.length === 0 ? {} : { kept }),
              ...(registrationError === undefined ? {} : { registrationError }),
            }
          );
        }
        case EnvoyToolOperation.listInterests: {
          const registry = await client.getInterest(sessionID);
          const interests = mergeInterestSources(registry.topics, [...subscriptions.keys()]);
          return toolSuccess(
            JSON.stringify({ ...registry, topics: [...interests.keys()] }, null, 2),
            {
              interests: [...interests].map(([topic, source]) => ({ topic, source })),
            }
          );
        }
        case EnvoyToolOperation.inbox:
          return toolSuccess(JSON.stringify(inbox, null, 2), { count: inbox.length });
        case EnvoyToolOperation.send: {
          const targetSessionID = stringFor(parameters, "session_id");
          const result = await client.send({
            sourceSessionID: await replyAddress(context),
            targetSessionID,
            message: stringFor(parameters, "message"),
            ...toMessageMetadata(parameters as MessageMetadataArguments),
          });
          return toolSuccess(sendConfirmationText(result), {
            event_id: result.envelope.event_id,
            recipient: result.recipient,
            confirmed: result.confirmed,
          });
        }
        case EnvoyToolOperation.publish: {
          const topic = stringFor(parameters, "topic");
          const inSubagent = await isSubagent(context);
          const source = await replyAddress(context, "", inSubagent);
          const result = await client.publish({
            sourceSessionID: source,
            topic,
            message: stringFor(parameters, "message"),
            ...toMessageMetadata(parameters as MessageMetadataArguments),
          });
          // The skip is the listener's own delivery: the role lane skips the resolved holder,
          // and the fanout skips its push to any interest whose session is the message's source.
          // A session with no serving port — every Oh My Pi session — takes no push at all and
          // still gets the message: a topic the stream stores comes back on its own consumer's
          // relay, and an ad-hoc topic on its core subscription, so its own publish, and a
          // subagent's, does reach it. Only the role lane,
          // which its holder does not subscribe to, actually goes undelivered: a subagent's
          // source is its parent, so a publish to a role that parent holds is accepted, answers
          // with that holder, and reaches nobody. Only the role case is visible here (a plain
          // topic's subscribers are not in the answer); AGENTS.md and the envoy skill carry the
          // general rule. A direct envoy_send is unaffected: the agent-subject lane has no such
          // skip.
          // A kept answer reached no session at all, so the echo rule cannot apply to it: the
          // holder it names is the lapsed claim's session, which never received the message.
          const undelivered =
            result.kept !== true && source !== "" && result.holder === source && inSubagent;
          const published = publishConfirmationText(result);
          return toolSuccess(
            undelivered
              ? `${published}\nNot delivered: that holder is the session this subagent sends as, and the listener drops a message whose source session is its recipient, so the agent that spawned you did not receive it. Reach it with a write to its agent:// address instead.`
              : published,
            {
              event_id: result.envelope.event_id,
              topic,
              ...(result.holder === undefined ? {} : { holder: result.holder }),
              ...(result.kept === undefined ? {} : { kept: result.kept }),
              ...(result.keptReason === undefined ? {} : { kept_reason: result.keptReason }),
              ...(undelivered ? { undelivered_echo: true } : {}),
            }
          );
        }
        case EnvoyToolOperation.setRole: {
          const role = stringFor(parameters, "role");
          // Read and checked inside the move, so a move landing first is the role this one leaves.
          // The session the move is decided for, read when its turn comes and kept for the
          // refusal below, which the session the host is on by then would misname.
          let target = sessionID;
          let claimAdopted = false;
          const moved = await registrationState.roleMoves.run(
            async (handle) => {
              target = sessionID;
              handle.actFor(target);
              const held = registrationState.heldRole();
              const moving = held !== undefined && held !== role;
              // Moving off a role closes every subscription another plugin opened `whileHolding`
              // it, so the session would stop taking that role's notices without being told. The
              // model's tool never does that; the plugin that owns the role moves it by its claim.
              const bound = registrationState.roleBoundSubjects();
              if (moving && bound.length > 0) {
                throw new Error(
                  `This session holds role ${held}, and ${bound.join(", ")} lasts only while it does; envoy_role_set does not move it to ${role}`
                );
              }
              await handle.claimRole(role);
              return { released: moving ? held : undefined };
            },
            (refusal) => {
              claimAdopted = refusal.claimAdopted;
            }
          );
          // The move decided nothing, because the session it was decided for stopped being the
          // open one: the tool says so rather than report a role it never took, and says which
          // of the two the listener left behind.
          if (moved === undefined) throw sessionLeftDuringClaim(target, role, claimAdopted);
          const { released } = moved;
          return toolSuccess(
            released === undefined
              ? `Now holding role: ${role}`
              : `Now holding role: ${role} (released role ${released})`,
            { role, ...(released === undefined ? {} : { released }) }
          );
        }
        case EnvoyToolOperation.getRole: {
          const role = await client.getRole(stringFor(parameters, "role"));
          return toolSuccess(JSON.stringify(role, null, 2), role);
        }
        case EnvoyToolOperation.whoami: {
          const inSubagent = await isSubagent(context);
          const address = await replyAddress(context, "", inSubagent);
          // A `task` subagent registers no Envoy session of its own, so the address a reply
          // reaches is the session that spawned it. Its own host id is reported beside that,
          // never as the reply address: nothing is listening on it.
          const subagent = inSubagent
            ? {
                session_id: context.sessionManager.getSessionId(),
                note:
                  address === ""
                    ? "This session is a task subagent and registers no Envoy session of its own, and this process records no top-level session its transcript traces back to, so it has no reply address at all: say who you are in the message body, and reach the agent that spawned you with a write to its agent:// address."
                    : "This session is a task subagent and registers no Envoy session of its own; a reply to session_id reaches the agent that spawned it, which relays it with a write to your agent:// address. Your own envoy_publish never reaches that agent — use a write to its agent:// address for that hop.",
              }
            : undefined;
          return toolSuccess(
            JSON.stringify(
              {
                session_id: address,
                machine_id: machineID(),
                // A subagent's instance stored no directory either (`restoreLocalSessionState`
                // never ran for it), so the host's live one answers for it.
                dir: sessionDirectory || context.cwd,
                ...(subagent === undefined ? {} : { subagent }),
              },
              null,
              2
            ),
            {
              sessionID: address,
              topics: [...subscriptions.keys()],
              ...(subagent === undefined ? {} : { subagent }),
            }
          );
        }
        case EnvoyToolOperation.listSessions: {
          const machine = optionalStringFor(parameters, "machine");
          const directory = optionalStringFor(parameters, "dir");
          const title = optionalStringFor(parameters, "title");
          const sessions = await client.listSessions({ directory, title });
          const result = sessionsOnMachine(sessions, machine);
          return toolSuccess(JSON.stringify(result, null, 2), {
            count: result.length,
            machine,
            dir: directory,
            title,
          });
        }
        case EnvoyToolOperation.slackPost: {
          const posted = await postSlackReply({
            channel: stringFor(parameters, "channel"),
            threadTs: stringFor(parameters, "thread_ts"),
            text: stringFor(parameters, "text"),
          });
          return toolSuccess(
            `posted ${posted.ts} in ${posted.channel}, thread ${posted.threadTs}`,
            { channel: posted.channel, thread_ts: posted.threadTs, ts: posted.ts }
          );
        }
      }
    } catch (error) {
      return toolFailure(error);
    }
  }
}

function schemaFor(pi: PiApi, operation: EnvoyToolOperation): unknown {
  const spec = envoyToolSpecs.find((candidate) => candidate.operation === operation);
  if (spec === undefined) throw new Error(`missing Envoy tool specification for ${operation}`);
  return pi.zod.object(spec.arguments(zodSchemaApi(pi.zod)));
}

function stringFor(parameters: Record<string, unknown>, key: string): string {
  const value = parameters[key];
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function optionalStringFor(parameters: Record<string, unknown>, key: string): string | undefined {
  const value = parameters[key];
  if (value === undefined) return undefined;
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function isStringArray(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((entry: unknown) => typeof entry === "string");
}

function topicsFor(
  parameters: Record<string, unknown>,
  fallback: readonly string[] = []
): readonly string[] {
  const value = parameters.topics;
  if (value === undefined) return fallback;
  if (!isStringArray(value)) throw new TypeError("topics must be strings");
  return expandSubscriptionTopics(value);
}

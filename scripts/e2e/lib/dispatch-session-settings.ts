// The driver of scripts/e2e/dispatch-session-settings.sh (INFRA-195): it owns every Oh My Pi
// process of the run, each in `--mode rpc` as a Legion pane runs it, and one held-open Chromium
// page signed in to Dispatch as alice, and runs the checks in order. Each check prints
// `== <name>`, what it observed, and `ok <name>`, and keeps a screenshot of the page in
// <evidence>/checks; the first failure prints `FAIL <name>: <why>` and exits 1.
//
//   bun scripts/e2e/lib/dispatch-session-settings.ts run <rig.json>
//     the checks; rig.json is the setup the shell script wrote (no secret is in it: the provider
//     keys reach each Oh My Pi from this process's environment, and nothing else)
//   bun scripts/e2e/lib/dispatch-session-settings.ts db-create <name>
//     creates database <name> in LEGION_TEST_PG_DSN's server and prints a DSN naming it
//   bun scripts/e2e/lib/dispatch-session-settings.ts db-drop <name>
//     drops database <name> there, with whatever is still connected to it
//   bun scripts/e2e/lib/dispatch-session-settings.ts free-port
//     prints a loopback TCP port nothing listens on
//
// What the page shows is never its own oracle. The session's model is RPC `get_state`'s; the
// models it lists are `get_available_models`'; its thinking level and the levels it offers are the
// `Thinking:` and `Available:` lines an `/effort` prompt prints (RPC `command_output`); turns and
// switches are its session file's entries; and who sent a switch is Dispatch's stored message.
//
// This file holds the setup, the checks (one function each, run in order from `checks`) and the
// commands. The parts they drive are its siblings: session-settings-omp.ts (Oh My Pi in
// `--mode rpc`), session-settings-services.ts (Dispatch, the listener and the bus),
// session-settings-browser.ts (the held-open page), session-settings-session-file.ts (the session
// file's entries) and session-settings-common.ts (the rig, the transcript and the waits).
import { cpSync, existsSync, readdirSync, readFileSync } from "node:fs";
import { agentStreamControlSubject } from "../../../packages/contracts/src/agent-stream";
import { Browser, type Header, headerText } from "./session-settings-browser";
import {
  begin,
  CheckFailure,
  currentName,
  currentNumber,
  expect,
  fail,
  note,
  PERSON,
  pass,
  type Rig,
  sleep,
  stage,
  until,
} from "./session-settings-common";
import {
  concrete,
  type Effort,
  effortLevel,
  effortText,
  Omp,
  type SessionState,
} from "./session-settings-omp";
import {
  newMessage,
  Services,
  type StoredRead,
  sessionReply,
  storedIds,
} from "./session-settings-services";
import {
  assistantMessages,
  cards,
  type Entry,
  entries,
  handledAttempts,
  text,
  turnModel,
  userMessages,
} from "./session-settings-session-file";

const [command, ...args] = process.argv.slice(2);

// The caller's provider keys, taken off this process's environment at once: Oh My Pi gets them
// (`Omp.start` is handed them), and nothing else the driver starts inherits them (Chromium,
// launched by Playwright, inherits `process.env`). The imports above load before this line runs;
// none of them starts a process when it loads.
const PROVIDER_KEYS: Readonly<Record<string, string>> = Object.fromEntries(
  ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"].flatMap((key) => {
    const value = process.env[key];
    delete process.env[key];
    return value === undefined || value === "" ? [] : [[key, value]];
  })
);

function refuse(message: string): never {
  process.stderr.write(`dispatch-session-settings: ${message}\n`);
  process.exit(2);
}

const OTHER_PERSON = "bob";
const BROKEN_MODEL = "rig-broken/broken-model";
const RESTART_NOTICE =
  "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here.";
const NO_ANSWER =
  "No answer came from the session within 30 seconds. The header shows the model and thinking level the session last reported.";
/** A1's bound: a change made anywhere but the header shows within this long. */
const SHOWN_WITHIN_MS = 20_000;

// ---- what the checks share ------------------------------------------------------------------------

/** Starts an Oh My Pi the run stops, and keeps the evidence of, however it ends. */
type StartOmp = (
  label: string,
  extension: string | null,
  extra?: readonly string[]
) => Promise<Omp>;

/** The model and level a session reports through RPC alone. */
interface Reading {
  readonly effort: Effort;
  readonly model: string | undefined;
  readonly state: SessionState;
}

/** What the setup hands every check, and what one check hands the checks after it. */
interface Context {
  readonly rig: Rig;
  readonly services: Services;
  readonly startOmp: StartOmp;
  /** The page held open across the checks, on the main session's live view between them. */
  readonly page: Browser;
  /** The two models the caller's keys serve: the profile's default and fallback target first. */
  readonly A: string;
  readonly B: string;
  /** What `/effort` offered on A and on B at the preflight. */
  readonly lists: Readonly<Record<string, Effort>>;
  /** The main session's id. */
  readonly S: string;
  /** The other session, started before every switch, its id, and what it reported at its start. */
  readonly other: Omp;
  readonly O: string;
  readonly otherStart: Reading;
  /** The main session's Oh My Pi: the one the setup started, until resume-keeps-switch ends it
   *  and relaunches the session with --resume; the last process it relaunched from then on. */
  main: Omp;
}

/** Waits until the session `omp` runs is on the listener, seen at or after `since`. */
async function registers(services: Services, omp: Omp, since = 0): Promise<string> {
  const { sessionId } = await omp.state();
  await until(
    `session ${sessionId} (${omp.label}) to register with the listener`,
    120_000,
    async () => ((await services.registered(sessionId, since)) ? true : undefined)
  );
  return sessionId;
}

/** The model and level `omp` reports through RPC alone. */
async function oracle(omp: Omp): Promise<Reading> {
  const state = await omp.state();
  const effort = await omp.effort();
  return { effort, model: state.model, state };
}

/** Waits until the session runs no turn and none is owed. */
async function idle(omp: Omp, limitMs = 240_000): Promise<SessionState> {
  return until(`${omp.label} to settle`, limitMs, async () => {
    const state = await omp.state();
    return !state.isStreaming && state.isSettled ? state : undefined;
  });
}

/** The user messages in the session file of the session `omp` runs. */
async function userCount(omp: Omp): Promise<number> {
  return userMessages((await omp.state()).sessionFile).length;
}

/** A pick from the header and the session's answer to it, shown on the page's switch line: the
 *  stored message it is, and the answer quoted. */
async function pickAndAnswer(
  { page, services }: Context,
  session: string,
  kind: "model" | "thinking",
  value: string,
  wantAnswer: string
): Promise<{ stored: StoredRead; status: string; tookMs: number }> {
  const before = await storedIds(services, session);
  const pickedAt = Date.now();
  if (kind === "model") await page.pickModel(value);
  else await page.pickThinking(value);
  // The switch is a stored message whose reply is the session's answer; only then is the
  // switch line read, so a line an earlier switch left cannot stand in for this one's.
  const body = kind === "model" ? `/model ${value}` : `/effort ${value}`;
  const stored = await newMessage(services, session, body, before);
  const reply = await sessionReply(services, session, stored.message.id, 60_000);
  expect(
    reply === wantAnswer,
    `the session answered "${body}" with "${reply}", want "${wantAnswer}"`
  );
  const want = `The session answered: ${wantAnswer}`;
  const { header } = await page.headerUntil(
    `the switch line "${want}"`,
    SHOWN_WITHIN_MS,
    (h) => h.status === want
  );
  return { status: header.status ?? "", stored, tookMs: Date.now() - pickedAt };
}

/** The person's prompt from the composer to the open session `omp` runs, and the assistant
 *  turns the session ran for it, read from its session file once the run has settled: the
 *  turns after the user message pi-envoy tagged with the prompt's Dispatch id. */
async function promptFromPage(
  { page, services }: Context,
  omp: Omp,
  body: string
): Promise<{ id: string; turns: Entry[] }> {
  const before = await storedIds(services, page.session);
  await page.send(body);
  const sent = await newMessage(services, page.session, body, before);
  const file = (await omp.state()).sessionFile;
  const turns = () => {
    const all = entries(file);
    const at = all.findIndex(
      (entry) =>
        entry.type === "message" &&
        entry.message?.role === "user" &&
        entry.message.tag === sent.message.id
    );
    return at < 0
      ? []
      : all
          .slice(at)
          .filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
  };
  await until(`the turn on "${body}"`, 240_000, () =>
    turns().some((turn) => turn.message?.stopReason !== "toolUse") ? true : undefined
  );
  await idle(omp);
  return { id: sent.message.id, turns: turns() };
}

// ---- the setup ------------------------------------------------------------------------------------

/** What the run launched, which it stops, and keeps the evidence of, however it ends. */
interface Launched {
  readonly omps: Omp[];
  browser: Browser | undefined;
}

/** A bare session's check that both models answer under the caller's keys, the broken model is
 *  listed and the unkeyed one is not; answers what `/effort` offers on each model. */
async function preflight(rig: Rig, startOmp: StartOmp): Promise<Record<string, Effort>> {
  const [A, B] = rig.models;
  stage("preflight");
  const omp = await startOmp("preflight", null);
  const available = await omp.models();
  for (const model of [A, B, BROKEN_MODEL]) {
    expect(
      available.includes(model),
      `${model} is not among the ${available.length} models the session lists as available under the caller's keys`
    );
  }
  expect(
    !available.includes(rig.unkeyedModel),
    `${rig.unkeyedModel} is listed as available, so it cannot stand for a model whose key the session cannot read`
  );
  const lists: Record<string, Effort> = {};
  for (const model of [A, B]) {
    const [provider, ...id] = model.split("/");
    await omp.request({ modelId: id.join("/"), provider, type: "set_model" });
    lists[model] = await omp.effort();
    const prompt = await omp.prompt("Reply with the one word: ready");
    await omp.settled(prompt, 180_000);
    const answer = assistantMessages((await omp.state()).sessionFile).at(-1);
    expect(
      answer !== undefined && turnModel(answer) === model && answer.message?.stopReason === "stop",
      `the one-word preflight prompt on ${model} did not answer: ${JSON.stringify(answer?.message ?? null).slice(0, 400)}`
    );
    note(
      `${model}: answered "${text(answer?.message?.content).trim()}"; /effort offers ${effortText(lists[model] as Effort)}`
    );
  }
  expect(
    JSON.stringify(concrete(lists[A] as Effort)) !== JSON.stringify(concrete(lists[B] as Effort)),
    `${A} and ${B} offer the same /effort levels, so a model pick cannot show the level changing`
  );
  await omp.stop();
  return lists;
}

/** The preflight, then the main and the other session, and the page on the main one's live view. */
async function setUp(
  rig: Rig,
  services: Services,
  startOmp: StartOmp,
  launched: Launched
): Promise<Context> {
  const [A, B] = rig.models;
  const lists = await preflight(rig, startOmp);
  stage("sessions");
  const main = await startOmp("main", rig.plugin);
  const other = await startOmp("other", rig.plugin);
  const S = await registers(services, main);
  const O = await registers(services, other);
  const otherStart = await oracle(other);
  note(
    `main session ${S} (pid ${main.pid}); other session ${O} (pid ${other.pid}) on ${otherStart.model}, ${effortText(otherStart.effort)}`
  );
  const page = await Browser.open(rig);
  launched.browser = page;
  await page.live(S);
  const firstReplay = await page.replay(S);
  expect(
    firstReplay.some((frame) => frame.kind === "commands"),
    `the first replay of ${S} has no commands frame, so this Oh My Pi (LEGION_TEST_OMP=${rig.omp}) cannot run typed input from Dispatch: ${JSON.stringify(firstReplay.map((frame) => frame.kind))}`
  );
  note(`first replay: ${firstReplay.map((frame) => frame.kind).join(", ")}`);
  return { A, B, lists, main, O, other, otherStart, page, rig, S, services, startOmp };
}

// ---- the checks -----------------------------------------------------------------------------------

async function headerBeforeAnyTurn({ main, page, S }: Context): Promise<void> {
  const { effort, model } = await oracle(main);
  const listed = await main.models();
  expect(
    !("none" in effort),
    `/effort in ${S} reports no adjustable thinking: ${effortText(effort)}`
  );
  const level = effortLevel(effort);
  const { header } = await page.headerUntil(
    `${model} and ${level}`,
    60_000,
    (h) => h.model === model && h.thinking === level
  );
  await page.shot("header-before-any-turn");
  expect(
    header.modelPicker && header.thinkingPicker,
    `the header offers no picker: ${headerText(header)}`
  );
  expect(
    JSON.stringify([...header.modelOptions].sort()) === JSON.stringify([...listed].sort()),
    `the model picker offers ${header.modelOptions.length} models, get_available_models lists ${listed.length}; only in the picker: ${header.modelOptions.filter((m) => !listed.includes(m)).join(", ")}; only listed: ${listed.filter((m) => !header.modelOptions.includes(m)).join(", ")}`
  );
  const offered = "none" in effort ? [] : effort.available;
  expect(
    JSON.stringify(header.thinkingOptions) === JSON.stringify(offered),
    `the thinking picker offers [${header.thinkingOptions.join(", ")}], /effort offers [${offered.join(", ")}]`
  );
  const file = (await main.state()).sessionFile;
  expect(assistantMessages(file).length === 0, `${S} has run a turn already`);
  expect((await page.thread()).turnModels.length === 0, "the page shows a turn");
  note(`get_state ${model}; /effort ${effortText(effort)}; header ${headerText(header)}`);
  note(
    `the model picker offers the ${listed.length} models get_available_models lists; the thinking picker offers [${header.thinkingOptions.join(", ")}]; no turn has run`
  );
}

async function terminalSetModel({ A, B, main, page }: Context): Promise<void> {
  const users = await userCount(main);
  for (const model of [B, A]) {
    const [provider, ...id] = model.split("/");
    await main.request({ modelId: id.join("/"), provider, type: "set_model" });
    const { tookMs } = await page.headerUntil(model, SHOWN_WITHIN_MS, (h) => h.model === model);
    const state = await main.state();
    expect(state.model === model, `get_state reports ${state.model} after set_model ${model}`);
    note(
      `RPC set_model ${model}: get_state ${state.model}; the header showed it ${tookMs} ms after the RPC answer`
    );
  }
  await page.shot("terminal-set-model");
  expect((await userCount(main)) === users, "the session file gained a user message");
  note(`the session file holds ${users} user messages, as before`);
}

async function terminalSetThinking({ main, page }: Context): Promise<void> {
  const users = await userCount(main);
  const level = "medium";
  await main.request({ level, type: "set_thinking_level" });
  const { tookMs } = await page.headerUntil(level, SHOWN_WITHIN_MS, (h) => h.thinking === level);
  const effort = await main.effort();
  expect(
    effortLevel(effort) === level,
    `/effort reports ${effortText(effort)} after set_thinking_level ${level}`
  );
  expect((await userCount(main)) === users, "the session file gained a user message");
  await page.shot("terminal-set-thinking");
  note(
    `RPC set_thinking_level ${level}: /effort ${effortText(effort)}; the header showed it ${tookMs} ms after the RPC answer; no user message`
  );
}

async function terminalTypedEffort({ main, page }: Context): Promise<void> {
  const users = await userCount(main);
  const level = "high";
  const printed = await main.typed(`/effort ${level}`);
  expect(
    printed.includes(`Thinking set to ${level}.`),
    `/effort ${level} printed ${JSON.stringify(printed)}`
  );
  const { tookMs } = await page.headerUntil(level, SHOWN_WITHIN_MS, (h) => h.thinking === level);
  const effort = await main.effort();
  expect(effortLevel(effort) === level, `/effort reports ${effortText(effort)}`);
  expect((await userCount(main)) === users, "the session file gained a user message");
  await page.shot("terminal-typed-effort");
  note(
    `RPC prompt "/effort ${level}" printed ${JSON.stringify(printed)}; the header showed ${level} ${tookMs} ms after the RPC answer; no user message`
  );
}

async function messageSwitch({ B, main, page, S, services }: Context): Promise<void> {
  const before = await storedIds(services, S);
  const sentAt = Date.now();
  await page.send(`/model ${B}`);
  const { tookMs } = await page.headerUntil(B, SHOWN_WITHIN_MS, (h) => h.model === B);
  const stored = await newMessage(services, S, `/model ${B}`, before);
  const reply = await sessionReply(services, S, stored.message.id, 60_000);
  const state = await main.state();
  expect(state.model === B, `get_state reports ${state.model}`);
  await page.shot("message-switch");
  note(
    `the composer sent "/model ${B}" (message ${stored.message.id}); the header showed ${B} ${tookMs} ms after Send (${Date.now() - sentAt} ms to here); the session replied "${reply}"; get_state ${state.model}`
  );
}

async function pickerModel(context: Context): Promise<void> {
  const { A, main, page, S, services } = context;
  const { status, stored, tookMs } = await pickAndAnswer(
    context,
    S,
    "model",
    A,
    `Model set to ${A}.`
  );
  expect(
    stored.message.author.kind === "user" && stored.message.author.id === PERSON,
    `Dispatch stored "/model ${A}" as ${JSON.stringify(stored.message.author)}, want ${PERSON}`
  );
  const reply =
    stored.replies.find((each) => each.author.kind === "session")?.body ??
    (await sessionReply(services, S, stored.message.id, 30_000));
  expect(reply === `Model set to ${A}.`, `the session replied "${reply}"`);
  const shown = await until("the thread to show the switch and its answer", 30_000, async () => {
    const thread = await page.thread();
    return thread.mine.includes(`/model ${A}`) &&
      thread.replies.some((each) => each.includes(`Model set to ${A}.`))
      ? thread
      : undefined;
  });
  const { effort, model } = await oracle(main);
  expect(model === A, `get_state reports ${model}`);
  const { header } = await page.headerUntil(
    `${A} and ${effortLevel(effort)}`,
    SHOWN_WITHIN_MS,
    (h) => h.model === A && h.thinking === effortLevel(effort)
  );
  note(
    `picked ${A}: Dispatch stored "/model ${A}" by ${stored.message.author.kind} ${stored.message.author.id} (message ${stored.message.id}); the switch line read "${status}" ${tookMs} ms after the pick; the thread shows "/model ${A}" as ${PERSON}'s and the reply "${reply}"`
  );
  note(
    `get_state ${model}; /effort ${effortText(effort)}; header ${headerText(header)}; ${shown.mine.length} of ${PERSON}'s messages shown`
  );
  const body = "Reply with the one word: alpha";
  const prompt = await promptFromPage(context, main, body);
  expect(
    prompt.turns.length > 0 && prompt.turns.every((turn) => turnModel(turn) === A),
    `the prompt's turns ran on ${prompt.turns.map(turnModel).join(", ")}, want ${A}`
  );
  const labels = await until(`the page to label the turn ${A}`, 30_000, async () => {
    const thread = await page.thread();
    return thread.turnModels.at(-1) === A ? thread.turnModels : undefined;
  });
  await page.shot("picker-model");
  note(
    `the composer's "${body}" (message ${prompt.id}) ran ${prompt.turns.length} turn(s), each on ${A} in the session file; the page labels the turn ${labels.at(-1)}`
  );
}

async function pickerAutoKept(context: Context): Promise<void> {
  const { B, main, page, S } = context;
  const auto = await pickAndAnswer(context, S, "thinking", "auto", "Thinking set to auto.");
  await page.headerUntil("auto", SHOWN_WITHIN_MS, (h) => h.thinking === "auto");
  const model = await pickAndAnswer(context, S, "model", B, `Model set to ${B}.`);
  const { effort, model: running } = await oracle(main);
  expect(running === B, `get_state reports ${running}`);
  expect(
    effortLevel(effort) === "auto",
    `/effort reports ${effortText(effort)} after the model pick, want auto`
  );
  const { header } = await page.headerUntil(
    `${B} and auto`,
    SHOWN_WITHIN_MS,
    (h) => h.model === B && h.thinking === "auto"
  );
  await page.shot("picker-auto-kept");
  note(
    `picked auto ("${auto.status}"), then ${B} ("${model.status}"): get_state ${running}; /effort ${effortText(effort)}; header ${headerText(header)}`
  );
}

async function pickerLevelAfterModel(context: Context): Promise<void> {
  const { A, B, lists, main, page, rig, S, services, startOmp } = context;
  const onB = concrete(lists[B] as Effort);
  const onA = concrete(lists[A] as Effort);
  const level = onB.find((each) => !onA.includes(each)) ?? (onB.at(-1) as string);
  await pickAndAnswer(context, S, "thinking", level, `Thinking set to ${level}.`);
  await page.headerUntil(level, SHOWN_WITHIN_MS, (h) => h.thinking === level);
  const startState = await oracle(main);
  expect(
    startState.model === B && effortLevel(startState.effort) === level,
    `the main session starts at ${startState.model}, ${effortText(startState.effort)}`
  );
  // The twin: a second session of the same profile, brought to the same model and level
  // through RPC, then sent `/model <A>` as a person's message through Dispatch.
  const twin = await startOmp("twin", rig.plugin);
  const T = await registers(services, twin);
  const [provider, ...id] = B.split("/");
  await twin.request({ modelId: id.join("/"), provider, type: "set_model" });
  await twin.request({ level, type: "set_thinking_level" });
  const twinStart = await oracle(twin);
  expect(
    twinStart.model === B && effortLevel(twinStart.effort) === level,
    `the twin starts at ${twinStart.model}, ${effortText(twinStart.effort)}`
  );
  const sent = await services.send(T, `/model ${A}`, PERSON);
  const twinReply = await sessionReply(services, T, sent.id, 60_000);
  expect(twinReply === `Model set to ${A}.`, `the twin replied "${twinReply}"`);
  const twinAfter = await oracle(twin);
  const expected = effortLevel(twinAfter.effort);
  await twin.stop();
  const picked = await pickAndAnswer(context, S, "model", A, `Model set to ${A}.`);
  const after = await oracle(main);
  const { header } = await page.headerUntil(
    `${A} and ${expected}`,
    SHOWN_WITHIN_MS,
    (h) => h.model === A && h.thinking === expected
  );
  expect(
    after.model === A && effortLevel(after.effort) === expected,
    `the main session's /effort reports ${effortText(after.effort)} on ${after.model}, the twin's ${expected}`
  );
  await page.shot("picker-level-after-model");
  note(
    `both at ${B} and ${level}; the twin ${T}, sent "/model ${A}" as a message, then reports ${twinAfter.model}, ${effortText(twinAfter.effort)}`
  );
  note(
    `the main session, picked ${A} in the header ("${picked.status}"): get_state ${after.model}, /effort ${effortText(after.effort)}; header ${headerText(header)}`
  );
}

async function pickerEffort(context: Context): Promise<void> {
  const { main, page, S } = context;
  const before = await main.effort();
  const level =
    concrete(before).find((each) => each !== effortLevel(before) && each === "medium") ??
    (concrete(before).find((each) => each !== effortLevel(before)) as string);
  const picked = await pickAndAnswer(context, S, "thinking", level, `Thinking set to ${level}.`);
  const effort = await main.effort();
  expect(effortLevel(effort) === level, `/effort reports ${effortText(effort)}, want ${level}`);
  const { header } = await page.headerUntil(level, SHOWN_WITHIN_MS, (h) => h.thinking === level);
  await page.shot("picker-effort");
  note(
    `picked ${level} from ${effortText(before)} ("${picked.status}" ${picked.tookMs} ms after the pick); /effort ${effortText(effort)}; header ${headerText(header)}`
  );
}

async function pickerDuringTurn(context: Context): Promise<void> {
  const { A, B, main, page, S, services } = context;
  const file = (await main.state()).sessionFile;
  const startAt = entries(file).length;
  const long =
    "Use your bash tool to run exactly this command in the foreground: sleep 45 . When it has finished, reply with the one word: done";
  const prompt = await main.prompt(long);
  await until("the long turn to be running its command", 120_000, async () => {
    const state = await main.state();
    const running = entries(file)
      .slice(startAt)
      .some((entry) => entry.type === "message" && entry.message?.role === "assistant");
    return state.isStreaming && running ? true : undefined;
  });
  const before = await storedIds(services, S);
  const pickedAt = Date.now();
  await page.pickModel(B);
  const want = `The session answered: Model set to ${B}.`;
  await page.headerUntil(`the switch line "${want}"`, 30_000, (h) => h.status === want);
  const answeredMs = Date.now() - pickedAt;
  const during = await main.state();
  expect(during.isStreaming, "the turn had ended by the time the session answered the switch");
  const stored = await newMessage(services, S, `/model ${B}`, before);
  note(
    `picked ${B} while the turn ran; the session answered "${want.slice("The session answered: ".length)}" ${answeredMs} ms later (message ${stored.message.id}), with the turn still running (get_state isStreaming ${during.isStreaming}, model ${during.model})`
  );
  await main.settled(prompt, 300_000);
  await idle(main);
  // The run's first request, which asked for the command, was on the old model; every
  // request the session started after it answered the switch (the run's continuation once the
  // command returned) is on the picked one.
  const answeredAt = pickedAt + answeredMs;
  const run = entries(file)
    .slice(startAt)
    .filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
  const later = run.filter((entry) => (entry.message?.timestamp ?? 0) > answeredAt);
  note(
    `the long prompt's run: ${run.map((entry) => `${turnModel(entry)} (${entry.message?.stopReason}, started ${(entry.message?.timestamp ?? 0) > answeredAt ? "after" : "before"} the answer)`).join(", ")}`
  );
  expect(
    turnModel(run[0] as Entry) === A,
    `the running turn started on ${turnModel(run[0] as Entry)}, want ${A}`
  );
  expect(
    later.every((entry) => turnModel(entry) === B),
    `a request started after the switch's answer ran on ${later.map(turnModel).join(", ")}, want ${B}`
  );
  const body = "Reply with the one word: bravo";
  const next = await promptFromPage(context, main, body);
  expect(
    next.turns.length > 0 && next.turns.every((turn) => turnModel(turn) === B),
    `the next turn ran on ${next.turns.map(turnModel).join(", ")}, want ${B}`
  );
  await page.shot("picker-during-turn");
  note(`the next prompt, "${body}", ran on ${next.turns.map(turnModel).join(", ")}`);
}

async function pickerRefusal(context: Context): Promise<void> {
  const { main, page, rig, S, services } = context;
  const before = await oracle(main);
  const replay = await page.replay(S);
  const settings = replay.filter((frame) => frame.kind === "settings").at(-1);
  expect(
    settings?.reported === true,
    `the replay of ${S} carries no reported settings frame: ${JSON.stringify(settings ?? null)}`
  );
  const top = Math.max(...replay.map((frame) => frame.seq));
  const injected = {
    ...settings,
    models: [...(settings?.models as string[]), rig.unkeyedModel],
    seq: top + 1_000_000,
  };
  await services.publishFrame(S, injected);
  await page.headerUntil(`${rig.unkeyedModel} among the models`, SHOWN_WITHIN_MS, (h) =>
    h.modelOptions.includes(rig.unkeyedModel)
  );
  note(
    `a bare bus client published on agentstream.${S}.frames a reported settings frame at seq ${injected.seq} (the replay's highest is ${top}) listing the session's ${(settings?.models as string[]).length} models and ${rig.unkeyedModel}`
  );
  const want = `Failed to set model: No API key for ${rig.unkeyedModel}`;
  const picked = await pickAndAnswer(context, S, "model", rig.unkeyedModel, want);
  const after = await oracle(main);
  expect(after.model === before.model, `get_state reports ${after.model}, was ${before.model}`);
  expect(
    effortText(after.effort) === effortText(before.effort),
    `/effort reports ${effortText(after.effort)}, was ${effortText(before.effort)}`
  );
  const { header } = await page.headerUntil(
    `${before.model}`,
    5_000,
    (h) => h.model === before.model
  );
  await page.shot("picker-refusal");
  note(
    `picked ${rig.unkeyedModel}: the switch line read "${picked.status}"; get_state ${after.model} and /effort ${effortText(after.effort)}, as before; header ${headerText(header)}`
  );
  await page.reload();
  const listed = await main.models();
  const restored = await page.headerUntil(
    "the session's own list after a reload",
    60_000,
    (h) =>
      h.model === before.model &&
      !h.modelOptions.includes(rig.unkeyedModel) &&
      h.modelOptions.length === listed.length
  );
  note(
    `after a reload the header offers the session's own ${restored.header.modelOptions.length} models again`
  );
}

async function forgedSwitchChangesNothing({
  A,
  B,
  main,
  page,
  S,
  services,
}: Context): Promise<void> {
  const before = await oracle(main);
  const forgedModel = before.model === B ? A : B;
  const forgedLevel = effortLevel(before.effort) === "high" ? "low" : "high";
  const file = before.state.sessionFile;
  const users = userMessages(file).length;
  const forgedId = crypto.randomUUID();
  const run = forgedId.slice(0, 8);
  await services.forgeDispatchFrame(S, forgedId, `/model ${forgedModel}`, `forged-${run}`);
  const sender = `rig-sender-${run}`;
  await services.registerClient(sender);
  await services.envoyMessage(sender, S, `/effort ${forgedLevel}`, `envoy-${run}`);
  await until("the forged frame and the Envoy message to arrive as cards", 120_000, () => {
    const all = entries(file);
    return handledAttempts(all).includes(forgedId) &&
      cards(all, `/model ${forgedModel}`).length > 0 &&
      cards(all, `/effort ${forgedLevel}`).length > 0
      ? true
      : undefined;
  });
  await idle(main);
  const after = await oracle(main);
  await page.shot("forged-switch-changes-nothing");
  note(`before: get_state ${before.model}, /effort ${effortText(before.effort)}`);
  note(
    `if either were applied: the model would read ${forgedModel} (not ${before.model}) or the level ${forgedLevel} (not ${effortLevel(before.effort)})`
  );
  note(
    `a frame forged with the listener token, a person-shaped steer naming message ${forgedId}, which Dispatch never stored, with body "/model ${forgedModel}": recorded as handled attempt ${forgedId} and delivered as a card`
  );
  note(
    `an Envoy message "/effort ${forgedLevel}" from the registered client ${sender}: delivered as a card`
  );
  note(`after: get_state ${after.model}, /effort ${effortText(after.effort)}`);
  expect(after.model === before.model, `the forged switch changed the model to ${after.model}`);
  expect(
    effortText(after.effort) === effortText(before.effort),
    `the Envoy message changed the thinking level to ${effortText(after.effort)}`
  );
  const raw = userMessages(file)
    .slice(users)
    .map((entry) => text(entry.message?.content));
  expect(
    !raw.some(
      (each) => each.includes(`/model ${forgedModel}`) || each.includes(`/effort ${forgedLevel}`)
    ),
    `a forged switch became a user message: ${JSON.stringify(raw)}`
  );
}

async function agentTokenRefused({ A, B, main, page, S, services }: Context): Promise<void> {
  const before = await oracle(main);
  const stored = (await services.thread(S)).length;
  const target = before.model === B ? A : B;
  const answer = await services.sendAsAgent(S, `/model ${target}`);
  const code =
    (answer.body as { code?: string; error?: { code?: string } } | null)?.code ??
    (answer.body as { error?: { code?: string } } | null)?.error?.code;
  expect(
    answer.status === 403 && JSON.stringify(answer.body).includes("HUMAN_ONLY"),
    `the agent bearer's switch answered ${answer.status} ${JSON.stringify(answer.body)}`
  );
  await sleep(3_000);
  const after = await oracle(main);
  expect((await services.thread(S)).length === stored, "Dispatch stored the agent's switch");
  expect(
    after.model === before.model && effortText(after.effort) === effortText(before.effort),
    `the session changed: ${after.model}, ${effortText(after.effort)}`
  );
  await page.shot("agent-token-refused");
  note(
    `POST /api/v1/agents/${S}/messages "/model ${target}" with the session's Dispatch bearer: ${answer.status} ${code ?? ""} ${JSON.stringify(answer.body)}`
  );
  note(
    `Dispatch still stores ${stored} messages; get_state ${after.model}, /effort ${effortText(after.effort)}, as before`
  );
}

async function otherSessionUnchanged({ O, other, otherStart, page }: Context): Promise<void> {
  const now = await oracle(other);
  expect(
    now.model === otherStart.model && effortText(now.effort) === effortText(otherStart.effort),
    `the other session is at ${now.model}, ${effortText(now.effort)}, started at ${otherStart.model}, ${effortText(otherStart.effort)}`
  );
  const changes = entries(now.state.sessionFile).filter(
    (entry) => entry.type === "model_change" || entry.type === "thinking_level_change"
  );
  expect(
    changes.length <= 2,
    `the other session's file records ${changes.length} model or level changes`
  );
  await page.shot("other-session-unchanged");
  note(
    `the other session ${O}, started before every switch: get_state ${now.model}, /effort ${effortText(now.effort)}, as at its start; ${changes.length} change entries in its file (its start's own)`
  );
}

async function noAnswer({ A, B, main, page, rig, S, services }: Context): Promise<void> {
  const before = await oracle(main);
  const shown = await page.header();
  const target = before.model === B ? A : B;
  const storedBefore = await storedIds(services, S);
  main.signal("SIGSTOP");
  note(`SIGSTOP Oh My Pi pid ${main.pid}`);
  const pickedAt = Date.now();
  await page.pickModel(target);
  const stored = await newMessage(services, S, `/model ${target}`, storedBefore);
  // Dispatch commits the message before it asks the listener for the target and records the
  // attempt, so a read can hold the message with no attempt yet: wait for the attempt.
  const attempt = await until(
    `Dispatch to record an attempt to deliver message ${stored.message.id}`,
    30_000,
    async () =>
      (await services.thread(S))
        .find((read) => read.message.id === stored.message.id)
        ?.message.deliveries.at(-1)
  );
  expect(attempt.state !== "failed", `Dispatch's send failed: ${JSON.stringify(attempt)}`);
  const { header } = await page.headerUntil(
    "the no-answer line",
    45_000,
    (h) => h.status === NO_ANSWER
  );
  const waited = Date.now() - pickedAt;
  expect(
    header.model === shown.model && header.thinking === shown.thinking,
    `the header moved from ${headerText(shown)} to ${headerText(header)}`
  );
  expect(!header.pickersDisabled, "the pickers are still disabled after the no-answer line");
  await page.shot("no-answer");
  note(
    `picked ${target} (message ${stored.message.id}, attempt ${attempt.attempt} ${attempt.state}); ${waited} ms later the switch line read "${header.status}"; the header still shows ${header.model}, ${header.thinking}, pickers enabled`
  );
  const createdAt = Date.parse(stored.message.created_at);
  const wait = createdAt + 65_000 - Date.now();
  if (wait > 0) await sleep(wait);
  main.signal("SIGCONT");
  note(`SIGCONT ${Math.round((Date.now() - createdAt) / 1000)}s after the message was stored`);
  const file = before.state.sessionFile;
  await until("the late frame to arrive as a card", 120_000, () => {
    const all = entries(file);
    return handledAttempts(all).includes(stored.message.id) &&
      cards(all, `/model ${target}`).length > 0
      ? true
      : undefined;
  });
  // The session logs Dispatch's refusal of its accept by the refusal's own text, which
  // Dispatch writes only with ACCEPT_STALE (message_delivery.go); the same accept, asked again
  // with the session's bearer, answers the code itself.
  const logs = readdirSync(`${rig.home}/.omp/logs`).filter((name) =>
    name.endsWith(`.${main.pid}.log`)
  );
  const stale = await until("the session's log to record the refused accept", 30_000, () => {
    const lines = logs.flatMap((name) =>
      readFileSync(`${rig.home}/.omp/logs/${name}`, "utf8").split("\n")
    );
    return lines.find(
      (line) =>
        line.includes(stored.message.id) &&
        line.includes(`attempt ${attempt.attempt} was requested more than a minute ago`)
    );
  });
  const again = await services.accept(stored.message.id, attempt.attempt, S);
  expect(
    again.status === 409 && JSON.stringify(again.body).includes('"ACCEPT_STALE"'),
    `the accept asked again answered ${again.status} ${JSON.stringify(again.body)}, want 409 ACCEPT_STALE`
  );
  await idle(main);
  const after = await oracle(main);
  expect(after.model === before.model, `get_state reports ${after.model}, was ${before.model}`);
  const delivered = (await services.thread(S)).find(
    (read) => read.message.id === stored.message.id
  );
  expect(
    delivered?.message.deliveries.every((each) => each.accepted_as === null) === true,
    `Dispatch records the late switch accepted: ${JSON.stringify(delivered?.message.deliveries)}`
  );
  note(`the session's log: ${stale.slice(0, 400)}`);
  note(`the same accept asked again: ${again.status} ${JSON.stringify(again.body)}`);
  note(
    `the frame arrived as a card and a handled attempt; get_state ${after.model}, /effort ${effortText(after.effort)}, as before; Dispatch records no acceptance`
  );
}

/** Waits up to SHOWN_WITHIN_MS for the page held open across the relaunches to show `what`.
 *  On a timeout it reloads the page, to tell a page that missed the report from a session
 *  that never made it, and fails naming `step`. */
function heldShows(
  page: Browser,
  step: string,
  what: string,
  wanted: (h: Header) => boolean
): Promise<{ header: Header; tookMs: number }> {
  return page
    .headerUntil(`${what} on the page held open across the relaunch`, SHOWN_WITHIN_MS, wanted)
    .catch(async (error: unknown) => {
      const missed = error instanceof Error ? error.message : String(error);
      await page.reload();
      const reloaded = await page
        .headerUntil(`${what} after a reload`, 60_000, wanted)
        .catch((again: unknown) =>
          fail(
            `${step}: ${missed}; after a reload, ${again instanceof Error ? again.message : String(again)}: the session never reported it`
          )
        );
      return fail(
        `${step}: ${missed}; after a reload the header shows ${headerText(reloaded.header)}: the session reported it and the page held open across the relaunch never applied it`
      );
    });
}

/** Ends the main session's Oh My Pi and relaunches the session with --resume, twice; leaves the
 *  last relaunched process in `context.main`. */
async function resumeKeepsSwitch(context: Context): Promise<void> {
  const { A, B, main, page, rig, S, services, startOmp } = context;
  const before = await oracle(main);
  const file = before.state.sessionFile;
  const picked = before.model;
  if (picked === undefined) fail(`get_state of ${S} reports no model`);

  // The switch stays (A2): the session relaunched with --resume is at the model and level
  // picked last.
  note("step 1, the switch stays:");
  await main.stop();
  note(`ended Oh My Pi pid ${main.pid} at ${picked}, ${effortText(before.effort)}`);
  const relaunched = Date.now();
  const first = await startOmp("resumed", rig.plugin, ["--resume", file]);
  context.main = first;
  const state = await first.state();
  expect(state.sessionId === S, `--resume opened session ${state.sessionId}, want ${S}`);
  await registers(services, first, relaunched);
  const registeredAt = Date.now();
  const after = await oracle(first);
  expect(
    after.model === picked && effortText(after.effort) === effortText(before.effort),
    `the resumed session is at ${after.model}, ${effortText(after.effort)}, was ${picked}, ${effortText(before.effort)}`
  );
  note(
    `relaunched with --resume ${file} (pid ${first.pid}), registered ${registeredAt - relaunched} ms after the launch: session ${state.sessionId}; get_state ${after.model}, /effort ${effortText(after.effort)}`
  );

  // Numbering: the page stays open across the relaunch, as a person's does when a session is
  // relaunched under it. Once the relay's watch has reached the resumed process, a change it
  // makes at its own terminal shows within 20 s with no reload (A1), as it did before the
  // relaunch: its frames number above the earlier process's.
  note("step 2, numbering:");
  const subject = agentStreamControlSubject(S);
  const subscribedAt = Date.now();
  const control = await services.nextControl(S, 30_000);
  if (control === undefined)
    fail(
      `numbering: no control message reached ${subject} within 30s of the rig subscribing, ${subscribedAt - registeredAt} ms after the resumed session registered`
    );
  const controlAt = Date.now();
  await sleep(1_000);
  note(
    `the first control message on ${subject} came ${controlAt - subscribedAt} ms after the rig subscribed, ${controlAt - registeredAt} ms after the resumed session registered: ${control.slice(0, 200)}; then waited ${Date.now() - controlAt} ms for the process to handle it`
  );
  const level = effortLevel(after.effort);
  const held = concrete(after.effort).find((each) => each !== level);
  if (held === undefined) fail(`/effort offers no other level than ${effortText(after.effort)}`);
  await first.request({ level: held, type: "set_thinking_level" });
  const { tookMs: heldTookMs } = await heldShows(
    page,
    "numbering",
    held,
    (h) => h.thinking === held
  );
  note(
    `RPC set_thinking_level ${held} on pid ${first.pid}: the held page showed it ${heldTookMs} ms after the RPC answer`
  );
  await first.request({ level, type: "set_thinking_level" });
  const { tookMs: backTookMs } = await heldShows(
    page,
    "numbering",
    `${picked} and ${level} again`,
    (h) => h.model === picked && h.thinking === level
  );
  note(
    `RPC set_thinking_level ${level}: the held page showed it ${backTookMs} ms after the RPC answer`
  );

  // Arming: a relaunch onto the other model, with no change after it, shows on the held page
  // within 20 s of registering: the relay's watch arms the new process, which publishes its
  // settings as they stand.
  note("step 3, arming:");
  const other = picked === A ? B : A;
  await first.stop();
  note(`ended Oh My Pi pid ${first.pid} at ${picked}, ${level}`);
  const relaunchedAgain = Date.now();
  const second = await startOmp("rearmed", rig.plugin, ["--resume", file, "--model", other]);
  context.main = second;
  const secondState = await second.state();
  expect(
    secondState.sessionId === S,
    `arming: --resume opened session ${secondState.sessionId}, want ${S}`
  );
  expect(
    secondState.model === other,
    `arming: --resume ${file} --model ${other} opened the session at ${secondState.model}`
  );
  await registers(services, second, relaunchedAgain);
  const registeredAgainAt = Date.now();
  const { header: armed } = await heldShows(
    page,
    "arming",
    `${other}, with no change and no reload after the relaunch,`,
    (h) => h.model === other
  );
  const armedMs = Date.now() - registeredAgainAt;
  await page.shot("resume-keeps-switch");
  const moved = await oracle(second);
  note(
    `relaunched with --resume ${file} --model ${other} (pid ${second.pid}), registered ${registeredAgainAt - relaunchedAgain} ms after the launch: get_state ${secondState.model}, /effort ${effortText(moved.effort)}; with no RPC change and no reload the held page showed ${other} ${armedMs} ms after the registration: ${headerText(armed)}`
  );
  const [provider, ...id] = picked.split("/");
  await second.request({ modelId: id.join("/"), provider, type: "set_model" });
  let restored = await oracle(second);
  if (effortLevel(restored.effort) !== level) {
    await second.request({ level, type: "set_thinking_level" });
    restored = await oracle(second);
  }
  expect(
    restored.model === picked && effortText(restored.effort) === effortText(before.effort),
    `back on ${picked}, the session is at ${restored.model}, ${effortText(restored.effort)}, was ${effortText(before.effort)}`
  );
  note(
    `RPC set_model ${picked}${effortLevel(moved.effort) === level ? "" : ` and set_thinking_level ${level}`}: get_state ${restored.model}, /effort ${effortText(restored.effort)}`
  );

  await page.reload();
  const { header } = await page.headerUntil(
    `${picked} and ${level}`,
    60_000,
    (h) => h.model === picked && h.thinking === level && h.modelPicker
  );
  note(`the reloaded header ${headerText(header)}`);
}

async function newSessionDefaults({
  A,
  main,
  page,
  rig,
  S,
  services,
  startOmp,
}: Context): Promise<void> {
  const fresh = await startOmp("new", rig.plugin);
  const N = await registers(services, fresh);
  const now = await oracle(fresh);
  expect(
    now.model === A && effortLevel(now.effort) === rig.defaultLevel,
    `the new session is at ${now.model}, ${effortText(now.effort)}, want the profile's ${A} and ${rig.defaultLevel}`
  );
  await page.live(N);
  const { header } = await page.headerUntil(
    `${A} and ${rig.defaultLevel}`,
    60_000,
    (h) => h.model === A && h.thinking === rig.defaultLevel
  );
  await page.shot("new-session-defaults");
  const switched = await oracle(main);
  note(
    `a new session ${N} of the same profile: get_state ${now.model}, /effort ${effortText(now.effort)}; its header ${headerText(header)}; the switched session stays at ${switched.model}, ${effortText(switched.effort)}`
  );
  await fresh.stop();
  await page.live(S);
}

async function fallback(context: Context): Promise<void> {
  const { A, main, page, S, services } = context;
  await page.headerUntil("the resumed session's pickers", 60_000, (h) => h.modelPicker);
  const picked = await pickAndAnswer(
    context,
    S,
    "model",
    BROKEN_MODEL,
    `Model set to ${BROKEN_MODEL}.`
  );
  await page.headerUntil(BROKEN_MODEL, SHOWN_WITHIN_MS, (h) => h.model === BROKEN_MODEL);
  const file = (await main.state()).sessionFile;
  const startAt = entries(file).length;
  const body = "Reply with the one word: charlie";
  const before = await storedIds(services, S);
  await page.send(body);
  await newMessage(services, S, body, before);
  const change = await until("a model_change with role fallback", 240_000, () =>
    entries(file)
      .slice(startAt)
      .find((entry) => entry.type === "model_change" && entry.role === "fallback")
  );
  const entryAt = Date.parse(change.timestamp ?? "");
  const usersAtFallback = userMessages(file).length;
  const { header } = await page.headerUntil(A, SHOWN_WITHIN_MS + 10_000, (h) => h.model === A);
  const shownAt = Date.now();
  expect(change.model === A, `the fallback moved the session to ${change.model}, want ${A}`);
  expect(
    shownAt - entryAt <= SHOWN_WITHIN_MS,
    `the header showed ${A} ${shownAt - entryAt} ms after the fallback entry`
  );
  expect(
    userMessages(file).length === usersAtFallback,
    "a user turn ran between the fallback and the header showing it"
  );
  await idle(main);
  // What each request of the prompt's run did, the retry on the fallback model included: the
  // check holds on the header, and the retry's own outcome is the host's, reported as seen.
  const turns = entries(file)
    .slice(startAt)
    .filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
  await page.shot("fallback");
  note(
    `picked ${BROKEN_MODEL} ("${picked.status}"), then sent "${body}"; the run's requests: ${turns
      .map(
        (turn) =>
          `${turnModel(turn)} ${turn.message?.stopReason}${turn.message?.errorMessage ? ` (${turn.message.errorMessage.split("\n")[0]?.slice(0, 160)})` : ""}`
      )
      .join("; ")}`
  );
  expect(
    turnModel(turns[0] as Entry) === BROKEN_MODEL && turns[0]?.message?.stopReason === "error",
    `the prompt's first request ran on ${turnModel(turns[0] as Entry)}, want a failed one on ${BROKEN_MODEL}`
  );
  note(
    `the session file's model_change at ${change.timestamp}: role ${change.role}, model ${change.model}; the header showed ${header.model} ${shownAt - entryAt} ms after it, with no further user message`
  );
}

async function olderPluginNotice({ page, rig, S, services, startOmp }: Context): Promise<void> {
  const older = await startOmp("released", rig.releasedPlugin);
  const R = await registers(services, older);
  const prompt = await older.prompt("Reply with the one word: delta");
  await older.settled(prompt, 180_000);
  const last = assistantMessages((await older.state()).sessionFile).at(-1);
  expect(last !== undefined, "the released plugin's session ran no turn");
  const lastModel = turnModel(last as Entry);
  await page.live(R);
  const { header } = await page.headerUntil(
    "the restart notice",
    60_000,
    (h) => h.notice === RESTART_NOTICE && h.model === lastModel
  );
  expect(
    !header.modelPicker && !header.thinkingPicker,
    `the older session's header offers a picker: ${headerText(header)}`
  );
  await page.shot("older-plugin-notice");
  const version = JSON.parse(readFileSync(`${rig.releasedPlugin}/package.json`, "utf8")).version;
  note(
    `session ${R} on the released @sjawhar/pi-envoy ${version}: its last turn ran on ${lastModel}; header ${headerText(header)}`
  );
  await older.stop();
  await page.live(S);
}

async function attribution({ A, B, main, page, S, services }: Context): Promise<void> {
  await page.headerUntil("the session's pickers", 60_000, (h) => h.modelPicker);
  const target = (await main.state()).model === B ? A : B;
  const sent = await services.send(S, `/model ${target}`, OTHER_PERSON);
  const reply = await sessionReply(services, S, sent.id, 60_000);
  expect(
    reply === `Model set to ${target}.`,
    `the session replied "${reply}" to ${OTHER_PERSON}'s switch`
  );
  const stored = (await services.thread(S)).find((read) => read.message.id === sent.id);
  expect(
    stored?.message.author.id === OTHER_PERSON && stored.message.author.kind === "user",
    `Dispatch stored the switch as ${JSON.stringify(stored?.message.author)}`
  );
  const shown = await until(`${PERSON}'s page to show ${OTHER_PERSON}'s switch`, 30_000, async () =>
    (await page.thread()).others.find(
      (each) => each.author === OTHER_PERSON && each.text.includes(`/model ${target}`)
    )
  );
  const { header } = await page.headerUntil(target, SHOWN_WITHIN_MS, (h) => h.model === target);
  await page.shot("attribution");
  note(
    `${OTHER_PERSON} sent "/model ${target}" with X-Dispatch-User: ${OTHER_PERSON} (message ${sent.id}); Dispatch stored it as ${stored?.message.author.kind} ${stored?.message.author.id}; the session replied "${reply}"`
  );
  note(
    `${PERSON}'s page shows it under "${shown.author}": "${shown.text}"; header ${headerText(header)}`
  );
}

/** The checks, in the order they run; each prints `== <name>` before it and `ok <name>` after. */
const checks: readonly { name: string; run: (context: Context) => Promise<void> }[] = [
  { name: "header-before-any-turn", run: headerBeforeAnyTurn },
  { name: "terminal-set-model", run: terminalSetModel },
  { name: "terminal-set-thinking", run: terminalSetThinking },
  { name: "terminal-typed-effort", run: terminalTypedEffort },
  { name: "message-switch", run: messageSwitch },
  { name: "picker-model", run: pickerModel },
  { name: "picker-auto-kept", run: pickerAutoKept },
  { name: "picker-level-after-model", run: pickerLevelAfterModel },
  { name: "picker-effort", run: pickerEffort },
  { name: "picker-during-turn", run: pickerDuringTurn },
  { name: "picker-refusal", run: pickerRefusal },
  { name: "forged-switch-changes-nothing", run: forgedSwitchChangesNothing },
  { name: "agent-token-refused", run: agentTokenRefused },
  { name: "other-session-unchanged", run: otherSessionUnchanged },
  { name: "no-answer", run: noAnswer },
  { name: "resume-keeps-switch", run: resumeKeepsSwitch },
  { name: "new-session-defaults", run: newSessionDefaults },
  { name: "fallback", run: fallback },
  { name: "older-plugin-notice", run: olderPluginNotice },
  { name: "attribution", run: attribution },
];

// ---- the run --------------------------------------------------------------------------------------

async function run(rigFile: string): Promise<void> {
  const rig = JSON.parse(readFileSync(rigFile, "utf8")) as Rig;
  const services = new Services(rig);
  const launched: Launched = { browser: undefined, omps: [] };
  const startOmp: StartOmp = async (label, extension, extra = []) => {
    const omp = await Omp.start(rig, PROVIDER_KEYS, label, extension, extra);
    launched.omps.push(omp);
    return omp;
  };
  const runStarted = Date.now();

  try {
    const context = await setUp(rig, services, startOmp, launched);
    for (const check of checks) {
      begin(check.name);
      await check.run(context);
      pass();
    }
    process.stdout.write(
      `all ${currentNumber()} checks passed in ${Math.round((Date.now() - runStarted) / 1000)}s\n`
    );
  } catch (error) {
    if (launched.browser !== undefined) await launched.browser.shot(`${currentName()}-failed`);
    const message = error instanceof Error ? error.message : String(error);
    process.stderr.write(`FAIL ${currentName()}: ${message}\n`);
    if (!(error instanceof CheckFailure) && error instanceof Error && error.stack) {
      process.stderr.write(`${error.stack}\n`);
    }
    process.exitCode = 1;
  } finally {
    // Each session's file and every Oh My Pi log go to the evidence before the work directory
    // is removed. Two sessions can share a file (a resumed one), so each is kept under its label.
    for (const omp of launched.omps) {
      await omp.stop().catch(() => undefined);
      if (omp.sessionFile !== undefined && existsSync(omp.sessionFile)) {
        await Bun.write(`${rig.evidence}/sessions/${omp.label}.jsonl`, Bun.file(omp.sessionFile));
      }
    }
    if (existsSync(`${rig.home}/.omp/logs`)) {
      cpSync(`${rig.home}/.omp/logs`, `${rig.evidence}/logs/omp`, { recursive: true });
    }
    await launched.browser?.close().catch(() => undefined);
  }
}

// ---- the database and a port ----------------------------------------------------------------------

function serverDsn(): URL {
  const dsn = process.env.LEGION_TEST_PG_DSN;
  if (dsn === undefined || dsn === "") refuse("LEGION_TEST_PG_DSN is required");
  return new URL(dsn);
}

function databaseName(name: string | undefined): string {
  if (name === undefined || !/^[a-z][a-z0-9_]*$/.test(name))
    refuse(`a database name of [a-z0-9_] is required, not ${name}`);
  return name;
}

async function createDatabase(): Promise<void> {
  const name = databaseName(args[0]);
  const server = serverDsn();
  const sql = new Bun.SQL(server.toString());
  try {
    await sql.unsafe(`create database "${name}"`);
  } finally {
    await sql.close();
  }
  const own = new URL(server.toString());
  own.pathname = `/${name}`;
  process.stdout.write(`${own.toString()}\n`);
}

async function dropDatabase(): Promise<void> {
  const name = databaseName(args[0]);
  const sql = new Bun.SQL(serverDsn().toString());
  try {
    await sql.unsafe(`drop database if exists "${name}" with (force)`);
  } finally {
    await sql.close();
  }
}

function freePort(): void {
  const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  process.stdout.write(`${server.port}\n`);
  server.stop(true);
}

if (command === "run") {
  if (args[0] === undefined) refuse("usage: run <rig.json>");
  await run(args[0]);
  // A request's or a stop's deadline timer would otherwise hold the process for minutes after the
  // last check.
  process.exit(process.exitCode ?? 0);
} else if (command === "db-create") await createDatabase();
else if (command === "db-drop") await dropDatabase();
else if (command === "free-port") freePort();
else refuse(`unknown command ${command ?? "(none)"}: run, db-create, db-drop or free-port`);

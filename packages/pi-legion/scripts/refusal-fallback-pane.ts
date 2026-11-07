// The driver of scripts/e2e/refusal-fallback-omp.sh: three Legion panes on the real Oh My Pi, each
// against an OpenAI-shaped stand-in of its own, each driven about once a minute for about twelve
// minutes, so the window the check c-r1-chain asks for is measured on a real clock. The three panes
// run at the same time, since each needs the whole twelve minutes and a pod holds three `omp --mode
// rpc` children at once; every line a pane prints names the pane, so the three read apart.
//
//   LEGION_OMP_PATH=<omp> bun packages/pi-legion/scripts/refusal-fallback-pane.ts \
//     <cooldown-ms> <drive-ms> <evidence-dir>
//
// The entry script owns the three numbers: the plugin's cooldown (LEGION_REFUSAL_COOLDOWN_MS), how
// long each pane keeps asking, and where the transcripts are kept. They are arguments rather than
// constants so a change to this driver can be tried out in a minute instead of a quarter of an
// hour; the proof's own numbers are in the script's header.
//
// Each pane is a root architect, the one role that holds a Legion claim — which is all the model
// fallback gates on — and the one role the phase reminder leaves alone, so nothing but the driver's
// two assignments starts a turn. The pane's own profile names two models of one OpenAI-shaped
// provider and a chain from the first to the second, with `retry.fallbackRevertPolicy: never`, the
// shape INFRA's operator overlay carries (deploy/kubernetes/operator-route/overlay.yml), and
// `retry.rateLimitWalksChain: true`, which Oh My Pi has off (`writeProfile` below says what it
// does). The stand-in answers every request of the long turn with a `sleep`, so each
// request says which model the session stood on a minute after the one before it.
//
// What the three panes differ in is the one answer that refuses:
//   - filtered-clean: `finish_reason: content_filter` with no content, which Oh My Pi may replay,
//     so the host walks its own chain and the plugin arms the return from the host's own reason.
//   - filtered-after-text: the same after streamed text, which is replay-unsafe, so the host leaves
//     the session where it is and the plugin walks the chain itself.
//   - rate-limited: HTTP 429 with `retry-after: 60`, which is no content filter, so the host notes
//     a suppression of the model for the 60 s the provider asked for, the arming's cause is
//     `other`, and the return waits on that suppression instead of on the cooldown. The host
//     spends its same-model retries on a rate limit first, so this pane's chain walk comes about
//     ten minutes in and its return about three minutes after that.
//
// Every expectation prints `== <name>`, what was measured and `ok <name>`, or `FAIL <name>: <why>`
// on stderr. Unlike a stage proof, a failed expectation does not stop the run: the twelve minutes
// are spent, and every other expectation's measurement is evidence about the same run. The exit
// status is 1 when any failed, naming the first, and 2 when the run could not be made at all.
import { cp, mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type FallbackCooldownState,
  LEGION_FALLBACK_ENTRY,
} from "@legion/pi-shared/fallback-cooldown";
import {
  type Cleanup,
  ompRoot,
  refusingNats,
  serveStandin,
  spawnRpc,
} from "@legion/pi-shared/test/omp-harness";

const [cooldownArg, driveArg, evidence] = process.argv.slice(2);
const binary = process.env.LEGION_OMP_PATH ?? "";
const cooldownMs = Number(cooldownArg);
const driveMs = Number(driveArg);
if (
  binary === "" ||
  evidence === undefined ||
  evidence === "" ||
  !Number.isInteger(cooldownMs) ||
  cooldownMs <= 0 ||
  !Number.isInteger(driveMs) ||
  driveMs <= 0
) {
  process.stderr.write(
    "usage: LEGION_OMP_PATH=<omp> bun packages/pi-legion/scripts/refusal-fallback-pane.ts <cooldown-ms> <drive-ms> <evidence-dir>\n"
  );
  process.exit(2);
}

/** The provider key both models sit under: one stand-in serves both, so a request's own `model`
 * says which of them it went to. */
const PROVIDER = "standin";
/** The model every role names, the one each pane's stand-in refuses. */
const STRONG_ID = "strong-model";
/** The model below it in the chain. */
const WEAK_ID = "weak-model";
const STRONG = `${PROVIDER}/${STRONG_ID}`;
const WEAK = `${PROVIDER}/${WEAK_ID}`;

/**
 * Oh My Pi's own floor for how long it suppresses a model it could not classify
 * (`packages/coding-agent/src/session/turn-recovery.ts:2029-2037` at the pinned tag): five
 * minutes. Without `retry.fallbackRevertPolicy: never` the host's own restore would come at about
 * this mark, so a pane with no request to the refused model between it and the cooldown's end
 * is the proof that the overlay's `never` and the plugin's ten minutes are both in force.
 */
const HOST_RESTORE_FLOOR_MS = 300_000;

/** How long the stand-in's `sleep` holds each request apart: a thirteenth of the span the pane is
 * driven for, which is about a minute at the proof's own numbers, so the model of each request
 * reads as the minute it came at. */
const sleepSeconds = Math.max(1, Math.round(driveMs / 13_000));

/** How late after the cooldown's end the session may come back and still be its return: one
 * request's worth of cadence, twice over. The host picks a request's model before it runs the
 * `context` hook, so the hook's switch is the model of the request after the one it ran under. */
const RETURN_SLACK_MS = 150_000;

/** How far the recorded arming's own numbers may sit from what the stand-in measured: a request
 * and its answer, generously. */
const CLOCK_SLACK_MS = 120_000;

/** How long past its driving span a pane has to start, settle its last assignment and stop before
 * the run gives up on it. The whole pane is held to that one budget, so a pane whose first
 * assignment never settles does not outlast the others by its own span. */
const SETTLE_SLACK_MS = 150_000;

const registerFixture = path.resolve(
  import.meta.dir,
  "../../contracts/fixtures/daemon-api/register.json"
);
const envoyEntry = path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts");
const legionEntry = path.resolve(import.meta.dir, "../extensions/legion.ts");

function say(line: string): void {
  process.stdout.write(`${line}\n`);
}
function note(line: string): void {
  say(`   ${line}`);
}

/** One request the stand-in answered, and what it answered with. */
interface Served {
  readonly at: number;
  readonly model: string;
  readonly answer: string;
}

/** What one pane recorded while it ran. */
interface Pane {
  /** When its first request arrived: every minute this driver prints is measured from here. */
  startedAt: number;
  /** When its first request on the chain's next model arrived — the moment the fallback took
   * effect — or undefined when none did. */
  fellBackAt: number | undefined;
  readonly served: Served[];
  /** The turns of the conversation, the run-end self-check not among them: which answer of the
   * run's own script the next request gets. */
  turns: number;
  /** The cooldown states the pane wrote to its transcript, in order; read once it has settled. */
  states: FallbackCooldownState[];
  /** Why the pane did not run to the end, when it did not: what one expectation of its own
   * reports, so the other panes' measurements are still read. */
  trouble: string | undefined;
}

/** One of the three panes: what it is called, the issue it is the architect of, and the answer its
 * stand-in gives each turn. */
interface Run {
  readonly label: string;
  readonly key: string;
  /** One line of the run's own header: what this pane's stand-in does. */
  readonly what: string;
  /** The answer for turn `nth` of this pane, `nth` counting from 1. */
  readonly answer: (pane: Pane, model: string, nth: number) => Answer;
  /** True for the two panes a content filter refuses: their return waits out the cooldown. A rate
   * limit's waits on the host's own suppression instead. */
  readonly refused: boolean;
}

/** What the stand-in answered one request with: the response, and the words the pane's line
 * prints for it. */
interface Answer {
  readonly response: Response;
  readonly what: string;
}

/** One OpenAI chat-completion stream: each delta with the `finish_reason` that follows it, then
 * the `[DONE]` every client reads as the end. */
function completion(
  model: string,
  id: string,
  chunks: readonly (readonly [object, string | null])[]
): Response {
  const body = chunks
    .map(
      ([delta, finish]) =>
        `data: ${JSON.stringify({
          id,
          object: "chat.completion.chunk",
          created: 1,
          model,
          choices: [{ index: 0, delta, finish_reason: finish }],
        })}\n\n`
    )
    .join("");
  return new Response(`${body}data: [DONE]\n\n`, {
    headers: { "content-type": "text/event-stream" },
  });
}

/** A turn answered with text, which ends the run when nothing else is pending. */
function says(model: string, nth: number, text: string): Answer {
  return {
    response: completion(model, `text_${nth}`, [
      [{ role: "assistant", content: text }, null],
      [{}, "stop"],
    ]),
    what: `text "${text}"`,
  };
}

/** A turn answered with one `bash sleep`, which keeps the same turn running for about a minute: the
 * next request of that turn is the next minute's reading of the session's model. */
function sleeps(model: string, nth: number): Answer {
  return {
    response: completion(model, `sleep_${nth}`, [
      [
        {
          role: "assistant",
          tool_calls: [
            {
              index: 0,
              id: `call_${nth}`,
              type: "function",
              function: {
                name: "bash",
                arguments: JSON.stringify({
                  i: "Waiting out the gap before the next request",
                  command: `sleep ${sleepSeconds}`,
                }),
              },
            },
          ],
        },
        null,
      ],
      [{}, "tool_calls"],
    ]),
    what: `bash sleep ${sleepSeconds}`,
  };
}

/** A turn a provider's own filter refused: `finish_reason: content_filter`, after `before` when the
 * model committed any text first. Text already streamed makes the turn replay-unsafe, which is the
 * one thing that decides whether the host walks its own chain or leaves the session where it is. */
function filtered(model: string, nth: number, before?: string): Answer {
  const opening =
    before === undefined ? { role: "assistant" } : { role: "assistant", content: before };
  return {
    response: completion(model, `filtered_${nth}`, [
      [opening, null],
      [{}, "content_filter"],
    ]),
    what:
      before === undefined
        ? "finish_reason content_filter, no text before it"
        : `finish_reason content_filter, after "${before}"`,
  };
}

/** A turn refused by a rate limit, with the wait the provider asks for. */
function rateLimited(): Answer {
  return {
    response: new Response(
      JSON.stringify({ error: { type: "rate_limit_error", message: "Rate limit reached" } }),
      { status: 429, headers: { "content-type": "application/json", "retry-after": "60" } }
    ),
    what: "429 rate limit, retry-after 60",
  };
}

/**
 * The answer every turn after the refusal gets: another minute's `sleep` until the pane has been
 * driven for its whole span, then text, which lets the run settle.
 *
 * A pane that fell back and has not been given its model again keeps asking past the span, for as
 * long as a return is still allowed to take. The host picks a request's model before it runs the
 * `context` hook the plugin switches in, so a return is read one request after the hook that made
 * it, and a span that ends between the two would report a return that happened as one that never
 * came.
 */
function pacing(pane: Pane, model: string, nth: number): Answer {
  const fellBack = pane.fellBackAt;
  const waitingForTheReturn =
    fellBack !== undefined &&
    !pane.served.some((served) => served.at >= fellBack && served.model === STRONG_ID);
  const span = driveMs + (waitingForTheReturn ? RETURN_SLACK_MS : 0);
  return Date.now() - pane.startedAt >= span ? says(model, nth, "Done.") : sleeps(model, nth);
}

const runs: readonly Run[] = [
  {
    label: "filtered-clean",
    key: "RFCL-1",
    what: "a content filter with no text before it, which Oh My Pi may replay: the host walks its own chain",
    refused: true,
    answer: (pane, model, nth) => (nth === 1 ? filtered(model, nth) : pacing(pane, model, nth)),
  },
  {
    label: "filtered-after-text",
    key: "RFAT-1",
    what: "a content filter after streamed text, which is replay-unsafe: the plugin walks the chain",
    refused: true,
    answer: (pane, model, nth) =>
      nth === 1 ? filtered(model, nth, "Looking at it.") : pacing(pane, model, nth),
  },
  {
    label: "rate-limited",
    key: "RLIM-1",
    what: "a 429 with retry-after 60, which is no content filter: the return waits on the host's own suppression",
    refused: false,
    answer: (pane, model, nth) =>
      model === STRONG_ID && pane.fellBackAt === undefined
        ? rateLimited()
        : pacing(pane, model, nth),
  },
];

/** Whether a request is the run-end self-check rather than a turn: pi-envoy asks it over a snapshot
 * of the conversation whose last message is the `<btw>` block around the question. */
function isSelfCheck(body: Record<string, unknown>): boolean {
  const messages = Array.isArray(body.messages) ? body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null).includes("<btw>");
}

/** The profile the pane runs on: one OpenAI-shaped provider at `base`, its two models, and the
 * operator overlay's own chain settings. The provider is keyed by a literal API key, as a plain
 * key caller is, and every provider a pod could otherwise answer from is disabled, so no turn
 * reaches a real model. */
async function writeProfile(home: string, base: string): Promise<void> {
  const agent = path.join(home, ".omp", "agent");
  await writeFile(
    path.join(agent, "models.yml"),
    [
      "providers:",
      `  ${PROVIDER}:`,
      `    baseUrl: ${base}/openai/v1`,
      "    api: openai-completions",
      "    apiKey: standin-key",
      "    models:",
      ...[STRONG_ID, WEAK_ID].flatMap((id) => [
        `      - id: ${id}`,
        `        name: Stand-in ${id}`,
        "        reasoning: false",
        "        input: [text]",
        "        contextWindow: 200000",
        "        maxTokens: 8000",
        "        cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}",
      ]),
      "",
    ].join("\n")
  );
  await writeFile(
    path.join(agent, "config.yml"),
    [
      "enabledModels:",
      `  - ${PROVIDER}/*`,
      "disabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]",
      "modelRoles:",
      ...["default", "smol", "slow", "plan", "task", "commit", "tiny", "vision", "advisor"].map(
        (role) => `  ${role}: ${STRONG}`
      ),
      "retry:",
      "  modelFallback: true",
      "  fallbackRevertPolicy: never",
      // On, a rate limit is admitted to the chain consult rather than deferred to another attempt
      // on the same model (`sourceRateLimited`, `session/turn-recovery.ts:2913-2920` and
      // `:3238-3246` at the pinned tag); Oh My Pi has it off. The host spends its same-model
      // retries either way — `retry.maxRetries` is 10 and each one waits out the 60 s the
      // stand-in's 429 asks for — so the walk this pane measures comes about ten minutes in.
      "  rateLimitWalksChain: true",
      "  fallbackChains:",
      `    ${STRONG}: [${WEAK}]`,
      "",
    ].join("\n")
  );
}

/** Rejects once `ms` has passed, so a pane that never settles ends the run instead of holding it. */
async function within<T>(ms: number, what: string, work: Promise<T>): Promise<T> {
  const timeout = Promise.withResolvers<never>();
  timeout.promise.catch(() => undefined);
  const handle = setTimeout(
    () => timeout.reject(new Error(`${what} did not settle within ${Math.round(ms / 1000)}s`)),
    ms
  );
  try {
    return await Promise.race([work, timeout.promise]);
  } finally {
    clearTimeout(handle);
  }
}

/**
 * Copies what the pane leaves behind into the evidence directory, which outlives its scratch root:
 * its Oh My Pi transcripts and the host's own log, the one record of what the host did with a
 * refusal the stand-in cannot see. Answers the cooldown states the transcripts hold, in order.
 */
async function keepEvidence(
  home: string,
  sessions: string,
  label: string
): Promise<FallbackCooldownState[]> {
  const logs = path.join(home, ".omp", "logs");
  for (const log of await readdir(logs).catch(() => [])) {
    await cp(path.join(logs, log), path.join(evidence, `${label}-${log}`)).catch(() => undefined);
  }
  const states: FallbackCooldownState[] = [];
  const files = (await readdir(sessions, { recursive: true })).filter((file) =>
    file.endsWith(".jsonl")
  );
  for (const file of files.sort()) {
    const full = path.join(sessions, file);
    await cp(full, path.join(evidence, `${label}-${path.basename(file)}`));
    for (const line of (await readFile(full, "utf8")).split("\n").filter(Boolean)) {
      const entry: unknown = JSON.parse(line);
      if (typeof entry !== "object" || entry === null) continue;
      if (!("customType" in entry) || entry.customType !== LEGION_FALLBACK_ENTRY) continue;
      if ("data" in entry) states.push(entry.data as FallbackCooldownState);
    }
  }
  return states;
}

/**
 * Runs one pane and answers what it recorded. A pane that cannot be run to the end answers what it
 * recorded until then, with its trouble, so the other panes' measurements are still reported.
 */
async function drivePane(run: Run, cleanup: Cleanup): Promise<Pane> {
  const pane: Pane = {
    startedAt: 0,
    fellBackAt: undefined,
    served: [],
    turns: 0,
    states: [],
    trouble: undefined,
  };
  try {
    await runPane(run, pane, cleanup);
  } catch (error) {
    pane.trouble = error instanceof Error ? error.message : String(error);
  }
  return pane;
}

/** One pane's whole life: its scratch root, its stand-in, its omp child and its two assignments,
 * the lot held to one budget: the driving span, the longest a return may take past it
 * (`pacing`), and the slack. */
async function runPane(run: Run, pane: Pane, cleanup: Cleanup): Promise<void> {
  const deadlineAt = Date.now() + driveMs + RETURN_SLACK_MS + SETTLE_SLACK_MS;
  const { root, home, workspace, sessions } = await ompRoot(
    binary,
    `refusal-${run.label}-`,
    cleanup
  );
  const state = path.join(root, "state");
  await mkdir(state, { recursive: true, mode: 0o700 });
  // The daemon's golden registration answer, so a field the daemon adds to it reaches this
  // stand-in's `claims/register` too.
  const registered: Record<string, unknown> = JSON.parse(await readFile(registerFixture, "utf8"));
  let grants = 0;

  const record = (at: number, model: string, what: string): void => {
    pane.served.push({ at, model, answer: what });
    const minute = ((at - pane.startedAt) / 60_000).toFixed(1);
    note(`${run.label.padEnd(19)} m${minute.padStart(5)}  ${model.padEnd(12)}  ${what}`);
  };

  const { base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/openai/v1/chat/completions") {
      const at = Date.now();
      const model = typeof body.model === "string" ? body.model : "";
      if (pane.startedAt === 0) pane.startedAt = at;
      if (model === WEAK_ID && pane.fellBackAt === undefined) pane.fellBackAt = at;
      if (isSelfCheck(body)) {
        const answer = says(model, pane.served.length, "PROCEEDING");
        record(at, model, "the run-end self-check");
        return answer.response;
      }
      pane.turns += 1;
      const answer = run.answer(pane, model, pane.turns);
      record(at, model, answer.what);
      return answer.response;
    }
    // Every other read of the provider — the model listing a client makes of its endpoint — is
    // empty: the profile names the two models itself. The provider sits under a prefix of its
    // own, since the Envoy listener this stand-in also answers for serves its own `/v1/` routes.
    if (url.pathname.startsWith("/openai/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: `legion-${run.label}-${run.key.toLowerCase()}-architect`,
        tree: run.key,
        issue: run.key,
        role: "architect",
        generation: 1,
        secret: `${run.label}-secret`,
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `${run.label}-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: `${run.label}-machine`,
      dir: workspace,
      topics: [],
    });
  });
  await writeProfile(home, base);

  let settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      // A Legion pane loads both plugins: the Legion entry refuses to run without the Envoy one.
      extensions: [envoyEntry, legionEntry],
      home,
      workspace,
      sessions,
      env: {
        ENVOY_URL: base,
        ENVOY_NATS_URL: refusingNats(cleanup),
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: "architect",
        LEGION_TREE: run.key,
        LEGION_ISSUE: run.key,
        LEGION_GENERATION: "1",
        LEGION_BOOT_TOKEN: `${run.label}-boot`,
        LEGION_STATE_DIR: state,
        LEGION_WORKSPACE: workspace,
        LEGION_REFUSAL_COOLDOWN_MS: String(cooldownMs),
        // Well above the one wait each pane takes, so no cap decides it.
        LEGION_REFUSAL_COOLDOWN_MAX_MS: String(cooldownMs * 4),
      },
      onFrame: (frame) => {
        if (
          "type" in frame &&
          frame.type === "agent_end" &&
          !("isTerminal" in frame && frame.isTerminal === false)
        ) {
          settled.resolve();
        }
      },
    },
    cleanup
  );

  // Two assignments: the first is refused, and the second is the long turn for the pane whose
  // refusal ended its run where it stood. For the two the host replays or retries, the long turn
  // is the first assignment's own and the second is one last reading of the session's model.
  try {
    for (const prompt of [`Start ${run.key}.`, "Keep working."]) {
      settled = Promise.withResolvers<void>();
      rpc.send({ type: "prompt", message: prompt });
      await within(
        deadlineAt - Date.now(),
        `${run.label}: the assignment "${prompt}"`,
        Promise.race([settled.promise, rpc.closed])
      );
    }
    await rpc.end();
  } finally {
    // A pane that never settled is the run this evidence is most wanted for.
    pane.states = await keepEvidence(home, sessions, run.label).catch(() => []);
  }
}

/** One expectation of the run: what it measured, and why it failed when it did. */
interface Verdict {
  readonly name: string;
  readonly measured: string;
  /** Empty when the expectation held. */
  readonly failure: string;
}

/** What each pane is held to, in the order the run reports them. */
function verdictsFor(run: Run, pane: Pane): Verdict[] {
  const minute = (at: number): string => `m${((at - pane.startedAt) / 60_000).toFixed(1)}`;
  const first = pane.served[0];
  const fellBack = pane.fellBackAt;
  const last = pane.served.at(-1);
  const verdicts: Verdict[] = [
    {
      name: `${run.label}-runs-to-the-end`,
      measured:
        last === undefined
          ? "the pane answered no request"
          : `${pane.served.length} request(s) over ${minute(last.at)}`,
      failure: pane.trouble ?? "",
    },
    {
      name: `${run.label}-walks-the-chain`,
      measured:
        first === undefined
          ? "the stand-in answered no request at all"
          : `the first request went to ${first.model} at ${minute(first.at)}; ` +
            (fellBack === undefined
              ? `no request went to ${WEAK_ID}`
              : `the first on ${WEAK_ID} came at ${minute(fellBack)}`),
      failure:
        first === undefined
          ? "the pane sent no request"
          : first.model !== STRONG_ID
            ? `the first request went to ${first.model}, not to ${STRONG_ID}`
            : fellBack === undefined
              ? `no request went to ${WEAK_ID}: the chain was not walked`
              : "",
    },
  ];
  const armed = pane.states.find((recorded) => recorded.arming !== undefined)?.arming;
  const cause = run.refused ? "refusal" : "other";
  // A refusal's return is due a whole cooldown after the fallback; any other fallback's arming is
  // due at once and waits on the host's own suppression instead.
  const dueIn = run.refused ? cooldownMs : 0;
  verdicts.push({
    name: `${run.label}-arms-the-return`,
    measured:
      armed === undefined
        ? "the pane recorded no arming in its transcript"
        : `cause ${armed.cause}, back to ${armed.restoreTo}, due at ${minute(armed.dueAt)}`,
    failure:
      armed === undefined
        ? "the pane recorded no arming: nothing armed the session's return"
        : armed.cause !== cause
          ? `the arming's cause is ${armed.cause}, not ${cause}`
          : armed.restoreTo !== STRONG
            ? `the arming goes back to ${armed.restoreTo}, not to ${STRONG}`
            : fellBack === undefined
              ? "the chain was not walked, so the arming's due time says nothing"
              : Math.abs(armed.dueAt - fellBack - dueIn) > CLOCK_SLACK_MS
                ? `the return is due ${Math.round((armed.dueAt - fellBack) / 1000)}s after the fallback, not ${Math.round(dueIn / 1000)}s`
                : "",
  });
  if (armed === undefined || fellBack === undefined) return verdicts;
  const back = pane.served.find((served) => served.at >= fellBack && served.model === STRONG_ID);
  if (!run.refused) {
    // The host suppresses a model it rate-limited for as long as the provider asked for, and the
    // arming is due the moment that suppression ends — a clock of the host's, and nothing like the
    // cooldown a refusal waits out.
    verdicts.push({
      name: `${run.label}-comes-back-on-the-suppression`,
      measured:
        back === undefined
          ? `the session never went back to ${STRONG_ID}`
          : `back on ${STRONG_ID} at ${minute(back.at)}, ${Math.round((back.at - fellBack) / 1000)}s after the fallback, with the cooldown at ${Math.round(cooldownMs / 1000)}s`,
      failure:
        back === undefined
          ? `the session never went back to ${STRONG_ID}`
          : back.at - fellBack >= cooldownMs / 2
            ? `it went back ${Math.round((back.at - fellBack) / 1000)}s after the fallback, which is the cooldown's own clock rather than the host's suppression`
            : "",
    });
    return verdicts;
  }
  const from = pane.startedAt + HOST_RESTORE_FLOOR_MS;
  const inWindow = pane.served.filter((served) => served.at >= from && served.at < armed.dueAt);
  const strongInWindow = inWindow.filter((served) => served.model === STRONG_ID);
  const firstStrong = strongInWindow[0];
  verdicts.push({
    name: `${run.label}-quiet-between-five-and-ten`,
    measured: `${inWindow.length} request(s) between ${minute(from)} and ${minute(armed.dueAt)}, ${strongInWindow.length} of them on ${STRONG_ID}`,
    failure:
      inWindow.length < 3
        ? `only ${inWindow.length} request(s) fell in the window, too few to say anything`
        : firstStrong === undefined
          ? ""
          : `${strongInWindow.length} request(s) went to ${STRONG_ID} inside the window, the first at ${minute(firstStrong.at)}`,
  });
  verdicts.push({
    name: `${run.label}-comes-back-after-ten`,
    measured:
      back === undefined
        ? `the session never went back to ${STRONG_ID}`
        : `back on ${STRONG_ID} at ${minute(back.at)}, with the return due at ${minute(armed.dueAt)}`,
    failure:
      back === undefined
        ? `the session never went back to ${STRONG_ID} after the cooldown`
        : back.at < armed.dueAt
          ? `it went back at ${minute(back.at)}, before the return was due at ${minute(armed.dueAt)}`
          : back.at - armed.dueAt > RETURN_SLACK_MS
            ? `it went back at ${minute(back.at)}, ${Math.round((back.at - armed.dueAt) / 1000)}s after the return was due`
            : "",
  });
  return verdicts;
}

const cleanup: Cleanup = [];
let panes: Pane[] = [];
try {
  await mkdir(evidence, { recursive: true });
  say("== drive");
  for (const run of runs) note(`${run.label.padEnd(19)} ${run.key}  ${run.what}`);
  note(
    `each pane: cooldown ${Math.round(cooldownMs / 1000)}s, driven for ${Math.round(driveMs / 1000)}s, a request about every ${sleepSeconds}s`
  );
  panes = await Promise.all(runs.map((run) => drivePane(run, cleanup)));
  say("ok drive");
} catch (error) {
  process.stderr.write(`FAIL drive: ${error instanceof Error ? error.message : String(error)}\n`);
  for (const step of cleanup.splice(0).reverse()) await step();
  process.exit(2);
}
for (const step of cleanup.splice(0).reverse()) await step();

const failed: string[] = [];
for (const [index, run] of runs.entries()) {
  const pane = panes[index];
  if (pane === undefined) continue;
  for (const verdict of verdictsFor(run, pane)) {
    say(`== ${verdict.name}`);
    note(verdict.measured);
    if (verdict.failure === "") {
      say(`ok ${verdict.name}`);
      continue;
    }
    failed.push(verdict.name);
    process.stderr.write(`FAIL ${verdict.name}: ${verdict.failure}\n`);
  }
}
const firstFailure = failed[0];
if (firstFailure !== undefined) {
  process.stderr.write(
    `refusal fallback: ${failed.length} expectation(s) failed, the first ${firstFailure}\n`
  );
  process.exit(1);
}
say(`refusal fallback: every expectation held, transcripts in ${evidence}`);

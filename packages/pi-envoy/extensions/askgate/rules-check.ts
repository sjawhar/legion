// AskGate's rule check: the second model call a gated Dispatch write gets when it opens a new ask
// for a person. It knows two rules and nothing else — an ask whose own recommendation is to change
// nothing, and an ask for the person's hands where the agent did not try the access it has — and a
// match refuses the call before it posts, in warn mode and in block mode.
//
// The check reads the new asks from the call's arguments (`new-asks.ts`), reads the document when
// it needs one (a retype's block text, and the ids an upload's current version already holds),
// lists the asks in its own request, and asks the model `createAskGate` resolved for the charter
// call, on the same chain, deadline, transcript and stop. It never imports `askgate-core.ts`: the
// walk, the dump and the stop are passed in, so the core owns them once for both calls.
import {
  type DocumentBlocks,
  type DocumentTarget,
  readDocumentBlocks,
} from "@legion/envoy-client/dispatch-execute";
import type { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { type NewAsk, newAsks, openingOf } from "./new-asks";
import requestTemplate from "./rule-check-request.md" with { type: "text" };
import {
  byteLength,
  clipMiddle,
  escapedHead,
  escapeXml,
  lastLineJson,
  renderTemplate,
} from "./text";

/** The whole asks list in one request. */
const ASKS_MAX_BYTES = 32 * 1024;
/** One ask in that list. */
const ASK_MAX_BYTES = 8 * 1024;
/**
 * The least of its text an ask is given, however much the tags take: a head and a tail of about
 * 48 bytes each around the clip's marker, enough for a request or a one-line recommendation.
 */
const ASK_MIN_BYTES = 128;
/** An ask's `#id` in its tag, escaped and with its cut's `…`. */
const ID_MAX_BYTES = 128;
/** How many matching asks a refusal names before it only counts the rest. */
const REFUSAL_MATCHES = 5;

export type RuleName = "no_change" | "untried_access";
/** The two rules, by the name the answer gives each: how the agent reads it, and what to do instead. */
const RULES: Record<RuleName, { label: string; fix: string }> = {
  no_change: {
    label: "no change",
    fix: "Keep things as they are, and write the decision and its reason in the spec or in a comment. For an ask block, put that decision in place of the block and send the call again.",
  },
  untried_access: {
    label: "untried access",
    fix: "Do the step with the access you have or can request: a key from `agent-secrets` where the session has it, a token or login already on the machine, or another agent that holds the access. Send the ask again only if that fails, with the command you ran and its error.",
  },
};

/** One new ask the rules call names: its number in the request, the rule it matches, and the words that match. */
export type RuleMatch = { ask: number; rule: RuleName; quote: string };

/**
 * How the rules check ended, recorded as `ruleOutcome`. `unjudged` is the one no model call made:
 * an upload whose stored version could not be read and whose every opener carries an `#id`, so the
 * check left every ask of the call unjudged — never a judgement that nothing matched.
 */
export type RuleOutcome =
  | "match"
  | "none"
  | "error"
  | "timeout"
  | "no-verdict"
  | "abandoned"
  | "shutdown"
  | "unjudged";

/** Every attempt's spend, as the gate passes it around. */
type RuleUsage = {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cost: number;
};
/**
 * One model call's answer and what it cost, as `createAskGate`'s walk gives them. Declared here,
 * structurally, so this module stays free of the core.
 */
export type RuleAnswer =
  | { kind: "answer"; text: string; usage?: RuleUsage }
  | { kind: "error"; reason: string; usage?: RuleUsage }
  | { kind: "timeout" | "abandoned" | "shutdown"; usage?: RuleUsage };
export type RuleWalked = {
  answer: RuleAnswer;
  called: { promptBytes?: number; model?: string; usage?: RuleUsage };
};

/** A Dispatch client for one directory, and the addresses a dashboard-URL `ref` may be on. */
export interface DispatchSession {
  readonly client: DispatchClient;
  readonly serverUrls: readonly string[];
}

/** What the core lends the rules check: its model walk, its stop's signal, and the session's Dispatch. */
export interface RulesCheckDeps {
  now: () => number;
  env: Record<string, string | undefined>;
  /**
   * The session's Dispatch as the directory the call runs in configures it, every request ending
   * with the gated call's stop; undefined when the session has no Dispatch configuration.
   */
  dispatchClient: (directory: string, signal: AbortSignal) => DispatchSession | undefined;
  /** The charter call's own walk: the role's chain under the call's deadline, dumped and raced by the core. */
  walk: (prompt: { system: string; user: () => string; label: string }) => Promise<RuleWalked>;
}

/** The gated call the rules check judges. */
export interface RulesCheckCall {
  /** The recorded tool name, `dispatch_ask` and the rest. */
  toolName: string;
  /** How the request names the call: `dispatch ask`, or `xd://dispatch_ask`. */
  tool: string;
  /** The arguments as the dry run printed them, or the device's own. */
  input: Readonly<Record<string, unknown>>;
  /**
   * Where the command runs after its own `cd`, the session's directory for a device call: asked
   * only when a document is read, and undefined when bash could not say.
   */
  directory: () => Promise<string | undefined>;
  /** The rules call's system role: its frame and the two rules. */
  system: string;
  /**
   * The agent under review as the request carries it: its own system prompt and the newest part of
   * its transcript. Read when the request is rendered, and only then, so the charter call's own
   * rendering of the same text is the one that pays for it.
   */
  context: () => { primarySystemPrompt: string; transcript: string };
  /** The gated call's one stop, which ends a document read as it ends a model call. */
  signal: AbortSignal;
}

/** What one rules check leaves on the record, and what it tells `decide` to do. */
export interface RulesCheck {
  outcome: RuleOutcome;
  /** How a refusal names each ask, in the order the request lists them. */
  names: readonly string[];
  /** The asks a rule matched, by ask number; empty for every other outcome. */
  matches: RuleMatch[];
  /** The model's own one-line reason, empty when it gave none. */
  reason: string;
  /** The provider's error text, `timeout` or `no-verdict`; unset when the call answered. */
  error?: string;
  model?: string;
  promptBytes: number;
  latencyMs: number;
  /** An ask's text could not be read, so the call judged that ask without it. */
  textMissing: boolean;
  /** Asks whose text the request does not carry whole: each clipped ask, counted once. */
  asksCut: number;
  /**
   * An upload whose stored version could not be read: the call was judged on part of its asks,
   * the openers no stored version could already hold, or on none of them (`unjudged`).
   */
  asksPartial: boolean;
  usage?: RuleUsage;
}

/**
 * The rules verdict on the answer's last line, `{"matches": [{"ask", "rule", "quote"}], "reason"}`. It
 * keeps each ask once, with the first rule listed for it. Any other shape, or an ask number the request
 * does not hold, gives undefined.
 */
export function parseRuleVerdict(
  text: string,
  askCount: number
): { matches: RuleMatch[]; reason: string } | undefined {
  const parsed = lastLineJson(text);
  if (parsed === undefined || !Array.isArray(parsed.matches) || typeof parsed.reason !== "string")
    return undefined;
  const matches: RuleMatch[] = [];
  for (const match of parsed.matches) {
    if (typeof match !== "object" || match === null || Array.isArray(match)) return undefined;
    const { ask, rule, quote } = match as Record<string, unknown>;
    if (typeof ask !== "number" || !Number.isInteger(ask) || ask < 1 || ask > askCount)
      return undefined;
    if (typeof rule !== "string" || !Object.hasOwn(RULES, rule) || typeof quote !== "string")
      return undefined;
    if (!matches.some((seen) => seen.ask === ask))
      matches.push({ ask, rule: rule as RuleName, quote });
  }
  return { matches, reason: parsed.reason.trim() };
}

/** How the agent overrides a refusal, as `askgate-core.ts` words it for the surface the call came in on. */
export interface RuleOverride {
  /** What to add to the call: a flag on a command, a field in a device's JSON. */
  again: string;
  /** What that addition is called, which the gate removes before sending. */
  what: string;
  /** A line the refusal ends its override with, as the charter's revise does (a file read before its batch ran). */
  note?: string;
}

/**
 * The refusal for a call whose new ask a rule matched, in every mode: each matching ask, its rule and
 * the quoted words (already agent-facing), what to do for each rule that matched, and the override.
 * A long list is cut to the first few matches and counted.
 */
export function renderRuleRefusal(
  delivered: string,
  rules: readonly RuleName[],
  second: boolean,
  override: RuleOverride,
  secondRevise: string
): string {
  return [
    "AskGate did not send this call: a rule refuses a new ask in it.",
    delivered,
    ...[...new Set(rules)].map((rule) => `For "${RULES[rule].label}": ${RULES[rule].fix}`),
    `If the person is needed anyway, send the call again with ${override.again}, naming which of the person's authority, taste or risk appetite it needs. An override is always sent and recorded, and the ${override.what} is removed before sending.`,
    ...(override.note ? [override.note] : []),
    ...(second ? [secondRevise] : []),
  ].join("\n");
}

/** The lines a refusal gives the matching asks: at most five, then how many more there are. */
export function matchLines(matches: readonly RuleMatch[], names: readonly string[]): string[] {
  const named = matches
    .slice(0, REFUSAL_MATCHES)
    .map(
      (match) =>
        `${names[match.ask - 1] ?? `Ask ${match.ask}`} matches the rule "${RULES[match.rule].label}": "${match.quote}".`
    );
  const more = matches.length - named.length;
  return more > 0 ? [...named, `${more} more new ask(s) in this call match a rule too.`] : named;
}

/** An XML attribute value: escaped as the text is, with the quote that would close it escaped too. */
const attribute = (value: string) => escapeXml(value).replaceAll('"', "&quot;");

/** What an ask whose text could not be read is listed as, in place of that text. */
const MISSING_TEXT =
  "The text of this block is not available, so judge it on what the call and the transcript show.";

/**
 * How many bytes of each ask's text the list carries, by the order the needs are given in: an
 * equal share of what the tags leave of `ASKS_MAX_BYTES`, except that an ask needing less than its
 * share hands the rest to the asks that need more, no ask is given more than `ASK_MAX_BYTES`, and
 * none less than `ASK_MIN_BYTES` (or its whole text, when that is shorter).
 * Sharing rather than spending first-come is what keeps every ask in the list.
 */
function textBudgets(needs: readonly number[], budget: number): number[] {
  const budgets = new Array<number>(needs.length).fill(0);
  const smallestFirst = needs.map((_, index) => index).sort((a, b) => needs[a] - needs[b]);
  let left = budget;
  for (const [taken, index] of smallestFirst.entries()) {
    const share = Math.floor(Math.max(0, left) / (smallestFirst.length - taken));
    budgets[index] = Math.min(needs[index], ASK_MAX_BYTES, Math.max(ASK_MIN_BYTES, share));
    left -= budgets[index];
  }
  return budgets;
}

/**
 * The asks as the request lists them: each in an `<ask>` tag of its own, numbered from 1 and
 * escaped, each clipped in its middle to its share of the list's 32 KiB and to 8 KiB of its own.
 * Also how a refusal names each ask, and how many of them the request does not carry whole.
 *
 * Every ask of the call is listed, so the rules check judges each one: an ask the request leaves
 * out matches no rule, and the call records `ruleOutcome: "none"` as if a check had read it. For
 * the same reason a clip keeps both ends of an ask (`clipMiddle`), the two places the rules read.
 *
 * An `#id` is in its tag only to name the ask back to the agent, so it is cut to `ID_MAX_BYTES`:
 * the server stores an id of any length, and an unbounded one would spend the list on its tag and
 * leave the other asks' text out. The ask's number names it to the rules call and the refusal
 * names it by the whole id, so the cut loses nothing a rule reads; the text, which a rule does
 * read, is never given less than `ASK_MIN_BYTES`. With both bounds an ask costs at most 298 bytes
 * before its share (a tag of 170 up to ask 999, and that floor), so a call needs over a hundred
 * asks to pass 32 KiB: 111 with every id at the bound, some 180 with ids of a usual length, which
 * a session does not make. Such a call still lists every ask at the floor and goes past the
 * bound: a longer request is better than an ask no check ever saw.
 */
export function describeAsks(asks: readonly NewAsk[]): {
  list: string;
  names: string[];
  asksCut: number;
} {
  // The bound clips the escaped text, so an ask is cut by what that text measures, not the raw one.
  const entries = asks.map((ask, index) => {
    let id = "";
    if (ask.id !== undefined) {
      const value = attribute(ask.id);
      const bounded =
        byteLength(value) <= ID_MAX_BYTES
          ? value
          : `${escapedHead(value, ID_MAX_BYTES - byteLength("…"))}…`;
      id = ` id="${bounded}"`;
    }
    const text = ask.textMissing ? MISSING_TEXT : escapeXml(ask.text.trim());
    return { ask, open: `<ask n="${index + 1}" kind="${ask.kind}"${id}>`, text };
  });
  // What the list spends before any ask's text: each ask's tags and their two newlines, and the
  // newline joining each ask to the one before it.
  const tags =
    entries.reduce((sum, entry) => sum + byteLength(`${entry.open}\n\n</ask>`), 0) +
    Math.max(0, entries.length - 1);
  const budgets = textBudgets(
    entries.map((entry) => byteLength(entry.text)),
    ASKS_MAX_BYTES - tags
  );
  let cut = 0;
  const listed = entries.map((entry, index) => {
    const whole = byteLength(entry.text) <= budgets[index];
    if (entry.ask.textMissing || !whole) cut++;
    const body = whole ? entry.text : clipMiddle(entry.text, budgets[index]);
    return `${entry.open}\n${body}\n</ask>`;
  });
  return { list: listed.join("\n"), names: entries.map(({ ask }) => askName(ask)), asksCut: cut };
}

/** How a refusal names one ask to the agent. */
function askName(ask: NewAsk): string {
  if (ask.kind === "ask") return "The ask";
  const id = ask.id === undefined ? "" : ` #${ask.id}`;
  return ask.opening === ""
    ? `The ask block${id}`
    : `The ask block${id} that starts '${ask.opening}'`;
}

/**
 * The name an upload's document is stored under: Dispatch trims the name it is given
 * (`packages/envoy/internal/dispatch/api/artifacts.go`), while the dry run prints it as the
 * command wrote it, so the read and the comparison both go by the trimmed one.
 */
const uploadName = (input: Readonly<Record<string, unknown>>) =>
  typeof input.name === "string" ? input.name.trim() : undefined;

/**
 * The new asks a call opens, read from `first` (the scan of its arguments): a retype gets its
 * block's own text from the document, and an upload leaves out the asks the version it replaces
 * already holds. An empty list is two different facts, which `asksPartial` tells apart: unset,
 * the call opens no new ask; set, the read failed and the gate judges none of the call's asks.
 */
async function readAsks(
  deps: RulesCheckDeps,
  call: RulesCheckCall,
  first: NewAsk[]
): Promise<{ asks: NewAsk[]; textMissing: boolean; asksPartial: boolean }> {
  const upload = call.toolName === "dispatch_artifact";
  const retypes = first.filter((ask) => ask.kind === "retype");
  if (!upload && retypes.length === 0)
    return { asks: first, textMissing: false, asksPartial: false };
  const read = await readDocument(deps, call);
  if (read === "failed") {
    // A retype is judged without its text, which only the document holds. For an upload the read
    // is the one thing telling the asks this upload adds from those the version it replaces
    // already holds, so an opener carrying an id may be one the stored version holds: judging it
    // would refuse a re-upload of an unchanged document for its own ask block. Only the openers
    // with no id, which no stored version can hold, are judged, as a read that found the rest
    // would leave them out.
    for (const ask of retypes) ask.textMissing = true;
    const asks = upload ? first.filter((ask) => ask.id === undefined) : first;
    return { asks, textMissing: retypes.length > 0, asksPartial: asks.length < first.length };
  }
  if (!upload) {
    const blocks = read === "not-found" ? [] : read.blocks;
    let textMissing = false;
    for (const ask of retypes) {
      // The server does not strip a `#` from the block a retype names; the block list never holds one.
      const block = blocks.find((held) => held.id === (ask.id ?? "").replace(/^#/, ""));
      if (block === undefined) {
        ask.textMissing = true;
        textMissing = true;
        continue;
      }
      ask.text = block.text.trim();
      ask.opening = openingOf(ask.text);
    }
    return { asks: first, textMissing, asksPartial: false };
  }
  // An upload replaces the document of its name, as the server matches one; the asks that version
  // already holds are not new. Any other document the reference found says nothing about this
  // upload, so every opener in it is judged.
  const name = uploadName(call.input);
  const currentIds =
    read !== "not-found" && read.name === name
      ? new Set(read.blocks.map((block) => block.id))
      : undefined;
  return {
    asks: first.filter((ask) => ask.id === undefined || currentIds?.has(ask.id) !== true),
    textMissing: false,
    asksPartial: false,
  };
}

/** The blocks of the document the call names, or why there are none. */
async function readDocument(
  deps: RulesCheckDeps,
  call: RulesCheckCall
): Promise<DocumentBlocks | "not-found" | "failed"> {
  const { issue, project, ref } = call.input;
  // An upload names its document by `--name`; every other call by the document argument doc-edit takes.
  const named =
    call.toolName === "dispatch_artifact" ? uploadName(call.input) : call.input.artifact;
  const target: DocumentTarget = {
    ...(typeof issue === "string" ? { issue } : {}),
    ...(typeof project === "string" ? { project } : {}),
    ...(typeof ref === "string" ? { ref } : {}),
    ...(typeof named === "string" ? { artifact: named } : {}),
  };
  try {
    const directory = await call.directory();
    if (directory === undefined) return "failed";
    const session = deps.dispatchClient(directory, call.signal);
    if (session === undefined) return "failed";
    return await readDocumentBlocks(session.client, target, {
      cwd: directory,
      env: deps.env,
      serverUrls: session.serverUrls,
    });
  } catch {
    // A directory bash could not name, a configuration that does not read, a refused reference, a
    // server that did not answer, a document that changed as it was read: the asks are judged
    // without it.
    return "failed";
  }
}

/**
 * The rules check of one gated call: `undefined` when it opens no new ask, so no model is asked and
 * the entry records nothing of it. An upload left with nothing to judge because its read failed is
 * not that call: it answers `unjudged` with no model call, so the entry and the record say it went
 * through unjudged. Its own failure (a read, a render, the model) never refuses a call: the charter
 * check decides alone, and the outcome is recorded.
 */
export async function runRulesCheck(
  deps: RulesCheckDeps,
  call: RulesCheckCall
): Promise<RulesCheck | undefined> {
  const began = deps.now();
  const ended = (outcome: RuleOutcome, rest: Partial<RulesCheck> = {}): RulesCheck => ({
    outcome,
    names: [],
    matches: [],
    reason: "",
    promptBytes: 0,
    latencyMs: deps.now() - began,
    textMissing: false,
    asksCut: 0,
    asksPartial: false,
    ...rest,
  });
  const first = newAsks(call.toolName, call.input);
  if (first.length === 0) return undefined;
  try {
    const { asks, textMissing, asksPartial } = await readAsks(deps, call, first);
    if (asks.length === 0)
      // With the read failed and every opener carrying an id, nothing says which of them are new:
      // the call may add an ask no one judges, so it answers rather than stays silent. Its latency
      // is the read's, the only time it spent.
      return asksPartial ? ended("unjudged", { asksPartial }) : undefined;
    const described = describeAsks(asks);
    const user = () => {
      const { primarySystemPrompt, transcript } = call.context();
      return renderTemplate(requestTemplate, {
        primarySystemPrompt: escapeXml(primarySystemPrompt),
        transcript,
        tool: call.tool,
        asks: described.list,
      });
    };
    const walked = await deps.walk({ system: call.system, user, label: " rules" });
    const { answer, called } = walked;
    const verdict =
      answer.kind === "answer" ? parseRuleVerdict(answer.text, asks.length) : undefined;
    const outcome: RuleOutcome =
      answer.kind !== "answer"
        ? answer.kind
        : verdict === undefined
          ? "no-verdict"
          : verdict.matches.length > 0
            ? "match"
            : "none";
    return ended(outcome, {
      names: described.names,
      matches: verdict?.matches.toSorted((a, b) => a.ask - b.ask) ?? [],
      reason: verdict?.reason ?? "",
      ...(answer.kind === "error"
        ? { error: answer.reason }
        : outcome === "timeout" || outcome === "no-verdict"
          ? { error: outcome }
          : {}),
      ...(called.model === undefined ? {} : { model: called.model }),
      promptBytes: called.promptBytes ?? 0,
      textMissing,
      asksCut: described.asksCut,
      asksPartial,
      ...(called.usage === undefined ? {} : { usage: called.usage }),
    });
  } catch (error) {
    // The check's own failure (its read, its render, its dump) changes nothing: the charter decides.
    return ended("error", { error: error instanceof Error ? error.message : String(error) });
  }
}

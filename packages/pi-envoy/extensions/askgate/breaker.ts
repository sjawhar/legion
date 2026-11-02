// AskGate's breaker: what one gated call is to the loop of refusals on its target, and what the
// gate has already said there. A leaf module beside `rules-check.ts`: it reads a call's own
// arguments and the rules check's result, and imports nothing of the core, so `askgate-core.ts`
// holds the decision and this holds the counting.
import { createHash } from "node:crypto";
import { type NewAsk, newAsks } from "@legion/envoy-client/askgate-new-asks";
import type { RulesCheck } from "./rules-check";

/** A call's arguments, as the dry run printed them or the device validated them. */
type Input = Record<string, unknown>;

/** The arguments that name the target a write lands on, beside the tool. */
const BREAKER_KEYS = ["issue", "ask", "artifact", "project", "in_reply_to"] as const;

/** Refusals of one thing on one target after which the next attempt at it goes out unreviewed. */
export const GATE_BREAKER_REFUSALS = 2;

/**
 * The call's target: its tool and each of the issue, ask, artifact, project and reply its arguments
 * name, as the `key` every count is held under and the `label` the entry records and a reader joins
 * calls on. The key's parts are a JSON array, never joined on a separator: a value is the agent's
 * argument and can hold any character, so a joined key would let one target forge another and give
 * it the refusals counted on the forgery. The label is those same parts joined with spaces, built
 * here beside the key rather than read back out of it.
 */
export function breakerKey(toolName: string, input: Input): { key: string; label: string } {
  const parts = [toolName];
  for (const k of BREAKER_KEYS) if (input[k] !== undefined) parts.push(`${k}=${String(input[k])}`);
  return { key: JSON.stringify(parts), label: parts.join(" ") };
}

/**
 * How the breaker tells one new ask of a call from another: a block's `#id` and its opening, the
 * first words that name it in a refusal. The id keeps two blocks of one write that start alike
 * apart; an ask without one is its opening alone, so an agent that rewords only an ask's tail
 * resends the same thing and its loop still ends. A retype carries no text of its own in this
 * scan — only the rules check reads the block from Dispatch — so the block it names stands for it.
 * The parts are a JSON array, never joined on a separator: an opening is agent text and can hold
 * any character, so a joined key would let an ask without an id forge another ask's identity and
 * give that ask the refusals counted on the forgery.
 */
export const askIdentity = (ask: NewAsk): string =>
  ask.kind === "retype"
    ? JSON.stringify(["retype", ask.id ?? null])
    : JSON.stringify(["ask", ask.id ?? null, ask.opening]);

/** The breaker's name for a call that opens no new ask: the call as a whole. No ask identity is empty. */
const WHOLE_CALL = "";

/**
 * What a call is to the breaker on its target, read from the call's own reviewed arguments before
 * any check runs: the one key set both what a resend trips on (`Breaker.tripped`) and what a
 * refusal counts (`refusalThings`) are taken from. A call that is not an upload is each new ask it
 * opens, or the call as a whole when it opens none, so a call opening no ask counts its own
 * refusals, whatever asks were refused there before it.
 *
 * An upload is each ask block its document holds, the ones its stored version already holds
 * included, since no check has run to tell them apart, and last its upload key: the document's
 * name as Dispatch stores it (trimmed) and a digest of the text the gate reviewed, which for a
 * `--path` upload is the bytes the gate read from that path. Only the same upload sent again
 * yields that key, never another document on the same issue (the breaker's target names no
 * document) and never a call that is not an upload. An upload is never the call as a whole, which
 * every other upload on the issue opening no ask would share.
 */
export const callKeys = (toolName: string, input: Input): string[] => {
  const asks = newAsks(toolName, input);
  if (toolName !== "dispatch_artifact")
    return asks.length === 0 ? [WHOLE_CALL] : asks.map(askIdentity);
  const content =
    typeof input.content === "string"
      ? createHash("sha256").update(input.content).digest("hex")
      : null;
  const name = typeof input.name === "string" ? input.name.trim() : null;
  return [...asks.map(askIdentity), JSON.stringify(["upload", name, content])];
};

/**
 * The things one refusal counts on its target: always some of the refused call's own keys
 * (`callKeys`), so the warning a refusal gives is true by construction. A refusal warns that the
 * same call sent again goes out unreviewed only when a thing it counted reaches the breaker's
 * count, and that resend's keys hold every such thing, so it trips. Nothing is counted that the
 * call's keys lack.
 *
 * A rules refusal counts the asks it matched, each numbered on the list the check judged, which for
 * an upload already leaves out the blocks its stored version holds. A charter refusal of any other
 * call counts every key of it, its scan holding only asks it opens. A charter refusal of an upload
 * never counts a block its stored version holds, since its keys carry every block of the document
 * and counting the stored ones would make every later upload of that document trip on a block no
 * check read, sending its own new asks out unreviewed. It counts the asks the rules check judged,
 * the ones its read found new or, its read failed, the openers with no id; where the check judged
 * none (it did not run, its stored version holding every opener, or it judged nothing), it counts
 * the upload key alone, so only that upload sent again unchanged ever trips on it.
 */
export const refusalThings = (
  toolName: string,
  keys: readonly string[],
  ruled: RulesCheck | undefined
): string[] => {
  const counted =
    ruled !== undefined && ruled.matches.length > 0
      ? ruled.matches.map((match) => askIdentity(ruled.judged[match.ask - 1]))
      : toolName !== "dispatch_artifact"
        ? keys
        : ruled !== undefined && ruled.judged.length > 0
          ? ruled.judged.map(askIdentity)
          : keys.slice(-1);
  return counted.filter((thing) => keys.includes(thing));
};

/**
 * What the gate has said on one target since the breaker last reset it. `refusals` counts, per
 * thing (`callKeys`), the calls the gate did not send; `revises` counts every revise,
 * the warn-mode charter ones that sent the call included; `charter` keeps the charter's reasons.
 */
type Held = { refusals: Map<string, number>; revises: number; charter: string[] };

/**
 * The breaker: a loop of refusals of one thing on one target ends on the third attempt, which goes
 * out unreviewed. It counts refusals, since a warn-mode revise sends the call and leaves no loop.
 *
 * A call trips when one of its own keys (`callKeys`) has been refused GATE_BREAKER_REFUSALS times,
 * and the refusal that brings a thing to that count warns of it. A refusal counts only keys of the
 * call it refused (`refusalThings`), so the warning and the pass are about the same thing, and the
 * call that warning names, sent again, trips. A call with several keys trips on any one of them: a
 * pass that needed every ask refused would never end a loop on one ask of several that the rules
 * check keeps refusing.
 *
 * The reset stays per target while the count is per thing: an allow, a rebuttal or a trip sends a
 * call on the target, which ends every loop there, so each thing on it starts again from zero.
 * Clearing more than the call's own things only ever costs a review, never gives a pass. A halted
 * gate sends calls before the breaker sees them, so a halt neither counts nor resets anything; a
 * session_switch or session_branch clears the halt and the breaker together.
 */
export class Breaker {
  readonly #held = new Map<string, Held>();
  /**
   * Records one answer of the gate on a target: the charter's reason when it gave one, and the
   * things the gate refused, none for a revise that sent the call. Returns whether a thing it
   * refused has now been refused GATE_BREAKER_REFUSALS times, so this refusal must warn that the
   * same call again goes out unreviewed.
   *
   * One refusal moves each thing once, however often it is named: both callers can name a thing
   * twice (two asks of one call that share an identity, or two rules matches on them), and a
   * count moved twice would warn on a first refusal and pass the next send unreviewed.
   */
  refused(key: string, refused: readonly string[], charter?: string): boolean {
    let held = this.#held.get(key);
    if (held === undefined) {
      held = { refusals: new Map(), revises: 0, charter: [] };
      this.#held.set(key, held);
    }
    held.revises++;
    if (charter !== undefined) held.charter.push(charter);
    let tripsNext = false;
    for (const thing of new Set(refused)) {
      const count = (held.refusals.get(thing) ?? 0) + 1;
      held.refusals.set(thing, count);
      if (count >= GATE_BREAKER_REFUSALS) tripsNext = true;
    }
    return tripsNext;
  }
  /**
   * Whether one of a call's things has been refused often enough on its target to send it
   * unreviewed. The things are asked for only where the target holds a refusal.
   */
  tripped(key: string, things: () => readonly string[]): boolean {
    const refusals = this.#held.get(key)?.refusals;
    if (refusals === undefined || refusals.size === 0) return false;
    return things().some((thing) => (refusals.get(thing) ?? 0) >= GATE_BREAKER_REFUSALS);
  }
  reset(key: string): void {
    this.#held.delete(key);
  }
  clear(): void {
    this.#held.clear();
  }
  /** How many times the gate answered revise on this target since its reset, the warn-mode ones included. */
  revises(key: string): number {
    return this.#held.get(key)?.revises ?? 0;
  }
  /** The charter's own reasons. They alone feed the charter request, so a rules reason never reads as one the charter gave. */
  charterReasons(key: string): readonly string[] {
    return this.#held.get(key)?.charter ?? [];
  }
}

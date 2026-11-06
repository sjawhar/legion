/**
 * What the provider that ended a turn left behind: nothing wrong, a content filter, or any other
 * provider failure. A caller holding the turn's last assistant message reads it with
 * `classifyProviderTurn`, and one holding an error text alone with `classifyProviderError`; both
 * read the one list of patterns below, so a refusal reads the same either way.
 *
 * A worker whose turn ends in a content filter has to step down Oh My Pi's model fallback chain,
 * because the same prompt on the same model is refused again; a 429 or a 529 is the opposite, a
 * model that works and is busy, which the host's own retry already handles. So the two are told
 * apart here, once, for the reader that walks the chain: pi-legion's model fallback
 * (`packages/pi-legion/src/model-fallback.ts`).
 *
 * Every provider writes its own refusal text, and the host hands an extension the message the
 * provider left, not a code, so the texts below are matched as Oh My Pi writes them at the pinned
 * tag `v18.16.0-sami.20261104-021732`. The module reads its input as `unknown` and narrows it
 * field by field, as `./phase-stall` does: a host whose message shape differs falls through to
 * `clean` rather than throwing inside an event handler.
 */

import type { AgentEndEvent } from "./pi-types";

export type ProviderTurnOutcome =
  | { kind: "clean" }
  | { kind: "content-filter"; category?: string }
  | { kind: "provider-error" };

/** Anthropic's refusal text: `Refusal`, or `Refusal (<category>)`, either one optionally followed
 * by `: <explanation>` (`packages/ai/src/providers/anthropic.ts:3121-3126` at the pinned tag).
 * The category is bounded as the Google reasons below are, and for the same reason. */
const ANTHROPIC_REFUSAL = /^Refusal(?: \(([^)]{0,200})\))?(?::|$)/;

/** What OpenAI's own filter leaves: the chat completion's `finish_reason`
 * (`packages/ai/src/providers/openai-completions.ts:3079-3080` at the pinned tag) and the
 * responses API's incomplete reason. */
const OPENAI_FILTER_TEXTS = [
  "Provider finish_reason: content_filter",
  "incomplete: content_filter",
];

/** Gemini finish reasons that are a content filter; every `IMAGE_*` reason is one too. A finish
 * reason outside this set (`OTHER`, `MALFORMED_FUNCTION_CALL`, `LANGUAGE`, …) ends the turn as an
 * error that another model would not refuse (`packages/ai/src/providers/google-shared.ts:456-464`
 * at the pinned tag). */
const GOOGLE_FILTER_REASONS: Record<string, true> = {
  SAFETY: true,
  PROHIBITED_CONTENT: true,
  BLOCKLIST: true,
  SPII: true,
  RECITATION: true,
};

/** `packages/ai/src/providers/google-shared.ts:851` at the pinned tag.
 *
 * Every repetition below is bounded, and that bound is what keeps a long error text cheap. The
 * text is the provider's own and nothing limits its length, so an unbounded `[^)]*` behind a
 * literal the text repeats costs one scan to the end of the text per repetition: 16,000
 * repetitions of `Request blocked by Google (` took 12.3 s in one call, measured in a worker pod
 * on bun 1.4.4, which is the stalled worker this issue exists to stop (CodeQL reads the same
 * shape as `js/polynomial-redos`). A reason Google names is one short token, so a bound well
 * above the longest of them changes no answer and leaves the work linear in the text. */
const GOOGLE_FINISH_REASON = /Generation failed with finish reason: ([A-Z_]{1,64})/;

/** The prompt feedback Google returns in place of a candidate (`google-shared.ts:806` at the
 * pinned tag), and the same block as the Gemini-through-Droid provider words it. */
const GOOGLE_PROMPT_BLOCKED = /Request blocked by Google \(([^)]{0,200})\)/;
const GOOGLE_FILTERS_BLOCKED = /Generation was blocked by content filters \(([^)]{0,200})\)/;

/** `packages/ai/src/providers/amazon-bedrock.ts:1071-1075` at the pinned tag: the service's own
 * filters, and a guardrail the request configured. */
const BEDROCK_FILTER_TEXTS = [
  "Response filtered by Amazon Bedrock content filters",
  "Response blocked by Amazon Bedrock guardrail",
];

/** What the host puts in front of the provider's own error text when it names why it walked the
 * fallback chain: ``reason: `Request failed: ${message.errorMessage ?? "provider returned an
 * error without details"}` `` (`packages/coding-agent/src/session/turn-recovery.ts` at the
 * pinned tag). It is taken off before the texts above are read, so one list of patterns reads
 * both a message's own error text and that reason. */
const REQUEST_FAILED = /^Request failed: /;

/** How the provider ended the turn that this message settled. */
export function classifyProviderTurn(message: unknown): ProviderTurnOutcome {
  if (typeof message !== "object" || message === null) return { kind: "clean" };
  if (!("stopReason" in message) || message.stopReason !== "error") return { kind: "clean" };
  const fromDetails = filterFromStopDetails(message);
  return fromDetails ?? classifyProviderError(errorText(message));
}

/**
 * How the provider ended the turn an `agent_end` event settled: the class of its last assistant
 * message, which is the message that says how the run ended, and `clean` for a run with no
 * assistant message at all.
 *
 * The `agent_end` handler of `packages/pi-legion/extensions/legion.ts` is the one caller: a
 * settled turn read here is what moves the session down its model chain or puts the chain's wait
 * back to the base (`packages/pi-legion/src/model-fallback.ts`).
 */
export function classifyTurnEnd(event: AgentEndEvent): ProviderTurnOutcome {
  return classifyProviderTurn(event.messages?.findLast((message) => message.role === "assistant"));
}

/**
 * How the provider ended a turn, read from its error text alone: the content filter the text
 * names, and a provider error for every other text, as `classifyProviderTurn` reads it. No text
 * answers `clean`: a text is an error's.
 *
 * This is the entry point for a caller that holds a text and no message, which is what the
 * host's `retry_fallback_applied` hands one (`packages/pi-legion/src/model-fallback.ts`). A
 * content filter answers the same outcome either way, its category included, so a refusal reads
 * the same whichever of the two a caller has.
 */
export function classifyProviderError(text: string): ProviderTurnOutcome {
  return filterFromText(text.replace(REQUEST_FAILED, "").trim()) ?? { kind: "provider-error" };
}

/** The filter the message's own `stopDetails` names, when it names one: what Anthropic sends as a
 * structured stop, which needs no text matching at all. */
function filterFromStopDetails(message: object): ProviderTurnOutcome | undefined {
  if (!("stopDetails" in message)) return undefined;
  const details = message.stopDetails;
  if (typeof details !== "object" || details === null || !("type" in details)) return undefined;
  if (
    details.type !== "refusal" &&
    details.type !== "sensitive" &&
    details.type !== "content_filter"
  )
    return undefined;
  const category =
    "category" in details && typeof details.category === "string" && details.category.length > 0
      ? details.category
      : undefined;
  return category === undefined ? { kind: "content-filter" } : { kind: "content-filter", category };
}

/** The message's error text, trimmed; empty when it carries none. */
function errorText(message: object): string {
  if (!("errorMessage" in message) || typeof message.errorMessage !== "string") return "";
  return message.errorMessage.trim();
}

/** The filter one provider's error text names, or undefined when the text is some other failure. */
function filterFromText(text: string): ProviderTurnOutcome | undefined {
  const anthropic = ANTHROPIC_REFUSAL.exec(text);
  if (anthropic !== null) {
    const category = anthropic[1];
    return category === undefined || category.length === 0
      ? { kind: "content-filter" }
      : { kind: "content-filter", category };
  }
  if (OPENAI_FILTER_TEXTS.some((phrase) => text.includes(phrase)))
    return { kind: "content-filter" };
  const google = googleFilter(text);
  if (google !== undefined) return google;
  if (BEDROCK_FILTER_TEXTS.some((phrase) => text.includes(phrase)))
    return { kind: "content-filter" };
  return undefined;
}

/** Google's three filter texts. A `Generation failed with finish reason:` naming a reason outside
 * the filter set is this provider's text and no filter, so it stops the walk here. */
function googleFilter(text: string): ProviderTurnOutcome | undefined {
  const finish = GOOGLE_FINISH_REASON.exec(text);
  if (finish !== null) {
    const reason = finish[1];
    if (reason === undefined || !(GOOGLE_FILTER_REASONS[reason] || reason.startsWith("IMAGE_")))
      return undefined;
    return { kind: "content-filter", category: reason };
  }
  const blocked = GOOGLE_PROMPT_BLOCKED.exec(text) ?? GOOGLE_FILTERS_BLOCKED.exec(text);
  if (blocked === null) return undefined;
  const reason = blocked[1];
  return reason === undefined || reason.length === 0
    ? { kind: "content-filter" }
    : { kind: "content-filter", category: reason };
}

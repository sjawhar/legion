/**
 * What a turn's last assistant message says about the provider that ended it: nothing wrong, a
 * content filter, or any other provider failure.
 *
 * A worker whose turn ends in a content filter has to step down Oh My Pi's model fallback chain,
 * because the same prompt on the same model is refused again; a 429 or a 529 is the opposite, a
 * model that works and is busy, which the host's own retry already handles. So the two are told
 * apart here, once, and the daemon reads `refused` differently from `provider-error`.
 *
 * Every provider writes its own refusal text, and the host hands an extension the message the
 * provider left, not a code, so the texts below are matched as Oh My Pi writes them at the pinned
 * tag `v18.16.0-sami.20261104-021732`. The module reads its input as `unknown` and narrows it
 * field by field, as `./phase-stall` does: a host whose message shape differs falls through to
 * `clean` rather than throwing inside an event handler.
 */

export type ProviderTurnOutcome =
  | { kind: "clean" }
  | { kind: "content-filter"; category?: string; provider?: string }
  | { kind: "provider-error"; status?: number };

/** Anthropic's refusal text: `Refusal`, or `Refusal (<category>)`, either one optionally followed
 * by `: <explanation>` (`packages/ai/src/providers/anthropic.ts:3121-3126` at the pinned tag). */
const ANTHROPIC_REFUSAL = /^Refusal(?: \(([^)]*)\))?(?::|$)/;

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

/** `packages/ai/src/providers/google-shared.ts:851` at the pinned tag. */
const GOOGLE_FINISH_REASON = /Generation failed with finish reason: ([A-Z_]+)/;

/** The prompt feedback Google returns in place of a candidate (`google-shared.ts:806` at the
 * pinned tag), and the same block as the Gemini-through-Droid provider words it. */
const GOOGLE_PROMPT_BLOCKED = /Request blocked by Google \(([^)]*)\)/;
const GOOGLE_FILTERS_BLOCKED = /Generation was blocked by content filters \(([^)]*)\)/;

/** `packages/ai/src/providers/amazon-bedrock.ts:1071-1075` at the pinned tag: the service's own
 * filters, and a guardrail the request configured. */
const BEDROCK_FILTER_TEXTS = [
  "Response filtered by Amazon Bedrock content filters",
  "Response blocked by Amazon Bedrock guardrail",
];

/** An HTTP status leading the error text, as a provider error carries it (`429 Too Many
 * Requests`, `529 Overloaded`). */
const LEADING_STATUS = /^(\d{3})\b/;

/** The `status:<code>` form Oh My Pi writes for an error it knows only by its status
 * (`packages/coding-agent/src/extensibility/shared-events.ts:285` at the pinned tag). */
const TAGGED_STATUS = /\bstatus:(\d{3})\b/;

/** How the provider ended the turn that this message settled. */
export function classifyProviderTurn(message: unknown): ProviderTurnOutcome {
  if (typeof message !== "object" || message === null) return { kind: "clean" };
  if (!("stopReason" in message) || message.stopReason !== "error") return { kind: "clean" };
  const fromDetails = filterFromStopDetails(message);
  if (fromDetails !== undefined) return fromDetails;
  const text = errorText(message);
  const fromText = filterFromText(text);
  if (fromText !== undefined) return fromText;
  const status = statusOf(LEADING_STATUS.exec(text)) ?? statusOf(TAGGED_STATUS.exec(text));
  return status === undefined ? { kind: "provider-error" } : { kind: "provider-error", status };
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
      ? { kind: "content-filter", provider: "anthropic" }
      : { kind: "content-filter", provider: "anthropic", category };
  }
  if (OPENAI_FILTER_TEXTS.some((phrase) => text.includes(phrase)))
    return { kind: "content-filter", provider: "openai" };
  const google = googleFilter(text);
  if (google !== undefined) return google;
  if (BEDROCK_FILTER_TEXTS.some((phrase) => text.includes(phrase)))
    return { kind: "content-filter", provider: "amazon-bedrock" };
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
    return { kind: "content-filter", provider: "google", category: reason };
  }
  const blocked = GOOGLE_PROMPT_BLOCKED.exec(text) ?? GOOGLE_FILTERS_BLOCKED.exec(text);
  if (blocked === null) return undefined;
  const reason = blocked[1];
  return reason === undefined || reason.length === 0
    ? { kind: "content-filter", provider: "google" }
    : { kind: "content-filter", provider: "google", category: reason };
}

/** The matched digits as a status, or undefined when they are no HTTP status (100-599). */
function statusOf(match: RegExpExecArray | null): number | undefined {
  const digits = match?.[1];
  if (digits === undefined) return undefined;
  const status = Number.parseInt(digits, 10);
  return status >= 100 && status <= 599 ? status : undefined;
}

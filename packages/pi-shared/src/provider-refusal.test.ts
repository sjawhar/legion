import { describe, expect, test } from "bun:test";
import {
  classifyProviderError,
  classifyProviderTurn,
  type ProviderTurnOutcome,
} from "./provider-refusal";

/** A turn that ended on a provider error, as `agent_end` hands its last assistant message. */
function errorTurn(errorMessage: string): unknown {
  return { role: "assistant", stopReason: "error", errorMessage };
}

describe("a provider's own refusal text", () => {
  test("Anthropic's bare refusal is a content filter with no category", () => {
    expect(classifyProviderTurn(errorTurn("Refusal"))).toEqual({
      kind: "content-filter",
      provider: "anthropic",
    });
  });

  test("Anthropic names the category in parentheses and the explanation after the colon", () => {
    expect(classifyProviderTurn(errorTurn("Refusal (csam): we cannot help with that"))).toEqual({
      kind: "content-filter",
      provider: "anthropic",
      category: "csam",
    });
    expect(classifyProviderTurn(errorTurn("Refusal: we cannot help with that"))).toEqual({
      kind: "content-filter",
      provider: "anthropic",
    });
    expect(classifyProviderTurn(errorTurn("Refusal (no details provided)"))).toEqual({
      kind: "content-filter",
      provider: "anthropic",
      category: "no details provided",
    });
  });

  test("OpenAI's two filter texts are a content filter", () => {
    expect(classifyProviderTurn(errorTurn("Provider finish_reason: content_filter"))).toEqual({
      kind: "content-filter",
      provider: "openai",
    });
    expect(classifyProviderTurn(errorTurn("incomplete: content_filter"))).toEqual({
      kind: "content-filter",
      provider: "openai",
    });
  });

  test("a Gemini finish reason inside the filter set carries that reason as the category", () => {
    for (const reason of [
      "SAFETY",
      "PROHIBITED_CONTENT",
      "BLOCKLIST",
      "SPII",
      "RECITATION",
      "IMAGE_SAFETY",
      "IMAGE_PROHIBITED_CONTENT",
      "IMAGE_RECITATION",
      "IMAGE_OTHER",
    ]) {
      expect(
        classifyProviderTurn(errorTurn(`Generation failed with finish reason: ${reason}`))
      ).toEqual({ kind: "content-filter", provider: "google", category: reason });
    }
  });

  test("a Gemini finish reason outside the filter set is a provider error", () => {
    for (const reason of ["OTHER", "MALFORMED_FUNCTION_CALL", "LANGUAGE", "UNEXPECTED_TOOL_CALL"]) {
      expect(
        classifyProviderTurn(errorTurn(`Generation failed with finish reason: ${reason}`))
      ).toEqual({ kind: "provider-error" });
    }
  });

  test("Google's two blocked texts carry their reason as the category", () => {
    expect(classifyProviderTurn(errorTurn("Request blocked by Google (SAFETY): hate"))).toEqual({
      kind: "content-filter",
      provider: "google",
      category: "SAFETY",
    });
    expect(
      classifyProviderTurn(errorTurn("Generation was blocked by content filters (PROHIBITED)"))
    ).toEqual({ kind: "content-filter", provider: "google", category: "PROHIBITED" });
  });

  test("Amazon Bedrock's filters and guardrails are a content filter", () => {
    expect(
      classifyProviderTurn(
        errorTurn("Response filtered by Amazon Bedrock content filters (stop reason: guardrail).")
      )
    ).toEqual({ kind: "content-filter", provider: "amazon-bedrock" });
    expect(
      classifyProviderTurn(
        errorTurn("Response blocked by Amazon Bedrock guardrail (stop reason: guardrail).")
      )
    ).toEqual({ kind: "content-filter", provider: "amazon-bedrock" });
  });
});

describe("a structured stop the provider sent", () => {
  test("a content_filter stop is a content filter with no text to read", () => {
    expect(
      classifyProviderTurn({ stopReason: "error", stopDetails: { type: "content_filter" } })
    ).toEqual({ kind: "content-filter" });
  });

  test("a refusal or sensitive stop carries its category", () => {
    expect(
      classifyProviderTurn({
        stopReason: "error",
        stopDetails: { type: "refusal", category: "csam", explanation: "no" },
      })
    ).toEqual({ kind: "content-filter", category: "csam" });
    expect(
      classifyProviderTurn({ stopReason: "error", stopDetails: { type: "sensitive" } })
    ).toEqual({ kind: "content-filter" });
  });
});

describe("a provider failure that is no refusal", () => {
  // The step-down exists for a model that refuses the prompt. A busy model answers the same prompt
  // a minute later, so a rate limit and an overload stay provider errors.
  test("a 429 and a 529 are provider errors carrying their status", () => {
    expect(classifyProviderTurn(errorTurn("429 Too Many Requests"))).toEqual({
      kind: "provider-error",
      status: 429,
    });
    expect(classifyProviderTurn(errorTurn("529 Overloaded"))).toEqual({
      kind: "provider-error",
      status: 529,
    });
  });

  test("the status:<code> form Oh My Pi writes is read as the status", () => {
    expect(classifyProviderTurn(errorTurn("Provider error (status:503)"))).toEqual({
      kind: "provider-error",
      status: 503,
    });
  });

  test("an error naming no status is a provider error with none", () => {
    expect(classifyProviderTurn(errorTurn("socket hang up"))).toEqual({ kind: "provider-error" });
    expect(classifyProviderTurn({ stopReason: "error" })).toEqual({ kind: "provider-error" });
  });

  test("a number that is no HTTP status is not read as one", () => {
    expect(classifyProviderTurn(errorTurn("999 tokens over the limit"))).toEqual({
      kind: "provider-error",
    });
    expect(classifyProviderTurn(errorTurn("8080 refused the connection"))).toEqual({
      kind: "provider-error",
    });
  });
});

describe("a turn no provider ended badly", () => {
  test("a stop reason that is not an error is clean, whatever text rides along", () => {
    expect(classifyProviderTurn({ role: "assistant", stopReason: "stop" })).toEqual({
      kind: "clean",
    });
    const aborted = { stopReason: "aborted", errorMessage: "Refusal (csam)" };
    expect(classifyProviderTurn(aborted)).toEqual({ kind: "clean" });
    expect(classifyProviderTurn({ role: "assistant" })).toEqual({ kind: "clean" });
  });

  test("a message that is not a readable object is clean", () => {
    expect(classifyProviderTurn(undefined)).toEqual({ kind: "clean" });
    expect(classifyProviderTurn(null)).toEqual({ kind: "clean" });
    expect(classifyProviderTurn("Refusal")).toEqual({ kind: "clean" });
  });
});

describe("an error text on its own", () => {
  /** Every filter text the module matches, and the outcome it reads as. */
  const FILTERS: readonly [string, ProviderTurnOutcome][] = [
    ["Refusal", { kind: "content-filter", provider: "anthropic" }],
    [
      "Refusal (csam): we cannot help with that",
      { kind: "content-filter", provider: "anthropic", category: "csam" },
    ],
    ["Refusal: we cannot help with that", { kind: "content-filter", provider: "anthropic" }],
    ["Provider finish_reason: content_filter", { kind: "content-filter", provider: "openai" }],
    ["incomplete: content_filter", { kind: "content-filter", provider: "openai" }],
    [
      "Generation failed with finish reason: SAFETY",
      { kind: "content-filter", provider: "google", category: "SAFETY" },
    ],
    [
      "Generation failed with finish reason: IMAGE_SAFETY",
      { kind: "content-filter", provider: "google", category: "IMAGE_SAFETY" },
    ],
    [
      "Request blocked by Google (SAFETY): hate",
      { kind: "content-filter", provider: "google", category: "SAFETY" },
    ],
    [
      "Generation was blocked by content filters (PROHIBITED)",
      { kind: "content-filter", provider: "google", category: "PROHIBITED" },
    ],
    [
      "Response filtered by Amazon Bedrock content filters (stop reason: guardrail).",
      { kind: "content-filter", provider: "amazon-bedrock" },
    ],
    [
      "Response blocked by Amazon Bedrock guardrail (stop reason: guardrail).",
      { kind: "content-filter", provider: "amazon-bedrock" },
    ],
  ];

  test("reads every provider's filter text as the turn's own message reads it", () => {
    for (const [text, outcome] of FILTERS) {
      expect(classifyProviderError(text)).toEqual(outcome);
      // The one list of patterns: the two exports answer the same text the same way.
      expect(classifyProviderTurn(errorTurn(text))).toEqual(outcome);
    }
  });

  test("reads a filter text the host wrapped as the reason it walked its chain for", () => {
    // What `retry_fallback_applied` carried in the live pane proof of INFRA-956 R1.
    expect(classifyProviderError("Request failed: Refusal (cyber): Declined")).toEqual({
      kind: "content-filter",
      provider: "anthropic",
      category: "cyber",
    });
    for (const [text, outcome] of FILTERS) {
      expect(classifyProviderError(`Request failed: ${text}`)).toEqual(outcome);
    }
  });

  test("reads a 429 and a 529 as provider errors carrying their status", () => {
    expect(classifyProviderError("429 Too Many Requests")).toEqual({
      kind: "provider-error",
      status: 429,
    });
    expect(classifyProviderError("Request failed: 429 Too Many Requests")).toEqual({
      kind: "provider-error",
      status: 429,
    });
    expect(classifyProviderError("529 Overloaded")).toEqual({
      kind: "provider-error",
      status: 529,
    });
    expect(classifyProviderError("Request failed: 529 Overloaded")).toEqual({
      kind: "provider-error",
      status: 529,
    });
  });

  test("reads anything else as a provider error, an empty text included", () => {
    expect(classifyProviderError("socket hang up")).toEqual({ kind: "provider-error" });
    expect(classifyProviderError("Generation failed with finish reason: OTHER")).toEqual({
      kind: "provider-error",
    });
    expect(
      classifyProviderError("Request failed: provider returned an error without details")
    ).toEqual({ kind: "provider-error" });
    expect(classifyProviderError("")).toEqual({ kind: "provider-error" });
  });
});

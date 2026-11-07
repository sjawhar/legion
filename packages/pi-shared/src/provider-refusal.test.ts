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
    expect(classifyProviderTurn(errorTurn("Refusal"))).toEqual({ kind: "content-filter" });
  });

  test("Anthropic names the category in parentheses and the explanation after the colon", () => {
    expect(classifyProviderTurn(errorTurn("Refusal (csam): we cannot help with that"))).toEqual({
      kind: "content-filter",
      category: "csam",
    });
    expect(classifyProviderTurn(errorTurn("Refusal: we cannot help with that"))).toEqual({
      kind: "content-filter",
    });
    expect(classifyProviderTurn(errorTurn("Refusal (no details provided)"))).toEqual({
      kind: "content-filter",
      category: "no details provided",
    });
  });

  test("OpenAI's two filter texts are a content filter", () => {
    expect(classifyProviderTurn(errorTurn("Provider finish_reason: content_filter"))).toEqual({
      kind: "content-filter",
    });
    expect(classifyProviderTurn(errorTurn("incomplete: content_filter"))).toEqual({
      kind: "content-filter",
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
      ).toEqual({ kind: "content-filter", category: reason });
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
      category: "SAFETY",
    });
    expect(
      classifyProviderTurn(errorTurn("Generation was blocked by content filters (PROHIBITED)"))
    ).toEqual({ kind: "content-filter", category: "PROHIBITED" });
  });

  test("Amazon Bedrock's filters and guardrails are a content filter", () => {
    expect(
      classifyProviderTurn(
        errorTurn("Response filtered by Amazon Bedrock content filters (stop reason: guardrail).")
      )
    ).toEqual({ kind: "content-filter" });
    expect(
      classifyProviderTurn(
        errorTurn("Response blocked by Amazon Bedrock guardrail (stop reason: guardrail).")
      )
    ).toEqual({ kind: "content-filter" });
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
  test("a rate limit, an overload and every other failure are a provider error", () => {
    for (const text of [
      "429 Too Many Requests",
      "529 Overloaded",
      "Provider error (status:503)",
      "socket hang up",
    ]) {
      expect(classifyProviderTurn(errorTurn(text))).toEqual({ kind: "provider-error" });
    }
    expect(classifyProviderTurn({ stopReason: "error" })).toEqual({ kind: "provider-error" });
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
    ["Refusal", { kind: "content-filter" }],
    ["Refusal (csam): we cannot help with that", { kind: "content-filter", category: "csam" }],
    ["Refusal: we cannot help with that", { kind: "content-filter" }],
    ["Provider finish_reason: content_filter", { kind: "content-filter" }],
    ["incomplete: content_filter", { kind: "content-filter" }],
    [
      "Generation failed with finish reason: SAFETY",
      { kind: "content-filter", category: "SAFETY" },
    ],
    [
      "Generation failed with finish reason: IMAGE_SAFETY",
      { kind: "content-filter", category: "IMAGE_SAFETY" },
    ],
    ["Request blocked by Google (SAFETY): hate", { kind: "content-filter", category: "SAFETY" }],
    [
      "Generation was blocked by content filters (PROHIBITED)",
      { kind: "content-filter", category: "PROHIBITED" },
    ],
    [
      "Response filtered by Amazon Bedrock content filters (stop reason: guardrail).",
      { kind: "content-filter" },
    ],
    [
      "Response blocked by Amazon Bedrock guardrail (stop reason: guardrail).",
      { kind: "content-filter" },
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
      category: "cyber",
    });
    for (const [text, outcome] of FILTERS) {
      expect(classifyProviderError(`Request failed: ${text}`)).toEqual(outcome);
    }
  });

  test("reads a rate limit and an overload as a provider error, wrapped or not", () => {
    for (const text of ["429 Too Many Requests", "529 Overloaded"]) {
      expect(classifyProviderError(text)).toEqual({ kind: "provider-error" });
      expect(classifyProviderError(`Request failed: ${text}`)).toEqual({ kind: "provider-error" });
    }
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

describe("an error text a provider chose the length of", () => {
  /** The text a provider error carries is the provider's own, and nothing bounds its length: the
   * host hands `message.errorMessage` through as it arrived, and the fallback reason is that text
   * behind `Request failed: `. A gateway that answers an error with its own page makes it long,
   * so every pattern this module runs must stay linear in that length. CodeQL reported the two
   * Google patterns as polynomial (`js/polynomial-redos`, two high alerts) at the head before the
   * patterns were bounded, and reports neither since. */

  test("classifies a long text that almost matches a Google filter in linear time", () => {
    for (const prefix of [
      "Request blocked by Google (",
      "Generation was blocked by content filters (",
    ]) {
      const text = prefix.repeat(16_000);
      const started = performance.now();
      expect(classifyProviderError(text)).toEqual({ kind: "provider-error" });
      const spent = performance.now() - started;
      expect({ prefix, spent: spent < 1_000 }).toEqual({ prefix, spent: true });
    }
  }, 60_000);
});

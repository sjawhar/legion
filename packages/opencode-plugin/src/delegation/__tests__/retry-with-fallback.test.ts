/**
 * Tests for createRetryWithFallback — runtime retry on model error (T7).
 *
 * Part of #276 / #200: OMO Replacement.
 *
 * Tests cover:
 * - 429 rate limit → retries with delay
 * - 5xx server error → retries with delay
 * - Timeout → retries with delay
 * - 400/401/403 → NOT retried (immediate fallback or error)
 * - Exhausted retries → triggers fallback chain
 * - Config overrides for max retries and delay
 */

import { afterEach, beforeEach, describe, expect, it, jest } from "bun:test";
import type { RetryConfig } from "../../config";
import { createRetryWithFallback, isTransientError } from "../retry-with-fallback";

// Simulated error types matching real API error shapes
function makeApiError(status: number, message = `HTTP ${status}`): Error {
  const err = new Error(message);
  (err as Error & { status: number }).status = status;
  return err;
}

function makeTimeoutError(): Error {
  const err = new Error("Request timed out");
  err.name = "TimeoutError";
  return err;
}

function makeNetworkError(): Error {
  const err = new Error("fetch failed");
  err.name = "TypeError";
  return err;
}

describe("isTransientError", () => {
  it("classifies 429 rate limit as transient", () => {
    expect(isTransientError(makeApiError(429))).toBe(true);
  });

  it("classifies 500 server error as transient", () => {
    expect(isTransientError(makeApiError(500))).toBe(true);
  });

  it("classifies 502 bad gateway as transient", () => {
    expect(isTransientError(makeApiError(502))).toBe(true);
  });

  it("classifies 503 service unavailable as transient", () => {
    expect(isTransientError(makeApiError(503))).toBe(true);
  });

  it("classifies 529 overloaded as transient", () => {
    expect(isTransientError(makeApiError(529))).toBe(true);
  });

  it("classifies timeout errors as transient", () => {
    expect(isTransientError(makeTimeoutError())).toBe(true);
  });

  it("classifies network errors as transient", () => {
    expect(isTransientError(makeNetworkError())).toBe(true);
  });

  it("classifies 400 bad request as NOT transient", () => {
    expect(isTransientError(makeApiError(400))).toBe(false);
  });

  it("classifies 401 unauthorized as NOT transient", () => {
    expect(isTransientError(makeApiError(401))).toBe(false);
  });

  it("classifies 403 forbidden as NOT transient", () => {
    expect(isTransientError(makeApiError(403))).toBe(false);
  });

  it("classifies 404 not found as NOT transient", () => {
    expect(isTransientError(makeApiError(404))).toBe(false);
  });

  it("classifies 422 unprocessable as NOT transient", () => {
    expect(isTransientError(makeApiError(422))).toBe(false);
  });

  it("classifies errors with 'overloaded' message as transient", () => {
    const err = new Error("The API is temporarily overloaded, please try again later");
    expect(isTransientError(err)).toBe(true);
  });

  it("classifies errors with 'rate limit' message as transient", () => {
    const err = new Error("Rate limit exceeded for model");
    expect(isTransientError(err)).toBe(true);
  });
});

describe("createRetryWithFallback", () => {
  // Use fake timers to avoid real delays in tests
  let originalSetTimeout: typeof setTimeout;
  let timeoutCalls: Array<{ fn: () => void; ms: number }>;

  beforeEach(() => {
    originalSetTimeout = globalThis.setTimeout;
    timeoutCalls = [];
    // Mock setTimeout to execute immediately but record the delay
    globalThis.setTimeout = ((fn: () => void, ms: number) => {
      timeoutCalls.push({ fn, ms });
      fn(); // Execute immediately in tests
      return 0 as unknown as ReturnType<typeof setTimeout>;
    }) as typeof setTimeout;
  });

  afterEach(() => {
    globalThis.setTimeout = originalSetTimeout;
  });

  it("returns result on first success without retrying", async () => {
    const config: RetryConfig = { maxRetries: 2, delayMs: 1000 };
    const fn = jest.fn(() => Promise.resolve("success"));

    const retrier = createRetryWithFallback(config);
    const result = await retrier.execute(fn);

    expect(result).toBe("success");
    expect(fn).toHaveBeenCalledTimes(1);
    expect(timeoutCalls).toHaveLength(0);
  });

  it("retries 429 rate limit errors with delay", async () => {
    const config: RetryConfig = { maxRetries: 2, delayMs: 500 };
    let attempts = 0;
    const fn = jest.fn(() => {
      attempts++;
      if (attempts <= 1) return Promise.reject(makeApiError(429));
      return Promise.resolve("recovered");
    });

    const retrier = createRetryWithFallback(config);
    const result = await retrier.execute(fn);

    expect(result).toBe("recovered");
    expect(fn).toHaveBeenCalledTimes(2);
    // Verify delay was applied
    expect(timeoutCalls.length).toBeGreaterThanOrEqual(1);
    expect(timeoutCalls[0]?.ms).toBe(500);
  });

  it("retries 500 server error with delay", async () => {
    const config: RetryConfig = { maxRetries: 2, delayMs: 1000 };
    let attempts = 0;
    const fn = jest.fn(() => {
      attempts++;
      if (attempts <= 1) return Promise.reject(makeApiError(500));
      return Promise.resolve("recovered");
    });

    const retrier = createRetryWithFallback(config);
    const result = await retrier.execute(fn);

    expect(result).toBe("recovered");
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("retries timeout errors with delay", async () => {
    const config: RetryConfig = { maxRetries: 2, delayMs: 1000 };
    let attempts = 0;
    const fn = jest.fn(() => {
      attempts++;
      if (attempts <= 1) return Promise.reject(makeTimeoutError());
      return Promise.resolve("recovered");
    });

    const retrier = createRetryWithFallback(config);
    const result = await retrier.execute(fn);

    expect(result).toBe("recovered");
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("does NOT retry 400 bad request — throws immediately", async () => {
    const config: RetryConfig = { maxRetries: 3, delayMs: 1000 };
    const fn = jest.fn(() => Promise.reject(makeApiError(400)));

    const retrier = createRetryWithFallback(config);
    await expect(retrier.execute(fn)).rejects.toThrow("HTTP 400");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("does NOT retry 401 unauthorized — throws immediately", async () => {
    const config: RetryConfig = { maxRetries: 3, delayMs: 1000 };
    const fn = jest.fn(() => Promise.reject(makeApiError(401)));

    const retrier = createRetryWithFallback(config);
    await expect(retrier.execute(fn)).rejects.toThrow("HTTP 401");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("does NOT retry 403 forbidden — throws immediately", async () => {
    const config: RetryConfig = { maxRetries: 3, delayMs: 1000 };
    const fn = jest.fn(() => Promise.reject(makeApiError(403)));

    const retrier = createRetryWithFallback(config);
    await expect(retrier.execute(fn)).rejects.toThrow("HTTP 403");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("exhausts all retries then throws the last error", async () => {
    const config: RetryConfig = { maxRetries: 2, delayMs: 100 };
    const fn = jest.fn(() => Promise.reject(makeApiError(503)));

    const retrier = createRetryWithFallback(config);
    await expect(retrier.execute(fn)).rejects.toThrow("HTTP 503");
    // 1 initial + 2 retries = 3 total
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it("respects maxRetries=0 — no retries at all", async () => {
    const config: RetryConfig = { maxRetries: 0, delayMs: 1000 };
    const fn = jest.fn(() => Promise.reject(makeApiError(429)));

    const retrier = createRetryWithFallback(config);
    await expect(retrier.execute(fn)).rejects.toThrow("HTTP 429");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("uses default config values when partially specified", async () => {
    // maxRetries defaults to 1, delayMs defaults to 2000
    const config: RetryConfig = {};
    let attempts = 0;
    const fn = jest.fn(() => {
      attempts++;
      if (attempts <= 1) return Promise.reject(makeApiError(429));
      return Promise.resolve("recovered");
    });

    const retrier = createRetryWithFallback(config);
    const result = await retrier.execute(fn);

    expect(result).toBe("recovered");
    expect(fn).toHaveBeenCalledTimes(2);
    expect(timeoutCalls[0]?.ms).toBe(2000);
  });

  describe("with fallback models", () => {
    it("tries fallback model after exhausting retries on primary", async () => {
      const config: RetryConfig = {
        maxRetries: 1,
        delayMs: 100,
        fallbackModel: "openai/gpt-5.3-codex",
      };

      const primaryFn = jest.fn(() => Promise.reject(makeApiError(429)));
      const fallbackFn = jest.fn(() => Promise.resolve("fallback-result"));

      const retrier = createRetryWithFallback(config);
      const result = await retrier.executeWithFallback(primaryFn, fallbackFn);

      expect(result).toBe("fallback-result");
      // Primary: 1 initial + 1 retry = 2 calls
      expect(primaryFn).toHaveBeenCalledTimes(2);
      // Fallback: called once
      expect(fallbackFn).toHaveBeenCalledTimes(1);
    });

    it("does not call fallback if primary succeeds", async () => {
      const config: RetryConfig = {
        maxRetries: 1,
        delayMs: 100,
        fallbackModel: "openai/gpt-5.3-codex",
      };

      const primaryFn = jest.fn(() => Promise.resolve("primary-result"));
      const fallbackFn = jest.fn(() => Promise.resolve("fallback-result"));

      const retrier = createRetryWithFallback(config);
      const result = await retrier.executeWithFallback(primaryFn, fallbackFn);

      expect(result).toBe("primary-result");
      expect(primaryFn).toHaveBeenCalledTimes(1);
      expect(fallbackFn).toHaveBeenCalledTimes(0);
    });

    it("does not call fallback if primary succeeds on retry", async () => {
      const config: RetryConfig = {
        maxRetries: 2,
        delayMs: 100,
        fallbackModel: "openai/gpt-5.3-codex",
      };

      let attempts = 0;
      const primaryFn = jest.fn(() => {
        attempts++;
        if (attempts <= 1) return Promise.reject(makeApiError(503));
        return Promise.resolve("retry-success");
      });
      const fallbackFn = jest.fn(() => Promise.resolve("fallback-result"));

      const retrier = createRetryWithFallback(config);
      const result = await retrier.executeWithFallback(primaryFn, fallbackFn);

      expect(result).toBe("retry-success");
      expect(primaryFn).toHaveBeenCalledTimes(2);
      expect(fallbackFn).toHaveBeenCalledTimes(0);
    });

    it("throws if both primary retries and fallback fail", async () => {
      const config: RetryConfig = {
        maxRetries: 1,
        delayMs: 100,
        fallbackModel: "openai/gpt-5.3-codex",
      };

      const primaryFn = jest.fn(() => Promise.reject(makeApiError(500)));
      const fallbackFn = jest.fn(() => Promise.reject(makeApiError(500, "Fallback also failed")));

      const retrier = createRetryWithFallback(config);
      await expect(retrier.executeWithFallback(primaryFn, fallbackFn)).rejects.toThrow(
        "Fallback also failed"
      );
    });

    it("does NOT try fallback for non-transient errors", async () => {
      const config: RetryConfig = {
        maxRetries: 1,
        delayMs: 100,
        fallbackModel: "openai/gpt-5.3-codex",
      };

      const primaryFn = jest.fn(() => Promise.reject(makeApiError(400)));
      const fallbackFn = jest.fn(() => Promise.resolve("fallback-result"));

      const retrier = createRetryWithFallback(config);
      await expect(retrier.executeWithFallback(primaryFn, fallbackFn)).rejects.toThrow("HTTP 400");
      expect(primaryFn).toHaveBeenCalledTimes(1);
      expect(fallbackFn).toHaveBeenCalledTimes(0);
    });

    it("executeWithFallback works without a fallbackFn — behaves like execute", async () => {
      const config: RetryConfig = { maxRetries: 1, delayMs: 100 };
      const fn = jest.fn(() => Promise.reject(makeApiError(429)));

      const retrier = createRetryWithFallback(config);
      await expect(retrier.executeWithFallback(fn)).rejects.toThrow("HTTP 429");
      // 1 initial + 1 retry = 2
      expect(fn).toHaveBeenCalledTimes(2);
    });
  });

  describe("retry delay behavior", () => {
    it("applies configured delay between retries", async () => {
      const config: RetryConfig = { maxRetries: 3, delayMs: 750 };
      let attempts = 0;
      const fn = jest.fn(() => {
        attempts++;
        if (attempts < 4) return Promise.reject(makeApiError(429));
        return Promise.resolve("ok");
      });

      const retrier = createRetryWithFallback(config);
      await retrier.execute(fn);

      // All retry delays should be 750ms
      for (const call of timeoutCalls) {
        expect(call.ms).toBe(750);
      }
    });
  });

  describe("retryCount tracking", () => {
    it("reports retry count via getLastRetryCount after success", async () => {
      const config: RetryConfig = { maxRetries: 3, delayMs: 100 };
      let attempts = 0;
      const fn = jest.fn(() => {
        attempts++;
        if (attempts <= 2) return Promise.reject(makeApiError(429));
        return Promise.resolve("ok");
      });

      const retrier = createRetryWithFallback(config);
      await retrier.execute(fn);
      expect(retrier.getLastRetryCount()).toBe(2);
    });

    it("reports retry count via getLastRetryCount after failure", async () => {
      const config: RetryConfig = { maxRetries: 2, delayMs: 100 };
      const fn = jest.fn(() => Promise.reject(makeApiError(503)));

      const retrier = createRetryWithFallback(config);
      try {
        await retrier.execute(fn);
      } catch {
        // expected
      }
      expect(retrier.getLastRetryCount()).toBe(2);
    });
  });
});

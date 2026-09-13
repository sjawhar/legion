import { describe, expect, it, spyOn } from "bun:test";
import type { CommandRunner } from "../../state/fetch";
import {
  type BootProbeOptions,
  IMAGE_PROBE_RETRY,
  verifyLegionPluginLoaded,
  verifyOmpAgentsCapability,
} from "../boot-probes";

/** A runner standing in for one the daemon abandoned mid-attempt. `defaultRunner` sets `aborted`
 * only from the abort listener on the very signal it was handed, so `aborted: true` on a result
 * implies that signal has fired: the stand-in aborts the controller before returning, exactly
 * as the daemon's teardown would have while the attempt was still running. */
function abortedRunner(attempts: { count: number }, controller: AbortController): CommandRunner {
  return async () => {
    attempts.count += 1;
    controller.abort();
    return { stdout: "", stderr: "", exitCode: 143, aborted: true };
  };
}

/** The bounded policy with a no-op sleep: an aborted attempt misclassified as transient fails in
 * milliseconds with `never completed within its retry budget` instead of looping forever. */
function options(signal: AbortSignal): BootProbeOptions {
  return {
    sleep: async () => {},
    timeoutMs: 300_000,
    retry: IMAGE_PROBE_RETRY,
    signal,
  };
}

describe("boot probes on an attempt the runner aborted", () => {
  it("the pi.agents probe ends without a transient log, a retry, or a false negative diagnosis", async () => {
    const attempts = { count: 0 };
    const controller = new AbortController();
    const logged: string[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      logged.push(args.map(String).join(" "));
    });
    try {
      const probe = verifyOmpAgentsCapability(
        "omp",
        [],
        abortedRunner(attempts, controller),
        options(controller.signal)
      );
      await expect(probe).rejects.toThrow(/probe abandoned/);
      await expect(probe).rejects.not.toThrow(/does not expose pi\.agents/);
      expect(attempts.count).toBe(1);
      expect(logged).toEqual([]);
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("the plugin-load probe ends without a transient log, a retry, or a launch-failure diagnosis", async () => {
    const attempts = { count: 0 };
    const controller = new AbortController();
    const logged: string[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      logged.push(args.map(String).join(" "));
    });
    try {
      const probe = verifyLegionPluginLoaded(
        "omp",
        [],
        abortedRunner(attempts, controller),
        async () => JSON.stringify({ version: "1.0.0" }),
        options(controller.signal)
      );
      await expect(probe).rejects.toThrow(/probe abandoned/);
      await expect(probe).rejects.not.toThrow(/OMP launch probe failed/);
      expect(attempts.count).toBe(1);
      expect(logged).toEqual([]);
    } finally {
      errorSpy.mockRestore();
    }
  });
});

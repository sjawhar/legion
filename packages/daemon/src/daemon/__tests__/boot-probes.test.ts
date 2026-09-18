import { describe, expect, it, spyOn } from "bun:test";
import type { CommandResult, CommandRunner } from "../../state/fetch";
import {
  type BootProbeOptions,
  findReferencedPromptDependencies,
  IMAGE_PROBE_RETRY,
  verifyLegionPluginLoaded,
  verifyLegionPromptDependencies,
  verifyOmpAgentsCapability,
  verifySessionStorageSetting,
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

describe("the session-storage setting probe", () => {
  /** A runner answering every attempt the same way, recording the commands it was handed. */
  function fixedRunner(result: CommandResult, commands: string[][]): CommandRunner {
    return async (command) => {
      commands.push(command);
      return result;
    };
  }
  const bounded = (sleeps: number[]): BootProbeOptions => ({
    sleep: async (ms) => {
      sleeps.push(ms);
    },
    timeoutMs: 300_000,
    retry: IMAGE_PROBE_RETRY,
  });

  it("passes when the build refuses the nonsense value, naming the variable on stderr", async () => {
    const commands: string[][] = [];
    const sleeps: number[] = [];
    await verifySessionStorageSetting(
      "/opt/omp/bin/omp",
      ["secrets", "ANTHROPIC_API_KEY", "--"],
      fixedRunner(
        {
          stdout: "",
          stderr: 'Error: OMP_SESSION_STORAGE is "legion-launch-probe"; expected "file" or "sql"\n',
          exitCode: 1,
        },
        commands
      ),
      bounded(sleeps)
    );
    expect(sleeps).toEqual([]);
    expect(commands).toHaveLength(1);
    // One launch through the configured prefix, the variable exported to it, on the exit-0 path
    // an older build takes (`PI_TIMING=x`, interactive with stdin closed, no session on disk).
    expect(commands[0]).toEqual([
      "sh",
      "-c",
      "export OMP_SESSION_STORAGE=legion-launch-probe PI_TIMING=x; exec secrets ANTHROPIC_API_KEY -- /opt/omp/bin/omp --no-session --no-extensions --no-skills --no-rules --no-lsp --no-tools </dev/null >/dev/null",
    ]);
  });

  it("fails definitively, without a retry, when the build starts on the nonsense value: it predates the setting", async () => {
    const commands: string[][] = [];
    const sleeps: number[] = [];
    const probe = verifySessionStorageSetting(
      "/opt/omp/bin/omp",
      [],
      fixedRunner(
        { stdout: "", stderr: "Total: 4925.1ms (since first marker)\n", exitCode: 0 },
        commands
      ),
      bounded(sleeps)
    );
    await expect(probe).rejects.toThrow(
      '[legion] OMP launch command "/opt/omp/bin/omp" started with OMP_SESSION_STORAGE=legion-launch-probe (exit 0): this build predates the session.storage setting and would silently keep sessions on files under a sql session store'
    );
    expect(commands).toHaveLength(1);
    expect(sleeps).toEqual([]);
  });

  it("retries a non-zero exit that does not name the variable and fails as exhausted under a bounded policy", async () => {
    const commands: string[][] = [];
    const sleeps: number[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const probe = verifySessionStorageSetting(
        "/opt/omp/bin/omp",
        [],
        fixedRunner(
          { stdout: "", stderr: "secrets: ANTHROPIC_API_KEY denied\n", exitCode: 1 },
          commands
        ),
        bounded(sleeps)
      );
      await expect(probe).rejects.toThrow(
        '[legion] OMP session storage setting probe never completed within its retry budget (6 attempts) for launch command "/opt/omp/bin/omp": secrets: ANTHROPIC_API_KEY denied'
      );
    } finally {
      errorSpy.mockRestore();
    }
    expect(commands).toHaveLength(6);
    expect(sleeps).toEqual([10_000, 20_000, 40_000, 80_000, 160_000]);
  });

  it("treats a runner kill at the budget as transient: the exit code that would answer never came", async () => {
    const commands: string[][] = [];
    const sleeps: number[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const probe = verifySessionStorageSetting(
        "/opt/omp/bin/omp",
        [],
        fixedRunner(
          {
            stdout: "",
            stderr: "",
            exitCode: 143,
            timedOut: { limitMs: 300_000, elapsedMs: 300_100 },
          },
          commands
        ),
        bounded(sleeps)
      );
      await expect(probe).rejects.toThrow(
        /never completed within its retry budget \(6 attempts\).*command timed out after 300 s/
      );
    } finally {
      errorSpy.mockRestore();
    }
    expect(commands).toHaveLength(6);
  });
});

describe("the Legion prompt agent resolver", () => {
  const bounded = (sleeps: number[]): BootProbeOptions => ({
    sleep: async (ms) => {
      sleeps.push(ms);
    },
    timeoutMs: 300_000,
    retry: IMAGE_PROBE_RETRY,
  });

  it("extracts every explicit agent and skill reference", () => {
    expect(
      findReferencedPromptDependencies([
        'task(agent="oracle") task(agent = "thermonuclear-deep-review")',
        'An explicit agent="not-a-task" reference must be checked too.',
        "task(agent='thermonuclear-code-quality') task(agent=\"oracle\")",
        "Read skill://legion-worker, then the `legion-architect` skill and [skills/dispatch](../dispatch/SKILL.md).",
      ])
    ).toEqual({
      agents: ["not-a-task", "oracle", "thermonuclear-code-quality", "thermonuclear-deep-review"],
      skills: ["dispatch", "legion-architect", "legion-worker"],
    });
  });

  it("refuses without retry when the launched OMP cannot resolve a prompt dependency", async () => {
    const commands: string[][] = [];
    const sleeps: number[] = [];

    const probe = verifyLegionPromptDependencies(
      "/opt/omp/bin/omp",
      ["secrets", "OPENAI_API_KEY", "--"],
      {
        agents: ["oracle", "thermonuclear-deep-review"],
        skills: ["legion-worker"],
      },
      async (command) => {
        commands.push(command);
        return {
          stdout: "",
          stderr:
            "LEGION_OMP_PROMPT_DEPENDENCIES=missing:agent:thermonuclear-deep-review,skill:legion-worker\n",
          exitCode: 0,
        };
      },
      bounded(sleeps)
    );

    await expect(probe).rejects.toThrow(
      "Launched OMP cannot resolve Legion prompt dependencies: agent:thermonuclear-deep-review, skill:legion-worker"
    );
    expect(sleeps).toEqual([]);
    expect(commands).toHaveLength(1);
    expect(commands[0]).toEqual([
      "sh",
      "-c",
      expect.stringContaining(
        'LEGION_PROMPT_DEPENDENCIES="$1" exec secrets OPENAI_API_KEY -- /opt/omp/bin/omp models --extension "$2" --json >/dev/null'
      ),
      "sh",
      JSON.stringify({
        agents: ["oracle", "thermonuclear-deep-review"],
        skills: ["legion-worker"],
      }),
      expect.stringContaining("@sjawhar/pi-legion-envoy/dist/prompt-dependencies-probe.js"),
    ]);
  });
});

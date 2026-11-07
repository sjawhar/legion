import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  runShellCommand,
  SHELL_OUTPUT_MAX,
  shellCommandBusyReport,
  shellCommandFrom,
  shellCommandMessage,
  shellCommandReport,
} from "./typed-shell";

/** A directory of this machine's own, so a command that prints its working directory proves the
 *  session's is the one it ran in. */
function scratch(): string {
  return mkdtempSync(join(tmpdir(), "typed-shell-test-"));
}

describe("the command a person's message says to run", () => {
  test("is the rest of a line whose first character is the mark, whatever it starts with", () => {
    expect(shellCommandFrom("!printf hi")).toBe("printf hi");
    expect(shellCommandFrom("!!printf hi")).toBe("!printf hi");
    expect(shellCommandFrom("!")).toBe("");
    expect(shellCommandFrom("!cd /tmp\nls")).toBe("cd /tmp\nls");
  });

  test("is nothing for a message that does not start with the mark", () => {
    expect(shellCommandFrom("printf hi")).toBeUndefined();
    expect(shellCommandFrom(" !printf hi")).toBeUndefined();
    expect(shellCommandFrom("Where is the dashboard?")).toBeUndefined();
    expect(shellCommandFrom("")).toBeUndefined();
  });
});

describe("running a person's command", () => {
  test("answers with what it printed, and names no status for one that exited 0", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("printf hi", { cwd: directory, limitMs: 10_000 });
      expect(result).toEqual({ command: "printf hi", cut: false, output: "hi", status: undefined });
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("runs in the directory it was given", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("pwd -P", { cwd: directory, limitMs: 10_000 });
      expect(result.output).toBe(directory);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("runs with the session's own environment", async () => {
    const directory = scratch();
    process.env.TYPED_SHELL_TEST_VARIABLE = "from the session";
    try {
      const result = await runShellCommand('printf %s "$TYPED_SHELL_TEST_VARIABLE"', {
        cwd: directory,
        limitMs: 10_000,
      });
      expect(result.output).toBe("from the session");
    } finally {
      delete process.env.TYPED_SHELL_TEST_VARIABLE;
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("names the exit status of a command that failed", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("exit 3", { cwd: directory, limitMs: 10_000 });
      expect(result.status).toBe("The command ended with exit status 3.");
      expect(result.output).toBe("");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("keeps what the shell itself printed on its error stream", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("!printf hi", { cwd: directory, limitMs: 10_000 });
      expect(result.output).toContain("!printf");
      expect(result.status).toBe("The command ended with exit status 127.");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("stops a command past its limit and keeps what it printed", async () => {
    const directory = scratch();
    const started = Date.now();
    try {
      const result = await runShellCommand("printf early; sleep 600", {
        cwd: directory,
        limitMs: 1_000,
      });
      expect(result.output).toBe("early");
      expect(result.status).toBe("The command was stopped after 1000 ms.");
      expect(Date.now() - started).toBeLessThan(10_000);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 20_000);

  test("cuts output past the bound and says so in each text", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("printf 'a%.0s' $(seq 1 20000)", {
        cwd: directory,
        limitMs: 20_000,
      });
      expect(result.cut).toBe(true);
      expect(result.output).toHaveLength(SHELL_OUTPUT_MAX);
      expect(shellCommandReport(result)).toContain(
        `The output was cut at ${SHELL_OUTPUT_MAX} characters.`
      );
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 30_000);
});

describe("the texts a command becomes", () => {
  const ran = {
    command: "printf hi",
    cut: false,
    output: "hi",
    status: undefined,
  };

  test("carry the person's line and what the command printed", () => {
    expect(shellCommandReport(ran)).toBe("!printf hi\n\nhi");
  });

  test("say so for a command that printed nothing, so every message has an answer", () => {
    expect(shellCommandReport({ ...ran, command: "true", output: "" })).toBe(
      "!true\n\nThe command printed nothing."
    );
  });

  test("end with the status of a command that did not exit 0", () => {
    expect(
      shellCommandReport({
        ...ran,
        command: "exit 3",
        output: "",
        status: "The command ended with exit status 3.",
      })
    ).toBe("!exit 3\n\nThe command printed nothing.\n\nThe command ended with exit status 3.");
  });

  test("tell the agent whose command it was before the command itself", () => {
    expect(shellCommandMessage(shellCommandReport(ran))).toEndWith("\n\n!printf hi\n\nhi");
    expect(shellCommandMessage(shellCommandReport(ran))).toStartWith("A person ran this shell");
  });

  test("name the command still running when one already runs", () => {
    expect(shellCommandBusyReport("printf hi", "sleep 600")).toBe(
      "!printf hi\n\nThe command was not run: this session is still running !sleep 600."
    );
  });
});

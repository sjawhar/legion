import { describe, expect, test } from "bun:test";
import { dispatchToolSpecs } from "@legion/contracts";
import {
  commandFlags,
  commandLine,
  commandName,
  flagName,
  parseCommand,
  toolForCommand,
} from "../dispatch-command";

const noFiles = {
  readText: (path: string): string => {
    throw new Error(`unexpected read ${path}`);
  },
};

function shellWords(line: string): string[] {
  // Splits the POSIX single-quoted output of commandLine back into argv.
  const words: string[] = [];
  let current = "";
  let quoted = false;
  let inWord = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quoted) {
      if (c === "'") quoted = false;
      else current += c;
    } else if (c === "'") {
      quoted = true;
      inWord = true;
    } else if (c === "\\" && line[i + 1] === "'") {
      current += "'";
      i++;
    } else if (c === " ") {
      if (inWord) words.push(current);
      current = "";
      inWord = false;
    } else {
      current += c;
      inWord = true;
    }
  }
  if (inWord) words.push(current);
  return words;
}

describe("the dispatch command surface", () => {
  test("every spec's example survives commandLine then parseCommand unchanged", () => {
    for (const spec of dispatchToolSpecs) {
      const argv = shellWords(commandLine(spec.name, spec.example as Record<string, unknown>));
      expect(argv[0]).toBe("dispatch");
      const parsed = parseCommand(argv.slice(1), noFiles);
      expect(parsed).toEqual({ kind: "call", tool: spec.name, args: spec.example, dryRun: false });
    }
  });

  test("names map both ways for all 25 tools, and no two flags of one command collide", () => {
    expect(dispatchToolSpecs).toHaveLength(25);
    for (const spec of dispatchToolSpecs) {
      expect(toolForCommand(commandName(spec.name))).toBe(spec.name);
      const flags = commandFlags(spec.name);
      expect(new Set(flags).size).toBe(flags.length);
    }
    expect(commandName("dispatch_issue_update")).toBe("issue-update");
    expect(flagName("reply_to_ask")).toBe("reply-to-ask");
  });

  test("dispatch project takes its key and name as one flag each", () => {
    expect(commandName("dispatch_project")).toBe("project");
    expect(commandFlags("dispatch_project")).toEqual(expect.arrayContaining(["--key", "--name"]));
    expect(parseCommand(["project", "--key", "SMOKE684", "--name", "Smoke"], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_project",
      args: { key: "SMOKE684", name: "Smoke" },
      dryRun: false,
    });
  });

  test("clearing, booleans, repeats, options and stdin", () => {
    const stdin = { readText: (path: string) => (path === "-" ? "line one\nline two\n" : "") };
    expect(
      parseCommand(["issue-update", "--issue", "LEGION-2", "--clear-priority"], noFiles)
    ).toMatchObject({ kind: "call", args: { issue: "LEGION-2", priority: null } });
    expect(parseCommand(["claim", "--issue", "LEGION-2", "--release"], noFiles)).toMatchObject({
      args: { issue: "LEGION-2", release: true },
    });
    expect(
      parseCommand(
        ["issue", "--project", "LEGION", "--title", "T", "--label", "a", "--label", "b"],
        noFiles
      )
    ).toMatchObject({ args: { labels: ["a", "b"] } });
    expect(
      parseCommand(
        [
          "ask",
          "--issue",
          "LEGION-2",
          "--question",
          "Q?",
          "--option",
          "Ship: costs a day",
          "--option",
          "Wait",
        ],
        noFiles
      )
    ).toMatchObject({
      args: { options: [{ label: "Ship", description: "costs a day" }, { label: "Wait" }] },
    });
    expect(
      parseCommand(["message", "--issue", "LEGION-2", "--body-file", "-"], stdin)
    ).toMatchObject({ args: { body: "line one\nline two\n" } });
    expect(
      parseCommand(
        ["issues", "--project", "LEGION", "--priority", "0", "--priority", "none"],
        noFiles
      )
    ).toMatchObject({ args: { priority: [0, null] } });
  });

  test("a single-value flag given twice is named twice whether or not its first value parsed", () => {
    expect(
      parseCommand(
        ["issue-update", "--issue", "LEGION-2", "--priority", "bad", "--priority", "3"],
        noFiles
      )
    ).toEqual({
      kind: "refused",
      tool: "dispatch_issue_update",
      problems: ['--priority must be a number, not "bad"', "--priority is given twice"],
    });
    expect(
      parseCommand(
        ["issue-update", "--issue", "LEGION-2", "--priority", "1", "--priority", "2"],
        noFiles
      )
    ).toEqual({
      kind: "refused",
      tool: "dispatch_issue_update",
      problems: ["--priority is given twice"],
    });
  });

  test("an option label holding the separator survives commandLine then parseCommand", () => {
    const args = {
      issue: "LEGION-2",
      question: "Q?",
      options: [
        { label: "b: with colon", description: "x" },
        { label: "Plain", description: "costs: a day" },
        { label: "c: no description" },
      ],
    };
    const argv = shellWords(commandLine("dispatch_ask", args));
    expect(parseCommand(argv.slice(1), noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_ask",
      args,
      dryRun: false,
    });
    // Every label free of the separator keeps the readable flag.
    expect(
      commandLine("dispatch_ask", {
        issue: "LEGION-2",
        question: "Q?",
        options: [{ label: "Ship", description: "a: b" }],
      })
    ).toContain("--option 'Ship: a: b'");
  });

  test("an unknown flag and a missing value are refused together, naming the flags", () => {
    const parsed = parseCommand(["message", "--isue", "LEGION-2", "--body"], noFiles);
    expect(parsed.kind).toBe("refused");
    if (parsed.kind !== "refused") return;
    expect(parsed.problems.join("\n")).toContain("--isue");
    expect(parsed.problems.join("\n")).toContain("--body needs a value");
  });

  test("an unknown command is refused with the list of commands", () => {
    expect(parseCommand(["isue"], noFiles)).toMatchObject({ kind: "refused" });
  });

  test("a bare issue key first is the --issue of a command that takes --issue", () => {
    expect(parseCommand(["read", "ACME-12"], noFiles)).toEqual(
      parseCommand(["read", "--issue", "ACME-12"], noFiles)
    );
    expect(parseCommand(["read", "ACME-12"], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_read",
      args: { issue: "ACME-12" },
      dryRun: false,
    });
    expect(parseCommand(["message", "ACME-12", "--body", "hi"], noFiles)).toMatchObject({
      kind: "call",
      args: { issue: "ACME-12", body: "hi" },
    });
  });

  test("every command that takes --issue takes a bare key as its first argument", () => {
    const commands = dispatchToolSpecs
      .map((spec) => spec.name)
      .filter((tool) => commandFlags(tool).includes("--issue"))
      .map(commandName);
    expect(commands.sort()).toEqual(
      [
        "artifact",
        "ask",
        "claim",
        "comment",
        "doc-edit",
        "doc-read",
        "handoff-read",
        "handoff-write",
        "issue-update",
        "message",
        "read",
        "request-approval",
        "suggest",
      ].sort()
    );
    for (const command of commands) {
      const bare = parseCommand([command, "ACME-12", "--dry-run"], noFiles);
      expect(bare).toEqual(parseCommand([command, "--issue", "ACME-12", "--dry-run"], noFiles));
      expect(JSON.stringify(bare)).not.toContain("unexpected argument");
    }
  });

  test("a key-shaped word after the first argument is refused, not taken as --issue", () => {
    expect(parseCommand(["message", "--body", "hi", "SHA-256"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_message",
      problems: [
        "unexpected argument SHA-256: a bare issue key is the --issue only as the first argument; otherwise write --issue SHA-256",
      ],
    });
  });

  test("a lowercase issue key is refused", () => {
    expect(parseCommand(["read", "acme-12"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["unexpected argument acme-12: every value follows its flag"],
    });
  });

  test("a bare issue key is refused by a command without --issue", () => {
    expect(parseCommand(["search", "--query", "q", "ACME-12"], noFiles)).toMatchObject({
      kind: "refused",
      tool: "dispatch_search",
    });
  });

  test("a bare issue key is refused when --issue is also given", () => {
    expect(parseCommand(["read", "--issue", "ACME-1", "ACME-12"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["unexpected argument ACME-12: --issue is already ACME-1"],
    });
    const issueFile = {
      readText: (path: string): string => {
        if (path === "issue.txt") return "ACME-1";
        throw new Error(`unexpected read ${path}`);
      },
    };
    expect(parseCommand(["read", "--issue-file", "issue.txt", "ACME-12"], issueFile)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["unexpected argument ACME-12: --issue-file already set --issue to ACME-1"],
    });
    expect(parseCommand(["read", "ACME-12", "--issue", "ACME-1"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["the issue key ACME-12 and --issue both set issue"],
    });
  });

  test("a second bare issue key is refused", () => {
    expect(parseCommand(["read", "ACME-12", "ACME-13"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: [
        "unexpected argument ACME-13: a bare issue key is the --issue only as the first argument; otherwise write --issue ACME-13",
      ],
    });
  });

  test("a bare value that is not an issue key is refused", () => {
    expect(parseCommand(["read", "hello"], noFiles)).toMatchObject({
      problems: ["unexpected argument hello: every value follows its flag"],
    });
  });

  test("handoff-write reads its body from a file or stdin unchanged, and handoff-read takes a version", () => {
    const body = '{"schemaVersion":1,\n  "phase": "implement"}\n';
    const files = { readText: (path: string) => (path === "-" || path === "h.json" ? body : "") };
    for (const source of ["h.json", "-"]) {
      expect(
        parseCommand(
          ["handoff-write", "DSP-1", "--phase", "implement", "--body-file", source],
          files
        )
      ).toEqual({
        kind: "call",
        tool: "dispatch_handoff_write",
        args: { issue: "DSP-1", phase: "implement", body },
        dryRun: false,
      });
    }
    expect(
      parseCommand(["handoff-read", "DSP-1", "--phase", "plan", "--version", "2"], noFiles)
    ).toEqual({
      kind: "call",
      tool: "dispatch_handoff_read",
      args: { issue: "DSP-1", phase: "plan", version: 2 },
      dryRun: false,
    });
  });
});

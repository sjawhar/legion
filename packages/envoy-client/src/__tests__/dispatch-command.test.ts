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

  test("names map both ways for all 21 tools, and no two flags of one command collide", () => {
    expect(dispatchToolSpecs).toHaveLength(21);
    for (const spec of dispatchToolSpecs) {
      expect(toolForCommand(commandName(spec.name))).toBe(spec.name);
      const flags = commandFlags(spec.name);
      expect(new Set(flags).size).toBe(flags.length);
    }
    expect(commandName("dispatch_issue_update")).toBe("issue-update");
    expect(flagName("reply_to_ask")).toBe("reply-to-ask");
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
});

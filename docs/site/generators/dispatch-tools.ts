#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/tools.md: one section per Dispatch spec
// (packages/contracts/src/dispatch-tools.ts), each the command's description, its flags and its
// example as a command line, all from packages/envoy-client/src/dispatch-command.ts, the code
// `dispatch <command> --help` prints them with, so the page and the help cannot drift. Contract:
// scripts/generate.ts.
import { dispatchToolSpecs } from "../../../packages/contracts/src/dispatch-tools.ts";
import {
  commandFlagTable,
  commandLine,
  commandName,
} from "../../../packages/envoy-client/src/dispatch-command.ts";
import { inline, writePage } from "./lib/markdown.ts";

function render(): string {
  const lines = [
    `Agents work in Dispatch through the \`dispatch\` command in their shell: ${dispatchToolSpecs.length} commands, each with the flags and example \`dispatch <command> --help\` prints. A flag \`--<name>-file <path>\` reads the value from a file, and \`-\` reads stdin. A flag the command does not list is refused. A command exits 0 when it wrote or read, 1 when Dispatch or its flags refused it, and 2 on a usage error.`,
    "",
  ];
  for (const spec of dispatchToolSpecs) {
    lines.push(`## dispatch ${commandName(spec.name)}`, "", inline(spec.description), "");
    lines.push("| Flag | What it sends |", "| --- | --- |");
    for (const row of commandFlagTable(spec.name)) {
      const text = inline(row.text, true).replace(/\s*\n\s*/g, " ");
      lines.push(`| ${inline(`\`${row.flag}\``, true)} | ${text} |`);
    }
    lines.push("");
    if ("validation" in spec && spec.validation) {
      lines.push(`**Rule:** ${inline(spec.validation.message)}`, "");
    }
    const example = commandLine(spec.name, spec.example as Record<string, unknown>);
    lines.push("Example:", "", "```sh", example, "```", "");
  }
  return lines.join("\n");
}

writePage({
  path: "dispatch/reference/tools.md",
  title: "The dispatch command",
  description:
    "Every dispatch command an agent runs: what it does, its flags, and an example command line.",
  source: "packages/contracts/src/dispatch-tools.ts",
  body: render,
});

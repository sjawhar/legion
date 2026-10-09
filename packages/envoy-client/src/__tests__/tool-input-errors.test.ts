import { describe, expect, test } from "bun:test";
import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import { commandFlags, commandLine } from "../dispatch-command";
import { formatZodIssues, ToolInputError } from "../tool-input-errors";

function specFor(name: string) {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === name);
  if (!spec) throw new Error(`missing ${name}`);
  return spec;
}

function problemsFor(name: string, args: unknown): string[] {
  const schema: z.ZodType = dispatchToolSchema(specFor(name), zodSchemaApi(z), { strict: true });
  const parsed = schema.safeParse(args, { reportInput: true });
  if (parsed.success) throw new Error("expected the call to be refused");
  return formatZodIssues(parsed.error.issues, schema, name);
}

describe("ToolInputError", () => {
  test("counts the problems and lists each on its own line", () => {
    const error = new ToolInputError("dispatch_message", ["--body is required (string)"]);
    expect(error.message).toBe(
      [
        "dispatch message was not called: 1 problem",
        "- --body is required (string)",
        "- Allowed flags: --issue, --issue-file, --body, --body-file, --in-reply-to, --in-reply-to-file, --image, --clear-images, --help, --dry-run",
        "- Example: dispatch message --issue DSP-1 --body 'Implementation started.'",
      ].join("\n")
    );
    expect(error.problems).toEqual(["--body is required (string)"]);

    const two = new ToolInputError("dispatch_ask", ["a", "b"]);
    expect(two.message.split("\n").slice(0, 3)).toEqual([
      "dispatch ask was not called: 2 problems",
      "- a",
      "- b",
    ]);
  });

  test("an Envoy tool's refusal keeps its JSON wording, with no flags or example", () => {
    expect(new ToolInputError("envoy_send", ["message is required (string)"]).message).toBe(
      ["envoy_send was not called: 1 problem", "- message is required (string)"].join("\n")
    );
  });

  test("gives every Dispatch refusal the command's flags and an example that parses", () => {
    for (const spec of dispatchToolSpecs) {
      const schema = dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
      const error = new ToolInputError(spec.name, ["invalid input"]);

      expect(error.message, spec.name).toContain(
        `- Allowed flags: ${commandFlags(spec.name).join(", ")}`
      );
      expect(error.message, spec.name).toContain(
        `- Example: ${commandLine(spec.name, { ...spec.example })}`
      );
      expect(schema.safeParse(spec.example).success, spec.name).toBe(true);
    }
  });
});

describe("formatZodIssues", () => {
  test("names each field by the flag that sets it, and an element by its position", () => {
    expect(
      problemsFor("dispatch_ask", {
        issue: "DSP-42",
        question: "q",
        options: [{ label: "a" }, {}],
        urgency: "now",
      })
    ).toEqual([
      "--option[1] label is required (string)",
      '--urgency must be one of low|med|high|blocking; got "now"',
    ]);
    expect(
      problemsFor("dispatch_doc_edit", {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: 3 }],
      })
    ).toEqual(["--ops-json[0] find must be a string, not 3"]);
  });

  test("names a missing field, an unknown flag, and a bad enum value", () => {
    expect(
      problemsFor("dispatch_message", { issue: "DSP-42", message: "x", urgency: "no" })
    ).toEqual(["--body is required (string)", "unknown flag --message", "unknown flag --urgency"]);
    expect(problemsFor("dispatch_resolve_ask", { ask: "a", kind: "no", reason: "r" })).toEqual([
      '--kind must be one of retracted|resolved; got "no"',
    ]);
    expect(problemsFor("dispatch_request_approval", { issue: "DSP-42" })).toEqual([
      "--summary is required (string)",
    ]);
  });

  test("names the object shape an array element must have, per element", () => {
    expect(
      problemsFor("dispatch_ask", { issue: "DSP-42", question: "q", options: ["a", "b"] })
    ).toEqual([
      "--option[0] must be an object {label, description?}, not a string",
      "--option[1] must be an object {label, description?}, not a string",
    ]);
  });

  // The allowed keys of a nested object are its own, not the tool's: a model told its mistyped
  // `replace` could be `issue, project, artifact, ref, ops, precondition, summary` would retry
  // the same op.
  test("names the operation's own keys for an unknown key inside a document edit op", () => {
    expect(
      problemsFor("dispatch_doc_edit", {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "old", replace: "new" }],
      })
    ).toEqual([
      'unknown field "replace" in --ops-json[0]; allowed: op, find, with, occurrence, markdown, ' +
        "after, before, block, index, type, attributes",
    ]);
  });

  test("carries the number to trim for an over-limit string and the item count for an array", () => {
    expect(
      problemsFor("dispatch_ask", {
        issue: "DSP-42",
        question: "x".repeat(850),
        options: Array.from({ length: 9 }, (_, index) => ({ label: `o${index}` })),
      })
    ).toEqual([
      "--question is 50 characters over the 800-character limit (850/800)",
      "--option has 9 items; the limit is 8",
    ]);
  });

  test("passes a cross-field refine message through verbatim and falls back to flag: message", () => {
    expect(problemsFor("dispatch_artifact", { issue: "DSP-42", name: "n" })).toEqual([
      "Exactly one of path or content is required. Exactly one of issue and project is required; with project, artifact names the document.",
    ]);
    expect(problemsFor("dispatch_search", { query: "a" })).toEqual([
      "--query: Too small: expected string to have >=2 characters",
    ]);
    expect(problemsFor("dispatch_issue", { project: "P", title: "t", priority: 1.5 })).toEqual([
      "--priority must be an integer, not 1.5",
    ]);
    expect(problemsFor("dispatch_issue_update", { issue: "DSP-1", priority: 4 })).toEqual([
      "--priority Too big: expected number to be <=3",
    ]);
    expect(problemsFor("dispatch_issue_update", { issue: "DSP-1", priority: "P1" })).toEqual([
      "--priority must be a number, not a string",
    ]);
  });
});

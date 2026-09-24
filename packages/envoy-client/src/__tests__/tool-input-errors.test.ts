import { describe, expect, test } from "bun:test";
import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import { formatZodIssues, ToolInputError } from "../tool-input-errors";

function schemaFor(name: string): z.ZodType {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === name);
  if (!spec) throw new Error(`missing ${name}`);
  return dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
}

function problemsFor(name: string, args: unknown): string[] {
  const schema = schemaFor(name);
  const parsed = schema.safeParse(args, { reportInput: true });
  if (parsed.success) throw new Error("expected the call to be refused");
  return formatZodIssues(parsed.error.issues, schema);
}

describe("ToolInputError", () => {
  test("counts the problems and lists each on its own line", () => {
    const error = new ToolInputError("dispatch_message", ["body is required (string)"]);
    expect(error.message).toBe(
      [
        "dispatch_message was not called: 1 problem",
        "- body is required (string)",
        "- Allowed keys: issue, body, in_reply_to",
        '- Example: dispatch_message({"issue":"DSP-1","body":"Implementation started."})',
      ].join("\n")
    );
    expect(error.problems).toEqual(["body is required (string)"]);

    const two = new ToolInputError("dispatch_ask", ["a", "b"]);
    expect(two.message).toBe(
      [
        "dispatch_ask was not called: 2 problems",
        "- a",
        "- b",
        "- Allowed keys: issue, project, artifact, ref, question, options, multiple, urgency, anchor",
        '- Example: dispatch_ask({"issue":"DSP-1","question":"Ship this?"})',
      ].join("\n")
    );
  });
  test("gives every Dispatch validation failure its allowed keys and a valid call", () => {
    for (const spec of dispatchToolSpecs) {
      const shape = spec.arguments(zodSchemaApi(z));
      const example = spec.example;
      const schema = dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
      const error = new ToolInputError(spec.name, ["invalid input"]);

      expect(error.message, spec.name).toContain(
        `- Allowed keys: ${Object.keys(shape).join(", ") || "none"}`
      );
      expect(error.message, spec.name).toContain(
        `- Example: ${spec.name}(${JSON.stringify(example)})`
      );
      expect(schema.safeParse(example).success, spec.name).toBe(true);
    }
  });
});

describe("formatZodIssues", () => {
  test("names a missing field, an unknown field with the allowed keys, and a bad enum value", () => {
    expect(
      problemsFor("dispatch_message", { issue: "DSP-42", message: "x", urgency: "no" })
    ).toEqual([
      "body is required (string)",
      'unknown field "message"; allowed: issue, body, in_reply_to',
      'unknown field "urgency"; allowed: issue, body, in_reply_to',
    ]);
    expect(problemsFor("dispatch_resolve_ask", { ask: "a", kind: "no", reason: "r" })).toEqual([
      'kind must be one of retracted|resolved; got "no"',
    ]);
  });

  test("names the object shape an array element must have, per element", () => {
    expect(
      problemsFor("dispatch_ask", { issue: "DSP-42", question: "q", options: ["a", "b"] })
    ).toEqual([
      "options.0 must be an object {label, description?}, not a string",
      "options.1 must be an object {label, description?}, not a string",
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
      "question is 50 characters over the 800-character limit (850/800)",
      "options has 9 items; the limit is 8",
    ]);
  });

  test("passes a cross-field refine message through verbatim and falls back to path: message", () => {
    expect(problemsFor("dispatch_artifact", { issue: "DSP-42", name: "n" })).toEqual([
      "Exactly one of path or content is required. Exactly one of issue and project is required; with project, artifact names the document.",
    ]);
    expect(problemsFor("dispatch_search", { query: "a" })).toEqual([
      "query: Too small: expected string to have >=2 characters",
    ]);
    expect(problemsFor("dispatch_issue", { project: "P", title: "t", priority: 1.5 })).toEqual([
      "priority must be an integer, not 1.5",
    ]);
    expect(problemsFor("dispatch_issue_update", { issue: "DSP-1", priority: 4 })).toEqual([
      "priority Too big: expected number to be <=3",
    ]);
    expect(problemsFor("dispatch_issue_update", { issue: "DSP-1", priority: "P1" })).toEqual([
      "priority must be a number, not a string",
    ]);
  });
});

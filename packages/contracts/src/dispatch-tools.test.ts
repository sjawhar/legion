import { describe, expect, test } from "bun:test";
import { z } from "zod";
import { dispatchToolSpecs } from "./dispatch-tools";
import { zodSchemaApi } from "./tool-schema";

const schemaApi = zodSchemaApi(z);

const validCalls = {
  dispatch_issue: { project: "DSP", title: "Native workspace" },
  dispatch_ask: { issue: "DSP-1", question: "Ship this?" },
  dispatch_comment: { issue: "DSP-1", body: "Looks good." },
  dispatch_suggest: {
    issue: "DSP-1",
    artifact: "spec",
    quote: "old wording",
    replace_with: "new wording",
  },
  dispatch_message: { issue: "DSP-1", body: "Implementation started." },
  dispatch_doc_edit: {
    issue: "DSP-1",
    artifact: "spec",
    ops: [{ op: "replace", find: "old", with: "new" }],
  },
  dispatch_doc_read: { issue: "DSP-1" },
  dispatch_artifact: { issue: "DSP-1", name: "design.pdf", path: "design.pdf" },
  dispatch_read: { issue: "DSP-1" },
} as const;

function schemaFor(name: keyof typeof validCalls) {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === name);
  if (!spec) throw new Error(`missing ${name}`);
  return z.object(spec.arguments(schemaApi) as z.ZodRawShape);
}

describe("zodSchemaApi", () => {
  test("rejects fractional values when integers are required", () => {
    expect(schemaApi.number({ int: true }).safeParse(1.5).success).toBe(false);
  });

  test("rejects strings beyond their maximum length", () => {
    expect(schemaApi.string({ max: 3 }).safeParse("abcd").success).toBe(false);
  });

  test("rejects arrays beyond their maximum length", () => {
    expect(
      schemaApi
        .array(schemaApi.string(), { max: 8 })
        .safeParse(Array.from({ length: 9 }, () => "item")).success
    ).toBe(false);
  });
});

describe("dispatchToolSpecs", () => {
  test("builds every dispatch tool on real Zod and accepts its valid invocation", () => {
    expect(dispatchToolSpecs.map((spec) => spec.name)).toEqual([
      "dispatch_issue",
      "dispatch_ask",
      "dispatch_comment",
      "dispatch_suggest",
      "dispatch_message",
      "dispatch_doc_edit",
      "dispatch_doc_read",
      "dispatch_artifact",
      "dispatch_read",
    ]);

    for (const [name, args] of Object.entries(validCalls) as Array<
      [keyof typeof validCalls, (typeof validCalls)[keyof typeof validCalls]]
    >) {
      expect(schemaFor(name).safeParse(args).success, name).toBe(true);
    }
  });

  test("rejects an ask with more than eight options", () => {
    expect(
      schemaFor("dispatch_ask").safeParse({
        issue: "DSP-1",
        question: "Which option?",
        options: Array.from({ length: 9 }, (_, index) => ({ label: `Option ${index}` })),
      }).success
    ).toBe(false);
  });

  test("requires ask multiple to be boolean", () => {
    expect(
      schemaFor("dispatch_ask").safeParse({
        issue: "DSP-1",
        question: "Which option?",
        multiple: "yes",
      }).success
    ).toBe(false);
  });

  test("rejects a document edit with an unknown operation", () => {
    expect(
      schemaFor("dispatch_doc_edit").safeParse({
        issue: "DSP-1",
        artifact: "spec",
        ops: [{ op: "bogus" }],
      }).success
    ).toBe(false);
  });
});

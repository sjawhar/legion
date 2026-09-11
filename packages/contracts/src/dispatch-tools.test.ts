import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { z } from "zod";
import { dispatchToolSchema, dispatchToolSpecs, SPEC_SECTIONS } from "./dispatch-tools";
import { zodSchemaApi } from "./tool-schema";

const schemaApi = zodSchemaApi(z);
function dispatchSkillSpecSections() {
  const repoRoot = resolve(import.meta.dir, "../../..");
  const skill = readFileSync(resolve(repoRoot, "skills/dispatch/SKILL.md"), "utf8");
  const sectionStart = skill.indexOf("## Writing a spec\n");
  if (sectionStart === -1) throw new Error("Dispatch skill has no Writing a spec section");

  const sectionEnd = skill.indexOf("\n## ", sectionStart + 1);
  return skill
    .slice(sectionStart, sectionEnd === -1 ? undefined : sectionEnd)
    .split("\n")
    .flatMap((line) => {
      const match = line.match(/^\| \*\*(.+?)\*\* \|/u);
      return match === null ? [] : [match[1]];
    });
}

const validCalls = {
  dispatch_issue: { project: "DSP", title: "Native workspace" },
  dispatch_ask: { issue: "DSP-1", question: "Ship this?" },
  dispatch_edit_ask: {
    ask: "ask-1",
    question: "Ship the revised plan?",
    options: [{ label: "Ship", description: "Approve the revision." }],
    multiple: false,
    urgency: "high",
  },
  dispatch_resolve_ask: {
    ask: "ask-1",
    kind: "retracted",
    reason: "A newer question supersedes this one.",
  },
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
  dispatch_search: { query: "astrolabe" },
} as const;

function schemaFor(name: keyof typeof validCalls) {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === name);
  if (!spec) throw new Error(`missing ${name}`);
  return dispatchToolSchema(spec, schemaApi);
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
      "dispatch_edit_ask",
      "dispatch_resolve_ask",
      "dispatch_comment",
      "dispatch_suggest",
      "dispatch_message",
      "dispatch_doc_edit",
      "dispatch_doc_read",
      "dispatch_artifact",
      "dispatch_read",
      "dispatch_search",
    ]);

    for (const [name, args] of Object.entries(validCalls) as Array<
      [keyof typeof validCalls, (typeof validCalls)[keyof typeof validCalls]]
    >) {
      expect(schemaFor(name).safeParse(args).success, name).toBe(true);
    }
  });

  test("dispatch_search rejects a one-character query and a limit above 50", () => {
    const schema = schemaFor("dispatch_search");

    expect(schema.safeParse({ query: "a" }).success).toBe(false);
    expect(schema.safeParse({ query: "ok", limit: 51 }).success).toBe(false);
    expect(schema.safeParse({ query: "ok", limit: 50, project: "LEGION" }).success).toBe(true);
  });

  test("dispatch_issue preserves force", () => {
    const result = schemaFor("dispatch_issue").safeParse({
      project: "DSP",
      title: "Native workspace",
      force: true,
    });

    expect(result.success).toBe(true);
    if (!result.success) return;
    expect(result.data).toMatchObject({ force: true });
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

  test("requires an ask edit to include a patch field", () => {
    const schema = schemaFor("dispatch_edit_ask");

    expect(schema.safeParse({ ask: "ask-1" }).success).toBe(false);
    expect(schema.safeParse({ ask: "ask-1", question: "Ship the revised plan?" }).success).toBe(
      true
    );
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

  test("requires exactly one artifact upload source", () => {
    const schema = schemaFor("dispatch_artifact");
    const shared = { issue: "DSP-1", name: "spec.md" };

    expect(schema.safeParse({ ...shared, path: "spec.md" }).success).toBe(true);
    expect(schema.safeParse({ ...shared, content: "# Spec\n" }).success).toBe(true);
    expect(schema.safeParse(shared).success).toBe(false);
    expect(schema.safeParse({ ...shared, path: "spec.md", content: "# Spec\n" }).success).toBe(
      false
    );
  });

  test("does not advertise a primary artifact option", () => {
    const artifact = dispatchToolSpecs.find((spec) => spec.name === "dispatch_artifact");
    if (!artifact) throw new Error("missing dispatch_artifact");
    const argumentsSchema = artifact.arguments(schemaApi) as Record<string, unknown>;
    expect(argumentsSchema).not.toHaveProperty("primary");
  });

  test("rejects an ask resolution without a reason", () => {
    expect(
      schemaFor("dispatch_resolve_ask").safeParse({
        ask: "ask-1",
        kind: "resolved",
        reason: "",
      }).success
    ).toBe(false);
  });
  test("keeps shared spec guidance aligned with the Dispatch skill", () => {
    const skillSections = dispatchSkillSpecSections();
    expect(skillSections).toEqual([...SPEC_SECTIONS]);
    const sectionOrder = SPEC_SECTIONS.join(", ");
    const issue = dispatchToolSpecs.find((spec) => spec.name === "dispatch_issue");
    const documentEdit = dispatchToolSpecs.find((spec) => spec.name === "dispatch_doc_edit");
    if (!issue || !documentEdit) throw new Error("missing spec-writing tools");

    const issueArguments = issue.arguments(schemaApi) as unknown as {
      spec: z.ZodOptional<z.ZodString>;
    };
    expect(issueArguments.spec.unwrap().description).toContain(sectionOrder);
    expect(documentEdit.description).toContain(sectionOrder);
  });

  test("leaves Dispatch subscriptions to successful tool results", () => {
    expect(dispatchToolSpecs.every((spec) => !("subscribes" in spec))).toBe(true);
  });
});

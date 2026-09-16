import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { z } from "zod";
import {
  dispatchToolSchema,
  dispatchToolSpecs,
  ISSUE_STATUSES,
  SPEC_SECTIONS,
} from "./dispatch-tools";
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
  dispatch_issue_update: { issue: "DSP-1", status: "in_progress" },
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
  dispatch_resolve_comment: { comment: "dispatch://DSP-1/comment/comment-1" },
  dispatch_follow: { ask: "ask-1", action: "follow" },
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
  dispatch_request_approval: { issue: "DSP-1" },
  dispatch_artifact: { issue: "DSP-1", name: "design.pdf", path: "design.pdf" },
  dispatch_read: { issue: "DSP-1" },
  dispatch_search: { query: "astrolabe" },
  dispatch_open_asks: {},
  dispatch_whoami: {},
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

  test("names how far an over-limit string is over its cap", () => {
    const result = schemaFor("dispatch_ask").safeParse({
      issue: "DSP-1",
      question: "x".repeat(850),
    });
    expect(result.success).toBe(false);
    expect(result.error?.issues.map((issue) => issue.message)).toEqual([
      "is 50 characters over the 800-character limit (850/800)",
    ]);
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
      "dispatch_issue_update",
      "dispatch_ask",
      "dispatch_edit_ask",
      "dispatch_resolve_ask",
      "dispatch_resolve_comment",
      "dispatch_follow",
      "dispatch_comment",
      "dispatch_suggest",
      "dispatch_message",
      "dispatch_doc_edit",
      "dispatch_doc_read",
      "dispatch_request_approval",
      "dispatch_artifact",
      "dispatch_read",
      "dispatch_search",
      "dispatch_open_asks",
      "dispatch_whoami",
    ]);

    for (const [name, args] of Object.entries(validCalls) as Array<
      [keyof typeof validCalls, (typeof validCalls)[keyof typeof validCalls]]
    >) {
      expect(schemaFor(name).safeParse(args).success, name).toBe(true);
    }
  });

  test("explains that quote anchors stay pinned to their block", () => {
    for (const name of ["dispatch_ask", "dispatch_comment"] as const) {
      const tool = dispatchToolSpecs.find((candidate) => candidate.name === name);
      expect(tool?.description).toContain("quote anchor is pinned to its block");
    }
  });

  test("explains that an ask ref is appended as a rendered link", () => {
    const tool = dispatchToolSpecs.find((candidate) => candidate.name === "dispatch_ask");
    if (tool === undefined) throw new Error("dispatch_ask spec is missing");
    const argumentsSchema = tool.arguments(schemaApi) as unknown as {
      ref: z.ZodOptional<z.ZodString>;
    };
    expect(argumentsSchema.ref.unwrap().description).toBe(
      "Optional dispatch:// reference (issue, document, message, or ask); appended to the question and rendered as a link."
    );
  });

  test("dispatch_search rejects a one-character query and a limit above 50", () => {
    const schema = schemaFor("dispatch_search");

    expect(schema.safeParse({ query: "a" }).success).toBe(false);
    expect(schema.safeParse({ query: "ok", limit: 51 }).success).toBe(false);
    expect(schema.safeParse({ query: "ok", limit: 50, project: "LEGION" }).success).toBe(true);
  });

  test("dispatch_open_asks accepts no arguments and rejects selectors", () => {
    const schema = schemaFor("dispatch_open_asks");

    expect(schema.safeParse({}).success).toBe(true);
    expect(schema.safeParse({ session_id: "another-session" }).success).toBe(false);
  });

  test("dispatch_whoami accepts no arguments and rejects any key", () => {
    const schema = schemaFor("dispatch_whoami");

    expect(schema.safeParse({}).success).toBe(true);
    expect(schema.safeParse({ session_id: "another-session" }).success).toBe(false);
  });

  test("dispatch_issue accepts an assignee login", () => {
    const result = schemaFor("dispatch_issue").safeParse({
      project: "DSP",
      title: "Native workspace",
      assignee: "alice",
    });
    expect(result.success).toBe(true);
    if (result.success) expect(result.data).toMatchObject({ assignee: "alice" });
    expect(
      schemaFor("dispatch_issue").safeParse({ project: "DSP", title: "x", assignee: 7 }).success
    ).toBe(false);
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

  test("dispatch_issue preserves optional initial labels", () => {
    const result = schemaFor("dispatch_issue").safeParse({
      project: "DSP",
      title: "Native workspace",
      labels: ["frontend", "urgent"],
    });

    expect(result).toMatchObject({
      data: { labels: ["frontend", "urgent"] },
      success: true,
    });
  });

  test("dispatch_issue accepts only the four coarse priority buckets", () => {
    const schema = schemaFor("dispatch_issue");

    expect(
      schema.safeParse({ project: "DSP", title: "Native workspace", priority: 2 })
    ).toMatchObject({
      data: { priority: 2 },
      success: true,
    });
    for (const priority of [-1, 4, 1.5]) {
      expect(
        schema.safeParse({ project: "DSP", title: "Native workspace", priority }).success
      ).toBe(false);
    }
  });

  test("dispatch_issue_update requires a field besides issue and names the updatable ones", () => {
    const schema = schemaFor("dispatch_issue_update");

    const bare = schema.safeParse({ issue: "DSP-1" });
    expect(bare.success).toBe(false);
    if (bare.success) return;
    expect(bare.error.issues.map((issue) => issue.message)).toEqual([
      "Issue update requires at least one field besides issue: status, title, labels, external_links, route, or parent.",
    ]);

    for (const args of [
      { issue: "DSP-1", title: "Renamed" },
      { issue: "DSP-1", labels: [] },
      { issue: "DSP-1", external_links: ["https://github.com/owner/repo/pull/7"] },
      { issue: "DSP-1", route: "" },
      { issue: "DSP-1", parent: "DSP-2" },
      { issue: "DSP-1", parent: "" },
    ]) {
      expect(schema.safeParse(args).success, JSON.stringify(args)).toBe(true);
    }
  });

  test("dispatch_issue_update accepts only Legion lifecycle statuses and never a priority", () => {
    const schema = schemaFor("dispatch_issue_update");

    for (const status of ISSUE_STATUSES) {
      expect(schema.safeParse({ issue: "DSP-1", status }).success, status).toBe(true);
    }
    expect(schema.safeParse({ issue: "DSP-1", status: "closed" }).success).toBe(false);
    expect(schema.safeParse({ issue: "DSP-1", status: "Done" }).success).toBe(false);
    expect(schema.safeParse({ issue: "DSP-1", status: "done", priority: 1 }).success).toBe(false);
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

  test("dispatch_ask exposes no kind: every ask it opens is a question", () => {
    const schema = schemaFor("dispatch_ask");

    for (const kind of ["action", "approval"]) {
      const parsed = schema.safeParse({ issue: "DSP-1", kind, question: "Confirm the deploy." });
      expect(parsed.success).toBe(true);
      expect(parsed.data).not.toHaveProperty("kind");
    }
  });

  test("requires an ask edit to include a patch field", () => {
    const schema = schemaFor("dispatch_edit_ask");

    expect(schema.safeParse({ ask: "ask-1" }).success).toBe(false);
    expect(schema.safeParse({ ask: "ask-1", question: "Ship the revised plan?" }).success).toBe(
      true
    );
  });

  test("accepts a comment turn only alongside reply_to_ask", () => {
    const schema = schemaFor("dispatch_comment");
    const reply = { issue: "DSP-1", body: "Dispatched two auditors, back with results." };

    expect(schema.safeParse({ ...reply, reply_to_ask: "ask-1", turn: "agent" })).toMatchObject({
      data: { turn: "agent" },
      success: true,
    });
    expect(schema.safeParse({ ...reply, reply_to_ask: "ask-1", turn: "human" }).success).toBe(true);
    expect(schema.safeParse({ ...reply, turn: "agent" }).success).toBe(false);
    expect(schema.safeParse({ ...reply, reply_to: "comment-1", turn: "agent" }).success).toBe(
      false
    );
    expect(schema.safeParse({ ...reply, reply_to_ask: "ask-1", turn: "nobody" }).success).toBe(
      false
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

  test("accepts a document retype operation with typed attributes", () => {
    expect(
      schemaFor("dispatch_doc_edit").safeParse({
        issue: "DSP-1",
        artifact: "spec",
        ops: [
          {
            op: "retype",
            block: "b-123",
            type: "ask",
            attributes: { multiple: false, urgency: "high" },
          },
        ],
      }).success
    ).toBe(true);
  });

  test("accepts block-addressed delete and move operations", () => {
    expect(
      schemaFor("dispatch_doc_edit").safeParse({
        issue: "DSP-1",
        artifact: "spec",
        ops: [
          { op: "delete", block: "b-123" },
          { op: "move", block: "b-123", after: "block:b-456" },
          { op: "move", block: "b-123", before: "heading:Design" },
        ],
      }).success
    ).toBe(true);
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

  test("accepts exactly one issue or project owner", () => {
    const cases = [
      ["dispatch_ask", { project: "CORE", artifact: "runbook-md", question: "Publish?" }],
      ["dispatch_comment", { project: "CORE", artifact: "runbook-md", body: "Looks good." }],
      [
        "dispatch_suggest",
        {
          project: "CORE",
          artifact: "runbook-md",
          quote: "draft",
          replace_with: "final",
        },
      ],
      [
        "dispatch_doc_edit",
        {
          project: "CORE",
          artifact: "runbook-md",
          ops: [{ op: "replace", find: "draft", with: "final" }],
        },
      ],
      ["dispatch_doc_read", { project: "CORE", artifact: "runbook-md" }],
      ["dispatch_artifact", { project: "CORE", name: "runbook.md", content: "# Runbook" }],
      ["dispatch_read", { project: "CORE", artifact: "runbook-md" }],
    ] as const;

    for (const [name, args] of cases) {
      const schema = schemaFor(name);
      expect(schema.safeParse(args).success, `${name} project owner`).toBe(true);
      expect(schema.safeParse({ ...args, issue: "CORE-1" }).success, `${name} both owners`).toBe(
        false
      );
    }
  });

  test("requires artifact with project on document tools but not dispatch_artifact", () => {
    const cases = [
      ["dispatch_ask", { project: "CORE", question: "Publish?" }],
      ["dispatch_comment", { project: "CORE", body: "Looks good." }],
      ["dispatch_suggest", { project: "CORE", quote: "draft", replace_with: "final" }],
      [
        "dispatch_doc_edit",
        { project: "CORE", ops: [{ op: "replace", find: "draft", with: "final" }] },
      ],
      ["dispatch_doc_read", { project: "CORE" }],
      ["dispatch_read", { project: "CORE" }],
    ] as const;

    for (const [name, args] of cases) {
      expect(schemaFor(name).safeParse(args).success, name).toBe(false);
    }
    expect(
      schemaFor("dispatch_artifact").safeParse({
        project: "CORE",
        name: "runbook.md",
        content: "# Runbook",
      }).success
    ).toBe(true);
  });

  test("accepts a project-document ref in place of issue/project on every ref-aware tool", () => {
    expect(
      schemaFor("dispatch_doc_read").safeParse({
        ref: "dispatch://CORE/artifact/runbook-md",
      }).success
    ).toBe(true);
    expect(
      schemaFor("dispatch_read").safeParse({
        ref: "dispatch://CORE/artifact/runbook-md",
      }).success
    ).toBe(true);
    expect(
      schemaFor("dispatch_ask").safeParse({
        ref: "dispatch://CORE/artifact/runbook-md",
        question: "Publish?",
      }).success
    ).toBe(true);
    expect(
      schemaFor("dispatch_comment").safeParse({
        ref: "dispatch://CORE/artifact/runbook-md",
        body: "Looks good.",
      }).success
    ).toBe(true);
    expect(
      schemaFor("dispatch_suggest").safeParse({
        ref: "dispatch://CORE/artifact/runbook-md",
        quote: "draft",
        replace_with: "final",
      }).success
    ).toBe(true);
    expect(
      schemaFor("dispatch_doc_edit").safeParse({
        ref: "dispatch://CORE/artifact/runbook-md",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      }).success
    ).toBe(true);
  });

  test("rejects dispatch_suggest and dispatch_doc_edit when neither artifact nor ref names the document", () => {
    expect(
      schemaFor("dispatch_suggest").safeParse({
        issue: "CORE-1",
        quote: "draft",
        replace_with: "final",
      }).success
    ).toBe(false);
    expect(
      schemaFor("dispatch_doc_edit").safeParse({
        issue: "CORE-1",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      }).success
    ).toBe(false);
  });

  test("does not advertise a primary artifact option", () => {
    const artifact = dispatchToolSpecs.find((spec) => spec.name === "dispatch_artifact");
    if (!artifact) throw new Error("missing dispatch_artifact");
    const argumentsSchema = artifact.arguments(schemaApi) as Record<string, unknown>;
    expect(argumentsSchema).not.toHaveProperty("primary");
  });

  test("describes project documents as accepting an artifact id, slug, or filename", () => {
    for (const name of [
      "dispatch_ask",
      "dispatch_comment",
      "dispatch_suggest",
      "dispatch_doc_edit",
      "dispatch_doc_read",
      "dispatch_request_approval",
      "dispatch_read",
    ] as const) {
      const spec = dispatchToolSpecs.find((candidate) => candidate.name === name);
      if (!spec) throw new Error(`missing ${name}`);
      const argumentsSchema = spec.arguments(schemaApi) as unknown as {
        artifact: z.ZodOptional<z.ZodString>;
      };
      expect(argumentsSchema.artifact.unwrap().description, name).toContain(
        "artifact id, slug, or filename"
      );
    }
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

  test("dispatch_resolve_comment takes only the comment reference", () => {
    const schema = schemaFor("dispatch_resolve_comment");

    expect(schema.safeParse({ comment: "5a660655-04ad-4ce0-8a9b-93dd03c412b7" }).success).toBe(
      true
    );
    expect(schema.safeParse({}).success).toBe(false);
    expect(schema.safeParse({ comment: "comment-1", reason: "done" }).success).toBe(false);
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

  test("dispatch_follow takes a full ask id and follow or unfollow, nothing else", () => {
    const schema = schemaFor("dispatch_follow");
    expect(schema.safeParse({ ask: "ask-1", action: "unfollow" }).success).toBe(true);
    expect(schema.safeParse({ ask: "ask-1", action: "mute" }).success).toBe(false);
    expect(schema.safeParse({ ask: "ask-1" }).success).toBe(false);
    expect(schema.safeParse({ ask: "ask-1", action: "follow", issue: "DSP-1" }).success).toBe(
      false
    );
  });

  test("no tool result subscribes the session to an issue; following an ask is the write's side effect", () => {
    expect(dispatchToolSpecs.every((spec) => !("subscribes" in spec))).toBe(true);
  });
});

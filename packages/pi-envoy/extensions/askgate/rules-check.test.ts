// The rules check's own units, driven without the gate: the verdict parser, the asks list the
// request renders, and `runRulesCheck` with the walk and the Dispatch the core lends it. The gate
// tests of the same check — a refusal, the breaker, the notice — are in `askgate.test.ts`.
import { describe, expect, test } from "bun:test";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import {
  describeAsks,
  parseRuleVerdict,
  type RuleMatch,
  type RulesCheckCall,
  type RulesCheckDeps,
  runRulesCheck,
} from "./rules-check";

const bytes = (s: string) => Buffer.byteLength(s, "utf8");
/** The rules call's answer, as its request asks for it. */
const rulesAnswer = (matches: readonly Partial<RuleMatch>[], reason = "") =>
  JSON.stringify({ matches, reason });
const RULES_NONE = rulesAnswer([]);
/** The charter's own answer, which is no rules verdict. */
const ALLOW = '{"decision":"allow"}';

describe("parseRuleVerdict", () => {
  test("parseRuleVerdict takes the rules JSON on the last line and calls any other shape no verdict (D3)", () => {
    expect(
      parseRuleVerdict(
        `Ask 1 keeps things.\n${rulesAnswer([{ ask: 1, rule: "no_change", quote: "q" }], "r")}`,
        1
      )
    ).toEqual({ matches: [{ ask: 1, rule: "no_change", quote: "q" }], reason: "r" });
    expect(parseRuleVerdict(`${RULES_NONE}\n\`\`\``, 2)).toEqual({ matches: [], reason: "" });
    for (const text of [
      ALLOW,
      "no json",
      `${RULES_NONE}\nDone.`,
      '{"matches": []}',
      rulesAnswer([{ ask: 2, rule: "no_change", quote: "q" }]),
      rulesAnswer([{ ask: 1, rule: "other" as never, quote: "q" }]),
      '{"matches": [{"ask": 1, "rule": "no_change"}], "reason": ""}',
    ]) {
      expect(parseRuleVerdict(text, 1)).toBeUndefined();
    }
    // Each ask is listed once, with its first rule.
    expect(
      parseRuleVerdict(
        rulesAnswer([
          { ask: 1, rule: "untried_access", quote: "a" },
          { ask: 1, rule: "no_change", quote: "b" },
        ]),
        1
      )?.matches
    ).toEqual([{ ask: 1, rule: "untried_access", quote: "a" }]);
  });
});

describe("describeAsks (E6)", () => {
  /** Each ask's text as the list carries it, in order. */
  const bodiesOf = (list: string) =>
    list
      .split(/<ask n="\d+"[^>]*>\n/)
      .slice(1)
      .map((body) => body.replace(/\n<\/ask>\n?$/, ""));
  test("a call's every ask is listed, each clipped to its share of the list (E6)", () => {
    // The call the review reproduced: three long ask blocks, then the one whose own
    // recommendation is to change nothing. Spending the list first-come left that fourth ask out
    // of the request, so no rules check ever saw it and the call recorded `none`.
    const long = "x".repeat(9 * 1024);
    const described = describeAsks([
      ...Array.from({ length: 3 }, (_, index) => ({
        kind: "block" as const,
        id: `a${index}`,
        text: `Keep item ${index} as it is? ${long}`,
        opening: `Keep item ${index}`,
      })),
      {
        kind: "block",
        id: "a3",
        text: `Should we change the retry budget? ${long} My recommendation is to change nothing`,
        opening: "Should we change",
      },
    ]);
    expect(described.list).toContain('<ask n="4" kind="block" id="a3">');
    expect(described.list.match(/<ask n=/g)).toHaveLength(4);
    expect(bytes(described.list)).toBeLessThanOrEqual(32 * 1024);
    // Four asks, each clipped: the cut counts each of them once and no ask is left out.
    expect(described.asksCut).toBe(4);
  });
  test("a call whose asks all fit is listed whole, with nothing counted cut (E6)", () => {
    const described = describeAsks([
      { kind: "ask", text: "Which deployment window?", opening: "Which deployment window?" },
      { kind: "block", id: "a1", text: "Keep the retry budget?", opening: "Keep the retry" },
    ]);
    expect(described.list).toBe(
      '<ask n="1" kind="ask">\nWhich deployment window?\n</ask>\n' +
        '<ask n="2" kind="block" id="a1">\nKeep the retry budget?\n</ask>'
    );
    expect(described.asksCut).toBe(0);
  });
  test("an ask the escaping pushes past the bound is clipped and counted as cut (E6)", () => {
    // Under 8 KiB as written, over it once `<`, `>` and `&` become entities, which is the text
    // the bound clips.
    const described = describeAsks([
      { kind: "block", id: "a1", text: "<&>".repeat(2 * 1024), opening: "<&>" },
    ]);
    expect(bytes(described.list)).toBeLessThan(9 * 1024);
    expect(described.list).toContain("elided");
    expect(described.asksCut).toBe(1);
  });
  test("a clipped ask keeps its start and the recommendation at its end (E6)", () => {
    // The review's three rows. A clip that kept only an ask's head cut the recommendation
    // `no_change` reads at the end of every one of them, so the rule could not match and each
    // call recorded `none` for asks nobody judged.
    const recommendation = "My recommendation is to keep it as it is.";
    for (const [count, size] of [
      [1, 9 * 1024],
      [2, 9 * 1024],
      [30, 1100],
    ]) {
      const described = describeAsks(
        Array.from({ length: count }, (_, index) => {
          const start = `Can you run the migration for item ${index}?`;
          const filler = "x".repeat(size - bytes(start) - bytes(recommendation) - 2);
          return {
            kind: "block" as const,
            id: `a${index}`,
            text: `${start} ${filler} ${recommendation}`,
            opening: start,
          };
        })
      );
      const bodies = bodiesOf(described.list);
      expect(bodies).toHaveLength(count);
      expect(described.asksCut).toBe(count);
      expect(bytes(described.list)).toBeLessThanOrEqual(32 * 1024);
      for (const [index, body] of bodies.entries()) {
        expect(body).toContain("elided");
        expect(body).toStartWith(`Can you run the migration for item ${index}?`);
        expect(body).toEndWith(recommendation);
      }
    }
  });
  test("a 32,000-character id leaves every other ask's recommendation in the list (E6)", () => {
    // The id entered its tag whole and the tags were paid before any text, so it spent the list
    // and the other asks were clipped to nothing.
    const recommendation = "Recommendation: keep it as it is";
    const pad = "y".repeat(380);
    const described = describeAsks([
      {
        kind: "block",
        id: "i".repeat(32_000),
        text: `Which deployment window? ${pad}`,
        opening: "Which deployment window?",
      },
      ...Array.from({ length: 3 }, (_, index) => ({
        kind: "block" as const,
        id: `b${index}`,
        text: `Should we change budget ${index}? ${pad} ${recommendation}`,
        opening: `Should we change budget ${index}?`,
      })),
    ]);
    const bodies = bodiesOf(described.list);
    for (const body of bodies.slice(1)) expect(body).toEndWith(recommendation);
    expect(bodies[0]).toBe(`Which deployment window? ${pad}`);
    expect(described.asksCut).toBe(0);
    // The id still names its ask, cut to 128 bytes.
    const id = described.list.match(/<ask n="1" kind="block" id="([^"]*)">/)?.[1] ?? "";
    expect(id).toMatch(/^i+…$/);
    expect(bytes(id)).toBeLessThanOrEqual(128);
  });
  test("a 64,000-character id keeps the list inside 32 KiB, cut on an entity's edge (E6)", () => {
    const described = describeAsks([
      { kind: "block", id: `"&<>`.repeat(16_000), text: "Which deployment window?", opening: "" },
      {
        kind: "block",
        id: "b",
        text: "Should we change the retry budget? Recommendation: keep it as it is",
        opening: "",
      },
    ]);
    expect(bytes(described.list)).toBeLessThanOrEqual(32 * 1024);
    expect(described.list).toContain("Recommendation: keep it as it is");
    expect(described.asksCut).toBe(0);
    const id = described.list.match(/<ask n="1" kind="block" id="([^"]*)">/)?.[1] ?? "";
    expect(bytes(id)).toBeLessThanOrEqual(128);
    expect(id).toEndWith("…");
    expect(id.slice(0, -1).replace(/&(?:quot|amp|lt|gt);/g, "")).toBe("");
  });
  test("a clip of escaped text never splits an entity or a character (E6)", () => {
    // Each unit escapes to 16 bytes (`&lt;€&amp;&gt;`). The short first ask moves the others'
    // share a byte at a time, so across these lengths both cuts land at every offset within a
    // unit: inside each entity and inside the three bytes of the `€`.
    const unit = "<€&>";
    const escapedBytes = 16 * 1000;
    for (let short = 1; short <= 64; short++) {
      const described = describeAsks([
        { kind: "ask", text: "z".repeat(short), opening: "z" },
        ...Array.from({ length: 4 }, (_, index) => ({
          kind: "block" as const,
          id: `a${index}`,
          text: unit.repeat(1000),
          opening: unit,
        })),
      ]);
      expect(described.list).not.toContain("\uFFFD");
      expect(bytes(described.list)).toBeLessThanOrEqual(32 * 1024);
      for (const body of bodiesOf(described.list).slice(1)) {
        const [head, elided, tail] = body.split(/ … \[elided (\d+) bytes\](?: … )?/);
        // Each side holds whole entities and whole characters and nothing else.
        expect(head.replace(/&(?:lt|amp|gt);|€/g, "")).toBe("");
        expect(tail.replace(/&(?:lt|amp|gt);|€/g, "")).toBe("");
        expect(Number(elided)).toBe(escapedBytes - bytes(head) - bytes(tail));
      }
    }
  });
});

describe("runRulesCheck: which asks a call opens", () => {
  /** The document Dispatch holds for the uploads below, under the name it trimmed on the way in. */
  const BRIEF = ":::ask{#window}\nWhich deployment window?\n:::\n";
  /** A Dispatch serving X-1 and its one document, as `readDocumentBlocks` reads it. */
  const serving = () => {
    const body = (value: unknown, status = 200) =>
      new Response(JSON.stringify(value), {
        status,
        headers: { "Content-Type": "application/json" },
      });
    const fetchImpl = (async (input: Request | URL | string) => {
      const url = new URL(input instanceof Request ? input.url : input.toString());
      if (url.pathname === "/api/v1/issues/X-1")
        return body({
          key: "X-1",
          primary_artifact_id: "doc-brief",
          artifacts: [
            {
              id: "doc-brief",
              issue_key: "X-1",
              project: "X",
              slug: "brief",
              name: "brief.md",
              kind: "doc",
              primary: true,
              versions: [{ number: 1 }],
            },
          ],
          open_asks: [],
        });
      const read = /^\/api\/v1\/artifacts\/doc-brief\/(text|blocks)$/.exec(url.pathname);
      if (read === null) return body({ code: "NOT_FOUND", error: "not found" }, 404);
      if (read[1] === "text") return body({ markdown: BRIEF, version: 1, token: "sha256:d" });
      return body([
        {
          id: "window",
          type: "ask",
          from: 0,
          to: bytes(BRIEF.trimEnd()),
          token: "sha256:0",
          references: { comments: 0, asks: 0 },
        },
      ]);
    }) as typeof fetch;
    return (_directory: string, signal: AbortSignal) => ({
      client: new DispatchClient("https://dispatch.example", "token", fetchImpl, signal),
      serverUrls: ["https://dispatch.example"],
    });
  };
  /** One rules check on its own, driven with the walk and the Dispatch the core lends it. */
  const checkRules = (
    call: Pick<RulesCheckCall, "toolName" | "tool" | "input">,
    deps: Partial<RulesCheckDeps> = {}
  ) =>
    runRulesCheck(
      {
        now: () => 0,
        env: {},
        dispatchClient: serving(),
        walk: async () => ({
          answer: { kind: "answer" as const, text: RULES_NONE },
          called: {},
          elapsedMs: 0,
        }),
        ...deps,
      },
      {
        directory: async () => "/work",
        system: "rules",
        context: () => ({ primarySystemPrompt: "", transcript: "" }),
        signal: new AbortController().signal,
        ...call,
      }
    );

  test("a re-upload judges only the asks the stored version does not hold, by the trimmed name Dispatch stores", async () => {
    const check = await checkRules({
      toolName: "dispatch_artifact",
      tool: "dispatch artifact",
      input: {
        issue: "X-1",
        name: "  brief.md  ",
        content: `${BRIEF}\n:::ask{#region}\nWhich region?\n:::\n`,
      },
    });
    expect(check?.names).toEqual(["The ask block #region that starts 'Which region?'"]);
  });
  test("only an upload leaves out the ids the document holds: a doc-edit insert of one is a new ask", async () => {
    const check = await checkRules({
      toolName: "dispatch_doc_edit",
      tool: "dispatch doc-edit",
      input: {
        issue: "X-1",
        artifact: "brief",
        ops: [{ op: "insert", after: "end", markdown: BRIEF }],
      },
    });
    expect(check?.names).toEqual(["The ask block #window that starts 'Which deployment window?'"]);
  });
  /** A session whose Dispatch does not resolve: the de-duplication read fails on every call. */
  const noDispatch: Partial<RulesCheckDeps> = { dispatchClient: () => undefined };
  test("an upload whose de-duplication read fails judges the openers with no id and leaves the id-carrying ones alone", async () => {
    // Without the read nothing says which asks the stored version already holds, and an opener
    // carrying an id may be one of them.
    const check = await checkRules(
      {
        toolName: "dispatch_artifact",
        tool: "dispatch artifact",
        input: {
          issue: "X-1",
          name: "brief.md",
          content: `${BRIEF}\n:::ask{}\nWhich region?\n:::\n`,
        },
      },
      noDispatch
    );
    expect(check?.names).toEqual(["The ask block that starts 'Which region?'"]);
    expect(check?.asksPartial).toBe(true);
    // No ask was judged without its own text: the upload carries the text of every ask it holds.
    expect(check?.textMissing).toBe(false);
  });
  test("an unchanged document re-uploaded when the de-duplication read fails is not refused", async () => {
    let walks = 0;
    const check = await checkRules(
      {
        toolName: "dispatch_artifact",
        tool: "dispatch artifact",
        input: { issue: "X-1", name: "brief.md", content: BRIEF },
      },
      {
        ...noDispatch,
        // The rule the stored ask block would match, were it judged as an ask this upload adds.
        walk: async () => {
          walks += 1;
          return {
            answer: {
              kind: "answer" as const,
              text: rulesAnswer([{ ask: 1, rule: "no_change", quote: "Which deployment window?" }]),
            },
            called: {},
            elapsedMs: 0,
          };
        },
      }
    );
    // Its one opener carries an id the stored version may hold, so no model judges it and nothing
    // matches; the check says it judged none of the call's asks rather than staying silent.
    expect(walks).toBe(0);
    expect(check).toMatchObject({
      outcome: "unjudged",
      asksPartial: true,
      matches: [],
      names: [],
      promptBytes: 0,
    });
    expect(check?.model).toBeUndefined();
    expect(check?.error).toBeUndefined();
  });
  test("an upload whose de-duplication read fails with an id on every opener, a new one among them, answers unjudged with no model call", async () => {
    // A document downloaded from Dispatch, every stored block already carrying an id, and a new
    // ask the agent wrote with one of its own: the stored version could not hold `#new`, but with
    // the read failed nothing says so, and a judged list emptied by the id filter is no clean call.
    let walks = 0;
    let clock = 0;
    const check = await checkRules(
      {
        toolName: "dispatch_artifact",
        tool: "dispatch artifact",
        input: {
          issue: "X-1",
          name: "brief.md",
          content: `${BRIEF}\n:::ask{#new}\nShould I change the cache path? Recommendation: keep it as it is.\n:::\n`,
        },
      },
      {
        // The read takes 7 ms of the clock, and that is all the check spends.
        now: () => clock,
        dispatchClient: () => {
          clock += 7;
          return undefined;
        },
        walk: async () => {
          walks += 1;
          return {
            answer: { kind: "answer" as const, text: RULES_NONE },
            called: {},
            elapsedMs: 0,
          };
        },
      }
    );
    expect(walks).toBe(0);
    expect(check).toMatchObject({ outcome: "unjudged", asksPartial: true, latencyMs: 7 });
    expect(check?.textMissing).toBe(false);
  });
  test("a re-upload whose stored version holds every opener opens no new ask, and the check answers nothing", async () => {
    // The other fact behind an empty judged list: the read answered, and every ask is already held.
    let walks = 0;
    const check = await checkRules(
      {
        toolName: "dispatch_artifact",
        tool: "dispatch artifact",
        input: { issue: "X-1", name: "brief.md", content: BRIEF },
      },
      {
        walk: async () => {
          walks += 1;
          return {
            answer: { kind: "answer" as const, text: RULES_NONE },
            called: {},
            elapsedMs: 0,
          };
        },
      }
    );
    expect(walks).toBe(0);
    expect(check).toBeUndefined();
  });
  test("a doc-edit whose document read fails still judges every ask it inserts", async () => {
    const check = await checkRules(
      {
        toolName: "dispatch_doc_edit",
        tool: "dispatch doc-edit",
        input: {
          issue: "X-1",
          artifact: "brief",
          ops: [{ op: "insert", after: "end", markdown: BRIEF }],
        },
      },
      noDispatch
    );
    expect(check?.names).toEqual(["The ask block #window that starts 'Which deployment window?'"]);
    expect(check?.asksPartial).toBe(false);
  });
});

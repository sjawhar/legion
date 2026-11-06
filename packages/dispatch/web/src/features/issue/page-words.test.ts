import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { issueStatuses, statusText } from "../project/board-model";
import {
  HELP_LINES,
  KEY_REPORTS,
  MARGIN_SECTION_NAMES,
  MARGIN_SECTION_ORDER,
  PROPERTY_LABELS,
} from "./page-structure";
import {
  APPROVAL_STATE_WORDS,
  BLOCK_ATTRIBUTE_LABELS,
  blockAttributeLabel,
  CHECK_WHEN_WORDS,
  CONTROL_LABELS,
  CONVERSATION_STATE_NAMES,
  CONVERSATION_STATE_ORDER,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  ISSUE_STATUS_WORDS,
  issueStatusWord,
  NODE_FACT_LABELS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  nodeFactLabel,
  nodeStateWord,
  nodeStateWords,
  nodeTypeName,
  PRESET_NAMES,
  PRESET_ORDER,
  RESULT_CURRENCY_WORDS,
  RESULT_VERDICT_WORDS,
  WAITING_LINES,
  wordsIn,
} from "./page-words";

/** The block schema the server owns, read here rather than repeated, so a type, an attribute or
 *  a `when` choice added to it fails this file until the page has a word for it. */
interface BlockSchema {
  readonly types: readonly {
    readonly attributes: Readonly<Record<string, { readonly choices?: readonly string[] }>>;
    readonly name: string;
  }[];
}

const schema: BlockSchema = JSON.parse(
  readFileSync(
    join(import.meta.dir, "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json"),
    "utf8"
  )
);

function blockType(name: string) {
  const type = schema.types.find((candidate) => candidate.name === name);
  if (type === undefined) {
    throw new Error(`blocks.json declares no ${name} block`);
  }
  return type;
}

function choicesOf(type: string, attribute: string): readonly string[] {
  const choices = blockType(type).attributes[attribute]?.choices;
  if (choices === undefined) {
    throw new Error(`the ${type} block's ${attribute} declares no choices`);
  }
  return choices;
}

/** Every string a declaration holds, however deeply. The module derives `DECLARED_WORDS` the
 *  same way; this walk is written again here so the test checks the derivation rather than
 *  calling it. */
function phrasesOf(value: unknown): readonly string[] {
  if (typeof value === "string") {
    return [value];
  }
  if (typeof value === "object" && value !== null) {
    return Object.values(value).flatMap(phrasesOf);
  }
  return [];
}

const WORD_SOURCES: readonly unknown[] = [
  NODE_TYPE_NAMES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  CONVERSATION_STATE_NAMES,
  ISSUE_STATUS_WORDS,
  EMPTY_STATE_LINES,
  KEY_REPORTS,
  GAP_LINES,
  GAP_WORDS,
  CHECK_WHEN_WORDS,
  RESULT_CURRENCY_WORDS,
  RESULT_VERDICT_WORDS,
  WAITING_LINES,
  APPROVAL_STATE_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  MARGIN_SECTION_NAMES,
  HELP_LINES,
  BLOCK_ATTRIBUTE_LABELS,
  NODE_FACT_LABELS,
];

test("the conversation's four states are named in the order the panel offers them", () => {
  expect(CONVERSATION_STATE_ORDER.map((state) => CONVERSATION_STATE_NAMES[state])).toEqual([
    "Everything",
    "Pinned",
    "Questions",
    "What people said",
  ]);
});

test("every issue status the board knows has a word, and it is the board's own", () => {
  for (const status of issueStatuses) {
    expect(Object.keys(ISSUE_STATUS_WORDS), `${status} has no word`).toContain(status);
    expect(issueStatusWord(status)).toBe(statusText(status));
  }
  expect(Object.keys(ISSUE_STATUS_WORDS).sort()).toEqual([...issueStatuses].sort());
  expect(issueStatusWord("something-the-server-added")).toBe("something-the-server-added");
});

test("every node type has a plain name, and no name is an internal token", () => {
  expect(NODE_TYPES.length).toBeGreaterThan(0);
  for (const type of NODE_TYPES) {
    const name = NODE_TYPE_NAMES[type];
    expect(name).toMatch(/^[a-z]+(?: [a-z]+)*$/);
    expect(name).not.toBe(name.toUpperCase());
    expect(name.includes("_")).toBe(false);
  }
});

test("every block type the schema declares has a name, and one it does not still reads", () => {
  for (const type of schema.types) {
    expect(Object.keys(NODE_TYPE_NAMES)).toContain(type.name);
    expect(nodeTypeName(type.name)).toBe(
      NODE_TYPE_NAMES[type.name as keyof typeof NODE_TYPE_NAMES]
    );
  }
  expect(Object.keys(NODE_TYPE_NAMES)).not.toContain("gantt_chart");
  expect(nodeTypeName("gantt_chart")).toBe("gantt chart");
});

test("a type the model gives no state has no state words, and the ask and the check have theirs", () => {
  for (const type of NODE_TYPES) {
    if (type === "ask" || type === "check") {
      continue;
    }
    expect(nodeStateWords(type)).toBeUndefined();
    expect(nodeStateWord(type, "open")).toBeUndefined();
  }
  expect(Object.keys(NODE_STATE_WORDS.ask).sort()).toEqual([...choicesOf("ask", "state")].sort());
  expect(Object.keys(NODE_STATE_WORDS.check).sort()).toEqual([
    "failed",
    "no-result",
    "older",
    "passed",
    "unmarked",
  ]);
  expect(nodeStateWord("check", "no-result")).toBe("No result");
  expect(nodeStateWord("ask", "answered")).toBe("Answered");
  expect(nodeStateWord("check", "retired")).toBeUndefined();
});

test("no two types share a glyph token, and no two states of one surface share one", () => {
  const typeTokens = NODE_TYPES.map((type) => GLYPH_TOKENS.types[type]);
  expect(new Set(typeTokens).size).toBe(typeTokens.length);

  const askTokens = Object.values(GLYPH_TOKENS.states.ask);
  const checkTokens = Object.values(GLYPH_TOKENS.states.check);
  expect(new Set(askTokens).size).toBe(askTokens.length);
  expect(new Set(checkTokens).size).toBe(checkTokens.length);
  expect(checkTokens.length).toBe(5);

  const everyToken = [...typeTokens, ...askTokens, ...checkTokens];
  expect(new Set(everyToken).size).toBe(everyToken.length);
});

test("every glyph token has an accessible name", () => {
  const tokens = [
    ...NODE_TYPES.map((type) => GLYPH_TOKENS.types[type]),
    ...Object.values(GLYPH_TOKENS.states.ask),
    ...Object.values(GLYPH_TOKENS.states.check),
  ];
  for (const token of tokens) {
    expect(GLYPH_NAMES[token]).toMatch(/\S/);
  }
  expect(Object.keys(GLYPH_NAMES).sort()).toEqual([...tokens].sort());
  expect(GLYPH_NAMES["check-passed"]).toBe("Passed check");
  expect(GLYPH_NAMES["plan-task"]).toBe("plan task");
});

test("the five presets are named in the order the row draws them", () => {
  expect(PRESET_ORDER.map((preset) => PRESET_NAMES[preset])).toEqual([
    "Tree",
    "Decisions",
    "Checks",
    "Spec",
    "Plan",
  ]);
  for (const preset of PRESET_ORDER) {
    expect(EMPTY_STATE_LINES[preset]).toMatch(/^[A-Z].*\.$/);
  }
});

test("every empty-state line is one sentence a person reads", () => {
  for (const line of Object.values(EMPTY_STATE_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
  expect(EMPTY_STATE_LINES.check_result).toBe("No result recorded against this check.");
});

test("every key report is one sentence saying where the key could not go", () => {
  for (const line of Object.values(KEY_REPORTS)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
  expect(KEY_REPORTS.no_next_question).toBe("No further open question.");
});

test("every gap line is one sentence that says what is absent", () => {
  for (const line of Object.values(GAP_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
  expect(GAP_LINES.no_origin).toBe("Nothing explains this node: it names nothing it came from.");
});

test("every gap a row draws has a short word, one per gap the page names", () => {
  expect(Object.keys(GAP_WORDS).sort()).toEqual(Object.keys(GAP_LINES).sort());
  for (const [kind, word] of Object.entries(GAP_WORDS)) {
    expect(word, `the gap ${kind} is named with an internal token`).toMatch(/^[A-Z][^_.]*$/);
  }
});

test("every property has a plain name, and no name is an internal token", () => {
  for (const [field, label] of Object.entries(PROPERTY_LABELS)) {
    expect(label, `${field} is named with an internal token`).toMatch(/^[A-Z][^_]*$/);
  }
  // The two the model names differently from the page: a reader is told what a route does to
  // their messages, and whose turn it is, in words rather than in the field's own name.
  expect(PROPERTY_LABELS.route).toBe("Messages go to");
  expect(PROPERTY_LABELS.blocked_by).toBe("Blocked by");
});

test("the panel draws three named sections, in the order it names them", () => {
  expect(MARGIN_SECTION_ORDER.map((section) => MARGIN_SECTION_NAMES[section])).toEqual([
    "Development",
    "People",
    "References",
  ]);
});

test("every help line is whole sentences, because it replaced a tooltip", () => {
  for (const line of Object.values(HELP_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
});

test("every `when` the block schema declares reads as words", () => {
  expect(Object.keys(CHECK_WHEN_WORDS).sort()).toEqual([...choicesOf("check", "when")].sort());
  expect(CHECK_WHEN_WORDS.before_merge).toBe("before the merge");
  for (const words of Object.values(CHECK_WHEN_WORDS)) {
    expect(words.includes("_")).toBe(false);
  }
});

test("every block attribute the schema declares has a label, and one it does not still reads", () => {
  for (const type of schema.types) {
    for (const attribute of Object.keys(type.attributes)) {
      expect(Object.keys(BLOCK_ATTRIBUTE_LABELS)).toContain(attribute);
      expect(blockAttributeLabel(attribute).includes("_")).toBe(false);
    }
  }
  expect(Object.keys(BLOCK_ATTRIBUTE_LABELS)).not.toContain("owner_of_record");
  expect(blockAttributeLabel("owner_of_record")).toBe("owner of record");
});

test("every fact a node's body draws has a plain label, and a block attribute keeps its own", () => {
  for (const [name, label] of Object.entries(NODE_FACT_LABELS)) {
    expect(label, `the fact ${name} is named with an internal token`).toMatch(/^[a-z][^_]*$/);
  }
  expect(nodeFactLabel("stands")).toBe("how it stands");
  expect(nodeFactLabel("negative_control")).toBe(BLOCK_ATTRIBUTE_LABELS.negative_control);
  expect(nodeFactLabel("owner_of_record")).toBe("owner of record");
});

test("a result's verdict reads as a word, and every waiting line is one sentence", () => {
  expect(Object.keys(RESULT_VERDICT_WORDS).sort()).toEqual(["fail", "pass"]);
  for (const word of Object.values(RESULT_VERDICT_WORDS)) {
    expect(word).toMatch(/^[A-Z][^_]*$/);
  }
  for (const line of Object.values(WAITING_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
});

test("DECLARED_WORDS holds every declared word and no other", () => {
  const derived = new Set(
    WORD_SOURCES.flatMap(phrasesOf).flatMap((phrase) => [...wordsIn(phrase)])
  );
  expect([...derived].sort()).toEqual([...DECLARED_WORDS]);

  for (const phrase of WORD_SOURCES.flatMap(phrasesOf)) {
    for (const word of wordsIn(phrase)) {
      expect(DECLARED_WORDS).toContain(word);
    }
  }
  for (const word of DECLARED_WORDS) {
    expect(WORD_SOURCES.flatMap(phrasesOf).some((phrase) => wordsIn(phrase).includes(word))).toBe(
      true
    );
  }

  expect(DECLARED_WORDS).toContain("merge");
  expect(DECLARED_WORDS).not.toContain("verification");
  expect(DECLARED_WORDS).not.toContain("before_merge");
});

// The two synthetic documents the large-document measurement opens, and the generator that
// spells them (dispatch://INFRA-341/spec, requirement R11 and acceptance A7). One is shaped like
// the document that would not finish drawing on a phone,
// dispatch://AHXV6-1/artifact/intermediate-specs-md - about 81,000 characters and 28 Mermaid
// diagrams - and the other is five times as large. Both are written here rather than copied,
// because this repository is public and that document is not.
//
// Every section holds the same mix the set-up names - a heading, prose, a subheading, a list, a
// table, a code block and one Mermaid diagram, with five decision blocks spread through the
// document - and every section spells the same number of characters, so the larger document is
// the smaller one's shape five times over rather than a different document. This module imports
// neither Playwright nor a browser module, so a spec loads it for its bytes alone.
import { padTo } from "./heavy-bodies";

/** How many characters a section spells, whatever else it holds: the prose is padded to fill
 *  what the section's other blocks leave, so a decision block costs prose rather than length. */
const sectionCharacters = 2_900;

/** The smaller document's sections, one diagram each: the AHXV6-1 document's 28 diagrams in
 *  about its 81,000 characters. */
const smallSections = 28;

/** Five times as many sections, which is five times the characters and five times the diagrams. */
const largeSections = smallSections * 5;

/** How many decision blocks a document holds, however many sections it has. The last of them
 *  sits in the last section, so a link to one lands near the end of the document. */
export const decisionBlocks = 5;

/** Every third section's diagram is a sequence diagram and the rest are flowcharts, which is
 *  about the mix the AHXV6-1 document has (17 flowcharts to 10 sequence diagrams). */
const sequenceDiagramEvery = 3;

/** The subjects the sections cycle through, so no two neighbouring sections read alike. */
const subjects = [
  "lease ledger",
  "settlement window",
  "replay queue",
  "block index",
  "reader's place",
  "diagram cache",
  "write store",
] as const;

function subject(index: number): string {
  return subjects[index % subjects.length];
}

/** The sentence a section's prose opens with, which names the section so no two sections are the
 *  same bytes. */
function proseHead(index: number): string {
  return (
    `Section ${index + 1} is about the ${subject(index)}. It is written here rather than taken ` +
    "from the document that failed on a phone, because this repository is public and that " +
    "document is not."
  );
}

/** What a section's prose is padded with, so the section reaches its budget whatever its other
 *  blocks cost. The cut lands mid-sentence, which a paragraph reads as the prose it is. */
const proseFiller =
  " The ledger holds three leases, and the window closes once the last of them has settled, so " +
  "a reader who opens this document late still reads the same order of events as the reader " +
  "who opened it first.";

function listBlock(index: number): string {
  const what = subject(index);
  return [
    `- The ${what} is read once the document opens, never before.`,
    `- A lease of the ${what} is taken for one window and released at its close.`,
    `- A second reader of the ${what} waits for the window rather than for the lease.`,
    `- A write to the ${what} that finds no lease is refused with the window it asked for.`,
    `- A ${what} with no lease at all settles immediately and records nothing.`,
  ].join("\n");
}

function tableBlock(index: number): string {
  const what = subject(index);
  return [
    "| Field | What it holds | Default |",
    "| --- | --- | --- |",
    `| lease | the window the ${what} was taken in | none |`,
    `| holder | who holds the ${what} until the window closes | nobody |`,
    "| settled_at | when the window closed, or nothing while it is open | nothing |",
    `| replays | how many times the ${what} was read again after a refusal | 0 |`,
  ].join("\n");
}

function codeBlock(index: number): string {
  return [
    "```ts",
    `export function settle${index + 1}(lease: Lease, window: Window): Settlement {`,
    "  const held = window.leases.filter((taken) => taken.id === lease.id);",
    "  if (held.length === 0) {",
    '    throw new Error("lease " + lease.id + " is not held in window " + window.id);',
    "  }",
    "  return { at: window.closesAt, lease: lease.id, window: window.id };",
    "}",
    "```",
  ].join("\n");
}

function flowchart(index: number): string {
  const number = index + 1;
  return [
    "```mermaid",
    "flowchart TD",
    `  open["Open section ${number}"] --> free{"Lease free?"}`,
    '  free -->|yes| take["Take the lease"]',
    '  free -->|no| hold["Wait for the window"]',
    '  take --> write["Write the ledger"]',
    "  hold --> write",
    `  write --> settle["Settle section ${number}"]`,
    '  settle --> read(["The reader reads on"])',
    "```",
  ].join("\n");
}

function sequenceDiagram(index: number): string {
  const number = index + 1;
  return [
    "```mermaid",
    "sequenceDiagram",
    "  participant R as Reader",
    "  participant S as Server",
    "  participant L as Ledger",
    `  R->>S: open section ${number}`,
    `  S->>L: read the lease of section ${number}`,
    `  L-->>S: the lease of section ${number} is held`,
    `  S-->>R: section ${number} is drawn`,
    "  Note over R,L: the window closes at the next settlement",
    "```",
  ].join("\n");
}

/** The id of the `decision`th decision block, counted from 1, which a link to a decision block
 *  names (`#b-<blockId>`). */
export function decisionBlockId(decision: number): string {
  return `decision-${decision}`;
}

function decisionBlock(decision: number, index: number): string {
  return [
    `:::ask{#${decisionBlockId(decision)} urgency="med" multiple="false"}`,
    `Which window should the ${subject(index)} of section ${index + 1} settle in?`,
    "",
    "- The next window",
    "- The window after it",
    ":::",
  ].join("\n");
}

/** Which section holds the `decision`th decision block: the five are spread evenly, and the
 *  last of them is in the last section. */
function decisionSection(decision: number, sections: number): number {
  return Math.floor((decision * sections) / decisionBlocks) - 1;
}

function sectionText(index: number, decision: number | undefined): string {
  const blocks = [
    `## ${index + 1}. The ${subject(index)}`,
    "",
    `### How the ${subject(index)} settles`,
    listBlock(index),
    tableBlock(index),
    codeBlock(index),
    index % sequenceDiagramEvery === sequenceDiagramEvery - 1
      ? sequenceDiagram(index)
      : flowchart(index),
  ];
  if (decision !== undefined) {
    blocks.push(decisionBlock(decision, index));
  }
  const prose = sectionCharacters - blocks.join("\n\n").length;
  const head = proseHead(index);
  if (prose < head.length) {
    throw new Error(
      `section ${index + 1} leaves ${prose} characters for prose, under the ${head.length} its ` +
        "opening sentence spells: raise sectionCharacters or shorten the section's blocks"
    );
  }
  blocks[1] = padTo(head, proseFiller, prose);
  return blocks.join("\n\n");
}

/** A synthetic document of `sections` sections: one diagram each and `decisionBlocks` decision
 *  blocks spread through it, about `sections * 2,900` characters long. */
export function syntheticDocument(sections: number): string {
  const decisions = new Map<number, number>();
  for (let decision = 1; decision <= decisionBlocks; decision += 1) {
    decisions.set(decisionSection(decision, sections), decision);
  }
  const written: string[] = [`# A synthetic document of ${sections} sections`];
  for (let index = 0; index < sections; index += 1) {
    written.push(sectionText(index, decisions.get(index)));
  }
  return `${written.join("\n\n")}\n`;
}

/** How many Mermaid diagrams `markdown` holds. */
export function diagramsIn(markdown: string): number {
  return markdown.match(/^```mermaid$/gm)?.length ?? 0;
}

/** How many diagrams the comment body of acceptance A7 holds. */
export const commentDiagrams = 20;

/** How long each paragraph above a diagram in that body is. Long enough that a phone's viewport
 *  and the viewport height either side of it - the margin a diagram draws inside
 *  (`MERMAID_DRAW_MARGIN`) - reach only the first few diagrams, and short enough that twenty of
 *  them stay inside `DISPATCH_BODY_MAX`. */
const commentParagraphCharacters = 1_000;

/** A comment body of `diagrams` flowcharts, each under a paragraph of its own. No two are the
 *  same source, so none of them draws from another's cache entry. */
export function diagramComment(diagrams: number): string {
  const written: string[] = [];
  for (let index = 0; index < diagrams; index += 1) {
    written.push(
      padTo(proseHead(index), proseFiller, commentParagraphCharacters),
      flowchart(index)
    );
  }
  return `${written.join("\n\n")}\n`;
}

/** The document shaped like dispatch://AHXV6-1/artifact/intermediate-specs-md: about 81,000
 *  characters and 28 diagrams. */
export const smallDocument = syntheticDocument(smallSections);

/** The same document five times over: about 406,000 characters and 140 diagrams. */
export const largeDocument = syntheticDocument(largeSections);

/** What each document is called, in a row's title and in the numbers it prints. */
export const documentNames = {
  large: `the larger document (${largeSections} sections)`,
  small: `the AHXV6-1-shaped document (${smallSections} sections)`,
} as const;

/** How many diagrams each document holds, which is one per section. */
export const documentDiagrams = { large: largeSections, small: smallSections } as const;

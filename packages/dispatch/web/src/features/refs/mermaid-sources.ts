/**
 * One source per diagram type upstream `mermaid` registers.
 *
 * This is product data, not a fixture: `mermaid-upstream.ts` draws a type's entry once per
 * worker, and throws that drawing away, to pull the layout engine Mermaid would otherwise fetch
 * inside the render, where it would spend the reader's own five seconds. The browser rows and
 * the unit row read the same table, so the sources a reader's diagram is warmed with are the
 * ones the suite draws.
 *
 * Every id `mermaid.getRegisteredDiagramsMetadata()` answers is here except two that no reader
 * ever types: `error`, mermaid's own drawing for a source it could not parse, and `---`, the
 * front-matter marker. The unit row holds this table level with the library: it asks mermaid for
 * its ids and fails when one is registered that this table has no entry for, so a type a mermaid
 * upgrade adds cannot go undrawn.
 *
 * The entries are in the order mermaid registers the types in. Each `source` is the smallest
 * source of its type that draws something a reader can see, and every one but `info` carries the
 * same word, `Dispatch`, inside a label of the diagram's own, so one row can loop over the table
 * and look for one text. They were measured by rendering every entry through
 * `mermaid-upstream.ts`, beside this module; the browser rows read it from here, as they read
 * `theme/contrast.ts`, so every other dependency of this package still runs from `e2e/` into
 * `web/src` and none the other way.
 */

/** A source of one registered diagram type. `label` is the text a browser row looks for in the
 *  drawing, or `null` for the two types a row reads differently, each of which says how in the
 *  comment on its own entry. */
export interface MermaidSource {
  /** The registered diagram id, exactly as `getRegisteredDiagramsMetadata()` gives it. */
  id: string;
  source: string;
  label: string | null;
}

export const MERMAID_SOURCES: readonly MermaidSource[] = [
  {
    id: "flowchart-elk",
    source: 'flowchart-elk TD\n  a["Dispatch"] --> b["Done"]',
    label: "Dispatch",
  },
  {
    id: "mindmap",
    source: "mindmap\n  root((Dispatch))",
    label: "Dispatch",
  },
  {
    // A service of an architecture diagram is drawn as an icon from an icon pack, and Dispatch
    // registers none: registering one would fetch at render. Mermaid draws its own fallback
    // glyph in each service's place instead, in a blue of its own (`#087ebf`) that no palette
    // key reaches, so the drawing is incomplete by design and a row reads that it carries its
    // shapes rather than reading it as a finished picture.
    id: "architecture",
    source: "architecture-beta\n  group dispatch[Dispatch]\n  service web(server)[Web] in dispatch",
    label: null,
  },
  {
    id: "agentflow",
    source: 'agentflow-beta\n  a["Dispatch"] --> b["Drawn"]',
    label: "Dispatch",
  },
  {
    id: "c4",
    source: 'C4Context\n  title Dispatch\n  Person(reader, "Dispatch", "reads an issue")',
    label: "Dispatch",
  },
  {
    id: "kanban",
    source: "kanban\n  Doing\n    task[Dispatch]",
    label: "Dispatch",
  },
  {
    id: "classDiagram",
    source: "classDiagram\n  class Dispatch\n  Dispatch : +ship()",
    label: "Dispatch",
  },
  {
    id: "er",
    source: "erDiagram\n  Dispatch ||--o{ Issue : holds",
    label: "Dispatch",
  },
  {
    id: "gantt",
    source: "gantt\n  title Dispatch\n  section Ship\n  Draw :a1, 2026-01-01, 2d",
    label: "Dispatch",
  },
  {
    // The info diagram's only text is mermaid's own version string, which no source can change,
    // so a row reads that the drawing carries any text at all rather than looking for a word.
    id: "info",
    source: "info",
    label: null,
  },
  {
    id: "pie",
    source: 'pie title Dispatch\n  "Drawn" : 7\n  "Left" : 3',
    label: "Dispatch",
  },
  {
    id: "requirement",
    source:
      "requirementDiagram\n  requirement Dispatch {\n  id: 1\n  text: every type draws\n" +
      "  risk: low\n  verifymethod: test\n  }",
    label: "Dispatch",
  },
  {
    id: "sequence",
    source: "sequenceDiagram\n  Dispatch->>Worker: draw",
    label: "Dispatch",
  },
  {
    id: "swimlane",
    source: 'swimlane-beta TD\n  subgraph Dispatch\n    a["Drawn"]\n  end',
    label: "Dispatch",
  },
  {
    id: "flowchart-v2",
    source: 'flowchart TD\n  a["Dispatch"] --> b["Done"]',
    label: "Dispatch",
  },
  {
    id: "timeline",
    source: "timeline\n  title Dispatch\n  2026 : Drawn",
    label: "Dispatch",
  },
  {
    id: "gitGraph",
    source: 'gitGraph\n  commit id: "Dispatch"',
    label: "Dispatch",
  },
  {
    id: "stateDiagram",
    source: "stateDiagram-v2\n  [*] --> Dispatch\n  Dispatch --> [*]",
    label: "Dispatch",
  },
  {
    id: "journey",
    source: "journey\n  title Dispatch\n  section Draw\n    Render: 5: Worker",
    label: "Dispatch",
  },
  {
    id: "quadrantChart",
    source:
      "quadrantChart\n  title Dispatch\n  x-axis Low --> High\n  y-axis Small --> Large\n" +
      "  Drawn: [0.6, 0.6]",
    label: "Dispatch",
  },
  {
    id: "sankey",
    source: "sankey-beta\n\nDispatch,Drawn,7",
    label: "Dispatch",
  },
  {
    id: "packet",
    source: 'packet-beta\n  0-7: "Dispatch"\n  8-15: "Drawn"',
    label: "Dispatch",
  },
  {
    id: "xychart",
    source:
      'xychart-beta\n  title "Dispatch"\n  x-axis [one, two]\n  y-axis "drawn" 0 --> 10\n' +
      "  bar [3, 7]",
    label: "Dispatch",
  },
  {
    id: "block",
    source: 'block-beta\n  columns 2\n  a["Dispatch"] b["Drawn"]',
    label: "Dispatch",
  },
  {
    id: "eventmodeling",
    source: "eventmodeling\n  tf 1 cmd Dispatch\n",
    label: "Dispatch",
  },
  {
    id: "treeView",
    source: "treeView-beta\n  Dispatch\n    Drawn",
    label: "Dispatch",
  },
  {
    id: "radar",
    source: 'radar-beta\n  axis one, two, three\n  curve d["Dispatch"]{1, 2, 3}',
    label: "Dispatch",
  },
  {
    id: "ishikawa",
    source: "ishikawa-beta\nDispatch\n  Drawing\n    Slow",
    label: "Dispatch",
  },
  {
    id: "treemap",
    source: 'treemap-beta\n"Dispatch"\n  "Drawn": 7\n  "Left": 3',
    label: "Dispatch",
  },
  {
    id: "railroad",
    source: 'railroad-beta\nDispatch = terminal("Dispatch");',
    label: "Dispatch",
  },
  {
    id: "railroadEbnf",
    source: 'railroad-ebnf-beta\nDispatch = "Dispatch";',
    label: "Dispatch",
  },
  {
    id: "railroadAbnf",
    source: 'railroad-abnf-beta\nDispatch = "Dispatch";',
    label: "Dispatch",
  },
  {
    id: "railroadPeg",
    source: 'railroad-peg-beta\nDispatch <- "Dispatch";',
    label: "Dispatch",
  },
  {
    id: "venn",
    source: 'venn-beta\n  set a["Dispatch"]: 7\n  set b["Drawn"]: 5',
    label: "Dispatch",
  },
  {
    id: "wardley",
    source: "wardley-beta\n  component Dispatch [0.6, 0.6]",
    label: "Dispatch",
  },
  {
    id: "cynefin",
    source: 'cynefin-beta\n  clear\n    "Dispatch"',
    label: "Dispatch",
  },
  {
    id: "usecase",
    source: "usecase-beta\n  actor Dispatch\n  Dispatch --> Draw",
    label: "Dispatch",
  },
];

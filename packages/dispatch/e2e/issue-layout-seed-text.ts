// The markdown `e2e/issue-layout-seed.ts` writes. One builder per document, so the whole issue,
// the request baseline and the twice-the-nodes issue write the same shapes at different sizes.
// Every typed block carries an explicit `#id`, so a seed names the ids its tests read rather than
// looking one up by its text.

/**
 * The three sentences a seeded spec puts at its head. Together with the block's own fence they
 * are well past the 240 runes the server cuts a block's stored excerpt to
 * (`packages/envoy/internal/dispatch/refs/graph.go`), and they hold characters outside ASCII,
 * so a head that read the excerpt rather than the document's own text at the block's span
 * would draw a shortened third sentence and fail c-three-things.
 */
export const SUMMARY_SENTENCES = [
  "The issue page draws one ordered body in place of eight tabs.",
  "Every property that carries a value is visible at every width.",
  "The reasoning behind each requirement is reachable without leaving the page, because every " +
    "node the issue holds — its documents, its decisions, its checks, its plan tasks and the " +
    "results recorded against them — is a row of the one tree every preset is a query over.",
] as const;

/** The block id of those three sentences. `callout` is the nearest thing today's block schema
 *  holds to a summary node (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:1-19`,
 *  read 2026-11-05: the five types are `callout`, `ask`, `requirement`, `plan_task` and
 *  `check`). */
export const SUMMARY_BLOCK_ID = "summary";

/** The block id of the `ask` block a seeded spec carries, which the server indexes into the
 *  issue's open question. */
export const OPEN_QUESTION_BLOCK_ID = "q-open";

/** How a seeded check's newest result stands, in the words `VerificationTab.tsx:29-43` draws
 *  today, plus `no-control`: a pass on a before-merge run that records no negative control,
 *  which is one of the five gaps `c-chain` names and reads `passed` on the page. */
export const CHECK_STATE_ORDER = [
  "passed",
  "failed",
  "no-result",
  "older",
  "unmarked",
  "no-control",
] as const;

export type SeededCheckState = (typeof CHECK_STATE_ORDER)[number];

/** The state check `#c<index>` is seeded in: the six states in order, repeating for a document
 *  that holds more than six checks. */
export function checkStateOf(index: number): SeededCheckState {
  return CHECK_STATE_ORDER[(index - 1) % CHECK_STATE_ORDER.length];
}

export function requirementBlockId(index: number): string {
  return `r${index}`;
}

export function checkBlockId(index: number): string {
  return `c${index}`;
}

export function planTaskBlockId(index: number): string {
  return `t${index}`;
}

/** The block id of the requirement whose own words cite a document of the issue (R16). The
 *  citation sits inside a typed block, because a document a block cites is a node under that
 *  block and a paragraph of a document is the document's own text and no block of the tree at
 *  all. A requirement is the typed block that carries it: a requirement whose `from` is absent
 *  sits under the issue, so the document it cites is drawn one level under that, inside the
 *  three levels the fold draws. */
export const CITATION_BLOCK_ID = "r-cites";

/** The words of that requirement, which name one document of `issueKey` by its address. */
export function citationText(issueKey: string, slug: string): string {
  return `The measurements behind the first requirement are in dispatch://${issueKey}/artifact/${slug}.`;
}

/** That requirement as markdown, for a seed to insert once the document it cites exists. */
export function citationRequirement(issueKey: string, slug: string): string {
  return `:::requirement{#${CITATION_BLOCK_ID}}\n${citationText(issueKey, slug)}\n:::`;
}

/** The `dispatch://` address of one block of an issue's spec. */
export function specBlockRef(issueKey: string, blockId: string): string {
  return `dispatch://${issueKey}/spec#b-${blockId}`;
}

/** `1` to `count`. */
function counted(count: number): number[] {
  return Array.from({ length: count }, (_, index) => index + 1);
}

/** Requirement `#r1` holds two sentences and more than 100 characters, so a test can read a row's
 *  first line against the stored text; every other requirement holds one short sentence. */
export function requirementText(index: number): string {
  if (index === 1) {
    return (
      "Every property of the issue that carries a value is visible at every width the page draws, " +
      "with no control opened and no sideways scroll. The right-hand panel holds the rest."
    );
  }
  return `Requirement ${index} of the redesign stands on a row of its own.`;
}

export function checkText(index: number): string {
  return `Check ${index} of the redesign holds its own row and its own state.`;
}

/** The words check `#c<index>` carries after the seed rewords it, which makes every result
 *  recorded against its earlier words read `older`. */
export function rewordedCheckText(index: number): string {
  return `Check ${index} of the redesign holds its own row, its own state and its own shape.`;
}

export function planTaskText(index: number): string {
  return `Task ${index} of the redesign is one step of the build.`;
}

/** The decisions `decisions.md` holds, in document order, the last one replaced. The kinds are
 *  the server's: a decision reads `person` when its blocks mention an ask and `agent` otherwise,
 *  `unsure` from a paragraph that starts `Unsure:`, and `replaced` from sitting under the section
 *  headed exactly `## Replaced` (`packages/envoy/internal/dispatch/api/decisions.go:25-28`). */
function seededDecisions(options: { askRef: string; issueKey: string }): {
  body: string;
  heading: string;
  replaced?: boolean;
}[] {
  const block = (index: number) => specBlockRef(options.issueKey, requirementBlockId(index));
  return [
    {
      body: `Chosen so the page makes no request per node. Shapes ${block(1)} and ${block(2)}.`,
      heading: "Draw the tree from one read",
    },
    {
      body: `Decided in ${options.askRef}. Shapes ${block(3)}.`,
      heading: "Keep every address the old page answered",
    },
    {
      body: "Unsure: a reader who never finds the newest question would change this.",
      heading: "Fold to the frontier and not to a depth",
    },
    {
      body: "A word and a shape carry every state, so no reader depends on a colour.",
      heading: "Name every state with a word and a shape",
    },
    {
      body: `One panel holds the properties and the conversation. Shapes ${block(4)}.`,
      heading: "Put every remaining property in one panel",
    },
    {
      body: `A document is a node under the block that cites it. Shapes ${block(5)}.`,
      heading: "Draw a document under the block that cites it",
    },
    {
      body: "Replaced by the tree above: the rail hid 744 px at 1440 px.",
      heading: "Keep the metadata rail behind a sideways scroll",
      replaced: true,
    },
  ];
}

/** The heading markdown of each decision a document of `count` decisions holds, in document
 *  order. A decision is addressed by its heading block, so this is what resolves its block id. */
export function decisionHeadings(count: number): string[] {
  return seededDecisions({ askRef: "", issueKey: "" })
    .slice(0, count)
    .map((decision) => `${decision.replaced === true ? "###" : "##"} ${decision.heading}`);
}

export function decisionsDocument(options: {
  askRef: string;
  count: number;
  issueKey: string;
}): string {
  const decisions = seededDecisions(options).slice(0, options.count);
  const sections = decisions
    .filter((decision) => decision.replaced !== true)
    .map((decision) => `## ${decision.heading}\n\n${decision.body}`);
  const replaced = decisions.filter((decision) => decision.replaced === true);
  if (replaced.length > 0) {
    const bodies = replaced.map((one) => `### ${one.heading}\n\n${one.body}`).join("\n\n");
    sections.push(`## Replaced\n\n${bodies}`);
  }
  return `${sections.join("\n\n")}\n`;
}

function summaryCallout(): string {
  const sentences = SUMMARY_SENTENCES.join(" ");
  return `:::callout{#${SUMMARY_BLOCK_ID} title="What this issue is"}\n${sentences}\n:::`;
}

function openQuestionBlock(): string {
  return `:::ask{#${OPEN_QUESTION_BLOCK_ID} urgency="med"}
At which width does the identity block condense?

- 1024 px: the phone layout begins there anyway.
- 1280 px: the preset row keeps its labels above that width.
:::`;
}

/** The issue's spec: its three sentences, its requirements, and the `ask` block the server
 *  indexes into an open question two levels below the issue (issue, document, block). */
export function specDocument(options: {
  openQuestion: boolean;
  requirements: number;
  summary: boolean;
}): string {
  const blocks: string[] = [];
  if (options.summary) blocks.push(summaryCallout());
  for (const index of counted(options.requirements)) {
    blocks.push(`:::requirement{#${requirementBlockId(index)}}\n${requirementText(index)}\n:::`);
  }
  if (options.openQuestion) blocks.push(openQuestionBlock());
  return `${blocks.join("\n\n")}\n`;
}

/** The issue's Verification document: `checks` `check` blocks, each serving one requirement in
 *  turn and each seeded in the state `checkStateOf` names. The one in `no-control` carries no
 *  `negative_control` attribute at all, which is what "records no negative control" is. */
export function verificationDocument(options: {
  checks: number;
  issueKey: string;
  requirements: number;
}): string {
  const blocks = counted(options.checks).map((index) => {
    const served = ((index - 1) % options.requirements) + 1;
    const serves = specBlockRef(options.issueKey, requirementBlockId(served));
    const control =
      checkStateOf(index) === "no-control"
        ? ""
        : ' negative_control="Draw a row with no state and the test fails."';
    return (
      `:::check{#${checkBlockId(index)} serves="${serves}" surface="browser"` +
      ` run="bun run e2e -- e2e/issue-layout.e2e.ts" when="before_merge"${control}}\n` +
      `${checkText(index)}\n:::`
    );
  });
  return `How the issue page is proved.\n\n${blocks.join("\n\n")}\n`;
}

/** The issue's plan: `planTasks` `plan_task` blocks. Every task but the last serves one
 *  requirement, and tasks 2 to 7 need the task before them, which is the 14 pointers the Plan tab
 *  of `dispatch://INFRA-1196/artifact/request-baseline-md` counts one reference read for. */
export function planDocument(options: {
  issueKey: string;
  planTasks: number;
  requirements: number;
}): string {
  const taskRef = (index: number) =>
    `dispatch://${options.issueKey}/artifact/plan-md#b-${planTaskBlockId(index)}`;
  const blocks = counted(options.planTasks).map((index) => {
    const attributes: string[] = [];
    if (index < options.planTasks) {
      const served = ((index - 1) % options.requirements) + 1;
      attributes.push(`serves="${specBlockRef(options.issueKey, requirementBlockId(served))}"`);
    }
    if (index >= 2 && index <= 7) attributes.push(`needs="${taskRef(index - 1)}"`);
    const head = attributes.length === 0 ? "" : ` ${attributes.join(" ")}`;
    return `:::plan_task{#${planTaskBlockId(index)}${head}}\n${planTaskText(index)}\n:::`;
  });
  return `How the issue page gets built.\n\n${blocks.join("\n\n")}\n`;
}

/** The ids of the four requirements `seedSpineCases` writes. */
export const SPINE_REQUIREMENTS = {
  servedTwiceA: "r-served-a",
  servedTwiceB: "r-served-b",
  shapedElsewhere: "r-shaped-elsewhere",
  unexplained: "r-unexplained",
} as const;

/** The heading of the one decision `seedSpineCases` writes, which is the block the decisions read
 *  answers it under. */
export const SPINE_DECISION_HEADING = "## Link a requirement the question does not";

/** The block id of the check `seedSpineCases` writes. */
export const SPINE_CHECK_BLOCK_ID = "c-two";

export const SPINE_SPEC = `:::requirement{#${SPINE_REQUIREMENTS.unexplained}}
Nothing explains this requirement: it names no node it came from.
:::

:::requirement{#${SPINE_REQUIREMENTS.shapedElsewhere}}
A decision links this requirement while the answered question stands elsewhere.
:::

:::requirement{#${SPINE_REQUIREMENTS.servedTwiceA}}
The first requirement one check serves.
:::

:::requirement{#${SPINE_REQUIREMENTS.servedTwiceB}}
The second requirement that same check serves.
:::
`;

/** The spine's `decisions.md`: one decision whose block links `shapedElsewhere` while the ask it
 *  names is the answered question a reader would read as that requirement's origin. */
export function spineDecisionsDocument(issueKey: string, askId: string): string {
  return (
    `${SPINE_DECISION_HEADING}\n\n` +
    `Shapes ${specBlockRef(issueKey, SPINE_REQUIREMENTS.shapedElsewhere)}, while ` +
    `dispatch://${issueKey}/ask/${askId} is the question that was answered.\n`
  );
}

/** The spine's Verification document: one check whose `serves` names two requirements. */
export function spineVerificationDocument(issueKey: string): string {
  const serves = [SPINE_REQUIREMENTS.servedTwiceA, SPINE_REQUIREMENTS.servedTwiceB]
    .map((requirement) => specBlockRef(issueKey, requirement))
    .join(" ");
  return (
    `How the spine is proved.\n\n:::check{#${SPINE_CHECK_BLOCK_ID} serves="${serves}" ` +
    `surface="browser" run="bun run e2e -- e2e/issue-layout.e2e.ts" ` +
    `negative_control="Draw the check under one parent and the test fails."}\n` +
    "One check serves two requirements and is drawn under both.\n:::\n"
  );
}

/**
 * The issue page's structural vocabulary: what it calls each property of an issue, what it calls
 * each section of its panel, the sentences it draws where a tooltip used to hold the meaning,
 * and the two marks it divides its own words from an author's with (dispatch://INFRA-1196 R11,
 * R21).
 *
 * It sits beside `page-words.ts`, which names the things the page draws - a node's type, its
 * state, a preset, a control - and derives `DECLARED_WORDS` from both modules, so a label added
 * here joins the page's declared vocabulary by itself. Nothing here imports that module, which
 * is why the derivation can run one way.
 *
 * This module is data. It holds no React, touches no DOM and imports nothing.
 */

/**
 * Freezes `value` and everything it holds. Each declaration in this module and in `page-words.ts`
 * is `as const`, which is the compiler's half of the promise; this is the runtime half, so no
 * caller can write a word into the page's vocabulary after the module loads.
 */
export function freeze<T>(value: T): T {
  for (const held of Object.values(value as Record<string, unknown>)) {
    if (typeof held === "object" && held !== null) {
      freeze(held);
    }
  }
  return Object.freeze(value);
}

/** The attribute every element carries whose visible text is the page's own vocabulary. */
export const PAGE_CHROME_ATTRIBUTE = "data-page-chrome";

/** The attribute every element carries whose visible text came from a person or an agent. */
export const AUTHOR_TEXT_ATTRIBUTE = "data-author-text";

/** The mark an element wears whose text is the page's own vocabulary: `<span {...pageChrome}>`,
 *  so the attribute is spelled once. */
export const pageChrome: Readonly<Record<string, string>> = freeze({
  [PAGE_CHROME_ATTRIBUTE]: "",
});

/** The same, for text a person or an agent wrote. */
export const authorText: Readonly<Record<string, string>> = freeze({
  [AUTHOR_TEXT_ATTRIBUTE]: "",
});

/**
 * The name of every property the page draws beside its value, in the identity block and in the
 * panel's sections: the fewest plain words that read on their own, because the value beside it
 * is the author's and carries `AUTHOR_TEXT_ATTRIBUTE`.
 */
export const PROPERTY_LABELS = freeze({
  assignee: "Assignee",
  /** The issues this one waits on (`blocked_by`). */
  blocked_by: "Blocked by",
  /** Who is working the issue, which is not where its messages go. */
  claim: "Claimed by",
  components: "Components",
  /** The issue's links out of Dispatch, such as a pull request. */
  external_links: "Links",
  /** The heading of the panel's facts of what the focus is on (R12). */
  focused_node: "What the focus is on",
  labels: "Labels",
  opened_by: "Opened by",
  parent: "Parent",
  priority: "Priority",
  referenced_by: "Referenced by",
  repository: "Repository",
  /** Where a message on this issue goes, said rather than named (R11). */
  route: "Messages go to",
  status: "Status",
  subscribers: "Subscribers",
  /** Row three's waiting marker: whose turn it is on the open asks. */
  waiting_on_agents: "Waiting on agents",
  waiting_on_you: "Waiting on you",
} as const);

/** The panel's three property sections (R12), in the order it draws them. */
export const MARGIN_SECTION_ORDER = freeze(["development", "people", "references"] as const);

/** One section of the panel's properties, by its own token, which is its `data-margin-section`. */
export type MarginSection = (typeof MARGIN_SECTION_ORDER)[number];

/** The name of each panel section: what the properties inside it are about. */
export const MARGIN_SECTION_NAMES = freeze({
  development: "Development",
  people: "People",
  references: "References",
} as const satisfies Record<MarginSection, string>);

/**
 * What the page says in full where a tooltip used to hold the meaning: a phone raises no
 * tooltip, so each of these is drawn as text beside the control it explains (R11).
 */
export const HELP_LINES = freeze({
  /** What the identity block says of a closed issue. */
  closed: "This issue is closed.",
  /** Why releasing is safe, beside the release control. */
  release_claim: "Releasing hands the issue back, so another agent can take it.",
  /** What a message route is, and is not, beside the route editor. */
  route:
    "New asks, comments and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim.",
} as const);

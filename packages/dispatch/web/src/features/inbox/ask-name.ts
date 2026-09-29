/**
 * What an Inbox row's controls are called, and how two rows on one owner are told apart.
 *
 * Every name on a row is derived from one place - which thing the ask belongs to (`askOwner`) -
 * so the checkbox, the Snooze control and the grouping that counts twins cannot disagree.
 */

/** What the naming rules read off a row. */
export interface NamedAsk {
  readonly id: string;
  readonly question: string;
  readonly issue?: { readonly key: string };
  readonly issue_key?: string | null;
  readonly document?: { readonly project: string; readonly slug: string; readonly name: string };
}

/** How much of a question a control's name can carry before it stops being a name. */
const CONTROL_NAME_QUESTION_CHARS = 48;

/** An ask the server listed without an issue and without a document: a row the reader can still
 *  see, so its controls still need a name. */
const UNOWNED_LABEL = "Document ask";

/** Where an ask sits among the rows of its own owner, when that owner has more than one listed. */
export interface AskOrdinal {
  index: number;
  total: number;
}

/** Which thing a row belongs to. */
export interface AskOwner {
  /** Identity: two asks share an owner only if they share this. A document has no issue key, so
   *  it is keyed by its own route. */
  key: string;
  /** What a control calls it: the issue key, or the document's name. */
  label: string;
}

/** The issue a row belongs to, by key, or `null` for a document ask. `issue` carries the title
 *  and assignee the row renders; `issue_key` is what a read without that object carries. */
export function askIssueKey(ask: NamedAsk): string | null {
  return ask.issue?.key ?? ask.issue_key ?? null;
}

/** The one definition of what a row belongs to, read by the name and by the grouping. */
export function askOwner(ask: NamedAsk): AskOwner {
  const issue = askIssueKey(ask);
  if (issue !== null) return { key: `issue:${issue}`, label: issue };
  if (ask.document !== undefined) {
    return {
      key: `document:${ask.document.project}/${ask.document.slug}`,
      label: ask.document.name,
    };
  }
  return { key: "none", label: UNOWNED_LABEL };
}

/**
 * The position of each ask among the rows sharing its owner, in the order the list shows them,
 * for the owners that have more than one. One ask on an issue needs no ordinal, which keeps the
 * common name short.
 */
export function askOrdinals(rows: readonly NamedAsk[]): ReadonlyMap<string, AskOrdinal> {
  const byOwner = new Map<string, string[]>();
  for (const row of rows) {
    const key = askOwner(row).key;
    const ids = byOwner.get(key);
    if (ids === undefined) byOwner.set(key, [row.id]);
    else ids.push(row.id);
  }
  const ordinals = new Map<string, AskOrdinal>();
  for (const ids of byOwner.values()) {
    if (ids.length < 2) continue;
    for (const [at, id] of ids.entries()) {
      ordinals.set(id, { index: at + 1, total: ids.length });
    }
  }
  return ordinals;
}

/**
 * What the row's controls are called. Two open asks on one issue are two different questions, so
 * a name taken from the owner alone says the same thing twice - `Select CORE-12` beside `Select
 * CORE-12` - and a screen reader gives the reader nothing to choose between. The owner answers
 * "which issue"; the start of the question answers "which ask", and where an issue has several
 * asks the position carries that on its own, since two questions can agree for as long as they
 * like and the question is cut to a name's length.
 *
 * Every ask has a question: the server refuses a blank one on both paths it can be created by
 * (`api/asks.go`'s `INVALID_ASK`, `docs/ask_blocks.go` for a document block), so the name always
 * has something after its colon.
 */
export function controlName(ask: NamedAsk, ordinal: AskOrdinal | undefined): string {
  const where = askOwner(ask).label;
  const which =
    ordinal === undefined ? where : `${where}, ask ${ordinal.index} of ${ordinal.total}`;
  const question = ask.question.replace(/\s+/g, " ").trim();
  const short =
    question.length > CONTROL_NAME_QUESTION_CHARS
      ? `${question.slice(0, CONTROL_NAME_QUESTION_CHARS).trimEnd()}…`
      : question;
  return `${which}: ${short}`;
}

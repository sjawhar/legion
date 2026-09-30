/**
 * Hiding issues from an agent under evaluation (`proxy.ts --exclude-key`). Every answer the proxy
 * serves passes through `withoutExcluded`, which drops what an excluded issue owns, and then
 * `assertNothingExcluded`, which fails closed: an excluded key left anywhere the filter does not
 * handle stops the answer instead of reaching the agent.
 */

/** A Dispatch issue key; its characters need no escaping inside a regular expression. */
export const ISSUE_KEY = /^[A-Z][A-Z0-9]*-\d+$/;

/** An excluded key left where the filter does not look, at `at` (a JSON path). */
export class ExcludedIssueLeak extends Error {
  constructor(readonly at: string) {
    super(`an excluded issue appears at ${at}, where the exclusion filter does not handle it`);
  }
}

/** The eval proxy's one record guard: every module that walks an unknown JSON value uses it. */
export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringField(value: unknown, key: string): string | undefined {
  if (!isRecord(value)) return undefined;
  const field = value[key];
  return typeof field === "string" ? field : undefined;
}

/** Whether a reference names the key: the key itself, `KEY/<slug>` (a document's `ref_key`),
 *  `dispatch://KEY...`, or a path through `/issues/KEY`. */
export function refNamesKey(ref: string, key: string): boolean {
  return new RegExp(`(^|^dispatch://|/issues/)${key}($|[/?#@])`).test(ref);
}

/** Whether free text mentions the key as a word, as an error message naming it does. */
export function textNamesKey(text: string, key: string): boolean {
  return new RegExp(`(^|[^A-Za-z0-9-])${key}($|[^0-9])`).test(text);
}

/**
 * Whether one list element belongs to an excluded issue, by the fields Dispatch's list shapes
 * carry: an issue row, child or component issue (`key`), anything issue-owned (`issue_key`), a
 * parent's child event (`payload.child_key`), a search hit or open ask (`owner`), a legacy
 * search hit (`issue`), a reference edge (`node`), a reference member (`artifact`), and any deep
 * link (`ref`, `href`).
 */
function ownedByExcluded(element: unknown, excluded: ReadonlySet<string>): boolean {
  if (!isRecord(element)) return false;
  const owns = (key: string | undefined) => key !== undefined && excluded.has(key);
  if (owns(stringField(element, "key")) || owns(stringField(element, "issue_key"))) return true;
  if (owns(stringField(element.payload, "child_key"))) return true;
  const owner = element.owner;
  if (isRecord(owner)) {
    if (owner.kind === "issue" && owns(stringField(owner, "key"))) return true;
    if (owns(stringField(owner.issue, "key"))) return true;
  }
  if (owns(stringField(element.issue, "key"))) return true;
  const node = element.node;
  if (isRecord(node)) {
    if (owns(stringField(node, "issue_key"))) return true;
    if (node.kind === "issue" && owns(stringField(node, "id"))) return true;
  }
  if (owns(stringField(element.artifact, "issue_key"))) return true;
  for (const link of [stringField(element, "ref"), stringField(element, "href")]) {
    if (link !== undefined && [...excluded].some((key) => refNamesKey(link, key))) return true;
  }
  return false;
}

/** Fields that point at one issue by key: a `parent`, or the ancestor an issue inherits its
 *  components from. Pointing at an excluded issue, they read null, as a root's do. */
const ISSUE_POINTERS: Readonly<Record<string, true>> = { parent: true, inherited_from: true };

/** Drops every array element an excluded issue owns, at any depth, and nulls every pointer at
 *  one. */
export function withoutExcluded(value: unknown, excluded: ReadonlySet<string>): unknown {
  if (excluded.size === 0) return value;
  if (Array.isArray(value)) {
    return value
      .filter((element) => !ownedByExcluded(element, excluded))
      .map((element) => withoutExcluded(element, excluded));
  }
  if (!isRecord(value)) return value;
  const result: Record<string, unknown> = {};
  for (const [field, inner] of Object.entries(value)) {
    result[field] =
      Object.hasOwn(ISSUE_POINTERS, field) && typeof inner === "string" && excluded.has(inner)
        ? null
        : withoutExcluded(inner, excluded);
  }
  return result;
}

/**
 * Fails closed: throws `ExcludedIssueLeak` when any string in the answer is an excluded key or a
 * reference naming one, or any object key is one. Mentions inside longer text are left alone.
 */
export function assertNothingExcluded(
  value: unknown,
  excluded: ReadonlySet<string>,
  at = "$"
): void {
  if (excluded.size === 0) return;
  if (typeof value === "string") {
    if ([...excluded].some((key) => refNamesKey(value, key))) throw new ExcludedIssueLeak(at);
    return;
  }
  if (Array.isArray(value)) {
    for (const [index, element] of value.entries()) {
      assertNothingExcluded(element, excluded, `${at}[${index}]`);
    }
    return;
  }
  if (!isRecord(value)) return;
  for (const [field, inner] of Object.entries(value)) {
    if ([...excluded].some((key) => refNamesKey(field, key))) throw new ExcludedIssueLeak(at);
    assertNothingExcluded(inner, excluded, `${at}.${field}`);
  }
}

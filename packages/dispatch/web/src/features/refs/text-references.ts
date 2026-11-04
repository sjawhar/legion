import { itemOfBlock } from "./block-reference";
import { referenceSpans } from "./reference-spans";
import {
  buildDispatchReference,
  type DispatchReferenceRoute,
  referenceRouteFromHref,
} from "./routes";
import { type ReferenceTargetKind, referenceTargetKind, routeInBound } from "./target-id";

/**
 * The dashboard's one reader of what a text cites, the twin of the server's `text.Mentions`
 * (`packages/envoy/internal/dispatch/text/mentions.go`), tested row for row against the table
 * both share (`DISPATCH_TEXT_REFERENCES` in `@legion/contracts`, whose header states the whole
 * grammar). Everything on the web that turns words into references reads through it: the link
 * pass over a rendered body (`RefLink.tsx`), the unfurl cards, and the document editors' host
 * scanner. There is no second reading of a bare key, a `#id` or an id prefix anywhere.
 *
 * It is pure: it takes the text and what is already known about it - the server origin, which
 * issue keys exist, the question map of the text's document, and the items a prefix resolved to -
 * and answers every reference the text holds. For a caller that has asked the server nothing yet
 * it answers, from the same pass, exactly what it would have to ask (`TextReferenceNeeds`), so a
 * hook starts those queries and no others.
 */

/** How a text wrote a reference: a `dispatch://` reference or a dashboard address, an issue key
 *  on its own, or `#` and a question block's id. */
export type TextReferenceForm = "address" | "key" | "question";

/** One reference a text holds, where it holds it. */
export interface TextReference {
  /** Where the reference starts in the text, and where it ends: `text.slice(start, end)` is what
   *  the author wrote, which is what a link wraps and what a title falls back to. */
  readonly start: number;
  readonly end: number;
  /** The reference as written. */
  readonly text: string;
  readonly form: TextReferenceForm;
  /** The reference it names: `undefined` for a key no issue holds and a `#id` no question
   *  declares, which name nothing. An address whose id is a prefix nothing resolved is the
   *  reference as written, as the server stores it. */
  readonly reference: string | undefined;
  readonly route: DispatchReferenceRoute | undefined;
  /** Whether what it names is still open: an unknown key, an unknown question, or a prefix no
   *  item has been resolved for. A caller that has asked the server nothing sees every candidate
   *  unresolved. */
  readonly unresolved: boolean;
}

/** What a reader of this text would have to ask the server before it can resolve everything:
 *  one query per entry and no more. */
export interface TextReferenceNeeds {
  /** The distinct issue keys the text writes bare, in order of first appearance. */
  readonly keys: readonly string[];
  /** Whether the text holds a `#id`, which asks for the question map of its document. */
  readonly questions: boolean;
  /** The distinct references whose item id or block id is written as a prefix, as the text
   *  writes them, in order of first appearance. */
  readonly prefixes: readonly string[];
}

/** The document a text belongs to, which its `#id`s resolve against: an issue, whose spec is
 *  the scope, or a document by its own id. It is the margin's own owner (`MarginOwner`), so a
 *  surface inside the margin passes the owner it already holds; `project` and `slug` are that
 *  owner's, and name the address an ask written on a project document is referenced by, having
 *  no issue to be addressed under. Text of no document - an agent's conversation, a broadcast,
 *  a search hit - names none and gets no `#id` links, which is right: a `#id` there declares
 *  nothing. */
export type TextSource =
  | { readonly kind: "issue"; readonly key: string }
  | {
      readonly kind: "document";
      readonly artifactId: string;
      readonly project?: string;
      readonly slug?: string;
    };

/** The source of a text an issue owns - an ask's question, a message, an activity line: that
 *  issue, whose spec its `#id`s name a question block of. None where there is no issue, which a
 *  project document's own item has; a caller showing one names that document instead. */
export function issueSource(key: string | null | undefined): TextSource | undefined {
  return key === null || key === undefined || key === "" ? undefined : { key, kind: "issue" };
}

/** What a caller already knows about the text. Each is read only where the text holds a
 *  candidate for it; everything absent leaves its candidates unresolved. */
export interface TextReferenceFacts {
  /** The dashboard's own origin, which its URLs are read against; the page's by default. */
  readonly origin?: string;
  /** The issue keys that name an issue that exists. */
  readonly issues?: readonly string[];
  /** The question map of the document the text belongs to: each question block id to the
   *  reference of the ask it declares. */
  readonly questions?: Readonly<Record<string, string>>;
  /** Each whole id a prefix resolved to, to the reference that item is stored under. */
  readonly items?: Readonly<Record<string, string>>;
}

export interface TextReading {
  /** Every reference the text holds, in the order it holds them. */
  readonly references: readonly TextReference[];
  readonly needs: TextReferenceNeeds;
}

/** An id written short: a hex digit and seven or more hex digits and hyphens, read without case
 *  (the server's `text.IsIDPrefix`). A whole uuid is no prefix - an exact id needs no resolving
 *  and always wins. */
const idPrefixPattern = /^[0-9a-f][0-9a-f-]{7,}$/i;
const uuidPattern =
  /^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;

export function isIdPrefix(value: string): boolean {
  return idPrefixPattern.test(value) && !uuidPattern.test(value);
}

/** The characters of a question block's id, as an `:::ask{#id}` line declares it (the server's
 *  `text.IsBlockIDByte`). */
const blockIdCharacter = /[A-Za-z0-9_-]/;

/** The whitespace that ends a reference, spelled out rather than written `\s`: JavaScript's
 *  `\s` also matches a vertical tab and U+FEFF, where the server's matches neither. */
const referenceSpace = /[\t\n\f\r\p{Z}]/u;

/** The marks a bare key or `#id` may stand right after: what opens a parenthesis, a link's text,
 *  a quote, emphasis, a quote block, a code span, strikethrough or a table cell. */
const mentionOpeners = "([\"'*_>`~|";

/** The character ending at `at`, read whole where a surrogate pair writes it; `""` at the start
 *  of the text. */
function characterBefore(text: string, at: number): string {
  if (at <= 0) {
    return "";
  }
  const low = text.charCodeAt(at - 1);
  if (at >= 2 && low >= 0xdc00 && low <= 0xdfff) {
    const high = text.charCodeAt(at - 2);
    if (high >= 0xd800 && high <= 0xdbff) {
      return text.slice(at - 2, at);
    }
  }
  return text.charAt(at - 1);
}

/** The character starting at `at`, read whole where a surrogate pair writes it; `""` at the end
 *  of the text. */
function characterAt(text: string, at: number): string {
  const code = text.codePointAt(at);
  return code === undefined ? "" : String.fromCodePoint(code);
}

function isLetterOrDigit(character: string): boolean {
  return character !== "" && /[\p{L}\p{N}]/u.test(character);
}

/**
 * Whether `prefix` ends with a numeric character reference, as CommonMark reads one (`&#` and 1
 * to 7 decimal digits, or `&#x` and 1 to 6 hex digits, then `;`), to a character that ends a
 * reference: stored markdown writes the space at a mark's edge that way.
 */
function endsWithWhitespaceReference(prefix: string): boolean {
  const window = prefix.slice(Math.max(0, prefix.length - 11), prefix.length - 1);
  const amp = window.lastIndexOf("&#");
  if (amp < 0) {
    return false;
  }
  let digits = window.slice(amp + 2);
  let base = 10;
  let limit = 7;
  if (digits.startsWith("x") || digits.startsWith("X")) {
    digits = digits.slice(1);
    base = 16;
    limit = 6;
  }
  if (digits === "" || digits.length > limit || !/^[0-9a-fA-F]+$/.test(digits)) {
    return false;
  }
  if (base === 10 && !/^[0-9]+$/.test(digits)) {
    return false;
  }
  const value = Number.parseInt(digits, base);
  return value <= 0x10ffff && referenceSpace.test(String.fromCodePoint(value));
}

/** Whether a bare key or `#id` may start at `at`: the start of the text, whitespace, a numeric
 *  character reference to whitespace, or one of the marks a mention may open against. */
function isMentionStart(text: string, at: number): boolean {
  if (at === 0) {
    return true;
  }
  const before = characterBefore(text, at);
  if (mentionOpeners.includes(before)) {
    return true;
  }
  if (before === ";") {
    return endsWithWhitespaceReference(text.slice(0, at));
  }
  return referenceSpace.test(before);
}

/** Whether a key running up to `end` stands alone on its right: at the end of the text, or
 *  before a character that is not a letter, a digit, `-`, `/`, `#` or `@`. The sentence
 *  punctuation and the emphasis and strikethrough delimiters end it only when no letter or digit
 *  follows them, so `CORE-1's` and `_CORE-1_.` read CORE-1, and `CORE-1.5`, `CORE-1_x`,
 *  `CORE-1/spec` and `CORE-1#q1` read nothing. */
function keyEnds(text: string, end: number): boolean {
  if (end === text.length) {
    return true;
  }
  const next = characterAt(text, end);
  if ("-/#@".includes(next)) {
    return false;
  }
  if (".,:;_*~".includes(next)) {
    const after = end + next.length;
    return after === text.length || !isLetterOrDigit(characterAt(text, after));
  }
  return !isLetterOrDigit(next);
}

/** Where the issue key starting at `start` ends (`[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*`), or
 *  `undefined` when none starts there or what follows does not end it. */
function keyAt(text: string, start: number): number | undefined {
  let end = start + 1;
  while (end < text.length && /[A-Z0-9]/.test(text.charAt(end))) {
    end += 1;
  }
  const letters = end - start;
  if (
    letters < 2 ||
    letters > 10 ||
    end + 1 >= text.length ||
    text.charAt(end) !== "-" ||
    !/[1-9]/.test(text.charAt(end + 1))
  ) {
    return undefined;
  }
  end += 2;
  while (end < text.length && /[0-9]/.test(text.charAt(end))) {
    end += 1;
  }
  return keyEnds(text, end) ? end : undefined;
}

/** One candidate a text writes bare: an issue key, or the run of block-id characters after a
 *  `#`. */
interface BareCandidate {
  readonly start: number;
  readonly end: number;
  readonly key?: string;
  readonly id?: string;
}

/** The keys and `#id`s written bare between `from` and `to`, in order. A candidate starts in the
 *  gap and is read to its own end, which the server reads the same way: a run that reaches into
 *  the span after the gap is read whole and then names nothing. */
function bareCandidates(text: string, from: number, to: number, found: BareCandidate[]): void {
  for (let at = from; at < to; at += 1) {
    const character = text.charAt(at);
    const isKeyStart = character >= "A" && character <= "Z";
    if ((character !== "#" && !isKeyStart) || !isMentionStart(text, at)) {
      continue;
    }
    if (character === "#") {
      let end = at + 1;
      while (end < text.length && blockIdCharacter.test(text.charAt(end))) {
        end += 1;
      }
      if (end > at + 1) {
        found.push({ end, id: text.slice(at + 1, end), start: at });
      }
      at = end - 1;
      continue;
    }
    const end = keyAt(text, at);
    if (end === undefined) {
      continue;
    }
    found.push({ end, key: text.slice(at, end), start: at });
    at = end - 1;
  }
}

/** The ask the question map gives `#id`: the id as written, else the id without the trailing `_`
 *  and `-` an emphasis or a sentence leaves on it, else - when what is left is an id written
 *  short - the one question block id it is a prefix of. A declared id wins over a short one, and
 *  a short one two question blocks or more carry names nothing. */
function questionReference(
  questions: Readonly<Record<string, string>> | undefined,
  id: string
): string | undefined {
  if (questions === undefined) {
    return undefined;
  }
  const trimmed = id.replace(/[_-]+$/, "");
  const shortened = trimmed === id || trimmed === "" ? undefined : trimmed;
  if (Object.hasOwn(questions, id)) {
    return questions[id];
  }
  if (shortened !== undefined && Object.hasOwn(questions, shortened)) {
    return questions[shortened];
  }
  return (
    questionPrefix(questions, id) ??
    (shortened === undefined ? undefined : questionPrefix(questions, shortened))
  );
}

/** The ask of the one question block whose id `value` is a short id of, compared without case. */
function questionPrefix(
  questions: Readonly<Record<string, string>>,
  value: string
): string | undefined {
  if (!isIdPrefix(value)) {
    return undefined;
  }
  let found: string | undefined;
  for (const [id, reference] of Object.entries(questions)) {
    if (
      id.length < value.length ||
      id.slice(0, value.length).toLowerCase() !== value.toLowerCase()
    ) {
      continue;
    }
    if (found !== undefined) {
      return undefined;
    }
    found = reference;
  }
  return found;
}

/** Which of a route's ids a text may have written short, each with the kind of record it names:
 *  the block's id, and the item's. Only an ask, a comment, a message and a block carries an id a
 *  prefix can name, and a reference that writes both short holds two prefixes. */
function prefixIds(route: DispatchReferenceRoute): {
  block?: string;
  item?: { id: string; kind: ReferenceTargetKind };
} {
  const found: { block?: string; item?: { id: string; kind: ReferenceTargetKind } } = {};
  if ("block" in route && route.block !== undefined && isIdPrefix(route.block.id)) {
    found.block = route.block.id;
  }
  let item: { id: string; kind: ReferenceTargetKind } | undefined;
  if (referenceTargetKind(route) === "block") {
    item = itemOfBlock(route);
  } else if (route.kind === "document") {
    item = route.item === undefined ? undefined : { id: route.item.id, kind: route.item.kind };
  } else if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
    item = { id: route.id, kind: route.kind };
  }
  if (item !== undefined && isIdPrefix(item.id)) {
    found.item = { id: item.id, kind: item.kind };
  }
  return found;
}

/** The kind of target each whole id of `items` names, read from the reference it is stored
 *  under, so an id prefix is resolved among the items of its own kind, as the server resolves it
 *  within the owner the reference names. */
function itemKinds(
  items: Readonly<Record<string, string>>,
  origin: string
): readonly { id: string; kind: ReferenceTargetKind }[] {
  const kinds: { id: string; kind: ReferenceTargetKind }[] = [];
  for (const [id, reference] of Object.entries(items)) {
    const route = referenceRouteFromHref(reference, origin);
    if (route !== undefined) {
      kinds.push({ id, kind: referenceTargetKind(route) });
    }
  }
  return kinds;
}

/** The one whole id of `items` of `kind` that starts with `prefix`, compared without case;
 *  undefined where none does or two or more do, each of which the text keeps as written. */
function wholeId(
  items: readonly { id: string; kind: ReferenceTargetKind }[],
  prefix: string,
  kind: ReferenceTargetKind
): string | undefined {
  const lower = prefix.toLowerCase();
  let found: string | undefined;
  for (const item of items) {
    if (item.kind !== kind || item.id.length < lower.length) {
      continue;
    }
    if (item.id.slice(0, lower.length).toLowerCase() !== lower) {
      continue;
    }
    if (found !== undefined) {
      return undefined;
    }
    found = item.id;
  }
  return found;
}

/** `route` with its block id replaced, keeping its pin. */
function withBlockId(route: DispatchReferenceRoute, id: string): DispatchReferenceRoute {
  if (!("block" in route) || route.block === undefined) {
    return route;
  }
  const pin = route.block.pin;
  return { ...route, block: pin === undefined ? { id } : { id, pin } } as DispatchReferenceRoute;
}

/** `route` with its item id replaced. */
function withItemId(route: DispatchReferenceRoute, id: string): DispatchReferenceRoute {
  if (route.kind === "document") {
    return route.item === undefined ? route : { ...route, item: { ...route.item, id } };
  }
  return { ...route, id } as DispatchReferenceRoute;
}

/** The reference whose graph read resolves `route`'s short ids - the reference as the text
 *  wrote it, which the server resolves within the owner it names - or undefined for a route
 *  that writes every id whole, which needs no resolving and is read under its own id. */
export function prefixReference(route: DispatchReferenceRoute): string | undefined {
  const { block, item } = prefixIds(route);
  return block === undefined && item === undefined ? undefined : buildDispatchReference(route);
}

/** `route` with each id it writes short replaced by the whole id `items` names, and whether any
 *  stayed short. A rewrite that would carry the reference past the bounds the graph's index keys
 *  is dropped, as the server drops it. */
function resolvePrefixIds(
  route: DispatchReferenceRoute,
  items: readonly { id: string; kind: ReferenceTargetKind }[]
): { route: DispatchReferenceRoute; unresolved: boolean } {
  const { block, item } = prefixIds(route);
  let resolved = route;
  let unresolved = false;
  if (block !== undefined) {
    const whole = wholeId(items, block, "block");
    if (whole === undefined) {
      unresolved = true;
    } else {
      resolved = withBlockId(resolved, whole);
    }
  }
  if (item !== undefined) {
    const whole = wholeId(items, item.id, item.kind);
    if (whole === undefined) {
      unresolved = true;
    } else {
      resolved = withItemId(resolved, whole);
    }
  }
  return routeInBound(resolved) ? { route: resolved, unresolved } : { route, unresolved: true };
}

/** `route` with the whole ids `items` names put in, or undefined while one of its short ids
 *  names nothing yet: a record is read under its whole id or not at all, so nothing is ever
 *  cached under a prefix. `items` is each whole id to the reference it is stored under. */
export function routeWithWholeIds(
  route: DispatchReferenceRoute,
  items: Readonly<Record<string, string>>,
  origin: string
): DispatchReferenceRoute | undefined {
  const resolved = resolvePrefixIds(route, itemKinds(items, origin));
  return resolved.unresolved ? undefined : resolved.route;
}

/**
 * Every reference `text` holds, and what a caller would have to ask to resolve the rest. The
 * reference-shaped spans are read first, as the server reads them, and a bare key or `#id` only
 * in the gaps between them, so a key inside an address is part of that address and never a
 * mention of its own.
 */
export function readTextReferences(text: string, facts: TextReferenceFacts = {}): TextReading {
  const origin = facts.origin ?? window.location.origin;
  const issues = facts.issues === undefined ? undefined : new Set(facts.issues);
  const references: TextReference[] = [];
  const keys: string[] = [];
  const seenKeys = new Set<string>();
  const prefixes: string[] = [];
  const seenPrefixes = new Set<string>();
  const bare: BareCandidate[] = [];
  let questions = false;
  let kinds: readonly { id: string; kind: ReferenceTargetKind }[] | undefined;

  const spans = referenceSpans(text);
  let from = 0;
  for (const span of spans) {
    bareCandidates(text, from, span.start, bare);
    from = span.end;
    const route = referenceRouteFromHref(span.value, origin);
    if (route === undefined) {
      continue;
    }
    // An address writing every id whole is the route it parsed to; one writing any short is
    // that route with the ids `facts` resolved put in, and is asked about either way.
    const written = prefixReference(route);
    let resolved = { route, unresolved: false };
    if (written !== undefined) {
      if (!seenPrefixes.has(written)) {
        seenPrefixes.add(written);
        prefixes.push(written);
      }
      kinds ??= itemKinds(facts.items ?? {}, origin);
      resolved = resolvePrefixIds(route, kinds);
    }
    references.push({
      end: span.start + span.value.length,
      form: "address",
      reference: buildDispatchReference(resolved.route),
      route: resolved.route,
      start: span.start,
      text: span.value,
      unresolved: resolved.unresolved,
    });
  }
  bareCandidates(text, from, text.length, bare);

  for (const candidate of bare) {
    const written = text.slice(candidate.start, candidate.end);
    if (candidate.key !== undefined) {
      const key = candidate.key;
      if (!seenKeys.has(key)) {
        seenKeys.add(key);
        keys.push(key);
      }
      const exists = issues?.has(key) === true;
      references.push({
        end: candidate.end,
        form: "key",
        reference: exists ? `dispatch://${key}` : undefined,
        route: exists ? { key, kind: "issue" } : undefined,
        start: candidate.start,
        text: written,
        unresolved: !exists,
      });
      continue;
    }
    questions = true;
    const reference = questionReference(facts.questions, candidate.id ?? "");
    references.push({
      end: candidate.end,
      form: "question",
      reference,
      route: reference === undefined ? undefined : referenceRouteFromHref(reference, origin),
      start: candidate.start,
      text: written,
      unresolved: reference === undefined,
    });
  }
  references.sort((one, other) => one.start - other.start);
  return { needs: { keys, prefixes, questions }, references };
}

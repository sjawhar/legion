import { isIdPrefix } from "@legion/contracts";

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
  /** How many characters from `start` the reference names, where that is fewer than all of the
   *  ones written, so a title drawn over it hides no character it does not stand for: a `#id`
   *  or an address an author left a sentence's or an emphasis's `-` or `_` on resolves without
   *  those characters, which stay the author's own text. Absent where the reference needs every
   *  character it was written with. */
  readonly named?: number;
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

/** What the text belongs to, as the reference graph knows it: the record whose text it is, by
 *  the kind the server indexes that record under and its id - a document version's own text
 *  (`artifact`), an ask's question, answer or resolution reason, a comment, an issue message.
 *  Which document a `#id` in it resolves against is the server's to say
 *  (`refs.ScopeOf`, read through `GET /api/v1/references/questions`): the document itself, the
 *  document an ask or a comment is written on or anchored to, an issue message's spec. Text
 *  that is no record's - an agent's conversation, a broadcast, a search hit - names no source
 *  and gets no `#id` links, which is right: a `#id` there declares nothing. */
export interface TextSource {
  readonly kind: "artifact" | "ask" | "comment" | "message";
  readonly id: string;
}

/** The source of one record's own text, or none where there is no record to name it by: a
 *  reply shown without its id, a row of text an issue owns but no record holds. */
export function textSourceOf(
  kind: TextSource["kind"],
  id: string | null | undefined
): TextSource | undefined {
  return id === null || id === undefined || id === "" ? undefined : { id, kind };
}

/** What a caller already knows about the text. Each is read only where the text holds a
 *  candidate for it; everything absent leaves its candidates unresolved. */
export interface TextReferenceFacts {
  /** The dashboard's own origin, which its URLs are read against; the page's by default. */
  readonly origin?: string;
  /** The issue keys that name an issue that exists. */
  readonly issues?: readonly string[];
  /** The question map of the document the text's source belongs to, as the server answers it:
   *  each question block id to the reference of the ask it declares. */
  readonly questions?: Readonly<Record<string, string>>;
  /** Each whole id a prefix resolved to, to the reference that item is stored under. */
  readonly items?: Readonly<Record<string, string>>;
}

export interface TextReading {
  /** Every reference the text holds, in the order it holds them. */
  readonly references: readonly TextReference[];
  readonly needs: TextReferenceNeeds;
}

/**
 * The one whole id of `ids` that starts with `prefix`, compared without case; undefined where
 * none does or two or more do, which the text keeps as written, because an id written short
 * names a record only where it names exactly one - as the server resolves one. The same id
 * listed twice is that one record again, not a second match.
 */
export function uniquePrefixMatch(ids: Iterable<string>, prefix: string): string | undefined {
  const lower = prefix.toLowerCase();
  let found: string | undefined;
  for (const id of ids) {
    if (id === found || !id.toLowerCase().startsWith(lower)) {
      continue;
    }
    if (found !== undefined) {
      return undefined;
    }
    found = id;
  }
  return found;
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

/** `written` without the trailing `_` and `-` an emphasis or a sentence leaves on a mention, or
 *  undefined where it has none or is nothing without them. It is the one statement of that
 *  trim: a mention is resolved as written first and this way second, and the characters it
 *  resolved without are the author's own (`TextReference.named`). */
function trimmedMention(written: string): string | undefined {
  const trimmed = written.replace(/[_-]+$/, "");
  return trimmed === written || trimmed === "" ? undefined : trimmed;
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
  const shortened = trimmedMention(id);
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
  const id = uniquePrefixMatch(Object.keys(questions), value);
  return id === undefined ? undefined : questions[id];
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

/** The one whole id of `items` of `kind` that starts with `prefix`: a prefix is resolved among
 *  the items of its own kind, as the server resolves it within the owner the reference names. */
function wholeId(
  items: readonly { id: string; kind: ReferenceTargetKind }[],
  prefix: string,
  kind: ReferenceTargetKind
): string | undefined {
  return uniquePrefixMatch(
    items.filter((item) => item.kind === kind).map((item) => item.id),
    prefix
  );
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

/** One address as read: the reference the reader stores for it, the route behind that
 *  reference, and `written`, the reference its short ids are asked about under (undefined where
 *  the address writes every id whole). */
interface AddressReading {
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly unresolved: boolean;
  readonly written: string | undefined;
}

/** The one reading of an address, written once so no caller can read one differently: the route
 *  it parses to, that route with the whole ids `items` names put in, and the reference built
 *  from the result. The reader runs it twice over a span - as the author wrote it and without
 *  the trailing `_` or `-` - and compares the two references, which is only an answer while
 *  both come from the same chain. `items` is called only where the address writes a short id,
 *  since naming the kind of every resolved item costs a pass over them.
 *  Undefined where the value is no address. */
function readAddress(
  value: string,
  origin: string,
  items: () => readonly { id: string; kind: ReferenceTargetKind }[]
): AddressReading | undefined {
  const route = referenceRouteFromHref(value, origin);
  if (route === undefined) {
    return undefined;
  }
  const written = prefixReference(route);
  const resolved =
    written === undefined ? { route, unresolved: false } : resolvePrefixIds(route, items());
  return {
    reference: buildDispatchReference(resolved.route),
    route: resolved.route,
    unresolved: resolved.unresolved,
    written,
  };
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
  // The kind of every item a prefix resolved to, read on the first address that writes a short
  // id and kept for the rest of the text.
  const items = () => (kinds ??= itemKinds(facts.items ?? {}, origin));

  const spans = referenceSpans(text);
  let from = 0;
  for (const span of spans) {
    bareCandidates(text, from, span.start, bare);
    from = span.end;
    const address = readAddress(span.value, origin, items);
    if (address === undefined) {
      continue;
    }
    // An address writing every id whole is the route it parsed to; one writing any short is
    // that route with the ids `facts` resolved put in, and is asked about either way.
    if (address.written !== undefined && !seenPrefixes.has(address.written)) {
      seenPrefixes.add(address.written);
      prefixes.push(address.written);
    }
    // The trailing `_` or `-` an author left on an address is theirs where the address names
    // the same thing without it.
    const shortened = trimmedMention(span.value);
    const named =
      shortened !== undefined &&
      readAddress(shortened, origin, items)?.reference === address.reference
        ? shortened.length
        : undefined;
    references.push({
      end: span.start + span.value.length,
      form: "address",
      named,
      reference: address.reference,
      route: address.route,
      start: span.start,
      text: span.value,
      unresolved: address.unresolved,
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
    const id = candidate.id ?? "";
    const reference = questionReference(facts.questions, id);
    // The `#id` names the ask without the `_` or `-` a sentence or an emphasis left on it where
    // the trimmed id names the same ask, and the `#` counts toward the characters it names.
    const shortened = trimmedMention(id);
    const named =
      reference !== undefined &&
      shortened !== undefined &&
      questionReference(facts.questions, shortened) === reference
        ? shortened.length + 1
        : undefined;
    references.push({
      end: candidate.end,
      form: "question",
      named,
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

/**
 * Every reference a list of candidates holds, each read on its own as the whole mention it is.
 * A document editor reports the distinct words its own reader found rather than one text, and
 * each is resolved here under exactly the rules a body's text is: a candidate neither starts
 * nor ends against what another one holds, so no joining character has to stand for the
 * boundary. Each reference's `start` and `end` are its own candidate's.
 */
export function readTextCandidates(
  candidates: readonly string[],
  facts: TextReferenceFacts = {}
): TextReading {
  const references: TextReference[] = [];
  const keys: string[] = [];
  const seenKeys = new Set<string>();
  const prefixes: string[] = [];
  const seenPrefixes = new Set<string>();
  let questions = false;
  for (const candidate of candidates) {
    const reading = readTextReferences(candidate, facts);
    references.push(...reading.references);
    for (const key of reading.needs.keys) {
      if (!seenKeys.has(key)) {
        seenKeys.add(key);
        keys.push(key);
      }
    }
    for (const prefix of reading.needs.prefixes) {
      if (!seenPrefixes.has(prefix)) {
        seenPrefixes.add(prefix);
        prefixes.push(prefix);
      }
    }
    questions ||= reading.needs.questions;
  }
  return { needs: { keys, prefixes, questions }, references };
}

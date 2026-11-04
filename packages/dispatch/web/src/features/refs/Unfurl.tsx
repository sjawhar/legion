import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { api } from "../../api/client";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  surfaceMutedBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

import { MarkdownPreview } from "./MarkdownPreview";
import { referenceSpans, trimReference } from "./reference-spans";
import { useReferencePath, useReferenceTarget } from "./reference-target";
import { buildDispatchReference, type DispatchReferenceRoute } from "./routes";
import {
  readTextReferences,
  type TextReference,
  type TextReferenceFacts,
  type TextSource,
} from "./text-references";
import { useTextReferences } from "./useTextReferences";

interface UnfurlProps {
  body: string;
  /** The document the body's text belongs to, which a `#id` in it names a question block of
   *  (`TextSource`); without one a `#id` names nothing, so a body holding one shows no card. */
  source?: TextSource;
}

interface GitHubIssue {
  body?: string | null;
  title?: string;
}

/** The card's body: two rendered lines of what the target says, formatted, then cut. */
function Body({ markdown }: { markdown: string }): ReactNode {
  return (
    <MarkdownPreview className={`mt-1 ${textSecondaryOnSurface}`} lines={2} markdown={markdown} />
  );
}

/** A card for one Dispatch reference: the route it resolved to, its `dispatch://` form (the
 *  card's identity, one card per distinct reference), and the words the body wrote for it, which
 *  the card shows while the target's title loads and for a target that has none. */
interface DispatchCard {
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly text: string;
}

interface GitHubReference {
  readonly href: string;
  readonly path: string;
}

/** The cards a reference-only body shows, each list in the order the body writes them. */
interface ReferenceCards {
  readonly dispatch: readonly DispatchCard[];
  readonly github: readonly GitHubReference[];
}

function DispatchUnfurl({ card }: { card: DispatchCard }): ReactNode {
  const { title, description } = useReferenceTarget(card.route, { text: true });
  const path = useReferencePath(card.route);

  return (
    <a
      className={`block rounded-lg border px-3 py-2 text-sm ${surfaceMutedBg} ${borderDefault} ${cardHoverBorder}`}
      href={path}
    >
      <span className={`block font-medium ${linkText}`}>{title ?? card.text}</span>
      {description === undefined ? null : <Body markdown={description} />}
    </a>
  );
}

function githubReference(value: string): string | undefined {
  try {
    const url = new URL(value);
    if (url.hostname !== "github.com") {
      return undefined;
    }
    const match = url.pathname.match(/^\/([^/]+)\/([^/]+)\/(?:issues|pull)\/(\d+)$/);
    return match === null ? undefined : `/repos/${match[1]}/${match[2]}/issues/${match[3]}`;
  } catch {
    return undefined;
  }
}

function GitHubUnfurl({ href, path }: { href: string; path: string }): ReactNode {
  const issue = useQuery({
    queryKey: ["github", path],
    queryFn: async () => (await api.githubRest(path)).json() as Promise<GitHubIssue>,
  });
  return (
    <a
      className={`block rounded-lg border px-3 py-2 text-sm ${surfaceMutedBg} ${borderDefault} ${cardHoverBorder}`}
      href={href}
      rel="noreferrer"
      target="_blank"
    >
      <span className={`block font-medium ${linkText}`}>{issue.data?.title ?? href}</span>
      {issue.data?.body === undefined ||
      issue.data.body === null ||
      issue.data.body.trim() === "" ? null : (
        <Body markdown={issue.data.body} />
      )}
    </a>
  );
}

/** One reference-shaped word of a body: a reference the one reader found, or a GitHub issue or
 *  pull-request URL, which it leaves alone and this module reads itself. */
interface ReferenceWord {
  readonly start: number;
  readonly end: number;
  readonly reference?: TextReference;
  readonly github?: GitHubReference;
}

const whitespaceOnly = /^\s*$/;

/** The run of non-whitespace right after a reference: what the word holding it ends with. It
 *  is read where the reference ends (sticky), so a body of many references costs one pass and
 *  not a copy of the rest of it per reference. */
const trailingRun = /\S*/y;

/**
 * Every reference-shaped word of `body`, in the order it holds them: what the one reader finds
 * (`readTextReferences` - a `dispatch://` reference, a dashboard address, an issue key written
 * bare, a `#id`) and the GitHub issue and pull-request URLs it leaves alone, which Dispatch
 * knows nothing of. A word spans only what its reference spans: the sentence punctuation and
 * emphasis the reader trims off a reference stays outside it.
 */
function referenceWords(body: string, facts: TextReferenceFacts): ReferenceWord[] {
  const words: ReferenceWord[] = readTextReferences(body, facts).references.map((reference) => ({
    end: reference.end,
    reference,
    start: reference.start,
  }));
  for (const span of referenceSpans(body, "https://github.com/")) {
    const path = githubReference(span.value);
    if (path !== undefined) {
      words.push({
        end: span.start + span.value.length,
        github: { href: span.value, path },
        start: span.start,
      });
    }
  }
  words.sort((one, other) => one.start - other.start);
  return words;
}

/**
 * How much of what trails the reference at `word` belongs to the word that holds it: the
 * non-whitespace run after it, when the reference grammar's own trim would cut all of that run
 * back to the reference (`trimReference`: a stop, a comma, the `*`, `_` and `~` that close
 * emphasis, an unopened parenthesis). `undefined` where it would leave any of it, which is a
 * word holding something besides its reference. The one list of those characters is
 * `trimReference`'s; this asks it rather than keeping a copy.
 */
function trimmedTail(body: string, word: ReferenceWord): string | undefined {
  trailingRun.lastIndex = word.end;
  const tail = trailingRun.exec(body)?.[0] ?? "";
  if (tail === "") {
    return "";
  }
  const written = body.slice(word.start, word.end);
  return trimReference(written + tail) === written ? tail : undefined;
}

/**
 * Whether `body` is its references and nothing else: between one word and the next, and on
 * either side of them all, there is only whitespace, and there is at least one. A word may end
 * in what the reference grammar trims off a reference in prose (`trimmedTail`), so a body of
 * one reference and a full stop is still that reference; anything else it holds - prose, the
 * `**` of emphasis opened before it, a bracket - leaves a character no word covers, which is
 * this answering false.
 *
 * Two references run together with no whitespace between them (`dispatch://A` and a URL) are
 * one run to the span reader, which names nothing, so such a body shows no card: a word is one
 * reference or none.
 *
 * One walk of the words, so a body at the size cap is read in one pass however many
 * reference-shaped runs it holds.
 */
function referencesOnly(body: string, words: readonly ReferenceWord[]): boolean {
  let covered = 0;
  for (const word of words) {
    if (word.start < covered || !whitespaceOnly.test(body.slice(covered, word.start))) {
      return false;
    }
    const tail = trimmedTail(body, word);
    if (tail === undefined) {
      return false;
    }
    covered = word.end + tail.length;
  }
  return covered !== 0 && whitespaceOnly.test(body.slice(covered));
}

/**
 * The cards `body` shows, or `undefined` for a body that shows none: it is nothing but
 * references (`referencesOnly`) and every one of them names something, read through the facts
 * the queries answered. A key whose issue does not exist, a `#id` no question of the body's
 * document declares, and an id prefix nothing resolved are each a word that names nothing, and
 * one of them is the whole body showing no card - until it resolves, and for good where it never
 * does.
 */
function referenceCards(body: string, facts: TextReferenceFacts): ReferenceCards | undefined {
  const words = referenceWords(body, facts);
  if (!referencesOnly(body, words)) {
    return undefined;
  }
  const dispatch: DispatchCard[] = [];
  const github: GitHubReference[] = [];
  const seen = new Set<string>();
  for (const word of words) {
    if (word.github !== undefined) {
      if (!seen.has(word.github.href)) {
        seen.add(word.github.href);
        github.push(word.github);
      }
      continue;
    }
    const found = word.reference;
    if (found === undefined || found.unresolved || found.route === undefined) {
      return undefined;
    }
    const reference = buildDispatchReference(found.route);
    if (!seen.has(reference)) {
      seen.add(reference);
      dispatch.push({ reference, route: found.route, text: found.text });
    }
  }
  return { dispatch, github };
}

/**
 * The cards under a body that is nothing but references: one card per distinct reference, in
 * the margin, the Inbox thread and the Conversation alike. The predicate is one
 * (`referenceCards`), over the one reader of what a text cites (`text-references.ts`), so every
 * form a reference is written in counts the same - a `dispatch://` reference, a dashboard
 * address, an issue key on its own, and a `#` and the id of a question block of `source`'s
 * document - and a GitHub issue or pull-request URL, which Dispatch does not read, counts too.
 *
 * A reference may end its sentence: a word is still one reference when everything it holds
 * beyond it is what the reference grammar itself trims (`trimmedTail`), so `dispatch://CORE-1.`
 * and `CORE-1,` card as they always have. A body with anything else beside its references
 * shows none: `MarkdownBody` already draws each reference inline with its title, so a card
 * there would say the same thing twice. A word that names nothing shows none either, so a body
 * of one bare key whose issue does not exist stays the text it is.
 *
 * Only a body already shaped like one asks the server anything: the shape is read first, from
 * the text alone, and the queries behind the keys, the `#id`s and the id prefixes
 * (`useTextReferences`) are started only for a body that passed it. Almost every body is prose
 * and starts none.
 */
export function Unfurl({ body, source }: UnfurlProps): ReactNode {
  const origin = window.location.origin;
  const shaped = useMemo(
    () => referencesOnly(body, referenceWords(body, { origin })),
    [body, origin]
  );
  const { facts } = useTextReferences(shaped ? body : "", source);
  const cards = useMemo(
    () => (shaped ? referenceCards(body, facts) : undefined),
    [body, facts, shaped]
  );

  if (cards === undefined) {
    return null;
  }
  return (
    <section aria-label="References" className="mt-3 space-y-2">
      {cards.dispatch.map((card) => (
        <DispatchUnfurl card={card} key={card.reference} />
      ))}
      {cards.github.map((reference) => (
        <GitHubUnfurl href={reference.href} key={reference.href} path={reference.path} />
      ))}
    </section>
  );
}

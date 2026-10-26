import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { api, type ReferenceSource } from "../../api/client";
import type { Artifact } from "../../api/types";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  surfaceMutedBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

import { MarkdownPreview } from "./MarkdownPreview";
import { ReferenceTitleWords } from "./RefLink";
import { useReferenceData, useReferenceTargetOf } from "./reference-target";
import {
  type BareReferences,
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  parseDispatchReference,
  resolveTextReference,
  type TextReference,
  textReferences,
} from "./routes";
import { useBareReferences } from "./useRenderedMarkdown";

interface UnfurlProps {
  body: string;
  /** The item whose text body is, which a `#id` in it resolves against (`ReferenceSource`).
   *  Without one a `#id` names nothing, so a body holding one shows no card. */
  source?: ReferenceSource;
}

interface GitHubIssue {
  body?: string | null;
  title?: string;
}

/** The card's body: two rendered lines of what the target says, formatted, then cut. `source`
 *  is the item that text is, when it is a Dispatch item's. */
function Body({ markdown, source }: { markdown: string; source?: ReferenceSource }): ReactNode {
  return (
    <MarkdownPreview
      className={`mt-1 ${textSecondaryOnSurface}`}
      lines={2}
      markdown={markdown}
      source={source}
    />
  );
}

/** The item whose text a card's description is: the ask, comment or message itself, or the
 *  document; none for an issue, whose description is its status. */
function targetSource(
  route: DispatchReferenceRoute | undefined,
  artifact: Artifact | undefined
): ReferenceSource | undefined {
  if (route?.kind === "ask" || route?.kind === "comment" || route?.kind === "message") {
    return { id: route.id, kind: route.kind };
  }
  if (route?.kind === "document" && route.item !== undefined) {
    return { id: route.item.id, kind: route.item.kind };
  }
  const namesDocument =
    route?.kind === "document" || route?.kind === "artifact" || route?.kind === "agent-artifact";
  return namesDocument && artifact !== undefined
    ? { id: artifact.id, kind: "artifact" }
    : undefined;
}

function DispatchUnfurl({ card }: { card: DispatchCard }): ReactNode {
  const route = parseDispatchReference(card.reference);
  const data = useReferenceData(route);
  const { title, titleKey, description } = useReferenceTargetOf(route, data);

  return (
    <a
      className={`block rounded-lg border px-3 py-2 text-sm ${surfaceMutedBg} ${borderDefault} ${cardHoverBorder}`}
      href={card.href}
    >
      <span className={`block font-medium ${linkText}`}>
        {title === undefined ? (
          card.text
        ) : (
          <ReferenceTitleWords title={title} titleKey={titleKey} />
        )}
      </span>
      {description === undefined ? null : (
        <Body markdown={description} source={targetSource(route, data.artifact)} />
      )}
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

interface GitHubReference {
  readonly href: string;
  readonly path: string;
}

/** A card for a Dispatch item: its `dispatch://` form, the page it opens, and the words the body
 *  wrote for it, which the card shows while its target's title loads and for a target that does
 *  not exist, as `RefLink` shows a reference inline. */
interface DispatchCard {
  readonly href: string;
  readonly reference: string;
  readonly text: string;
}

/** The cards a reference-only body shows, one per distinct reference, in order. */
interface ReferenceCards {
  readonly dispatch: readonly DispatchCard[];
  readonly github: readonly GitHubReference[];
}

const whitespaceOnly = /^\s*$/;

/**
 * Whether body, trimmed, is nothing but the references `textReferences` reads in it, each
 * separated from the next by whitespace. Each reference is handed to `take`, in order, which
 * answers where in body it ends (its `end`, or short of it where the name it resolves to is), or
 * `undefined` to refuse it, which refuses the body; the next gap is read from that end. One scan
 * of the body, so a body at the size cap is read in one pass.
 */
function referencesOnly(
  body: string,
  take: (reference: TextReference) => number | undefined
): boolean {
  let covered = 0;
  for (const reference of textReferences(body)) {
    if (reference.start < covered || !whitespaceOnly.test(body.slice(covered, reference.start))) {
      return false;
    }
    const end = take(reference);
    if (end === undefined) {
      return false;
    }
    covered = end;
  }
  return covered !== 0 && whitespaceOnly.test(body.slice(covered));
}

/**
 * The cards of body when, trimmed, it is nothing but references separated by whitespace
 * (`referencesOnly`), each word of it one whole reference in a form Dispatch reads as one and
 * that names its target by the one resolver (`resolveTextReference`): a `dispatch://` or
 * dashboard address, an issue key written bare whose issue exists, a `#id` that names a question
 * of the body's document (both as `resolved` holds them, `useBareReferences`), or else a GitHub
 * issue or pull request URL. `undefined` for any other body: one with other text beside a
 * reference (punctuation and emphasis too, and a `#id`'s trailing `_` its question's id leaves
 * out), an address that names nothing, and a key or `#id` that has not resolved, yet or ever.
 */
function referenceCards(
  body: string,
  resolved: BareReferences,
  appOrigin: string
): ReferenceCards | undefined {
  const dispatch: DispatchCard[] = [];
  const github: GitHubReference[] = [];
  const seen = new Set<string>();
  const shaped = referencesOnly(body, (reference) => {
    const target = resolveTextReference(reference, resolved, appOrigin);
    if (target === undefined) {
      const href = reference.kind === "address" ? reference.value : undefined;
      const path = href === undefined ? undefined : githubReference(href);
      if (href === undefined || path === undefined) {
        return undefined;
      }
      if (!seen.has(href)) {
        seen.add(href);
        github.push({ href, path });
      }
      return reference.end;
    }
    const written = buildDispatchReference(target.route);
    if (!seen.has(written)) {
      seen.add(written);
      dispatch.push({
        href: buildReferencePath(target.route),
        reference: written,
        text: body.slice(reference.start, target.end),
      });
    }
    return target.end;
  });
  return shaped ? { dispatch, github } : undefined;
}

/**
 * The cards of a reference-only body (`referenceCards`): trimmed, every whitespace-separated word
 * of it is one whole reference that names its target. Such a body shows a card per distinct
 * reference beside its rendered text, in the margin, the Inbox thread and the Conversation alike;
 * a body with anything else beside its references shows none, since `MarkdownBody` already draws
 * each reference inline with its title. The issue keys and `#id`s are read through the same
 * cached queries `MarkdownBody` reads them through (`useBareReferences`), the `#id`s against
 * `source`'s document, so a key or `#id` still loading shows no card until it resolves; a body
 * without the shape of one (the same walk, `referencesOnly`, taking each reference whole), which
 * is most prose, asks about none: `referenceCards` covers no more of a reference than it spans, so
 * such a body shows no card whatever its keys and `#id`s name.
 */
export function Unfurl({ body, source }: UnfurlProps): ReactNode {
  const shaped = useMemo(() => referencesOnly(body, (reference) => reference.end), [body]);
  const resolved = useBareReferences(shaped ? body : "", source);
  const cards = useMemo(
    () => referenceCards(body, resolved, window.location.origin),
    [body, resolved]
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

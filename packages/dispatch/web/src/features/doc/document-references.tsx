import type { EditorReferences, ReferenceTarget } from "@legion/proof-editor";
import { useQueryClient } from "@tanstack/react-query";
import {
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useState,
} from "react";
import { useNavigate } from "react-router-dom";

import { fetchReferencePath, needsSpecSlug, useReferenceTarget } from "../refs/reference-target";
import {
  buildReferencePath,
  type DispatchReferenceRoute,
  parseNodeReference,
} from "../refs/routes";
import { readTextReferences, type TextSource } from "../refs/text-references";
import { useTextReferences } from "../refs/useTextReferences";
import type { EditorHandle } from "./editor";

/**
 * What a document editor shows for the references its own text holds, and what a click on one
 * opens. The live editor and a past version both draw them, and both read them the one way the
 * dashboard reads what a text cites (`features/refs/text-references.ts`), so a bare issue key, a
 * `#id`, a bare `dispatch://` reference and a link whose text is its own address resolve under
 * exactly the rules a rendered body resolves them under. A link whose text its author wrote
 * keeps those words.
 *
 * The editor owns the positions and this owns the grammar: the plugin hands each text block's own
 * text to `scan`, which is the one reader, and reports the distinct words it found; this resolves
 * those words - their existence, their document's question map, the whole id of a prefix - and
 * hands back a title for each. The document's stored text never changes
 * (`@legion/proof-editor`'s `src/reference-decorations.ts`).
 */

/** The empty answers, shared so an editor whose text cites nothing keeps one identity across
 *  renders and never hands the plugin a fresh value that says the same thing. */
const NO_WORDS: readonly string[] = [];
const NO_TITLES: Readonly<Record<string, string>> = {};

/** One reference the editor's text holds, as the host resolved it. */
interface ResolvedReference {
  /** The words the editor reported, which it keys its candidates by. */
  readonly words: string;
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly length: number | undefined;
}

/** One resolved reference's title, reported upward as its queries answer. The queries are the
 *  ones every other surface reads a reference's title through, so a title the hover card has
 *  already read costs the editor nothing. */
function ReferenceTitle({
  onTitle,
  reference,
  route,
}: {
  onTitle(reference: string, title: string | undefined): void;
  reference: string;
  route: DispatchReferenceRoute;
}): null {
  const { title } = useReferenceTarget(route, { text: false });
  const shown = route.kind === "issue" && title !== undefined ? `${route.key} ${title}` : title;
  useEffect(() => {
    onTitle(reference, shown);
  }, [onTitle, reference, shown]);
  // A reference the text no longer holds takes its title with it.
  useEffect(() => () => onTitle(reference, undefined), [onTitle, reference]);
  return null;
}

/**
 * Feeds `editor` the references its text holds and their titles, and renders nothing of its own.
 * `source` is the record the text is - the document itself - whose question map the server
 * answers for, so a `#id` in the editor resolves exactly as it does in a rendered body
 * (`useTextReferences`). Text of no record resolves no `#id`.
 */
export function DocumentReferences({
  editor,
  source,
}: {
  editor: EditorHandle | undefined;
  source: TextSource | undefined;
}): ReactNode {
  // The words the editor last reported: every candidate its reader found in the document, which
  // is what this resolves - never the markdown the page loaded, which a typed reference is not in.
  const [words, setWords] = useState<readonly string[]>(NO_WORDS);
  const [titles, setTitles] = useState<Readonly<Record<string, string>>>(NO_TITLES);

  const onKeys = useCallback((keys: readonly string[]) => {
    setWords((current) =>
      current.length === keys.length && current.every((word, index) => word === keys[index])
        ? current
        : [...keys]
    );
  }, []);
  const onTitle = useCallback((reference: string, title: string | undefined) => {
    setTitles((current) => {
      if (current[reference] === title) {
        return current;
      }
      const next = { ...current };
      if (title === undefined) {
        delete next[reference];
      } else {
        next[reference] = title;
      }
      return next;
    });
  }, []);
  // Every candidate the one reader finds, resolved or not: what a candidate resolves to is the
  // host's answer below, so facts the editor never sees cannot change where a candidate is.
  const scan = useCallback(
    (text: string) =>
      readTextReferences(text).references.map((reference) => ({
        end: reference.end,
        key: reference.text,
        start: reference.start,
      })),
    []
  );

  // Each candidate is read as the whole mention it was, on its own: the editor's words never
  // become one text, so nothing another word holds can end this one (`readTextCandidates`).
  const { reading } = useTextReferences(words, source);
  const resolved = useMemo(() => {
    const found: ResolvedReference[] = [];
    for (const reference of reading.references) {
      if (reference.reference === undefined || reference.route === undefined) {
        continue;
      }
      found.push({
        length: reference.named,
        reference: reference.reference,
        route: reference.route,
        words: reference.text,
      });
    }
    return found;
  }, [reading]);

  const targets = useMemo(() => {
    const drawn = new Map<string, ReferenceTarget | null>();
    for (const word of words) {
      drawn.set(word, null);
    }
    for (const { length, reference, route, words: written } of resolved) {
      const title = titles[reference];
      if (title === undefined) {
        continue;
      }
      drawn.set(written, {
        href: reference,
        length,
        title,
        titleKey: route.kind === "issue" ? route.key : undefined,
      });
    }
    return drawn;
  }, [resolved, titles, words]);

  const references: EditorReferences = useMemo(
    () => ({ onKeys, scan, targets }),
    [onKeys, scan, targets]
  );
  useEffect(() => {
    editor?.setReferences(references);
    return () => editor?.setReferences(null);
  }, [editor, references]);

  // One mount per reference, since a title is a query of its own; a reference two candidates
  // write differently is read once.
  const read = useMemo(() => {
    const distinct = new Map<string, DispatchReferenceRoute>();
    for (const { reference, route } of resolved) {
      if (!distinct.has(reference)) {
        distinct.set(reference, route);
      }
    }
    return [...distinct];
  }, [resolved]);
  return (
    <>
      {read.map(([reference, route]) => (
        <ReferenceTitle key={reference} onTitle={onTitle} reference={reference} route={route} />
      ))}
    </>
  );
}

/** A rendered link mark's `dispatch://` target, if any. `@legion/proof-editor`'s Markdown
 * serializer sanitizes a `dispatch://` href to `""` (Milkdown's link sanitizer only allows
 * http/https/mailto/tel/ftp — a document strangers can edit should never render an
 * attacker-chosen non-http scheme as a clickable href) and tags the anchor with
 * `data-dispatch-href` carrying the original target instead; a title the editor draws over a
 * reference carries the same attribute. */
function dispatchHrefOf(anchor: Element): string | null {
  const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
  return href?.startsWith("dispatch://") ? href : null;
}

/**
 * What a click inside a document's container does with a `dispatch://` link: the editor's own
 * link marks and the titles it draws over the references in its text both carry their target in
 * `data-dispatch-href`, and both open it in the dashboard. A past version's container takes the
 * same handler as the live editor's, so a reference in a version a reader is reading opens what
 * it names.
 */
export function useReferenceClick(): (event: ReactMouseEvent<HTMLElement>) => void {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  return useCallback(
    (event: ReactMouseEvent<HTMLElement>) => {
      // A modifier click (open in new tab/window) or a drag-selection that happens to end
      // on the link should reach the browser/editor's own handling, not steal the click.
      const modified = event.ctrlKey || event.metaKey || event.shiftKey || event.altKey;
      if (event.button !== 0 || modified) {
        return;
      }
      const selection = window.getSelection();
      if (selection !== null && !selection.isCollapsed) {
        return;
      }
      const anchor = (event.target as HTMLElement).closest<HTMLAnchorElement>("a");
      // The link's target is a node its author named, and opening it is a read, so it is read
      // as a node's reference, whatever the length of its name.
      const href = anchor === null ? null : dispatchHrefOf(anchor);
      const route = href === null ? undefined : parseNodeReference(href);
      if (route === undefined) {
        return;
      }
      event.preventDefault();
      // A block of a spec pinned to a version opens that version under the spec's slug, which
      // only the issue names; every other reference's path is known at once.
      if (needsSpecSlug(route)) {
        void fetchReferencePath(queryClient, route).then((path) => navigate(path));
        return;
      }
      navigate(buildReferencePath(route));
    },
    [navigate, queryClient]
  );
}

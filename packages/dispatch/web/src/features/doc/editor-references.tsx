import type { EditorReferences, ReferenceCandidate, ReferenceTarget } from "@legion/proof-editor";
import { type MouseEvent, type ReactNode, useCallback, useEffect, useMemo, useState } from "react";

import { useQuestionMap, useReferenceTarget } from "../refs/reference-target";
import {
  buildReferencePath,
  type DispatchReferenceRoute,
  parseDispatchReference,
  resolveTextReference,
  textReferences,
} from "../refs/routes";
import type { EditorHandle } from "./editor";

/**
 * The editor's reference candidates in one text block's text, by the grammar the renderers and
 * the reference graph read (`textReferences`): a bare issue key keyed by the key (`CORE-1`), a
 * `#id` by `#` and its run (`#q2`), and a bare `dispatch://` reference by its address. Any other
 * address (a web URL) is no candidate.
 */
export function scanEditorReferences(text: string): ReferenceCandidate[] {
  const candidates: ReferenceCandidate[] = [];
  for (const reference of textReferences(text)) {
    const key =
      reference.kind === "key"
        ? reference.key
        : reference.kind === "question"
          ? `#${reference.id}`
          : reference.value.startsWith("dispatch://")
            ? reference.value
            : undefined;
    if (key !== undefined) {
      candidates.push({ end: reference.end, key, start: reference.start });
    }
  }
  return candidates;
}

/** An editor's side of its reference titles: the `references` option it is built with, and where
 *  the resolved targets go. */
export interface EditorReferenceBinding {
  readonly references: EditorReferences;
  /** Makes `editor` the one the targets go to, handing it the latest at once; `undefined` when
   *  the editor is torn down. */
  bind(editor: EditorHandle | undefined): void;
  /** The latest targets: handed to the bound editor, and kept for one bound later. */
  setTargets(targets: ReadonlyMap<string, ReferenceTarget | null>): void;
}

/** The candidate keys the component's editor reports, and its binding, which is the same object
 *  for the component's life, so an editor effect may name it freely. */
export function useEditorReferences(): {
  keys: readonly string[];
  binding: EditorReferenceBinding;
} {
  const [keys, setKeys] = useState<readonly string[]>([]);
  const [binding] = useState<EditorReferenceBinding>(() => {
    let editor: EditorHandle | undefined;
    let targets: ReadonlyMap<string, ReferenceTarget | null> = new Map();
    return {
      bind(next) {
        editor = next;
        next?.setReferenceTargets(targets);
      },
      references: { onKeys: setKeys, scan: scanEditorReferences },
      setTargets(next) {
        targets = next;
        editor?.setReferenceTargets(next);
      },
    };
  });
  return { binding, keys };
}

/** What a candidate key names before its title is known: the `dispatch://` reference its link
 *  carries, the route its title is read from, and how many of the key's characters the title
 *  stands for when that is fewer than all of them (a `#q2-` whose question is q2). */
interface NamedReference {
  readonly href: string;
  readonly route: DispatchReferenceRoute;
  readonly length: number | undefined;
}

/** The title one reference reads as (`useReferenceTarget`, the rule `RefLink` draws with), handed
 *  to `onTarget` with the key it resolves; `null` while it loads and for a target that does not
 *  exist. It draws nothing. */
function ReferenceTitle({
  href,
  length,
  name,
  onTarget,
  route,
}: NamedReference & {
  name: string;
  onTarget(name: string, target: ReferenceTarget | null): void;
}): ReactNode {
  const { title, titleKey } = useReferenceTarget(route);
  useEffect(() => {
    onTarget(name, title === undefined ? null : { href, length, title, titleKey });
  }, [href, length, name, onTarget, title, titleKey]);
  return null;
}

/**
 * Resolves the reference candidates `keys` names and hands the result to `onTargets` (for the
 * editor's `setReferenceTargets`), each by the one resolver the rendered bodies use
 * (`resolveTextReference`): a key once its issue answers (a 404 is final; its title's own issue
 * query is what says so), a `#id` by the question map of `artifactId`'s document
 * (`GET /api/v1/references/questions`, refetched with the document's asks, so a question block
 * links once settlement makes its ask), its title covering the `#id` the renderers link (`#q2` of
 * a `#q2-` whose question is q2), and a `dispatch://` address by its target's own queries. The
 * title is the target's (`useReferenceTarget`); an unresolved candidate is drawn as its stored
 * text. It draws nothing itself.
 */
export function EditorReferenceTargets({
  artifactId,
  keys,
  onTargets,
}: {
  artifactId: string;
  keys: readonly string[];
  onTargets(targets: ReadonlyMap<string, ReferenceTarget | null>): void;
}): ReactNode {
  const blocks = useQuestionMap(
    { id: artifactId, kind: "artifact" },
    keys.some((key) => key.startsWith("#"))
  );
  const named = useMemo(() => {
    // Every candidate key counts as an issue that exists here: its title reads its own issue
    // query, which draws it as its stored text on a 404.
    const bare = { issues: new Set(keys), questions: blocks };
    const references = new Map<string, NamedReference>();
    for (const key of keys) {
      // A candidate key is the text of the one reference `scanEditorReferences` read.
      const [reference] = textReferences(key);
      const resolved = reference === undefined ? undefined : resolveTextReference(reference, bare);
      if (resolved !== undefined) {
        references.set(key, {
          href: resolved.href,
          length: resolved.end < key.length ? resolved.end : undefined,
          route: resolved.route,
        });
      }
    }
    return references;
  }, [keys, blocks]);

  const [titles, setTitles] = useState<ReadonlyMap<string, ReferenceTarget | null>>(new Map());
  const onTarget = useCallback((name: string, target: ReferenceTarget | null) => {
    setTitles((current) => {
      const previous = current.get(name);
      const same =
        previous === target ||
        (previous !== undefined &&
          previous !== null &&
          target !== null &&
          previous.href === target.href &&
          previous.title === target.title &&
          previous.titleKey === target.titleKey);
      return same ? current : new Map(current).set(name, target);
    });
  }, []);

  useEffect(() => {
    const targets = new Map<string, ReferenceTarget | null>();
    for (const [key, reference] of named) {
      const target = titles.get(key);
      targets.set(key, target?.href === reference.href ? target : null);
    }
    onTargets(targets);
  }, [named, onTargets, titles]);

  // A key and its href fix the length: one `#id` run names one question, by one id.
  return [...named].map(([key, { href, length, route }]) => (
    <ReferenceTitle
      href={href}
      key={`${key}\n${href}`}
      length={length}
      name={key}
      onTarget={onTarget}
      route={route}
    />
  ));
}

/**
 * Follows a click on a `dispatch://` link inside a document editor in-app: a link mark (its
 * `href` sanitized to `""`, its target in `data-dispatch-href`) or a reference title the editor
 * draws. A modifier or non-primary click, and a click that ends a drag-selection, is left to the
 * browser and the editor.
 */
export function followEditorDispatchLink(
  event: MouseEvent<HTMLElement>,
  navigate: (path: string) => void
): void {
  const modified = event.ctrlKey || event.metaKey || event.shiftKey || event.altKey;
  if (event.button !== 0 || modified) {
    return;
  }
  const selection = window.getSelection();
  if (selection !== null && !selection.isCollapsed) {
    return;
  }
  const anchor = (event.target as HTMLElement).closest("a");
  const href = anchor?.getAttribute("data-dispatch-href") ?? anchor?.getAttribute("href");
  const route = href?.startsWith("dispatch://") ? parseDispatchReference(href) : undefined;
  if (route === undefined) {
    return;
  }
  event.preventDefault();
  navigate(buildReferencePath(route));
}

import { useCallback, useRef, useState } from "react";

import type { DocumentToolbar } from "../doc/ProofDocument";

export interface DocumentToolbars {
  /** Which documents are drawn against their previous version. */
  readonly diffs: ReadonlySet<string>;
  /** Turns a document's diff on or off. */
  readonly showDiff: (artifactId: string, next: boolean) => void;
  /** The one callback a document reports its toolbar through, by artifact id. */
  readonly toolbarSink: (artifactId: string) => (next: DocumentToolbar | undefined) => void;
  /** The toolbar each open document published. */
  readonly toolbars: ReadonlyMap<string, DocumentToolbar>;
}

/**
 * One toolbar and one diff per document, by its artifact id: the issue page draws the toolbar
 * of whichever document the preset has open (dispatch://INFRA-1196 R16), and every editor on
 * the page reports its own through a callback of its own.
 *
 * Each callback is held across renders, because an editor publishes its toolbar from an effect:
 * a fresh function every render would have it publish again on every commit.
 */
export function useDocumentToolbars(): DocumentToolbars {
  const [toolbars, setToolbars] = useState<ReadonlyMap<string, DocumentToolbar>>(() => new Map());
  const [diffs, setDiffs] = useState<ReadonlySet<string>>(() => new Set());
  const sinks = useRef(new Map<string, (next: DocumentToolbar | undefined) => void>());
  const showDiff = useCallback(
    (artifactId: string, next: boolean) =>
      setDiffs((held) => {
        const made = new Set(held);
        if (next) made.add(artifactId);
        else made.delete(artifactId);
        return made;
      }),
    []
  );
  const toolbarSink = useCallback(
    (artifactId: string) => {
      const held = sinks.current.get(artifactId);
      if (held !== undefined) return held;
      const made = (next: DocumentToolbar | undefined) => {
        setToolbars((held) => {
          const replaced = new Map(held);
          if (next === undefined) replaced.delete(artifactId);
          else replaced.set(artifactId, next);
          return replaced;
        });
        // A document that has closed shows no diff: the control that turned one on has gone
        // with it.
        if (next === undefined) showDiff(artifactId, false);
      };
      sinks.current.set(artifactId, made);
      return made;
    },
    [showDiff]
  );
  return { diffs, showDiff, toolbarSink, toolbars };
}

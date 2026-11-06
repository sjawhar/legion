import { createContext, type ReactNode, useCallback, useContext, useMemo, useRef } from "react";
import { createPortal } from "react-dom";

import { AUTHOR_TEXT_ATTRIBUTE } from "./page-structure";

/**
 * Where each document the issue page has open is drawn (dispatch://INFRA-1196 R2, R16,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t17`).
 *
 * One document has one room, so the page mounts one editor for it and never a second: a row
 * that opens a node's own text takes the editor the page already has rather than building one
 * of its own, which would be a second connection (`e2e/doc.e2e.ts`, "tab round-trips keep one
 * document connection"). Each open document is rendered into a *stage* - one element held for
 * the life of the page - and that stage is moved into whichever host claims it most strongly:
 * the row the reader is editing in (`STAGE_IN_ROW`) over the panel the preset draws it in
 * (`STAGE_IN_PANEL`).
 *
 * The move is `append`, which keeps every node inside the stage: the editor, its socket, its
 * collaborative cursors, its undo history and the marks in it are untouched, and React never
 * sees it, because the portal's container is the stage and the stage is what moves. What the
 * move costs is the focus - the browser drops it as the element leaves the document - and the
 * caller takes it back by focusing the block it moved the stage for.
 */

/** How strongly a host claims a document's stage. A row the reader is editing in outranks the
 *  panel the preset draws the document in, so the stage goes to the row and comes back when the
 *  row lets go. */
export const STAGE_IN_PANEL = 0;
export const STAGE_IN_ROW = 1;

/** One host's hold on one document's stage. */
interface StageClaim {
  readonly host: HTMLElement;
  readonly rank: number;
}

/** The page's stages: one element per document, and who is holding each. */
interface StageBoard {
  /** The element `artifactId`'s document is drawn into, made on first ask and held after. */
  element(artifactId: string): HTMLElement;
  /** Draws `artifactId` inside `host` until the returned release is called. */
  claim(artifactId: string, host: HTMLElement, rank: number): () => void;
}

function createStageBoard(): StageBoard {
  const elements = new Map<string, HTMLElement>();
  const claims = new Map<string, StageClaim[]>();
  const place = (artifactId: string): void => {
    const stage = elements.get(artifactId);
    if (stage === undefined) return;
    let best: StageClaim | undefined;
    for (const claim of claims.get(artifactId) ?? []) {
      if (best === undefined || claim.rank >= best.rank) best = claim;
    }
    if (best === undefined) {
      // Nothing is drawing this document. The stage keeps its contents off the page rather
      // than unmounting them, so the room stays open and the reader's text is where they left
      // it when a host claims it again.
      stage.remove();
      return;
    }
    if (stage.parentElement !== best.host) best.host.append(stage);
  };
  return {
    claim(artifactId, host, rank) {
      const claim: StageClaim = { host, rank };
      claims.set(artifactId, [...(claims.get(artifactId) ?? []), claim]);
      place(artifactId);
      return () => {
        claims.set(
          artifactId,
          (claims.get(artifactId) ?? []).filter((held) => held !== claim)
        );
        place(artifactId);
      };
    },
    element(artifactId) {
      const held = elements.get(artifactId);
      if (held !== undefined) return held;
      const made = document.createElement("div");
      made.className = "flex min-w-0 flex-col";
      // Whatever is drawn on a stage is one document as its author wrote it - its blocks, their
      // attributes and the words inside them - and never the page's own vocabulary, wherever
      // the stage is placed (dispatch://INFRA-1196 R21, `page-structure.ts`).
      made.setAttribute(AUTHOR_TEXT_ATTRIBUTE, "");
      elements.set(artifactId, made);
      return made;
    },
  };
}

const StageContext = createContext<StageBoard | undefined>(undefined);

/** Holds the issue page's stages, above every host that draws one. */
export function DocumentStages({ children }: { children: ReactNode }): ReactNode {
  const board = useMemo(createStageBoard, []);
  return <StageContext.Provider value={board}>{children}</StageContext.Provider>;
}

/** Draws `children` - one document's editor - on that document's stage, wherever the stage is.
 *  Outside a `DocumentStages` it draws them where it stands, which is what a unit test that
 *  mounts a panel on its own gets. */
export function DocumentStage({
  artifactId,
  children,
}: {
  artifactId: string;
  children: ReactNode;
}): ReactNode {
  const board = useContext(StageContext);
  const stage = useMemo(() => board?.element(artifactId), [artifactId, board]);
  return stage === undefined ? children : createPortal(children, stage);
}

/**
 * A ref for the element that draws `artifactId`'s stage while it is mounted. `undefined` claims
 * nothing, so a host with no document of its own passes the ref and draws nothing.
 */
export function useDocumentStage(
  artifactId: string | undefined,
  rank: number
): (node: HTMLElement | null) => void {
  const board = useContext(StageContext);
  const release = useRef<(() => void) | undefined>(undefined);
  return useCallback(
    (node: HTMLElement | null) => {
      release.current?.();
      release.current = undefined;
      if (node === null || artifactId === undefined || board === undefined) return;
      release.current = board.claim(artifactId, node, rank);
    },
    [artifactId, board, rank]
  );
}

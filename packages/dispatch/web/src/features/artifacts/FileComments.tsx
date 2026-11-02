import { itemFromSearch } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type PointerEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import type { AnchorBox, Artifact } from "../../api/types";
import {
  borderDefault,
  pictureBox,
  pictureBoxActive,
  pictureBoxDraft,
  pictureBoxTag,
  pictureBoxTagActive,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { MentionComposer } from "../conversation/MentionComposer";
import { useMargin } from "../margin/margin-context";
import { documentItemPath } from "../refs/routes";
import { type FileCommentOwner, fileCommentsQuery } from "./file-comments-query";
import { boxFromDrag, boxStyle, fileBoxNumbers, isFileAnchor, type Point } from "./picture-box";

export type { FileCommentOwner } from "./file-comments-query";

/** The path that opens `artifact` at `version` with comment `id`'s thread chosen. */
export function fileVersionPath(artifact: Artifact, version: number, id: string): string {
  return documentItemPath(artifact, { id, kind: "comment" }, version);
}

interface Drag {
  readonly current: Point;
  readonly pointerId: number;
  readonly rect: { left: number; top: number; width: number; height: number };
  readonly start: Point;
}

const NO_ACTIVE: ReadonlySet<string> = new Set();

/**
 * A picture with every open box thread written on the version shown drawn over it, numbered as
 * its card is (`fileBoxNumbers`). A drag over the picture - with a mouse or pen, or with a finger
 * while **Draw a box** is on - draws the box the next comment is about (`onDraw`).
 *
 * It is the margin's document while it is on the page, as a Markdown document's editor is: each
 * box is a mark whose id is its comment's, so choosing a box opens its card (`focusItemForMark`),
 * a chosen or hovered card lights its box (`setActiveMarks`), focusing a card scrolls to its box or
 * opens the version the box was drawn on (`focusMark`), and the boxes' places order and align the
 * cards (`setMarkPlacements`). It writes no text mark, so the rest of the bridge refuses or does
 * nothing, and a held margin send for another document is told that document is closed.
 */
export function PictureBoxes({
  alt,
  artifact,
  canDraw,
  draft,
  drawing,
  onDraw,
  owner,
  src,
  version,
}: {
  alt: string;
  artifact: Artifact;
  canDraw: boolean;
  draft: AnchorBox | undefined;
  /** Whether a touch draws a box; a mouse or pen always does. */
  drawing: boolean;
  onDraw(box: AnchorBox): void;
  owner: FileCommentOwner;
  src: string;
  version: number;
}): ReactNode {
  const comments = useQuery(fileCommentsQuery(owner));
  const numbers = useMemo(
    () => fileBoxNumbers(comments.data ?? [], artifact.id),
    [artifact.id, comments.data]
  );
  // The open boxes of the version shown, top to bottom and then left to right: the order their
  // cards stack in beside the picture.
  const shown = useMemo(
    () =>
      (comments.data ?? [])
        .filter(
          (comment) =>
            numbers.has(comment.id) && !comment.resolved && comment.anchor?.version === version
        )
        .sort((left, right) => {
          const a = left.anchor?.box;
          const b = right.anchor?.box;
          if (a === undefined || b === undefined) return 0;
          return a.top - b.top || a.left - b.left;
        }),
    [comments.data, numbers, version]
  );
  const {
    focusItemForMark,
    hoverItemForMark,
    registerDocument,
    setBlockPlacements,
    setMarkPlacements,
    withdrawPlacements,
  } = useMargin();
  const navigate = useNavigate();
  const { search } = useLocation();
  const [active, setActive] = useState<ReadonlySet<string>>(NO_ACTIVE);
  const [drag, setDrag] = useState<Drag>();
  const frame = useRef<HTMLDivElement>(null);
  const boxes = useRef(new Map<string, HTMLButtonElement>());

  const openVersion = useCallback(
    (target: number, id: string, replace: boolean) => {
      navigate(fileVersionPath(artifact, target, id), { replace });
    },
    [artifact, navigate]
  );
  // What the bridge reads when the margin calls it, which may be long after it registered.
  const latest = useRef({ comments: comments.data, openVersion, version });
  latest.current = { comments: comments.data, openVersion, version };

  useEffect(() => {
    registerDocument({
      artifactId: artifact.id,
      focusBlock: () => {},
      focusMark: (markId) => {
        const anchor = latest.current.comments?.find((comment) => comment.id === markId)?.anchor;
        if (isFileAnchor(anchor) && anchor.version !== latest.current.version) {
          latest.current.openVersion(anchor.version, markId, false);
          return;
        }
        boxes.current.get(markId)?.scrollIntoView?.({ block: "nearest" });
      },
      removeMark: () => {},
      remarkMark: () => ({ refused: "unmarkable" }),
      retypeMark: () => ({ refused: "missing" }),
      setActiveBlocks: () => {},
      setActiveMarks: (markIds) => setActive(markIds.length === 0 ? NO_ACTIVE : new Set(markIds)),
      whenSent: async () => {},
    });
    return () => registerDocument(undefined);
  }, [artifact.id, registerDocument]);

  // A link to a box drawn on another version opens that version, where the box is.
  useEffect(() => {
    const item = itemFromSearch(search);
    if (item?.kind !== "comment") return;
    const anchor = comments.data?.find((comment) => comment.id === item.id)?.anchor;
    if (!numbers.has(item.id) || !isFileAnchor(anchor) || anchor.version === version) return;
    openVersion(anchor.version, item.id, true);
  }, [comments.data, numbers, openVersion, search, version]);

  useLayoutEffect(() => {
    const element = frame.current;
    if (element === null) return;
    let request = 0;
    const publish = () => {
      cancelAnimationFrame(request);
      request = requestAnimationFrame(() => {
        // A hidden picture has no layout, and zero offsets would read as a placed landing.
        if (element.getClientRects().length === 0) {
          withdrawPlacements();
          return;
        }
        const origin = element.getBoundingClientRect().top;
        const placements = new Map<string, { pos: number; top: number }>();
        shown.forEach((comment, index) => {
          const box = boxes.current.get(comment.id);
          if (box !== undefined) {
            placements.set(comment.id, {
              pos: index,
              top: box.getBoundingClientRect().top - origin,
            });
          }
        });
        setMarkPlacements(placements);
        setBlockPlacements(new Map());
      });
    };
    const observer =
      typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(publish);
    observer?.observe(element);
    publish();
    return () => {
      cancelAnimationFrame(request);
      observer?.disconnect();
    };
  }, [setBlockPlacements, setMarkPlacements, shown, withdrawPlacements]);

  const startDrag = (event: PointerEvent<HTMLDivElement>) => {
    // A press on a box chooses its thread; only the picture itself starts a box.
    if (!canDraw || event.button !== 0 || event.target !== event.currentTarget) return;
    if (event.pointerType === "touch" && !drawing) return;
    event.preventDefault();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    const rect = event.currentTarget.getBoundingClientRect();
    const start = { x: event.clientX, y: event.clientY };
    setDrag({
      current: start,
      pointerId: event.pointerId,
      rect: { height: rect.height, left: rect.left, top: rect.top, width: rect.width },
      start,
    });
  };
  const moveDrag = (event: PointerEvent<HTMLDivElement>) => {
    if (drag === undefined || drag.pointerId !== event.pointerId) return;
    setDrag({ ...drag, current: { x: event.clientX, y: event.clientY } });
  };
  const endDrag = (event: PointerEvent<HTMLDivElement>) => {
    if (drag === undefined || drag.pointerId !== event.pointerId) return;
    setDrag(undefined);
    const box = boxFromDrag(drag.start, { x: event.clientX, y: event.clientY }, drag.rect);
    if (box !== undefined) onDraw(box);
  };
  const live = drag === undefined ? undefined : boxFromDrag(drag.start, drag.current, drag.rect);
  const pending = live ?? draft;

  return (
    <div className={`relative overflow-hidden rounded-lg border ${borderDefault}`}>
      <img alt={alt} className="block h-auto w-full select-none" draggable={false} src={src} />
      <div
        className={`absolute inset-0 ${canDraw ? "cursor-crosshair" : ""} ${drawing ? "touch-none" : ""}`}
        data-testid="picture-boxes"
        onPointerCancel={() => setDrag(undefined)}
        onPointerDown={startDrag}
        onPointerMove={moveDrag}
        onPointerUp={endDrag}
        ref={frame}
      >
        {shown.map((comment) => {
          const box = comment.anchor?.box;
          const number = numbers.get(comment.id);
          if (box === undefined || number === undefined) return null;
          const chosen = active.has(comment.id);
          return (
            <button
              aria-label={`Box ${number}: ${comment.body.slice(0, 80)}`}
              aria-pressed={chosen}
              className={`absolute rounded-sm ${chosen ? pictureBoxActive : pictureBox}`}
              data-id={comment.id}
              key={comment.id}
              onClick={() => focusItemForMark(comment.id)}
              onMouseEnter={() => hoverItemForMark(comment.id)}
              onMouseLeave={() => hoverItemForMark(null)}
              ref={(element) => {
                if (element === null) boxes.current.delete(comment.id);
                else boxes.current.set(comment.id, element);
              }}
              style={boxStyle(box)}
              type="button"
            >
              <span
                aria-hidden="true"
                className={`pointer-events-none absolute left-0 top-0 min-w-5 rounded-br px-1 text-xs font-semibold leading-5 ${
                  chosen ? pictureBoxTagActive : pictureBoxTag
                }`}
              >
                {number}
              </span>
            </button>
          );
        })}
        {pending === undefined ? null : (
          <div
            aria-label="The box your comment is about"
            className={`pointer-events-none absolute rounded-sm ${pictureBoxDraft}`}
            data-testid="picture-box-draft"
            role="img"
            style={boxStyle(pending)}
          />
        )}
      </div>
    </div>
  );
}

/**
 * The **Comment** box under an uploaded file: a comment there starts a thread on the whole file at
 * the version shown, or, once a box is drawn on the picture, on that part of it. The thread joins
 * the margin beside the file and the issue's Conversation like any other.
 */
export function FileCommentBox({
  artifact,
  draft,
  drawing,
  isClosed,
  onClearDraft,
  onDrawingChange,
  owner,
  version,
}: {
  artifact: Artifact;
  draft: AnchorBox | undefined;
  drawing: boolean;
  isClosed: boolean;
  onClearDraft(): void;
  onDrawingChange(next: boolean): void;
  owner: FileCommentOwner;
  version: number;
}): ReactNode {
  const picture = artifact.kind === "image";
  const fileAnchor = useMemo(
    () => ({ artifact: artifact.id, version, ...(draft === undefined ? {} : { box: draft }) }),
    [artifact.id, draft, version]
  );
  const mutationKey = useMemo(() => ["file-composer", artifact.id], [artifact.id]);
  return (
    <section aria-label={`Comment on ${artifact.name}`} className="space-y-2">
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>Comment</h3>
      {isClosed ? null : (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <p className={textMutedOnCanvas}>
            {draft === undefined
              ? picture
                ? `On the whole picture, version ${version}. Drag over the picture to comment on part of it.`
                : `On the whole file, version ${version}.`
              : `On the box you drew, version ${version}.`}
          </p>
          {picture ? (
            <button
              aria-pressed={drawing}
              className={`min-h-11 rounded-lg px-3 py-2 font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={() => onDrawingChange(!drawing)}
              type="button"
            >
              Draw a box
            </button>
          ) : null}
          {draft === undefined ? null : (
            <button
              className={`min-h-11 rounded-lg px-3 py-2 font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onClearDraft}
              type="button"
            >
              Clear box
            </button>
          )}
        </div>
      )}
      <MentionComposer
        closed={isClosed}
        fileAnchor={fileAnchor}
        mutationKey={mutationKey}
        onClose={() => {}}
        onSent={onClearDraft}
        owner={owner}
      />
    </section>
  );
}
